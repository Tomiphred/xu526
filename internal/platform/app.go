package platform

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"xu526/internal/runtime"
)

const startRetryBackoff = 5 * time.Second

type instance struct {
	*runtime.Instance
	proxy    *httputil.ReverseProxy
	console  *httputil.ReverseProxy // an object store's console page
	inflight atomic.Int64
	started  time.Time
}

// app is the live state of one deployed app.
type app struct {
	spec AppSpec
	rt   runtime.Runtime
	p    *Platform

	mu         sync.Mutex
	instances  []*instance
	starting   int
	changed    chan struct{} // closed and replaced whenever instances change
	lastErr    string
	lastFailAt time.Time
	deleted    bool

	inflight atomic.Int64 // requests in the router for this app, incl. waiting
	peak     atomic.Int64 // highest inflight since the last autoscaler tick
	lastReq  atomic.Int64 // unix nanos of the latest request

	window []int64 // per-tick peaks, owned by the autoscaler goroutine
}

func newApp(p *Platform, spec AppSpec, rt runtime.Runtime) *app {
	a := &app{spec: spec, rt: rt, p: p, changed: make(chan struct{})}
	a.lastReq.Store(time.Now().UnixNano())
	return a
}

func (a *app) notifyLocked() {
	close(a.changed)
	a.changed = make(chan struct{})
}

func (a *app) countLocked() int { return len(a.instances) + a.starting }

// acquire returns the least-busy instance, starting one if the app is at zero
// (a cold start) and waiting until it is ready or ctx ends.
func (a *app) acquire(ctx context.Context) (*instance, error) {
	for {
		a.mu.Lock()
		var best *instance
		for _, in := range a.instances {
			if best == nil || in.inflight.Load() < best.inflight.Load() {
				best = in
			}
		}
		if best != nil {
			best.inflight.Add(1)
			a.mu.Unlock()
			return best, nil
		}
		if a.deleted {
			a.mu.Unlock()
			return nil, errors.New("app was deleted")
		}
		if a.starting == 0 {
			if time.Since(a.lastFailAt) < startRetryBackoff {
				err := fmt.Errorf("app failed to start: %s", a.lastErr)
				a.mu.Unlock()
				return nil, err
			}
			a.startLocked(1)
		}
		ch := a.changed
		a.mu.Unlock()

		select {
		case <-ch:
		case <-ctx.Done():
			return nil, fmt.Errorf("no instance became ready: %w", ctx.Err())
		}
	}
}

// scaleTo moves the app towards n instances. Scale-down drains gracefully.
func (a *app) scaleTo(n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.deleted {
		return
	}
	switch cur := a.countLocked(); {
	case n > cur:
		if time.Since(a.lastFailAt) >= startRetryBackoff {
			a.startLocked(n - cur)
		}
	case n < cur:
		remove := min(cur-n, len(a.instances))
		for range remove {
			a.retireLocked(a.leastBusyIndexLocked())
		}
	}
}

func (a *app) leastBusyIndexLocked() int {
	idx := 0
	for i, in := range a.instances {
		if in.inflight.Load() < a.instances[idx].inflight.Load() {
			idx = i
		}
	}
	return idx
}

func (a *app) startLocked(n int) {
	for range n {
		a.starting++
		go a.startOne()
	}
}

