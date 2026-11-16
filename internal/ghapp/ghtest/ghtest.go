// Package ghtest is a fake GitHub for tests: app JWT verification,
// installation tokens, OAuth code/refresh/device grants, user
// installations, repository lookups, manifest conversion, signed webhook
// delivery and git over HTTP (via git http-backend) guarded by tokens.
package ghtest

import (
	"bytes"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Repo is a fake repository. GitDir (optional) is a bare repository served
// at /<FullName>.git.
type Repo struct {
	ID       int64
	FullName string
	Private  bool
	GitDir   string
}

// Installation is a fake app installation.
type Installation struct {
	ID          int64
	Account     string
	AccountID   int64
	Selection   string  // all | selected
	Repos       []int64 // repositories it covers
	Suspended   bool
	Permissions map[string]string
}

// User is a fake GitHub account. Access lists the repos they can see.
type User struct {
	ID     int64
	Login  string
	Access []int64
}

// Mint records an installation token request.
type Mint struct {
	Installation int64
	Repos        []int64
	Permissions  map[string]string
	Token        string
}

// Fake is a running fake GitHub.
type Fake struct {
	*httptest.Server
	ClientID, ClientSecret string
	AppID                  int64
	Key                    *rsa.PrivateKey // the app's private key (for the PEM)

	mu            sync.Mutex
	Repos         map[int64]*Repo
	Installations map[int64]*Installation
	Users         map[int64]*User
	AuthAs        int64             // user who approves browser authorizations
	AutoApprove   int64             // if set, device codes are approved at once as this user
	AppCallback   string            // the callback URL from the manifest flow (installs return there)
	GrantExtra    map[string]string // misbehave: add these permissions to minted tokens
	NonExpiring   bool              // misbehave: issue user tokens without expiry
	Mints         []Mint
	Clones        []string // repo full names cloned with a valid token
	Revoked       []int64  // users whose grant was revoked

	codes      map[string]codeGrant
	devices    map[string]*device
	userTok    map[string]int64 // access token → user
	refreshTok map[string]int64
	deviceRT   map[string]bool // refresh tokens from the device flow: refreshing needs no client secret
	instTok    map[string]instToken
	now        func() time.Time
}

type codeGrant struct {
	user      int64
	challenge string
	redirect  string
}

type device struct {
	user     int64
	approved bool
	userCode string
}

type instToken struct {
	inst    int64
	repos   []int64
	expires time.Time
}

// New starts a fake GitHub with a fresh app key on a random local port.
func New() *Fake {
	f := NewHandler()
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

// Handler serves the fake (for running it on a fixed address).
func (f *Fake) Handler() http.Handler { return http.HandlerFunc(f.serve) }

// NewHandler returns a fake without starting a server; set URL yourself.
func NewHandler() *Fake {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	f := &Fake{ClientID: "Iv1.fakeclient", ClientSecret: "fake-client-secret", AppID: 4242, Key: key,
		Repos: map[int64]*Repo{}, Installations: map[int64]*Installation{}, Users: map[int64]*User{},
		codes: map[string]codeGrant{}, devices: map[string]*device{}, userTok: map[string]int64{},
		refreshTok: map[string]int64{}, deviceRT: map[string]bool{}, instTok: map[string]instToken{}, now: time.Now}
	f.Server = &httptest.Server{}
	return f
}

// PEM is the app's private key as GitHub hands it out.
func (f *Fake) PEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(f.Key)})
}

func random() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"message": msg})
}

// verifyJWT checks an app JWT like GitHub does.
func (f *Fake) verifyJWT(r *http.Request) error {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return fmt.Errorf("no bearer token")
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return fmt.Errorf("not a JWT")
	}
	enc := base64.RawURLEncoding
	var hdr struct{ Alg string }
	hb, _ := enc.DecodeString(parts[0])
	json.Unmarshal(hb, &hdr)
	if hdr.Alg != "RS256" {
		return fmt.Errorf("alg %q", hdr.Alg)
	}
	sig, err := enc.DecodeString(parts[2])
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&f.Key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		return fmt.Errorf("bad signature")
	}
	var c struct {
		Iat, Exp int64
		Iss      any
	}
	cb, _ := enc.DecodeString(parts[1])
	json.Unmarshal(cb, &c)
	now := f.now().Unix()
	if fmt.Sprint(c.Iss) != f.ClientID && fmt.Sprint(c.Iss) != strconv.FormatInt(f.AppID, 10) {
		return fmt.Errorf("iss %v", c.Iss)
	}
	if c.Exp <= now || c.Iat > now+60 || c.Exp-c.Iat > 660 {
		return fmt.Errorf("JWT expired or too long-lived")
	}
	return nil
}

