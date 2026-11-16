package tunnel

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Path is where the relay accepts agents on its public web port. Using an
// HTTP upgrade (like WebSockets) means tunnels work wherever ports 80/443 do.
const (
	Path     = "/_xu/tunnel"
	protocol = "xu-tunnel/1"
)

type bufConn struct {
	net.Conn
	r io.Reader
}

func (c bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// Dial connects to a relay (http:// or https:// URL) and authenticates.
func Dial(ctx context.Context, relayURL, token string) (*Session, error) {
	u, err := url.Parse(strings.TrimRight(relayURL, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("relay address should look like https://xu.example.com")
	}
	addr := u.Host
	if u.Port() == "" {
		addr += map[string]string{"http": ":80", "https": ":443"}[u.Scheme]
	}
	var conn net.Conn
	d := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	if u.Scheme == "https" {
		conn, err = (&tls.Dialer{NetDialer: d, Config: &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	req, _ := http.NewRequest("GET", u.String()+Path, nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", protocol)
	req.Header.Set("Authorization", "Bearer "+token)
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("relay handshake: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		conn.Close()
		return nil, fmt.Errorf("relay refused the connection: %s", strings.TrimSpace(string(msg)))
	}
	_ = conn.SetDeadline(time.Time{})
	return NewSession(bufConn{conn, br}, true), nil
}

// Upgrade turns an agent's HTTP request into a session (relay side). The
// caller has already authenticated the request.
func Upgrade(w http.ResponseWriter, r *http.Request) (*Session, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), protocol) {
		http.Error(w, "this endpoint is for xu agents", http.StatusBadRequest)
		return nil, errors.New("not a tunnel upgrade")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "upgrade not supported", http.StatusInternalServerError)
		return nil, errors.New("hijack not supported")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	if _, err := rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + protocol + "\r\n\r\n"); err == nil {
		err = rw.Flush()
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return NewSession(bufConn{conn, rw.Reader}, false), nil
}

// Limiter is a token bucket shared by all streams of one user.
type Limiter struct {
	mu     sync.Mutex
	rate   float64 // bytes per second
	tokens float64
	last   time.Time
}

func NewLimiter(bytesPerSec float64) *Limiter {
	return &Limiter{rate: bytesPerSec, tokens: bytesPerSec, last: time.Now()}
}

// Wait blocks until n bytes may pass.
func (l *Limiter) Wait(n int) {
	if l == nil || l.rate <= 0 {
		return
	}
	l.mu.Lock()
	now := time.Now()
	l.tokens = min(l.rate, l.tokens+now.Sub(l.last).Seconds()*l.rate)
	l.last = now
	l.tokens -= float64(n)
	wait := time.Duration(0)
	if l.tokens < 0 {
		wait = time.Duration(-l.tokens / l.rate * float64(time.Second))
	}
	l.mu.Unlock()
	time.Sleep(wait)
}

// Limit wraps a connection so both directions count against l.
func Limit(c net.Conn, l *Limiter) net.Conn {
	if l == nil || l.rate <= 0 {
		return c
	}
	return &limited{Conn: c, l: l}
}

type limited struct {
	net.Conn
	l *Limiter
}

func (c *limited) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.l.Wait(n)
	return n, err
}

func (c *limited) Write(p []byte) (int, error) {
	c.l.Wait(len(p))
	return c.Conn.Write(p)
}
