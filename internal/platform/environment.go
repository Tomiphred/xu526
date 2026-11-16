package platform

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// DefaultEnvironment holds everything deployed without naming an
// environment, including every stack and app from before environments
// existed. Its stacks and apps keep their plain names.
const DefaultEnvironment = "production"

// Environment is an isolated copy of a set of stacks and apps (staging,
// preview, production …). Each environment has its own variables and
// secrets, and its stacks get their own apps, network and volumes, so the
// same stack file deploys into any of them without sharing data.
//
// Stack files read the values with ${{ vars.NAME }} and ${{ secrets.NAME }}.
// Secrets are never shown again once set (the API and dashboard list only
// their names) and may not be baked into images through build arguments.
type Environment struct {
	Name      string            `json:"name"`
	Variables map[string]string `json:"variables"`
	Secrets   map[string]string `json:"secrets,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

// EnvironmentStatus is an environment as the API shows it: secret values
// are left out, and what it contains is listed.
type EnvironmentStatus struct {
	Name      string            `json:"name"`
	Default   bool              `json:"default,omitempty"`
	Variables map[string]string `json:"variables"`
	Secrets   []string          `json:"secrets"` // names only
	Stacks    []string          `json:"stacks"`  // stack names (as written in `xu stack deploy NAME`)
	Apps      []string          `json:"apps"`    // every app, stack services included
	CreatedAt time.Time         `json:"created_at"`
}

var (
	envNameRE = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,13}[a-z0-9])?$`)
	varNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// stackKey is the name a stack is stored, deployed and routed under:
// the stack's own name in production, name-environment elsewhere. Its apps
// are key-service, its network xu-key and its volumes xu-key-volume, which
// is what keeps environments apart.
func stackKey(stack, env string) string {
	if env == "" || env == DefaultEnvironment {
		return stack
	}
	return stack + "-" + env
}

// StackKey is stackKey for the CLI.
func StackKey(stack, env string) string { return stackKey(stack, env) }

// envOrDefault maps "" to the default environment.
func envOrDefault(env string) string {
	if env == "" {
		return DefaultEnvironment
	}
	return env
}

// ensureEnvLocked returns the environment, creating it if needed (a stack
// deployed into a new environment creates it). Callers hold p.mu.
func (p *Platform) ensureEnvLocked(name string) (*Environment, bool, error) {
	name = envOrDefault(name)
	if e := p.envs[name]; e != nil {
		return e, false, nil
	}
	if !envNameRE.MatchString(name) {
		return nil, false, fmt.Errorf("environment name %q: use 1-15 lowercase letters, digits and dashes, starting with a letter", name)
	}
	e := &Environment{Name: name, Variables: map[string]string{}, Secrets: map[string]string{}, CreatedAt: time.Now()}
	p.envs[name] = e
	return e, true, nil
}

// CreateEnvironment adds an empty environment.
func (p *Platform) CreateEnvironment(name string) error {
	p.mu.Lock()
	_, created, err := p.ensureEnvLocked(name)
	p.mu.Unlock()
	if err != nil {
		return err
	}
	if !created {
		return fmt.Errorf("environment %s already exists", name)
	}
	p.event("", "deploy", "created environment %s", name)
	return p.save()
}

// envValues returns copies of an environment's variables and secrets. The
// default environment always exists, even before anything is stored in it.
func (p *Platform) envValues(name string) (vars, secrets map[string]string, ok bool) {
	name = envOrDefault(name)
	p.mu.RLock()
	defer p.mu.RUnlock()
	vars, secrets = map[string]string{}, map[string]string{}
	e := p.envs[name]
	if e == nil {
		return vars, secrets, name == DefaultEnvironment
	}
	for k, v := range e.Variables {
		vars[k] = v
	}
	for k, v := range e.Secrets {
		secrets[k] = v
	}
	return vars, secrets, true
}

// SetEnvironmentValues sets (or, with a nil value, removes) variables and
// secrets. Deployed stacks pick the new values up on their next deploy.
func (p *Platform) SetEnvironmentValues(name string, vars, secrets map[string]*string) error {
	for k := range vars {
		if !varNameRE.MatchString(k) {
			return fmt.Errorf("variable name %q: use letters, digits and _", k)
		}
	}
	for k := range secrets {
		if !varNameRE.MatchString(k) {
			return fmt.Errorf("secret name %q: use letters, digits and _", k)
		}
	}
	p.mu.Lock()
	e, _, err := p.ensureEnvLocked(name)
	if err != nil {
		p.mu.Unlock()
		return err
	}
	apply := func(dst map[string]string, src map[string]*string) {
		for k, v := range src {
			if v == nil {
				delete(dst, k)
			} else {
				dst[k] = *v
			}
		}
	}
	if e.Variables == nil {
		e.Variables = map[string]string{}
	}
	if e.Secrets == nil {
		e.Secrets = map[string]string{}
	}
	apply(e.Variables, vars)
	apply(e.Secrets, secrets)
	p.mu.Unlock()
	return p.save()
}