func (f *Fake) userFrom(r *http.Request) (*User, bool) {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.userTok[tok]
	if !ok {
		return nil, false
	}
	return f.Users[id], true
}

func (f *Fake) instJSON(in *Installation) map[string]any {
	typ := "User"
	if strings.HasPrefix(in.Account, "org-") {
		typ = "Organization"
	}
	perms := in.Permissions
	if perms == nil {
		perms = map[string]string{"contents": "read", "metadata": "read"}
	}
	m := map[string]any{"id": in.ID, "app_id": f.AppID, "repository_selection": in.Selection, "permissions": perms,
		"account": map[string]any{"id": in.AccountID, "login": in.Account, "type": typ}}
	if in.Suspended {
		m["suspended_at"] = f.now().UTC().Format(time.RFC3339)
	}
	return m
}

func (f *Fake) repoJSON(r *Repo) map[string]any {
	return map[string]any{"id": r.ID, "full_name": r.FullName, "private": r.Private, "default_branch": "main"}
}

func contains(xs []int64, x int64) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/login/oauth/authorize":
		q := r.URL.Query()
		if q.Get("client_id") != f.ClientID {
			http.Error(w, "unknown client", 400)
			return
		}
		code := random()
		f.mu.Lock()
		f.codes[code] = codeGrant{user: f.AuthAs, challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri")}
		f.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	case p == "/login/device/code":
		r.ParseForm()
		if r.Form.Get("client_id") != f.ClientID {
			writeJSON(w, 200, map[string]string{"error": "incorrect_client_credentials"})
			return
		}
		dc := random()
		uc := strings.ToUpper(random()[:4] + "-" + random()[:4])
		f.mu.Lock()
		f.devices[dc] = &device{userCode: uc, approved: f.AutoApprove != 0, user: f.AutoApprove}
		f.mu.Unlock()
		writeJSON(w, 200, map[string]any{"device_code": dc, "user_code": uc, "verification_uri": f.URL + "/login/device", "expires_in": 900, "interval": 5})
	case p == "/login/oauth/access_token":
		f.token(w, r)
	case p == "/user":
		u, ok := f.userFrom(r)
		if !ok {
			fail(w, 401, "Bad credentials")
			return
		}
		writeJSON(w, 200, map[string]any{"id": u.ID, "login": u.Login})
	case p == "/user/installations":
		u, ok := f.userFrom(r)
		if !ok {
			fail(w, 401, "Bad credentials")
			return
		}
		f.mu.Lock()
		var list []map[string]any
		for _, in := range f.Installations {
			for _, rid := range in.Repos {
				if contains(u.Access, rid) {
					list = append(list, f.instJSON(in))
					break
				}
			}
		}
		f.mu.Unlock()
		if r.URL.Query().Get("page") != "1" && r.URL.Query().Get("page") != "" {
			list = nil
		}
		writeJSON(w, 200, map[string]any{"total_count": len(list), "installations": list})
	case strings.HasPrefix(p, "/user/installations/") && strings.HasSuffix(p, "/repositories"):
		u, ok := f.userFrom(r)
		if !ok {
			fail(w, 401, "Bad credentials")
			return
		}
		id, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(p, "/user/installations/"), "/repositories"), 10, 64)
		f.mu.Lock()
		in := f.Installations[id]
		var repos []map[string]any
		visible := false
		if in != nil {
			for _, rid := range in.Repos {
				if contains(u.Access, rid) {
					visible = true
					repos = append(repos, f.repoJSON(f.Repos[rid]))
				}
			}
		}
		f.mu.Unlock()
		if !visible {
			fail(w, 404, "Not Found")
			return
		}
		writeJSON(w, 200, map[string]any{"total_count": len(repos), "repositories": repos})
	case strings.HasPrefix(p, "/app/installations/"):
		if err := f.verifyJWT(r); err != nil {
			fail(w, 401, "A JSON web token could not be decoded: "+err.Error())
			return
		}
		rest := strings.TrimPrefix(p, "/app/installations/")
		idStr, action, _ := strings.Cut(rest, "/")
		id, _ := strconv.ParseInt(idStr, 10, 64)
		f.mu.Lock()
		in := f.Installations[id]
		f.mu.Unlock()
		if in == nil {
			fail(w, 404, "Not Found")
			return
		}
		if action == "" {
			writeJSON(w, 200, f.instJSON(in))
			return
		}
		f.mint(w, r, in)
	case strings.HasPrefix(p, "/repositories/"):
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		id, _ := strconv.ParseInt(strings.TrimPrefix(p, "/repositories/"), 10, 64)
		f.mu.Lock()
		it, ok := f.instTok[tok]
		repo := f.Repos[id]
		allowed := ok && !f.now().After(it.expires) && contains(it.repos, id)
		if uid, isUser := f.userTok[tok]; isUser && !allowed {
			allowed = f.userCanReadLocked(uid, id) // a user token: repos both the user and the app can reach
		}
		f.mu.Unlock()
		if !allowed || repo == nil {
			fail(w, 404, "Not Found")
			return
		}
		writeJSON(w, 200, f.repoJSON(repo))
	case strings.HasPrefix(p, "/repos/") && strings.HasSuffix(p, "/branches"):
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		full := strings.TrimSuffix(strings.TrimPrefix(p, "/repos/"), "/branches")
		f.mu.Lock()
		var repo *Repo
		for _, rp := range f.Repos {
			if rp.FullName == full {
				repo = rp
			}
		}
		allowed := false
		if uid, ok := f.userTok[tok]; ok && repo != nil {
			allowed = f.userCanReadLocked(uid, repo.ID)
		}
		f.mu.Unlock()
		if !allowed {
			fail(w, 404, "Not Found")
			return
		}
		names := []map[string]string{}
		if repo.GitDir != "" {
			out, _ := exec.Command("git", "--git-dir="+repo.GitDir, "for-each-ref", "--format=%(refname:short)", "refs/heads").Output()
			for _, n := range strings.Fields(string(out)) {
				names = append(names, map[string]string{"name": n})
			}
		}
		writeJSON(w, 200, names)
	case strings.HasPrefix(p, "/repos/") && strings.Contains(p, "/commits/"):
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		full, sha, _ := strings.Cut(strings.TrimPrefix(p, "/repos/"), "/commits/")
		f.mu.Lock()
		var repo *Repo
		for _, rp := range f.Repos {
			if rp.FullName == full {
				repo = rp
			}
		}
		allowed := false
		if repo != nil {
			if it, ok := f.instTok[tok]; ok && contains(it.repos, repo.ID) {
				allowed = true
			} else if uid, ok := f.userTok[tok]; ok {
				allowed = f.userCanReadLocked(uid, repo.ID)
			}
		}
		f.mu.Unlock()
		if !allowed || repo.GitDir == "" {
			fail(w, 404, "Not Found")
			return
		}
		out, err := exec.Command("git", "--git-dir="+repo.GitDir, "log", "-1", "--format=%H%n%an%n%aI%n%B", sha).Output()
		if err != nil {
			fail(w, 422, "No commit found for SHA: "+sha)
			return
		}
		parts := strings.SplitN(string(out), "\n", 4)
		for len(parts) < 4 {
			parts = append(parts, "")
		}
		writeJSON(w, 200, map[string]any{"sha": parts[0], "commit": map[string]any{"message": strings.TrimSpace(parts[3]),
			"author": map[string]any{"name": parts[1], "date": parts[2]}}})
	case strings.HasPrefix(p, "/applications/") && strings.HasSuffix(p, "/grant") && r.Method == "DELETE":
		cid, sec, ok := r.BasicAuth()
		if !ok || cid != f.ClientID || sec != f.ClientSecret {
			fail(w, 401, "Bad credentials")
			return
		}
		var body struct {
			AccessToken string `json:"access_token"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		if uid, ok := f.userTok[body.AccessToken]; ok {
			for t, u := range f.userTok {
				if u == uid {
					delete(f.userTok, t)
				}
			}
			for t, u := range f.refreshTok {
				if u == uid {
					delete(f.refreshTok, t)
				}
			}
			f.Revoked = append(f.Revoked, uid)
		}
		f.mu.Unlock()
		w.WriteHeader(204)
	case p == "/settings/apps/new" && r.Method == "POST":
		// The manifest flow: "Create GitHub App" is clicked at once.
		r.ParseForm()
		var m struct {
			RedirectURL  string   `json:"redirect_url"`
			CallbackURLs []string `json:"callback_urls"`
		}
		if err := json.Unmarshal([]byte(r.Form.Get("manifest")), &m); err != nil || m.RedirectURL == "" {
			http.Error(w, "bad manifest", 400)
			return
		}
		f.mu.Lock()
		if len(m.CallbackURLs) > 0 {
			f.AppCallback = m.CallbackURLs[0]
		}
		f.mu.Unlock()
		http.Redirect(w, r, m.RedirectURL+"?code="+random()+"&state="+url.QueryEscape(r.URL.Query().Get("state")), http.StatusFound)
	case strings.HasPrefix(p, "/apps/") && strings.HasSuffix(p, "/installations/new"):
		// Installing with "request user authorization during installation":
		// GitHub sends the user to the callback URL with an OAuth code.
		f.mu.Lock()
		cb := f.AppCallback
		code := random()
		f.codes[code] = codeGrant{user: f.AuthAs, redirect: cb}
		f.mu.Unlock()
		if cb == "" {
			http.Error(w, "no callback URL known", 400)
			return
		}
		inst := int64(0)
		for id := range f.Installations {
			inst = id
		}
		http.Redirect(w, r, fmt.Sprintf("%s?code=%s&installation_id=%d&setup_action=install&state=%s", cb, code, inst, url.QueryEscape(r.URL.Query().Get("state"))), http.StatusFound)
	case strings.HasPrefix(p, "/app-manifests/") && strings.HasSuffix(p, "/conversions"):
		writeJSON(w, 201, map[string]any{"id": f.AppID, "slug": "xu526-test", "client_id": f.ClientID, "client_secret": f.ClientSecret,
			"webhook_secret": "manifest-webhook-secret", "pem": string(f.PEM()), "html_url": f.URL + "/apps/xu526-test"})
	case strings.Contains(p, ".git/"):
		f.git(w, r)
	default:
		fail(w, 404, "Not Found: "+p)
	}
}

func (f *Fake) issueUser(uid int64) map[string]any {
	at, rt := "ghu_"+random(), "ghr_"+random()
	f.userTok[at] = uid
	f.refreshTok[rt] = uid
	m := map[string]any{"access_token": at, "token_type": "bearer", "scope": ""}
	if !f.NonExpiring {
		m["expires_in"], m["refresh_token"], m["refresh_token_expires_in"] = 28800, rt, 15811200
	}
	return m
}

func (f *Fake) token(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	fm := r.Form
	if fm.Get("client_id") != f.ClientID {
		writeJSON(w, 200, map[string]string{"error": "incorrect_client_credentials"})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch fm.Get("grant_type") {
	case "urn:ietf:params:oauth:grant-type:device_code":
		d := f.devices[fm.Get("device_code")]
		switch {
		case d == nil:
			writeJSON(w, 200, map[string]string{"error": "expired_token"})
		case !d.approved:
			writeJSON(w, 200, map[string]string{"error": "authorization_pending"})
		default:
			delete(f.devices, fm.Get("device_code"))
			m := f.issueUser(d.user)
			if rt, ok := m["refresh_token"].(string); ok {
				f.deviceRT[rt] = true
			}
			writeJSON(w, 200, m)
		}
	case "refresh_token":
		rt := fm.Get("refresh_token")
		device := f.deviceRT[rt]
		if !device && fm.Get("client_secret") != f.ClientSecret { // GitHub: the secret is required unless the sign-in used the device flow
			writeJSON(w, 200, map[string]string{"error": "incorrect_client_credentials"})
			return
		}
		uid, ok := f.refreshTok[rt]
		if !ok {
			writeJSON(w, 200, map[string]string{"error": "bad_refresh_token", "error_description": "The refresh token passed is incorrect or expired."})
			return
		}
		delete(f.refreshTok, rt) // rotation: single use
		delete(f.deviceRT, rt)
		m := f.issueUser(uid)
		if nrt, ok := m["refresh_token"].(string); ok && device {
			f.deviceRT[nrt] = true
		}
		writeJSON(w, 200, m)
	default: // authorization_code
		if fm.Get("client_secret") != f.ClientSecret {
			writeJSON(w, 200, map[string]string{"error": "incorrect_client_credentials"})
			return
		}
		g, ok := f.codes[fm.Get("code")]
		delete(f.codes, fm.Get("code"))
		if !ok {
			writeJSON(w, 200, map[string]string{"error": "bad_verification_code"})
			return
		}
		if g.challenge != "" {
			sum := sha256.Sum256([]byte(fm.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
				writeJSON(w, 200, map[string]string{"error": "invalid_grant", "error_description": "PKCE verification failed"})
				return
			}
		}
		if g.redirect != fm.Get("redirect_uri") {
			writeJSON(w, 200, map[string]string{"error": "redirect_uri_mismatch"})
			return
		}
		writeJSON(w, 200, f.issueUser(g.user))
	}
}

func (f *Fake) mint(w http.ResponseWriter, r *http.Request, in *Installation) {
	var req struct {
		RepositoryIDs []int64           `json:"repository_ids"`
		Permissions   map[string]string `json:"permissions"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if in.Suspended {
		fail(w, 403, "This installation has been suspended")
		return
	}
	repos := req.RepositoryIDs
	if len(repos) == 0 {
		repos = in.Repos
	}
	for _, id := range repos {
		if !contains(in.Repos, id) {
			fail(w, 422, "There is at least one repository that does not exist or is not accessible to the parent installation.")
			return
		}
	}
	perms := map[string]string{"metadata": "read"}
	for k, v := range req.Permissions {
		perms[k] = v
	}
	f.mu.Lock()
	for k, v := range f.GrantExtra {
		perms[k] = v
	}
	tok := "ghs_" + random()
	exp := f.now().Add(time.Hour)
	f.instTok[tok] = instToken{inst: in.ID, repos: repos, expires: exp}
	f.Mints = append(f.Mints, Mint{Installation: in.ID, Repos: repos, Permissions: req.Permissions, Token: tok})
	var rs []map[string]any
	for _, id := range repos {
		rs = append(rs, f.repoJSON(f.Repos[id]))
	}
	f.mu.Unlock()
	writeJSON(w, 201, map[string]any{"token": tok, "expires_at": exp.UTC().Format(time.RFC3339), "permissions": perms, "repositories": rs})
}

