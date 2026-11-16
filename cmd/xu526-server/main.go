// xu526-server is the relay: it accepts tunnels from xu526 on people's
// computers and gives their shared apps public addresses
// (APP-USER.<domain>). With XU_PORTAL_DOMAIN set it also runs the
// "xu526 tunnel" sign-up site, where people get their own token.
//
// It runs no apps itself; that's xu526 (the daemon on each computer).
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"xu526/internal/platform"
	"xu526/internal/portal"
	"xu526/internal/serve"
	"xu526/internal/version"
)

const usage = `xu526-server: the xu526 relay and its sign-up site (xu526 tunnel)

  xu526-server [serve] [flags]               run the relay (installed as a service; xu526-server serve -h)
  xu526-server users                         people with a token: online?, apps, traffic
  xu526-server add-user NAME [-apps 5] [-mbps 10]
                                             make a token by hand (the sign-up site does this for people)
  xu526-server rm-user NAME                  remove someone's token and disconnect them
  xu526-server version

The sign-up site is configured with XU_PORTAL_* environment variables (see the README).
Set XU_API to reach a server other than http://127.0.0.1:7070.
`

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serveRelay(args)
	case "users":
		err = users()
	case "add-user":
		err = addUser(args)
	case "rm-user":
		if len(args) != 1 {
			err = errors.New("usage: xu526-server rm-user NAME")
		} else if err = call("DELETE", "/v1/relay/users/"+args[0], nil, nil); err == nil {
			fmt.Println("removed", args[0])
		}
	case "version", "--version":
		fmt.Println("xu526-server", version.Version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func serveRelay(args []string) error {
	fs := flag.NewFlagSet("xu526-server serve", flag.ContinueOnError)
	httpAddr := fs.String("http", ":80", "address for plain HTTP (redirects the sign-up site to HTTPS)")
	httpsAddr := fs.String("https", "", "HTTPS address, e.g. :443 (needs -tls-cert and -tls-key)")
	apiAddr := fs.String("api", "127.0.0.1:7070", "admin API and dashboard (keep it private)")
	domain := fs.String("domain", "auto", `shared apps get APP-USER.<domain> and agents connect to xu.<domain>; needs a wildcard DNS record`)
	publicIP := fs.String("public-ip", "", "this server's public IP (detected when empty)")
	state := fs.String("state", "/var/lib/xu526/apps.json", "file storing relay users (the sign-up site's key lives beside it)")
	tlsCert := fs.String("tls-cert", "", "certificate file, a wildcard for the domain (e.g. from Let's Encrypt); reloaded when it changes")
	tlsKey := fs.String("tls-key", "", "private key file for -tls-cert")
	portalCert := fs.String("portal-cert", "", "certificate for the sign-up site's domain (XU_PORTAL_DOMAIN), when -tls-cert doesn't cover it")
	portalKey := fs.String("portal-key", "", "private key file for -portal-cert")
	// Accepted so a former `xud -relay ...` configuration starts unchanged.
	fs.Bool("relay", true, "(always on; accepted for old configurations)")
	fs.Bool("allow-process", false, "(ignored: the relay runs no apps)")
	fs.String("collector", "", "(ignored)")
	fs.String("docker-sock", "", "(ignored)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	logger := log.New(os.Stderr, "xu526-server ", log.LstdFlags)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := platform.Config{Domain: *domain, StatePath: *state, Logger: logger, PublicIP: *publicIP,
		Relay: true, HTTPSPort: serve.PortOf(*httpsAddr), PublicPort: serve.PortOf(*httpAddr)}
	if *domain == "auto" {
		cfg.Domain, cfg.PublicIP, _ = platform.DetectDomain(*publicIP)
		logger.Printf("note: with -domain auto, shared apps get %s addresses; a real domain with a wildcard DNS record and certificate is better", cfg.Domain)
	}
	// Read the sign-up site's secrets before anything else: inline values
	// are removed from the environment.
	settings, err := portal.LoadSettings(filepath.Dir(*state))
	if err != nil {
		return fmt.Errorf("sign-up site: %w", err)
	}
	p, err := platform.New(cfg)
	if err != nil {
		return err
	}
	defer p.Close()

	if settings != nil {
		if *httpsAddr == "" && settings.Secure {
			return fmt.Errorf("sign-up site: it needs HTTPS (-https :443 with -tls-cert, and -portal-cert if the certificate doesn't cover %s)", settings.Host)
		}
		store, err := portal.OpenPG(ctx, settings.DatabaseURL)
		if err != nil {
			return fmt.Errorf("sign-up site: %w", err)
		}
		defer store.Close()
		site, err := portal.New(settings.Config, store, &settings.SMTP, relayAdapter{p}, logger)
		if err != nil {
			return err
		}
		go site.Run(ctx)
		p.SetPortal(settings.Host, httpsOnly(site, settings.Secure, serve.PortOf(*httpsAddr)))
		logger.Printf("sign-up site on https://%s (email through %s:%d as %s)", settings.Host, settings.SMTP.Host, settings.SMTP.Port, settings.SMTP.From)
	}

	errc := make(chan error, 3)
	servers := []*http.Server{
		{Addr: *httpAddr, Handler: p, ReadHeaderTimeout: 10 * time.Second},
		{Addr: *apiAddr, Handler: p.API(), ReadHeaderTimeout: 10 * time.Second},
	}
	for _, s := range servers {
		go func() {
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}
	if *httpsAddr != "" {
		if *tlsCert == "" || *tlsKey == "" {
			return errors.New("-https needs -tls-cert and -tls-key")
		}
		certs := &serve.CertReloader{Cert: *tlsCert, Key: *tlsKey}
		if _, err := certs.Get(nil); err != nil {
			return fmt.Errorf("load certificate: %w", err)
		}
		getCert := certs.Get
		if *portalCert != "" || *portalKey != "" {
			if settings == nil || *portalCert == "" || *portalKey == "" {
				return fmt.Errorf("-portal-cert and -portal-key go together, with %s set", portal.EnvDomain)
			}
			pc := &serve.CertReloader{Cert: *portalCert, Key: *portalKey}
			if _, err := pc.Get(nil); err != nil {
				return fmt.Errorf("load -portal-cert: %w", err)
			}
			host := settings.Host
			getCert = func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
				if hello != nil && strings.EqualFold(hello.ServerName, host) {
					return pc.Get(hello)
				}
				return certs.Get(hello)
			}
		}
		secure := &http.Server{Addr: *httpsAddr, Handler: p, ReadHeaderTimeout: 10 * time.Second,
			TLSConfig: &tls.Config{GetCertificate: getCert, MinVersion: tls.VersionTLS12}}
		servers = append(servers, secure)
		go func() {
			if err := secure.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
		logger.Printf("HTTPS on %s", *httpsAddr)
	}
	logger.Printf("relay for %s: computers connect to %s; shared apps get APP-USER.%s", cfg.Domain, p.RelayAgentURL(), cfg.Domain)
	logger.Printf("admin API and dashboard on http://%s", *apiAddr)

	select {
	case <-ctx.Done():
	case err := <-errc:
		return err
	}
	logger.Printf("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(sctx)
	}
	return nil
}

// relayAdapter gives the sign-up site the relay's user management.
type relayAdapter struct{ p *platform.Platform }

func (r relayAdapter) AddUser(name string, maxApps int, mbps float64) (string, error) {
	return r.p.AddRelayUser(name, maxApps, mbps)
}
func (r relayAdapter) RotateToken(name string) (string, error) { return r.p.RotateRelayToken(name) }
func (r relayAdapter) AgentURL() string                        { return r.p.RelayAgentURL() }
func (r relayAdapter) User(name string) (portal.RelayUser, bool) {
	u, ok := r.p.RelayUser(name)
	if !ok {
		return portal.RelayUser{}, false
	}
	return portal.RelayUser{Online: u.Online, MaxApps: u.MaxApps, Apps: u.Apps}, true
}

// httpsOnly sends plain-HTTP visitors of the sign-up site to HTTPS:
// passwords never cross the network unencrypted.
func httpsOnly(h http.Handler, on bool, httpsPort int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if on && r.TLS == nil {
			host := r.Host
			if hh, _, err := net.SplitHostPort(host); err == nil {
				host = hh
			}
			if httpsPort != 0 && httpsPort != 443 {
				host = net.JoinHostPort(host, strconv.Itoa(httpsPort))
			}
			http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusPermanentRedirect)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// ---- admin commands (talk to the running server's private API) ----

func apiBase() string {
	if v := os.Getenv("XU_API"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://127.0.0.1:7070"
}

func call(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, apiBase()+path, body)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach xu526-server (is it running? sudo systemctl status xu526-server): %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct{ Error string }
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return errors.New(e.Error)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func addUser(args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: xu526-server add-user NAME [-apps 5] [-mbps 10]")
	}
	name := args[0]
	fs := flag.NewFlagSet("add-user", flag.ContinueOnError)
	apps := fs.Int("apps", 5, "max public apps")
	mbps := fs.Float64("mbps", 10, "bandwidth limit in MB/s")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	var out map[string]string
	if err := call("POST", "/v1/relay/users", map[string]any{"name": name, "max_apps": *apps, "rate_mbps": *mbps}, &out); err != nil {
		return err
	}
	fmt.Printf("created %s. Give them this command (the token is shown only once):\n\n  xu tunnel connect %s %s\n", name, out["relay_url"], out["token"])
	return nil
}

func users() error {
	var list []platform.RelayUserStatus
	if err := call("GET", "/v1/relay/users", nil, &list); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "USER\tONLINE\tFROM\tAPPS\tLIMIT\tREQUESTS\tTRAFFIC")
	for _, u := range list {
		var apps []string
		for a := range u.Apps {
			apps = append(apps, a)
		}
		sort.Strings(apps)
		fmt.Fprintf(tw, "%s\t%v\t%s\t%s\t%d\t%d\t%.1f MB\n", u.Name, u.Online, u.Remote, strings.Join(apps, ","), u.MaxApps, u.Requests,
			float64(u.BytesIn+u.BytesOut)/(1<<20))
	}
	return tw.Flush()
}
