package ghapp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"xu526/internal/ghapp/ghtest"
)

type recorder struct {
	mu      sync.Mutex
	pushes  []PushEvent
	gone    map[int64]string
	back    []int64
	removed map[int64][]int64
}

func (r *recorder) Push(ev PushEvent) { r.mu.Lock(); r.pushes = append(r.pushes, ev); r.mu.Unlock() }
func (r *recorder) InstallationGone(id int64, why string) {
	r.mu.Lock()
	r.gone[id] = why
	r.mu.Unlock()
}
func (r *recorder) InstallationBack(id int64) {
	r.mu.Lock()
	r.back = append(r.back, id)
	r.mu.Unlock()
}
func (r *recorder) ReposRemoved(id int64, repos []int64) {
	r.mu.Lock()
	r.removed[id] = repos
	r.mu.Unlock()
}

type env struct {
	t    *testing.T
	gh   *ghtest.Fake
	svc  *Service
	rec  *recorder
	api  *httptest.Server // private API + browser routes behind Middleware
	dir  string
	now  time.Time
	nowM sync.Mutex
}

func (e *env) clock() time.Time { e.nowM.Lock(); defer e.nowM.Unlock(); return e.now }
func (e *env) advance(d time.Duration) {
	e.nowM.Lock()
	e.now = e.now.Add(d)
	e.nowM.Unlock()
}

// setup: repo 101 (acme/api) and 102 (acme/secret) in installation 7;
// alice can see 101 only; bob isn't on the allowlist.
func setup(t *testing.T) *env {
	gh := ghtest.New()
	t.Cleanup(gh.Close)
	gh.Repos[101] = &ghtest.Repo{ID: 101, FullName: "acme/api", Private: true}
	gh.Repos[102] = &ghtest.Repo{ID: 102, FullName: "acme/secret", Private: true}
	gh.Installations[7] = &ghtest.Installation{ID: 7, Account: "org-acme", AccountID: 900, Selection: "selected", Repos: []int64{101, 102}}
	gh.Users[1] = &ghtest.User{ID: 1, Login: "alice", Access: []int64{101}}
	gh.Users[2] = &ghtest.User{ID: 2, Login: "bob", Access: []int64{101, 102}}
	gh.AuthAs = 1
	e := &env{t: t, gh: gh, dir: t.TempDir(), now: time.Now()}
	cfg := &Config{AppID: gh.AppID, ClientID: gh.ClientID, Slug: "xu526-test", ClientSecret: gh.ClientSecret,
		WebhookSecret: "whsec", Keys: nil, SecretKey: bytes.Repeat([]byte{7}, 32),
		WebURL: gh.URL, APIURL: gh.URL, Users: []string{"alice"}}
	k, err := ParsePrivateKey(gh.PEM())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Keys = append(cfg.Keys, k)
	e.rec = &recorder{gone: map[int64]string{}, removed: map[int64][]int64{}}
	svc, err := New(cfg, e.dir, e.rec, log.New(io.Discard, "", 0), nil)
	if err != nil {
		t.Fatal(err)
	}
	svc.now = e.clock
	gh.SetClock(e.clock)
	e.svc = svc
	mux := http.NewServeMux()
	svc.BrowserRoutes(mux)
	svc.APIRoutes(mux)
	mux.HandleFunc("POST /v1/thing", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "changed by "+UserFrom(r.Context()).Login)
	})
	mux.HandleFunc("GET /v1/thing", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "read by "+UserFrom(r.Context()).Login)
	})
	exempt := func(r *http.Request) bool {
		return strings.HasPrefix(r.URL.Path, "/github/") || r.URL.Path == "/v1/auth/device" || r.URL.Path == "/v1/auth/device/poll"
	}
	e.api = httptest.NewServer(svc.Middleware(mux, exempt))
	t.Cleanup(e.api.Close)
	cfg.CallbackURL = e.api.URL + "/github/callback"
	return e
}

func (e *env) auditLines() []map[string]any {
	b, _ := os.ReadFile(filepath.Join(e.dir, "audit.log"))
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		json.Unmarshal([]byte(l), &m)
		out = append(out, m)
	}
	return out
}

