package platform

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"xu526/internal/ghapp"
)

// ---- cloning ----

// gitAuth is how one clone authenticates. secret is redacted from output.
type gitAuth struct {
	url, display string
	env          []string
	secret       string
	kind         string // github-app | token | none
	installation int64
	repoID       int64
	repoName     string
}

// gitBaseEnv is the whole environment git gets: enough to find itself,
// proxies and CA certificates, and nothing of xud's own secrets.
func gitBaseEnv() []string {
	env := []string{"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=true", "GIT_CONFIG_NOSYSTEM=1"}
	for _, k := range []string{"PATH", "HOME", "LANG", "TMPDIR", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
		"http_proxy", "https_proxy", "no_proxy", "SSL_CERT_FILE", "SSL_CERT_DIR", "GIT_SSL_CAINFO"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// headerEnv scopes an Authorization header to one URL prefix through
// git's environment config, so it never appears in argv or .git/config.
func headerEnv(prefix, value string) []string {
	return []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http." + prefix + ".extraheader", "GIT_CONFIG_VALUE_0=" + value}
}

func (p *Platform) gitAuth(ctx context.Context, s *Source) (gitAuth, error) {
	if s.InstallationID != 0 {
		if p.gh == nil {
			return gitAuth{}, errors.New("this source uses the GitHub App, which isn't configured on this server")
		}
		var acc *ghapp.CloneAccess
		var err error
		if p.gh.Config().Shared {
			acc, err = p.gh.CloneAs(ctx, s.ConnectedBy, s.RepoID)
		} else {
			acc, err = p.gh.Clone(ctx, s.InstallationID, s.RepoID, "system:build")
		}
		if err != nil {
			return gitAuth{}, err
		}
		if acc.Repo.FullName != s.RepoFullName {
			p.setSource(s.ID, func(x *Source) {
				x.RepoFullName, x.Repo = acc.Repo.FullName, strings.TrimSuffix(p.gh.CloneURL(acc.Repo.FullName), ".git")
			})
		}
		return gitAuth{url: p.gh.CloneURL(acc.Repo.FullName), display: acc.Repo.FullName, env: p.gh.GitEnv(acc.Token),
			secret: acc.Token, kind: "github-app", installation: s.InstallationID, repoID: s.RepoID, repoName: acc.Repo.FullName}, nil
	}
	a := gitAuth{url: s.Repo, display: s.Repo, kind: "none", repoName: s.Repo}
	if s.Token != "" && strings.HasPrefix(s.Repo, "https://") {
		u, err := url.Parse(s.Repo)
		if err != nil {
			return gitAuth{}, err
		}
		cred := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + s.Token))
		a.env, a.secret, a.kind = headerEnv(u.Scheme+"://"+u.Host+"/", "Authorization: Basic "+cred), s.Token, "token"
	}
	return a, nil
}

// auditClone records every clone (App and legacy sources alike).
func (p *Platform) auditClone(s *Source, a gitAuth, rel *Release, info commitInfo, err error) {
	f := map[string]any{"actor": "system:build", "auth": a.kind, "repo_name": a.repoName, "ref": "refs/heads/" + s.Branch,
		"target": s.Kind + "/" + s.Target, "release": rel.Version, "trigger": firstNonEmpty(rel.Trigger, "manual")}
	if a.kind == "github-app" {
		f["installation"], f["repo"] = a.installation, a.repoID
	}
	if a.secret != "" {
		f["token_fp"] = fingerprintOf(a.secret)
	}
	if err != nil {
		f["result"], f["error"] = "error", redactToken(err.Error(), a.secret)
	} else {
		f["result"], f["commit"] = "ok", info.sha
	}
	p.audit.Log("repo.clone", f)
}

func fingerprintOf(tok string) string {
	sum := sha256Sum([]byte(tok))
	return fmt.Sprintf("%x", sum[:4])
}

// ---- enabling the app ----

