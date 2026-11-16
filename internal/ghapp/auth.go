package ghapp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Session lifetimes.
const (
	cliSessionTTL = 30 * 24 * time.Hour
	webSessionTTL = 12 * time.Hour
	flowTTL       = 10 * time.Minute
)

// Cookie names.
const (
	sessionCookie = "xu_session"
	stateCookie   = "xu_oauth_state"
)

// ErrSignedOut means the user must sign in again.
var ErrSignedOut = errors.New("your GitHub sign-in has expired or was revoked; sign in again (xu login)")

type userToken struct {
	token   string
	expires time.Time
}

type oauthFlow struct {
	state, verifier, purpose, next string
	expires                        time.Time
}

// tokenResponse is GitHub's answer for code, device and refresh grants.
type tokenResponse struct {
	AccessToken           string `json:"access_token"`
	ExpiresIn             int64  `json:"expires_in"`
	RefreshToken          string `json:"refresh_token"`
	RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
	Error                 string `json:"error"`
	ErrorDescription      string `json:"error_description"`
	Interval              int    `json:"interval"`
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// ---- allowlist ----

// allowed checks a GitHub user against XU_GITHUB_USERS. With no list,
// anyone who can reach xud's API may link their GitHub account (they
// already control the server). Logins are pinned to the first numeric ID
// that signs in with them, so a renamed-and-reclaimed login stays locked out.
func (s *Service) allowedLocked(id int64, login string) bool {
	if len(s.cfg.Users) == 0 {
		return true
	}
	l := strings.ToLower(login)
	for _, u := range s.cfg.Users {
		if n, err := strconv.ParseInt(u, 10, 64); err == nil {
			if n == id {
				return true
			}
			continue
		}
		if strings.ToLower(u) != l {
			continue
		}
		if pinned, ok := s.st.Pins[l]; ok && pinned != id {
			return false
		}
		s.st.Pins[l] = id
		return true
	}
	return false
}

// ---- login completion (shared by browser and device flows) ----

func (s *Service) completeLogin(ctx context.Context, tok *tokenResponse, kind string) (*User, string, error) {
	if tok.RefreshToken == "" || tok.ExpiresIn == 0 {
		return nil, "", errors.New(`GitHub returned a non-expiring token: turn on "Expire user authorization tokens" in the GitHub App's settings`)
	}
	var gu struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if err := s.tokenDo(ctx, tok.AccessToken, "GET", "/user", nil, &gu); err != nil {
		return nil, "", err
	}
	s.mu.Lock()
	if !s.allowedLocked(gu.ID, gu.Login) {
		s.mu.Unlock()
		s.audit.Log("user.login", map[string]any{"actor": "github:" + gu.Login, "github_id": gu.ID, "kind": kind, "result": "denied"})
		s.revokeGrant(ctx, tok.AccessToken)
		return nil, "", fmt.Errorf("%s isn't allowed to sign in to this server (ask the operator to add them to %s)", gu.Login, EnvUsers)
	}
	var u *User
	for _, x := range s.st.Users {
		if x.GitHubID == gu.ID {
			u = x
		}
	}
	if u == nil {
		u = &User{ID: "u_" + randomToken(9), GitHubID: gu.ID, CreatedAt: s.now()}
		s.st.Users[u.ID] = u
	}
	u.Login, u.LastSeenAt = gu.Login, s.now()
	sealed, err := seal(s.cfg.SecretKey, tok.RefreshToken, u.ID)
	if err != nil {
		s.mu.Unlock()
		return nil, "", err
	}
	u.RefreshToken = sealed
	u.RefreshExpiresAt = s.now().Add(time.Duration(tok.RefreshTokenExpiresIn) * time.Second)
	session := "xus_" + randomToken(32)
	ttl := s.sessionTTL(kind)
	s.st.Sessions[hashToken(session)] = &Session{Hash: hashToken(session), UserID: u.ID, Kind: kind, CreatedAt: s.now(), ExpiresAt: s.now().Add(ttl)}
	s.saveLocked()
	cp := *u
	s.mu.Unlock()

	s.tokMu.Lock()
	s.userTokens[u.ID] = userToken{token: tok.AccessToken, expires: s.now().Add(time.Duration(tok.ExpiresIn) * time.Second)}
	s.tokMu.Unlock()
	s.audit.Log("user.login", map[string]any{"actor": "user:" + gu.Login, "github_id": gu.ID, "kind": kind, "result": "ok"})
	if err := s.syncInstallations(ctx, cp.ID); err != nil {
		s.log.Printf("github: sync installations for %s: %v", gu.Login, err)
	}
	return &cp, session, nil
}

// syncInstallations records every installation the user can access,
// confirming each one as the app.
func (s *Service) syncInstallations(ctx context.Context, uid string) error {
	tok, err := s.userAccessToken(ctx, uid)
	if err != nil {
		return err
	}
	list, err := paginate[ghInstallation](ctx, s, tok, "/user/installations", "installations")
	if err != nil {
		return err
	}
	for _, in := range list {
		confirmed, err := s.confirmFor(ctx, &in)
		if err != nil {
			return err
		}
		s.upsertInstallation(confirmed)
	}
	return nil
}

// userAccessToken returns the user's 8-hour token, refreshing it (and
// rotating the refresh token) when needed. It is never written to disk.
func (s *Service) userAccessToken(ctx context.Context, uid string) (string, error) {
	s.tokMu.Lock()
	t, ok := s.userTokens[uid]
	s.tokMu.Unlock()
	if ok && s.now().Add(time.Minute).Before(t.expires) {
		return t.token, nil
	}
	s.mu.Lock()
	u := s.st.Users[uid]
	if u == nil || u.RefreshToken == "" {
		s.mu.Unlock()
		return "", ErrSignedOut
	}
	refresh, err := u.RefreshToken.open(s.cfg.SecretKey, uid)
	login := u.Login
	s.mu.Unlock()
	if err != nil {
		return "", fmt.Errorf("stored sign-in: %w", err)
	}
	var tok tokenResponse
	form := url.Values{"client_id": {s.cfg.ClientID}, "grant_type": {"refresh_token"}, "refresh_token": {refresh}}
	if s.cfg.ClientSecret != "" { // not needed for device-flow sign-ins (the shared app)
		form.Set("client_secret", s.cfg.ClientSecret)
	}
	err = s.oauthPost(ctx, "/login/oauth/access_token", form, &tok)
	if err == nil && tok.Error != "" {
		err = fmt.Errorf("%s: %s", tok.Error, tok.ErrorDescription)
	}
	if err != nil || tok.AccessToken == "" {
		s.audit.Log("user.refresh", map[string]any{"actor": "user:" + login, "result": "error", "error": fmt.Sprint(err)})
		if tok.Error == "bad_refresh_token" || tok.Error == "unauthorized" {
			s.signOutUser(uid, "refresh token rejected by GitHub")
			return "", ErrSignedOut
		}
		return "", fmt.Errorf("refresh GitHub sign-in: %v", err)
	}
	sealed, err := seal(s.cfg.SecretKey, tok.RefreshToken, uid)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	if u := s.st.Users[uid]; u != nil {
		u.RefreshToken = sealed
		u.RefreshExpiresAt = s.now().Add(time.Duration(tok.RefreshTokenExpiresIn) * time.Second)
		s.saveLocked()
	}
	s.mu.Unlock()
	s.tokMu.Lock()
	s.userTokens[uid] = userToken{token: tok.AccessToken, expires: s.now().Add(time.Duration(tok.ExpiresIn) * time.Second)}
	s.tokMu.Unlock()
	s.audit.Log("user.refresh", map[string]any{"actor": "user:" + login, "result": "ok"})
	return tok.AccessToken, nil
}

// revokeGrant asks GitHub to revoke the user's authorization of this app
// (all their tokens for it). Best effort.
func (s *Service) revokeGrant(ctx context.Context, accessToken string) {
	if s.cfg.ClientSecret == "" {
		return // the shared app: people revoke it in their GitHub settings (Applications)
	}
	body, _ := json.Marshal(map[string]string{"access_token": accessToken})
	req, err := http.NewRequestWithContext(ctx, "DELETE", s.cfg.APIURL+"/applications/"+s.cfg.ClientID+"/grant", strings.NewReader(string(body)))
	if err != nil {
		return
	}
	req.SetBasicAuth(s.cfg.ClientID, s.cfg.ClientSecret)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "xu526")
	if resp, err := s.http.Do(req); err == nil {
		resp.Body.Close()
	}
}