func (e *env) auditHas(action, key string, val any) bool {
	for _, l := range e.auditLines() {
		if l["action"] == action && (key == "" || l[key] == val) {
			return true
		}
	}
	return false
}

// deviceLogin signs in as uid through the device flow and returns the session token.
func (e *env) deviceLogin(uid int64) (string, int) {
	var start struct{ ID, UserCode string }
	resp, err := http.Post(e.api.URL+"/v1/auth/device", "application/json", nil)
	if err != nil {
		e.t.Fatal(err)
	}
	var raw map[string]any
	json.NewDecoder(resp.Body).Decode(&raw)
	resp.Body.Close()
	start.ID, start.UserCode = raw["id"].(string), raw["user_code"].(string)
	poll := func() (int, map[string]any) {
		b, _ := json.Marshal(map[string]string{"id": start.ID})
		resp, err := http.Post(e.api.URL+"/v1/auth/device/poll", "application/json", bytes.NewReader(b))
		if err != nil {
			e.t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}
	if _, m := poll(); m["status"] != "pending" {
		e.t.Fatalf("before approval: %v", m)
	}
	e.gh.ApproveDevice(start.UserCode, uid)
	if _, m := poll(); m["status"] != "pending" {
		e.t.Fatalf("polling faster than the interval should stay pending locally: %v", m)
	}
	e.advance(6 * time.Second)
	code, m := poll()
	tok, _ := m["token"].(string)
	return tok, code
}

func get(t *testing.T, c *http.Client, u, token string) (int, string) {
	req, _ := http.NewRequest("GET", u, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestReadSecret(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "s")
	os.WriteFile(p, []byte("  shh\n"), 0o644)
	t.Setenv("XU_T_SECRET_FILE", p)
	if _, err := readSecret(os.Getenv, "XU_T_SECRET"); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("world-readable secret accepted: %v", err)
	}
	os.Chmod(p, 0o600)
	if v, err := readSecret(os.Getenv, "XU_T_SECRET"); err != nil || v != "shh" {
		t.Fatalf("%q %v", v, err)
	}
	os.Unsetenv("XU_T_SECRET_FILE")
	t.Setenv("XU_T_INLINE", "value")
	if v, _ := readSecret(os.Getenv, "XU_T_INLINE"); v != "value" || os.Getenv("XU_T_INLINE") != "" {
		t.Fatal("inline secrets must be removed from the environment so children don't inherit them")
	}
	if k, err := parseKey(strings.Repeat("ab", 32)); err != nil || len(k) != 32 {
		t.Fatal(err)
	}
	if _, err := parseKey("short"); err == nil {
		t.Fatal("short key accepted")
	}
	c := &Config{AppID: 1, ClientID: "c", Slug: "s", ClientSecret: "x", WebhookSecret: "w", SecretKey: make([]byte, 32), CallbackURL: "http://x"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), EnvPrivateKey) {
		t.Fatalf("missing key: %v", err)
	}
}

func TestSeal(t *testing.T) {
	key := bytes.Repeat([]byte{1}, 32)
	s, err := seal(key, "ghr_refresh", "u_1")
	if err != nil || strings.Contains(string(s), "ghr_refresh") {
		t.Fatal("sealed value leaks the plaintext")
	}
	if pt, err := s.open(key, "u_1"); err != nil || pt != "ghr_refresh" {
		t.Fatalf("%q %v", pt, err)
	}
	if _, err := s.open(key, "u_2"); err == nil {
		t.Fatal("a sealed token moved to another user must not open")
	}
	if _, err := s.open(bytes.Repeat([]byte{2}, 32), "u_1"); err == nil {
		t.Fatal("wrong key opened it")
	}
}

func TestAuditChain(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.log")
	a, _ := OpenAudit(p)
	a.Log("token.mint", map[string]any{"installation": 1})
	a.Log("repo.clone", map[string]any{"repo": 2})
	b, _ := OpenAudit(p) // reopening continues the chain
	b.Log("token.mint", map[string]any{"installation": 3})
	if n, err := VerifyAudit(p); err != nil || n != 3 {
		t.Fatalf("%d %v", n, err)
	}
	if info, _ := os.Stat(p); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	data, _ := os.ReadFile(p)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	os.WriteFile(p, []byte(strings.Replace(string(data), `"repo":2`, `"repo":9`, 1)), 0o600)
	if _, err := VerifyAudit(p); err == nil {
		t.Fatal("edited record not detected")
	}
	os.WriteFile(p, []byte(lines[0]+"\n"+lines[2]+"\n"), 0o600)
	if _, err := VerifyAudit(p); err == nil {
		t.Fatal("deleted record not detected")
	}
}

