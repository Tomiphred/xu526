package portal

// Connecting a computer without copying a token (the "TV sign-in" pattern,
// like OAuth's device flow):
//
//  1. The computer's xud calls POST /api/pair/start and gets a secret device
//     code plus a short user code, which its dashboard shows.
//  2. The person opens /pair?code=…, signs in (or signs up), checks the code
//     matches, and approves.
//  3. The computer, polling POST /api/pair/poll with its device code,
//     receives the token once and connects.
//
// The token never appears in a URL, and the site never stores it.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode"
)

const (
	pairTTL      = 15 * time.Minute
	pairInterval = 3 * time.Second
	// Consonants only: no confusable characters, no accidental words.
	pairAlphabet = "BCDFGHJKLMNPQRSTVWXZ"
	pairLen      = 8
)

func newUserCode() (string, error) {
	b := make([]byte, pairLen)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(pairAlphabet))))
		if err != nil {
			return "", err
		}
		b[i] = pairAlphabet[n.Int64()]
	}
	return string(b), nil
}

// normalCode accepts "kjwx-bmpq", "KJWX BMPQ" and the like.
func normalCode(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if strings.ContainsRune(pairAlphabet, r) {
			b.WriteRune(r)
		} else if !unicode.IsSpace(r) && r != '-' {
			return "" // a character no code has
		}
	}
	if b.Len() != pairLen {
		return ""
	}
	return b.String()
}

func showCode(c string) string {
	if len(c) != pairLen {
		return c
	}
	return c[:4] + "-" + c[4:]
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// serveAPI answers the computer-facing JSON endpoints. They use no cookies,
// so the browser-form protections don't apply; the device code is the
// credential.
func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "use POST"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	switch r.URL.Path {
	case "/api/pair/start":
		s.pairStart(w, r)
	case "/api/pair/poll":
		s.pairPoll(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func (s *Server) pairStart(w http.ResponseWriter, r *http.Request) {
	ctx, now := r.Context(), s.now()
	if ok, wait := s.pairStarts.allow(clientIP(r), now); !ok {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many connection requests from this address", "retry_after": int(wait.Seconds()) + 1})
		return
	}
	var req struct {
		Device string `json:"device"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	device := cleanDeviceName(req.Device)
	secret, err := randomID()
	if err != nil {
		s.apiFail(w, r, err)
		return
	}
	p := &Pairing{DeviceHash: sha256Hex(secret), Device: device, IP: clientIP(r), Created: now, Expires: now.Add(pairTTL), Status: PairPending}
	for i := 0; ; i++ {
		if p.UserCode, err = newUserCode(); err != nil {
			s.apiFail(w, r, err)
			return
		}
		err = s.store.PutPairing(ctx, p)
		if !errors.Is(err, ErrExists) || i == 5 {
			break
		}
	}
	if err != nil {
		s.apiFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"device_code": secret,
		"user_code":   showCode(p.UserCode),
		"verify_url":  "https://" + s.publicHost(r) + "/pair?code=" + showCode(p.UserCode),
		"interval":    int(pairInterval.Seconds()),
		"expires_in":  int(pairTTL.Seconds()),
		"site":        s.cfg.SiteName,
	})
}

// cleanDeviceName keeps a computer's self-reported name printable and short.
func cleanDeviceName(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		if unicode.IsPrint(r) && b.Len() < 60 {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "a computer"
	}
	return b.String()
}

func (s *Server) pairPoll(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req struct {
		DeviceCode string `json:"device_code"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.DeviceCode == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "device_code is required"})
		return
	}
	h := sha256Hex(req.DeviceCode)
	if ok, _ := s.pollGap.allow(h, s.now()); !ok {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"status": "slow_down", "interval": int(pairInterval.Seconds())})
		return
	}
	p, err := s.store.PairingByDevice(ctx, h)
	if errors.Is(err, ErrNotFound) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "expired"})
		return
	}
	if err != nil {
		s.apiFail(w, r, err)
		return
	}
	if !s.now().Before(p.Expires) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "expired"})
		return
	}
	switch p.Status {
	case PairPending:
		writeJSON(w, http.StatusOK, map[string]string{"status": "pending"})
		return
	case PairDenied:
		writeJSON(w, http.StatusOK, map[string]string{"status": "denied"})
		return
	case PairClaimed:
		writeJSON(w, http.StatusOK, map[string]string{"status": "expired"}) // collected already
		return
	}
	// Approved: claim it exactly once, then make the token.
	if ok, err := s.store.SetPairingStatus(ctx, h, PairApproved, PairClaimed, 0, ""); err != nil || !ok {
		if err != nil {
			s.apiFail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "expired"})
		return
	}
	acct, err := s.store.Account(ctx, p.AccountID)
	if err != nil {
		s.apiFail(w, r, err)
		return
	}
	token, relayUser, event, problem, err := s.issueToken(ctx, acct, p.Name)
	if err != nil {
		s.apiFail(w, r, err)
		return
	}
	if problem != "" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "error", "error": problem + " Start connecting again from your dashboard."})
		return
	}
	s.audit(r, acct.ID, event, relayUser+" for "+p.Device)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		s.mailToken(ctx, acct, relayUser, token)
	}()
	writeJSON(w, http.StatusOK, map[string]string{"status": "approved", "relay_url": s.relay.AgentURL(), "token": token, "name": relayUser})
}

func (s *Server) apiFail(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Printf("portal: %s: %v", r.URL.Path, err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "something went wrong on the server; try again in a minute"})
}

