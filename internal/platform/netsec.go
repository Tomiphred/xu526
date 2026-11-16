package platform

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Network security: every port this computer listens on, who can reach it
// (only this computer, or the network), who is connected to it right now,
// and warnings for risky exposure. Programs shared through xu tunnel also
// have every request recorded, since xu526 forwards them.

// NetConn is one connection into a listening port.
type NetConn struct {
	Remote string `json:"remote"` // ip:port of the other side
	Where  string `json:"where"`  // this computer | local network | internet
}

// NetPort is a listening port with its exposure and connections.
type NetPort struct {
	Port        int       `json:"port"`
	Listen      string    `json:"listen"`
	Process     string    `json:"process,omitempty"`
	PID         int       `json:"pid,omitempty"`
	Side        string    `json:"side"`                // this computer | Windows
	Exposure    string    `json:"exposure"`            // local (this computer only) | network
	Owner       string    `json:"owner,omitempty"`     // xu526, or the xu526 app it belongs to
	SharedAs    string    `json:"shared_as,omitempty"` // shared through xu tunnel as this app
	PublicURL   string    `json:"public_url,omitempty"`
	Service     string    `json:"service,omitempty"` // what usually listens on this port
	Connections []NetConn `json:"connections"`
	Internet    int       `json:"internet"` // connections from the internet
	LAN         int       `json:"lan"`      // from the local network
	Risk        string    `json:"risk"`     // high | medium | low | none
	Warnings    []string  `json:"warnings"`
}

// NetReport is what the Network security page shows.
type NetReport struct {
	Ports    []NetPort `json:"ports"`
	Scanned  time.Time `json:"scanned"`
	Exposed  int       `json:"exposed"`  // ports reachable from the network
	Internet int       `json:"internet"` // connections from the internet right now
	High     int       `json:"high"`     // ports with a high-risk warning
	Notes    []string  `json:"notes,omitempty"`
}

// sensitivePorts are services that shouldn't be reachable from a network.
var sensitivePorts = map[int]struct {
	name string
	risk string
}{
	22: {"SSH", "medium"}, 23: {"Telnet", "high"}, 445: {"Windows file sharing (SMB)", "medium"}, 3389: {"Remote Desktop", "high"},
	5900: {"VNC remote desktop", "high"}, 2375: {"Docker API without TLS", "high"}, 2376: {"Docker API", "medium"},
	5432: {"PostgreSQL", "high"}, 3306: {"MySQL", "high"}, 27017: {"MongoDB", "high"}, 6379: {"Redis", "high"},
	9200: {"Elasticsearch", "high"}, 11211: {"Memcached", "high"}, 5984: {"CouchDB", "high"}, 8086: {"InfluxDB", "medium"},
	9229: {"Node.js debugger", "high"}, 5555: {"Android debug bridge", "high"}, 1433: {"SQL Server", "high"},
	5672: {"RabbitMQ", "medium"}, 15672: {"RabbitMQ admin", "medium"}, 9090: {"Prometheus", "medium"}, 8888: {"Jupyter", "high"},
}

// classifyIP says where a remote address is.
func classifyIP(ip string) string {
	a := net.ParseIP(strings.Trim(ip, "[]"))
	switch {
	case a == nil:
		return "internet"
	case a.IsLoopback() || a.IsUnspecified():
		return "this computer"
	case a.IsPrivate() || a.IsLinkLocalUnicast():
		return "local network"
	}
	if a4 := a.To4(); a4 != nil && a4[0] == 100 && a4[1]&0xC0 == 64 { // 100.64.0.0/10 (CGNAT, Tailscale)
		return "local network"
	}
	return "internet"
}

type rawConn struct {
	localPort int
	remote    string
}

