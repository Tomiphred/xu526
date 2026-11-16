package portal

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- fakes ----

type sentMail struct{ To, Subject, Body string }

type fakeMailer struct {
	mu   sync.Mutex
	sent []sentMail
	fail bool
}

func (m *fakeMailer) Send(_ context.Context, to, subject, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return fmt.Errorf("smtp down")
	}
	m.sent = append(m.sent, sentMail{to, subject, body})
	return nil
}

func (m *fakeMailer) count(to string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, s := range m.sent {
		if s.To == to {
			n++
		}
	}
	return n
}

func (m *fakeMailer) last(to string) sentMail {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.sent) - 1; i >= 0; i-- {
		if m.sent[i].To == to {
			return m.sent[i]
		}
	}
	return sentMail{}
}

var codeRE = regexp.MustCompile(`\b(\d{6})\b`)

func (m *fakeMailer) code(t *testing.T, to string) string {
	t.Helper()
	var got []string
	for i := 0; i < 300 && got == nil; i++ { // codes are mailed in the background
		if got = codeRE.FindStringSubmatch(m.last(to).Body); got == nil {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if got == nil {
		t.Fatalf("no code mailed to %s; last: %+v", to, m.last(to))
	}
	return got[1]
}

type fakeRelay struct {
	mu     sync.Mutex
	users  map[string]*RelayUser
	tokens map[string]string
	n      int
}

func newFakeRelay() *fakeRelay {
	return &fakeRelay{users: map[string]*RelayUser{}, tokens: map[string]string{}}
}

func (f *fakeRelay) AddUser(name string, maxApps int, _ float64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.users[name] != nil {
		return "", fmt.Errorf("user %s already exists", name)
	}
	f.n++
	f.users[name] = &RelayUser{MaxApps: maxApps, Apps: map[string]string{}}
	f.tokens[name] = fmt.Sprintf("xu_token%03d", f.n)
	return f.tokens[name], nil
}

func (f *fakeRelay) RotateToken(name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.users[name] == nil {
		return "", fmt.Errorf("no user")
	}
	f.n++
	f.tokens[name] = fmt.Sprintf("xu_token%03d", f.n)
	return f.tokens[name], nil
}

func (f *fakeRelay) User(name string) (RelayUser, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u := f.users[name]
	if u == nil {
		return RelayUser{}, false
	}
	return *u, true
}

func (f *fakeRelay) AgentURL() string { return "https://xu.example.com" }

// ---- a browser ----

type env struct {
	t     *testing.T
	srv   *Server
	ts    *httptest.Server
	store Store
	mail  *fakeMailer
	relay *fakeRelay
	now   time.Time
	mu    sync.Mutex
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, store: newTestStore(t), mail: &fakeMailer{}, relay: newFakeRelay(), now: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)}
	srv, err := New(Config{Host: "tunnels.example.com", CodeKey: []byte("0123456789abcdef0123456789abcdef")},
		e.store, e.mail, e.relay, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	srv.SetClock(func() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now })
	e.srv = srv
	e.ts = httptest.NewServer(srv)
	t.Cleanup(e.ts.Close)
	return e
}

func (e *env) advance(d time.Duration) { e.mu.Lock(); e.now = e.now.Add(d); e.mu.Unlock() }

type browser struct {
	e *env
	c *http.Client
}

