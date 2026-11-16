package platform

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"xu526/internal/tunnel"
)

// The agent side: xu on someone's own computer keeps one outbound tunnel
// to a relay and publishes the apps marked "Share on the internet".

// TunnelConfig is how this computer reaches its relay.
type TunnelConfig struct {
	URL   string `json:"url,omitempty"`
	Token string `json:"token,omitempty"`
}

type agentState struct {
	mu        sync.Mutex
	cfg       TunnelConfig
	connected bool
	user      string
	pattern   string // the relay's address for shared apps, with {app} in it
	since     time.Time
	lastErr   string
	urls      map[string]string
	errs      map[string]string
	ctrl      *json.Encoder
	ctrlMu    sync.Mutex
	sess      *tunnel.Session
	kick      chan struct{}
	conns     chan net.Conn
}

// TunnelStatus is shown in the dashboard.
type TunnelStatus struct {
	Configured bool              `json:"configured"`
	URL        string            `json:"url"`
	Connected  bool              `json:"connected"`
	User       string            `json:"user,omitempty"`
	Since      time.Time         `json:"since,omitempty"`
	Error      string            `json:"error,omitempty"`
	Latency    string            `json:"latency,omitempty"`
	URLs       map[string]string `json:"urls"`
	Errors     map[string]string `json:"errors"`
	Relay      bool              `json:"relay"`             // this server is itself a relay
	Service    string            `json:"service,omitempty"` // the one-click tunnel service, if any
	// Pattern is a shared app's address with {app} for its name, once known.
	Pattern string `json:"pattern,omitempty"`
}

func newAgentState() *agentState {
	return &agentState{urls: map[string]string{}, errs: map[string]string{}, kick: make(chan struct{}, 1), conns: make(chan net.Conn, 64)}
}

func (ag *agentState) poke() {
	select {
	case ag.kick <- struct{}{}:
	default:
	}
}

// SetTunnelConfig connects (or with an empty URL, disconnects) this computer.
func (p *Platform) SetTunnelConfig(c TunnelConfig) error {
	c.URL = strings.TrimRight(strings.TrimSpace(c.URL), "/")
	ag := p.agent
	ag.mu.Lock()
	if strings.HasPrefix(c.Token, "••") || (c.Token == "" && c.URL == ag.cfg.URL) {
		c.Token = ag.cfg.Token
	}
	if c.URL != "" && (!strings.HasPrefix(c.URL, "http://") && !strings.HasPrefix(c.URL, "https://")) {
		ag.mu.Unlock()
		return errors.New("the relay address should start with https:// (or http:// while testing)")
	}
	if c.URL != "" && c.Token == "" {
		ag.mu.Unlock()
		return errors.New("enter the token the relay operator gave you")
	}
	if c.URL != ag.cfg.URL || c.Token != ag.cfg.Token { // addresses from the old relay no longer apply
		ag.urls, ag.errs, ag.user, ag.pattern = map[string]string{}, map[string]string{}, "", ""
	}
	ag.cfg = c
	ag.lastErr = ""
	sess := ag.sess
	ag.mu.Unlock()
	if sess != nil {
		sess.Close() // reconnect with the new settings
	}
	ag.poke()
	return p.save()
}

func (p *Platform) TunnelStatus() TunnelStatus {
	ag := p.agent
	ag.mu.Lock()
	defer ag.mu.Unlock()
	st := TunnelStatus{Configured: ag.cfg.URL != "", URL: ag.cfg.URL, Connected: ag.connected, User: ag.user,
		Since: ag.since, Error: ag.lastErr, URLs: map[string]string{}, Errors: map[string]string{}, Relay: p.relay != nil,
		Service: p.cfg.TunnelService}
	for k, v := range ag.urls {
		st.URLs[k] = v
	}
	for k, v := range ag.errs {
		st.Errors[k] = v
	}
	ag.mu.Unlock()
	if u, ok := p.tunnelURLFor("xu-pattern-probe"); ok {
		st.Pattern = strings.Replace(u, "xu-pattern-probe", "{app}", 1)
	}
	ag.mu.Lock()
	if ag.sess != nil && ag.connected {
		sess := ag.sess
		ag.mu.Unlock()
		if d, err := sess.Ping(2 * time.Second); err == nil {
			st.Latency = d.Round(time.Millisecond).String()
		}
		ag.mu.Lock()
	}
	return st
}

// SetTunnel shares an app through the relay (or stops sharing it).
func (p *Platform) SetTunnel(name string, on bool) error {
	a := p.get(name)
	if a == nil {
		return errors.New("no app " + name)
	}
	if on && !p.hasPort(a) {
		return errors.New("this service has no port, so there is nothing to share")
	}
	p.mu.Lock()
	r := p.routes[name]
	if r == nil {
		r = &Route{}
		p.routes[name] = r
	}
	r.Tunnel = on
	p.mu.Unlock()
	p.publishTunnels()
	verb := map[bool]string{true: "shared on the internet through the relay", false: "no longer shared through the relay"}[on]
	p.event(name, "deploy", "%s", verb)
	return p.save()
}

func (p *Platform) tunneled(name string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	r := p.routes[name]
	return r != nil && r.Tunnel && p.apps[name] != nil
}

