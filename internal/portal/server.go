// Package portal is the tunnel-token sign-up site: people create an
// account with email and password, confirm a code sent by email on every
// sign-in, and get a token for `xu tunnel connect`, which is emailed to
// them. Each token may publish a limited number of apps (5 by default).
package portal

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Relay is what the portal needs from the relay server.
type Relay interface {
	AddUser(name string, maxApps int, rateMBps float64) (token string, err error)
	RotateToken(name string) (token string, err error)
	User(name string) (RelayUser, bool)
	AgentURL() string // what `xu tunnel connect` dials
}

// RelayUser is a relay user's public state.
type RelayUser struct {
	Online  bool
	MaxApps int
	Apps    map[string]string // app → public URL
}

// Config describes the site.
type Config struct {
	Host     string // the portal's hostname, e.g. tunnels.example.com
	SiteName string // shown in pages and emails
	MaxApps  int    // public apps per token (5)
	RateMBps float64
	// Secure marks cookies Secure (with the __Host- prefix) and sends HSTS.
	// Only local testing over plain HTTP turns it off.
	Secure  bool
	CodeKey []byte // HMAC key for one-time codes; keep it out of the database
}

const (
	codeTTL        = 10 * time.Minute
	maxCodeTries   = 5
	pendingTTL     = 15 * time.Minute
	sessionTTL     = 7 * 24 * time.Hour
	maxFormBytes   = 16 << 10
	purposeLogin   = "login"
	purposeReset   = "reset"
	auditSignup    = "signup"
	auditSignin    = "signin"
	auditBadPass   = "password-wrong"
	auditBadCode   = "code-wrong"
	auditReset     = "password-reset"
	auditIssued    = "token-issued"
	auditRotated   = "token-regenerated"
	auditSignout   = "signout"
	auditCodeSent  = "code-sent"
	auditDupSignup = "signup-existing-email"
)

// Server is the portal's HTTP handler.
type Server struct {
	cfg   Config
	store Store
	mail  Mailer
	relay Relay
	log   *log.Logger
	now   func() time.Time
	pages *pages

	authByIP    *limiter // sign-in, sign-up, forgot-password posts per IP
	authByEmail *limiter // the same per email address
	codeSends   *limiter // codes emailed per account: a few per hour
	codeGap     *limiter // and at most one a minute
	tokenOps    *limiter // tokens issued or regenerated per account per day
	pairStarts  *limiter // computers asking to connect, per IP
	pollGap     *limiter // a computer checking for approval: once per interval
}

// New makes a portal server.
func New(cfg Config, store Store, mail Mailer, relay Relay, logger *log.Logger) (*Server, error) {
	if cfg.Host == "" || len(cfg.CodeKey) < 16 {
		return nil, errors.New("portal: Host and a CodeKey of at least 16 bytes are required")
	}
	if cfg.SiteName == "" {
		cfg.SiteName = "xu526 tunnel"
	}
	if cfg.MaxApps <= 0 {
		cfg.MaxApps = 5
	}
	if cfg.RateMBps <= 0 {
		cfg.RateMBps = 10
	}
	if logger == nil {
		logger = log.Default()
	}
	return &Server{
		cfg: cfg, store: store, mail: mail, relay: relay, log: logger, now: time.Now, pages: newPages(),
		authByIP:    newLimiter(30, 10*time.Minute),
		authByEmail: newLimiter(10, time.Hour),
		codeSends:   newLimiter(5, time.Hour),
		codeGap:     newLimiter(1, time.Minute),
		tokenOps:    newLimiter(10, 24*time.Hour),
		pairStarts:  newLimiter(20, time.Hour),
		pollGap:     newLimiter(1, pairInterval-500*time.Millisecond),
	}, nil
}

// SetClock replaces time.Now (tests).
func (s *Server) SetClock(now func() time.Time) { s.now = now }