func (e *env) browser() *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{e: e, c: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

var csrfRE = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

// get loads a page and returns status, body.
func (b *browser) get(path string) (int, string) {
	b.e.t.Helper()
	resp, err := b.c.Get(b.e.ts.URL + path)
	if err != nil {
		b.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, html.UnescapeString(string(body))
}

// post submits a form as a browser would (same origin, with the CSRF
// token from the page), following one redirect.
func (b *browser) post(path string, form url.Values) (int, string) {
	b.e.t.Helper()
	_, page := b.get("/signup") // any page sets the CSRF cookie
	m := csrfRE.FindStringSubmatch(page)
	if m == nil {
		b.e.t.Fatal("no csrf token in page")
	}
	form.Set("csrf", m[1])
	return b.rawPost(path, form, b.e.ts.URL)
}

func (b *browser) rawPost(path string, form url.Values, origin string) (int, string) {
	b.e.t.Helper()
	req, _ := http.NewRequest("POST", b.e.ts.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := b.c.Do(req)
	if err != nil {
		b.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther {
		return b.get(resp.Header.Get("Location"))
	}
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, html.UnescapeString(string(body))
}

func (b *browser) cookie(name string) string {
	u, _ := url.Parse(b.e.ts.URL)
	for _, c := range b.c.Jar.Cookies(u) {
		if c.Name == "xu526_"+name {
			return c.Value
		}
	}
	return ""
}

const pw = "correct horse battery"

// signUp creates and confirms an account; the browser ends up signed in.
func (b *browser) signUp(email string) {
	b.e.t.Helper()
	st, body := b.post("/signup", url.Values{"email": {email}, "password": {pw}, "confirm": {pw}})
	if st != 200 || !strings.Contains(body, "Check your email") {
		b.e.t.Fatalf("signup: %d %s", st, body)
	}
	st, body = b.post("/verify", url.Values{"code": {b.e.mail.code(b.e.t, strings.ToLower(email))}})
	if st != 200 || !strings.Contains(body, "Get your tunnel token") {
		b.e.t.Fatalf("verify: %d %s", st, body)
	}
}

// ---- tests ----

func TestSignupTokenAndRegenerate(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	b.signUp("Ada@Example.com")
	if a, _ := e.store.AccountByEmail(context.Background(), "ada@example.com"); a == nil || !a.Verified {
		t.Fatal("account not created or not verified")
	}
	st, body := b.post("/token", url.Values{"name": {"ada"}})
	if st != 200 || !strings.Contains(body, "xu_token001") || !strings.Contains(body, "xu tunnel connect https://xu.example.com xu_token001") {
		t.Fatalf("token page: %d %s", st, body)
	}
	if u, ok := e.relay.User("ada"); !ok || u.MaxApps != 5 {
		t.Fatalf("relay user: %+v %v", u, ok)
	}
	mailed := e.mail.last("ada@example.com")
	if !strings.Contains(mailed.Subject, "tunnel token") || !strings.Contains(mailed.Body, "xu_token001") || !strings.Contains(mailed.Body, "up to 5 apps") {
		t.Fatalf("token email: %+v", mailed)
	}
	// Reloading shows the tunnel, not the token.
	_, body = b.get("/")
	if strings.Contains(body, "xu_token001") || !strings.Contains(body, "0 of 5 in use") || !strings.Contains(body, "https://APP-ada.example.com") {
		t.Fatalf("dashboard: %s", body)
	}
	e.relay.users["ada"].Apps["shop"] = "https://shop-ada.example.com"
	if _, body = b.get("/"); !strings.Contains(body, "1 of 5 in use") || !strings.Contains(body, "https://shop-ada.example.com") {
		t.Fatalf("dashboard with an app: %s", body)
	}
	// Regenerate: same name, new token.
	st, body = b.post("/token", url.Values{})
	if st != 200 || !strings.Contains(body, "xu_token002") || e.relay.tokens["ada"] != "xu_token002" {
		t.Fatalf("regenerate: %d %s", st, body)
	}
	if got := e.events(); !strings.Contains(got, "signup,code-sent,signin,token-issued,token-regenerated") {
		t.Fatalf("audit: %s", got)
	}
}

func TestTokenNames(t *testing.T) {
	e := newEnv(t)
	e.relay.AddUser("taken", 10, 10)
	b := e.browser()
	b.signUp("bob@example.com")
	for name, want := range map[string]string{
		"admin": "reserved", "-bad": "lowercase letters", "ab": "lowercase letters", "Bob Smith": "lowercase letters", "taken": "taken",
	} {
		st, body := b.post("/token", url.Values{"name": {name}})
		if st != 400 || !strings.Contains(body, want) {
			t.Errorf("name %q: %d, want %q in page", name, st, want)
		}
	}
	if suggestName("Mary.Jane+tunnels@x.io") != "mary-jane-tunnel" {
		t.Errorf("suggestion: %s", suggestName("Mary.Jane+tunnels@x.io"))
	}
}

func TestSignInNeedsPasswordAndCode(t *testing.T) {
	e := newEnv(t)
	e.browser().signUp("cy@example.com")
	b := e.browser()
	before := e.mail.count("cy@example.com")
	_, wrong := b.post("/signin", url.Values{"email": {"cy@example.com"}, "password": {"wrong password!"}})
	_, unknown := b.post("/signin", url.Values{"email": {"nobody@example.com"}, "password": {pw}})
	if !strings.Contains(wrong, "don't match an account") || !strings.Contains(unknown, "don't match an account") {
		t.Fatalf("wrong password and unknown email must look the same:\n%s\n---\n%s", alertOf(wrong), alertOf(unknown))
	}
	if e.mail.count("cy@example.com") != before {
		t.Fatal("a wrong password must not send a code")
	}
	// Right password: a code is needed; the password alone doesn't sign in.
	e.advance(2 * time.Minute)
	st, body := b.post("/signin", url.Values{"email": {"cy@example.com"}, "password": {pw}})
	if st != 200 || !strings.Contains(body, "Check your email") {
		t.Fatalf("signin: %d %s", st, body)
	}
	if _, body = b.get("/"); !strings.Contains(body, "Sign in") || strings.Contains(body, "Your tunnel") {
		t.Fatal("signed in without the code")
	}
	pending := b.cookie("session")
	code := e.mail.code(t, "cy@example.com")
	b.post("/verify", url.Values{"code": {code}})
	if _, body = b.get("/"); !strings.Contains(body, "Get your tunnel token") {
		t.Fatalf("after code: %s", body)
	}
	if b.cookie("session") == pending {
		t.Fatal("the session ID must change when signing in")
	}
	// A code works once.
	b2 := e.browser()
	e.advance(2 * time.Minute)
	b2.post("/signin", url.Values{"email": {"cy@example.com"}, "password": {pw}})
	if st, _ := b2.post("/verify", url.Values{"code": {code}}); st != 401 {
		t.Fatal("an old code was accepted")
	}
}

func TestCodeAttemptsAndExpiry(t *testing.T) {
	e := newEnv(t)
	e.browser().signUp("di@example.com")
	b := e.browser()
	e.advance(2 * time.Minute)
	b.post("/signin", url.Values{"email": {"di@example.com"}, "password": {pw}})
	code := e.mail.code(t, "di@example.com")
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	var body string
	for i := 0; i < maxCodeTries; i++ {
		_, body = b.post("/verify", url.Values{"code": {wrong}})
	}
	if !strings.Contains(body, "Too many wrong codes") {
		t.Fatalf("after %d wrong codes: %s", maxCodeTries, body)
	}
	if st, _ := b.post("/verify", url.Values{"code": {code}}); st != 401 {
		t.Fatal("the right code must not work after too many wrong ones")
	}
	// A new code works; it expires after 10 minutes.
	e.advance(2 * time.Minute)
	b.post("/resend", url.Values{})
	code = e.mail.code(t, "di@example.com")
	e.advance(codeTTL + time.Second)
	if _, body = b.post("/verify", url.Values{"code": {code}}); !strings.Contains(body, "That sign-in took too long") && !strings.Contains(body, "expired") {
		t.Fatalf("expired code: %s", body)
	}
}

func TestSignupWithExistingEmailRevealsNothing(t *testing.T) {
	e := newEnv(t)
	e.browser().signUp("eve@example.com")
	before, _ := e.store.AccountByEmail(context.Background(), "eve@example.com")
	b := e.browser()
	st, body := b.post("/signup", url.Values{"email": {"eve@example.com"}, "password": {"attacker password"}, "confirm": {"attacker password"}})
	if st != 200 || !strings.Contains(body, "Check your email") {
		t.Fatalf("duplicate signup should look normal: %d", st)
	}
	time.Sleep(50 * time.Millisecond) // the warning email is sent in the background
	if m := e.mail.last("eve@example.com"); !strings.Contains(m.Subject, "tried to sign up") || codeRE.MatchString(m.Body) {
		t.Fatalf("the owner should get a warning, not a code: %+v", m)
	}
	for _, guess := range []string{"000000", "123456"} {
		if st, _ := b.post("/verify", url.Values{"code": {guess}}); st != 401 {
			t.Fatal("a decoy session must never verify")
		}
	}
	after, _ := e.store.AccountByEmail(context.Background(), "eve@example.com")
	if after.PasswordHash != before.PasswordHash {
		t.Fatal("signing up again must not change a confirmed account's password")
	}
}

func TestForgotAndReset(t *testing.T) {
	e := newEnv(t)
	old := e.browser()
	old.signUp("fay@example.com")
	b := e.browser()
	_, unknown := b.post("/forgot", url.Values{"email": {"ghost@example.com"}})
	_, known := b.post("/forgot", url.Values{"email": {"fay@example.com"}})
	if !strings.Contains(unknown, "Choose a new password") || !strings.Contains(known, "Choose a new password") {
		t.Fatal("forgot must look the same for any email")
	}
	code := e.mail.code(t, "fay@example.com")
	st, body := b.post("/reset", url.Values{"code": {code}, "password": {"a brand new password"}, "confirm": {"a brand new password"}})
	if st != 200 || !strings.Contains(body, "Get your tunnel token") {
		t.Fatalf("reset: %d %s", st, body)
	}
	if _, body = old.get("/"); strings.Contains(body, "Get your tunnel token") {
		t.Fatal("other sessions must be signed out after a reset")
	}
	a, _ := e.store.AccountByEmail(context.Background(), "fay@example.com")
	if !checkPassword("a brand new password", a.PasswordHash) {
		t.Fatal("password not changed")
	}
}

func TestFormsNeedCSRFAndSameOrigin(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	b.get("/")
	if st, _ := b.rawPost("/signin", url.Values{"email": {"x@example.com"}, "password": {pw}}, e.ts.URL); st != 403 {
		t.Fatalf("no csrf: %d", st)
	}
	_, page := b.get("/")
	tok := csrfRE.FindStringSubmatch(page)[1]
	if st, _ := b.rawPost("/signin", url.Values{"csrf": {tok}, "email": {"x@example.com"}, "password": {pw}}, "https://evil.example"); st != 403 {
		t.Fatalf("cross-origin: %d", st)
	}
	// A different browser's token doesn't work with this cookie.
	_, other := e.browser().get("/")
	if st, _ := b.rawPost("/signin", url.Values{"csrf": {csrfRE.FindStringSubmatch(other)[1]}, "email": {"x@example.com"}, "password": {pw}}, e.ts.URL); st != 403 {
		t.Fatalf("someone else's token: %d", st)
	}
	resp, _ := http.Get(e.ts.URL + "/")
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("security headers: %v", resp.Header)
	}
}

func TestRateLimits(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	var st int
	for i := 0; i < 11; i++ {
		st, _ = b.post("/signin", url.Values{"email": {"target@example.com"}, "password": {"guess number " + fmt.Sprint(i)}})
	}
	if st != http.StatusTooManyRequests {
		t.Fatalf("11th try on one email: %d", st)
	}
}

func TestTokenNeedsSignIn(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	if st, _ := b.post("/token", url.Values{"name": {"sneaky"}}); st != 401 {
		t.Fatalf("token without signing in: %d", st)
	}
	if _, ok := e.relay.User("sneaky"); ok {
		t.Fatal("relay user created without signing in")
	}
}

func TestTokenShownWhenMailFails(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	b.signUp("gus@example.com")
	e.mail.fail = true
	st, body := b.post("/token", url.Values{"name": {"gus"}})
	if st != 200 || !strings.Contains(body, "couldn't email it") || !strings.Contains(body, "xu_token001") {
		t.Fatalf("%d %s", st, body)
	}
}

func TestPasswordsAndEmails(t *testing.T) {
	h, err := hashPassword(pw)
	if err != nil || !checkPassword(pw, h) || checkPassword(pw+"x", h) || checkPassword(pw, "garbage") {
		t.Fatal("password hashing")
	}
	for in, ok := range map[string]bool{"a@b.co": true, "A.B+c@Sub.Example.org": true, "nope": false, "a@b": false,
		"Eve <e@x.io>": false, "a@b.co\r\nBcc: x@y.z": false, "": false} {
		_, err := normalEmail(in)
		if (err == nil) != ok {
			t.Errorf("%q: %v", in, err)
		}
	}
}

// ---- SMTP ----

// smtpServer is a minimal SMTP server for one message, speaking STARTTLS
// or implicit TLS, and requiring AUTH PLAIN over TLS.
func smtpServer(t *testing.T, implicit bool) (addr string, cfg *tls.Config, got chan string) {
	t.Helper()
	ts := httptest.NewUnstartedServer(nil)
	ts.StartTLS()
	cert := ts.TLS.Certificates[0]
	pool := ts.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	ts.Close()
	srvTLS := &tls.Config{Certificates: []tls.Certificate{cert}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got = make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if implicit {
			conn = tls.Server(conn, srvTLS)
		}
		r, w := bufio.NewReader(conn), conn
		say := func(s string) { io.WriteString(w, s+"\r\n") }
		say("220 test ESMTP")
		secure, authed := implicit, false
		var data strings.Builder
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(cmd, "EHLO"):
				if secure {
					say("250-test\r\n250 AUTH PLAIN")
				} else {
					say("250-test\r\n250 STARTTLS")
				}
			case cmd == "STARTTLS":
				say("220 go ahead")
				tc := tls.Server(conn, srvTLS)
				conn, r, w, secure = tc, bufio.NewReader(tc), tc, true
			case strings.HasPrefix(cmd, "AUTH PLAIN"):
				raw, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(line)[len("AUTH PLAIN "):])
				if !secure || string(raw) != "\x00mailer@example.com\x00s3cret" {
					say("535 no")
					continue
				}
				authed = true
				say("235 ok")
			case strings.HasPrefix(cmd, "MAIL FROM"), strings.HasPrefix(cmd, "RCPT TO"):
				if !authed {
					say("530 auth first")
					continue
				}
				say("250 ok")
			case cmd == "DATA":
				say("354 go")
				for {
					l, err := r.ReadString('\n')
					if err != nil || l == ".\r\n" {
						break
					}
					data.WriteString(l)
				}
				say("250 queued")
				got <- data.String()
			case cmd == "QUIT":
				say("221 bye")
				return
			default:
				say("250 ok")
			}
		}
	}()
	return ln.Addr().String(), &tls.Config{RootCAs: pool, ServerName: "example.com"}, got
}

