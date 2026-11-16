package platform

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"xu526/internal/dotenv"
	"xu526/internal/runtime"
)

// Source connects an app or stack to a git repository. Pushes to its branch
// (through a GitHub webhook, or "Deploy now") build and deploy a new release.
type Source struct {
	ID           string   `json:"id"`
	Kind         string   `json:"kind"`          // app | stack
	Target       string   `json:"target"`        // app or stack name
	Repo         string   `json:"repo"`          // https://github.com/owner/repo (or any git URL)
	Dir          string   `json:"dir,omitempty"` // a folder on this computer instead of a repository
	Environment  string   `json:"environment,omitempty"`
	StackName    string   `json:"stack_name,omitempty"` // stack sources: the name without the environment
	Branch       string   `json:"branch"`
	Path         string   `json:"path"`                 // app: build folder; stack: compose file
	Dockerfile   string   `json:"dockerfile,omitempty"` // app only, relative to Path
	Token        string   `json:"token,omitempty"`      // for private repos and creating the webhook
	Secret       string   `json:"webhook_secret"`
	AutoDeploy   bool     `json:"auto_deploy"`
	// NoHostBuild: don't build this repository with xu build when it has no
	// Dockerfile (see hostBuildOK).
	NoHostBuild bool `json:"no_host_build,omitempty"`
	PushRegistry string   `json:"push_registry,omitempty"` // registry name to push built images to
	PushRepo     string   `json:"push_repo,omitempty"`     // e.g. myorg/myapp
	App          *AppSpec `json:"app,omitempty"`           // settings for app deploys (port, scaling, env)

	// GitHub App sources: bound to immutable IDs, cloned with a short-lived
	// token for that one repository. Token and Secret stay empty.
	InstallationID int64  `json:"installation_id,omitempty"`
	RepoID         int64  `json:"repo_id,omitempty"`
	RepoFullName   string `json:"repo_full_name,omitempty"` // display only; refreshed from GitHub on each build
	ConnectedBy    string `json:"connected_by,omitempty"`   // xu user ID that connected it
	Disabled       string `json:"disabled,omitempty"`       // why builds are paused
	Auth           string `json:"auth"`                     // github-app | token (legacy) | none

	WebhookURL  string    `json:"webhook_url"`
	LastCommit  string    `json:"last_commit,omitempty"`
	LastStatus  string    `json:"last_status,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	LastBuildAt time.Time `json:"last_build_at,omitempty"`
	Building    bool      `json:"building"`

	// Checking for new commits (watch.go).
	Pending    *PendingCommit `json:"pending,omitempty"`     // on the branch, not deployed yet
	CheckedAt  time.Time      `json:"checked_at,omitempty"`  // last check
	CheckError string         `json:"check_error,omitempty"` // why the last check failed
}

func (s Source) redacted() Source {
	switch {
	case s.InstallationID != 0:
		s.Auth = "github-app"
	case s.Token != "":
		s.Auth = "token"
	default:
		s.Auth = "none"
	}
	if s.Token != "" {
		s.Token = "••••" + tail(s.Token, 4)
	}
	return s
}

func tail(s string, n int) string {
	if len(s) <= n {
		return ""
	}
	return s[len(s)-n:]
}

type buildQueue struct {
	running bool
	next    *buildReq
}

type buildReq struct {
	commit string
	meta   ReleaseMeta
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var ghRepoRE = regexp.MustCompile(`github\.com[/:]([^/]+)/([^/]+?)(\.git)?/?$`)

func (p *Platform) webhookURL(id string) string {
	u := "http://xu." + p.cfg.Domain
	if p.cfg.PublicPort != 0 && p.cfg.PublicPort != 80 {
		u += ":" + strconv.Itoa(p.cfg.PublicPort)
	}
	return u + "/hooks/github/" + id
}

// ConnectSource links a repo to an app or stack and starts the first build.
func (p *Platform) ConnectSource(s Source) (Source, error) {
	// GitHub App fields are set only by ConnectGitHubSource, after checking
	// the user's access; a request body can't supply them.
	s.InstallationID, s.RepoID, s.RepoFullName, s.ConnectedBy, s.Disabled = 0, 0, "", "", ""
	s.Dir, s.StackName = "", "" // folders connect through ConnectFolder
	return p.connectSource(s)
}

func (p *Platform) connectSource(s Source) (Source, error) {
	s.Repo = strings.TrimSpace(s.Repo)
	if s.Repo == "" {
		return s, errors.New("enter the repository URL, e.g. https://github.com/you/app")
	}
	if strings.HasPrefix(s.Repo, "-") || strings.ContainsAny(s.Repo, " \n") {
		return s, errors.New("that doesn't look like a repository URL")
	}
	if s.Branch == "" {
		s.Branch = "main"
	}
	switch s.Kind {
	case "app":
		if s.Path == "" {
			s.Path = "."
		}
		if s.App == nil {
			return s, errors.New("app settings are required")
		}
		s.App.Name = s.Target
		s.App.Image = "xu/" + s.Target // filled per build
		port := s.App.Port
		if port == 0 {
			s.App.Port = 8080 // the real port comes from the build
		}
		err := s.App.Normalize()
		if port == 0 {
			s.App.Port = 0
		}
		if err != nil {
			return s, err
		}
	case "stack":
		if s.Path == "" {
			s.Path = "docker-compose.yml"
		}
		if !stackNameRE.MatchString(s.Target) {
			return s, errors.New("stack name: use 1-20 lowercase letters, digits and dashes")
		}
	default:
		return s, errors.New("kind must be app or stack")
	}
	if !nameRE.MatchString(s.Target) {
		return s, fmt.Errorf("invalid name %q", s.Target)
	}
	if s.PushRegistry != "" && s.PushRepo == "" {
		return s, errors.New("enter the repository to push to, e.g. myorg/myapp")
	}
	if _, err := safeJoin("/repo", s.Path); err != nil {
		return s, err
	}

	p.mu.Lock()
	for _, o := range p.sources {
		if o.Kind == s.Kind && o.Target == s.Target {
			s.ID, s.Secret = o.ID, o.Secret // reconnect keeps the webhook working
			if s.InstallationID != 0 {
				s.Token = "" // App sources never keep a stored token
			} else if s.Token == "" || strings.HasPrefix(s.Token, "••") {
				s.Token = o.Token
			}
		}
	}
	if s.ID == "" {
		s.ID = randomHex(8)
		s.Secret = randomHex(20)
	}
	s.WebhookURL = p.webhookURL(s.ID)
	if s.InstallationID != 0 {
		s.WebhookURL = ""
	}
	cp := s
	p.sources[s.ID] = &cp
	p.mu.Unlock()
	_ = p.save()
	p.event(s.Target, "deploy", "connected to %s (%s)", s.Repo, s.Branch)
	p.TriggerBuild(s.ID, ReleaseMeta{Source: "github", Branch: s.Branch})
	return cp.redacted(), nil
}

func (p *Platform) source(id string) *Source {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.sources[id]
}

// Sources lists connected repositories.
func (p *Platform) Sources() []Source {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := []Source{}
	for _, s := range p.sources {
		c := s.redacted()
		c.WebhookURL = p.webhookURL(s.ID)
		if s.InstallationID != 0 || s.Dir != "" {
			c.WebhookURL = "" // the app's own webhook covers it; folders have none
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Target < out[j].Target })
	return out
}

// SourceFor returns the source connected to an app or stack.
func (p *Platform) SourceFor(kind, target string) (Source, bool) {
	for _, s := range p.Sources() {
		if s.Kind == kind && s.Target == target {
			return s, true
		}
	}
	return Source{}, false
}

// SourceSecret returns the webhook secret (shown once to paste into GitHub).
func (p *Platform) SourceSecret(id string) (string, bool) {
	s := p.source(id)
	if s == nil {
		return "", false
	}
	return s.Secret, true
}

// SetAutoDeploy turns deploy-on-push on or off.
func (p *Platform) SetAutoDeploy(id string, on bool) error {
	p.mu.Lock()
	s := p.sources[id]
	if s != nil {
		s.AutoDeploy = on
	}
	p.mu.Unlock()
	if s == nil {
		return errors.New("no such source")
	}
	return p.save()
}

// DisconnectSource removes the repo link (the app keeps running).
func (p *Platform) DisconnectSource(id string) error {
	p.mu.Lock()
	s := p.sources[id]
	delete(p.sources, id)
	p.mu.Unlock()
	if s == nil {
		return errors.New("no such source")
	}
	_ = os.RemoveAll(filepath.Join(p.dataDir, "builds", id))
	return p.save()
}

// TriggerBuild queues a build. While one runs, newer pushes collapse into a
// single follow-up build of the latest commit.
func (p *Platform) TriggerBuild(id string, m ReleaseMeta) {
	p.bmu.Lock()
	q := p.builds[id]
	if q == nil {
		q = &buildQueue{}
		p.builds[id] = q
	}
	req := &buildReq{commit: m.Commit, meta: m}
	if q.running {
		q.next = req
		p.bmu.Unlock()
		return
	}
	q.running = true
	p.bmu.Unlock()
	go func() {
		for req != nil {
			p.runBuild(id, req)
			p.bmu.Lock()
			req, q.next = q.next, nil
			if req == nil {
				q.running = false
			}
			p.bmu.Unlock()
		}
	}()
}

func (p *Platform) setSource(id string, fn func(s *Source)) {
	p.mu.Lock()
	if s := p.sources[id]; s != nil {
		fn(s)
	}
	p.mu.Unlock()
	_ = p.save()
}

func (p *Platform) runBuild(id string, req *buildReq) {
	src := p.source(id)
	if src == nil {
		return
	}
	s := *src
	if s.App != nil {
		app := *s.App
		s.App = &app
	}
	if s.Disabled != "" {
		p.setSource(id, func(x *Source) { x.LastStatus, x.LastError = "disabled", x.Disabled })
		p.event(s.Target, "error", "build skipped: %s", s.Disabled)
		return
	}
	ctx, cancel := context.WithTimeout(p.ctx, 30*time.Minute)
	defer cancel()

	m := req.meta
	m.Source, m.Branch = "github", s.Branch
	if s.Dir != "" {
		m.Source, m.Branch = "folder", ""
	}
	rel := p.newRelease(s.Kind, s.Target, "building", m)
	logw := p.releaseLog(rel)
	defer logw.Close()
	p.setSource(id, func(x *Source) { x.Building, x.LastStatus, x.LastError = true, "building", "" })
	var err error
	if s.Dir != "" {
		fmt.Fprintf(logw, "== release %d of %s: building the folder %s\n", rel.Version, s.Target, s.Dir)
		p.event(s.Target, "deploy", "building release %d from %s", rel.Version, s.Dir)
		err = p.folderBuildAndDeploy(ctx, &s, rel, logw)
	} else {
		fmt.Fprintf(logw, "== release %d of %s: building %s (%s)\n", rel.Version, s.Target, s.Repo, s.Branch)
		p.event(s.Target, "deploy", "building release %d from %s", rel.Version, s.Branch)
		err = p.buildAndDeploy(ctx, &s, req.commit, rel, logw)
	}
	if err != nil {
		fmt.Fprintf(logw, "\n== FAILED: %v\n", err)
		if rel.Status == "building" || rel.Status == "deploying" {
			p.finishRelease(rel, err)
		}
		p.event(s.Target, "error", "release %d failed: %v", rel.Version, err)
	} else {
		fmt.Fprintf(logw, "\n== release %d is live\n", rel.Version)
	}
	p.setSource(id, func(x *Source) {
		x.Building, x.LastBuildAt, x.LastCommit = false, time.Now(), rel.Commit
		if err == nil && x.Pending != nil && sameCommit(x.Pending.SHA, rel.Commit) {
			x.Pending = nil
		}
		if err != nil {
			x.LastStatus, x.LastError = "failed", err.Error()
		} else {
			x.LastStatus, x.LastError = "live", ""
		}
	})
}

func (p *Platform) buildAndDeploy(ctx context.Context, s *Source, commit string, rel *Release, logw io.Writer) error {
	d, ok := p.cfg.Docker.(*runtime.Docker)
	if !ok {
		return errors.New("building from git needs Docker")
	}
	repoDir := filepath.Join(p.dataDir, "builds", s.ID, "repo")
	auth, err := p.gitAuth(ctx, s)
	if err != nil {
		return err
	}
	info, err := gitSync(ctx, s, repoDir, commit, logw, auth)
	p.auditClone(s, auth, rel, info, err)
	if err != nil {
		return err
	}
	p.mu.Lock()
	rel.Commit, rel.CommitMsg, rel.Author = info.sha, info.subject, info.author
	p.mu.Unlock()
	fmt.Fprintf(logw, "== commit %s by %s: %s\n", info.sha[:12], info.author, info.subject)
	tag := info.sha[:12]
	regCfg := p.registryConfig(ctx)

	build := func(name, ctxDir, dockerfile string) (string, string, error) {
		local := "xu/" + name + ":" + tag
		fmt.Fprintf(logw, "\n== building %s\n", local)
		var id string
		var err error
		if hasDockerfile(ctxDir, dockerfile) {
			id, err = d.Build(ctx, ctxDir, dockerfile, local, regCfg, logw)
		} else if dockerfile != "" && dockerfile != "Dockerfile" {
			err = fmt.Errorf("%s not found", dockerfile)
		} else {
			id, err = p.hostBuild(ctx, d, s, ctxDir, local, logw)
		}
		if err != nil {
			return "", "", fmt.Errorf("build %s: %w", name, err)
		}
		ref := local
		if s.PushRegistry != "" {
			pushed, err := p.push(ctx, d, s, id, name, tag, logw)
			if err != nil {
				return "", "", err
			}
			ref = pushed
		}
		return ref, id, nil
	}

	switch s.Kind {
	case "app":
		ctxDir, err := safeJoin(repoDir, s.Path)
		if err != nil {
			return err
		}
		ref, id, err := build(s.Target, ctxDir, s.Dockerfile)
		if err != nil {
			return err
		}
		spec := *s.App
		spec.Image, spec.ImageID, spec.ImageDigest = ref, id, ""
		// .env files committed with the app become its environment (never
		// part of the image); the source's own settings fill in the rest.
		if vars, read, err := dotenv.Load(ctxDir, dotenv.ProductionFiles); err != nil {
			return fmt.Errorf("read env files: %w", err)
		} else if len(read) > 0 {
			merged := map[string]string{}
			for k, v := range spec.Env {
				merged[k] = v
			}
			for k, v := range vars {
				merged[k] = v
			}
			spec.Env = merged
			fmt.Fprintf(logw, "\n== %s\n", dotenv.Summary(vars, read))
		}
		// Apps connected from the dashboard leave the port and kind to the build.
		if spec.Kind == "worker" {
			spec.Port, spec.Internal = 0, true
			spec.MinScale = max(1, spec.MinScale)
			spec.MaxScale = max(spec.MinScale, spec.MaxScale)
		} else {
			if spec.Port == 0 {
				spec.Port = d.ImagePortOf(ctx, id)
			}
			if spec.Port == 0 {
				spec.Port = 8080
			}
			if spec.Kind == "" {
				spec.Kind = "api"
			}
			if spec.Port != s.App.Port || spec.Kind != s.App.Kind {
				p.setSource(s.ID, func(x *Source) {
					if x.App != nil {
						x.App.Port, x.App.Kind = spec.Port, spec.Kind
					}
				})
			}
		}
		p.setReleaseStatus(rel, "deploying")
		fmt.Fprintf(logw, "\n== deploying %s\n", ref)
		pinned, err := p.deployApp(ctx, spec)
		if err == nil {
			p.mu.Lock()
			rel.Spec = &pinned
			p.mu.Unlock()
		}
		p.finishRelease(rel, err)
		return err

	case "stack":
		composePath, err := safeJoin(repoDir, s.Path)
		if err != nil {
			return err
		}
		yml, err := os.ReadFile(composePath)
		if err != nil {
			return fmt.Errorf("read %s: %w", s.Path, err)
		}
		// Like a folder: the stack's name and environment, the files it
		// reads, and one docker compose build for the services with a
		// Dockerfile (when docker compose is installed and nothing is
		// pushed to a registry).
		t := p.targetOf(s.Target)
		if s.StackName != "" {
			t = stackTarget{Stack: s.StackName, Env: envOrDefault(s.Environment)}
		}
		t.Files = StackFiles(filepath.Dir(composePath), yml)
		plan := p.planStack(t, yml, nil)
		if len(plan.Errors) > 0 {
			return fmt.Errorf("the stack file has problems: %s", strings.Join(plan.Errors, "; "))
		}
		composeDir := filepath.Dir(composePath)
		built := map[string]string{}
		var withDockerfile []string
		for _, svc := range plan.Services {
			if svc.Build == nil {
				continue
			}
			ctxDir, err := safeJoin(composeDir, svc.Build.Context)
			if err != nil {
				return fmt.Errorf("service %s: %w", svc.Name, err)
			}
			if rel, _ := filepath.Rel(repoDir, ctxDir); strings.HasPrefix(rel, "..") {
				return fmt.Errorf("service %s: build context leaves the repository", svc.Name)
			}
			if svc.Build.HasDockerfile(ctxDir) {
				withDockerfile = append(withDockerfile, svc.Name)
			}
		}
		if len(withDockerfile) > 0 && s.PushRegistry == "" {
			file, refs, err := ComposeBuildFile(repoBuildPlan(plan, logw), composeDir, tag, withDockerfile)
			if err != nil {
				return err
			}
			fmt.Fprintf(logw, "\n== building %s with docker compose\n", strings.Join(withDockerfile, ", "))
			switch _, err := d.ComposeBuild(ctx, file, composeDir, "xu-"+plan.Stack, refs, logw); {
			case errors.Is(err, runtime.ErrNoCompose):
				fmt.Fprintln(logw, "docker compose isn't installed here: building each service on its own")
			case err != nil:
				return err
			default:
				for svc, ref := range refs {
					built[svc] = ref
				}
			}
		}
		for _, svc := range plan.Services {
			if svc.Build == nil || built[svc.Name] != "" {
				continue
			}
			ctxDir, _ := safeJoin(composeDir, svc.Build.Context)
			ref, _, err := build(s.Target+"-"+appSafe(svc.Name), ctxDir, svc.Build.Dockerfile)
			if err != nil {
				return err
			}
			built[svc.Name] = ref
		}
		p.setReleaseStatus(rel, "deploying")
		fmt.Fprintf(logw, "\n== deploying stack %s\n", t.Stack)
		_, err = p.deployStackTo(ctx, t, yml, nil, built, ReleaseMeta{}, rel)
		return err
	}
	return nil
}

func (p *Platform) push(ctx context.Context, d *runtime.Docker, s *Source, id, name, tag string, logw io.Writer) (string, error) {
	var reg *Registry
	p.mu.RLock()
	for _, r := range p.registries {
		if r.Name == s.PushRegistry {
			reg = r
		}
	}
	p.mu.RUnlock()
	if reg == nil {
		return "", fmt.Errorf("registry %q isn't configured", s.PushRegistry)
	}
	repo := strings.Trim(s.PushRepo, "/")
	if s.Kind == "stack" {
		repo += "-" + strings.TrimPrefix(name, s.Target+"-")
	}
	if reg.Server != "docker.io" {
		repo = reg.Server + "/" + repo
	}
	auth, err := p.authFor(ctx, repo)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(logw, "\n== pushing %s:%s\n", repo, tag)
	if err := d.Push(ctx, id, repo, tag, auth, logw); err != nil {
		return "", fmt.Errorf("push to %s: %w", s.PushRegistry, err)
	}
	return repo + ":" + tag, nil
}

// safeJoin joins rel under root and refuses paths (or symlinks) that escape it.
func safeJoin(root, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("path %q must be relative to the repository", rel)
	}
	p := filepath.Join(root, filepath.Clean("/"+rel))
	if real, err := filepath.EvalSymlinks(p); err == nil {
		rootReal, _ := filepath.EvalSymlinks(root)
		if r, err := filepath.Rel(rootReal, real); err != nil || strings.HasPrefix(r, "..") {
			return "", fmt.Errorf("path %q points outside the repository", rel)
		}
	}
	return p, nil
}

type commitInfo struct{ sha, author, subject string }

// gitSync fetches commit (or the branch head) into dir with the git CLI.
func gitSync(ctx context.Context, s *Source, dir, commit string, logw io.Writer, auth gitAuth) (commitInfo, error) {
	git := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		// A minimal environment: xud's own (which may hold secrets) is not
		// inherited. Credentials arrive through GIT_CONFIG_*, never argv.
		cmd.Env = append(gitBaseEnv(), auth.env...)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		if err != nil {
			return out.String(), fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(redactToken(out.String(), auth.secret)))
		}
		return out.String(), nil
	}
	if _, err := exec.LookPath("git"); err != nil {
		return commitInfo{}, errors.New("git is not installed on this server (sudo apt install git)")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return commitInfo{}, err
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if _, err := git("init", "-q"); err != nil {
			return commitInfo{}, err
		}
		if _, err := git("remote", "add", "origin", auth.url); err != nil {
			return commitInfo{}, err
		}
	} else if _, err := git("remote", "set-url", "origin", auth.url); err != nil {
		return commitInfo{}, err
	}
	fmt.Fprintf(logw, "== fetching %s %s\n", auth.display, firstNonEmpty(commit, s.Branch))
	fetched := false
	if commit != "" {
		if _, err := git("fetch", "-q", "--depth", "1", "origin", commit); err == nil {
			fetched = true
		}
	}
	if !fetched {
		if _, err := git("fetch", "-q", "--depth", "1", "origin", "refs/heads/"+s.Branch); err != nil {
			return commitInfo{}, err
		}
	}
	if _, err := git("checkout", "-q", "--force", "FETCH_HEAD"); err != nil {
		return commitInfo{}, err
	}
	if _, err := git("clean", "-q", "-fdx"); err != nil {
		return commitInfo{}, err
	}
	out, err := git("log", "-1", "--format=%H%n%an%n%s")
	if err != nil {
		return commitInfo{}, err
	}
	parts := strings.SplitN(strings.TrimSpace(out), "\n", 3)
	for len(parts) < 3 {
		parts = append(parts, "")
	}
	if commit != "" && !strings.HasPrefix(parts[0], commit) {
		fmt.Fprintf(logw, "== note: %s could not be fetched directly; building the branch head %s\n", commit, parts[0][:12])
	}
	return commitInfo{sha: parts[0], author: parts[1], subject: parts[2]}, nil
}

func redactToken(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "••••")
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ---- webhooks ----

// publicEndpoints serves http://xu.<domain>/: GitHub webhooks only. The
// dashboard and control API are never exposed here.
func (p *Platform) publicEndpoints() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hooks/github/{id}", p.githubWebhook) // legacy per-repo webhooks
	if p.gh != nil {
		if p.gh.Config().WebhookSecret != "" { // the shared app has no webhook: xu526 checks for commits itself
			mux.Handle("POST /github/webhook", p.gh.WebhookHandler()) // the GitHub App's webhook
		}
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "xu is running\n")
	})
	return mux
}

func (p *Platform) githubWebhook(w http.ResponseWriter, r *http.Request) {
	s := p.source(r.PathValue("id"))
	// Folder and GitHub App sources have no webhook secret: anyone could sign
	// with an empty key, so they have no repository webhook at all.
	if s == nil || s.Secret == "" || s.Dir != "" {
		http.Error(w, "unknown webhook", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 10<<20))
	if err != nil {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	if !validSignature(s.Secret, body, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(w, "signature does not match the webhook secret", http.StatusUnauthorized)
		return
	}
	switch r.Header.Get("X-GitHub-Event") {
	case "ping":
		p.event(s.Target, "deploy", "GitHub webhook connected")
		io.WriteString(w, "pong\n")
		return
	case "push":
	default:
		io.WriteString(w, "ignored event\n")
		return
	}
	var push struct {
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Deleted    bool   `json:"deleted"`
		HeadCommit struct {
			ID      string `json:"id"`
			Message string `json:"message"`
			Author  struct {
				Name string `json:"name"`
			} `json:"author"`
		} `json:"head_commit"`
	}
	if err := json.Unmarshal(body, &push); err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	if push.Deleted || push.Ref != "refs/heads/"+s.Branch {
		fmt.Fprintf(w, "ignored: push to %s, watching %s\n", push.Ref, s.Branch)
		return
	}
	msg, _, _ := strings.Cut(push.HeadCommit.Message, "\n")
	if !s.AutoDeploy {
		p.notePending(s.ID, PendingCommit{SHA: push.After, Message: push.HeadCommit.Message, Author: push.HeadCommit.Author.Name}, "")
		io.WriteString(w, "new commit noted; deploy it from the dashboard\n")
		return
	}
	p.TriggerBuild(s.ID, ReleaseMeta{Commit: push.After, CommitMsg: msg, Author: push.HeadCommit.Author.Name})
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, "building %s\n", short(push.After))
}

func validSignature(secret string, body []byte, header string) bool {
	sig, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hmac.Equal(got, m.Sum(nil))
}

// GitHubAPI is the base URL for GitHub's API (replaceable in tests).
var GitHubAPI = "https://api.github.com"

// CreateGitHubWebhook adds the webhook to the repository using the token.
func (p *Platform) CreateGitHubWebhook(ctx context.Context, id string) error {
	s := p.source(id)
	if s == nil {
		return errors.New("no such source")
	}
	m := ghRepoRE.FindStringSubmatch(s.Repo)
	if m == nil {
		return errors.New("only github.com repositories can have their webhook created automatically")
	}
	if s.Token == "" {
		return errors.New("add a GitHub token with permission to manage webhooks, or add the webhook by hand")
	}
	body, _ := json.Marshal(map[string]any{
		"name": "web", "active": true, "events": []string{"push"},
		"config": map[string]string{"url": p.webhookURL(s.ID), "content_type": "json", "secret": s.Secret},
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", GitHubAPI+"/repos/"+m[1]+"/"+m[2]+"/hooks", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
			Errors  []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		msg := e.Message
		if len(e.Errors) > 0 {
			msg += ": " + e.Errors[0].Message
		}
		return fmt.Errorf("GitHub said %s (%s)", resp.Status, msg)
	}
	p.event(s.Target, "deploy", "webhook created on GitHub")
	return nil
}

// repoBuildOK are the build: settings a repository's compose file may use.
// The rest (secrets, ssh, additional_contexts, network, entitlements, …)
// would hand files, keys or networks of this computer to the repository's
// Dockerfile, so they're kept for folders, which the computer's owner picked.
var repoBuildOK = map[string]bool{"labels": true, "cache_from": true, "cache_to": false, "shm_size": true, "platforms": true,
	"pull": true, "no_cache": true, "tags": false}

// repoBuildPlan is plan with only the build settings a repository may use.
func repoBuildPlan(plan *StackPlan, logw io.Writer) *StackPlan {
	cp := *plan
	cp.BuildSecrets = nil
	cp.Services = make([]PlanService, len(plan.Services))
	for i, svc := range plan.Services {
		cp.Services[i] = svc
		if svc.Build == nil || len(svc.Build.Extra) == 0 {
			continue
		}
		b := *svc.Build
		b.Extra = map[string]any{}
		var dropped []string
		for k, v := range svc.Build.Extra {
			if repoBuildOK[k] {
				b.Extra[k] = v
			} else {
				dropped = append(dropped, k)
			}
		}
		if len(dropped) > 0 {
			sort.Strings(dropped)
			fmt.Fprintf(logw, "%s: build %s ignored (only used for folders on this computer)\n", svc.Name, strings.Join(dropped, ", "))
		}
		cp.Services[i].Build = &b
	}
	return &cp
}