func TestInstallationTokens(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	acc, err := e.svc.Clone(ctx, 7, 101, "system:build")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Repo.FullName != "acme/api" {
		t.Fatalf("%+v", acc.Repo)
	}
	if len(e.gh.Mints) != 1 || len(e.gh.Mints[0].Repos) != 1 || e.gh.Mints[0].Repos[0] != 101 || e.gh.Mints[0].Permissions["contents"] != "read" || len(e.gh.Mints[0].Permissions) != 1 {
		t.Fatalf("token not narrowed to one repo + contents:read: %+v", e.gh.Mints)
	}
	// Cached: a second clone within the hour mints nothing.
	if acc2, _ := e.svc.Clone(ctx, 7, 101, "system:build"); acc2.Token != acc.Token || len(e.gh.Mints) != 1 {
		t.Fatal("token should be reused from memory")
	}
	// A different repo never reuses that token.
	if acc3, _ := e.svc.Clone(ctx, 7, 102, "system:build"); acc3.Token == acc.Token {
		t.Fatal("token for repo 101 handed out for 102")
	}
	// Five minutes before expiry it is replaced.
	e.advance(56 * time.Minute)
	acc4, err := e.svc.Clone(ctx, 7, 101, "system:build")
	if err != nil {
		t.Fatal(err)
	}
	if acc4.Token == acc.Token {
		t.Fatal("token should be re-minted near expiry")
	}
	// Audit has mints with fingerprints; the token itself never appears.
	logText, _ := os.ReadFile(filepath.Join(e.dir, "audit.log"))
	if strings.Contains(string(logText), acc.Token) || !e.auditHas("token.mint", "token_fp", fingerprint(acc.Token)) {
		t.Fatal("audit must hold the fingerprint, never the token")
	}
	for _, f := range []string{"github.json"} {
		b, _ := os.ReadFile(filepath.Join(e.dir, f))
		if strings.Contains(string(b), acc.Token) {
			t.Fatal("installation token persisted")
		}
	}
	// A token wider than requested is refused.
	e.gh.GrantExtra = map[string]string{"contents": "write"}
	e.svc.Evict(7, nil, "test")
	if _, err := e.svc.Clone(ctx, 7, 101, "system:build"); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("over-broad token accepted: %v", err)
	}
	if !e.auditHas("token.mint", "result", "error") {
		t.Fatal("failed mint not audited")
	}
	// A JWT signed with the wrong key is rejected; the fallback key is used.
	e.gh.GrantExtra = nil
	other, _ := ParsePrivateKey(ghtest.New().PEM())
	e.svc.cfg.Keys = append([]*rsaKey{other}, e.svc.cfg.Keys...)
	e.svc.Evict(7, nil, "test")
	if _, err := e.svc.Clone(ctx, 7, 101, "system:build"); err != nil {
		t.Fatalf("rotation fallback key not used: %v", err)
	}
}

