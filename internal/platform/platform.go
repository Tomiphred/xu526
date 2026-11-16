// Package platform is the hosting control plane: it keeps the list of apps,
// routes public traffic to their instances, and scales them on live traffic.
package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"xu526/internal/ghapp"

	"xu526/internal/cloud"
	"xu526/internal/runtime"
	"xu526/internal/telemetry"
)

// Config sets up a Platform.
type Config struct {
	Domain           string          // apps are served at <name>.<Domain>
	StatePath        string          // JSON file holding deployed specs ("" = memory only)
	Docker           runtime.Runtime // nil if Docker isn't available
	Process          runtime.Runtime // nil to forbid running raw commands
	ColdStartTimeout time.Duration   // how long a request may wait for a new instance
	Tick             time.Duration   // autoscaler period
	PublicPort       int             // port apps are served on (for URLs)
	PublicIP         string          // this server's public address (for DNS checks)
	LogMaxBytes      int64           // rotate an app's log at this size (default 10 MB)
	LogMaxAge        time.Duration   // ... or this age (default 1 hour)
	CollectorPort    int             // telemetry ingest port apps send traces/profiles to (0 = off)
	ProfileRetention time.Duration   // how long profiles are kept (default 6 hours)
	Relay            bool            // accept tunnels from agents on other computers
	// TunnelService is a sign-up site this computer can connect through with
	// one click (the dashboard's "Connect with …"), e.g. https://tunnel.xu526.com.
	TunnelService    string
	HTTPSPort        int    // public HTTPS port, if TLS is configured (for URLs)
	HostBuilds       bool   // build git sources without a Dockerfile using xu build, on this host
	GitHubSecretsDir string // where the one-click GitHub App setup writes the app's secrets
	Logger           *log.Logger
}

// Platform runs apps.
type Platform struct {
	cfg       Config
	log       *log.Logger
	transport *http.Transport
	ctx       context.Context
	cancel    context.CancelFunc

	mu     sync.RWMutex
	apps   map[string]*app
	stacks map[string]*stackRecord
	// what each stack service needs running first (depends.go)
	depCache stackDepCache
	envs     map[string]*Environment

	stackMu sync.Mutex // one stack deploy/delete at a time

	registries  []*Registry
	regMu       sync.Mutex // ECR token refresh
	releases    map[string][]*Release
	routes      map[string]*Route
	domainIndex map[string]string // custom domain → app
	sources     map[string]*Source
	builds      map[string]*buildQueue
	bmu         sync.Mutex
	metrics     map[string]*appMetrics
	traces      *telemetry.TraceStore
	profiles    *telemetry.ProfileStore
	tokens      map[string]string // app → telemetry ingest token
	profileErr  map[string]string
	spansIn     atomic.Uint64
	relay       *relay
	pair        pairState    // connecting through the tunnel service
	portalHost  string       // the token sign-up site's hostname, if any
	portal      http.Handler // and its handler
	agent       *agentState
	mmu         sync.Mutex
	logs        *LogStore
	dataDir     string
	audit       *ghapp.Audit   // token mints, clones, sign-ins (audit.log)
	gh          *ghapp.Service // nil unless a GitHub App is configured
	ghMu        sync.Mutex
	apiMuxes    []*apiMux // to add the GitHub routes when the app is created

	events  eventLog
	started time.Time

	saveMu sync.Mutex
	stops  sync.WaitGroup // instances being drained and stopped
}