// Run deletes expired sessions and codes until ctx ends.
func (s *Server) Run(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.store.DeleteExpired(ctx, s.now()); err != nil {
				s.log.Printf("portal: cleanup: %v", err)
			}
		}
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self' data:; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "same-origin")
	if s.cfg.Secure {
		h.Set("Strict-Transport-Security", "max-age=31536000")
	}
	if strings.HasPrefix(r.URL.Path, "/api/") {
		s.serveAPI(w, r)
		return
	}
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		if !s.sameOrigin(r) {
			http.Error(w, "This form was sent from another site.", http.StatusForbidden)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "That form couldn't be read.", http.StatusBadRequest)
			return
		}
		if c, err := r.Cookie(s.cookieName("csrf")); err != nil || r.PostFormValue("csrf") == "" || !sameString(c.Value, r.PostFormValue("csrf")) {
			http.Error(w, "This page expired. Go back, reload it and try again.", http.StatusForbidden)
			return
		}
	}
	switch r.Method + " " + r.URL.Path {
	case "GET /style.css":
		h.Set("Content-Type", "text/css; charset=utf-8")
		h.Set("Cache-Control", "public, max-age=3600")
		fmt.Fprint(w, styleCSS)
	case "GET /healthz":
		fmt.Fprintln(w, "ok")
	case "GET /":
		s.home(w, r)
	case "GET /signup":
		s.render(w, r, http.StatusOK, "signup", nil)
	case "POST /signup":
		s.signup(w, r)
	case "POST /signin":
		s.signin(w, r)
	case "GET /verify":
		s.verifyPage(w, r, "")
	case "POST /verify":
		s.verify(w, r)
	case "POST /resend":
		s.resend(w, r)
	case "GET /forgot":
		s.render(w, r, http.StatusOK, "forgot", nil)
	case "POST /forgot":
		s.forgot(w, r)
	case "GET /reset":
		s.resetPage(w, r, "")
	case "POST /reset":
		s.reset(w, r)
	case "POST /token":
		s.token(w, r)
	case "POST /signout":
		s.signout(w, r)
	case "GET /pair":
		s.pairPage(w, r, "", "")
	case "POST /pair":
		s.pairDecide(w, r)
	default:
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.render(w, r, http.StatusNotFound, "notfound", nil)
	}
}

// sameOrigin rejects cross-site form posts (with the CSRF cookie check as
// the second line of defence). Browsers send Origin on POST.
func (s *Server) sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" || o == "null" {
		return o == "" // non-browser clients; "null" is an opaque, untrusted origin
	}
	u, err := url.Parse(o)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func (s *Server) cookieName(n string) string {
	if s.cfg.Secure {
		return "__Host-xu526_" + n
	}
	return "xu526_" + n
}

func (s *Server) setCookie(w http.ResponseWriter, name, value string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{Name: s.cookieName(name), Value: value, Path: "/", HttpOnly: true,
		Secure: s.cfg.Secure, SameSite: http.SameSiteLaxMode, MaxAge: int(maxAge.Seconds())})
}

func (s *Server) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: s.cookieName(name), Value: "", Path: "/", HttpOnly: true,
		Secure: s.cfg.Secure, SameSite: http.SameSiteLaxMode, MaxAge: -1})
}

// csrf returns the form token, setting its cookie the first time.
func (s *Server) csrf(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(s.cookieName("csrf")); err == nil && len(c.Value) >= 32 {
		return c.Value
	}
	v, _ := randomID()
	s.setCookie(w, "csrf", v, 30*24*time.Hour)
	return v
}