// signOutUser forgets a user's GitHub tokens and ends their sessions.
func (s *Service) signOutUser(uid, reason string) {
	s.mu.Lock()
	u := s.st.Users[uid]
	login := ""
	if u != nil {
		login = u.Login
		u.RefreshToken = ""
	}
	n := 0
	for h, se := range s.st.Sessions {
		if se.UserID == uid {
			delete(s.st.Sessions, h)
			n++
		}
	}
	s.saveLocked()
	s.mu.Unlock()
	s.tokMu.Lock()
	delete(s.userTokens, uid)
	s.tokMu.Unlock()
	s.audit.Log("user.revoked", map[string]any{"actor": "user:" + login, "reason": reason, "sessions_ended": n})
}

func (s *Service) revokeGitHubUser(githubID int64, reason string) {
	s.mu.Lock()
	var uid string
	for _, u := range s.st.Users {
		if u.GitHubID == githubID {
			uid = u.ID
		}
	}
	s.mu.Unlock()
	if uid != "" {
		s.signOutUser(uid, reason)
	}
}

// ---- sessions and API authentication ----

type ctxKey struct{}

// UserFrom returns the signed-in user of a request that passed Middleware.
func UserFrom(ctx context.Context) *User {
	u, _ := ctx.Value(ctxKey{}).(*User)
	return u
}

