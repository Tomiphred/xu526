package platform

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"xu526/internal/runtime"
)

// Latency buckets in milliseconds (upper bounds). The last one is +Inf.
var latBuckets = []float64{1, 2, 5, 10, 20, 50, 100, 200, 300, 500, 750, 1000, 1500, 2000, 3000, 5000, 10000, 30000, math.Inf(1)}

const (
	sampleEvery = 10 * time.Second
	keepPoints  = 24 * 60 * 60 / 10 // 24 hours
)

// Point is one 10-second sample of an app.
type Point struct {
	T         int64   `json:"t"` // unix seconds
	RPS       float64 `json:"rps"`
	ErrorRate float64 `json:"error_rate"` // share of 5xx responses, 0..1
	P50       float64 `json:"p50_ms"`
	P95       float64 `json:"p95_ms"`
	P99       float64 `json:"p99_ms"`
	Instances int     `json:"instances"`
	InFlight  int64   `json:"in_flight"`
	CPU       float64 `json:"cpu_percent"` // sum over instances, 100 = one core
	MemMB     float64 `json:"mem_mb"`
}

// appMetrics survives redeploys (it is keyed by app name, not instance).
type appMetrics struct {
	requests atomic.Uint64
	codes    [6]atomic.Uint64 // index = status/100
	buckets  []atomic.Uint64
	sumMs    atomic.Uint64

	mu        sync.Mutex
	prevReq   uint64
	prev5xx   uint64
	prevBkt   []uint64
	points    []Point
	cpuPrev   map[string]runtime.Usage
	lastCPU   float64
	lastMemMB float64
}

func newAppMetrics() *appMetrics {
	return &appMetrics{buckets: make([]atomic.Uint64, len(latBuckets)), prevBkt: make([]uint64, len(latBuckets)),
		cpuPrev: map[string]runtime.Usage{}}
}

func (p *Platform) metricsFor(name string) *appMetrics {
	p.mmu.Lock()
	defer p.mmu.Unlock()
	m := p.metrics[name]
	if m == nil {
		m = newAppMetrics()
		p.metrics[name] = m
	}
	return m
}

func (m *appMetrics) observe(status int, d time.Duration) {
	m.requests.Add(1)
	if c := status / 100; c >= 1 && c <= 5 {
		m.codes[c].Add(1)
	}
	ms := float64(d) / float64(time.Millisecond)
	m.sumMs.Add(uint64(ms))
	for i, b := range latBuckets {
		if ms <= b {
			m.buckets[i].Add(1)
			break
		}
	}
}

// percentile interpolates within the histogram of this interval.
func percentile(counts []uint64, q float64) float64 {
	var total uint64
	for _, c := range counts {
		total += c
	}
	if total == 0 {
		return 0
	}
	rank := q * float64(total)
	var cum float64
	for i, c := range counts {
		if cum+float64(c) >= rank && c > 0 {
			lo := 0.0
			if i > 0 {
				lo = latBuckets[i-1]
			}
			hi := latBuckets[i]
			if math.IsInf(hi, 1) {
				return lo
			}
			return lo + (hi-lo)*(rank-cum)/float64(c)
		}
		cum += float64(c)
	}
	return latBuckets[len(latBuckets)-2]
}

// monitorLoop samples every app every 10 seconds.
func (p *Platform) monitorLoop() {
	t := time.NewTicker(sampleEvery)
	defer t.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case now := <-t.C:
			p.mu.RLock()
			apps := make([]*app, 0, len(p.apps))
			for _, a := range p.apps {
				apps = append(apps, a)
			}
			p.mu.RUnlock()
			var wg sync.WaitGroup
			for _, a := range apps {
				wg.Add(1)
				go func() { defer wg.Done(); p.sample(a, now) }()
			}
			wg.Wait()
		}
	}
}