// New starts a platform, restoring previously deployed apps from StatePath.
func New(cfg Config) (*Platform, error) {
	if cfg.ColdStartTimeout == 0 {
		cfg.ColdStartTimeout = 60 * time.Second
	}
	if cfg.Tick == 0 {
		cfg.Tick = time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = log.New(os.Stderr, "", log.LstdFlags)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &Platform{
		cfg: cfg, log: cfg.Logger, ctx: ctx, cancel: cancel,
		apps: map[string]*app{}, stacks: map[string]*stackRecord{}, envs: map[string]*Environment{}, started: time.Now(),
		releases: map[string][]*Release{}, routes: map[string]*Route{}, domainIndex: map[string]string{},
		sources: map[string]*Source{}, builds: map[string]*buildQueue{}, metrics: map[string]*appMetrics{},
		traces: telemetry.NewTraceStore(2000), agent: newAgentState(), tokens: map[string]string{}, profileErr: map[string]string{},
		transport: &http.Transport{
			MaxIdleConns:        4096,
			MaxIdleConnsPerHost: 512,
			IdleConnTimeout:     90 * time.Second,
		},
	}
	for _, rt := range []runtime.Runtime{cfg.Docker, cfg.Process} {
		if rt != nil {
			if err := rt.Cleanup(ctx); err != nil {
				p.log.Printf("cleanup %s: %v", rt.Name(), err)
			}
		}
	}
	p.dataDir = filepath.Dir(cfg.StatePath)
	if cfg.StatePath == "" {
		p.dataDir, _ = os.MkdirTemp("", "xu-data")
	}
	if err := os.MkdirAll(p.dataDir, 0o700); err != nil {
		cancel()
		return nil, err
	}
	if cfg.GitHubSecretsDir == "" { // the one-click GitHub setup saves its app here
		cfg.GitHubSecretsDir = filepath.Join(p.dataDir, "github")
	}
	audit, err := ghapp.OpenAudit(filepath.Join(p.dataDir, "audit.log"))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("audit log: %w", err)
	}
	p.audit = audit
	if cfg.LogMaxBytes == 0 {
		cfg.LogMaxBytes = 10 << 20
	}
	if cfg.LogMaxAge == 0 {
		cfg.LogMaxAge = time.Hour
	}
	if cfg.ProfileRetention == 0 {
		cfg.ProfileRetention = 6 * time.Hour
	}
	p.cfg = cfg
	p.profiles = telemetry.NewProfileStore(cfg.ProfileRetention)
	host, _ := os.Hostname()
	p.logs = NewLogStore(filepath.Join(p.dataDir, "logs"), cfg.LogMaxBytes, cfg.LogMaxAge, host)
	if cfg.Relay {
		p.relay = &relay{p: p, users: map[string]*RelayUser{}, agents: map[string]*relayAgent{}, hosts: map[string]hostRoute{}}
	}
	if err := p.restore(); err != nil {
		cancel()
		return nil, err
	}
	go p.agentLoop()
	go p.logs.Run(ctx)
	go p.monitorLoop()
	go p.profileLoop()
	go p.autoscaleLoop()
	go p.watchLoop(p.ctx)
	return p, nil
}

func (p *Platform) runtimeFor(s AppSpec) (runtime.Runtime, error) {
	if s.Upstream != "" {
		return runtime.External{}, nil
	}
	if s.Image != "" {
		if p.cfg.Docker == nil {
			return nil, errors.New("docker is not available on this server")
		}
		return p.cfg.Docker, nil
	}
	if p.cfg.Process == nil {
		return nil, errors.New("running raw commands is disabled (start the daemon with -allow-process)")
	}
	return p.cfg.Process, nil
}

// Deploy creates or updates an app and records it as a new release.
func (p *Platform) Deploy(ctx context.Context, spec AppSpec) error {
	if spec.Environment == DefaultEnvironment {
		spec.Environment = ""
	}
	// The API shows secret values masked; a spec sent back from the
	// dashboard keeps the real ones.
	if old := p.get(spec.Name); old != nil {
		for k, v := range spec.Env {
			if v == maskedValue {
				if ov, ok := old.spec.Env[k]; ok {
					spec.Env[k] = ov
				}
			}
		}
	}
	if spec.Environment != "" && spec.Stack == "" {
		p.mu.Lock()
		_, _, err := p.ensureEnvLocked(spec.Environment)
		p.mu.Unlock()
		if err != nil {
			return err
		}
	}
	_, err := p.DeployRelease(ctx, spec, ReleaseMeta{Source: "manual"})
	return err
}