func TestWebhooks(t *testing.T) {
	e := setup(t)
	hook := httptest.NewServer(e.svc.WebhookHandler())
	defer hook.Close()
	wait := func(cond func() bool) {
		for i := 0; i < 100 && !cond(); i++ {
			time.Sleep(10 * time.Millisecond)
		}
	}
	// Bad signature.
	resp, _ := e.gh.Deliver(hook.URL, "wrong-secret", "push", map[string]any{})
	if resp.StatusCode != 401 || !e.auditHas("webhook.rejected", "reason", "bad signature") {
		t.Fatalf("bad signature: %d", resp.StatusCode)
	}
	// Replay of the same delivery.
	body := []byte(`{"zen":"hi"}`)
	req := func() *http.Response {
		r, _ := http.NewRequest("POST", hook.URL, bytes.NewReader(body))
		r.Header.Set("X-GitHub-Event", "ping")
		r.Header.Set("X-GitHub-Delivery", "same-guid")
		r.Header.Set("X-Hub-Signature-256", sign("whsec", body))
		resp, _ := http.DefaultClient.Do(r)
		return resp
	}
	req()
	if r := req(); r.StatusCode != 200 || !e.auditHas("webhook.rejected", "reason", "replayed delivery") {
		t.Fatal("replay not rejected")
	}
	// installation.created is confirmed against the API, then recorded.
	e.gh.Deliver(hook.URL, "whsec", "installation", map[string]any{"action": "created",
		"installation": map[string]any{"id": 7, "account": map[string]any{"login": "org-acme"}},
		"repositories": []map[string]any{{"id": 101, "full_name": "acme/api"}, {"id": 102, "full_name": "acme/secret"}},
		"sender":       map[string]any{"login": "alice"}})
	wait(func() bool { _, ok := e.svc.Installation(7); return ok })
	in, ok := e.svc.Installation(7)
	if !ok || in.AccountLogin != "org-acme" || in.Selection != "selected" || len(in.Repos) != 2 {
		t.Fatalf("installation: %+v", in)
	}
	// Repository removed: tokens for it are dropped and the platform told.
	e.svc.Clone(context.Background(), 7, 102, "system:build")
	e.gh.Deliver(hook.URL, "whsec", "installation_repositories", map[string]any{"action": "removed",
		"installation":         map[string]any{"id": 7, "repository_selection": "selected"},
		"repositories_removed": []map[string]any{{"id": 102, "full_name": "acme/secret"}}})
	wait(func() bool { e.rec.mu.Lock(); defer e.rec.mu.Unlock(); return len(e.rec.removed[7]) == 1 })
	if r := e.rec.removed[7]; len(r) != 1 || r[0] != 102 || !e.auditHas("token.evict", "reason", "repositories removed") {
		t.Fatalf("removed: %v", e.rec.removed)
	}
	// A forged-looking "deleted" while GitHub still lists the installation is ignored.
	e.gh.Deliver(hook.URL, "whsec", "installation", map[string]any{"action": "deleted", "installation": map[string]any{"id": 7}})
	time.Sleep(100 * time.Millisecond)
	if _, ok := e.svc.Installation(7); !ok || len(e.rec.gone) != 0 {
		t.Fatal("deleted acted on although GitHub still has the installation")
	}
	// Really deleted.
	e.gh.Lock()
	delete(e.gh.Installations, 7)
	e.gh.Unlock()
	e.gh.Deliver(hook.URL, "whsec", "installation", map[string]any{"action": "deleted", "installation": map[string]any{"id": 7, "account": map[string]any{"login": "org-acme"}}})
	wait(func() bool { e.rec.mu.Lock(); defer e.rec.mu.Unlock(); return e.rec.gone[7] != "" })
	if _, ok := e.svc.Installation(7); ok || !e.auditHas("installation.deleted", "", nil) {
		t.Fatal("installation not removed")
	}
	// Push is routed by IDs.
	e.gh.Deliver(hook.URL, "whsec", "push", map[string]any{"installation": map[string]any{"id": 7}, "ref": "refs/heads/main", "after": "abc123",
		"repository": map[string]any{"id": 101, "full_name": "acme/api"}, "head_commit": map[string]any{"message": "fix\n\nbody", "author": map[string]any{"name": "Al"}}})
	wait(func() bool { e.rec.mu.Lock(); defer e.rec.mu.Unlock(); return len(e.rec.pushes) == 1 })
	if p := e.rec.pushes[0]; p.RepoID != 101 || p.Installation != 7 || p.Message != "fix" {
		t.Fatalf("push: %+v", p)
	}
	// Rate limit per installation.
	limited := false
	for i := 0; i < 70 && !limited; i++ {
		r, _ := e.gh.Deliver(hook.URL, "whsec", "ping", map[string]any{"installation": map[string]any{"id": 55}})
		limited = r.StatusCode == 429
	}
	if !limited {
		t.Fatal("no rate limit")
	}
}

func sign(secret string, body []byte) string { return "sha256=" + hmacHex(secret, body) }