func TestSMTPDelivery(t *testing.T) {
	for _, implicit := range []bool{false, true} {
		addr, cfg, got := smtpServer(t, implicit)
		host, port, _ := net.SplitHostPort(addr)
		var p int
		fmt.Sscan(port, &p)
		m := &SMTP{Host: host, Port: p, User: "mailer@example.com", Password: "s3cret", From: "mailer@example.com",
			FromName: "xu526 tunnel", ImplicitTLS: implicit, TLSConfig: cfg}
		if err := m.Send(context.Background(), "ada@example.com", "123456 is your code", "Your code is 123456.\nBye"); err != nil {
			t.Fatalf("implicit=%v: %v", implicit, err)
		}
		msg := <-got
		for _, want := range []string{"From: xu526 tunnel <mailer@example.com>", "To: ada@example.com", "Subject: 123456 is your code", "Your code is 123456."} {
			if !strings.Contains(msg, want) {
				t.Errorf("implicit=%v: message lacks %q:\n%s", implicit, want, msg)
			}
		}
	}
	m := &SMTP{Host: "x", Port: 587, From: "a@b.co"}
	if err := m.Send(context.Background(), "x@y.z\r\nBcc: z@z.z", "hi", "body"); err == nil {
		t.Fatal("header injection accepted")
	}
}