func clientIP(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

func (s *Server) audit(r *http.Request, account int64, event, detail string) {
	if err := s.store.Audit(r.Context(), AuditEntry{At: s.now(), AccountID: account, Event: event, IP: clientIP(r), Detail: detail}); err != nil {
		s.log.Printf("portal: audit %s: %v", event, err)
	}
}

// session returns the browser's session if it's valid and at stage.
func (s *Server) session(r *http.Request, stages ...string) *Session {
	c, err := r.Cookie(s.cookieName("session"))
	if err != nil || c.Value == "" {
		return nil
	}
	sess, err := s.store.GetSession(r.Context(), sha256Hex(c.Value))
	if err != nil || !s.now().Before(sess.Expires) {
		return nil
	}
	for _, st := range stages {
		if sess.Stage == st {
			return sess
		}
	}
	return nil
}

// startSession replaces the browser's session with a new one (a new ID at
// every step, so a planted session ID never becomes a signed-in one).
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, accountID int64, stage string, ttl time.Duration) error {
	if c, err := r.Cookie(s.cookieName("session")); err == nil {
		s.store.DeleteSession(r.Context(), sha256Hex(c.Value))
	}
	id, err := randomID()
	if err != nil {
		return err
	}
	if err := s.store.PutSession(r.Context(), &Session{IDHash: sha256Hex(id), AccountID: accountID, Stage: stage, Expires: s.now().Add(ttl)}); err != nil {
		return err
	}
	s.setCookie(w, "session", id, ttl)
	return nil
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Printf("portal: %s %s: %v", r.Method, r.URL.Path, err)
	s.render(w, r, http.StatusInternalServerError, "error", map[string]any{"Message": "Something went wrong on our side. Try again in a minute."})
}

func (s *Server) limited(w http.ResponseWriter, r *http.Request, page string, wait time.Duration, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	mins := int(wait.Minutes()) + 1
	data["Error"] = fmt.Sprintf("Too many attempts. Try again in %d minute%s.", mins, plural(mins))
	s.render(w, r, http.StatusTooManyRequests, page, data)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// ---- sign-up and sign-in ----

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	if sess := s.session(r, StageFull); sess != nil {
		s.dashboard(w, r, sess, nil)
		return
	}
	s.render(w, r, http.StatusOK, "signin", nil)
}

