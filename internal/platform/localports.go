package platform

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The Tunnel section: programs listening on this computer, found by
// scanning its ports, that can be shared through xu526 tunnel without
// running them on xu526. Sharing one creates a forwarding app (Upstream)
// that the router and the tunnel reach like any other app.

// LocalPort is a program listening on a TCP port.
type LocalPort struct {
	Port     int    `json:"port"`
	Listen   string `json:"listen"` // the address it listens on, e.g. 127.0.0.1 or 0.0.0.0
	PID      int    `json:"pid,omitempty"`
	Process  string `json:"process,omitempty"`
	Side     string `json:"side"`               // "this computer", or "Windows" for programs outside WSL
	Upstream string `json:"upstream,omitempty"` // where xu526 forwards to; "" when it can't reach it
	HTTP     bool   `json:"http"`               // answers HTTP, so it can be shared
	Title    string `json:"title,omitempty"`    // the page's <title>
	Server   string `json:"server,omitempty"`   // its Server header
	Hint     string `json:"hint,omitempty"`     // why it can't be shared, and how to fix that
	SharedAs string `json:"shared_as,omitempty"`
}

// rawListener is one LISTEN socket before probing.
type rawListener struct {
	ip      string
	port    int
	pid     int
	process string
}

// ScanLocal lists programs listening on this computer (and, under WSL,
// on Windows too), skipping xu526 itself and its own apps.
func (p *Platform) ScanLocal(ctx context.Context) []LocalPort {
	own := p.ownPorts()
	var found []LocalPort
	seen := map[string]bool{}
	local, _ := listListeners()
	localPorts := map[int]bool{}
	for _, l := range local {
		localPorts[l.port] = true
		if own[l.port] || l.pid == os.Getpid() || systemProcess(l.process) {
			continue
		}
		k := "l" + strconv.Itoa(l.port)
		if seen[k] {
			continue
		}
		seen[k] = true
		lp := LocalPort{Port: l.port, Listen: l.ip, PID: l.pid, Process: l.process, Side: "this computer"}
		lp.Upstream = localUpstream(l.ip, l.port)
		found = append(found, lp)
	}
	if isWSL() {
		win, err := windowsListeners(ctx)
		gw := wslHostIP()
		for _, l := range win {
			if err != nil || own[l.port] || localPorts[l.port] || systemProcess(l.process) || strings.EqualFold(l.process, "wslrelay.exe") {
				continue
			}
			k := "w" + strconv.Itoa(l.port)
			if seen[k] {
				continue
			}
			seen[k] = true
			lp := LocalPort{Port: l.port, Listen: l.ip, PID: l.pid, Process: l.process, Side: "Windows"}
			// Mirrored networking shares localhost with Windows; otherwise only
			// programs listening on all addresses are reachable, at the host's IP.
			switch {
			case canDial(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(l.port))):
				lp.Upstream = net.JoinHostPort("127.0.0.1", strconv.Itoa(l.port))
			case gw != "" && (l.ip == "0.0.0.0" || l.ip == "::") && canDial(ctx, net.JoinHostPort(gw, strconv.Itoa(l.port))):
				lp.Upstream = net.JoinHostPort(gw, strconv.Itoa(l.port))
			default:
				lp.Hint = "xu526 runs in WSL and can't reach this Windows program. Turn on mirrored networking: add networkingMode=mirrored under [wsl2] in %UserProfile%\\.wslconfig, run wsl --shutdown, and start WSL again."
			}
			found = append(found, lp)
		}
	}

	// Which ones answer HTTP (the tunnel carries HTTP).
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for i := range found {
		if found[i].Upstream == "" {
			continue
		}
		wg.Add(1)
		go func(lp *LocalPort) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			probeHTTP(ctx, lp)
		}(&found[i])
	}
	wg.Wait()

	shared := map[string]string{}
	p.mu.RLock()
	for n, a := range p.apps {
		if a.spec.Upstream != "" {
			shared[a.spec.Upstream] = n
		}
	}
	p.mu.RUnlock()
	for i := range found {
		found[i].SharedAs = shared[found[i].Upstream]
		if found[i].Upstream != "" && !found[i].HTTP && found[i].Hint == "" {
			found[i].Hint = "It doesn't answer HTTP, and Xu Tunnel carries web traffic only."
		}
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].HTTP != found[j].HTTP {
			return found[i].HTTP
		}
		if found[i].Side != found[j].Side {
			return found[i].Side == "this computer"
		}
		return found[i].Port < found[j].Port
	})
	return found
}