var alertRE = regexp.MustCompile(`(?s)<p class="alert"[^>]*>(.*?)</p>|<h1>(.*?)</h1>`)

func alertOf(page string) string {
	var out []string
	for _, m := range alertRE.FindAllStringSubmatch(page, -1) {
		out = append(out, m[1]+m[2])
	}
	return strings.Join(out, " | ")
}

// newTestStore makes the store tests run on: memory, or Postgres inside
// TestOnPostgres.
var newTestStore = func(t *testing.T) Store { return NewMemStore() }

// events lists audit event names in order.
func (e *env) events() string {
	switch st := e.store.(type) {
	case *MemStore:
		return st.Events()
	case *PGStore:
		var names []string
		rows, err := st.db.Query(`SELECT event FROM portal_audit ORDER BY at, id`)
		if err != nil {
			e.t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var n string
			rows.Scan(&n)
			names = append(names, n)
		}
		return strings.Join(names, ",")
	}
	return ""
}

// TestOnPostgres runs the portal's flows against PostgreSQL
// (XU_TEST_POSTGRES_URL, a database the test may wipe).
func TestOnPostgres(t *testing.T) {
	url := os.Getenv("XU_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set XU_TEST_POSTGRES_URL to run the portal on PostgreSQL")
	}
	fresh := func(t *testing.T) Store {
		st, err := OpenPG(context.Background(), url)
		if err != nil {
			t.Fatal(err)
		}
		st.db.Exec(`DROP TABLE IF EXISTS portal_audit, portal_tokens, portal_sessions, portal_codes, portal_accounts, portal_schema CASCADE`)
		st.Close()
		if st, err = OpenPG(context.Background(), url); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(st.Close)
		return st
	}
	old := newTestStore
	newTestStore = fresh
	defer func() { newTestStore = old }()
	for name, f := range map[string]func(*testing.T){
		"SignupTokenAndRegenerate": TestSignupTokenAndRegenerate, "TokenNames": TestTokenNames,
		"SignInNeedsPasswordAndCode": TestSignInNeedsPasswordAndCode, "CodeAttemptsAndExpiry": TestCodeAttemptsAndExpiry,
		"SignupWithExistingEmail": TestSignupWithExistingEmailRevealsNothing, "ForgotAndReset": TestForgotAndReset,
		"TokenShownWhenMailFails": TestTokenShownWhenMailFails, "ParallelGuessesCounted": TestParallelGuessesCounted,
		"PairNewAccount": TestPairNewAccount, "PairSecondComputer": TestPairSecondComputerRegenerates, "PairDenyExpireAndLimits": TestPairDenyExpireAndLimits,
	} {
		t.Run(name, f)
	}
	// Migrations are idempotent, and the schema version is recorded.
	st := fresh(t).(*PGStore)
	if err := st.migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	var v int64
	st.db.QueryRow(`SELECT version FROM portal_schema`).Scan(&v)
	if int(v) != len(migrations) {
		t.Fatalf("schema version %d", v)
	}
	// Expired sessions and codes are cleaned up.
	ctx := context.Background()
	a, _ := st.CreateAccount(ctx, "old@example.com", "h", time.Now())
	st.PutSession(ctx, &Session{IDHash: "x", AccountID: a.ID, Stage: StageFull, Expires: time.Now().Add(-time.Minute)})
	st.PutSession(ctx, &Session{IDHash: "decoy", Stage: StageCode, Expires: time.Now().Add(time.Hour)})
	st.PutCode(ctx, &Code{AccountID: a.ID, Purpose: "login", MAC: "m", Expires: time.Now().Add(-time.Second)})
	if err := st.DeleteExpired(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetSession(ctx, "x"); err != ErrNotFound {
		t.Fatal("expired session kept")
	}
	if s, err := st.GetSession(ctx, "decoy"); err != nil || s.AccountID != 0 {
		t.Fatalf("decoy session: %+v %v", s, err)
	}
	if _, err := st.GetCode(ctx, a.ID, "login"); err != ErrNotFound {
		t.Fatal("expired code kept")
	}
}