// WithUser attaches a user to a context (tests and internal callers).
func WithUser(ctx context.Context, u *User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

// authenticate finds the session behind a request: a Bearer token (CLI)
// or the session cookie (dashboard). cookie reports which one was used.
func (s *Service) authenticate(r *http.Request) (u *User, cookie bool, ok bool) {
	tok := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		tok = strings.TrimPrefix(h, "Bearer ")
	} else if c, err := r.Cookie(sessionCookie); err == nil {
		tok, cookie = c.Value, true
	}
	if !strings.HasPrefix(tok, "xus_") {
		return nil, cookie, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h := hashToken(tok)
	se := s.st.Sessions[h]
	if se == nil {
		return nil, cookie, false
	}
	if s.now().After(se.ExpiresAt) {
		delete(s.st.Sessions, h)
		s.saveLocked()
		return nil, cookie, false
	}
	usr := s.st.Users[se.UserID]
	if usr == nil {
		return nil, cookie, false
	}
	usr.LastSeenAt = s.now()
	cp := *usr
	return &cp, cookie, true
}

// CSRFHeader must accompany cookie-authenticated requests that change
// anything. Browsers won't send custom headers cross-site without CORS.
const CSRFHeader = "X-Xu-Csrf"

// Middleware requires a session for every request except those exempt
// reports true for.
func (s *Service) Middleware(next http.Handler, exempt func(*http.Request) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if exempt != nil && exempt(r) {
			// Open to everyone, but still tell the handler who's asking.
			if u, _, ok := s.authenticate(r); ok {
				r = r.WithContext(WithUser(r.Context(), u))
			}
			next.ServeHTTP(w, r)
			return
		}
		u, cookie, ok := s.authenticate(r)
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "sign in with GitHub first: run `xu login`, or use \"Sign in with GitHub\" in the dashboard", "login_url": "/github/login"})
			return
		}
		if cookie && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(CSRFHeader) != "1" {
			http.Error(w, "missing "+CSRFHeader+" header", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), u)))
	})
}

// Logout ends the request's session; everywhere also revokes the GitHub
// authorization, ending every session of that user.
func (s *Service) Logout(ctx context.Context, r *http.Request, everywhere bool) error {
	u, _, ok := s.authenticate(r)
	if !ok {
		return ErrSignedOut
	}
	if everywhere {
		if tok, err := s.userAccessToken(ctx, u.ID); err == nil {
			s.revokeGrant(ctx, tok)
		}
		s.signOutUser(u.ID, "signed out everywhere")
		return nil
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if c, err := r.Cookie(sessionCookie); err == nil && !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		tok = c.Value
	}
	s.mu.Lock()
	delete(s.st.Sessions, hashToken(tok))
	s.saveLocked()
	s.mu.Unlock()
	s.audit.Log("session.end", map[string]any{"actor": "user:" + u.Login})
	return nil
}

// ---- what a user may access ----

// UserInstallation is an installation with the repositories this user can
// reach through it.
type UserInstallation struct {
	Installation
	UserRepos []RepoRef `json:"user_repos"`
}

// UserInstallations lists, live from GitHub, what the user can connect.
func (s *Service) UserInstallations(ctx context.Context, uid string) ([]UserInstallation, error) {
	tok, err := s.userAccessToken(ctx, uid)
	if err != nil {
		return nil, err
	}
	list, err := paginate[ghInstallation](ctx, s, tok, "/user/installations", "installations")
	if err != nil {
		return nil, err
	}
	var out []UserInstallation
	for _, gi := range list {
		in, ok := s.Installation(gi.ID)
		if !ok {
			confirmed, err := s.confirmFor(ctx, &gi)
			if err != nil {
				return nil, err
			}
			in = *s.upsertInstallation(confirmed)
		}
		repos, err := paginate[ghRepo](ctx, s, tok, "/user/installations/"+strconv.FormatInt(gi.ID, 10)+"/repositories", "repositories")
		if err != nil {
			return nil, err
		}
		ui := UserInstallation{Installation: in}
		for _, r := range repos {
			ui.UserRepos = append(ui.UserRepos, r.ref())
		}
		out = append(out, ui)
	}
	return out, nil
}