// EnableGitHub turns on the GitHub App integration.
func (p *Platform) EnableGitHub(cfg *ghapp.Config) error {
	svc, err := ghapp.New(cfg, p.dataDir, ghHooks{p}, p.log, p.audit)
	if err != nil {
		return err
	}
	p.ghMu.Lock()
	p.gh = svc
	for _, a := range p.apiMuxes { // dashboards already being served
		p.wireGitHub(a, svc)
	}
	p.ghMu.Unlock()
	go svc.Run(p.ctx)
	go p.recheckLoop(p.ctx)
	p.mu.RLock()
	for _, s := range p.sources {
		if s.InstallationID == 0 && s.Token != "" {
			p.log.Printf("source %s/%s uses a stored GitHub token (legacy); migrate it to the GitHub App: xu source migrate %s", s.Kind, s.Target, s.Target)
		}
	}
	p.mu.RUnlock()
	return nil
}

// GitHub returns the integration, or nil.
func (p *Platform) GitHub() *ghapp.Service {
	p.ghMu.Lock()
	defer p.ghMu.Unlock()
	return p.gh
}

// GitHubWebhookURL is where the app's webhook must point.
func (p *Platform) GitHubWebhookURL() string {
	return strings.TrimSuffix(p.webhookURL("x"), "/hooks/github/x") + "/github/webhook"
}

// ghHooks receives events from the GitHub App.
type ghHooks struct{ p *Platform }

func (h ghHooks) Push(ev ghapp.PushEvent) {
	p := h.p
	var ids []string
	p.mu.Lock()
	for id, s := range p.sources {
		if s.InstallationID != ev.Installation || s.RepoID != ev.RepoID {
			continue
		}
		if ev.RepoFullName != "" {
			s.RepoFullName = ev.RepoFullName
		}
		if ev.Deleted || ev.Ref != "refs/heads/"+s.Branch {
			continue
		}
		if s.Disabled != "" {
			p.mu.Unlock()
			p.event(s.Target, "error", "push %s ignored: %s", short(ev.After), s.Disabled)
			p.mu.Lock()
			continue
		}
		if !s.AutoDeploy {
			id, pc := s.ID, PendingCommit{SHA: ev.After, Message: ev.Message, Author: ev.Author}
			p.mu.Unlock()
			p.notePending(id, pc, "")
			p.mu.Lock()
			continue
		}
		ids = append(ids, id)
	}
	p.mu.Unlock()
	for _, id := range ids {
		p.TriggerBuild(id, ReleaseMeta{Commit: ev.After, CommitMsg: ev.Message, Author: ev.Author, Delivery: "webhook:" + ev.Delivery})
	}
}

func (h ghHooks) disable(match func(*Source) bool, reason string) {
	p := h.p
	var hit []*Source
	p.mu.Lock()
	for _, s := range p.sources {
		if s.InstallationID != 0 && match(s) && s.Disabled == "" {
			s.Disabled = reason
			hit = append(hit, s)
		}
	}
	p.mu.Unlock()
	for _, s := range hit {
		p.event(s.Target, "error", "builds paused: %s", reason)
		p.audit.Log("source.disable", map[string]any{"actor": "system", "target": s.Kind + "/" + s.Target, "installation": s.InstallationID, "repo": s.RepoID, "reason": reason})
	}
	if len(hit) > 0 {
		_ = p.save()
	}
}

func (h ghHooks) InstallationGone(inst int64, reason string) {
	h.disable(func(s *Source) bool { return s.InstallationID == inst }, reason)
}

func (h ghHooks) InstallationBack(inst int64) {
	p := h.p
	p.mu.Lock()
	for _, s := range p.sources {
		if s.InstallationID == inst && strings.Contains(s.Disabled, "is suspended") {
			s.Disabled = ""
		}
	}
	p.mu.Unlock()
	_ = p.save()
}

func (h ghHooks) ReposRemoved(inst int64, repos []int64) {
	gone := map[int64]bool{}
	for _, r := range repos {
		gone[r] = true
	}
	h.disable(func(s *Source) bool { return s.InstallationID == inst && gone[s.RepoID] },
		"the repository was removed from the GitHub App installation")
}

