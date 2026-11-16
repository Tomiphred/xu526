// Package ghapp integrates xu526 with GitHub as a GitHub App: users install
// it on all or selected repositories, xud mints short-lived installation
// tokens (one repository, contents:read) to clone, sign-in uses expiring
// user tokens with refresh, and every token mint and clone is audited.
//
// See docs/design/github-app.md for the data and threat models.
package ghapp

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Config is the GitHub App's identity and secrets. Secrets come from the
// environment or, preferably, from files (secret managers, systemd
// LoadCredential), never from xud's state.
type Config struct {
	AppID         int64
	ClientID      string
	Slug          string
	ClientSecret  string
	WebhookSecret string
	Keys          []*rsa.PrivateKey // signs with Keys[0]; Keys[1] is a rotation fallback
	SecretKey     []byte            // 32 bytes: seals refresh tokens at rest

	WebURL      string   // https://github.com (or a GitHub Enterprise Server URL)
	APIURL      string   // https://api.github.com (or https://ghes.example.com/api/v3)
	CallbackURL string   // where GitHub sends the browser back: <dashboard>/github/callback
	Users       []string // if set, only these GitHub logins or numeric IDs may link their account

	// Shared: the shared xu526 app, known only by its public client ID.
	// Sign-in uses the device flow; repositories are read with the
	// signed-in user's own token (limited to what both the user and the
	// app can access: Contents and Metadata, read). No webhooks: xu526
	// checks connected repositories for new commits itself.
	Shared bool
}

// Env names. Every secret has a _FILE form that names a file to read.
const (
	EnvAppID         = "XU_GITHUB_APP_ID"
	EnvClientID      = "XU_GITHUB_CLIENT_ID"
	EnvSlug          = "XU_GITHUB_APP_SLUG"
	EnvClientSecret  = "XU_GITHUB_CLIENT_SECRET"
	EnvWebhookSecret = "XU_GITHUB_WEBHOOK_SECRET"
	EnvPrivateKey    = "XU_GITHUB_PRIVATE_KEY"
	EnvPrivateKey2   = "XU_GITHUB_PRIVATE_KEY_2"
	EnvSecretKey     = "XU_SECRET_KEY"
	EnvWebURL        = "XU_GITHUB_URL"
	EnvAPIURL        = "XU_GITHUB_API_URL"
	EnvCallbackURL   = "XU_GITHUB_CALLBACK_URL"
	EnvUsers         = "XU_GITHUB_USERS"
)