// ownPorts are ports xu526 and its apps listen on.
func (p *Platform) ownPorts() map[int]bool {
	own := map[int]bool{}
	for _, port := range []int{p.cfg.PublicPort, p.cfg.HTTPSPort, p.cfg.CollectorPort} {
		if port > 0 {
			own[port] = true
		}
	}
	p.mu.RLock()
	apps := make([]*app, 0, len(p.apps))
	for _, a := range p.apps {
		apps = append(apps, a)
	}
	p.mu.RUnlock()
	for _, a := range apps {
		if a.spec.Upstream != "" {
			continue // shared programs stay listed, marked as shared
		}
		a.mu.Lock()
		for _, in := range a.instances {
			if _, port, err := net.SplitHostPort(in.Addr); err == nil {
				if n, _ := strconv.Atoi(port); n > 0 {
					own[n] = true
				}
			}
		}
		a.mu.Unlock()
	}
	return own
}

func localUpstream(ip string, port int) string {
	switch ip {
	case "0.0.0.0", "127.0.0.1", "":
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	case "::", "::1":
		return net.JoinHostPort("::1", strconv.Itoa(port))
	}
	return net.JoinHostPort(ip, strconv.Itoa(port))
}

func canDial(ctx context.Context, addr string) bool {
	d := net.Dialer{Timeout: 400 * time.Millisecond}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

var titleRE = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

// probeHTTP checks whether a port answers HTTP, and reads its page title.
func probeHTTP(ctx context.Context, lp *LocalPort) {
	ctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+lp.Upstream+"/", nil)
	req.Header.Set("User-Agent", "xu526-scan")
	client := &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	lp.HTTP = true
	lp.Server = resp.Header.Get("Server")
	if strings.Contains(resp.Header.Get("Content-Type"), "html") {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if m := titleRE.FindSubmatch(b); m != nil {
			t := strings.Join(strings.Fields(html.UnescapeString(string(m[1]))), " ")
			if len(t) > 80 {
				t = t[:80] + "…"
			}
			lp.Title = t
		}
	}
}

// systemProcess hides the operating system's own listeners.
func systemProcess(name string) bool {
	switch strings.ToLower(strings.TrimSuffix(strings.ToLower(name), ".exe")) {
	case "system", "svchost", "lsass", "wininit", "services", "spoolsv", "smss", "csrss", "winlogon", "lsaiso",
		"systemd", "systemd-resolved", "systemd-resolve", "sshd", "cupsd", "rpcbind", "avahi-daemon", "chronyd", "dnsmasq",
		"launchd", "mdnsresponder", "rapportd", "controlcenter", "sharingd", "airplayxpchelper", "remoted", "netbiosd",
		"dockerd", "containerd", "xu526", "xu526-service", "xu526-server":
		return true
	}
	return false
}

// ---- listing listeners ---------------------------------------------------------

func listListeners() ([]rawListener, error) {
	switch goruntime.GOOS {
	case "linux":
		return procListeners("/proc")
	case "darwin":
		out, err := exec.Command("lsof", "-nP", "-iTCP", "-sTCP:LISTEN", "-FpcnT").Output()
		if err != nil && len(out) == 0 {
			return nil, err
		}
		return parseLsof(out), nil
	case "windows":
		return netstatListeners(context.Background(), "netstat", "tasklist")
	}
	return nil, errors.New("scanning isn't supported on " + goruntime.GOOS)
}

