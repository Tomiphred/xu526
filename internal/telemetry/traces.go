package telemetry

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Trace is one request (or background operation) and its spans.
type Trace struct {
	ID        string `json:"id"`
	App       string `json:"app"`
	Method    string `json:"method,omitempty"`
	Path      string `json:"path,omitempty"`
	Route     string `json:"route"`
	Status    int    `json:"status,omitempty"`
	Instance  string `json:"instance,omitempty"`
	Start     int64  `json:"start"`    // unix ns
	Duration  int64  `json:"duration"` // ns
	Wait      int64  `json:"wait"`     // ns spent waiting for an instance
	Error     bool   `json:"error"`
	SpanCount int    `json:"span_count"`
	Client    string `json:"client,omitempty"`     // the visitor's IP
	UserAgent string `json:"user_agent,omitempty"` // their browser or tool
	Spans     []Span `json:"spans,omitempty"`

	fromRouter     bool
	inRing, inSlow bool
}

// Latency buckets in milliseconds (upper bounds), last is +Inf.
var buckets = []float64{1, 2, 5, 10, 20, 50, 100, 200, 300, 500, 750, 1000, 1500, 2000, 3000, 5000, 10000, 30000, math.Inf(1)}

type minuteStat struct {
	minute        int64
	count, errors uint64
	sumMs         float64
	hist          [19]uint64
}

type routeStat struct {
	method, route string
	mins          [60]minuteStat
}

type appTraces struct {
	ring   []*Trace
	next   int
	slow   []*Trace
	routes map[string]*routeStat
}

// TraceStore keeps recent traces in memory, plus the slowest of the last day.
type TraceStore struct {
	mu   sync.Mutex
	keep int
	apps map[string]*appTraces
	byID map[string]*Trace
}

const (
	keepSlow       = 50
	maxRoutes      = 300
	maxSpans       = 1000
	slowWindowNano = int64(24 * time.Hour)
)

func NewTraceStore(perApp int) *TraceStore {
	return &TraceStore{keep: perApp, apps: map[string]*appTraces{}, byID: map[string]*Trace{}}
}

func (s *TraceStore) app(name string) *appTraces {
	a := s.apps[name]
	if a == nil {
		a = &appTraces{ring: make([]*Trace, s.keep), routes: map[string]*routeStat{}}
		s.apps[name] = a
	}
	return a
}

// AddRequest records a request seen by the router (with its root spans).
func (s *TraceStore) AddRequest(t *Trace) {
	t.fromRouter = true
	s.mu.Lock()
	defer s.mu.Unlock()
	if old := s.byID[t.ID]; old != nil { // app spans arrived first
		t.Spans = append(t.Spans, old.Spans...)
		s.removeLocked(old)
	}
	t.SpanCount = len(t.Spans)
	a := s.app(t.App)
	s.insertLocked(a, t)
	s.recordLocked(a, t.Method, t.Route, t.Error, t.Duration, t.Start)
}

// AddSpans attaches spans sent by an application to their traces. Spans
// whose trace the router never saw (background work, internal services)
// become traces of their own.
func (s *TraceStore) AddSpans(app string, spans []Span) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sp := range spans {
		if sp.TraceID == "" || sp.SpanID == "" {
			continue
		}
		if sp.Service == "" {
			sp.Service = app
		}
		t := s.byID[sp.TraceID]
		if t == nil {
			t = &Trace{ID: sp.TraceID, App: app, Route: sp.Name, Start: sp.Start}
			s.insertLocked(s.app(app), t)
		}
		if len(t.Spans) >= maxSpans {
			continue
		}
		t.Spans = append(t.Spans, sp)
		t.SpanCount = len(t.Spans)
		if sp.Error {
			t.Error = true
		}
		if !t.fromRouter { // the trace spans everything we have
			if sp.Start < t.Start || t.Start == 0 {
				t.Duration += t.Start - sp.Start
				t.Start = sp.Start
			}
			if end := sp.Start + sp.Duration; end-t.Start > t.Duration {
				t.Duration = end - t.Start
			}
			if sp.ParentID == "" {
				t.Route = sp.Name
			}
		}
		s.considerSlowLocked(s.app(t.App), t)
	}
}