// Environments lists every environment, the default one first.
func (p *Platform) Environments() []EnvironmentStatus {
	p.mu.RLock()
	defer p.mu.RUnlock()
	byName := map[string]*EnvironmentStatus{}
	get := func(name string) *EnvironmentStatus {
		if s := byName[name]; s != nil {
			return s
		}
		s := &EnvironmentStatus{Name: name, Default: name == DefaultEnvironment,
			Variables: map[string]string{}, Secrets: []string{}, Stacks: []string{}, Apps: []string{}}
		if e := p.envs[name]; e != nil {
			s.CreatedAt = e.CreatedAt
			for k, v := range e.Variables {
				s.Variables[k] = v
			}
			for k := range e.Secrets {
				s.Secrets = append(s.Secrets, k)
			}
			sort.Strings(s.Secrets)
		}
		byName[name] = s
		return s
	}
	get(DefaultEnvironment)
	for name := range p.envs {
		get(name)
	}
	for _, r := range p.stacks {
		s := get(envOrDefault(r.Environment))
		s.Stacks = append(s.Stacks, r.stackName())
	}
	for name, a := range p.apps {
		s := get(envOrDefault(a.spec.Environment))
		s.Apps = append(s.Apps, name)
	}
	out := make([]EnvironmentStatus, 0, len(byName))
	for _, s := range byName {
		sort.Strings(s.Stacks)
		sort.Strings(s.Apps)
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Default != out[j].Default {
			return out[i].Default
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// DeleteEnvironment deletes an environment with its stacks and apps. Named
// volumes are kept, as when a stack is deleted. The default environment
// can't be deleted.
func (p *Platform) DeleteEnvironment(ctx context.Context, name string) error {
	if name == DefaultEnvironment || name == "" {
		return errors.New("the production environment can't be deleted; delete its stacks and apps instead")
	}
	p.mu.RLock()
	_, ok := p.envs[name]
	var stacks, apps []string
	for key, r := range p.stacks {
		if r.Environment == name {
			stacks = append(stacks, key)
		}
	}
	for n, a := range p.apps {
		if a.spec.Environment == name && a.spec.Stack == "" {
			apps = append(apps, n)
		}
	}
	p.mu.RUnlock()
	if !ok {
		return fmt.Errorf("no environment %q", name)
	}
	for _, key := range stacks {
		if err := p.DeleteStack(ctx, key); err != nil {
			return fmt.Errorf("stack %s: %w", key, err)
		}
	}
	for _, n := range apps {
		if err := p.Delete(n); err != nil {
			return fmt.Errorf("app %s: %w", n, err)
		}
	}
	p.mu.Lock()
	delete(p.envs, name)
	p.mu.Unlock()
	p.event("", "delete", "deleted environment %s (%d stacks, %d apps; volumes are kept)", name, len(stacks), len(apps))
	return p.save()
}

// environmentAPI serves /v1/environments.
//
//	GET    /v1/environments                 list (secret values left out)
//	POST   /v1/environments                 create {name}
//	PATCH  /v1/environments/{name}          {variables: {K: "v" | null}, secrets: {K: "v" | null}}
//	DELETE /v1/environments/{name}          delete it with its stacks and apps
func (p *Platform) environmentAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/environments", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, p.Environments())
	})
	mux.HandleFunc("POST /v1/environments", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		if err := decodeJSON(w, r, &req); err != nil {
			return
		}
		if err := p.CreateEnvironment(strings.TrimSpace(req.Name)); err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"name": req.Name})
	})
	mux.HandleFunc("PATCH /v1/environments/{name}", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Variables map[string]*string `json:"variables"`
			Secrets   map[string]*string `json:"secrets"`
		}
		if err := decodeJSON(w, r, &req); err != nil {
			return
		}
		if err := p.SetEnvironmentValues(r.PathValue("name"), req.Variables, req.Secrets); err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /v1/environments/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := p.DeleteEnvironment(r.Context(), r.PathValue("name")); err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