// recheckLoop confirms once a day that whoever connected each source can
// still see its repository; if not, the source stops building.
func (p *Platform) recheckLoop(ctx context.Context) {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.RecheckAccess(ctx)
		}
	}
}

// RecheckAccess runs the access check now.
func (p *Platform) RecheckAccess(ctx context.Context) {
	if p.gh == nil {
		return
	}
	var list []Source
	p.mu.RLock()
	for _, s := range p.sources {
		if s.InstallationID != 0 && s.Disabled == "" {
			list = append(list, *s)
		}
	}
	p.mu.RUnlock()
	for _, s := range list {
		_, err := p.gh.UserCanAccess(ctx, s.ConnectedBy, s.InstallationID, s.RepoID)
		if err == nil {
			continue
		}
		var apiErr *ghapp.APIError
		if errors.As(err, &apiErr) && apiErr.Status >= 500 {
			continue // GitHub trouble: try again tomorrow
		}
		who := s.ConnectedBy
		if u, ok := p.gh.UserByID(s.ConnectedBy); ok {
			who = u.Login
		}
		id := s.ID
		ghHooks{p}.disable(func(x *Source) bool { return x.ID == id },
			fmt.Sprintf("%s, who connected it, can no longer access %s (%v); reconnect it", who, s.RepoFullName, err))
	}
}

// ---- connecting and migrating ----

// GitHubSourceRequest connects a repository through the GitHub App.
type GitHubSourceRequest struct {
	Kind           string   `json:"kind"`   // app | stack
	Target         string   `json:"target"` // app or stack name
	Repo           string   `json:"repo"`   // owner/name, or use the IDs below
	InstallationID int64    `json:"installation_id"`
	RepoID         int64    `json:"repo_id"`
	Branch         string   `json:"branch"`
	Path           string   `json:"path"`
	AutoDeploy     *bool    `json:"auto_deploy"`
	App            *AppSpec `json:"app"`
	// From the dashboard, like folders: the name without the environment
	// (instead of target), the environment and, for apps, the section and
	// what it uses. The port and kind are then worked out by the build.
	Name        string            `json:"name"`
	Environment string            `json:"environment"`
	AppKind     string            `json:"app_kind"`
	Links       []string          `json:"links"`
	Env         map[string]string `json:"env"`
}

// ConnectGitHubSource links a repository the user can access through an
// installation. Access is checked with the user's own GitHub token.
func (p *Platform) ConnectGitHubSource(ctx context.Context, u *ghapp.User, req GitHubSourceRequest) (Source, error) {
	if p.gh == nil {
		return Source{}, errors.New("the GitHub App isn't configured on this server")
	}
	if u == nil {
		return Source{}, errors.New("sign in first (xu login)")
	}
	var ref ghapp.RepoRef
	var err error
	inst := req.InstallationID
	if req.Repo != "" {
		inst, ref, err = p.gh.FindRepo(ctx, u.ID, strings.TrimSuffix(strings.TrimPrefix(req.Repo, "https://github.com/"), ".git"))
	} else {
		ref, err = p.gh.UserCanAccess(ctx, u.ID, req.InstallationID, req.RepoID)
	}
	if err != nil {
		return Source{}, err
	}
	s := Source{Kind: req.Kind, Target: req.Target, App: req.App}
	if req.Name != "" {
		if s, err = p.newTarget(req.Kind, req.Name, req.Environment, req.AppKind, req.Links, req.Env); err != nil {
			return Source{}, err
		}
		if env := envOrDefault(s.Environment); env != DefaultEnvironment {
			p.mu.Lock()
			_, _, err = p.ensureEnvLocked(env)
			p.mu.Unlock()
			if err != nil {
				return Source{}, err
			}
		}
	}
	s.Branch, s.Path = firstNonEmpty(req.Branch, ref.DefaultBranch), req.Path
	s.AutoDeploy = req.AutoDeploy != nil && *req.AutoDeploy
	s.InstallationID, s.RepoID, s.RepoFullName, s.ConnectedBy = inst, ref.ID, ref.FullName, u.ID
	s.Repo = strings.TrimSuffix(p.gh.CloneURL(ref.FullName), ".git")
	if s.Kind == "" {
		s.Kind = "app"
	}
	out, err := p.connectSource(s)
	if err == nil {
		p.audit.Log("source.connect", map[string]any{"actor": "user:" + u.Login, "target": s.Kind + "/" + s.Target,
			"installation": inst, "repo": ref.ID, "repo_name": ref.FullName, "branch": s.Branch})
	}
	return out, err
}

