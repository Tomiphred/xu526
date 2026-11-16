package ghapp

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xu526/internal/ghapp/ghtest"
)

func TestManifestFlow(t *testing.T) {
	gh := ghtest.New()
	defer gh.Close()
	dir := filepath.Join(t.TempDir(), "secrets")
	mc := &ManifestConfig{SecretsDir: dir, WebURL: gh.URL, APIURL: gh.URL, Name: "xu526-test",
		WebhookURL: "http://xu.example.test/github/webhook"}
	mux := http.NewServeMux()
	ManifestRoutes(mux, mc)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	resp, _ := c.Get(srv.URL + "/github/manifest")
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	html := string(page)
	if !strings.Contains(html, gh.URL+"/settings/apps/new?state=") || !strings.Contains(html, "&#34;contents&#34;:&#34;read&#34;") || strings.Contains(html, "write") ||
		strings.Contains(html, "hook_attributes") || !strings.Contains(html, "&#34;callback_urls&#34;:[&#34;"+srv.URL+"/github/callback&#34;]") {
		t.Fatalf("manifest page: %s", html)
	}
	state := html[strings.Index(html, "state=")+6:]
	state = state[:strings.Index(state, `"`)]
	resp, _ = c.Get(srv.URL + "/github/manifest/callback?code=abc&state=" + state)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "Enable Device Flow") {
		t.Fatalf("callback: %d %s", resp.StatusCode, body)
	}
	for _, f := range []string{"github-app.pem", "github-client-secret", "github-webhook-secret", "xu-secret-key", "github.env"} {
		info, err := os.Stat(filepath.Join(dir, f))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", f, err, info)
		}
	}
	env, _ := os.ReadFile(filepath.Join(dir, "github.env"))
	if strings.Contains(string(env), "BEGIN RSA") || !strings.Contains(string(env), "XU_GITHUB_PRIVATE_KEY_FILE=") ||
		!strings.Contains(string(env), "XU_GITHUB_CALLBACK_URL="+srv.URL+"/github/callback") {
		t.Fatalf("env file should point at the key, not contain it:\n%s", env)
	}
	if _, err := ParsePrivateKey(must(os.ReadFile(filepath.Join(dir, "github-app.pem")))); err != nil {
		t.Fatal(err)
	}
	// State is single-use; a second callback is refused, and existing secrets are never overwritten.
	resp, _ = c.Get(srv.URL + "/github/manifest/callback?code=abc&state=" + state)
	if resp.StatusCode != 400 {
		t.Fatalf("replayed manifest callback: %d", resp.StatusCode)
	}
	if cfg, err := LoadConfigFile(filepath.Join(dir, "github.env")); err != nil || cfg.AppID != gh.AppID || len(cfg.Keys) != 1 {
		t.Fatalf("load saved app: %v", err)
	}
	if _, err := writeAppSecrets(mc, "x", new(int64), "s", "c", "x", "w", "pem"); err == nil {
		t.Fatal("existing secrets overwritten")
	}
}

// With OnCreated (the dashboard), the new app is turned on at once and the
// browser goes straight on to installing it.
func TestManifestFlowTurnsOn(t *testing.T) {
	gh := ghtest.New()
	defer gh.Close()
	dir := filepath.Join(t.TempDir(), "secrets")
	var got string
	on := false
	mc := &ManifestConfig{SecretsDir: dir, WebURL: gh.URL, APIURL: gh.URL, Name: "xu526-test", WebhookURL: "http://x/github/webhook",
		Configured: func() bool { return on }, OnCreated: func(env string) error { got, on = env, true; return nil }}
	mux := http.NewServeMux()
	ManifestRoutes(mux, mc)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, _ := c.Get(srv.URL + "/github/manifest?next=" + "/%3Fgh%3Drepos")
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	state := string(page)[strings.Index(string(page), "state=")+6:]
	state = state[:strings.Index(state, `"`)]
	resp, _ = c.Get(srv.URL + "/github/manifest/callback?code=abc&state=" + state)
	if resp.StatusCode != 302 || resp.Header.Get("Location") != "/github/install?next=%2F%3Fgh%3Drepos" || got != filepath.Join(dir, "github.env") {
		t.Fatalf("callback: %d %q %q", resp.StatusCode, resp.Header.Get("Location"), got)
	}
	// Set up already: the setup page moves on to signing in.
	resp, _ = c.Get(srv.URL + "/github/manifest?next=/x")
	if resp.StatusCode != 302 || !strings.HasPrefix(resp.Header.Get("Location"), "/github/login?next=") {
		t.Fatalf("after setup: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}