// deployApp starts the new version and waits until one instance is healthy
// before switching traffic, so updates have no downtime and a broken image
// never replaces a working one. It returns the spec with its image pinned.
func (p *Platform) deployApp(ctx context.Context, spec AppSpec) (AppSpec, error) {
	if err := p.fillImagePort(ctx, &spec); err != nil {
		return spec, err
	}
	if err := spec.Normalize(); err != nil {
		return spec, err
	}
	rt, err := p.runtimeFor(spec)
	if err != nil {
		return spec, err
	}
	if err := p.pinImage(ctx, &spec); err != nil {
		return spec, err
	}

	p.mu.RLock()
	old := p.apps[spec.Name]
	p.mu.RUnlock()
	if old != nil && old.spec.Stack != spec.Stack {
		if old.spec.Stack != "" {
			return spec, fmt.Errorf("%s is part of the stack %s; change it by editing the stack's compose file", spec.Name, old.spec.Stack)
		}
		return spec, fmt.Errorf("an app named %s already exists outside this stack", spec.Name)
	}

	want := max(1, spec.MinScale)
	if old != nil {
		old.mu.Lock()
		want = max(want, len(old.instances))
		old.mu.Unlock()
	}
	want = min(want, spec.MaxScale)

	next := newApp(p, spec, rt)
	next.scaleTo(want)
	if err := next.waitReady(ctx, p.depWait(spec)); err != nil {
		next.shutdown()
		return spec, err
	}

	p.mu.Lock()
	p.apps[spec.Name] = next
	p.mu.Unlock()
	if old != nil {
		old.shutdown()
	}
	p.event(spec.Name, "deploy", "deployed %s", spec.Source())
	return spec, p.save()
}

func (a *app) waitReady(ctx context.Context, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		a.mu.Lock()
		ready, starting, lastErr, ch := len(a.instances), a.starting, a.lastErr, a.changed
		a.mu.Unlock()
		if ready > 0 {
			return nil
		}
		if starting == 0 {
			return fmt.Errorf("app failed to start: %s", lastErr)
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return fmt.Errorf("app did not become healthy: %w", ctx.Err())
		}
	}
}

// Delete stops an app and forgets it. Stack services are deleted with their stack.
func (p *Platform) Delete(name string) error {
	a := p.get(name)
	if a != nil && a.spec.Stack != "" {
		return fmt.Errorf("%s is part of the stack %s; delete the stack instead", name, a.spec.Stack)
	}
	if a != nil && engines[a.spec.Engine].consoleApp != nil { // its web UI goes with it
		if c := p.get(name + "-console"); c != nil && c.spec.LoginOf == name {
			_ = p.deleteApp(name + "-console")
		}
	}
	return p.deleteApp(name)
}

func (p *Platform) deleteApp(name string) error {
	p.mu.Lock()
	a := p.apps[name]
	delete(p.apps, name)
	delete(p.routes, name)
	p.rebuildDomainIndexLocked()
	for id, s := range p.sources {
		if s.Kind == "app" && s.Target == name {
			delete(p.sources, id)
		}
	}
	p.mu.Unlock()
	p.logs.Rotate(name)
	p.traces.Forget(name)
	go p.publishTunnels()
	p.profiles.Forget(name)
	if a == nil {
		return fmt.Errorf("no app %q", name)
	}
	a.shutdown()
	p.event(name, "delete", "deleted")
	return p.save()
}

// Close stops every instance. Deployed specs stay on disk.
func (p *Platform) Close() {
	p.cancel()
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, a := range p.apps {
		a.shutdown()
	}
	done := make(chan struct{})
	go func() { p.stops.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(45 * time.Second):
		p.log.Printf("shutdown: some instances did not stop in time")
	}
}

func (p *Platform) get(name string) *app {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.apps[name]
}

// ---- status ---------------------------------------------------------------

type InstanceStatus struct {
	ID       string `json:"id"`
	Addr     string `json:"addr"`
	InFlight int64  `json:"in_flight"`
	Uptime   string `json:"uptime"`
}

type AppStatus struct {
	Spec      AppSpec          `json:"spec"`
	URL       string           `json:"url"`
	Runtime   string           `json:"runtime"`
	Running   int              `json:"running"`
	Starting  int              `json:"starting"`
	InFlight  int64            `json:"in_flight"`
	Instances []InstanceStatus `json:"instances"`
	LastError string           `json:"last_error,omitempty"`
	// Kind is the dashboard section (database, cache, backend, web).
	Kind string `json:"kind"`
	// Connection: for databases and caches made by xu526, password hidden.
	Connection *Connection `json:"connection,omitempty"`
	// UsedBy lists the apps linked to this database or cache.
	UsedBy []string `json:"used_by,omitempty"`
}