// ---- the approval page ----

// pairPage shows a computer's request for the signed-in person to approve.
// Signed out, it remembers the code and sends them to sign in or sign up.
func (s *Server) pairPage(w http.ResponseWriter, r *http.Request, code string, errMsg string) {
	if code == "" {
		code = normalCode(r.URL.Query().Get("code"))
	}
	sess := s.session(r, StageFull)
	if sess == nil {
		if code != "" {
			s.setCookie(w, "pair", code, pairTTL)
		}
		s.render(w, r, http.StatusOK, "signin", map[string]any{"Notice": "Sign in, or create an account, to connect your computer to " + s.cfg.SiteName + "."})
		return
	}
	data := map[string]any{"Error": errMsg, "Code": showCode(code)}
	if code == "" {
		s.render(w, r, http.StatusOK, "pair", data)
		return
	}
	p, err := s.store.PairingByUserCode(r.Context(), code)
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.fail(w, r, err)
		return
	}
	if p == nil || !s.now().Before(p.Expires) || p.Status != PairPending {
		s.clearCookie(w, "pair")
		data["Error"] = "That code isn't valid any more. Start connecting again from your xu526 dashboard to get a new one."
		data["Code"] = ""
		s.render(w, r, http.StatusNotFound, "pair", data)
		return
	}
	acct, err := s.store.Account(r.Context(), sess.AccountID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	data["Device"], data["IP"] = p.Device, p.IP
	data["Ago"] = agoText(s.now().Sub(p.Created))
	data["Domain"] = domainOf(s.relay.AgentURL())
	data["Suggested"] = suggestName(acct.Email)
	if t, err := s.store.GetToken(r.Context(), acct.ID); err == nil {
		if u, ok := s.relay.User(t.RelayUser); ok {
			data["Name"] = t.RelayUser
			data["Replaces"] = u.Online
		} else {
			data["Suggested"] = t.RelayUser
		}
	}
	if n := r.PostFormValue("name"); n != "" {
		data["Suggested"] = n
	}
	s.render(w, r, http.StatusOK, "pair", data)
}

func agoText(d time.Duration) string {
	switch m := int(d.Minutes()); {
	case m < 1:
		return "just now"
	case m == 1:
		return "1 minute ago"
	default:
		return fmt.Sprintf("%d minutes ago", m)
	}
}

// pairDecide approves or denies a request.
func (s *Server) pairDecide(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	code := normalCode(r.PostFormValue("code"))
	sess := s.session(r, StageFull)
	if sess == nil {
		s.pairPage(w, r, code, "")
		return
	}
	if r.PostFormValue("lookup") != "" { // the "enter the code" form
		s.pairPage(w, r, code, map[bool]string{true: "Enter the 8-letter code your dashboard shows.", false: ""}[code == ""])
		return
	}
	p, err := s.store.PairingByUserCode(ctx, code)
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.fail(w, r, err)
		return
	}
	if p == nil || !s.now().Before(p.Expires) || p.Status != PairPending {
		s.pairPage(w, r, code, "")
		return
	}
	s.clearCookie(w, "pair")
	if r.PostFormValue("action") == "deny" {
		s.store.SetPairingStatus(ctx, p.DeviceHash, PairPending, PairDenied, 0, "")
		s.audit(r, sess.AccountID, "computer-denied", p.Device)
		s.render(w, r, http.StatusOK, "paired", map[string]any{"Denied": true, "Device": p.Device})
		return
	}
	acct, err := s.store.Account(ctx, sess.AccountID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if ok, _ := s.tokenOps.allow(fmt.Sprint(acct.ID), s.now()); !ok {
		s.pairPage(w, r, code, "You've connected too many computers today. Try again tomorrow.")
		return
	}
	// A first token needs a name; check it now so the computer doesn't
	// collect an error later.
	name := ""
	existing, err := s.store.GetToken(ctx, acct.ID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.fail(w, r, err)
		return
	}
	hasUser := false
	if existing != nil {
		_, hasUser = s.relay.User(existing.RelayUser)
	}
	if !hasUser {
		name = strings.ToLower(strings.TrimSpace(r.PostFormValue("name")))
		if name == "" && existing != nil {
			name = existing.RelayUser
		}
		problem, err := s.nameProblem(ctx, name, existing)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if problem != "" {
			s.pairPage(w, r, code, problem)
			return
		}
	}
	ok, err := s.store.SetPairingStatus(ctx, p.DeviceHash, PairPending, PairApproved, acct.ID, name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !ok {
		s.pairPage(w, r, code, "")
		return
	}
	s.audit(r, acct.ID, "computer-approved", p.Device)
	s.render(w, r, http.StatusOK, "paired", map[string]any{"Device": p.Device})
}

// afterSignIn sends someone who arrived to connect a computer back to it.
func (s *Server) afterSignIn(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(s.cookieName("pair")); err == nil && normalCode(c.Value) != "" {
		http.Redirect(w, r, "/pair?code="+showCode(normalCode(c.Value)), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// publicHost is the site's host as this request reached it: the configured
// name, keeping a non-standard port (e.g. tunnel.example.com:8443).
func (s *Server) publicHost(r *http.Request) string {
	h, port, err := net.SplitHostPort(r.Host)
	if err == nil && strings.EqualFold(h, s.cfg.Host) && port != "443" {
		return net.JoinHostPort(s.cfg.Host, port)
	}
	return s.cfg.Host
}