func (s *TraceStore) insertLocked(a *appTraces, t *Trace) {
	if old := a.ring[a.next]; old != nil {
		old.inRing = false
		if !old.inSlow && s.byID[old.ID] == old {
			delete(s.byID, old.ID)
		}
	}
	a.ring[a.next] = t
	t.inRing = true
	a.next = (a.next + 1) % len(a.ring)
	s.byID[t.ID] = t
	s.considerSlowLocked(a, t)
}

func (s *TraceStore) removeLocked(t *Trace) {
	for _, a := range s.apps {
		for i, x := range a.ring {
			if x == t {
				a.ring[i] = nil
			}
		}
		for i, x := range a.slow {
			if x == t {
				a.slow = append(a.slow[:i], a.slow[i+1:]...)
				break
			}
		}
	}
	delete(s.byID, t.ID)
}

func (s *TraceStore) considerSlowLocked(a *appTraces, t *Trace) {
	cutoff := time.Now().UnixNano() - slowWindowNano
	kept := a.slow[:0]
	for _, x := range a.slow {
		if x.Start >= cutoff {
			kept = append(kept, x)
		} else {
			x.inSlow = false
			if !x.inRing && s.byID[x.ID] == x {
				delete(s.byID, x.ID)
			}
		}
	}
	a.slow = kept
	if t.inSlow {
		sort.Slice(a.slow, func(i, j int) bool { return a.slow[i].Duration > a.slow[j].Duration })
		return
	}
	if len(a.slow) < keepSlow || t.Duration > a.slow[len(a.slow)-1].Duration {
		t.inSlow = true
		a.slow = append(a.slow, t)
		sort.Slice(a.slow, func(i, j int) bool { return a.slow[i].Duration > a.slow[j].Duration })
		if len(a.slow) > keepSlow {
			out := a.slow[keepSlow:]
			a.slow = a.slow[:keepSlow]
			for _, x := range out {
				x.inSlow = false
				if !x.inRing && s.byID[x.ID] == x {
					delete(s.byID, x.ID)
				}
			}
		}
	}
}

func (s *TraceStore) recordLocked(a *appTraces, method, route string, isErr bool, dur, start int64) {
	key := method + " " + route
	rs := a.routes[key]
	if rs == nil {
		if len(a.routes) >= maxRoutes {
			key, method, route = "* other", "", "(other routes)"
			rs = a.routes[key]
		}
		if rs == nil {
			rs = &routeStat{method: method, route: route}
			a.routes[key] = rs
		}
	}
	minute := start / int64(time.Minute)
	m := &rs.mins[minute%60]
	if m.minute != minute {
		*m = minuteStat{minute: minute}
	}
	ms := float64(dur) / 1e6
	m.count++
	m.sumMs += ms
	if isErr {
		m.errors++
	}
	for i, b := range buckets {
		if ms <= b {
			m.hist[i]++
			break
		}
	}
}

// Endpoint is latency statistics for one route.
type Endpoint struct {
	Method    string  `json:"method"`
	Route     string  `json:"route"`
	Count     uint64  `json:"count"`
	Errors    uint64  `json:"errors"`
	ErrorRate float64 `json:"error_rate"`
	AvgMs     float64 `json:"avg_ms"`
	P50       float64 `json:"p50_ms"`
	P95       float64 `json:"p95_ms"`
	P99       float64 `json:"p99_ms"`
	TotalMs   float64 `json:"total_ms"`
	Share     float64 `json:"share"` // of all time spent serving this app
}

// Endpoints summarises every route over the last minutes (max 60), sorted by
// total time spent: the top rows are where optimising pays off most.
func (s *TraceStore) Endpoints(app string, minutes int) []Endpoint {
	minutes = max(1, min(minutes, 60))
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.apps[app]
	out := []Endpoint{}
	if a == nil {
		return out
	}
	now := time.Now().UnixNano() / int64(time.Minute)
	var all float64
	for _, rs := range a.routes {
		var e Endpoint
		var hist [19]uint64
		for _, m := range rs.mins {
			if m.count == 0 || now-m.minute >= int64(minutes) {
				continue
			}
			e.Count += m.count
			e.Errors += m.errors
			e.TotalMs += m.sumMs
			for i := range hist {
				hist[i] += m.hist[i]
			}
		}
		if e.Count == 0 {
			continue
		}
		e.Method, e.Route = rs.method, rs.route
		e.AvgMs = e.TotalMs / float64(e.Count)
		e.ErrorRate = float64(e.Errors) / float64(e.Count)
		e.P50, e.P95, e.P99 = Percentile(hist[:], .5), Percentile(hist[:], .95), Percentile(hist[:], .99)
		all += e.TotalMs
		out = append(out, e)
	}
	for i := range out {
		if all > 0 {
			out[i].Share = out[i].TotalMs / all
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TotalMs > out[j].TotalMs })
	return out
}

