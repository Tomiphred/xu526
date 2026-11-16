package platform

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// depends_on at run time, which scale to zero needs. Traffic between
// services of a stack doesn't pass through the router, so nothing else
// would wake a service another one needs:
//
//   - Before an instance of an app starts (cold start, scale up, restart,
//     deploy), every app it depends on is started first and must be
//     healthy, down the whole chain (frontend → backend → db).
//   - An app isn't scaled to zero while an app that depends on it is
//     running or starting; once those are gone, its own idle timeout
//     applies again.

// For stack services, what each one needs comes from the stack's file,
// worked out when needed (so stacks deployed by older versions get it
// too): its depends_on, and the services its settings point at
// (API_URL=http://backend:8000, ${{ services.backend.url }}), which it
// calls directly, not through the router. A pointer that would make a loop
// (backend's CORS setting naming frontend while frontend calls backend) is
// left out: databases, caches and storage come first, then APIs, then
// front ends.

// Dependency is one app another app needs running before it starts.
type Dependency struct {
	App string `json:"app"`
	// Optional: depends_on with required: false. Started first when it's
	// there, but its failure doesn't stop the app.
	Optional bool `json:"optional,omitempty"`
}

// depWait is how long an app may wait for its dependencies to come up:
// a cold start per level of the chain.
func (p *Platform) depWait(spec AppSpec) time.Duration {
	deep := 0
	for _, d := range p.depsOf(spec) {
		deep = max(deep, 1+p.depDepth(d.App, map[string]bool{spec.Name: true}))
	}
	return p.cfg.ColdStartTimeout * time.Duration(1+deep)
}

// depDepth is the length of the longest chain of dependencies below an app.
func (p *Platform) depDepth(name string, seen map[string]bool) int {
	if seen[name] {
		return 0 // a loop; stacks with one are refused when planned
	}
	seen[name] = true
	defer delete(seen, name)
	a := p.get(name)
	if a == nil {
		return 0
	}
	deep := 0
	for _, d := range p.depsOf(a.spec) {
		deep = max(deep, 1+p.depDepth(d.App, seen))
	}
	return deep
}

// wakeDependencies starts what spec depends on, if it isn't running, and
// waits until each has a healthy instance.
func (p *Platform) wakeDependencies(ctx context.Context, spec AppSpec) error {
	deps := p.depsOf(spec)
	if len(deps) == 0 {
		return nil
	}
	type result struct {
		dep Dependency
		err error
	}
	results := make(chan result, len(deps))
	for _, d := range deps { // in parallel: siblings don't wait for each other
		go func(d Dependency) {
			results <- result{d, p.ensureUp(ctx, spec.Name, d.App)}
		}(d)
	}
	var errs []string
	for range deps {
		r := <-results
		switch {
		case r.err == nil:
		case r.dep.Optional:
			p.event(spec.Name, "warning", "starting without %s (optional, required: false): %v", (r.dep.App), r.err)
		default:
			errs = append(errs, fmt.Sprintf("%s: %v", (r.dep.App), r.err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("what it depends on isn't up: %s", strings.Join(errs, "; "))
	}
	return nil
}

// ensureUp starts an app if it has no instance and waits until one is ready.
func (p *Platform) ensureUp(ctx context.Context, by, name string) error {
	a := p.get(name)
	if a == nil {
		return errors.New("it isn't deployed")
	}
	woke := false
	for {
		a.mu.Lock()
		if len(a.instances) > 0 {
			a.mu.Unlock()
			if woke {
				p.event(by, "ready", "%s is up, so %s starts now", (name), (by))
			}
			return nil
		}
		if a.deleted {
			a.mu.Unlock()
			return errors.New("it was deleted")
		}
		if a.starting == 0 {
			if time.Since(a.lastFailAt) < startRetryBackoff && a.lastErr != "" {
				err := errors.New(a.lastErr)
				a.mu.Unlock()
				return err
			}
			if !woke {
				p.event(by, "deploy", "starting %s first (%s depends on it)", (name), (by))
			}
			woke = true
			a.lastReq.Store(time.Now().UnixNano()) // counts as use, for its idle timeout
			a.startLocked(1)
		}
		ch := a.changed
		a.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return fmt.Errorf("didn't become ready in time: %w", ctx.Err())
		}
	}
}

// neededApps are apps that something running or starting depends on: the
// autoscaler keeps one instance of each.
func (p *Platform) neededApps(apps []*app) map[string]bool {
	need := map[string]bool{}
	for _, a := range apps {
		deps := p.depsOf(a.spec)
		if len(deps) == 0 {
			continue
		}
		a.mu.Lock()
		up := a.countLocked() > 0
		a.mu.Unlock()
		if up {
			for _, d := range deps {
				need[d.App] = true
			}
		}
	}
	return need
}

var mentionOnlyRE = regexp.MustCompile(`(?i)(CORS|ORIGIN|ALLOWED|TRUSTED|CSRF|REDIRECT|CALLBACK|RETURN|FRONTEND|CLIENT_URL|SITE_URL|APP_URL|WEB_URL|HOSTS)`)

// depsOf is what an app needs running before it starts.
func (p *Platform) depsOf(spec AppSpec) []Dependency {
	if spec.Stack != "" {
		if deps, ok := p.stackDeps(spec.Stack); ok {
			return deps[spec.Name]
		}
	}
	return spec.DependsOn
}

type stackDepCache struct {
	mu sync.Mutex
	m  map[string]stackDepEntry
}

type stackDepEntry struct {
	yaml     string
	deployed time.Time
	deps     map[string][]Dependency // app → what it needs
}

// stackDeps works out (and caches until the stack is redeployed) what
// every service of a stack needs.
func (p *Platform) stackDeps(stack string) (map[string][]Dependency, bool) {
	p.mu.RLock()
	rec := p.stacks[stack]
	var r stackRecord
	if rec != nil {
		r = *rec
	}
	p.mu.RUnlock()
	if rec == nil {
		return nil, false
	}
	c := &p.depCache
	c.mu.Lock()
	if e, ok := c.m[stack]; ok && e.yaml == r.YAML && e.deployed.Equal(r.DeployedAt) {
		c.mu.Unlock()
		return e.deps, true
	}
	c.mu.Unlock()

	plan := p.planStack(stackTarget{Stack: r.stackName(), Env: r.Environment, Files: r.Files}, []byte(r.YAML), r.Built)
	deps := planDeps(plan)
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]stackDepEntry{}
	}
	c.m[stack] = stackDepEntry{yaml: r.YAML, deployed: r.DeployedAt, deps: deps}
	c.mu.Unlock()
	return deps, true
}