func (s *Server) signup(w http.ResponseWriter, r *http.Request) {
	ctx, now, ip := r.Context(), s.now(), clientIP(r)
	form := map[string]any{"Email": r.PostFormValue("email")}
	if ok, wait := s.authByIP.allow(ip, now); !ok {
		s.limited(w, r, "signup", wait, form)
		return
	}
	email, err := normalEmail(r.PostFormValue("email"))
	if err != nil {
		form["Error"] = err.Error()
		s.render(w, r, http.StatusBadRequest, "signup", form)
		return
	}
	pw := r.PostFormValue("password")
	if p := passwordProblem(pw, email); p != "" {
		form["Error"] = p
		s.render(w, r, http.StatusBadRequest, "signup", form)
		return
	}
	if pw != r.PostFormValue("confirm") {
		form["Error"] = "The two passwords don't match."
		s.render(w, r, http.StatusBadRequest, "signup", form)
		return
	}
	if ok, wait := s.authByEmail.allow(email, now); !ok {
		s.limited(w, r, "signup", wait, form)
		return
	}
	hash, err := hashPassword(pw)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	acct, err := s.store.AccountByEmail(ctx, email)
	switch {
	case errors.Is(err, ErrNotFound):
		acct, err = s.store.CreateAccount(ctx, email, hash, now)
		if errors.Is(err, ErrExists) { // signed up at the same moment
			acct, err = s.store.AccountByEmail(ctx, email)
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		s.audit(r, acct.ID, auditSignup, "")
	case err != nil:
		s.fail(w, r, err)
		return
	case !acct.Verified:
		// Never confirmed: whoever proves they own the address with the
		// code gets it, with the password they chose now.
		if err := s.store.SetPassword(ctx, acct.ID, hash); err != nil {
			s.fail(w, r, err)
			return
		}
		s.audit(r, acct.ID, auditSignup, "again, before confirming")
	default:
		// The address already has an account. The page looks the same
		// either way (so it doesn't reveal who has an account); the owner
		// gets an email instead of a code, and the decoy session can't be
		// confirmed.
		s.audit(r, acct.ID, auditDupSignup, "")
		go s.sendMail(acct.Email, "Someone tried to sign up with your email",
			fmt.Sprintf("Someone (maybe you) tried to create a %s account with this email address, which already has one.\n\n"+
				"If it was you, sign in at https://%s/ instead, or reset your password at https://%s/forgot.\n"+
				"If it wasn't, you don't need to do anything: your account hasn't changed.\n", s.cfg.SiteName, s.cfg.Host, s.cfg.Host))
		if err := s.startSession(w, r, 0, StageCode, pendingTTL); err != nil {
			s.fail(w, r, err)
			return
		}
		http.Redirect(w, r, "/verify", http.StatusSeeOther)
		return
	}
	if err := s.startSession(w, r, acct.ID, StageCode, pendingTTL); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.sendCode(r, acct, purposeLogin); err != nil {
		s.log.Printf("portal: code for %d: %v", acct.ID, err)
	}
	http.Redirect(w, r, "/verify", http.StatusSeeOther)
}

func (s *Server) signin(w http.ResponseWriter, r *http.Request) {
	ctx, now, ip := r.Context(), s.now(), clientIP(r)
	form := map[string]any{"Email": r.PostFormValue("email")}
	if ok, wait := s.authByIP.allow(ip, now); !ok {
		s.limited(w, r, "signin", wait, form)
		return
	}
	email, err := normalEmail(r.PostFormValue("email"))
	if err != nil {
		form["Error"] = err.Error()
		s.render(w, r, http.StatusBadRequest, "signin", form)
		return
	}
	if ok, wait := s.authByEmail.allow(email, now); !ok {
		s.limited(w, r, "signin", wait, form)
		return
	}
	pw := r.PostFormValue("password")
	acct, err := s.store.AccountByEmail(ctx, email)
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.fail(w, r, err)
		return
	}
	if acct == nil {
		checkPassword(pw, dummyHash) // same time as a wrong password
		form["Error"] = "That email and password don't match an account."
		s.render(w, r, http.StatusUnauthorized, "signin", form)
		return
	}
	if !checkPassword(pw, acct.PasswordHash) {
		s.audit(r, acct.ID, auditBadPass, "")
		form["Error"] = "That email and password don't match an account."
		s.render(w, r, http.StatusUnauthorized, "signin", form)
		return
	}
	if err := s.startSession(w, r, acct.ID, StageCode, pendingTTL); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.sendCode(r, acct, purposeLogin); err != nil {
		s.log.Printf("portal: code for %d: %v", acct.ID, err)
	}
	http.Redirect(w, r, "/verify", http.StatusSeeOther)
}

// sendCode emails a new code, replacing any earlier one.
func (s *Server) sendCode(r *http.Request, acct *Account, purpose string) error {
	now := s.now()
	if ok, _ := s.codeGap.allow(fmt.Sprint(acct.ID, "/", purpose), now); !ok {
		return nil // one was sent less than a minute ago; it still works
	}
	if ok, _ := s.codeSends.allow(fmt.Sprint(acct.ID), now); !ok {
		return errors.New("too many codes this hour")
	}
	code, err := newCode()
	if err != nil {
		return err
	}
	c := &Code{AccountID: acct.ID, Purpose: purpose, MAC: codeMAC(s.cfg.CodeKey, acct.ID, purpose, code), Expires: now.Add(codeTTL)}
	if err := s.store.PutCode(r.Context(), c); err != nil {
		return err
	}
	s.audit(r, acct.ID, auditCodeSent, purpose)
	subject := code + " is your " + s.cfg.SiteName + " code"
	what := "sign in"
	if purpose == purposeReset {
		what = "reset your password"
	} else if !acct.Verified {
		what = "confirm your email address"
	}
	body := fmt.Sprintf("Your code to %s is:\n\n    %s\n\nIt works once and expires in %d minutes.\n\n"+
		"If you didn't ask for it, someone typed your email address by mistake or knows your password. "+
		"Nobody can sign in without this code; if you think your password is known, reset it at https://%s/forgot.\n",
		what, code, int(codeTTL.Minutes()), s.cfg.Host)
	// Sent in the background: the page answers at the same speed whether or
	// not the address has an account (the decoy paths send nothing or send
	// in the background too), and a slow mail server doesn't hold it up.
	go s.sendMail(acct.Email, subject, body)
	return nil
}