func TestBrowserLogin(t *testing.T) {
	e := setup(t)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	code, body := get(t, c, e.api.URL+"/github/login?next=/apps", "")
	if code != 200 || !strings.Contains(body, `url=/apps`) {
		t.Fatalf("login: %d %s", code, body)
	}
	if code, body := get(t, c, e.api.URL+"/v1/thing", ""); code != 200 || body != "read by alice" {
		t.Fatalf("session cookie not working: %d %s", code, body)
	}
	// Cookie sessions need the CSRF header to change anything.
	req, _ := http.NewRequest("POST", e.api.URL+"/v1/thing", nil)
	if resp, _ := c.Do(req); resp.StatusCode != 403 {
		t.Fatalf("cookie POST without %s: %d", CSRFHeader, resp.StatusCode)
	}
	req, _ = http.NewRequest("POST", e.api.URL+"/v1/thing", nil)
	req.Header.Set(CSRFHeader, "1")
	if resp, _ := c.Do(req); resp.StatusCode != 200 {
		t.Fatalf("with header: %d", resp.StatusCode)
	}
	// The installation alice can see was linked from her token.
	if in, ok := e.svc.Installation(7); !ok || in.AccountLogin != "org-acme" {
		t.Fatal("installation not linked at sign-in")
	}
	// No cookie: the callback can't be completed with a stolen state/code.
	noJar := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, _ := noJar.Get(e.api.URL + "/github/login")
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc.Query().Get("code_challenge_method") != "S256" || loc.Query().Get("state") == "" {
		t.Fatalf("authorize URL lacks PKCE/state: %s", loc)
	}
	auth, _ := noJar.Get(loc.String())
	back := auth.Header.Get("Location")
	if code, _ := get(t, noJar, back, ""); code != 400 {
		t.Fatalf("callback without the state cookie: %d", code)
	}
	// Open redirects are refused.
	if safeNext("//evil.example") != "/" || safeNext("https://evil.example") != "/" || safeNext("/apps?x=1") != "/apps?x=1" {
		t.Fatal("safeNext")
	}
	// The setup URL's installation_id is never trusted: it only leads to sign-in.
	resp, _ = noJar.Get(e.api.URL + "/github/setup?installation_id=999&setup_action=install")
	if resp.Header.Get("Location") != "/github/login?next=%2F%3Fgh%3Drepos" {
		t.Fatalf("setup: %s", resp.Header.Get("Location"))
	}
	if _, ok := e.svc.Installation(999); ok {
		t.Fatal("spoofed installation recorded")
	}
	// Someone not on the allowlist is refused and their grant revoked.
	e.gh.AuthAs = 2
	jar2, _ := cookiejar.New(nil)
	if code, body := get(t, &http.Client{Jar: jar2}, e.api.URL+"/github/login", ""); code != 403 || !strings.Contains(body, "isn&#39;t allowed") {
		t.Fatalf("bob: %d %s", code, body)
	}
	if !e.auditHas("user.login", "result", "denied") || len(e.gh.Revoked) != 1 {
		t.Fatal("denied login not audited/revoked")
	}
	// Non-expiring user tokens are refused.
	e.gh.AuthAs, e.gh.NonExpiring = 1, true
	jar3, _ := cookiejar.New(nil)
	if code, body := get(t, &http.Client{Jar: jar3}, e.api.URL+"/github/login", ""); code != 403 || !strings.Contains(body, "Expire user authorization tokens") {
		t.Fatalf("non-expiring: %d %s", code, body)
	}
}