// procListeners reads LISTEN sockets from /proc/net/tcp{,6} and finds
// their processes through /proc/<pid>/fd.
func procListeners(proc string) ([]rawListener, error) {
	var out []rawListener
	inodes := map[string]int{} // socket inode → index in out
	for _, f := range []string{"net/tcp", "net/tcp6"} {
		b, err := os.ReadFile(filepath.Join(proc, f))
		if err != nil {
			continue
		}
		for _, l := range parseProcNet(b) {
			inodes[l.inode] = len(out)
			out = append(out, l.rawListener)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	pids, _ := filepath.Glob(filepath.Join(proc, "[0-9]*"))
	for _, dir := range pids {
		fds, err := os.ReadDir(filepath.Join(dir, "fd"))
		if err != nil {
			continue
		}
		pid, _ := strconv.Atoi(filepath.Base(dir))
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(dir, "fd", fd.Name()))
			if err != nil || !strings.HasPrefix(link, "socket:[") {
				continue
			}
			if i, ok := inodes[strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")]; ok && out[i].pid == 0 {
				comm, _ := os.ReadFile(filepath.Join(dir, "comm"))
				out[i].pid, out[i].process = pid, strings.TrimSpace(string(comm))
			}
		}
	}
	return out, nil
}

type procNetEntry struct {
	rawListener
	inode string
}

// parseProcNet reads LISTEN (state 0A) entries of /proc/net/tcp or tcp6.
func parseProcNet(b []byte) []procNetEntry {
	var out []procNetEntry
	for _, e := range parseProcTCP(b) {
		if e.state == "0A" {
			out = append(out, procNetEntry{rawListener{ip: e.localIP, port: e.localPort}, e.inode})
		}
	}
	return out
}

type procTCP struct {
	localIP, remoteIP     string
	localPort, remotePort int
	state, inode          string
}

// parseProcTCP reads every socket in /proc/net/tcp or tcp6.
func parseProcTCP(b []byte) []procTCP {
	var out []procTCP
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Scan() // header
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 10 {
			continue
		}
		lip, lport, ok1 := procAddr(f[1])
		rip, rport, ok2 := procAddr(f[2])
		if !ok1 || !ok2 {
			continue
		}
		out = append(out, procTCP{localIP: lip, localPort: lport, remoteIP: rip, remotePort: rport, state: f[3], inode: f[9]})
	}
	return out
}

// procAddr decodes "0100007F:0BB8" (or the 32-hex-digit IPv6 form).
func procAddr(s string) (string, int, bool) {
	hostHex, portHex, ok := strings.Cut(s, ":")
	if !ok {
		return "", 0, false
	}
	port, err := strconv.ParseUint(portHex, 16, 16)
	if err != nil {
		return "", 0, false
	}
	raw, err := hex.DecodeString(hostHex)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return "", 0, false
	}
	// /proc stores each 32-bit word in host (little-endian) order.
	for i := 0; i+4 <= len(raw); i += 4 {
		raw[i], raw[i+1], raw[i+2], raw[i+3] = raw[i+3], raw[i+2], raw[i+1], raw[i]
	}
	ip := net.IP(raw)
	str := ip.String()
	if ip.To4() != nil && len(raw) == 16 { // ::ffff:a.b.c.d
		str = ip.To4().String()
	}
	return str, int(port), true
}

// parseLsof reads `lsof -FpcnT` output.
func parseLsof(b []byte) []rawListener {
	var out []rawListener
	pid, cmd := 0, ""
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			pid, _ = strconv.Atoi(line[1:])
		case 'c':
			cmd = line[1:]
		case 'n':
			host, port, err := net.SplitHostPort(line[1:])
			if err != nil {
				continue
			}
			if host == "*" {
				host = "0.0.0.0"
			}
			n, _ := strconv.Atoi(port)
			out = append(out, rawListener{ip: strings.Trim(host, "[]"), port: n, pid: pid, process: cmd})
		}
	}
	return out
}

// netstatListeners runs Windows' netstat and tasklist (also from WSL).
func netstatListeners(ctx context.Context, netstat, tasklist string) ([]rawListener, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var out []rawListener
	for _, proto := range []string{"TCP", "TCPv6"} {
		b, err := exec.CommandContext(ctx, netstat, "-ano", "-p", proto).Output()
		if err != nil {
			if proto == "TCP" {
				return nil, fmt.Errorf("netstat: %w", err)
			}
			continue
		}
		out = append(out, parseNetstat(b)...)
	}
	names := map[int]string{}
	if b, err := exec.CommandContext(ctx, tasklist, "/FO", "CSV", "/NH").Output(); err == nil {
		r := csv.NewReader(bytes.NewReader(b))
		r.FieldsPerRecord = -1
		for {
			rec, err := r.Read()
			if err != nil {
				break
			}
			if len(rec) >= 2 {
				if pid, err := strconv.Atoi(strings.TrimSpace(rec[1])); err == nil {
					names[pid] = rec[0]
				}
			}
		}
	}
	for i := range out {
		out[i].process = names[out[i].pid]
	}
	return out, nil
}