// MigrateSource moves a legacy token source onto the GitHub App and forgets
// its stored token.
func (p *Platform) MigrateSource(ctx context.Context, u *ghapp.User, id string) (Source, error) {
	if p.gh == nil {
		return Source{}, errors.New("the GitHub App isn't configured on this server")
	}
	s := p.source(id)
	if s == nil {
		return Source{}, errors.New("no such source")
	}
	if s.InstallationID != 0 {
		return Source{}, errors.New("this source already uses the GitHub App")
	}
	m := ghRepoRE.FindStringSubmatch(s.Repo)
	if m == nil {
		return Source{}, errors.New("only github.com repositories can move to the GitHub App")
	}
	inst, ref, err := p.gh.FindRepo(ctx, u.ID, m[1]+"/"+m[2])
	if err != nil {
		return Source{}, err
	}
	p.mu.Lock()
	s.InstallationID, s.RepoID, s.RepoFullName, s.ConnectedBy = inst, ref.ID, ref.FullName, u.ID
	s.Token, s.Disabled = "", ""
	cp := *s
	p.mu.Unlock()
	_ = p.save()
	p.audit.Log("source.connect", map[string]any{"actor": "user:" + u.Login, "target": s.Kind + "/" + s.Target,
		"installation": inst, "repo": ref.ID, "repo_name": ref.FullName, "migrated_from": "token"})
	p.event(s.Target, "deploy", "moved to the GitHub App; the stored token was deleted. Remove the old webhook from %s's settings on GitHub", ref.FullName)
	out := cp.redacted()
	return out, nil
}

// ---- HTTP ----

// githubAPI adds the platform's GitHub endpoints (behind sign-in).
func (p *Platform) githubAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/github", func(w http.ResponseWriter, r *http.Request) {
		st := map[string]any{"configured": p.gh != nil}
		if p.gh != nil {
			c := p.gh.Config()
			st["app"], st["install_url"], st["shared"] = c.Slug, p.gh.InstallURL(), c.Shared
			if !c.Shared {
				st["webhook_url"] = p.GitHubWebhookURL()
			}
			if u := ghapp.UserFrom(r.Context()); u != nil {
				st["login"] = u.Login
			}
		} else if p.cfg.GitHubSecretsDir != "" {
			st["setup_url"] = "/github/manifest"
		}
		st["browser"] = p.gh != nil && !p.gh.Config().Shared // sign in by going to GitHub and back
		writeJSON(w, http.StatusOK, st)
	})
	mux.HandleFunc("POST /v1/sources/github", func(w http.ResponseWriter, r *http.Request) {
		var req GitHubSourceRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "bad request: "+err.Error())
			return
		}
		out, err := p.ConnectGitHubSource(r.Context(), ghapp.UserFrom(r.Context()), req)
		if err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("POST /v1/sources/{id}/migrate", func(w http.ResponseWriter, r *http.Request) {
		out, err := p.MigrateSource(r.Context(), ghapp.UserFrom(r.Context()), r.PathValue("id"))
		if err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /v1/audit/verify", func(w http.ResponseWriter, r *http.Request) {
		n, err := ghapp.VerifyAudit(p.audit.Path())
		res := map[string]any{"records": n, "ok": err == nil, "path": p.audit.Path()}
		if err != nil {
			res["error"] = err.Error()
		}
		writeJSON(w, http.StatusOK, res)
	})
}