// publishTunnels sends the relay the full list of shared apps.
func (p *Platform) publishTunnels() {
	p.mu.RLock()
	apps := []string{}
	for name, r := range p.routes {
		if r.Tunnel && p.apps[name] != nil {
			apps = append(apps, name)
		}
	}
	p.mu.RUnlock()
	sort.Strings(apps)
	ag := p.agent
	ag.mu.Lock()
	enc := ag.ctrl
	ag.mu.Unlock()
	if enc == nil {
		return
	}
	ag.ctrlMu.Lock()
	_ = enc.Encode(ctrlMsg{Type: "publish", Apps: apps})
	ag.ctrlMu.Unlock()
}

// agentLoop keeps the tunnel up, reconnecting with backoff.
func (p *Platform) agentLoop() {
	ag := p.agent
	go p.serveTunnelStreams()
	backoff := time.Second
	for {
		ag.mu.Lock()
		cfg := ag.cfg
		ag.mu.Unlock()
		if cfg.URL == "" {
			select {
			case <-ag.kick:
				continue
			case <-p.ctx.Done():
				return
			}
		}
		ctx, cancel := context.WithTimeout(p.ctx, 30*time.Second)
		sess, err := tunnel.Dial(ctx, cfg.URL, cfg.Token)
		cancel()
		if err != nil {
			ag.mu.Lock()
			ag.lastErr = err.Error()
			ag.mu.Unlock()
			select {
			case <-time.After(backoff):
			case <-ag.kick:
			case <-p.ctx.Done():
				return
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		ctrl, err := sess.Open()
		if err != nil {
			sess.Close()
			continue
		}
		ag.mu.Lock()
		ag.sess, ag.connected, ag.since, ag.lastErr = sess, true, time.Now(), ""
		ag.ctrl = json.NewEncoder(ctrl)
		ag.mu.Unlock()
		p.log.Printf("tunnel: connected to %s", cfg.URL)
		go p.readControl(ctrl)
		p.publishTunnels()
		go func() {
			for {
				st, err := sess.Accept()
				if err != nil {
					return
				}
				go p.handleTunnelStream(st)
			}
		}()
		select {
		case <-sess.Done():
		case <-p.ctx.Done():
			sess.Close()
		}
		ag.mu.Lock()
		ag.connected, ag.ctrl, ag.sess = false, nil, nil
		if err := sess.Err(); err != nil && !errors.Is(err, tunnel.ErrSessionClosed) {
			ag.lastErr = "connection lost: " + err.Error()
		}
		ag.mu.Unlock()
		p.log.Printf("tunnel: disconnected (%v)", sess.Err())
		select {
		case <-p.ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (p *Platform) readControl(st *tunnel.Stream) {
	dec := json.NewDecoder(bufio.NewReader(st))
	for {
		var m ctrlMsg
		if err := dec.Decode(&m); err != nil {
			return
		}
		if m.Type == "published" {
			ag := p.agent
			ag.mu.Lock()
			ag.user, ag.urls, ag.errs = m.User, m.URLs, m.Errors
			if m.Pattern != "" {
				ag.pattern = m.Pattern
			}
			if ag.urls == nil {
				ag.urls = map[string]string{}
			}
			if ag.errs == nil {
				ag.errs = map[string]string{}
			}
			ag.mu.Unlock()
		}
	}
}

type tunnelConn struct {
	*tunnel.Stream
	br  *bufio.Reader
	app string
}

func (c *tunnelConn) Read(b []byte) (int, error) { return c.br.Read(b) }

func (p *Platform) handleTunnelStream(st *tunnel.Stream) {
	_ = st.SetReadDeadline(time.Now().Add(15 * time.Second))
	br := bufio.NewReader(st)
	line, err := br.ReadBytes('\n')
	var hdr struct {
		App string `json:"app"`
	}
	if err != nil || json.Unmarshal(line, &hdr) != nil || hdr.App == "" {
		st.Close()
		return
	}
	_ = st.SetReadDeadline(time.Time{})
	select {
	case p.agent.conns <- &tunnelConn{Stream: st, br: br, app: hdr.App}:
	case <-p.ctx.Done():
		st.Close()
	}
}

type chanListener struct {
	ch   chan net.Conn
	done <-chan struct{}
}

func (l chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l chanListener) Close() error   { return nil }
func (l chanListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

type tunnelAppKey struct{}

// serveTunnelStreams answers visitors arriving through the relay with the
// normal app path (metrics, traces, logs and autoscaling all apply).
func (p *Platform) serveTunnelStreams() {
	srv := &http.Server{
		ReadHeaderTimeout: 15 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if tc, ok := c.(*tunnelConn); ok {
				return context.WithValue(ctx, tunnelAppKey{}, tc.app)
			}
			return ctx
		},
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			name, _ := r.Context().Value(tunnelAppKey{}).(string)
			a := p.get(name)
			if a == nil || !p.tunneled(name) {
				http.Error(w, name+" is not shared on this computer", http.StatusNotFound)
				return
			}
			if xff := r.Header.Get("X-Forwarded-For"); xff != "" { // the visitor, as seen by the relay
				r.RemoteAddr = net.JoinHostPort(strings.TrimSpace(strings.Split(xff, ",")[0]), "0")
			}
			p.serveApp(w, r, a)
		}),
	}
	_ = srv.Serve(chanListener{ch: p.agent.conns, done: p.ctx.Done()})
}