func (p *Platform) status(a *app) AppStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	spec := a.spec
	if len(spec.SecretKeys) > 0 { // values that came from an environment's secrets
		spec.Env = copyMap(spec.Env)
		for _, k := range spec.SecretKeys {
			if _, ok := spec.Env[k]; ok {
				spec.Env[k] = maskedValue
			}
		}
	}
	s := AppStatus{
		Spec: spec, Runtime: a.rt.Name(), Running: len(a.instances), Starting: a.starting,
		InFlight: a.inflight.Load(), LastError: a.lastErr,
		URL:       p.appURL(a.spec.Name),
		Instances: []InstanceStatus{},
		Kind:      kindOf(a.spec),
	}
	if c, ok := p.connectionOf(a.spec); ok {
		m := c.masked()
		s.Connection = &m
	}
	for _, in := range a.instances {
		s.Instances = append(s.Instances, InstanceStatus{
			ID: in.ID, Addr: in.Addr, InFlight: in.inflight.Load(),
			Uptime: time.Since(in.started).Round(time.Second).String(),
		})
	}
	return s
}

// maskedValue stands in for secret values in the API.
const maskedValue = "••••••"

func copyMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (p *Platform) appURL(name string) string {
	u := "http://" + name + "." + p.cfg.Domain
	if p.cfg.PublicPort != 0 && p.cfg.PublicPort != 80 {
		u += ":" + strconv.Itoa(p.cfg.PublicPort)
	}
	return u
}

// Status returns one app's live state.
func (p *Platform) Status(name string) (AppStatus, bool) {
	a := p.get(name)
	if a == nil {
		return AppStatus{}, false
	}
	return p.status(a), true
}

// List returns every app's live state, sorted by name.
func (p *Platform) List() []AppStatus {
	p.mu.RLock()
	apps := make([]*app, 0, len(p.apps))
	for _, a := range p.apps {
		apps = append(apps, a)
	}
	p.mu.RUnlock()
	out := make([]AppStatus, 0, len(apps))
	for _, a := range apps {
		out = append(out, p.status(a))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Spec.Name < out[j].Spec.Name })
	idx := map[string]int{}
	for i, s := range out {
		idx[s.Spec.Name] = i
	}
	for _, s := range out {
		for _, l := range s.Spec.Links {
			if i, ok := idx[l]; ok {
				out[i].UsedBy = append(out[i].UsedBy, s.Spec.Name)
			}
		}
	}
	return out
}

// ---- persistence ------------------------------------------------------------

type savedState struct {
	Apps       []AppSpec             `json:"apps"`
	Stacks     []stackRecord         `json:"stacks"`
	Envs       []Environment         `json:"environments,omitempty"`
	Registries []Registry            `json:"registries,omitempty"`
	Releases   map[string][]*Release `json:"releases,omitempty"`
	Routes     map[string]*Route     `json:"routes,omitempty"`
	Sources    []Source              `json:"sources,omitempty"`
	Tokens     map[string]string     `json:"telemetry_tokens,omitempty"`
	RelayUsers []RelayUser           `json:"relay_users,omitempty"`
	Tunnel     TunnelConfig          `json:"tunnel,omitempty"`
	LogStorage cloud.StorageConfig   `json:"log_storage,omitempty"`
}