// planDeps turns a plan's depends_on and the connections found in its
// settings into run-time dependencies, without loops.
func planDeps(plan *StackPlan) map[string][]Dependency {
	app := map[string]string{}
	kind := map[string]string{}
	optional := map[[2]string]bool{}
	for _, s := range plan.Services {
		app[s.Name], kind[s.Name] = s.App, s.Kind
		for _, d := range s.Spec.DependsOn {
			if d.Optional {
				optional[[2]string{s.App, d.App}] = true
			}
		}
	}
	graph := map[string][]string{} // service → services it needs
	reaches := func(from, to string) bool {
		seen := map[string]bool{}
		var walk func(n string) bool
		walk = func(n string) bool {
			if n == to {
				return true
			}
			if seen[n] {
				return false
			}
			seen[n] = true
			for _, m := range graph[n] {
				if walk(m) {
					return true
				}
			}
			return false
		}
		return walk(from)
	}
	add := func(from, to string) {
		if app[from] == "" || app[to] == "" || from == to || reaches(to, from) {
			return // not in the stack, or a loop
		}
		for _, m := range graph[from] {
			if m == to {
				return
			}
		}
		graph[from] = append(graph[from], to)
	}
	for _, e := range plan.Edges {
		if e.Kind == "depends_on" {
			add(e.From, e.To)
		}
	}
	rank := map[string]int{"database": 0, "cache": 0, "storage": 0, "api": 1, "worker": 2, "web": 3}
	uses := []PlanEdge{}
	for _, e := range plan.Edges {
		// Settings that only name a service (who may call me, where to send
		// users back) aren't calls to it.
		if e.Kind == "uses" && !mentionOnlyRE.MatchString(e.Via) {
			uses = append(uses, e)
		}
	}
	sort.SliceStable(uses, func(i, j int) bool { return rank[kind[uses[i].To]] < rank[kind[uses[j].To]] })
	for _, e := range uses {
		add(e.From, e.To)
	}
	out := map[string][]Dependency{}
	for from, tos := range graph {
		for _, to := range tos {
			out[app[from]] = append(out[app[from]], Dependency{App: app[to], Optional: optional[[2]string{app[from], app[to]}]})
		}
	}
	return out
}
