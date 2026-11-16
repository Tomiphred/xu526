package ghapp

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"crypto/rand"
)

// ManifestConfig describes the GitHub App that the one-click manifest flow
// creates. It is used only while no app is configured.
type ManifestConfig struct {
	SecretsDir string // where the app's secrets are written (0600 files)
	WebURL     string // https://github.com
	APIURL     string // https://api.github.com
	Name       string // app name shown on GitHub
	// DashboardURL is where GitHub sends people back. Empty: the address
	// the dashboard was opened at (http://127.0.0.1:7070), so the sign-in
	// cookie set before leaving is there on return.
	DashboardURL string
	WebhookURL   string // public: http(s)://xu.<domain>/github/webhook
	HTTP         *http.Client
	// Configured reports whether an app is set up already (the setup page
	// then just moves on). OnCreated turns the new app on at once, from the
	// env file it was written to, so no restart is needed.
	Configured func() bool
	OnCreated  func(envFile string) error
}

// manifest is what GitHub needs to create the app: the minimum
// permissions (contents and metadata, read only) and no webhook at all.
// xu526 usually runs where GitHub can't reach it (GitHub refuses a hook
// URL it can't reach, even an inactive one), so it checks for new commits
// itself every 30 seconds.
func (mc *ManifestConfig) manifest(dash string) map[string]any {
	return map[string]any{
		"name": mc.Name, "url": "https://xu526.com", "public": false, // the homepage GitHub shows; must be a public address
		"redirect_url":             dash + "/github/manifest/callback",
		"callback_urls":            []string{dash + "/github/callback"},
		"setup_url":                dash + "/github/setup",
		"setup_on_update":          true,
		"request_oauth_on_install": true,
		"default_permissions":      map[string]string{"contents": "read", "metadata": "read"},
		"default_events":           []string{},
		"description":              "Deploys your repositories with xu526. Reads code only.",
	}
}

// ManifestRoutes serves GET /github/manifest (start) and
// GET /github/manifest/callback (GitHub returns the new app's credentials,
// which are written to SecretsDir and never kept in xud's state).
func ManifestRoutes(mux *http.ServeMux, mc *ManifestConfig) {
	type pending struct {
		expires time.Time
		next    string
		dash    string
	}
	dashOf := func(r *http.Request) string {
		if mc.DashboardURL != "" {
			return strings.TrimRight(mc.DashboardURL, "/")
		}
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		return scheme + "://" + r.Host
	}
	var mu sync.Mutex
	states := map[string]pending{}
	mux.HandleFunc("GET /github/manifest", func(w http.ResponseWriter, r *http.Request) {
		next := safeNext(r.URL.Query().Get("next"))
		if mc.Configured != nil && mc.Configured() {
			http.Redirect(w, r, "/github/login?next="+url.QueryEscape(next), http.StatusFound)
			return
		}
		if mc.SecretsDir == "" {
			page(w, http.StatusNotFound, "GitHub App setup is off", "Start the xu526 daemon with -github-secrets-dir to create the GitHub App from here.")
			return
		}
		st := randomToken(24)
		mu.Lock()
		dash := dashOf(r)
		states[st] = pending{time.Now().Add(flowTTL), next, dash}
		mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: st, Path: "/github/", MaxAge: int(flowTTL.Seconds()), HttpOnly: true, SameSite: http.SameSiteLaxMode})
		m, _ := json.Marshal(mc.manifest(dash))
		target := mc.WebURL + "/settings/apps/new"
		if org := r.URL.Query().Get("org"); org != "" {
			target = mc.WebURL + "/organizations/" + url.PathEscape(org) + "/settings/apps/new"
		}
		if r.URL.Query().Get("format") == "json" {
			// The dashboard posts the form itself, skipping this page.
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"action": target + "?state=" + url.QueryEscape(st), "manifest": string(m)})
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self' "+mc.WebURL)
		fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>Create the GitHub App</title>