func (a *app) startOne() {
	// What it depends on comes up first (and stays up while it runs).
	if len(a.p.depsOf(a.spec)) > 0 {
		dctx, dcancel := context.WithTimeout(a.p.ctx, a.p.depWait(a.spec)-a.p.cfg.ColdStartTimeout)
		err := a.p.wakeDependencies(dctx, a.spec)
		dcancel()
		if err != nil {
			a.mu.Lock()
			a.starting--
			a.lastErr, a.lastFailAt = err.Error(), time.Now()
			a.notifyLocked()
			a.mu.Unlock()
			a.p.event(a.spec.Name, "error", "start failed: %v", err)
			return
		}
	}
	ctx, cancel := context.WithTimeout(a.p.ctx, a.p.cfg.ColdStartTimeout)
	defer cancel()

	t0 := time.Now()
	auth, _ := a.p.authFor(ctx, a.spec.Image)
	rs := runtime.Spec{
		ImageID: a.spec.ImageID, Auth: auth,
		App: a.spec.Name, Image: a.spec.Image, Command: a.spec.Command, Port: a.spec.Port, Upstream: a.spec.Upstream,
		Entrypoint: a.spec.Entrypoint, WorkingDir: a.spec.WorkingDir, User: a.spec.User,
		Env: a.p.telemetryEnv(a.spec, a.rt.Name() == "docker"), Memory: a.spec.MemoryMB, CPUs: a.spec.CPUs,
		Args: a.spec.Args, Stack: a.spec.Stack, Network: a.spec.Network, Internal: a.spec.Internal,
	}
	if a.spec.Service != "" {
		rs.Aliases = []string{a.spec.Service}
	}
	if nets := a.spec.Networks; len(nets) > 0 { // compose networks, as in the file
		rs.Network, rs.Aliases = nets[0].Name, nets[0].Aliases
		rs.NetworkInternal, rs.NetworkExternal = nets[0].Internal, nets[0].External
		for _, n := range nets[1:] {
			rs.Extra = append(rs.Extra, runtime.NetworkAttach{Name: n.Name, Aliases: n.Aliases, Internal: n.Internal, External: n.External})
		}
	}
	if a.rt.Name() == "docker" && !onlyInternal(a.spec.Networks) {
		// Every app also joins its environment's shared network, under its
		// own name, which is how apps reach databases and caches.
		envNet := envNetwork(a.spec.Environment)
		if rs.Network == "" {
			rs.Network, rs.Aliases = envNet, append(rs.Aliases, a.spec.Name)
		} else {
			rs.Extra = append(rs.Extra, runtime.NetworkAttach{Name: envNet, Aliases: []string{a.spec.Name}})
		}
	}
	if links := a.p.linkEnv(a.spec); len(links) > 0 {
		env := make(map[string]string, len(rs.Env)+len(links))
		for k, v := range links {
			env[k] = v
		}
		for k, v := range rs.Env { // the app's own settings win
			env[k] = v
		}
		rs.Env = env
	}
	for _, v := range a.spec.Volumes {
		rs.Volumes = append(rs.Volumes, runtime.Volume{Name: v.Volume, Path: v.Path, ReadOnly: v.ReadOnly})
	}
	a.p.engineStart(&rs, a.spec)
	ri, err := a.rt.Start(ctx, rs)
	if err == nil {
		if err = waitHealthy(ctx, ri, a.spec); err == nil {
			err = a.p.engineSetup(ctx, a.spec, ri)
		}
		if err != nil {
			err = withOutput(err, a.rt, ri)
			_ = a.rt.Stop(context.Background(), ri)
		}
	}

	a.mu.Lock()
	a.starting--
	if err != nil {
		a.lastErr, a.lastFailAt = err.Error(), time.Now()
		a.notifyLocked()
		a.mu.Unlock()
		a.p.event(a.spec.Name, "error", "start failed: %v", err)
		return
	}
	if a.deleted {
		a.mu.Unlock()
		_ = a.rt.Stop(context.Background(), ri)
		return
	}
	in := &instance{Instance: ri, started: time.Now()}
	if ri.Addr != "" { // internal services can be made public later
		in.proxy = a.p.newProxy(ri.Addr, a.spec.RewriteHost)
	}
	if addr := ri.Ports[consolePortOf(a.spec)]; addr != "" {
		in.console = a.p.newProxy(addr, false)
	}
	a.instances = append(a.instances, in)
	a.lastErr = ""
	a.notifyLocked()
	count := len(a.instances)
	a.mu.Unlock()

	a.p.event(a.spec.Name, "ready", "instance %s ready in %v (%d running)",
		ri.ID, time.Since(t0).Round(time.Millisecond), count)
	go a.watch(in)
	go a.collectLogs(in, t0)
}