// readSecret returns NAME_FILE's contents (refusing files that group or
// others can read) or NAME's value. An inline value is removed from the
// environment so child processes (git, builds) never inherit it.
func readSecret(get func(string) string, name string) (string, error) {
	if path := get(name + "_FILE"); path != "" {
		info, err := os.Stat(path)
		if err != nil {
			return "", fmt.Errorf("%s_FILE: %w", name, err)
		}
		// Windows has no Unix permissions (Go reports 0666); its ACLs keep
		// files in the user's profile private.
		if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("%s_FILE: %s is readable by other users (mode %o); run chmod 600 on it", name, path, info.Mode().Perm())
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("%s_FILE: %w", name, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	v := get(name)
	os.Unsetenv(name)
	return strings.TrimSpace(v), nil
}

// LoadConfig reads the GitHub App configuration. It returns nil, nil when
// no app is configured.
func LoadConfig() (*Config, error) { return loadConfig(os.Getenv) }

// LoadConfigFile reads the env file the one-click setup writes
// (KEY=value lines; the secrets themselves are in the *_FILE files).
func LoadConfigFile(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && !strings.HasPrefix(k, "#") {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	if m[EnvAppID] == "" {
		return nil, fmt.Errorf("%s has no %s", path, EnvAppID)
	}
	return loadConfig(func(k string) string { return m[k] })
}

func loadConfig(get func(string) string) (*Config, error) {
	idStr := strings.TrimSpace(get(EnvAppID))
	if idStr == "" {
		return loadShared(get)
	}
	c := &Config{
		ClientID:    strings.TrimSpace(get(EnvClientID)),
		Slug:        strings.TrimSpace(get(EnvSlug)),
		WebURL:      strings.TrimRight(strings.TrimSpace(get(EnvWebURL)), "/"),
		APIURL:      strings.TrimRight(strings.TrimSpace(get(EnvAPIURL)), "/"),
		CallbackURL: strings.TrimSpace(get(EnvCallbackURL)),
	}
	var err error
	if c.AppID, err = strconv.ParseInt(idStr, 10, 64); err != nil {
		return nil, fmt.Errorf("%s must be a number", EnvAppID)
	}
	for _, u := range strings.Split(get(EnvUsers), ",") {
		if u = strings.TrimSpace(u); u != "" {
			c.Users = append(c.Users, u)
		}
	}
	if c.WebURL == "" {
		c.WebURL = "https://github.com"
	}
	if c.APIURL == "" {
		if c.WebURL == "https://github.com" {
			c.APIURL = "https://api.github.com"
		} else {
			c.APIURL = c.WebURL + "/api/v3" // GitHub Enterprise Server
		}
	}
	if c.ClientSecret, err = readSecret(get, EnvClientSecret); err != nil {
		return nil, err
	}
	if c.WebhookSecret, err = readSecret(get, EnvWebhookSecret); err != nil {
		return nil, err
	}
	for _, name := range []string{EnvPrivateKey, EnvPrivateKey2} {
		pemText, err := readSecret(get, name)
		if err != nil {
			return nil, err
		}
		if pemText == "" {
			continue
		}
		k, err := ParsePrivateKey([]byte(pemText))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		c.Keys = append(c.Keys, k)
	}
	sk, err := readSecret(get, EnvSecretKey)
	if err != nil {
		return nil, err
	}
	if c.SecretKey, err = parseKey(sk); err != nil {
		return nil, fmt.Errorf("%s: %w", EnvSecretKey, err)
	}
	return c, c.Validate()
}

// loadShared configures the shared xu526 app, or returns nil, nil when no
// client ID is known (GitHub stays off).
func loadShared(get func(string) string) (*Config, error) {
	c := &Config{Shared: true,
		ClientID: firstNonEmpty(strings.TrimSpace(get(EnvClientID)), SharedClientID),
		Slug:     firstNonEmpty(strings.TrimSpace(get(EnvSlug)), SharedAppSlug),
		WebURL:   strings.TrimRight(strings.TrimSpace(get(EnvWebURL)), "/"),
		APIURL:   strings.TrimRight(strings.TrimSpace(get(EnvAPIURL)), "/"),
	}
	if c.ClientID == "" {
		return nil, nil
	}
	for _, u := range strings.Split(get(EnvUsers), ",") {
		if u = strings.TrimSpace(u); u != "" {
			c.Users = append(c.Users, u)
		}
	}
	if c.WebURL == "" {
		c.WebURL = "https://github.com"
	}
	if c.APIURL == "" {
		if c.WebURL == "https://github.com" {
			c.APIURL = "https://api.github.com"
		} else {
			c.APIURL = c.WebURL + "/api/v3"
		}
	}
	sk, err := readSecret(get, EnvSecretKey)
	if err != nil {
		return nil, err
	}
	if c.SecretKey, err = parseKey(sk); err != nil {
		return nil, fmt.Errorf("%s: %w", EnvSecretKey, err)
	}
	return c, nil // the sealing key, when not given, is made by New
}

// Validate reports the first missing piece of configuration.
func (c *Config) Validate() error {
	if c.Shared {
		if c.ClientID == "" || c.Slug == "" {
			return fmt.Errorf("the shared GitHub app needs %s and %s", EnvClientID, EnvSlug)
		}
		if len(c.SecretKey) != 32 {
			return fmt.Errorf("%s: a 32-byte key is needed to seal sign-ins", EnvSecretKey)
		}
		return nil
	}
	var missing []string
	if c.AppID == 0 {
		missing = append(missing, EnvAppID)
	}
	if c.ClientID == "" {
		missing = append(missing, EnvClientID)
	}
	if c.Slug == "" {
		missing = append(missing, EnvSlug)
	}
	if c.ClientSecret == "" {
		missing = append(missing, EnvClientSecret+"[_FILE]")
	}
	// No webhook secret is fine: xu526 then checks repositories for new
	// commits itself, and the webhook endpoint stays off.
	if len(c.Keys) == 0 {
		missing = append(missing, EnvPrivateKey+"[_FILE]")
	}
	if len(c.SecretKey) != 32 {
		missing = append(missing, EnvSecretKey+"[_FILE] (32 bytes, hex or base64; `openssl rand -hex 32`)")
	}
	if c.CallbackURL == "" {
		missing = append(missing, EnvCallbackURL+" (e.g. http://localhost:7070/github/callback)")
	}
	if len(missing) > 0 {
		return fmt.Errorf("GitHub App configuration is incomplete; set %s", strings.Join(missing, ", "))
	}
	return nil
}

// ParsePrivateKey reads an RSA key in PKCS#1 (GitHub's format) or PKCS#8 PEM.
func ParsePrivateKey(b []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("not a PEM private key")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("the private key is not RSA")
	}
	return rk, nil
}

func parseKey(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	if b, err := hex.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil && len(b) == 32 {
		return b, nil
	}
	if len(s) == 32 {
		return []byte(s), nil
	}
	return nil, errors.New("must be 32 bytes, written as 64 hex digits or base64")
}