func (p *Platform) save() error {
	if p.cfg.StatePath == "" {
		return nil
	}
	p.saveMu.Lock()
	defer p.saveMu.Unlock()
	st := savedState{Apps: []AppSpec{}, Stacks: []stackRecord{}}
	p.mu.RLock()
	for _, a := range p.apps {
		st.Apps = append(st.Apps, a.spec)
	}
	for _, r := range p.stacks {
		st.Stacks = append(st.Stacks, *r)
	}
	for _, r := range p.registries {
		st.Registries = append(st.Registries, *r)
	}
	for _, e := range p.envs {
		c := *e
		c.Variables, c.Secrets = copyMap(e.Variables), copyMap(e.Secrets)
		st.Envs = append(st.Envs, c)
	}
	sort.Slice(st.Envs, func(i, j int) bool { return st.Envs[i].Name < st.Envs[j].Name })
	st.Releases, st.Routes = map[string][]*Release{}, map[string]*Route{}
	for k, v := range p.releases {
		cp := make([]*Release, len(v))
		for i, r := range v {
			c := *r
			cp[i] = &c
		}
		st.Releases[k] = cp
	}
	for k, v := range p.routes {
		c := *v
		st.Routes[k] = &c
	}
	st.Tokens = map[string]string{}
	for k, v := range p.tokens {
		st.Tokens[k] = v
	}
	for _, s := range p.sources {
		st.Sources = append(st.Sources, *s)
	}
	p.mu.RUnlock()
	st.LogStorage = p.logs.Storage()
	if p.relay != nil {
		p.relay.mu.Lock()
		for _, u := range p.relay.users {
			st.RelayUsers = append(st.RelayUsers, *u)
		}
		p.relay.mu.Unlock()
		sort.Slice(st.RelayUsers, func(i, j int) bool { return st.RelayUsers[i].Name < st.RelayUsers[j].Name })
	}
	p.agent.mu.Lock()
	st.Tunnel = p.agent.cfg
	p.agent.mu.Unlock()
	sort.Slice(st.Apps, func(i, j int) bool { return st.Apps[i].Name < st.Apps[j].Name })
	sort.Slice(st.Stacks, func(i, j int) bool { return st.Stacks[i].Name < st.Stacks[j].Name })

	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := p.cfg.StatePath + ".tmp"
	if err := os.MkdirAll(filepath.Dir(p.cfg.StatePath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.cfg.StatePath) // atomic replace
}

func (p *Platform) restore() error {
	if p.cfg.StatePath == "" {
		return nil
	}
	b, err := os.ReadFile(p.cfg.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	var st savedState
	if err := json.Unmarshal(b, &st); err != nil {
		// files written before stacks existed hold a plain list of apps
		if err2 := json.Unmarshal(b, &st.Apps); err2 != nil {
			return fmt.Errorf("read %s: %w", p.cfg.StatePath, err)
		}
	}
	for _, s := range st.Apps {
		if err := s.Normalize(); err != nil {
			p.log.Printf("skip saved app %s: %v", s.Name, err)
			continue
		}
		rt, err := p.runtimeFor(s)
		if err != nil {
			p.log.Printf("skip saved app %s: %v", s.Name, err)
			continue
		}
		a := newApp(p, s, rt)
		p.apps[s.Name] = a
		a.scaleTo(s.MinScale) // min 0 apps wake on their first request
	}
	for _, r := range st.Registries {
		r := r
		p.registries = append(p.registries, &r)
	}
	if st.Releases != nil {
		p.releases = st.Releases
		for _, list := range p.releases {
			for _, r := range list {
				if r.Status == "building" || r.Status == "deploying" {
					r.Status, r.Error = "failed", "interrupted by a restart"
				}
			}
		}
	}
	if st.Routes != nil {
		p.routes = st.Routes
	}
	p.rebuildDomainIndexLocked()
	for _, s := range st.Sources {
		s := s
		s.Building = false
		p.sources[s.ID] = &s
	}
	p.logs.SetStorage(st.LogStorage)
	for k, v := range st.Tokens {
		p.tokens[k] = v
	}
	p.agent.cfg = st.Tunnel
	if p.relay != nil {
		for _, u := range st.RelayUsers {
			u := u
			p.relay.users[u.Name] = &u
		}
	}
	for _, e := range st.Envs {
		e := e
		p.envs[e.Name] = &e
	}
	for _, r := range st.Stacks {
		r := r
		if r.Status == "deploying" {
			r.Status, r.Error = "failed", "a restart interrupted this deploy; deploy the stack again"
		}
		p.stacks[r.Name] = &r
	}
	p.log.Printf("restored %d app(s) and %d stack(s)", len(p.apps), len(p.stacks))
	return nil
}

// SetPortal serves h for requests to host (the tunnel-token sign-up site).
// Call it before serving.
func (p *Platform) SetPortal(host string, h http.Handler) {
	p.portalHost, p.portal = strings.ToLower(host), h
}