// TestParallelGuessesCounted: wrong codes sent at the same moment are all
// counted, so a burst can't try more than maxCodeTries codes.
func TestParallelGuessesCounted(t *testing.T) {
	e := newEnv(t)
	e.browser().signUp("hal@example.com")
	b := e.browser()
	e.advance(2 * time.Minute)
	b.post("/signin", url.Values{"email": {"hal@example.com"}, "password": {pw}})
	code := e.mail.code(t, "hal@example.com")
	_, page := b.get("/verify")
	tok := csrfRE.FindStringSubmatch(page)[1]
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			guess := fmt.Sprintf("%06d", (i*7919)%1000000)
			if guess == code {
				guess = "999999"
			}
			b.rawPost("/verify", url.Values{"csrf": {tok}, "code": {guess}}, e.ts.URL)
		}(i)
	}
	wg.Wait()
	if st, _ := b.rawPost("/verify", url.Values{"csrf": {tok}, "code": {code}}, e.ts.URL); st == 200 {
		t.Fatal("the right code worked after a burst of 20 wrong guesses")
	}
	wrong := strings.Count(e.events(), auditBadCode)
	if wrong > maxCodeTries {
		t.Fatalf("%d codes were compared; at most %d may be", wrong, maxCodeTries)
	}
}

// ---- connecting a computer (pairing) ----