// git serves /<owner>/<repo>.git/* with git http-backend, after checking
// the installation token in the Basic auth header covers that repository.
func (f *Fake) git(w http.ResponseWriter, r *http.Request) {
	name, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), ".git/")
	f.mu.Lock()
	var repo *Repo
	for _, rp := range f.Repos {
		if rp.FullName == name {
			repo = rp
		}
	}
	user, pass, ok := r.BasicAuth()
	it, tokOK := f.instTok[pass]
	if tokOK {
		tokOK = repo != nil && !f.now().After(it.expires) && contains(it.repos, repo.ID)
	} else if uid, isUser := f.userTok[pass]; isUser && repo != nil {
		tokOK = f.userCanReadLocked(uid, repo.ID) // GitHub App user tokens work for git too
	}
	f.mu.Unlock()
	if repo == nil || repo.GitDir == "" {
		http.NotFound(w, r)
		return
	}
	if !ok || user != "x-access-token" || !tokOK {
		w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
		http.Error(w, "Invalid username or token", http.StatusUnauthorized)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/git-upload-pack") {
		f.mu.Lock()
		f.Clones = append(f.Clones, name)
		f.mu.Unlock()
	}
	backend, err := exec.LookPath("git")
	if err != nil {
		http.Error(w, "git missing", 500)
		return
	}
	h := &cgi.Handler{Path: backend, Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + strings.TrimSuffix(repo.GitDir, "/"+name+".git"), "GIT_HTTP_EXPORT_ALL=1"}}
	h.ServeHTTP(w, r)
}