// Percentile interpolates within a latency histogram (milliseconds).
func Percentile(counts []uint64, q float64) float64 {
	var total uint64
	for _, c := range counts {
		total += c
	}
	if total == 0 {
		return 0
	}
	rank, cum := q*float64(total), 0.0
	for i, c := range counts {
		if c > 0 && cum+float64(c) >= rank {
			lo := 0.0
			if i > 0 {
				lo = buckets[i-1]
			}
			if math.IsInf(buckets[i], 1) {
				return lo
			}
			return lo + (buckets[i]-lo)*(rank-cum)/float64(c)
		}
		cum += float64(c)
	}
	return buckets[len(buckets)-2]
}

// Query filters the trace list.
type Query struct {
	Sort       string // recent (default) or slowest
	Route      string // exact route, e.g. "GET /users/:id"
	Search     string // substring of path or route
	ErrorsOnly bool
	MinMs      float64
	Limit      int
}

// List returns matching traces without their spans.
func (s *TraceStore) List(app string, q Query) []Trace {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.apps[app]
	out := []Trace{}
	if a == nil {
		return out
	}
	if q.Limit <= 0 || q.Limit > 500 {
		q.Limit = 100
	}
	seen := map[*Trace]bool{}
	var all []*Trace
	for i := 1; i <= len(a.ring); i++ { // newest first
		if t := a.ring[(a.next-i+len(a.ring))%len(a.ring)]; t != nil {
			all = append(all, t)
			seen[t] = true
		}
	}
	for _, t := range a.slow {
		if !seen[t] {
			all = append(all, t)
		}
	}
	search := strings.ToLower(q.Search)
	for _, t := range all {
		if q.ErrorsOnly && !t.Error {
			continue
		}
		if q.MinMs > 0 && float64(t.Duration)/1e6 < q.MinMs {
			continue
		}
		if q.Route != "" && strings.TrimSpace(t.Method+" "+t.Route) != q.Route {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(t.Path+" "+t.Route), search) {
			continue
		}
		c := *t
		c.Spans = nil
		out = append(out, c)
	}
	if q.Sort == "slowest" {
		sort.SliceStable(out, func(i, j int) bool { return out[i].Duration > out[j].Duration })
	} else {
		sort.SliceStable(out, func(i, j int) bool { return out[i].Start > out[j].Start })
	}
	if len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out
}

// Get returns a trace with its spans in start order.
func (s *TraceStore) Get(id string) (Trace, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.byID[id]
	if t == nil {
		return Trace{}, false
	}
	c := *t
	c.Spans = append([]Span(nil), t.Spans...)
	sort.SliceStable(c.Spans, func(i, j int) bool { return c.Spans[i].Start < c.Spans[j].Start })
	return c, true
}

// Forget drops an app's traces (when the app is deleted).
func (s *TraceStore) Forget(app string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.apps[app]; a != nil {
		for _, t := range a.ring {
			if t != nil {
				delete(s.byID, t.ID)
			}
		}
		for _, t := range a.slow {
			delete(s.byID, t.ID)
		}
	}
	delete(s.apps, app)
}

var (
	reNum  = regexp.MustCompile(`^\d+$`)
	reUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	reHex  = regexp.MustCompile(`^[0-9a-fA-F]{16,}$`)
	reDig  = regexp.MustCompile(`\d`)
)

// RouteOf turns a path into a route template: /users/42/orders/7f3a… becomes
// /users/:id/orders/:id, so requests group into endpoints.
func RouteOf(path string) string {
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if p == "" {
			continue
		}
		if reNum.MatchString(p) || reUUID.MatchString(p) || reHex.MatchString(p) ||
			(len(p) >= 20 && reDig.MatchString(p)) {
			parts[i] = ":id"
		}
	}
	r := strings.Join(parts, "/")
	if r == "" {
		r = "/"
	}
	return r
}
