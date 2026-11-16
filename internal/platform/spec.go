package platform

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
)

// Source is the image, or the command for process apps.
func (s AppSpec) Source() string {
	if s.Image != "" {
		return s.Image
	}
	return strings.Join(s.Command, " ")
}

// AppSpec is what a user deploys.
type AppSpec struct {
	Name    string            `json:"name"`
	Image   string            `json:"image,omitempty"`   // Docker image
	Command []string          `json:"command,omitempty"` // or a local command (dev only)
	Port    int               `json:"port,omitempty"`    // port the app listens on (images)
	Env     map[string]string `json:"env,omitempty"`

	MemoryMB int64   `json:"memory_mb,omitempty"` // per instance, 0 = unlimited
	CPUs     float64 `json:"cpus,omitempty"`      // per instance, 0 = unlimited

	MinScale       int    `json:"min_scale"`            // instances kept even when idle (0 = scale to zero)
	MaxScale       int    `json:"max_scale"`            // hard ceiling
	Target         int    `json:"target_concurrency"`   // in-flight requests one instance should handle
	IdleTimeoutSec int    `json:"idle_timeout_sec"`     // silence before scaling to zero
	ScaleDownSec   int    `json:"scale_down_delay_sec"` // how far back to look before removing instances
	HealthPath     string `json:"health_path,omitempty"`
	ProfilePath    string `json:"profile_path,omitempty"` // Go net/http/pprof base path, e.g. /debug/pprof

	// Set for services deployed as part of a stack (compose file).
	Args    []string `json:"args,omitempty"`    // overrides the image's command
	Stack   string   `json:"stack,omitempty"`   // owning stack
	Service string   `json:"service,omitempty"` // name other services use to reach it
	Network string   `json:"network,omitempty"` // private network shared by the stack
	// Networks are the compose networks a stack service is on (the first is
	// Network); empty when the stack has just its one network.
	Networks []NetworkRef  `json:"networks,omitempty"`
	Internal bool          `json:"internal,omitempty"` // not reachable from the internet
	Volumes  []VolumeMount `json:"volumes,omitempty"`

	// Environment is the environment the app belongs to ("" = production).
	Environment string `json:"environment,omitempty"`
	// SecretKeys are Env entries whose values came from the environment's
	// secrets; the API shows them masked.
	SecretKeys []string `json:"secret_keys,omitempty"`

	// Kind is the dashboard section: database, cache, backend or web
	// ("" = worked out from the image).
	Kind string `json:"kind,omitempty"`
	// Engine marks a database or cache created by xu526 (postgres, mysql,
	// mongodb, redis); its connection settings come from its Env.
	Engine string `json:"engine,omitempty"`
	// Display is the name as the user typed it, without the environment.
	Display string `json:"display,omitempty"`
	// Links are databases and caches (app names) this app uses: it gets
	// their connection settings (DATABASE_URL, REDIS_URL, ...) as variables.
	Links []string `json:"links,omitempty"`
	// Upstream forwards to a program already running on this computer
	// (host:port), so it can be shared through xu526 tunnel. Nothing is
	// started: xu526 only routes to it.
	Upstream string `json:"upstream,omitempty"`
	// RewriteHost sends the upstream's own address as the Host header, which
	// dev servers that check it (Vite, webpack, Angular) need.
	RewriteHost bool `json:"rewrite_host,omitempty"`
	// DependsOn are apps started (and healthy) before this one starts, and
	// kept running while it runs (compose depends_on).
	DependsOn []Dependency `json:"depends_on,omitempty"`
	// LoginOf makes xu526 ask for that object store's access key and secret
	// key before showing this app (a console without a sign-in of its own).
	LoginOf string `json:"login_of,omitempty"`
	// Entrypoint, WorkingDir and User override the image's (compose entrypoint, working_dir, user).
	Entrypoint []string `json:"entrypoint,omitempty"`
	WorkingDir string   `json:"working_dir,omitempty"`
	User       string   `json:"user,omitempty"`

	// Filled in at deploy time: the exact image this release runs.
	ImageID     string `json:"image_id,omitempty"`
	ImageDigest string `json:"image_digest,omitempty"`
}

// VolumeMount attaches a persistent named volume to every instance.
type VolumeMount struct {
	Volume   string `json:"volume"` // Docker volume name
	Path     string `json:"path"`   // mount point inside the container
	ReadOnly bool   `json:"read_only,omitempty"`
}

var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// Normalize validates the spec and fills in defaults.
func (s *AppSpec) Normalize() error {
	if s.Name == "xu" {
		return errors.New(`the name "xu" is reserved for the platform itself`)
	}
	if !nameRE.MatchString(s.Name) {
		return fmt.Errorf("invalid name %q: use lowercase letters, digits and dashes (max 40)", s.Name)
	}
	switch {
	case s.Upstream != "" && (s.Image != "" || len(s.Command) > 0 || s.Internal):
		return errors.New("an app forwarding to a running program has no image or command, and is public")
	case s.Upstream != "":
		if _, _, err := net.SplitHostPort(s.Upstream); err != nil {
			return fmt.Errorf("address %q: write host:port, e.g. 127.0.0.1:3000", s.Upstream)
		}
		s.MinScale, s.MaxScale = 1, 1
	case s.Image != "" && len(s.Command) > 0:
		return errors.New("give either an image or a command, not both")
	case s.Image == "" && len(s.Command) == 0:
		return errors.New("an image (or a command) is required")
	case s.Image != "" && !s.Internal && (s.Port <= 0 || s.Port > 65535):
		return errors.New("port is required: the image doesn't say which port it listens on (no EXPOSE in its Dockerfile), so give the port it listens on inside the container (compose: ports: [\"8080\"])")
	case s.Port < 0 || s.Port > 65535:
		return fmt.Errorf("invalid port %d", s.Port)
	case s.Network != "" && s.Image == "":
		return errors.New("stack services need an image")
	}
	if s.MaxScale == 0 {
		s.MaxScale = 10
	}
	if s.Target <= 0 {
		s.Target = 50
	}
	if s.IdleTimeoutSec <= 0 {
		s.IdleTimeoutSec = 300
	}
	if s.ScaleDownSec <= 0 {
		s.ScaleDownSec = 30
	}
	if s.HealthPath == "" {
		s.HealthPath = "/"
	}
	if s.MinScale < 0 || s.MaxScale < 1 || s.MinScale > s.MaxScale {
		return fmt.Errorf("need 0 <= min_scale (%d) <= max_scale (%d) and max_scale >= 1", s.MinScale, s.MaxScale)
	}
	return nil
}