func TestDeviceLoginRefreshAndLogout(t *testing.T) {
	e := setup(t)
	tok, code := e.deviceLogin(1)
	if code != 200 || !strings.HasPrefix(tok, "xus_") {
		t.Fatalf("device login: %d %q", code, tok)
	}
	if code, _ := get(t, http.DefaultClient, e.api.URL+"/v1/thing", ""); code != 401 {
		t.Fatal("API answered without a session")
	}
	if code, body := get(t, http.DefaultClient, e.api.URL+"/v1/auth/whoami", tok); code != 200 || !strings.Contains(body, "alice") {
		t.Fatalf("whoami: %d %s", code, body)
	}
	// Only hashes of sessions and sealed refresh tokens are on disk.
	st, _ := os.ReadFile(filepath.Join(e.dir, "github.json"))
	if strings.Contains(string(st), tok) || strings.Contains(string(st), "ghr_") || strings.Contains(string(st), "ghu_") {
		t.Fatal("a token was written to github.json in the clear")
	}
	// What alice can connect: acme/api, not acme/secret.
	_, body := get(t, http.DefaultClient, e.api.URL+"/v1/github/installations", tok)
	if !strings.Contains(body, "acme/api") || strings.Contains(body, "acme/secret") {
		t.Fatalf("installations: %s", body)
	}
	uid := ""
	for id := range e.svc.st.Users {
		uid = id
	}
	if _, err := e.svc.UserCanAccess(context.Background(), uid, 7, 102); err == nil {
		t.Fatal("alice can't see acme/secret, so she must not connect it")
	}
	if r, err := e.svc.UserCanAccess(context.Background(), uid, 7, 101); err != nil || r.FullName != "acme/api" {
		t.Fatal(err)
	}
	// After 8 hours the access token is refreshed and the refresh token rotated.
	e.gh.ExpireUserTokens()
	e.advance(9 * time.Hour)
	before := e.svc.st.Users[uid].RefreshToken
	if _, err := e.svc.UserCanAccess(context.Background(), uid, 7, 101); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if e.svc.st.Users[uid].RefreshToken == before || !e.auditHas("user.refresh", "result", "ok") {
		t.Fatal("refresh token not rotated")
	}
	// A rejected refresh token signs the user out everywhere.
	e.gh.Lock()
	e.gh.Users[1].Login = "alice"
	e.gh.Unlock()
	e.svc.tokMu.Lock()
	delete(e.svc.userTokens, uid)
	e.svc.tokMu.Unlock()
	e.gh.ExpireUserTokens()
	e.svc.mu.Lock()
	e.svc.st.Users[uid].RefreshToken, _ = seal(e.svc.cfg.SecretKey, "ghr_revoked", uid)
	e.svc.mu.Unlock()
	if _, err := e.svc.UserCanAccess(context.Background(), uid, 7, 101); err != ErrSignedOut {
		t.Fatalf("want ErrSignedOut, got %v", err)
	}
	if code, _ := get(t, http.DefaultClient, e.api.URL+"/v1/auth/whoami", tok); code != 401 {
		t.Fatal("session survived a revoked refresh token")
	}
	// Sign in again, then log out everywhere: the GitHub grant is revoked.
	tok, _ = e.deviceLogin(1)
	req, _ := http.NewRequest("POST", e.api.URL+"/v1/auth/logout", strings.NewReader(`{"everywhere":true}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 204 || len(e.gh.Revoked) == 0 {
		t.Fatalf("logout everywhere: %d revoked=%v", resp.StatusCode, e.gh.Revoked)
	}
	if code, _ := get(t, http.DefaultClient, e.api.URL+"/v1/auth/whoami", tok); code != 401 {
		t.Fatal("session survived logout")
	}
}

func TestLoginPinnedToID(t *testing.T) {
	e := setup(t)
	if tok, _ := e.deviceLogin(1); tok == "" {
		t.Fatal("alice should sign in")
	}
	// alice renames; someone else (ID 3) takes the login "alice".
	e.gh.Lock()
	e.gh.Users[3] = &ghtest.User{ID: 3, Login: "alice"}
	e.gh.Unlock()
	if tok, code := e.deviceLogin(3); tok != "" || code != 403 {
		t.Fatalf("a different account with the same login signed in: %d", code)
	}
}

func TestGitEnvKeepsTokenOutOfArgs(t *testing.T) {
	e := setup(t)
	env := e.svc.GitEnv("ghs_secret")
	if len(env) != 3 || !strings.HasPrefix(env[1], "GIT_CONFIG_KEY_0=http."+e.gh.URL+"/.extraheader") {
		t.Fatalf("%v", env)
	}
	if e.svc.CloneURL("acme/api") != e.gh.URL+"/acme/api.git" {
		t.Fatal(e.svc.CloneURL("acme/api"))
	}
}

type rsaKey = rsa.PrivateKey

func hmacHex(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func TestNoAllowlistMeansAnyone(t *testing.T) {
	e := setup(t)
	e.svc.cfg.Users = nil // anyone who can reach the API may link their account
	if tok, code := e.deviceLogin(2); tok == "" || code != 200 {
		t.Fatalf("bob without an allowlist: %d", code)
	}
}
