package platform

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"xu526/internal/tunnel"
)

// The relay runs on a server with a public IP. Agents (xu on people's own
// computers, usually behind NAT) connect out to it and keep one tunnel open;
// visitors to https://<app>-<user>.<domain> are routed through that tunnel.

// RelayUser is someone allowed to connect an agent.
type RelayUser struct {
	Name      string    `json:"name"`
	TokenHash string    `json:"token_hash"`
	MaxApps   int       `json:"max_apps"`
	RateMBps  float64   `json:"rate_mbps"` // bandwidth across all their apps
	Created   time.Time `json:"created"`
}

type relayAgent struct {
	user     *RelayUser
	sess     *tunnel.Session
	limiter  *tunnel.Limiter
	since    time.Time
	remote   string
	apps     []string
	requests atomic.Uint64

	mu      sync.Mutex
	proxies map[string]*httputil.ReverseProxy
	trans   []*http.Transport
}

type hostRoute struct{ user, app string }

type relay struct {
	p      *Platform
	mu     sync.Mutex
	users  map[string]*RelayUser
	agents map[string]*relayAgent // by user
	hosts  map[string]hostRoute
}

var userRE = nameRE

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

func (rl *relay) host(app, user string) string { return app + "-" + user + "." + rl.p.cfg.Domain }

func (rl *relay) url(host string) string {
	if p := rl.p.cfg.HTTPSPort; p != 0 {
		if p == 443 {
			return "https://" + host
		}
		return "https://" + host + ":" + strconv.Itoa(p)
	}
	u := "http://" + host
	if pp := rl.p.cfg.PublicPort; pp != 0 && pp != 80 {
		u += ":" + strconv.Itoa(pp)
	}
	return u
}

// ---- users ----

// AddRelayUser creates a user and returns their token (shown once).
func (p *Platform) AddRelayUser(name string, maxApps int, rateMBps float64) (string, error) {
	rl := p.relay
	if rl == nil {
		return "", errors.New("this server is not a relay (the relay is xu526-server)")
	}
	if !userRE.MatchString(name) || len(name) > 20 {
		return "", errors.New("user name: 1-20 lowercase letters, digits and dashes")
	}
	if maxApps <= 0 {
		maxApps = 10
	}
	if rateMBps <= 0 {
		rateMBps = 10
	}
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	token := "xu_" + hex.EncodeToString(b)
	rl.mu.Lock()
	if _, exists := rl.users[name]; exists {
		rl.mu.Unlock()
		return "", fmt.Errorf("user %s already exists", name)
	}
	rl.users[name] = &RelayUser{Name: name, TokenHash: hashToken(token), MaxApps: maxApps, RateMBps: rateMBps, Created: time.Now()}
	rl.mu.Unlock()
	p.event(name, "deploy", "relay user created")
	return token, p.save()
}

// RemoveRelayUser deletes a user and disconnects their agent.
func (p *Platform) RemoveRelayUser(name string) error {
	rl := p.relay
	if rl == nil {
		return errors.New("this server is not a relay")
	}
	rl.mu.Lock()
	_, ok := rl.users[name]
	delete(rl.users, name)
	ag := rl.agents[name]
	for h, r := range rl.hosts {
		if r.user == name {
			delete(rl.hosts, h)
		}
	}
	rl.mu.Unlock()
	if !ok {
		return fmt.Errorf("no user %q", name)
	}
	if ag != nil {
		ag.sess.Close()
	}
	return p.save()
}

// RotateRelayToken gives a user a new token and disconnects their agent,
// which must reconnect with it. The name, limits and public addresses stay.
func (p *Platform) RotateRelayToken(name string) (string, error) {
	rl := p.relay
	if rl == nil {
		return "", errors.New("this server is not a relay")
	}
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	token := "xu_" + hex.EncodeToString(b)
	rl.mu.Lock()
	u, ok := rl.users[name]
	if ok {
		u.TokenHash = hashToken(token)
	}
	ag := rl.agents[name]
	rl.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("no user %q", name)
	}
	if ag != nil {
		ag.sess.Close()
	}
	p.event(name, "deploy", "relay token rotated")
	return token, p.save()
}

