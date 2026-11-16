// Package daemon is the xu526 background service on someone's computer or
// own server (`xu526 daemon`): the control-plane API and dashboard, the
// public router and autoscaler, git deploys, and the connection to a relay.
// The relay and its sign-up site are a separate program, xu526-server.
package daemon

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"xu526/internal/ghapp"
	"xu526/internal/platform"
	"xu526/internal/runtime"
	"xu526/internal/serve"
)

// Main runs the daemon until SIGINT or SIGTERM. args are its flags.
func Main(args []string) error {
	fs := flag.NewFlagSet("xu526 daemon", flag.ContinueOnError)
	httpAddr := fs.String("http", ":8080", "public address apps are served on")
	apiAddr := fs.String("api", "127.0.0.1:7070", "control-plane API and dashboard address (keep it private)")
	domain := fs.String("domain", "auto", `apps are served at <name>.<domain>; "auto" uses <your-ip>.sslip.io, a public DNS name that needs no setup`)
	publicIP := fs.String("public-ip", "", "this computer's public IP (detected when empty)")
	logMB := fs.Int("log-chunk-mb", 10, "rotate an app's log after this many MB")
	logAge := fs.Duration("log-chunk-age", time.Hour, "... or after this long")
	state := fs.String("state", serve.DefaultState(), "file storing deployed apps (logs, audit and GitHub data live beside it)")
	sock := fs.String("docker-sock", "", "Docker Engine socket (found automatically: DOCKER_HOST, Docker Desktop, OrbStack, Colima, /var/run/docker.sock)")
	allowProcess := fs.Bool("allow-process", false, "allow deploying raw commands (dev only, no isolation)")
	hostBuilds := fs.Bool("host-builds", false, "build git sources that have no Dockerfile with xu526 build (Go, Node, Python, C, static); runs their build steps on this computer, so only for repositories you trust")
	apiHosts := fs.String("api-hosts", "", "extra host names the dashboard/API may be reached as (comma-separated)")
	collector := fs.String("collector", ":4318", `telemetry address apps send traces and profiles to ("" turns it off); apps authenticate with their own token`)
	profileKeep := fs.Duration("profile-retention", 6*time.Hour, "how long profiles are kept")
	tunnelService := fs.String("tunnel-service", "https://tunnel.xu526.com", `where the dashboard's "Connect with xu526 tunnel" button signs you up and gets a token ("" turns it off)`)
	httpsAddr := fs.String("https", "", "serve apps over HTTPS on this address too, e.g. :443 (needs -tls-cert and -tls-key)")
	tlsCert := fs.String("tls-cert", "", "certificate file (e.g. a wildcard from Let's Encrypt); reloaded when it changes")
	tlsKey := fs.String("tls-key", "", "private key file for -tls-cert")
	coldStart := fs.Duration("cold-start-timeout", 60*time.Second, "max wait for a new instance")
	ghSecrets := fs.String("github-secrets-dir", "", "where Connect GitHub saves this computer's GitHub App keys (0600; default: next to -state)")
	logFile := fs.String("log", "", "write the log (and apps' output) to this file instead of the terminal; used by the Windows service")
	podman := fs.String("podman", "", `use Podman instead of Docker: the path to podman (or "podman" when it's on PATH). Its machine is started when stopped, and its API found by itself`)
	waitDocker := fs.Duration("wait-docker", 0, "keep trying to reach Docker for this long at start (Docker Desktop starts after you sign in), e.g. 5m")
	relay := fs.Bool("relay", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *relay {
		return errors.New("the relay is now its own program: install xu526-server on the relay machine (see the README, \"Run the relay\")")
	}

	if *logFile != "" {
		// Apps' output goes to stdout/stderr too, so both point at the file
		// (a windowless service has no terminal to write to).
		if err := os.MkdirAll(filepath.Dir(*logFile), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		os.Stdout, os.Stderr = f, f
	}
	logger := log.New(os.Stderr, "xu526 ", log.LstdFlags)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := platform.Config{
		Domain: *domain, StatePath: *state, ColdStartTimeout: *coldStart, Logger: logger,
		PublicIP: *publicIP, LogMaxBytes: int64(*logMB) << 20, LogMaxAge: *logAge, ProfileRetention: *profileKeep,
		HTTPSPort: serve.PortOf(*httpsAddr), HostBuilds: *hostBuilds,
		GitHubSecretsDir: *ghSecrets, TunnelService: *tunnelService,
	}
	if *domain == "auto" {
		d, ip, private := platform.DetectDomain(*publicIP)
		cfg.Domain, cfg.PublicIP = d, ip
		if private {
			logger.Printf("note: %s is a private address, so %s only works on your local network. "+
				"Share apps through xu526 tunnel (dashboard → Settings), or pass -public-ip/-domain.", ip, d)
		}
	}
	cfg.PublicPort = serve.PortOf(*httpAddr)
	if *collector != "" {
		cfg.CollectorPort = serve.PortOf(*collector)
	}
	if *podman != "" && *sock == "" {
		if addr, err := runtime.PodmanEngine(ctx, *podman, logger.Printf); err != nil {
			logger.Printf("warning: %v", err)
		} else {
			*sock = addr
			logger.Printf("using Podman at %s", addr)
		}
	}
	if *sock == "" {
		*sock = runtime.DockerSocket()
	}
	d, err := runtime.NewDocker(ctx, *sock)
	if err != nil && *waitDocker > 0 {
		logger.Printf("waiting up to %s for Docker (%v)", *waitDocker, err)
		deadline := time.Now().Add(*waitDocker)
		for err != nil && time.Now().Before(deadline) && ctx.Err() == nil {
			select {
			case <-ctx.Done():
			case <-time.After(3 * time.Second):
			}
			d, err = runtime.NewDocker(ctx, *sock)
		}
	}
	if err != nil {
		logger.Printf("warning: %v (image deploys disabled)", err)
	} else {
		cfg.Docker = d
		if d.InVM() {
			logger.Printf("docker at %s runs in a VM; internal services are reached through 127.0.0.1 ports", *sock)
		}
	}
	if *allowProcess {
		cfg.Process = runtime.NewProcess()
	}
	if cfg.Docker == nil && cfg.Process == nil {
		return errors.New("no runtime available: install Docker or pass -allow-process")
	}

	// Read the GitHub App's secrets first: inline values are removed from
	// the environment so no child process inherits them.
	ghCfg, err := ghapp.LoadConfig()
	if err != nil {
		return err
	}
	p, err := platform.New(cfg)
	if err != nil {
		return err
	}
	defer p.Close()
	if ghCfg != nil {
		if err := p.EnableGitHub(ghCfg); err != nil {
			return err
		}
		if ghCfg.Shared {
			logger.Printf("GitHub sign-in on (the shared %s app); connected repositories are checked for new commits every %s", ghCfg.Slug, platform.RepoCheckInterval)
		} else {
			logger.Printf("GitHub App %s enabled; webhook URL %s", ghCfg.Slug, p.GitHubWebhookURL())
		}
	} else if ok, err := p.EnableSavedGitHub(); err != nil {
		logger.Printf("warning: the saved GitHub App couldn't be turned on: %v", err)
	} else if ok {
		logger.Printf("GitHub App %s enabled; connected repositories are checked for new commits every %s", p.GitHub().Config().Slug, platform.RepoCheckInterval)
	} else {
		logger.Printf("GitHub isn't connected yet: use Connect GitHub in the dashboard (it sets up a private GitHub App for this computer)")
	}

	public := &http.Server{Addr: *httpAddr, Handler: p, ReadHeaderTimeout: 10 * time.Second}
	api := &http.Server{Addr: *apiAddr, Handler: p.API(strings.Split(*apiHosts, ",")...), ReadHeaderTimeout: 10 * time.Second}
	servers := []*http.Server{public, api}
	errc := make(chan error, 4)
	if *httpsAddr != "" {
		if *tlsCert == "" || *tlsKey == "" {
			return errors.New("-https needs -tls-cert and -tls-key")
		}
		certs := &serve.CertReloader{Cert: *tlsCert, Key: *tlsKey}
		if _, err := certs.Get(nil); err != nil {
			return fmt.Errorf("load certificate: %w", err)
		}
		secure := &http.Server{Addr: *httpsAddr, Handler: p, ReadHeaderTimeout: 10 * time.Second,
			TLSConfig: &tls.Config{GetCertificate: certs.Get, MinVersion: tls.VersionTLS12}}
		go func() {
			if err := secure.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
		servers = append(servers, secure)
		logger.Printf("serving apps over HTTPS on %s", *httpsAddr)
	}
	if *collector != "" {
		servers = append(servers, &http.Server{Addr: *collector, Handler: p.Collector(), ReadHeaderTimeout: 10 * time.Second})
		logger.Printf("telemetry collector (OpenTelemetry, Pyroscope) on %s", *collector)
	}
	for _, s := range servers {
		if s.TLSConfig != nil {
			continue
		}
		go func() {
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}
	logger.Printf("serving apps on %s as <name>.%s", *httpAddr, cfg.Domain)
	logger.Printf("dashboard and API on http://%s", *apiAddr)

	select {
	case <-ctx.Done():
	case err := <-errc:
		return err
	}
	logger.Printf("shutting down: draining requests and stopping instances")
	sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(sctx)
	}
	return nil
}