func (p *Platform) sample(a *app, now time.Time) {
	m := p.metricsFor(a.spec.Name)
	a.mu.Lock()
	insts := append([]*instance(nil), a.instances...)
	a.mu.Unlock()

	// CPU and memory per container, computed from the change since last sample.
	var cpu, mem float64
	stats, ok := a.rt.(interface {
		Stats(context.Context, *runtime.Instance) (runtime.Usage, error)
	})
	seen := map[string]bool{}
	if ok {
		for _, in := range insts {
			ctx, cancel := context.WithTimeout(p.ctx, 3*time.Second)
			u, err := stats.Stats(ctx, in.Instance)
			cancel()
			if err != nil {
				continue
			}
			seen[in.ID] = true
			mem += float64(u.MemBytes) / (1 << 20)
			m.mu.Lock()
			if prev, ok := m.cpuPrev[in.ID]; ok && u.SystemTotal > prev.SystemTotal && u.CPUTotal >= prev.CPUTotal {
				cpu += float64(u.CPUTotal-prev.CPUTotal) / float64(u.SystemTotal-prev.SystemTotal) * float64(u.CPUs) * 100
			}
			m.cpuPrev[in.ID] = u
			m.mu.Unlock()
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.cpuPrev {
		if !seen[id] {
			delete(m.cpuPrev, id)
		}
	}
	req, e5 := m.requests.Load(), m.codes[5].Load()
	counts := make([]uint64, len(latBuckets))
	for i := range m.buckets {
		v := m.buckets[i].Load()
		counts[i] = v - m.prevBkt[i]
		m.prevBkt[i] = v
	}
	dReq := req - m.prevReq
	pt := Point{
		T: now.Unix(), RPS: float64(dReq) / sampleEvery.Seconds(),
		P50: percentile(counts, .50), P95: percentile(counts, .95), P99: percentile(counts, .99),
		Instances: len(insts), InFlight: a.inflight.Load(), CPU: math.Round(cpu*10) / 10, MemMB: math.Round(mem*10) / 10,
	}
	if dReq > 0 {
		pt.ErrorRate = float64(e5-m.prev5xx) / float64(dReq)
	}
	m.prevReq, m.prev5xx = req, e5
	m.lastCPU, m.lastMemMB = pt.CPU, pt.MemMB
	m.points = append(m.points, pt)
	if len(m.points) > keepPoints {
		m.points = append([]Point(nil), m.points[len(m.points)-keepPoints:]...)
	}
}

// MetricsSummary is what the dashboard shows above the charts.
type MetricsSummary struct {
	Points   []Point `json:"points"`
	Range    string  `json:"range"`
	Requests uint64  `json:"requests_total"`
	Errors   uint64  `json:"errors_total"`
	RPS      float64 `json:"rps"`
	Error    float64 `json:"error_rate"`
	P95      float64 `json:"p95_ms"`
	CPU      float64 `json:"cpu_percent"`
	MemMB    float64 `json:"mem_mb"`
}

// Metrics returns an app's history for a range (15m, 1h, 6h, 24h), averaged
// down to at most 120 points.
func (p *Platform) Metrics(name, rng string) (MetricsSummary, error) {
	if p.get(name) == nil {
		return MetricsSummary{}, fmt.Errorf("no app %q", name)
	}
	durs := map[string]time.Duration{"15m": 15 * time.Minute, "1h": time.Hour, "6h": 6 * time.Hour, "24h": 24 * time.Hour}
	d, ok := durs[rng]
	if !ok {
		rng, d = "1h", time.Hour
	}
	m := p.metricsFor(name)
	m.mu.Lock()
	from := time.Now().Add(-d).Unix()
	var pts []Point
	for _, pt := range m.points {
		if pt.T >= from {
			pts = append(pts, pt)
		}
	}
	s := MetricsSummary{Range: rng, Requests: m.requests.Load(), Errors: m.codes[5].Load(), CPU: m.lastCPU, MemMB: m.lastMemMB}
	m.mu.Unlock()

	// headline numbers: last 5 minutes
	cut := time.Now().Add(-5 * time.Minute).Unix()
	var n, reqs, errs float64
	var p95 []float64
	for _, pt := range pts {
		if pt.T >= cut {
			n++
			reqs += pt.RPS
			errs += pt.ErrorRate * pt.RPS
			if pt.RPS > 0 {
				p95 = append(p95, pt.P95)
			}
		}
	}
	if n > 0 {
		s.RPS = reqs / n
	}
	if reqs > 0 {
		s.Error = errs / reqs
	}
	if len(p95) > 0 {
		sort.Float64s(p95)
		s.P95 = p95[len(p95)*95/100]
	}
	s.Points = downsample(pts, 120)
	return s, nil
}

func downsample(pts []Point, limit int) []Point {
	if len(pts) <= limit {
		if pts == nil {
			return []Point{}
		}
		return pts
	}
	group := (len(pts) + limit - 1) / limit
	out := make([]Point, 0, limit)
	for i := 0; i < len(pts); i += group {
		end := min(i+group, len(pts))
		var a Point
		k := float64(end - i)
		for _, p := range pts[i:end] {
			a.RPS += p.RPS / k
			a.ErrorRate += p.ErrorRate / k
			a.P50 = math.Max(a.P50, p.P50)
			a.P95 = math.Max(a.P95, p.P95)
			a.P99 = math.Max(a.P99, p.P99)
			a.Instances = max(a.Instances, p.Instances)
			a.InFlight = max(a.InFlight, p.InFlight)
			a.CPU += p.CPU / k
			a.MemMB = math.Max(a.MemMB, p.MemMB)
		}
		a.T = pts[end-1].T
		out = append(out, a)
	}
	return out
}

// WritePrometheus writes metrics in the Prometheus text format.
func (p *Platform) WritePrometheus(w io.Writer) {
	apps := p.List()
	fmt.Fprintln(w, "# HELP xu_requests_total Requests served through the router.")
	fmt.Fprintln(w, "# TYPE xu_requests_total counter")
	for _, a := range apps {
		m := p.metricsFor(a.Spec.Name)
		for c := 1; c <= 5; c++ {
			fmt.Fprintf(w, "xu_requests_total{app=%q,code=\"%dxx\"} %d\n", a.Spec.Name, c, m.codes[c].Load())
		}
	}
	fmt.Fprintln(w, "# HELP xu_request_duration_seconds Request latency through the router.")
	fmt.Fprintln(w, "# TYPE xu_request_duration_seconds histogram")
	for _, a := range apps {
		m := p.metricsFor(a.Spec.Name)
		var cum uint64
		for i, b := range latBuckets {
			cum += m.buckets[i].Load()
			le := "+Inf"
			if !math.IsInf(b, 1) {
				le = strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.3f", b/1000), "0"), ".")
			}
			fmt.Fprintf(w, "xu_request_duration_seconds_bucket{app=%q,le=%q} %d\n", a.Spec.Name, le, cum)
		}
		fmt.Fprintf(w, "xu_request_duration_seconds_sum{app=%q} %.3f\n", a.Spec.Name, float64(m.sumMs.Load())/1000)
		fmt.Fprintf(w, "xu_request_duration_seconds_count{app=%q} %d\n", a.Spec.Name, m.requests.Load())
	}
	gauge := func(name, help string, val func(AppStatus, *appMetrics) float64) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", name, help, name)
		for _, a := range apps {
			fmt.Fprintf(w, "%s{app=%q} %g\n", name, a.Spec.Name, val(a, p.metricsFor(a.Spec.Name)))
		}
	}
	gauge("xu_instances", "Running instances.", func(a AppStatus, _ *appMetrics) float64 { return float64(a.Running) })
	gauge("xu_in_flight_requests", "Requests being handled now.", func(a AppStatus, _ *appMetrics) float64 { return float64(a.InFlight) })
	gauge("xu_cpu_percent", "CPU use across instances (100 = one core).", func(_ AppStatus, m *appMetrics) float64 {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.lastCPU
	})
	gauge("xu_memory_bytes", "Memory use across instances.", func(_ AppStatus, m *appMetrics) float64 {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.lastMemMB * (1 << 20)
	})
	if p.logs != nil {
		st := p.logs.Status()
		fmt.Fprintf(w, "# HELP xu_log_chunks_pending Log chunks waiting to be uploaded.\n# TYPE xu_log_chunks_pending gauge\nxu_log_chunks_pending %d\n", st.Pending)
		fmt.Fprintf(w, "# HELP xu_log_upload_failures_total Failed log uploads.\n# TYPE xu_log_upload_failures_total counter\nxu_log_upload_failures_total %d\n", st.Failures)
	}
}

// statusRecorder captures the status code written by the proxy.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
