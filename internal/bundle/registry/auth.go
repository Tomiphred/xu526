package registry

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// DockerConfig reads credentials the way the docker CLI stores them:
// $DOCKER_CONFIG/config.json (default ~/.docker/config.json), using
// credHelpers or credsStore when set and inline "auth" otherwise.
func DockerConfig(host string) (string, string, error) {
	dir := os.Getenv("DOCKER_CONFIG")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", nil
		}
		dir = filepath.Join(home, ".docker")
	}
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return "", "", nil // no config: anonymous
	}
	return fromConfig(b, host, runHelper)
}

type helperFunc func(helper, server string) (string, string, error)

func fromConfig(b []byte, host string, helper helperFunc) (string, string, error) {
	var cfg struct {
		Auths map[string]struct {
			Auth          string `json:"auth"`
			Username      string `json:"username"`
			Password      string `json:"password"`
			IdentityToken string `json:"identitytoken"`
		} `json:"auths"`
		CredHelpers map[string]string `json:"credHelpers"`
		CredsStore  string            `json:"credsStore"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return "", "", fmt.Errorf("docker config: %w", err)
	}
	// Docker Hub is stored under its v1 URL.
	keys := []string{host, "https://" + host, "http://" + host}
	if host == "registry-1.docker.io" || host == "docker.io" || host == "index.docker.io" {
		keys = []string{"https://index.docker.io/v1/", "index.docker.io", "docker.io", "registry-1.docker.io"}
	}
	for _, k := range keys {
		if h, ok := cfg.CredHelpers[k]; ok {
			return helper(h, k)
		}
	}
	for _, k := range keys {
		a, ok := cfg.Auths[k]
		if !ok {
			continue
		}
		if a.Auth != "" {
			raw, err := base64.StdEncoding.DecodeString(a.Auth)
			if err != nil {
				return "", "", fmt.Errorf("docker config: bad auth for %s", k)
			}
			user, pass, _ := strings.Cut(string(raw), ":")
			return user, pass, nil
		}
		if a.Username != "" {
			return a.Username, a.Password, nil
		}
		if a.IdentityToken != "" {
			return "<token>", a.IdentityToken, nil
		}
		if cfg.CredsStore != "" {
			return helper(cfg.CredsStore, k)
		}
	}
	if cfg.CredsStore != "" {
		if u, p, err := helper(cfg.CredsStore, keys[0]); err == nil {
			return u, p, nil
		}
	}
	return "", "", nil
}

// runHelper runs docker-credential-<name> get. "not found" means anonymous.
func runHelper(name, server string) (string, string, error) {
	cmd := exec.Command("docker-credential-"+name, "get")
	cmd.Stdin = strings.NewReader(server)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(out.String() + errb.String())
		if strings.Contains(strings.ToLower(msg), "not found") {
			return "", "", nil
		}
		if _, lookErr := exec.LookPath("docker-credential-" + name); lookErr != nil {
			return "", "", nil // helper configured but not installed: try anonymous
		}
		return "", "", fmt.Errorf("docker-credential-%s: %s", name, msg)
	}
	var c struct{ Username, Secret string }
	if err := json.Unmarshal(out.Bytes(), &c); err != nil {
		return "", "", fmt.Errorf("docker-credential-%s: %w", name, err)
	}
	return c.Username, c.Secret, nil
}
