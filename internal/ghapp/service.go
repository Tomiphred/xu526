package ghapp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Hooks is how the platform learns about events that affect sources.
type Hooks interface {
	// Push is a signed push event for a repository in an installation.
	Push(ev PushEvent)
	// InstallationGone means sources using it must stop building
	// (uninstalled or suspended). reason is shown to users.
	InstallationGone(installation int64, reason string)
	// InstallationBack undoes a suspension.
	InstallationBack(installation int64)
	// ReposRemoved means these repositories left the installation.
	ReposRemoved(installation int64, repos []int64)
}

// PushEvent is the part of a push webhook the platform needs.
type PushEvent struct {
	Installation int64
	RepoID       int64
	RepoFullName string
	Ref          string
	After        string
	Deleted      bool
	Message      string
	Author       string
	Delivery     string
}

// Service is the GitHub App integration.
type Service struct {
	cfg   *Config
	http  *http.Client
	audit *Audit
	hooks Hooks
	log   *log.Logger
	now   func() time.Time

	mu        sync.Mutex
	st        *state
	statePath string

	tokMu      sync.Mutex
	tokens     map[string]cachedToken
	userTokens map[string]userToken // user ID → access token (memory only)

	flowMu     sync.Mutex
	oauth      map[string]*oauthFlow  // state → flow
	devices    map[string]*deviceFlow // request ID → device flow
	deliveries map[string]time.Time   // webhook GUIDs seen recently
	buckets    map[int64]*bucket      // webhook rate limits per installation
}

// New starts the integration. dataDir holds github.json; audit is the
// shared audit log (opened at dataDir/audit.log when nil).
func New(cfg *Config, dataDir string, hooks Hooks, logger *log.Logger, audit *Audit) (*Service, error) {
	if logger == nil {
		logger = log.Default()
	}
	s := &Service{cfg: cfg, hooks: hooks, log: logger, now: time.Now,
		http:      &http.Client{Timeout: 30 * time.Second},
		statePath: filepath.Join(dataDir, "github.json"),
		tokens:    map[string]cachedToken{}, userTokens: map[string]userToken{},
		oauth: map[string]*oauthFlow{}, devices: map[string]*deviceFlow{},
		deliveries: map[string]time.Time{}, buckets: map[int64]*bucket{},
	}
	var err error
	if cfg.Shared && len(cfg.SecretKey) == 0 {
		// The shared app: seal sign-ins with a key made on this computer.
		if cfg.SecretKey, err = localKey(filepath.Join(dataDir, "github.key")); err != nil {
			return nil, err
		}
	}
	if cfg.Shared {
		if err := cfg.Validate(); err != nil {
			return nil, err
		}
	}
	if s.st, err = loadState(s.statePath); err != nil {
		return nil, err
	}
	s.audit = audit
	if s.audit == nil {
		if s.audit, err = OpenAudit(filepath.Join(dataDir, "audit.log")); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// SetClock replaces the clock (tests).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// SetHTTPClient replaces the client used to reach GitHub (tests).
func (s *Service) SetHTTPClient(c *http.Client) { s.http = c }

// Config returns the app configuration.
func (s *Service) Config() *Config { return s.cfg }

// AuditPath is where the audit log is written.
func (s *Service) AuditPath() string { return s.audit.Path() }

// Audit records an event in the audit log (used by the platform for clones).
func (s *Service) Audit(action string, fields map[string]any) { s.audit.Log(action, fields) }

func (s *Service) saveLocked() {
	if err := s.st.save(s.statePath); err != nil {
		s.log.Printf("github: save state: %v", err)
	}
}

// Installations returns a copy of the known installations.
func (s *Service) Installations() []Installation {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Installation, 0, len(s.st.Installations))
	for _, in := range s.st.Installations {
		out = append(out, *in)
	}
	return out
}

// Installation returns one installation, if known.
func (s *Service) Installation(id int64) (Installation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.st.Installations[id]
	if !ok {
		return Installation{}, false
	}
	return *in, true
}

// Run does periodic housekeeping until ctx ends: expired sessions,
// flows and webhook delivery IDs.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweep()
		}
	}
}

func (s *Service) sweep() {
	now := s.now()
	s.mu.Lock()
	changed := false
	for h, se := range s.st.Sessions {
		if now.After(se.ExpiresAt) {
			delete(s.st.Sessions, h)
			changed = true
		}
	}
	if changed {
		s.saveLocked()
	}
	s.mu.Unlock()
	s.flowMu.Lock()
	for k, f := range s.oauth {
		if now.After(f.expires) {
			delete(s.oauth, k)
		}
	}
	for k, d := range s.devices {
		if now.After(d.expires) {
			delete(s.devices, k)
		}
	}
	for k, t := range s.deliveries {
		if now.Sub(t) > 72*time.Hour {
			delete(s.deliveries, k)
		}
	}
	s.flowMu.Unlock()
	s.tokMu.Lock()
	for k, t := range s.tokens {
		if now.After(t.expires) {
			delete(s.tokens, k)
		}
	}
	s.tokMu.Unlock()
}

// localKey reads a 32-byte key from path, creating it (mode 0600) the first time.
func localKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil {
		if k, err := parseKey(strings.TrimSpace(string(b))); err == nil && len(k) == 32 {
			return k, nil
		}
		return nil, fmt.Errorf("%s isn't a 32-byte key; delete it to make a new one (you'll need to sign in to GitHub again)", path)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(k)+"\n"), 0o600); err != nil {
		return nil, err
	}
	return k, nil
}
