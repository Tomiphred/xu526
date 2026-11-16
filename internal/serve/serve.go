// Package serve holds what the xu526 daemon and xu526-server share for
// serving HTTP(S).
package serve

import (
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"
)

// PortOf returns the port of an address like ":443" (0 if none).
func PortOf(addr string) int {
	if _, p, err := net.SplitHostPort(addr); err == nil {
		n, _ := strconv.Atoi(p)
		return n
	}
	return 0
}

// CertReloader serves a certificate from files, picking up renewals
// (certbot) without a restart.
type CertReloader struct {
	Cert, Key string
	mu        sync.Mutex
	loaded    *tls.Certificate
	mod       time.Time
	checked   time.Time
}

// Get is a tls.Config GetCertificate function.
func (c *CertReloader) Get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loaded != nil && time.Since(c.checked) < time.Minute {
		return c.loaded, nil
	}
	c.checked = time.Now()
	st, err := os.Stat(c.Cert)
	if err != nil {
		if c.loaded != nil {
			return c.loaded, nil
		}
		return nil, err
	}
	if c.loaded == nil || st.ModTime().After(c.mod) {
		cert, err := tls.LoadX509KeyPair(c.Cert, c.Key)
		if err != nil {
			if c.loaded != nil {
				return c.loaded, nil
			}
			return nil, err
		}
		c.loaded, c.mod = &cert, st.ModTime()
	}
	return c.loaded, nil
}

// DefaultState is where xu526 keeps its state: /var/lib/xu526 on Linux,
// the per-user Application Support folder on macOS, and %LOCALAPPDATA%\xu526
// on Windows.
func DefaultState() string {
	if runtime.GOOS == "windows" {
		// The service runs as the signed-in person (Docker Desktop is per person).
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "xu526", "apps.json")
		}
	}
	if runtime.GOOS == "darwin" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Application Support", "xu526", "apps.json")
		}
	}
	return "/var/lib/xu526/apps.json"
}