// collectLogs streams the instance's output into the app's log store until
// the instance exits.
func (a *app) collectLogs(in *instance, since time.Time) {
	lf, ok := a.rt.(interface {
		Follow(context.Context, *runtime.Instance, time.Time, func(stream, line string)) error
	})
	if !ok || a.p.logs == nil {
		return
	}
	ctx, cancel := context.WithCancel(a.p.ctx)
	defer cancel()
	go func() {
		select {
		case <-in.Exited:
			time.Sleep(2 * time.Second) // let the last lines arrive
			cancel()
		case <-ctx.Done():
		}
	}()
	_ = lf.Follow(ctx, in.Instance, since.Add(-time.Second), func(stream, line string) {
		a.p.logs.Write(a.spec.Name, in.ID, stream, line)
	})
}

// watch removes an instance that dies on its own; the autoscaler replaces it.
func (a *app) watch(in *instance) {
	<-in.Exited
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, x := range a.instances {
		if x == in {
			a.instances = append(a.instances[:i], a.instances[i+1:]...)
			a.lastErr = "instance " + in.ID + " exited unexpectedly"
			a.notifyLocked()
			a.p.event(a.spec.Name, "crashed", "%s", a.lastErr)
			go a.rt.Stop(context.Background(), in.Instance) // remove the dead container
			return
		}
	}
}

// retireLocked takes an instance out of rotation, then stops it once its
// in-flight requests finish (or after 30s).
func (a *app) retireLocked(i int) {
	in := a.instances[i]
	a.instances = append(a.instances[:i], a.instances[i+1:]...)
	a.notifyLocked()
	left := len(a.instances)
	a.p.stops.Add(1)
	go func() {
		defer a.p.stops.Done()
		deadline := time.Now().Add(30 * time.Second)
		for in.inflight.Load() > 0 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		_ = a.rt.Stop(context.Background(), in.Instance)
		a.p.event(a.spec.Name, "stopped", "instance %s stopped (%d running)", in.ID, left)
	}()
}

// shutdown stops every instance, draining first.
func (a *app) shutdown() {
	a.mu.Lock()
	a.deleted = true
	for len(a.instances) > 0 {
		a.retireLocked(0)
	}
	a.notifyLocked()
	a.mu.Unlock()
}

// waitHealthy decides when a new instance can take work:
//   - public apps: any HTTP answer on the health path
//   - internal services with a port (databases, caches, APIs): a TCP connection
//   - services without a port (workers): still running after a moment
func waitHealthy(ctx context.Context, ri *runtime.Instance, spec AppSpec) error {
	if ri.Addr == "" {
		select {
		case <-ri.Exited:
			return errors.New("instance exited during startup")
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1500 * time.Millisecond):
			return nil
		}
	}
	client := &http.Client{Timeout: time.Second}
	url := "http://" + ri.Addr + spec.HealthPath
	for {
		if spec.Internal {
			if c, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", ri.Addr); err == nil {
				c.Close()
				return nil
			}
		} else {
			req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
			if resp, err := client.Do(req); err == nil {
				resp.Body.Close()
				return nil // any HTTP answer means the app is serving
			}
		}
		select {
		case <-ri.Exited:
			return errors.New("instance exited during startup")
		case <-ctx.Done():
			return fmt.Errorf("health check on %s: %w", ri.Addr, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (p *Platform) newProxy(addr string, rewriteHost bool) *httputil.ReverseProxy {
	target := &url.URL{Scheme: "http", Host: addr}
	host := addr
	if h, port, err := net.SplitHostPort(addr); err == nil && (h == "127.0.0.1" || h == "::1") {
		host = net.JoinHostPort("localhost", port)
	}
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Host = r.In.Host
			if rewriteHost {
				r.Out.Host = host
			}
			r.SetXForwarded()
		},
		Transport: p.transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		},
	}
}

// withOutput adds the instance's last lines of output to a startup error,
// so "exited during startup" comes with the app's own reason.
func withOutput(err error, rt runtime.Runtime, ri *runtime.Instance) error {
	lr, ok := rt.(interface {
		Logs(context.Context, *runtime.Instance, int) (string, error)
	})
	if !ok {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, lerr := lr.Logs(ctx, ri, 25)
	out = strings.TrimSpace(out)
	if lerr != nil || out == "" {
		return fmt.Errorf("%w (it printed nothing)", err)
	}
	return fmt.Errorf("%w; its last output:\n%s", err, out)
}
