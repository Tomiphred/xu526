package platform

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"html"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"xu526/internal/ghapp"
	"xu526/internal/ghapp/ghtest"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Dev", "GIT_AUTHOR_EMAIL=dev@example.com", "GIT_COMMITTER_NAME=Dev", "GIT_COMMITTER_EMAIL=dev@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

type ghEnv struct {
	gh    *ghtest.Fake
	p     *Platform
	api   *httptest.Server
	clock func() time.Time
	adv   func(time.Duration)
	dir   string
	work  string // a working copy that pushes to acme/api
	bare  string
}

func newGHEnv(t *testing.T) *ghEnv { return newGHEnvMode(t, false) }

// newGHEnvMode: shared = the one public xu526 app (client ID only: no
// private key, no client secret, no webhooks).
func newGHEnvMode(t *testing.T, shared bool) *ghEnv {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	gh := ghtest.New()
	t.Cleanup(gh.Close)
	// A real repository served over HTTP at /acme/api.git.
	root := t.TempDir()
	bare := filepath.Join(root, "acme", "api.git")
	os.MkdirAll(filepath.Dir(bare), 0o755)
	run(t, root, "init", "-q", "--bare", "-b", "main", bare)
	work := t.TempDir()
	run(t, work, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(work, "main.go"), []byte("package main\nfunc main(){}\n"), 0o644)
	run(t, work, "add", ".")
	run(t, work, "commit", "-qm", "first")
	run(t, work, "push", "-q", bare, "main")
	gh.Repos[101] = &ghtest.Repo{ID: 101, FullName: "acme/api", Private: true, GitDir: bare}
	gh.Repos[102] = &ghtest.Repo{ID: 102, FullName: "acme/secret", Private: true}
	gh.Installations[7] = &ghtest.Installation{ID: 7, Account: "org-acme", AccountID: 900, Selection: "selected", Repos: []int64{101, 102}}
	gh.Users[1] = &ghtest.User{ID: 1, Login: "alice", Access: []int64{101}}

	dir := t.TempDir()
	p, err := New(Config{Domain: "example.test", StatePath: filepath.Join(dir, "apps.json"), Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	key, _ := ghapp.ParsePrivateKey(gh.PEM())
	cfg := &ghapp.Config{AppID: gh.AppID, ClientID: gh.ClientID, Slug: "xu526-test", ClientSecret: gh.ClientSecret,
		WebhookSecret: "whsec", Keys: []*rsaKey{key}, SecretKey: bytes.Repeat([]byte{9}, 32),
		WebURL: gh.URL, APIURL: gh.URL, CallbackURL: "http://127.0.0.1/github/callback", Users: []string{"alice"}}
	if shared {
		cfg = &ghapp.Config{Shared: true, ClientID: gh.ClientID, Slug: "xu526-test", WebURL: gh.URL, APIURL: gh.URL, Users: []string{"alice"}}
	}
	if err := p.EnableGitHub(cfg); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	adv := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	p.gh.SetClock(clock)
	gh.SetClock(clock)
	api := httptest.NewServer(p.API())
	t.Cleanup(api.Close)
	return &ghEnv{gh: gh, p: p, api: api, clock: clock, adv: adv, dir: dir, work: work, bare: bare}
}

func (e *ghEnv) login(t *testing.T) string {
	resp, err := http.Post(e.api.URL+"/v1/auth/device", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var start map[string]any
	json.NewDecoder(resp.Body).Decode(&start)
	resp.Body.Close()
	e.gh.ApproveDevice(start["user_code"].(string), 1)
	e.adv(6 * time.Second)
	b, _ := json.Marshal(map[string]any{"id": start["id"]})
	resp, _ = http.Post(e.api.URL+"/v1/auth/device/poll", "application/json", bytes.NewReader(b))
	var done map[string]any
	json.NewDecoder(resp.Body).Decode(&done)
	resp.Body.Close()
	tok, _ := done["token"].(string)
	if tok == "" {
		t.Fatalf("login: %v", done)
	}
	return tok
}

func (e *ghEnv) do(t *testing.T, method, path, token string, body any) (int, string) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.api.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func audit(t *testing.T, p *Platform) string {
	b, _ := os.ReadFile(p.audit.Path())
	return string(b)
}

func TestGitHubAppSources(t *testing.T) {
	e := newGHEnv(t)
	// GitHub is optional: the API works without signing in...
	for _, path := range []string{"/v1/apps", "/v1/stacks", "/v1/sources", "/v1/node"} {
		if code, body := e.do(t, "GET", path, "", nil); code != 200 {
			t.Fatalf("%s without GitHub sign-in: %d %s", path, code, body)
		}
	}
	// ...and only acting with someone's GitHub identity needs a session.
	if code, _ := e.do(t, "GET", "/v1/github/installations", "", nil); code != 401 {
		t.Fatalf("installations without sign-in: %d", code)
	}
	if code, _ := e.do(t, "POST", "/v1/sources/github", "", map[string]any{"target": "x", "repo": "acme/api"}); code != 401 {
		t.Fatalf("connect without sign-in: %d", code)
	}
	if code, body := e.do(t, "GET", "/v1/github", "", nil); code != 200 || !strings.Contains(body, `"configured":true`) {
		t.Fatalf("status endpoint: %d %s", code, body)
	}
	tok := e.login(t)
	if code, body := e.do(t, "GET", "/v1/github", tok, nil); code != 200 || !strings.Contains(body, `"login":"alice"`) {
		t.Fatalf("status with a session: %d %s", code, body)
	}
	app := map[string]any{"port": 8080, "max_scale": 3, "target_concurrency": 50, "idle_timeout_sec": 300}
	// alice can't see acme/secret, so she can't connect it.
	code, body := e.do(t, "POST", "/v1/sources/github", tok, map[string]any{"target": "secret", "repo": "acme/secret", "app": app, "auto_deploy": false})
	if code != 422 || !strings.Contains(body, "isn't in any GitHub App installation you can access") {
		t.Fatalf("connect acme/secret: %d %s", code, body)
	}
	code, body = e.do(t, "POST", "/v1/sources/github", tok, map[string]any{"target": "secret", "installation_id": 7, "repo_id": 102, "app": app, "auto_deploy": false})
	if code != 422 || !strings.Contains(body, "isn't available to you") {
		t.Fatalf("connect by ID: %d %s", code, body)
	}
	// The legacy endpoint can't be used to smuggle in installation/repo IDs.
	e.do(t, "POST", "/v1/sources", tok, map[string]any{"kind": "app", "target": "sneaky", "repo": "https://github.com/acme/secret",
		"installation_id": 7, "repo_id": 102, "app": app})
	for _, s := range e.p.Sources() {
		if s.Target == "sneaky" && (s.InstallationID != 0 || s.RepoID != 0) {
			t.Fatal("request body set installation/repo IDs without an access check")
		}
	}
	// acme/api connects; the branch defaults to the repo's default branch.
	code, body = e.do(t, "POST", "/v1/sources/github", tok, map[string]any{"target": "api", "repo": "acme/api", "app": app, "auto_deploy": false})
	if code != 200 {
		t.Fatalf("connect acme/api: %d %s", code, body)
	}
	var src Source
	json.Unmarshal([]byte(body), &src)
	if src.InstallationID != 7 || src.RepoID != 101 || src.Branch != "main" || src.Auth != "github-app" || src.WebhookURL != "" || src.Token != "" {
		t.Fatalf("source: %+v", src)
	}

	// Clone with a minted token: no token in argv or .git/config, audited.
	s := e.p.source(src.ID)
	ga, err := e.p.gitAuth(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	repoDir := filepath.Join(t.TempDir(), "repo")
	info, err := gitSync(context.Background(), s, repoDir, "", io.Discard, ga)
	e.p.auditClone(s, ga, &Release{Version: 1, Trigger: "webhook:abc"}, info, err)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repoDir, "main.go")); err != nil {
		t.Fatal("repository not cloned")
	}
	gitCfg, _ := os.ReadFile(filepath.Join(repoDir, ".git", "config"))
	if strings.Contains(string(gitCfg), ga.secret) || strings.Contains(string(gitCfg), "extraheader") {
		t.Fatalf("credentials written to .git/config:\n%s", gitCfg)
	}
	// (Connecting also started a build, which cloned too.)
	for _, c := range e.gh.Clones {
		if c != "acme/api" {
			t.Fatalf("cloned %s", c)
		}
	}
	if len(e.gh.Clones) == 0 {
		t.Fatal("nothing cloned")
	}
	log := audit(t, e.p)
	if strings.Contains(log, ga.secret) || !strings.Contains(log, `"action":"repo.clone"`) || !strings.Contains(log, `"trigger":"webhook:abc"`) ||
		!strings.Contains(log, `"action":"token.mint"`) || !strings.Contains(log, `"commit":"`+info.sha+`"`) {
		t.Fatalf("audit log:\n%s", log)
	}
	if n, err := ghapp.VerifyAudit(e.p.audit.Path()); err != nil || n < 3 {
		t.Fatalf("audit chain: %d %v", n, err)
	}

	// Push webhooks route by IDs; builds carry the delivery ID.
	hook := httptest.NewServer(e.p.publicEndpoints())
	defer hook.Close()
	e.p.SetAutoDeploy(src.ID, true)
	e.gh.Deliver(hook.URL+"/github/webhook", "whsec", "push", map[string]any{"installation": map[string]any{"id": 7}, "ref": "refs/heads/main",
		"after": strings.Repeat("a", 40), "repository": map[string]any{"id": 101, "full_name": "acme/api"},
		"head_commit": map[string]any{"message": "ship it", "author": map[string]any{"name": "Al"}}})
	var rel *Release
	for i := 0; i < 200 && rel == nil; i++ {
		for _, r := range e.p.Releases("app", "api") {
			if strings.HasPrefix(r.Trigger, "webhook:") {
				r := r
				rel = &r
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if rel == nil || rel.CommitMsg != "ship it" || !strings.HasPrefix(rel.Trigger, "webhook:") {
		t.Fatalf("push did not start a release: %+v", rel)
	}

	// The repository leaves the installation: the source stops building.
	e.gh.Deliver(hook.URL+"/github/webhook", "whsec", "installation_repositories", map[string]any{"action": "removed",
		"installation": map[string]any{"id": 7}, "repositories_removed": []map[string]any{{"id": 101, "full_name": "acme/api"}}})
	for i := 0; i < 100 && e.p.source(src.ID).Disabled == ""; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if d := e.p.source(src.ID).Disabled; !strings.Contains(d, "removed from the GitHub App installation") {
		t.Fatalf("not disabled: %q", d)
	}
	e.p.mu.Lock()
	e.p.sources[src.ID].Disabled = ""
	e.p.mu.Unlock()

	// Daily recheck: alice loses access, the source is paused.
	e.gh.Lock()
	e.gh.Users[1].Access = nil
	e.gh.Unlock()
	e.p.RecheckAccess(context.Background())
	if d := e.p.source(src.ID).Disabled; !strings.Contains(d, "alice, who connected it, can no longer access") {
		t.Fatalf("recheck: %q", d)
	}
	if !strings.Contains(audit(t, e.p), `"action":"source.disable"`) {
		t.Fatal("disable not audited")
	}
	// Audit verify endpoint.
	if code, body := e.do(t, "GET", "/v1/audit/verify", tok, nil); code != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("verify: %d %s", code, body)
	}
}

func TestLegacyTokenNotInArgv(t *testing.T) {
	p := &Platform{}
	a, err := p.gitAuth(context.Background(), &Source{Repo: "https://github.com/me/app", Token: "ghp_secret"})
	if err != nil {
		t.Fatal(err)
	}
	if a.kind != "token" || len(a.env) != 3 || !strings.Contains(a.env[1], "http.https://github.com/.extraheader") {
		t.Fatalf("%+v", a)
	}
	for _, kv := range gitBaseEnv() {
		if strings.HasPrefix(kv, "XU_") {
			t.Fatalf("git would inherit %s", kv)
		}
	}
}

type rsaKey = rsa.PrivateKey

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// The shared app: sign in with only a client ID, connect a repository, and
// see new commits as "new commit available" (ask first) within one check.
func TestSharedAppWatchesCommits(t *testing.T) {
	e := newGHEnvMode(t, true)
	if code, body := e.do(t, "GET", "/v1/github", "", nil); code != 200 || !strings.Contains(body, `"shared":true`) ||
		strings.Contains(body, "webhook_url") || !strings.Contains(body, "/apps/xu526-test/installations/new") {
		t.Fatalf("status: %d %s", code, body)
	}
	key, err := os.ReadFile(filepath.Join(e.dir, "github.key"))
	if err != nil || len(strings.TrimSpace(string(key))) != 64 {
		t.Fatalf("no local sealing key: %v", err)
	}
	if st, _ := os.Stat(filepath.Join(e.dir, "github.key")); st.Mode().Perm() != 0o600 {
		t.Fatalf("github.key mode %v", st.Mode().Perm())
	}
	tok := e.login(t)
	if code, body := e.do(t, "GET", "/v1/github/repos/101/branches", tok, nil); code != 200 || !strings.Contains(body, `"main"`) {
		t.Fatalf("branches: %d %s", code, body)
	}
	if code, _ := e.do(t, "GET", "/v1/github/repos/102/branches", tok, nil); code == 200 {
		t.Fatal("branches of a repository alice can't see")
	}
	// Connected the way the dashboard does it: a name, a section, no port.
	code, body := e.do(t, "POST", "/v1/sources/github", tok, map[string]any{"name": "api", "app_kind": "api", "kind": "app",
		"installation_id": 7, "repo_id": 101, "path": "."})
	if code != 200 {
		t.Fatalf("connect: %d %s", code, body)
	}
	var src Source
	json.Unmarshal([]byte(body), &src)
	if src.AutoDeploy {
		t.Fatal("auto-deploy should be off by default (ask first)")
	}
	if src.Target != "api" || src.App == nil || src.App.Kind != "api" || src.App.Port != 0 {
		t.Fatalf("target/spec: %s %+v", src.Target, src.App)
	}
	// A monolith in another environment.
	code, body = e.do(t, "POST", "/v1/sources/github", tok, map[string]any{"name": "shop", "kind": "stack", "environment": "staging",
		"installation_id": 7, "repo_id": 101, "path": "docker-compose.yml"})
	var st Source
	json.Unmarshal([]byte(body), &st)
	if code != 200 || st.Target != "shop-staging" || st.StackName != "shop" || st.Environment != "staging" {
		t.Fatalf("stack source: %d %s", code, body)
	}
	for i := 0; i < 500 && e.p.source(src.ID).Building; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	first := gitOut(t, e.work, "rev-parse", "HEAD")
	e.p.setSource(src.ID, func(x *Source) { x.LastCommit = first })

	// Nothing new yet.
	if s, err := e.p.CheckSource(context.Background(), src.ID); err != nil || s.Pending != nil {
		t.Fatalf("first check: %v %+v", err, s.Pending)
	}
	// A push to GitHub: the next check finds it, with its message and author.
	os.WriteFile(filepath.Join(e.work, "README.md"), []byte("hi\n"), 0o644)
	run(t, e.work, "add", ".")
	run(t, e.work, "commit", "-qm", "add a readme\n\nlonger body")
	run(t, e.work, "push", "-q", e.bare, "main")
	second := gitOut(t, e.work, "rev-parse", "HEAD")

	// The user token expires (8h) and is renewed without a client secret.
	e.gh.ExpireUserTokens()
	e.adv(9 * time.Hour)
	s, err := e.p.CheckSource(context.Background(), src.ID)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if s.Pending == nil || s.Pending.SHA != second || s.Pending.Message != "add a readme\n\nlonger body" || s.Pending.Author != "Dev" {
		t.Fatalf("pending: %+v", s.Pending)
	}
	if s.CheckError != "" || s.CheckedAt.IsZero() {
		t.Fatalf("check state: %q %v", s.CheckError, s.CheckedAt)
	}
	for _, r := range e.p.Releases("app", "api") {
		if r.Commit == second {
			t.Fatal("ask first: the new commit was deployed without asking")
		}
	}
	if code, body := e.do(t, "GET", "/v1/sources", tok, nil); !strings.Contains(body, `"pending":{"sha":"`+second) {
		t.Fatalf("sources list doesn't show the pending commit: %d %s", code, body)
	}
	// Deployed (here: recorded as deployed): the pending commit clears.
	e.p.setSource(src.ID, func(x *Source) { x.LastCommit = second })
	if s, _ := e.p.CheckSource(context.Background(), src.ID); s.Pending != nil {
		t.Fatalf("pending after deploy: %+v", s.Pending)
	}
	// A user who loses access gets a clear error, logged once.
	e.gh.Lock()
	e.gh.Users[1].Access = nil
	e.gh.Unlock()
	if _, err := e.p.CheckSource(context.Background(), src.ID); err == nil {
		t.Fatal("check succeeded without access")
	}
}

// A repository's compose file can't reach this computer's files or keys
// through build: settings.
func TestRepoBuildPlanDropsHostAccess(t *testing.T) {
	plan := &StackPlan{BuildSecrets: map[string]any{"k": map[string]any{"file": "/root/.ssh/id_rsa"}},
		Services: []PlanService{{Name: "web", Build: &PlanBuild{Context: ".", Extra: map[string]any{
			"secrets": []any{"k"}, "ssh": []any{"default"}, "additional_contexts": map[string]any{"h": "/etc"}, "network": "host", "labels": map[string]any{"a": "b"}}}}}}
	var log bytes.Buffer
	out := repoBuildPlan(plan, &log)
	if out.BuildSecrets != nil || len(out.Services[0].Build.Extra) != 1 || out.Services[0].Build.Extra["labels"] == nil {
		t.Fatalf("kept: %+v %+v", out.BuildSecrets, out.Services[0].Build.Extra)
	}
	if len(plan.Services[0].Build.Extra) != 5 {
		t.Fatal("the original plan was changed")
	}
	if !strings.Contains(log.String(), "additional_contexts, network, secrets, ssh") {
		t.Fatalf("log: %s", log.String())
	}
	file, _, err := ComposeBuildFile(out, "/repo", "t", []string{"web"})
	if err != nil || strings.Contains(string(file), "id_rsa") || strings.Contains(string(file), "ssh") {
		t.Fatalf("compose build file:\n%s %v", file, err)
	}
}

// Connect GitHub on a computer with no app yet: the dashboard sends the
// browser to GitHub to create this computer's own app, install it on the
// chosen repositories and authorize it, and back, signed in, with no
// restart. The saved app is turned on again at the next start.
func TestConnectGitHubCreatesOwnApp(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	gh := ghtest.New()
	t.Cleanup(gh.Close)
	gh.Repos[101] = &ghtest.Repo{ID: 101, FullName: "acme/api", Private: true}
	gh.Installations[7] = &ghtest.Installation{ID: 7, Account: "acme", AccountID: 900, Selection: "selected", Repos: []int64{101}}
	gh.Users[1] = &ghtest.User{ID: 1, Login: "alice", Access: []int64{101}}
	gh.AuthAs = 1
	t.Setenv(ghapp.EnvWebURL, gh.URL)
	t.Setenv(ghapp.EnvAPIURL, gh.URL)

	dir := t.TempDir()
	p, err := New(Config{Domain: "example.test", StatePath: filepath.Join(dir, "apps.json"), Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(p.API())
	t.Cleanup(api.Close)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	get := func(path string) (int, string) {
		resp, err := c.Get(api.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if _, body := get("/v1/github"); !strings.Contains(body, `"setup_url":"/github/manifest"`) || !strings.Contains(body, `"configured":false`) {
		t.Fatalf("before setup: %s", body)
	}
	// The setup page posts the manifest to GitHub ("Continue on GitHub").
	_, page := get("/github/manifest?next=" + url.QueryEscape("/?gh=repos"))
	action := page[strings.Index(page, `action="`)+8:]
	action = html.UnescapeString(action[:strings.Index(action, `"`)])
	manifest := page[strings.Index(page, `name="manifest" value="`)+23:]
	manifest = html.UnescapeString(manifest[:strings.Index(manifest, `"`)])
	if !strings.Contains(manifest, `"callback_urls":["`+api.URL+`/github/callback"]`) {
		t.Fatalf("manifest: %s", manifest)
	}
	// GitHub creates the app → xu526 turns it on → install → authorize →
	// callback → the dashboard, signed in.
	resp, err := c.PostForm(action, url.Values{"manifest": {manifest}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "/?gh=repos") {
		t.Fatalf("end of the flow: %d %s %s", resp.StatusCode, resp.Request.URL, body)
	}
	if p.GitHub() == nil {
		t.Fatal("the app wasn't turned on")
	}
	if _, body := get("/v1/github"); !strings.Contains(body, `"login":"alice"`) || !strings.Contains(body, `"browser":true`) {
		t.Fatalf("after setup: %s", body)
	}
	if code, body := get("/v1/github/installations"); code != 200 || !strings.Contains(body, "acme/api") {
		t.Fatalf("installations: %d %s", code, body)
	}
	for _, f := range []string{"github.env", "github-app.pem", "github-client-secret"} {
		if st, err := os.Stat(filepath.Join(dir, "github", f)); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v", f, err)
		}
	}
	p.Close()

	// Next start: the saved app is on again.
	p2, err := New(Config{Domain: "example.test", StatePath: filepath.Join(dir, "apps.json"), Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p2.Close)
	if ok, err := p2.EnableSavedGitHub(); !ok || err != nil {
		t.Fatalf("saved app: %v %v", ok, err)
	}
}
