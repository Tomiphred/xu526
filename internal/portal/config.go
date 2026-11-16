package portal

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Environment variables that configure the portal (in /etc/default/xud).
const (
	EnvDomain   = "XU_PORTAL_DOMAIN"   // the site's hostname; setting it turns the portal on
	EnvName     = "XU_PORTAL_NAME"     // shown on pages and in emails
	EnvDatabase = "XU_PORTAL_DATABASE" // postgres://… (or XU_PORTAL_DATABASE_FILE)
	EnvSMTPHost = "XU_PORTAL_SMTP_HOST"
	EnvSMTPPort = "XU_PORTAL_SMTP_PORT"
	EnvSMTPUser = "XU_PORTAL_SMTP_USER"
	EnvSMTPTLS  = "XU_PORTAL_SMTP_TLS"      // implicit (port 465's default) or starttls (any other port's)
	EnvSMTPPass = "XU_PORTAL_SMTP_PASSWORD" // or XU_PORTAL_SMTP_PASSWORD_FILE
	EnvMailFrom = "XU_PORTAL_MAIL_FROM"
	EnvMaxApps  = "XU_PORTAL_MAX_APPS"
	EnvMBps     = "XU_PORTAL_MBPS"
	EnvInsecure = "XU_PORTAL_INSECURE_HTTP" // local testing only: cookies without Secure
)

// Settings is everything xud needs to run the portal.
type Settings struct {
	Config
	DatabaseURL string
	SMTP        SMTP
}

// LoadSettings reads the portal's settings from the environment. It returns
// nil, nil when XU_PORTAL_DOMAIN isn't set. The code-signing key lives in
// keyDir/portal.key, created on first use.
func LoadSettings(keyDir string) (*Settings, error) {
	host := strings.ToLower(strings.TrimSpace(os.Getenv(EnvDomain)))
	if host == "" {
		return nil, nil
	}
	if strings.ContainsAny(host, "/: ") {
		return nil, fmt.Errorf("%s should be a hostname like tunnels.example.com", EnvDomain)
	}
	s := &Settings{Config: Config{Host: host, SiteName: strings.TrimSpace(os.Getenv(EnvName)), Secure: os.Getenv(EnvInsecure) != "1"}}
	var err error
	if s.DatabaseURL, err = secret(EnvDatabase); err != nil {
		return nil, err
	}
	if s.DatabaseURL == "" {
		return nil, fmt.Errorf("%s (or %s_FILE) is required: postgres://user:password@127.0.0.1:5432/xu_portal", EnvDatabase, EnvDatabase)
	}
	s.SMTP.Host = strings.TrimSpace(os.Getenv(EnvSMTPHost))
	if s.SMTP.Host == "" {
		s.SMTP.Host = "smtp.hostinger.com"
	}
	s.SMTP.Port = 465
	if p := strings.TrimSpace(os.Getenv(EnvSMTPPort)); p != "" {
		if s.SMTP.Port, err = strconv.Atoi(p); err != nil || s.SMTP.Port <= 0 {
			return nil, fmt.Errorf("%s must be a port number (465 or 587)", EnvSMTPPort)
		}
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvSMTPTLS))) {
	case "":
		s.SMTP.ImplicitTLS = s.SMTP.Port == 465
	case "implicit", "ssl", "tls":
		s.SMTP.ImplicitTLS = true
	case "starttls":
	default:
		return nil, fmt.Errorf("%s must be implicit or starttls", EnvSMTPTLS)
	}
	s.SMTP.User = strings.TrimSpace(os.Getenv(EnvSMTPUser))
	if s.SMTP.Password, err = secret(EnvSMTPPass); err != nil {
		return nil, err
	}
	if s.SMTP.User == "" || s.SMTP.Password == "" {
		return nil, fmt.Errorf("%s and %s (or %s_FILE) are required: the mailbox and password that send email", EnvSMTPUser, EnvSMTPPass, EnvSMTPPass)
	}
	s.SMTP.From = strings.TrimSpace(os.Getenv(EnvMailFrom))
	if s.SMTP.From == "" {
		s.SMTP.From = s.SMTP.User
	}
	if a, err := mail.ParseAddress(s.SMTP.From); err != nil || a.Address != s.SMTP.From {
		return nil, fmt.Errorf("%s must be a plain address like tunnels@example.com", EnvMailFrom)
	}
	s.SMTP.FromName = s.SiteName
	if s.SMTP.FromName == "" {
		s.SMTP.FromName = "xu526 tunnel"
	}
	if v := os.Getenv(EnvMaxApps); v != "" {
		if s.MaxApps, err = strconv.Atoi(v); err != nil || s.MaxApps < 1 {
			return nil, fmt.Errorf("%s must be a positive number", EnvMaxApps)
		}
	}
	if v := os.Getenv(EnvMBps); v != "" {
		if s.RateMBps, err = strconv.ParseFloat(v, 64); err != nil || s.RateMBps <= 0 {
			return nil, fmt.Errorf("%s must be a positive number (MB/s)", EnvMBps)
		}
	}
	if s.CodeKey, err = loadKey(filepath.Join(keyDir, "portal.key")); err != nil {
		return nil, err
	}
	return s, nil
}

// secret reads NAME, or the file named by NAME_FILE (which only xud's user
// may read). A value read from the environment is removed from it so child
// processes don't inherit it.
func secret(name string) (string, error) {
	if path := os.Getenv(name + "_FILE"); path != "" {
		info, err := os.Stat(path)
		if err != nil {
			return "", fmt.Errorf("%s_FILE: %w", name, err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("%s_FILE: %s is readable by other users (mode %o); run chmod 600 on it", name, path, info.Mode().Perm())
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("%s_FILE: %w", name, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	v := os.Getenv(name)
	os.Unsetenv(name)
	return strings.TrimSpace(v), nil
}

// loadKey reads the 32-byte code key, creating it (mode 0600) if missing.
func loadKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		if len(b) < 32 {
			return nil, fmt.Errorf("%s is too short; delete it to make a new one (codes already sent stop working)", path)
		}
		return b[:32], nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	b = make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		return nil, err
	}
	return b, nil
}