func (e *env) api(path string, body any) (int, map[string]any) {
	e.t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(e.ts.URL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// poll checks for approval, waiting out the per-device interval.
func (e *env) poll(device string) map[string]any {
	e.t.Helper()
	e.advance(pairInterval)
	_, out := e.api("/api/pair/poll", map[string]string{"device_code": device})
	return out
}

func TestPairNewAccount(t *testing.T) {
	e := newEnv(t)
	st, start := e.api("/api/pair/start", map[string]string{"device": "DESKTOP-QFERBLV"})
	if st != 200 || !regexp.MustCompile(`^[B-Z]{4}-[B-Z]{4}$`).MatchString(start["user_code"].(string)) {
		t.Fatalf("start: %d %v", st, start)
	}
	device, code := start["device_code"].(string), start["user_code"].(string)
	if want := "https://tunnels.example.com/pair?code=" + code; start["verify_url"] != want {
		t.Fatalf("verify_url %v", start["verify_url"])
	}
	if s := e.poll(device)["status"]; s != "pending" {
		t.Fatalf("before approval: %v", s)
	}
	// The person follows the link signed out, signs up, and lands back on
	// the approval page.
	b := e.browser()
	_, body := b.get("/pair?code=" + code)
	if !strings.Contains(body, "Sign in, or create an account, to connect your computer") {
		t.Fatalf("signed out: %s", alertOf(body))
	}
	b.post("/signup", url.Values{"email": {"nia@example.com"}, "password": {pw}, "confirm": {pw}})
	st, body = b.post("/verify", url.Values{"code": {e.mail.code(t, "nia@example.com")}})
	if st != 200 || !strings.Contains(body, "Connect DESKTOP-QFERBLV?") || !strings.Contains(body, code) {
		t.Fatalf("after sign-up: %d %s", st, alertOf(body))
	}
	if strings.Contains(body, "xu_") {
		t.Fatal("the approval page must not show a token")
	}
	st, body = b.post("/pair", url.Values{"code": {code}, "action": {"approve"}, "name": {"nia"}})
	if st != 200 || !strings.Contains(body, "You're connected") {
		t.Fatalf("approve: %d %s", st, alertOf(body))
	}
	got := e.poll(device)
	if got["status"] != "approved" || got["token"] != "xu_token001" || got["relay_url"] != "https://xu.example.com" || got["name"] != "nia" {
		t.Fatalf("collect: %v", got)
	}
	if u, ok := e.relay.User("nia"); !ok || u.MaxApps != 5 {
		t.Fatal("relay user not created with 5 apps")
	}
	// Collected once only.
	if s := e.poll(device)["status"]; s != "expired" {
		t.Fatalf("second collect: %v", s)
	}
	// And emailed, as with Get token.
	for i := 0; i < 100 && !strings.Contains(e.mail.last("nia@example.com").Body, "xu_token001"); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(e.mail.last("nia@example.com").Body, "xu_token001") {
		t.Fatal("token not emailed")
	}
	if !strings.Contains(e.events(), "computer-approved") {
		t.Fatalf("audit: %s", e.events())
	}
}

func TestPairSecondComputerRegenerates(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	b.signUp("oz@example.com")
	b.post("/token", url.Values{"name": {"ozzy"}})
	e.relay.users["ozzy"].Online = true
	_, start := e.api("/api/pair/start", map[string]string{"device": "laptop"})
	device, code := start["device_code"].(string), start["user_code"].(string)
	_, body := b.get("/pair?code=" + url.QueryEscape(strings.ToLower(strings.ReplaceAll(code, "-", " "))))
	if !strings.Contains(body, "It will use your tunnel <code>ozzy</code>") || !strings.Contains(body, "will be disconnected") {
		t.Fatalf("page: %s", body)
	}
	b.post("/pair", url.Values{"code": {code}, "action": {"approve"}})
	got := e.poll(device)
	if got["status"] != "approved" || got["name"] != "ozzy" || got["token"] == "xu_token001" || e.relay.tokens["ozzy"] != got["token"] {
		t.Fatalf("collect: %v (relay has %s)", got, e.relay.tokens["ozzy"])
	}
}

func TestPairDenyExpireAndLimits(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	b.signUp("pat@example.com")
	_, start := e.api("/api/pair/start", map[string]string{"device": "evil-box"})
	device, code := start["device_code"].(string), start["user_code"].(string)
	// Too-fast polling is told to slow down.
	e.advance(pairInterval)
	e.api("/api/pair/poll", map[string]string{"device_code": device})
	if _, out := e.api("/api/pair/poll", map[string]string{"device_code": device}); out["status"] != "slow_down" {
		t.Fatalf("fast poll: %v", out)
	}
	_, body := b.post("/pair", url.Values{"code": {code}, "action": {"deny"}})
	if !strings.Contains(body, "Request denied") {
		t.Fatalf("deny: %s", alertOf(body))
	}
	if s := e.poll(device)["status"]; s != "denied" {
		t.Fatalf("after deny: %v", s)
	}
	if _, out := e.api("/api/pair/poll", map[string]string{"device_code": "made-up"}); out["status"] != "expired" && out["status"] != "slow_down" {
		t.Fatalf("unknown device: %v", out)
	}
	// Codes expire.
	_, start = e.api("/api/pair/start", map[string]string{"device": "slow"})
	e.advance(pairTTL + time.Second)
	if s := e.poll(start["device_code"].(string))["status"]; s != "expired" {
		t.Fatalf("expired: %v", s)
	}
	if _, body = b.get("/pair?code=" + start["user_code"].(string)); !strings.Contains(body, "isn't valid any more") {
		t.Fatalf("expired page: %s", alertOf(body))
	}
	// Typing a code by hand.
	_, start = e.api("/api/pair/start", map[string]string{"device": "typed"})
	if _, body = b.get("/pair"); !strings.Contains(body, "Enter the code your xu526 dashboard shows") {
		t.Fatalf("enter page: %s", alertOf(body))
	}
	if _, body = b.post("/pair", url.Values{"lookup": {"1"}, "code": {start["user_code"].(string)}}); !strings.Contains(body, "Connect typed?") {
		t.Fatalf("lookup: %s", alertOf(body))
	}
	// A taken name is caught at approval, not when the computer collects.
	e.relay.AddUser("taken", 5, 10)
	if _, body = b.post("/pair", url.Values{"code": {start["user_code"].(string)}, "action": {"approve"}, "name": {"taken"}}); !strings.Contains(body, "That name is taken") {
		t.Fatalf("taken name: %s", alertOf(body))
	}
	// The API isn't a browser form: no CSRF token needed, but nothing else is reachable there.
	if st, _ := e.api("/api/token", map[string]string{}); st != 404 {
		t.Fatalf("/api/token: %d", st)
	}
	if normalCode("bcdf-ghjk") != "BCDFGHJK" || normalCode("ABCD-EFGH") != "" || normalCode("BCD") != "" {
		t.Fatal("normalCode")
	}
}