func (s *Server) sendMail(to, subject, body string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := s.mail.Send(ctx, to, subject, body); err != nil {
		s.log.Printf("portal: email to %s: %v", to, err)
	}
}

func (s *Server) verifyPage(w http.ResponseWriter, r *http.Request, msg string) {
	sess := s.session(r, StageCode)
	if sess == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "verify", map[string]any{"Error": msg})
}

func (s *Server) verify(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r, StageCode)
	if sess == nil {
		s.render(w, r, http.StatusUnauthorized, "signin", map[string]any{"Error": "That sign-in took too long. Sign in again."})
		return
	}
	ok, msg, err := s.checkCode(r, sess, purposeLogin)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !ok {
		s.render(w, r, http.StatusUnauthorized, "verify", map[string]any{"Error": msg})
		return
	}
	if err := s.store.SetVerified(r.Context(), sess.AccountID); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.startSession(w, r, sess.AccountID, StageFull, sessionTTL); err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, sess.AccountID, auditSignin, "")
	s.afterSignIn(w, r)
}

// checkCode compares the submitted code. Every try is counted before the
// comparison, atomically in the store, so parallel guesses can't get past
// the limit; after maxCodeTries the code is void and a new one must be sent.
func (s *Server) checkCode(r *http.Request, sess *Session, purpose string) (bool, string, error) {
	ctx := r.Context()
	if ok, wait := s.authByIP.allow(clientIP(r), s.now()); !ok {
		return false, fmt.Sprintf("Too many attempts. Try again in %d minutes.", int(wait.Minutes())+1), nil
	}
	submitted := strings.Join(strings.Fields(r.PostFormValue("code")), "")
	if sess.AccountID == 0 { // decoy: no code will ever match
		return false, "That code isn't right. Check the latest email we sent.", nil
	}
	c, err := s.store.GetCode(ctx, sess.AccountID, purpose)
	if errors.Is(err, ErrNotFound) {
		return false, "There's no active code. Send a new one.", nil
	}
	if err != nil {
		return false, "", err
	}
	if !s.now().Before(c.Expires) {
		s.store.DeleteCode(ctx, sess.AccountID, purpose)
		return false, "That code has expired. Send a new one.", nil
	}
	n, err := s.store.AddCodeAttempt(ctx, sess.AccountID, purpose)
	if errors.Is(err, ErrNotFound) {
		return false, "There's no active code. Send a new one.", nil
	}
	if err != nil {
		return false, "", err
	}
	if n > maxCodeTries {
		s.store.DeleteCode(ctx, sess.AccountID, purpose)
		return false, "Too many wrong codes. Send a new one.", nil
	}
	if len(submitted) != 6 || !sameString(codeMAC(s.cfg.CodeKey, sess.AccountID, purpose, submitted), c.MAC) {
		s.audit(r, sess.AccountID, auditBadCode, purpose)
		if n >= maxCodeTries {
			s.store.DeleteCode(ctx, sess.AccountID, purpose)
			return false, "Too many wrong codes. Send a new one.", nil
		}
		left := maxCodeTries - n
		return false, fmt.Sprintf("That code isn't right. %d tr%s left.", left, map[bool]string{true: "y", false: "ies"}[left == 1]), nil
	}
	return true, "", s.store.DeleteCode(ctx, sess.AccountID, purpose)
}