// UserCanAccess checks, with the user's own token, that a repository is
// reachable by them through this installation. This is what stops a
// signed-in user from connecting repositories they can't see.
func (s *Service) UserCanAccess(ctx context.Context, uid string, inst, repo int64) (RepoRef, error) {
	tok, err := s.userAccessToken(ctx, uid)
	if err != nil {
		return RepoRef{}, err
	}
	repos, err := paginate[ghRepo](ctx, s, tok, "/user/installations/"+strconv.FormatInt(inst, 10)+"/repositories", "repositories")
	if err != nil {
		if NotFound(err) {
			return RepoRef{}, fmt.Errorf("you don't have access to installation %d", inst)
		}
		return RepoRef{}, err
	}
	for _, r := range repos {
		if r.ID == repo {
			return r.ref(), nil
		}
	}
	return RepoRef{}, fmt.Errorf("repository %d isn't available to you through installation %d", repo, inst)
}

// FindRepo resolves owner/name among the user's installations.
func (s *Service) FindRepo(ctx context.Context, uid, fullName string) (int64, RepoRef, error) {
	list, err := s.UserInstallations(ctx, uid)
	if err != nil {
		return 0, RepoRef{}, err
	}
	for _, in := range list {
		for _, r := range in.UserRepos {
			if strings.EqualFold(r.FullName, fullName) {
				return in.ID, r, nil
			}
		}
	}
	return 0, RepoRef{}, fmt.Errorf("%s isn't in any GitHub App installation you can access; install the app on it: %s", fullName, s.InstallURL())
}

// InstallURL is where users pick "all" or "only select" repositories.
func (s *Service) InstallURL() string {
	return s.cfg.WebURL + "/apps/" + s.cfg.Slug + "/installations/new"
}

// UserByID returns a user record.
func (s *Service) UserByID(uid string) (User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.st.Users[uid]
	if !ok {
		return User{}, false
	}
	return *u, true
}

// ---- browser flows (served on the dashboard's origin) ----

func pkce() (verifier, challenge string) {
	verifier = randomToken(48)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func (s *Service) newFlow(w http.ResponseWriter, purpose, next string) *oauthFlow {
	f := &oauthFlow{state: randomToken(24), purpose: purpose, next: next, expires: s.now().Add(flowTTL)}
	s.flowMu.Lock()
	s.oauth[f.state] = f
	s.flowMu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: f.state, Path: "/github/", MaxAge: int(flowTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: s.secureCookies()})
	return f
}

func (s *Service) takeFlow(state string) *oauthFlow {
	s.flowMu.Lock()
	defer s.flowMu.Unlock()
	f := s.oauth[state]
	delete(s.oauth, state) // single use
	if f == nil || s.now().After(f.expires) {
		return nil
	}
	return f
}

func (s *Service) secureCookies() bool { return strings.HasPrefix(s.cfg.CallbackURL, "https://") }

// safeNext allows only local paths, so the callback can't redirect elsewhere.
func safeNext(n string) string {
	if !strings.HasPrefix(n, "/") || strings.HasPrefix(n, "//") || strings.HasPrefix(n, "/\\") {
		return "/"
	}
	return n
}

