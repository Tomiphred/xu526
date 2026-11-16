package portal

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Passwords: PBKDF2-HMAC-SHA256 with a random salt (Go's standard library,
// no extra dependency), at OWASP's recommended iteration count.
const (
	pbkdf2Iter   = 600_000
	pbkdf2KeyLen = 32
	minPassword  = 10
	maxPassword  = 128
)

func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, pbkdf2KeyLen)
	if err != nil {
		return "", err
	}
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iter, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// checkPassword compares in constant time. A malformed hash never matches.
func checkPassword(pw, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 || iter > 10_000_000 {
		return false
	}
	b64 := base64.RawStdEncoding
	salt, err1 := b64.DecodeString(parts[2])
	want, err2 := b64.DecodeString(parts[3])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash is checked against when an email has no account, so a wrong
// email takes as long as a wrong password.
var dummyHash = func() string {
	h, _ := hashPassword("not a real password, only spends time")
	return h
}()

func passwordProblem(pw, email string) string {
	n := utf8.RuneCountInString(pw)
	switch {
	case n < minPassword:
		return fmt.Sprintf("Use at least %d characters for your password.", minPassword)
	case n > maxPassword:
		return fmt.Sprintf("Use at most %d characters for your password.", maxPassword)
	case strings.EqualFold(pw, email):
		return "Your password can't be your email address."
	}
	return ""
}

// normalEmail validates an address and lowercases it. Display names,
// newlines and anything mail.ParseAddress would rewrite are refused.
func normalEmail(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 254 || strings.ContainsAny(s, "\r\n<>\" ,") {
		return "", errors.New("Enter a valid email address.")
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || a.Name != "" {
		return "", errors.New("Enter a valid email address.")
	}
	at := strings.LastIndex(s, "@")
	if at < 1 || !strings.Contains(s[at+1:], ".") {
		return "", errors.New("Enter a valid email address.")
	}
	return strings.ToLower(s), nil
}

// newCode is a 6-digit code from crypto/rand.
func newCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// codeMAC binds a code to its account and purpose with the server's key:
// six digits are easy to brute-force from a plain hash in a leaked database,
// not from an HMAC whose key isn't in it.
func codeMAC(key []byte, accountID int64, purpose, code string) string {
	m := hmac.New(sha256.New, key)
	fmt.Fprintf(m, "%d|%s|%s", accountID, purpose, code)
	return hex.EncodeToString(m.Sum(nil))
}

func randomID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func sameString(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// limiter counts events per key in fixed windows (one process, one server).
type limiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string]*window
	swept  time.Time
}

type window struct {
	start time.Time
	n     int
}

func newLimiter(limit int, per time.Duration) *limiter {
	return &limiter{limit: limit, window: per, hits: map[string]*window{}}
}

// allow records an event and reports whether it's within the limit, and if
// not, how long until it is.
func (l *limiter) allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.swept) > l.window {
		for k, w := range l.hits {
			if now.Sub(w.start) >= l.window {
				delete(l.hits, k)
			}
		}
		l.swept = now
	}
	w := l.hits[key]
	if w == nil || now.Sub(w.start) >= l.window {
		w = &window{start: now}
		l.hits[key] = w
	}
	if w.n >= l.limit {
		return false, w.start.Add(l.window).Sub(now)
	}
	w.n++
	return true, 0
}

// reset forgets key's count.
func (l *limiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, key)
}