func (s *Server) resend(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r, StageCode, StageReset)
	if sess == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	page, purpose := "verify", purposeLogin
	if sess.Stage == StageReset {
		page, purpose = "reset", purposeReset
	}
	if sess.AccountID != 0 {
		acct, err := s.store.Account(r.Context(), sess.AccountID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if ok, wait := s.codeGap.allow(fmt.Sprint(acct.ID)+"/resend", s.now()); !ok {
			s.render(w, r, http.StatusTooManyRequests, page, map[string]any{"Error": fmt.Sprintf("A code was just sent. You can ask for another in %d seconds.", int(wait.Seconds())+1)})
			return
		}
		// A deliberate resend skips the one-a-minute gap of sendCode.
		s.codeGap.reset(fmt.Sprint(acct.ID, "/", purpose))
		if err := s.sendCode(r, acct, purpose); err != nil {
			s.render(w, r, http.StatusTooManyRequests, page, map[string]any{"Error": "Too many codes were sent. Wait an hour and try again."})
			return
		}
	}
	s.render(w, r, http.StatusOK, page, map[string]any{"Notice": "We sent a new code. Codes sent before it no longer work."})
}

// ---- forgotten passwords ----

func (s *Server) forgot(w http.ResponseWriter, r *http.Request) {
	ctx, now := r.Context(), s.now()
	form := map[string]any{"Email": r.PostFormValue("email")}
	if ok, wait := s.authByIP.allow(clientIP(r), now); !ok {
		s.limited(w, r, "forgot", wait, form)
		return
	}
	email, err := normalEmail(r.PostFormValue("email"))
	if err != nil {
		form["Error"] = err.Error()
		s.render(w, r, http.StatusBadRequest, "forgot", form)
		return
	}
	if ok, wait := s.authByEmail.allow(email, now); !ok {
		s.limited(w, r, "forgot", wait, form)
		return
	}
	acct, err := s.store.AccountByEmail(ctx, email)
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.fail(w, r, err)
		return
	}
	var id int64 // 0: no such account; the page looks the same
	if acct != nil {
		id = acct.ID
	}
	if err := s.startSession(w, r, id, StageReset, pendingTTL); err != nil {
		s.fail(w, r, err)
		return
	}
	if acct != nil {
		if err := s.sendCode(r, acct, purposeReset); err != nil {
			s.log.Printf("portal: reset code for %d: %v", acct.ID, err)
		}
	}
	http.Redirect(w, r, "/reset", http.StatusSeeOther)
}

func (s *Server) resetPage(w http.ResponseWriter, r *http.Request, msg string) {
	if s.session(r, StageReset) == nil {
		http.Redirect(w, r, "/forgot", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "reset", map[string]any{"Error": msg})
}

func (s *Server) reset(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r, StageReset)
	if sess == nil {
		s.render(w, r, http.StatusUnauthorized, "forgot", map[string]any{"Error": "That took too long. Start again."})
		return
	}
	pw := r.PostFormValue("password")
	if pw != r.PostFormValue("confirm") {
		s.render(w, r, http.StatusBadRequest, "reset", map[string]any{"Error": "The two passwords don't match."})
		return
	}
	email := ""
	if sess.AccountID != 0 {
		if a, err := s.store.Account(r.Context(), sess.AccountID); err == nil {
			email = a.Email
		}
	}
	if p := passwordProblem(pw, email); p != "" {
		s.render(w, r, http.StatusBadRequest, "reset", map[string]any{"Error": p})
		return
	}
	ok, msg, err := s.checkCode(r, sess, purposeReset)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !ok {
		s.render(w, r, http.StatusUnauthorized, "reset", map[string]any{"Error": msg})
		return
	}
	hash, err := hashPassword(pw)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ctx := r.Context()
	if err := s.store.SetPassword(ctx, sess.AccountID, hash); err != nil {
		s.fail(w, r, err)
		return
	}
	s.store.SetVerified(ctx, sess.AccountID) // the code proved the address
	if err := s.store.DeleteSessions(ctx, sess.AccountID); err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, sess.AccountID, auditReset, "all sessions signed out")
	if err := s.startSession(w, r, sess.AccountID, StageFull, sessionTTL); err != nil {
		s.fail(w, r, err)
		return
	}
	s.afterSignIn(w, r)
}