func page(w http.ResponseWriter, status int, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>%s</title><body style="font:16px/1.5 system-ui;max-width:36rem;margin:15vh auto;padding:0 1rem"><h1 style="font-size:1.4rem">%s</h1><p>%s</p><p><a href="/">Back to the dashboard</a></p>`,
		html.EscapeString(title), html.EscapeString(title), html.EscapeString(msg))
}

// BrowserRoutes adds /github/login, /github/install, /github/setup and
// /github/callback.
func (s *Service) BrowserRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /github/login", func(w http.ResponseWriter, r *http.Request) {
		f := s.newFlow(w, "login", safeNext(r.URL.Query().Get("next")))
		verifier, challenge := pkce()
		f.verifier = verifier
		q := url.Values{"client_id": {s.cfg.ClientID}, "redirect_uri": {s.cfg.CallbackURL}, "state": {f.state},
			"code_challenge": {challenge}, "code_challenge_method": {"S256"}}
		http.Redirect(w, r, s.cfg.WebURL+"/login/oauth/authorize?"+q.Encode(), http.StatusFound)
	})
	mux.HandleFunc("GET /github/install", func(w http.ResponseWriter, r *http.Request) {
		f := s.newFlow(w, "install", safeNext(r.URL.Query().Get("next")))
		http.Redirect(w, r, s.InstallURL()+"?state="+url.QueryEscape(f.state), http.StatusFound)
	})
	// GitHub's post-install redirect. installation_id here is unauthenticated
	// and is ignored: installations are linked only from the user's own token.
	mux.HandleFunc("GET /github/setup", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/github/login?next="+url.QueryEscape("/?gh=repos"), http.StatusFound)
	})
	mux.HandleFunc("GET /github/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		c, err := r.Cookie(stateCookie)
		if (err != nil || q.Get("state") == "" || c.Value != q.Get("state")) && q.Get("setup_action") != "" {
			// Back from installing the app without our state: the code here
			// can't be trusted, so sign in afresh (GitHub skips the prompt
			// for an app the user has just authorized).
			http.Redirect(w, r, "/github/login?next="+url.QueryEscape("/?gh=repos"), http.StatusFound)
			return
		}
		if err != nil || q.Get("state") == "" || c.Value != q.Get("state") {
			page(w, http.StatusBadRequest, "Sign-in couldn't be verified", "This link didn't start from this browser, or it expired. Start again from the dashboard.")
			return
		}
		f := s.takeFlow(q.Get("state"))
		if f == nil {
			page(w, http.StatusBadRequest, "Sign-in link expired", "This sign-in link expired or was already used. Start again from the dashboard.")
			return
		}
		http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: "", Path: "/github/", MaxAge: -1})
		if e := q.Get("error"); e != "" {
			page(w, http.StatusForbidden, "GitHub sign-in was cancelled", q.Get("error_description"))
			return
		}
		code := q.Get("code")
		if code == "" {
			// Installed without authorizing: sign in to link the installation.
			http.Redirect(w, r, "/github/login?next="+url.QueryEscape(f.next), http.StatusFound)
			return
		}
		form := url.Values{"client_id": {s.cfg.ClientID}, "client_secret": {s.cfg.ClientSecret}, "code": {code}, "redirect_uri": {s.cfg.CallbackURL}}
		if f.verifier != "" {
			form.Set("code_verifier", f.verifier)
		}
		var tok tokenResponse
		err = s.oauthPost(r.Context(), "/login/oauth/access_token", form, &tok)
		if err == nil && tok.Error != "" {
			err = fmt.Errorf("%s", firstNonEmpty(tok.ErrorDescription, tok.Error))
		}
		if err != nil {
			page(w, http.StatusBadGateway, "GitHub sign-in failed", err.Error())
			return
		}
		_, session, err := s.completeLogin(r.Context(), &tok, "web")
		if err != nil {
			page(w, http.StatusForbidden, "Can't sign in", err.Error())
			return
		}
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: session, Path: "/", MaxAge: int(webSessionTTL.Seconds()),
			HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: s.secureCookies()})
		// A Strict cookie isn't sent on the redirect that follows a cross-site
		// navigation, so land on a same-site page that moves on.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta http-equiv="refresh" content="0;url=%s"><p>Signed in. <a href="%s">Continue</a></p>`,
			html.EscapeString(f.next), html.EscapeString(f.next))
	})
	mux.HandleFunc("POST /github/logout", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(CSRFHeader) != "1" {
			http.Error(w, "missing "+CSRFHeader+" header", http.StatusForbidden)
			return
		}
		s.Logout(r.Context(), r, false)
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
		w.WriteHeader(http.StatusNoContent)
	})
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// confirmFor confirms an installation the user's token listed. The full app
// asks GitHub as the app; the shared app has no app key, so it takes
// GitHub's answer to the user's own token, which lists only this app's
// installations that the user can access.
func (s *Service) confirmFor(ctx context.Context, gi *ghInstallation) (*ghInstallation, error) {
	if s.cfg.Shared {
		return gi, nil
	}
	return s.confirmInstallation(ctx, gi.ID)
}

// sessionTTL is how long a session lasts. The shared app's dashboard runs
// on the user's own computer, so its sessions last as long as the CLI's.
func (s *Service) sessionTTL(kind string) time.Duration {
	if kind == "cli" || s.cfg.Shared {
		return cliSessionTTL
	}
	return webSessionTTL
}
