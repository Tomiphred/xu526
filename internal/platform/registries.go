package platform

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"xu526/internal/cloud"
	"xu526/internal/runtime"
)

// Registry holds credentials for a container registry.
type Registry struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`   // dockerhub, ecr, ghcr, generic
	Server    string `json:"server"` // docker.io, ghcr.io, 1234.dkr.ecr.eu-west-1.amazonaws.com, …
	Username  string `json:"username,omitempty"`
	Password  string `json:"password,omitempty"` // password or access token
	Region    string `json:"region,omitempty"`   // ecr
	AccessKey string `json:"access_key,omitempty"`
	SecretKey string `json:"secret_key,omitempty"`

	ecrUser, ecrPass string
	ecrExpires       time.Time
}

func (r Registry) redacted() Registry {
	if r.Password != "" {
		r.Password = "••••"
	}
	if r.SecretKey != "" {
		r.SecretKey = "••••"
	}
	return r
}

func (r *Registry) normalize() error {
	if !nameRE.MatchString(r.Name) {
		return errors.New("registry name: use lowercase letters, digits and dashes")
	}
	switch r.Kind {
	case "dockerhub":
		r.Server = "docker.io"
	case "ghcr":
		r.Server = "ghcr.io"
	case "ecr":
		if r.Region == "" || r.AccessKey == "" || r.SecretKey == "" || r.Server == "" {
			return errors.New("ECR needs the registry address (…dkr.ecr.<region>.amazonaws.com), region, access key and secret key")
		}
	case "generic":
		if r.Server == "" {
			return errors.New("enter the registry address, e.g. registry.example.com")
		}
	default:
		return fmt.Errorf("unknown registry kind %q", r.Kind)
	}
	r.Server = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(r.Server, "https://"), "http://"), "/")
	if r.Kind != "ecr" && (r.Username == "" || r.Password == "") {
		return errors.New("username and password (or access token) are required")
	}
	return nil
}

// imageHost returns the registry host of an image reference.
func imageHost(ref string) string {
	first, _, found := strings.Cut(ref, "/")
	if found && (strings.ContainsAny(first, ".:") || first == "localhost") {
		return first
	}
	return "docker.io"
}

func sameHost(a, b string) bool {
	norm := func(h string) string {
		switch h {
		case "index.docker.io", "registry-1.docker.io", "registry.hub.docker.com":
			return "docker.io"
		}
		return h
	}
	return norm(a) == norm(b)
}

// credentials returns a username/password for r, logging in to ECR if needed.
func (p *Platform) credentials(ctx context.Context, r *Registry) (string, string, error) {
	if r.Kind != "ecr" {
		return r.Username, r.Password, nil
	}
	p.regMu.Lock()
	defer p.regMu.Unlock()
	if time.Until(r.ecrExpires) > 10*time.Minute {
		return r.ecrUser, r.ecrPass, nil
	}
	user, pass, exp, err := cloud.ECRLogin(ctx, cloud.Credentials{AccessKey: r.AccessKey, SecretKey: r.SecretKey}, r.Region)
	if err != nil {
		return "", "", err
	}
	r.ecrUser, r.ecrPass, r.ecrExpires = user, pass, exp
	return user, pass, nil
}

func (p *Platform) registryFor(host string) *Registry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, r := range p.registries {
		if sameHost(r.Server, host) {
			return r
		}
	}
	return nil
}

// authFor returns the X-Registry-Auth value for pulling or pushing image.
func (p *Platform) authFor(ctx context.Context, image string) (string, error) {
	r := p.registryFor(imageHost(image))
	if r == nil || image == "" {
		return "", nil
	}
	user, pass, err := p.credentials(ctx, r)
	if err != nil {
		return "", fmt.Errorf("registry %s: %w", r.Name, err)
	}
	b, _ := json.Marshal(map[string]string{"username": user, "password": pass, "serveraddress": r.Server})
	return base64.URLEncoding.EncodeToString(b), nil
}

// registryConfig returns X-Registry-Config for builds (all registries).
func (p *Platform) registryConfig(ctx context.Context) string {
	p.mu.RLock()
	regs := append([]*Registry(nil), p.registries...)
	p.mu.RUnlock()
	cfg := map[string]map[string]string{}
	for _, r := range regs {
		if user, pass, err := p.credentials(ctx, r); err == nil {
			server := r.Server
			if server == "docker.io" {
				server = "https://index.docker.io/v1/"
			}
			cfg[server] = map[string]string{"username": user, "password": pass}
		}
	}
	b, _ := json.Marshal(cfg)
	return base64.URLEncoding.EncodeToString(b)
}

// pinImage resolves the spec's image to an exact ID so this release can be
// restored later even if the tag moves.
func (p *Platform) pinImage(ctx context.Context, spec *AppSpec) error {
	d, ok := p.cfg.Docker.(*runtime.Docker)
	if !ok || spec.Image == "" {
		return nil
	}
	auth, err := p.authFor(ctx, spec.Image)
	if err != nil {
		return err
	}
	if spec.ImageID != "" { // rollback: use the exact image again
		if _, err := d.Resolve(ctx, spec.ImageID, ""); err == nil {
			return nil
		}
		if spec.ImageDigest != "" {
			if _, err := d.Resolve(ctx, spec.ImageDigest, auth); err == nil {
				return nil
			}
		}
		return fmt.Errorf("the image of this release (%s) is no longer on this server or in its registry", short(spec.ImageID))
	}
	info, err := d.Resolve(ctx, spec.Image, auth)
	if err != nil {
		return err
	}
	spec.ImageID, spec.ImageDigest = info.ID, info.Digest
	return nil
}

func short(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// ---- management ----

func (p *Platform) Registries() []Registry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := []Registry{}
	for _, r := range p.registries {
		out = append(out, r.redacted())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// PutRegistry adds or replaces a registry. Blank secrets keep the old ones.
func (p *Platform) PutRegistry(r Registry) error {
	p.mu.Lock()
	for _, old := range p.registries {
		if old.Name == r.Name {
			if r.Password == "" || strings.HasPrefix(r.Password, "••") {
				r.Password = old.Password
			}
			if r.SecretKey == "" || strings.HasPrefix(r.SecretKey, "••") {
				r.SecretKey = old.SecretKey
			}
		}
	}
	p.mu.Unlock()
	if err := r.normalize(); err != nil {
		return err
	}
	p.mu.Lock()
	list := []*Registry{}
	for _, old := range p.registries {
		if old.Name != r.Name {
			list = append(list, old)
		}
	}
	p.registries = append(list, &r)
	p.mu.Unlock()
	return p.save()
}

func (p *Platform) DeleteRegistry(name string) error {
	p.mu.Lock()
	list := []*Registry{}
	for _, r := range p.registries {
		if r.Name != name {
			list = append(list, r)
		}
	}
	found := len(list) != len(p.registries)
	p.registries = list
	p.mu.Unlock()
	if !found {
		return fmt.Errorf("no registry %q", name)
	}
	return p.save()
}

// TestRegistry checks the credentials by logging in through Docker.
func (p *Platform) TestRegistry(ctx context.Context, name string) error {
	var reg *Registry
	p.mu.RLock()
	for _, r := range p.registries {
		if r.Name == name {
			reg = r
		}
	}
	p.mu.RUnlock()
	if reg == nil {
		return fmt.Errorf("no registry %q", name)
	}
	user, pass, err := p.credentials(ctx, reg)
	if err != nil {
		return err
	}
	d, ok := p.cfg.Docker.(*runtime.Docker)
	if !ok {
		return errors.New("docker is not available")
	}
	return d.Login(ctx, user, pass, reg.Server)
}