// NetworkReport scans listening ports and current connections.
func (p *Platform) NetworkReport(ctx context.Context) NetReport {
	rep := NetReport{Scanned: time.Now(), Ports: []NetPort{}}
	own := p.ownPortsByApp()
	shared := map[string]string{} // upstream → app
	p.mu.RLock()
	for n, a := range p.apps {
		if a.spec.Upstream != "" {
			shared[a.spec.Upstream] = n
		}
	}
	p.mu.RUnlock()
	tun := p.TunnelStatus()

	add := func(side string, ls []rawListener, conns []rawConn) {
		byPort := map[int][]rawConn{}
		for _, c := range conns {
			byPort[c.localPort] = append(byPort[c.localPort], c)
		}
		seen := map[int]bool{}
		for _, l := range ls {
			if seen[l.port] {
				continue
			}
			seen[l.port] = true
			np := NetPort{Port: l.port, Listen: l.ip, Process: l.process, PID: l.pid, Side: side, Exposure: "network",
				Connections: []NetConn{}, Warnings: []string{}, Risk: "none"}
			if ip := net.ParseIP(l.ip); ip != nil && ip.IsLoopback() {
				np.Exposure = "local"
			}
			if s, ok := sensitivePorts[l.port]; ok {
				np.Service = s.name
			}
			if side == "this computer" {
				if l.pid == os.Getpid() {
					np.Owner = "xu526"
				} else if app := own[l.port]; app != "" {
					np.Owner = app
				}
			}
			up := localUpstream(l.ip, l.port)
			if side == "Windows" {
				up = net.JoinHostPort("127.0.0.1", strconv.Itoa(l.port))
			}
			if app := shared[up]; app != "" {
				np.SharedAs, np.PublicURL = app, tun.URLs[app]
			}
			for _, c := range byPort[l.port] {
				host, _, _ := net.SplitHostPort(c.remote)
				w := classifyIP(host)
				np.Connections = append(np.Connections, NetConn{Remote: c.remote, Where: w})
				switch w {
				case "internet":
					np.Internet++
				case "local network":
					np.LAN++
				}
			}
			assessPort(&np)
			rep.Ports = append(rep.Ports, np)
		}
	}
	ls, _ := listListeners()
	add("this computer", ls, listConnections(ctx))
	if isWSL() {
		if wl, err := windowsListeners(ctx); err == nil {
			wc, _ := netstatConnections(ctx, "/mnt/c/Windows/System32/netstat.exe")
			add("Windows", wl, wc)
		}
		rep.Notes = append(rep.Notes, "xu526 runs in WSL: Windows' own ports are listed too. Whether the network can reach them also depends on Windows Firewall.")
	}

	rank := map[string]int{"high": 0, "medium": 1, "low": 2, "none": 3}
	sort.SliceStable(rep.Ports, func(i, j int) bool {
		a, b := rep.Ports[i], rep.Ports[j]
		if rank[a.Risk] != rank[b.Risk] {
			return rank[a.Risk] < rank[b.Risk]
		}
		if a.Exposure != b.Exposure {
			return a.Exposure == "network"
		}
		return a.Port < b.Port
	})
	for _, np := range rep.Ports {
		if np.Exposure == "network" {
			rep.Exposed++
		}
		rep.Internet += np.Internet
		if np.Risk == "high" {
			rep.High++
		}
	}
	return rep
}