func (s *Server) signout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(s.cookieName("session")); err == nil {
		if sess, err := s.store.GetSession(r.Context(), sha256Hex(c.Value)); err == nil {
			s.audit(r, sess.AccountID, auditSignout, "")
		}
		s.store.DeleteSession(r.Context(), sha256Hex(c.Value))
	}
	s.clearCookie(w, "session")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ---- tokens ----

var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{1,18}[a-z0-9])$`)

// reserved names would make confusing or misleading addresses.
var reserved = map[string]bool{"xu": true, "www": true, "api": true, "admin": true, "root": true, "mail": true, "smtp": true,
	"portal": true, "tunnel": true, "tunnels": true, "relay": true, "support": true, "help": true, "login": true, "signin": true,
	"account": true, "billing": true, "status": true, "security": true, "official": true, "staff": true}

func suggestName(email string) string {
	local := email[:strings.Index(email, "@")]
	var b strings.Builder
	for _, r := range strings.ToLower(local) {
		if b.Len() >= 16 {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	n := strings.Trim(b.String(), "-")
	if len(n) < 3 {
		n += "user"
	}
	return n
}

type dashboardData struct {
	Email     string
	Token     string // only right after it was made
	Name      string
	Suggested string
	HasToken  bool
	Removed   bool // the relay user was deleted by the admin
	Online    bool
	Apps      [][2]string
	Used, Max int
	Issued    time.Time
	Rotated   time.Time
	Connect   string
	Domain    string
	MailError bool
	Error     string
	Notice    string
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request, sess *Session, d *dashboardData) {
	if d == nil {
		d = &dashboardData{}
	}
	acct, err := s.store.Account(r.Context(), sess.AccountID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d.Email, d.Max, d.Connect = acct.Email, s.cfg.MaxApps, s.relay.AgentURL()
	d.Suggested = suggestName(acct.Email)
	if u, err := url.Parse(d.Connect); err == nil {
		d.Domain = strings.TrimPrefix(u.Hostname(), "xu.")
	}
	t, err := s.store.GetToken(r.Context(), acct.ID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.fail(w, r, err)
		return
	}
	if t != nil {
		d.Name, d.Issued, d.Rotated = t.RelayUser, t.Issued, t.Rotated
		if u, ok := s.relay.User(t.RelayUser); ok {
			d.HasToken, d.Online, d.Used = true, u.Online, len(u.Apps)
			if u.MaxApps > 0 {
				d.Max = u.MaxApps
			}
			for app, link := range u.Apps {
				d.Apps = append(d.Apps, [2]string{app, link})
			}
		} else {
			d.Removed = true
		}
	}
	status := http.StatusOK
	if d.Error != "" {
		status = http.StatusBadRequest
	}
	s.render(w, r, status, "dashboard", map[string]any{"D": d, "Error": d.Error, "Notice": d.Notice})
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	sess := s.session(r, StageFull)
	if sess == nil {
		s.render(w, r, http.StatusUnauthorized, "signin", map[string]any{"Error": "Sign in first."})
		return
	}
	ctx := r.Context()
	acct, err := s.store.Account(ctx, sess.AccountID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if ok, _ := s.tokenOps.allow(fmt.Sprint(acct.ID), s.now()); !ok {
		s.dashboard(w, r, sess, &dashboardData{Error: "You've made too many tokens today. Try again tomorrow."})
		return
	}
	token, relayUser, event, problem, err := s.issueToken(ctx, acct, r.PostFormValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if problem != "" {
		s.dashboard(w, r, sess, &dashboardData{Error: problem})
		return
	}
	s.audit(r, acct.ID, event, relayUser)
	mailErr := s.mailToken(ctx, acct, relayUser, token)
	w.Header().Set("Cache-Control", "no-store")
	s.dashboard(w, r, sess, &dashboardData{Token: token, MailError: mailErr != nil})
}

// nameProblem checks a requested relay name ("" when it can be used).
// An account whose relay user was removed may take its old name back.
func (s *Server) nameProblem(ctx context.Context, name string, existing *Token) (string, error) {
	switch {
	case !nameRE.MatchString(name):
		return "Names are 3 to 20 lowercase letters, digits and dashes, and can't start or end with a dash.", nil
	case reserved[name]:
		return "That name is reserved. Choose another.", nil
	}
	taken, err := s.store.RelayUserTaken(ctx, name)
	if err != nil {
		return "", err
	}
	if _, exists := s.relay.User(name); exists || (taken && (existing == nil || existing.RelayUser != name)) {
		return "That name is taken. Choose another.", nil
	}
	return "", nil
}

// issueToken gives acct a token. With a relay user already, it regenerates
// (same name and addresses); otherwise it creates one named name. problem
// is a message for the person when the name can't be used.
func (s *Server) issueToken(ctx context.Context, acct *Account, name string) (token, relayUser, event, problem string, err error) {
	existing, err := s.store.GetToken(ctx, acct.ID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return "", "", "", "", err
	}
	now := s.now()
	if existing != nil {
		if _, ok := s.relay.User(existing.RelayUser); ok {
			if token, err = s.relay.RotateToken(existing.RelayUser); err != nil {
				return "", "", "", "", err
			}
			existing.Rotated = now
			if err := s.store.PutToken(ctx, existing); err != nil {
				return "", "", "", "", err
			}
			return token, existing.RelayUser, auditRotated, "", nil
		}
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if existing != nil && name == "" {
		name = existing.RelayUser // removed by the admin; take the name back
	}
	if problem, err := s.nameProblem(ctx, name, existing); problem != "" || err != nil {
		return "", "", "", problem, err
	}
	if token, err = s.relay.AddUser(name, s.cfg.MaxApps, s.cfg.RateMBps); err != nil {
		return "", "", "", "That name can't be used: " + err.Error(), nil
	}
	if err := s.store.PutToken(ctx, &Token{AccountID: acct.ID, RelayUser: name, Issued: now}); err != nil {
		return "", "", "", "", err
	}
	return token, name, auditIssued, "", nil
}

// mailToken emails a token to its owner (as asked: the token always
// arrives by email too).
func (s *Server) mailToken(ctx context.Context, acct *Account, relayUser, token string) error {
	connect := fmt.Sprintf("xu tunnel connect %s %s", s.relay.AgentURL(), token)
	body := fmt.Sprintf("Here is your %s token for the name %q.\n\n"+
		"If you connected from the xu526 dashboard, your computer already has it and there's nothing more to do. "+
		"To connect a computer by hand, run:\n\n    %s\n\n"+
		"Shared apps get addresses like https://APP-%s.%s. You can share up to %d apps at a time.\n\n"+
		"Keep this token secret: anyone who has it can publish apps under your name. If it leaks, sign in at https://%s/ and choose Regenerate token; the old one stops working at once.\n",
		s.cfg.SiteName, relayUser, connect, relayUser, domainOf(s.relay.AgentURL()), s.cfg.MaxApps, s.cfg.Host)
	err := s.mail.Send(ctx, acct.Email, "Your "+s.cfg.SiteName+" token", body)
	if err != nil {
		s.log.Printf("portal: token email to %d: %v", acct.ID, err)
	}
	return err
}

func domainOf(agentURL string) string {
	u, err := url.Parse(agentURL)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(u.Hostname(), "xu.")
}