// RelayUser reports one user's state.
func (p *Platform) RelayUser(name string) (RelayUserStatus, bool) {
	for _, u := range p.RelayUsers() {
		if u.Name == name {
			return u, true
		}
	}
	return RelayUserStatus{}, false
}

// RelayAgentURL is the address agents connect to ("" when not a relay).
func (p *Platform) RelayAgentURL() string { return p.relayAgentURL() }

// RelayUserStatus is a user with their connection state.
type RelayUserStatus struct {
	Name     string            `json:"name"`
	MaxApps  int               `json:"max_apps"`
	RateMBps float64           `json:"rate_mbps"`
	Created  time.Time         `json:"created"`
	Online   bool              `json:"online"`
	Since    time.Time         `json:"since,omitempty"`
	Remote   string            `json:"remote,omitempty"`
	Apps     map[string]string `json:"apps"` // app → public URL
	Requests uint64            `json:"requests"`
	BytesIn  uint64            `json:"bytes_in"`
	BytesOut uint64            `json:"bytes_out"`
	Streams  int               `json:"streams"`
}

func (p *Platform) RelayUsers() []RelayUserStatus {
	rl := p.relay
	out := []RelayUserStatus{}
	if rl == nil {
		return out
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	for _, u := range rl.users {
		st := RelayUserStatus{Name: u.Name, MaxApps: u.MaxApps, RateMBps: u.RateMBps, Created: u.Created, Apps: map[string]string{}}
		for h, r := range rl.hosts {
			if r.user == u.Name {
				st.Apps[r.app] = rl.url(h)
			}
		}
		if ag := rl.agents[u.Name]; ag != nil {
			st.Online, st.Since, st.Remote = true, ag.since, ag.remote
			st.Requests, st.BytesIn, st.BytesOut = ag.requests.Load(), ag.sess.BytesIn.Load(), ag.sess.BytesOut.Load()
			st.Streams = ag.sess.NumStreams()
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ---- agent connections ----

func (p *Platform) relayAccept(w http.ResponseWriter, r *http.Request) {
	rl := p.relay
	if rl == nil {
		http.NotFound(w, r)
		return
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	h := hashToken(tok)
	rl.mu.Lock()
	var user *RelayUser
	for _, u := range rl.users {
		if u.TokenHash == h {
			user = u
		}
	}
	rl.mu.Unlock()
	if user == nil || tok == "" {
		http.Error(w, "unknown token: ask the relay operator for one", http.StatusUnauthorized)
		return
	}
	sess, err := tunnel.Upgrade(w, r)
	if err != nil {
		return
	}
	sess.MaxStreams = 256
	ag := &relayAgent{user: user, sess: sess, limiter: tunnel.NewLimiter(user.RateMBps * (1 << 20)),
		since: time.Now(), remote: clientIP(r), proxies: map[string]*httputil.ReverseProxy{}}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		ag.remote = strings.TrimSpace(strings.Split(xff, ",")[0])
	}

	rl.mu.Lock()
	old := rl.agents[user.Name]
	rl.agents[user.Name] = ag // the newest connection wins
	rl.mu.Unlock()
	if old != nil {
		old.sess.Close()
	}
	p.event(user.Name, "ready", "agent connected from %s", ag.remote)

	go rl.control(ag)
	<-sess.Done()

	rl.mu.Lock()
	if rl.agents[user.Name] == ag {
		delete(rl.agents, user.Name)
	}
	rl.mu.Unlock()
	ag.mu.Lock()
	for _, t := range ag.trans {
		t.CloseIdleConnections()
	}
	ag.mu.Unlock()
	p.event(user.Name, "stopped", "agent disconnected (%v)", sess.Err())
}

type ctrlMsg struct {
	Type    string            `json:"type"`
	Apps    []string          `json:"apps,omitempty"`
	URLs    map[string]string `json:"urls,omitempty"`
	Errors  map[string]string `json:"errors,omitempty"`
	User    string            `json:"user,omitempty"`
	Pattern string            `json:"pattern,omitempty"` // published: the address of a shared app, {app} for its name
	Message string            `json:"message,omitempty"`
}

// control reads the agent's control stream: it lists the apps to publish.
func (rl *relay) control(ag *relayAgent) {
	st, err := ag.sess.Accept() // the agent opens its control stream first
	if err != nil {
		return
	}
	defer st.Close()
	dec := json.NewDecoder(bufio.NewReader(st))
	enc := json.NewEncoder(st)
	for {
		var m ctrlMsg
		if err := dec.Decode(&m); err != nil {
			ag.sess.Close()
			return
		}
		if m.Type != "publish" {
			continue
		}
		reply := ctrlMsg{Type: "published", User: ag.user.Name, URLs: map[string]string{}, Errors: map[string]string{},
			Pattern: rl.url(rl.host("{app}", ag.user.Name))}
		rl.mu.Lock()
		for h, r := range rl.hosts {
			if r.user == ag.user.Name {
				delete(rl.hosts, h)
			}
		}
		var apps []string
		for _, app := range m.Apps {
			host := rl.host(app, ag.user.Name)
			switch {
			case !nameRE.MatchString(app):
				reply.Errors[app] = "invalid app name"
			case len(apps) >= ag.user.MaxApps:
				reply.Errors[app] = fmt.Sprintf("your plan allows %d public apps", ag.user.MaxApps)
			case rl.p.get(app+"-"+ag.user.Name) != nil:
				reply.Errors[app] = "that address is taken on the relay"
			default:
				rl.hosts[host] = hostRoute{user: ag.user.Name, app: app}
				reply.URLs[app] = rl.url(host)
				apps = append(apps, app)
			}
		}
		ag.apps = apps
		rl.mu.Unlock()
		if err := enc.Encode(reply); err != nil {
			ag.sess.Close()
			return
		}
		rl.p.event(ag.user.Name, "deploy", "publishing %d app(s): %s", len(apps), strings.Join(apps, ", "))
	}
}

// serve routes a visitor into a tunnel. It returns false when host isn't a
// tunnel address.
func (rl *relay) serve(w http.ResponseWriter, r *http.Request, host string) bool {
	rl.mu.Lock()
	route, ok := rl.hosts[strings.ToLower(host)]
	ag := rl.agents[route.user]
	rl.mu.Unlock()
	if !ok {
		return false
	}
	if ag == nil {
		offlinePage(w, route.app)
		return true
	}
	ag.requests.Add(1)
	ag.proxy(route.app).ServeHTTP(w, r)
	return true
}

func (ag *relayAgent) proxy(app string) *httputil.ReverseProxy {
	ag.mu.Lock()
	defer ag.mu.Unlock()
	if px := ag.proxies[app]; px != nil {
		return px
	}
	header, _ := json.Marshal(map[string]string{"app": app})
	header = append(header, '\n')
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			st, err := ag.sess.Open()
			if err != nil {
				return nil, err
			}
			if _, err := st.Write(header); err != nil {
				st.Close()
				return nil, err
			}
			return tunnel.Limit(st, ag.limiter), nil
		},
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     60 * time.Second,
		ForceAttemptHTTP2:   false,
	}
	target := &url.URL{Scheme: "http", Host: "tunnel"}
	px := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
		},
		Transport:     tr,
		FlushInterval: -1, // stream responses (server-sent events, long polls)
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, tunnel.ErrTooManyStreams) {
				http.Error(w, "this app is getting too many simultaneous visitors; try again shortly", http.StatusServiceUnavailable)
				return
			}
			offlinePage(w, app)
		},
	}
	ag.proxies[app] = px
	ag.trans = append(ag.trans, tr)
	return px
}

func offlinePage(w http.ResponseWriter, app string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Retry-After", "30")
	w.WriteHeader(http.StatusBadGateway)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>%[1]s is offline</title>
<body style="font:16px/1.5 system-ui,sans-serif;max-width:34rem;margin:15vh auto;padding:0 1rem;color:#16212b">
<h1 style="font-size:1.4rem">%[1]s is offline right now</h1>
<p>This app runs on its owner's computer, which isn't connected at the moment. It may be asleep or offline.
Try again in a little while.</p></body>`, html.EscapeString(app))
}

// relayAgentURL is the address agents connect to.
func (p *Platform) relayAgentURL() string {
	if p.relay == nil {
		return ""
	}
	return p.relay.url("xu." + p.cfg.Domain)
}