// userCanReadLocked: a user token reaches a repository the user can see and
// an installation of the app covers.
func (f *Fake) userCanReadLocked(uid, repo int64) bool {
	u := f.Users[uid]
	if u == nil || !contains(u.Access, repo) {
		return false
	}
	for _, in := range f.Installations {
		if !in.Suspended && contains(in.Repos, repo) {
			return true
		}
	}
	return false
}

// ApproveDevice lets a device code through as a user, as if they typed it
// at github.com/login/device.
func (f *Fake) ApproveDevice(userCode string, uid int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, d := range f.devices {
		if d.userCode == userCode {
			d.approved, d.user = true, uid
			return true
		}
	}
	return false
}

// ExpireUserTokens invalidates every user access token (8-hour expiry).
func (f *Fake) ExpireUserTokens() {
	f.mu.Lock()
	f.userTok = map[string]int64{}
	f.mu.Unlock()
}

// Deliver signs and posts a webhook event to url.
func (f *Fake) Deliver(target, secret, event string, payload any) (*http.Response, error) {
	body, _ := json.Marshal(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	req, _ := http.NewRequest("POST", target, bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", random())
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}

// SetClock makes the fake use the same clock as the code under test.
func (f *Fake) SetClock(now func() time.Time) { f.now = now }

// Lock exposes the fake's mutex for tests that edit its maps.
func (f *Fake) Lock()   { f.mu.Lock() }
func (f *Fake) Unlock() { f.mu.Unlock() }