// parseNetstat reads `netstat -ano` LISTENING lines.
func parseNetstat(b []byte) []rawListener {
	var out []rawListener
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || !strings.HasPrefix(f[0], "TCP") || f[3] != "LISTENING" {
			continue
		}
		host, port, err := net.SplitHostPort(f[1])
		if err != nil {
			continue
		}
		n, _ := strconv.Atoi(port)
		pid, _ := strconv.Atoi(f[4])
		out = append(out, rawListener{ip: strings.Trim(host, "[]"), port: n, pid: pid})
	}
	return out
}

// ---- WSL -------------------------------------------------------------------------

func isWSL() bool {
	if goruntime.GOOS != "linux" {
		return false
	}
	b, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	return bytes.Contains(bytes.ToLower(b), []byte("microsoft"))
}

func windowsListeners(ctx context.Context) ([]rawListener, error) {
	netstat, tasklist := "/mnt/c/Windows/System32/netstat.exe", "/mnt/c/Windows/System32/tasklist.exe"
	if _, err := os.Stat(netstat); err != nil {
		return nil, errors.New("can't run Windows' netstat from WSL")
	}
	return netstatListeners(ctx, netstat, tasklist)
}

// wslHostIP is the Windows host's address as seen from WSL (NAT mode).
func wslHostIP() string {
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) > 2 && f[1] == "00000000" {
			raw, err := hex.DecodeString(f[2])
			if err == nil && len(raw) == 4 {
				return net.IPv4(raw[3], raw[2], raw[1], raw[0]).String()
			}
		}
	}
	return ""
}

// ---- forwarding (shared programs) -------------------------------------------------

// ShareLocal creates an app that forwards to a program on this computer
// and shares it through xu526 tunnel (when this computer is connected).
func (p *Platform) ShareLocal(ctx context.Context, name, upstream string, rewriteHost bool) (AppSpec, error) {
	if !resourceNameRE.MatchString(name) {
		return AppSpec{}, fmt.Errorf("name %q: use 1-20 lowercase letters, digits and dashes, starting with a letter", name)
	}
	if a := p.get(name); a != nil && a.spec.Upstream == "" {
		return AppSpec{}, fmt.Errorf("%s is already the name of an app", name)
	}
	if !canDial(ctx, upstream) {
		return AppSpec{}, fmt.Errorf("nothing answers at %s; start the program first", upstream)
	}
	spec := AppSpec{Name: name, Display: name, Upstream: upstream, Kind: "tunnel", RewriteHost: rewriteHost, MinScale: 1, MaxScale: 1}
	if err := p.Deploy(ctx, spec); err != nil {
		return spec, err
	}
	if err := p.SetTunnel(name, true); err != nil {
		p.event(name, "error", "sharing: %v", err)
	}
	p.event(name, "deploy", "forwarding to %s", upstream)
	return spec, nil
}

// localAPI serves the Tunnel section.
//
//	GET  /v1/local/scan                   programs listening on this computer
//	POST /v1/local/share {name, upstream, rewrite_host}
func (p *Platform) localAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/local/scan", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		writeJSON(w, http.StatusOK, p.ScanLocal(ctx))
	})
	mux.HandleFunc("POST /v1/local/share", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name        string `json:"name"`
			Upstream    string `json:"upstream"`
			RewriteHost *bool  `json:"rewrite_host"`
		}
		if err := decodeJSON(w, r, &req); err != nil {
			return
		}
		rewrite := req.RewriteHost == nil || *req.RewriteHost
		spec, err := p.ShareLocal(r.Context(), strings.TrimSpace(req.Name), strings.TrimSpace(req.Upstream), rewrite)
		if err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		u, _ := p.tunnelURLFor(spec.Name)
		writeJSON(w, http.StatusCreated, map[string]string{"name": spec.Name, "url": u, "local_url": p.appURL(spec.Name)})
	})
}