// assessPort sets a port's risk and warnings.
func assessPort(np *NetPort) {
	if np.Risk == "" {
		np.Risk = "none"
	}
	raise := func(r string) {
		order := map[string]int{"none": 0, "low": 1, "medium": 2, "high": 3}
		if order[r] > order[np.Risk] {
			np.Risk = r
		}
	}
	s, sensitive := sensitivePorts[np.Port]
	if np.Exposure == "network" {
		switch {
		case np.Owner == "xu526":
			np.Warnings = append(np.Warnings, "xu526 itself; it serves your apps.")
			raise("low")
		case sensitive:
			np.Warnings = append(np.Warnings, s.name+" is reachable from your network. If only this computer needs it, make it listen on 127.0.0.1, or block the port in the firewall.")
			raise(s.risk)
		default:
			np.Warnings = append(np.Warnings, "Anyone on the same network (Wi-Fi, office) can connect. Fine for a server you meant to expose; otherwise make it listen on 127.0.0.1.")
			raise("low")
		}
	}
	if np.Internet > 0 && np.SharedAs == "" {
		np.Warnings = append(np.Warnings, strconv.Itoa(np.Internet)+" connection(s) from the internet right now. Check that you expected them.")
		raise("high")
	}
	if np.SharedAs != "" {
		np.Warnings = append(np.Warnings, "Public on the internet through Xu Tunnel as "+np.SharedAs+". Its requests are listed below.")
		raise("low")
	}
}

// ownPortsByApp maps host ports to the xu526 apps that publish them.
func (p *Platform) ownPortsByApp() map[int]string {
	out := map[int]string{}
	p.mu.RLock()
	apps := make([]*app, 0, len(p.apps))
	for _, a := range p.apps {
		apps = append(apps, a)
	}
	p.mu.RUnlock()
	for _, a := range apps {
		if a.spec.Upstream != "" {
			continue
		}
		a.mu.Lock()
		for _, in := range a.instances {
			if host, port, err := net.SplitHostPort(in.Addr); err == nil && (host == "127.0.0.1" || host == "::1") {
				if n, _ := strconv.Atoi(port); n > 0 {
					out[n] = a.spec.Name
				}
			}
		}
		a.mu.Unlock()
	}
	return out
}

// ---- connections -------------------------------------------------------------------

func listConnections(ctx context.Context) []rawConn {
	switch goruntime.GOOS {
	case "linux":
		var out []rawConn
		for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
			b, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			for _, e := range parseProcTCP(b) {
				if e.state == "01" { // ESTABLISHED
					out = append(out, rawConn{localPort: e.localPort, remote: net.JoinHostPort(e.remoteIP, strconv.Itoa(e.remotePort))})
				}
			}
		}
		return out
	case "darwin":
		b, _ := exec.CommandContext(ctx, "lsof", "-nP", "-iTCP", "-sTCP:ESTABLISHED", "-Fn").Output()
		return parseLsofConns(b)
	case "windows":
		c, _ := netstatConnections(ctx, "netstat")
		return c
	}
	return nil
}

// parseLsofConns reads "n127.0.0.1:3000->127.0.0.1:52344" lines.
func parseLsofConns(b []byte) []rawConn {
	var out []rawConn
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "n") {
			continue
		}
		local, remote, ok := strings.Cut(line[1:], "->")
		if !ok {
			continue
		}
		if _, port, err := net.SplitHostPort(local); err == nil {
			n, _ := strconv.Atoi(port)
			out = append(out, rawConn{localPort: n, remote: remote})
		}
	}
	return out
}

func netstatConnections(ctx context.Context, netstat string) ([]rawConn, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var out []rawConn
	for _, proto := range []string{"TCP", "TCPv6"} {
		b, err := exec.CommandContext(ctx, netstat, "-ano", "-p", proto).Output()
		if err != nil {
			continue
		}
		out = append(out, parseNetstatConns(b)...)
	}
	return out, nil
}

func parseNetstatConns(b []byte) []rawConn {
	var out []rawConn
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || !strings.HasPrefix(f[0], "TCP") || f[3] != "ESTABLISHED" {
			continue
		}
		if _, port, err := net.SplitHostPort(f[1]); err == nil {
			n, _ := strconv.Atoi(port)
			out = append(out, rawConn{localPort: n, remote: f[2]})
		}
	}
	return out
}

// netsecAPI serves the Network security page.
//
//	GET /v1/netsec       ports, exposure, connections and warnings
func (p *Platform) netsecAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/netsec", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		writeJSON(w, http.StatusOK, p.NetworkReport(ctx))
	})
}
