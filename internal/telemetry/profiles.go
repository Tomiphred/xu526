package telemetry

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// ProfileStore keeps profiles merged into one-minute buckets per app and type.
type ProfileStore struct {
	mu        sync.Mutex
	retention time.Duration
	apps      map[string]*appProfiles
}

type appProfiles struct {
	types  map[string]*profileSeries
	intern map[string]string
}

type profileSeries struct {
	unit    string
	source  string
	minutes []*minuteProfile // sorted by minute
	last    time.Time
}

type minuteProfile struct {
	minute int64
	stacks map[string]int64
}

const maxStacksPerMinute = 20000

func NewProfileStore(retention time.Duration) *ProfileStore {
	return &ProfileStore{retention: retention, apps: map[string]*appProfiles{}}
}

// Add merges stacks observed at time at.
func (s *ProfileStore) Add(app, typ, unit, source string, at time.Time, stacks map[string]int64) {
	if len(stacks) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.apps[app]
	if a == nil {
		a = &appProfiles{types: map[string]*profileSeries{}, intern: map[string]string{}}
		s.apps[app] = a
	}
	ps := a.types[typ]
	if ps == nil {
		ps = &profileSeries{}
		a.types[typ] = ps
	}
	ps.unit, ps.source, ps.last = unit, source, at
	minute := at.Unix() / 60
	var mp *minuteProfile
	if n := len(ps.minutes); n > 0 && ps.minutes[n-1].minute == minute {
		mp = ps.minutes[n-1]
	} else {
		mp = &minuteProfile{minute: minute, stacks: map[string]int64{}}
		ps.minutes = append(ps.minutes, mp)
		sort.Slice(ps.minutes, func(i, j int) bool { return ps.minutes[i].minute < ps.minutes[j].minute })
	}
	for k, v := range stacks {
		if _, ok := mp.stacks[k]; !ok && len(mp.stacks) >= maxStacksPerMinute {
			k = "(too many distinct stacks)"
		}
		if in, ok := a.intern[k]; ok {
			k = in
		} else {
			a.intern[k] = k
		}
		mp.stacks[k] += v
	}
	cut := time.Now().Add(-s.retention).Unix() / 60
	i := 0
	for i < len(ps.minutes) && ps.minutes[i].minute < cut {
		i++
	}
	if i > 0 {
		ps.minutes = append([]*minuteProfile(nil), ps.minutes[i:]...)
		if len(a.intern) > 200000 { // rebuild the intern table now and then
			a.intern = map[string]string{}
		}
	}
}

// ProfileType describes one available profile type.
type ProfileType struct {
	Type   string    `json:"type"`
	Unit   string    `json:"unit"`
	Source string    `json:"source"` // pprof pull, pyroscope push
	Last   time.Time `json:"last"`
}

func (s *ProfileStore) Types(app string) []ProfileType {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []ProfileType{}
	if a := s.apps[app]; a != nil {
		for t, ps := range a.types {
			out = append(out, ProfileType{Type: t, Unit: ps.unit, Source: ps.source, Last: ps.last})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return rank(out[i].Type) < rank(out[j].Type) || rank(out[i].Type) == rank(out[j].Type) && out[i].Type < out[j].Type
	})
	return out
}

func rank(t string) int {
	switch {
	case t == "cpu":
		return 0
	case strings.HasPrefix(t, "alloc"):
		return 1
	case strings.HasPrefix(t, "inuse"):
		return 2
	}
	return 3
}

// Node is a flame graph node: name, total value, self value, children.
type Node struct {
	Name     string  `json:"n"`
	Total    int64   `json:"v"`
	Self     int64   `json:"s"`
	Children []*Node `json:"c,omitempty"`
	index    map[string]*Node
}

// FuncStat is one row of the top-functions table.
type FuncStat struct {
	Name  string `json:"name"`
	Self  int64  `json:"self"`
	Total int64  `json:"total"`
}

// Flame is a merged profile ready to draw.
type Flame struct {
	Type  string     `json:"type"`
	Unit  string     `json:"unit"`
	Total int64      `json:"total"`
	Root  *Node      `json:"root"`
	Top   []FuncStat `json:"top"`
}

// Flame merges the profile of one type between from and to.
func (s *ProfileStore) Flame(app, typ string, from, to time.Time) Flame {
	s.mu.Lock()
	merged := map[string]int64{}
	unit := ""
	if a := s.apps[app]; a != nil {
		if ps := a.types[typ]; ps != nil {
			unit = ps.unit
			f, t := from.Unix()/60, to.Unix()/60
			for _, m := range ps.minutes {
				if m.minute >= f && m.minute <= t {
					for k, v := range m.stacks {
						merged[k] += v
					}
				}
			}
		}
	}
	s.mu.Unlock()

	root := &Node{Name: "all", index: map[string]*Node{}}
	self, total := map[string]int64{}, map[string]int64{}
	for stack, v := range merged {
		frames := strings.Split(stack, ";")
		n := root
		n.Total += v
		seen := map[string]bool{}
		for _, f := range frames {
			c := n.index[f]
			if c == nil {
				c = &Node{Name: f, index: map[string]*Node{}}
				n.index[f] = c
				n.Children = append(n.Children, c)
			}
			c.Total += v
			n = c
			if !seen[f] { // recursion counts once toward total
				total[f] += v
				seen[f] = true
			}
		}
		n.Self += v
		self[frames[len(frames)-1]] += v
	}
	prune(root, root.Total/1000)
	top := make([]FuncStat, 0, len(self))
	for f, v := range self {
		top = append(top, FuncStat{Name: f, Self: v, Total: total[f]})
	}
	sort.Slice(top, func(i, j int) bool { return top[i].Self > top[j].Self })
	if len(top) > 50 {
		top = top[:50]
	}
	return Flame{Type: typ, Unit: unit, Total: root.Total, Root: root, Top: top}
}

// prune drops nodes too small to see (under 0.1%) and sorts the rest.
func prune(n *Node, minV int64) {
	kept := n.Children[:0]
	for _, c := range n.Children {
		if c.Total >= minV && c.Total > 0 {
			prune(c, minV)
			kept = append(kept, c)
		}
	}
	n.Children = kept
	n.index = nil
	sort.Slice(n.Children, func(i, j int) bool { return n.Children[i].Name < n.Children[j].Name })
}

// Forget drops an app's profiles.
func (s *ProfileStore) Forget(app string) {
	s.mu.Lock()
	delete(s.apps, app)
	s.mu.Unlock()
}