// needsGitHubSession reports the endpoints that act with someone's GitHub
// identity: listing their repositories and connecting or migrating one.
// Everything else in the API works without GitHub, as before.
func needsGitHubSession(r *http.Request) bool {
	path := r.URL.Path
	switch path {
	case "/v1/github/installations", "/v1/sources/github", "/v1/auth/whoami", "/v1/auth/logout":
		return true
	}
	return strings.HasPrefix(path, "/v1/sources/") && strings.HasSuffix(path, "/migrate")
}

// withGitHub wires the GitHub routes into the API mux. A GitHub session is
// needed only for the GitHub endpoints; the rest of the API is unchanged.
// Until an app is set up, the one-click setup (/github/manifest) is served;
// once it creates the app, the sign-in routes are added to the same mux.
func (p *Platform) withGitHub(mux *http.ServeMux) http.Handler {
	p.githubAPI(mux)
	a := &apiMux{mux: mux}
	p.ghMu.Lock()
	p.apiMuxes = append(p.apiMuxes, a)
	gh := p.gh
	p.ghMu.Unlock()
	if gh == nil {
		ghapp.ManifestRoutes(mux, p.manifestConfig())
	} else {
		p.wireGitHub(a, gh)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := a.h.Load(); h != nil {
			(*h).ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// apiMux is one API handler: its mux and, once GitHub is on, the mux
// behind the sign-in middleware.
type apiMux struct {
	mux   *http.ServeMux
	wired bool
	h     atomic.Pointer[http.Handler]
}

func (p *Platform) wireGitHub(a *apiMux, gh *ghapp.Service) {
	if a.wired {
		return
	}
	a.wired = true
	if !gh.Config().Shared {
		gh.BrowserRoutes(a.mux)
	}
	gh.APIRoutes(a.mux)
	var h http.Handler = gh.Middleware(a.mux, func(r *http.Request) bool { return !needsGitHubSession(r) })
	a.h.Store(&h)
}

// EnableSavedGitHub turns on the GitHub App that the one-click setup saved
// in this computer's data folder, if there is one.
func (p *Platform) EnableSavedGitHub() (bool, error) {
	env := filepath.Join(p.cfg.GitHubSecretsDir, "github.env")
	if _, err := os.Stat(env); err != nil {
		return false, nil
	}
	cfg, err := ghapp.LoadConfigFile(env)
	if err != nil {
		return false, fmt.Errorf("%s: %w", env, err)
	}
	return true, p.EnableGitHub(cfg)
}

func (p *Platform) manifestConfig() *ghapp.ManifestConfig {
	web, api := os.Getenv(ghapp.EnvWebURL), os.Getenv(ghapp.EnvAPIURL)
	if web == "" {
		web, api = "https://github.com", "https://api.github.com"
	} else if api == "" {
		api = strings.TrimRight(web, "/") + "/api/v3"
	}
	name := "xu526-" + strings.ReplaceAll(p.cfg.Domain, ".", "-")
	if h, err := os.Hostname(); err == nil && h != "" {
		name = "xu526-" + strings.ToLower(h)
	}
	if len(name) > 34 { // GitHub App names: at most 34 characters
		name = name[:34]
	}
	return &ghapp.ManifestConfig{SecretsDir: p.cfg.GitHubSecretsDir, WebURL: web, APIURL: api,
		Name: name, DashboardURL: strings.TrimSuffix(os.Getenv(ghapp.EnvCallbackURL), "/github/callback"),
		WebhookURL: p.GitHubWebhookURL(),
		Configured: func() bool { return p.GitHub() != nil },
		OnCreated: func(envFile string) error {
			cfg, err := ghapp.LoadConfigFile(envFile)
			if err != nil {
				return err
			}
			if err := p.EnableGitHub(cfg); err != nil {
				return err
			}
			p.log.Printf("GitHub App %s created and turned on", cfg.Slug)
			return nil
		}}
}

func sha256Sum(b []byte) [32]byte { return sha256.Sum256(b) }
