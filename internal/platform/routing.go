package platform

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Route controls how an app is reached from the internet.
type Route struct {
	Public  *bool    `json:"public,omitempty"` // nil: public unless the app is internal
	Domains []string `json:"domains,omitempty"`
	Tunnel  bool     `json:"tunnel,omitempty"` // shared through the relay
}

// DomainStatus reports whether a custom domain's DNS points at this server.
type DomainStatus struct {
	Domain    string   `json:"domain"`
	OK        bool     `json:"ok"`
	Addresses []string `json:"addresses"`
	Message   string   `json:"message"`
}

// RoutingStatus is an app's public-access state.
type RoutingStatus struct {
	Public        bool           `json:"public"`
	DefaultPublic bool           `json:"default_public"`
	CanBePublic   bool           `json:"can_be_public"`
	DefaultURL    string         `json:"default_url"`
	Domains       []DomainStatus `json:"domains"`
	ServerIPs     []string       `json:"server_ips"`
	Tunnel        bool           `json:"tunnel"`
	TunnelURL     string         `json:"tunnel_url,omitempty"`
	TunnelError   string         `json:"tunnel_error,omitempty"`
}

var hostnameRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

func (p *Platform) isPublic(a *app) bool {
	p.mu.RLock()
	r := p.routes[a.spec.Name]
	p.mu.RUnlock()
	if r != nil && r.Public != nil {
		return *r.Public
	}
	return !a.spec.Internal
}

// appForHost maps a Host header to an app: custom domains first, then
// <name>.<default domain>.
func (p *Platform) appForHost(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	p.mu.RLock()
	name, ok := p.domainIndex[host]
	p.mu.RUnlock()
	if ok {
		return name
	}
	if name, ok := strings.CutSuffix(host, "."+p.cfg.Domain); ok && !strings.Contains(name, ".") {
		return name
	}
	return ""
}

func (p *Platform) rebuildDomainIndexLocked() {
	p.domainIndex = map[string]string{}
	for app, r := range p.routes {
		for _, d := range r.Domains {
			p.domainIndex[d] = app
		}
	}
}

// SetRoute changes an app's public access and custom domains.
func (p *Platform) SetRoute(name string, public *bool, domains []string) error {
	a := p.get(name)
	if a == nil {
		return fmt.Errorf("no app %q", name)
	}
	if public != nil && *public {
		if !p.hasPort(a) {
			return errors.New("this service has no port, so there is nothing to serve on the web")
		}
	}
	clean := []string{}
	seen := map[string]bool{}
	for _, d := range domains {
		d = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
		d = strings.TrimPrefix(strings.TrimPrefix(d, "https://"), "http://")
		if d == "" || seen[d] {
			continue
		}
		if !hostnameRE.MatchString(d) {
			return fmt.Errorf("%q isn't a valid domain name", d)
		}
		if d == p.cfg.Domain || strings.HasSuffix(d, "."+p.cfg.Domain) {
			return fmt.Errorf("%s is under the platform's own domain; every app already has %s", d, p.appURL(name))
		}
		seen[d] = true
		clean = append(clean, d)
	}
	sort.Strings(clean)

	p.mu.Lock()
	for _, d := range clean {
		if owner, ok := p.domainIndex[d]; ok && owner != name {
			p.mu.Unlock()
			return fmt.Errorf("%s is already attached to %s", d, owner)
		}
	}
	r := p.routes[name]
	if r == nil {
		r = &Route{}
		p.routes[name] = r
	}
	r.Public, r.Domains = public, clean
	if r.Public == nil && len(r.Domains) == 0 && !r.Tunnel {
		delete(p.routes, name)
	}
	p.rebuildDomainIndexLocked()
	p.mu.Unlock()

	state := "private"
	if p.isPublic(a) {
		state = "public"
	}
	p.event(name, "deploy", "is now %s%s", state, map[bool]string{true: ", domains: " + strings.Join(clean, ", "), false: ""}[len(clean) > 0])
	return p.save()
}

// Routing returns an app's public-access state, checking each domain's DNS.
func (p *Platform) Routing(ctx context.Context, name string) (RoutingStatus, error) {
	a := p.get(name)
	if a == nil {
		return RoutingStatus{}, fmt.Errorf("no app %q", name)
	}
	p.mu.RLock()
	var domains []string
	if r := p.routes[name]; r != nil {
		domains = append(domains, r.Domains...)
	}
	p.mu.RUnlock()
	ips := p.serverIPs()
	st := RoutingStatus{
		Public: p.isPublic(a), DefaultPublic: !a.spec.Internal, CanBePublic: !(a.spec.Internal && a.spec.Port == 0),
		DefaultURL: p.appURL(name), Domains: []DomainStatus{}, ServerIPs: ips,
	}
	for _, d := range domains {
		st.Domains = append(st.Domains, checkDomain(ctx, d, ips))
	}
	st.Tunnel = p.tunneled(name)
	p.agent.mu.Lock()
	st.TunnelURL, st.TunnelError = p.agent.urls[name], p.agent.errs[name]
	p.agent.mu.Unlock()
	return st, nil
}

func checkDomain(ctx context.Context, domain string, serverIPs []string) DomainStatus {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	ds := DomainStatus{Domain: domain, Addresses: []string{}}
	addrs, err := net.DefaultResolver.LookupHost(ctx, domain)
	if err != nil || len(addrs) == 0 {
		ds.Message = "No DNS record yet. Add an A record for " + domain + " pointing to " + strings.Join(serverIPs, " or ") + "."
		return ds
	}
	ds.Addresses = addrs
	for _, a := range addrs {
		for _, s := range serverIPs {
			if a == s {
				ds.OK, ds.Message = true, "DNS points to this server."
				return ds
			}
		}
	}
	ds.Message = "DNS points to " + strings.Join(addrs, ", ") + ", not this server (" + strings.Join(serverIPs, ", ") + "). DNS changes can take a while."
	return ds
}

// serverIPs lists the addresses this server answers on: the configured
// public IP first, then non-loopback interface addresses.
func (p *Platform) serverIPs() []string {
	var out []string
	if p.cfg.PublicIP != "" {
		out = append(out, p.cfg.PublicIP)
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() || ipn.IP.To4() == nil {
			continue
		}
		ip := ipn.IP.String()
		if ip != p.cfg.PublicIP && !strings.HasPrefix(ip, "172.17.") && !isDockerBridge(ipn) {
			out = append(out, ip)
		}
	}
	return out
}

func isDockerBridge(ipn *net.IPNet) bool {
	// Docker's user-defined networks live in 172.18.0.0/16 – 172.31.0.0/16 by default.
	ip4 := ipn.IP.To4()
	return ip4[0] == 172 && ip4[1] >= 18 && ip4[1] <= 31
}

// DetectDomain returns <ip-with-dashes>.sslip.io for this server: a public
// DNS name that needs no setup (sslip.io answers with the IP in the name).
func DetectDomain(publicIP string) (domain, ip string, private bool) {
	ip = publicIP
	if ip == "" {
		// No packets are sent: this only asks the OS which source address it would use.
		if c, err := net.Dial("udp", "1.1.1.1:80"); err == nil {
			ip = c.LocalAddr().(*net.UDPAddr).IP.String()
			c.Close()
		}
	}
	if ip == "" {
		ip = "127.0.0.1"
	}
	parsed := net.ParseIP(ip)
	private = parsed == nil || parsed.IsPrivate() || parsed.IsLoopback()
	return strings.ReplaceAll(ip, ".", "-") + ".sslip.io", ip, private
}