<body style="font:16px/1.5 system-ui;max-width:38rem;margin:12vh auto;padding:0 1rem">
<h1 style="font-size:1.4rem">Connect xu526 to GitHub</h1>
<p>This is needed once. GitHub creates a private app on your account for this xu526. It can only <b>read</b> code (Contents and Metadata, read-only), and only from the repositories you pick.</p>
<ol><li>On GitHub, click <b>Create GitHub App</b>.</li><li>Choose the repositories it may read, then <b>Install &amp; Authorize</b>.</li><li>You come back here, signed in, to pick the repository to deploy.</li></ol>
<p style="color:#555;font-size:14px">The app's keys are saved on this computer in <code>%s</code> (readable only by xu526).</p>
<form method="post" action="%s?state=%s"><input type="hidden" name="manifest" value="%s"><button type="submit" style="font:inherit;padding:8px 16px;cursor:pointer">Continue on GitHub</button></form>`,
			html.EscapeString(mc.SecretsDir), html.EscapeString(target), url.QueryEscape(st), html.EscapeString(string(m)))
	})
	mux.HandleFunc("GET /github/manifest/callback", func(w http.ResponseWriter, r *http.Request) {
		st := r.URL.Query().Get("state")
		c, err := r.Cookie(stateCookie)
		mu.Lock()
		pend, ok := states[st]
		delete(states, st)
		mu.Unlock()
		if err != nil || c.Value != st || !ok || time.Now().After(pend.expires) {
			page(w, http.StatusBadRequest, "Setup couldn't be verified", "This link didn't start from this browser, or it expired. Start again at /github/manifest.")
			return
		}
		client := mc.HTTP
		if client == nil {
			client = &http.Client{Timeout: 30 * time.Second}
		}
		req, _ := http.NewRequestWithContext(r.Context(), "POST", mc.APIURL+"/app-manifests/"+url.PathEscape(r.URL.Query().Get("code"))+"/conversions", nil)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "xu526")
		resp, err := client.Do(req)
		if err != nil {
			page(w, http.StatusBadGateway, "GitHub didn't answer", err.Error())
			return
		}
		defer resp.Body.Close()
		var app struct {
			ID            int64  `json:"id"`
			Slug          string `json:"slug"`
			ClientID      string `json:"client_id"`
			ClientSecret  string `json:"client_secret"`
			WebhookSecret string `json:"webhook_secret"`
			PEM           string `json:"pem"`
			HTMLURL       string `json:"html_url"`
		}
		if resp.StatusCode >= 300 || json.NewDecoder(resp.Body).Decode(&app) != nil || app.PEM == "" {
			page(w, http.StatusBadGateway, "GitHub didn't create the app", resp.Status)
			return
		}
		envFile, err := writeAppSecrets(mc, pend.dash+"/github/callback", &app.ID, app.Slug, app.ClientID, app.ClientSecret, app.WebhookSecret, app.PEM)
		if err != nil {
			page(w, http.StatusInternalServerError, "Couldn't save the app's secrets", err.Error())
			return
		}
		if mc.OnCreated != nil {
			if err := mc.OnCreated(envFile); err == nil {
				// Straight on to installing it: the user picks repositories,
				// authorizes, and lands back in the dashboard signed in.
				http.Redirect(w, r, "/github/install?next="+url.QueryEscape(pend.next), http.StatusFound)
				return
			} else {
				page(w, http.StatusInternalServerError, "The GitHub App was created but couldn't be turned on", err.Error()+". Restart xu526 to try again.")
				return
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
		fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>GitHub App created</title>
<body style="font:16px/1.5 system-ui;max-width:40rem;margin:10vh auto;padding:0 1rem">
<h1 style="font-size:1.4rem">GitHub App %s created</h1>
<p>Its secrets are in <code>%s</code> (readable only by xud's user). Two steps left:</p>
<ol><li>On <a href="%s">the app's settings page</a>, turn on <b>Enable Device Flow</b> (for <code>xu login</code>) and check that <b>Expire user authorization tokens</b> is on.</li>
<li>Restart xud with <code>%s</code> as its environment (systemd: <code>EnvironmentFile=%s</code>). Optionally list the GitHub logins that may link their account in <code>XU_GITHUB_USERS</code>.</li></ol>
<p>Move the secret files into your secret manager if you use one; the <code>*_FILE</code> variables can point anywhere.</p>`,
			html.EscapeString(app.Slug), html.EscapeString(mc.SecretsDir), html.EscapeString(app.HTMLURL), html.EscapeString(envFile), html.EscapeString(envFile))
	})
}

// writeAppSecrets writes each secret to its own 0600 file (refusing to
// overwrite) and an env file that points at them.
func writeAppSecrets(mc *ManifestConfig, callback string, id *int64, slug, clientID, clientSecret, webhookSecret, pemText string) (string, error) {
	if err := os.MkdirAll(mc.SecretsDir, 0o700); err != nil {
		return "", err
	}
	key := make([]byte, 32)
	rand.Read(key)
	files := map[string]string{
		"github-app.pem": pemText, "github-client-secret": clientSecret, "xu-secret-key": hex.EncodeToString(key),
	}
	if webhookSecret != "" {
		files["github-webhook-secret"] = webhookSecret
	}
	for name, body := range files {
		f, err := os.OpenFile(filepath.Join(mc.SecretsDir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return "", fmt.Errorf("%s: %w (remove the old app's files first)", name, err)
		}
		f.WriteString(body)
		f.Close()
	}
	envFile := filepath.Join(mc.SecretsDir, "github.env")
	lines := []string{
		fmt.Sprintf("%s=%d", EnvAppID, *id), EnvClientID + "=" + clientID, EnvSlug + "=" + slug,
		EnvPrivateKey + "_FILE=" + filepath.Join(mc.SecretsDir, "github-app.pem"),
		EnvClientSecret + "_FILE=" + filepath.Join(mc.SecretsDir, "github-client-secret"),
		EnvSecretKey + "_FILE=" + filepath.Join(mc.SecretsDir, "xu-secret-key"),
		EnvCallbackURL + "=" + callback,
		EnvUsers + "=",
	}
	if webhookSecret != "" {
		lines = append(lines, EnvWebhookSecret+"_FILE="+filepath.Join(mc.SecretsDir, "github-webhook-secret"))
	}
	if mc.WebURL != "https://github.com" {
		lines = append(lines, EnvWebURL+"="+mc.WebURL, EnvAPIURL+"="+mc.APIURL)
	}
	if err := os.WriteFile(envFile, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return "", err
	}
	return envFile, nil
}
