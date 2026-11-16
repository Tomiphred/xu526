package platform

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"xu526/internal/runtime"
)

// stackRecord is what is stored for a deployed compose file.
type stackRecord struct {
	Name        string            `json:"name"`            // key: stack, or stack-environment outside production
	Stack       string            `json:"stack,omitempty"` // the name given to `xu stack deploy` ("" = Name)
	Environment string            `json:"environment,omitempty"`
	YAML        string            `json:"yaml"`
	Built       map[string]string `json:"built,omitempty"` // service → image built from source
	Files       map[string]string `json:"files,omitempty"` // .env and env_file contents, from its folder
	Status      string            `json:"status"`          // deploying, running, failed
	Error       string            `json:"error,omitempty"`
	DeployedAt  time.Time         `json:"deployed_at"`
}

// stackName is the stack's own name, without the environment.
func (r *stackRecord) stackName() string {
	if r.Stack != "" {
		return r.Stack
	}
	return r.Name
}

// StackStatus is a stack with its plan, for the API and dashboard.
type StackStatus struct {
	stackRecord
	Plan *StackPlan `json:"plan"`
}

// DeployStack deploys (or updates) every service of a compose file, in
// dependency order. Each service waits until it is healthy before the services
// that depend on it start. Services removed from the file are deleted.
func (p *Platform) DeployStack(ctx context.Context, name string, src []byte) (*StackPlan, error) {
	return p.DeployStackIn(ctx, name, DefaultEnvironment, src, nil, nil)
}

// DeployStackIn deploys a stack into an environment, creating the
// environment if it doesn't exist. built maps services that use build: to
// the images the CLI built and uploaded (xu/...).
func (p *Platform) DeployStackIn(ctx context.Context, name, env string, src []byte, built, files map[string]string) (*StackPlan, error) {
	for svc, ref := range built {
		if !uploadRef.MatchString(ref) {
			return nil, fmt.Errorf("service %s: built images must be uploaded ones (xu/NAME:TAG), not %q", svc, ref)
		}
	}
	t := stackTarget{Stack: name, Env: envOrDefault(env), Files: files}
	_, err := p.deployStackTo(ctx, t, src, nil, built, ReleaseMeta{Source: "manual"}, nil)
	return p.planStack(t, src, p.builtFor(t.key(), built)), err
}

// builtFor fills in images from the stack's last deploy for services that
// weren't rebuilt, so changing only settings needs no rebuild.
func (p *Platform) builtFor(key string, built map[string]string) map[string]string {
	out := map[string]string{}
	p.mu.RLock()
	if r := p.stacks[key]; r != nil {
		for k, v := range r.Built {
			out[k] = v
		}
	}
	p.mu.RUnlock()
	for k, v := range built {
		out[k] = v
	}
	return out
}

// deployStack redeploys a stored stack (rollback, git push) by its key.
func (p *Platform) deployStack(ctx context.Context, key string, src []byte, pins map[string]PinnedImage,
	built map[string]string, m ReleaseMeta, rel *Release) (*Release, error) {
	return p.deployStackTo(ctx, p.targetOf(key), src, pins, built, m, rel)
}

// deployStackTo deploys a compose file as a new stack release. pins restores
// exact images (rollback); built maps services to images built from source.
func (p *Platform) deployStackTo(ctx context.Context, t stackTarget, src []byte, pins map[string]PinnedImage,
	built map[string]string, m ReleaseMeta, rel *Release) (*Release, error) {
	p.stackMu.Lock()
	defer p.stackMu.Unlock()
	t.Env = envOrDefault(t.Env)
	key := t.key()
	p.mu.RLock()
	if r := p.stacks[key]; r != nil && (r.stackName() != t.Stack || envOrDefault(r.Environment) != t.Env) {
		p.mu.RUnlock()
		return nil, fmt.Errorf("the name %s is taken by stack %s in %s; choose another stack name", key, r.stackName(), envOrDefault(r.Environment))
	}
	p.mu.RUnlock()
	if rel == nil {
		rel = p.newRelease("stack", key, "deploying", m)
	} else {
		p.setReleaseStatus(rel, "deploying")
	}
	built = p.builtFor(key, built)
	plan := p.planStack(t, src, built)
	err := p.deployStackPlan(ctx, t, src, plan, pins, built, rel)
	p.finishRelease(rel, err)
	return rel, err
}

func (p *Platform) deployStackPlan(ctx context.Context, t stackTarget, src []byte, plan *StackPlan,
	pins map[string]PinnedImage, built map[string]string, rel *Release) error {
	name := t.key()
	if len(plan.Errors) > 0 {
		return fmt.Errorf("the stack file has %d problem(s): %s", len(plan.Errors), strings.Join(plan.Errors, "; "))
	}
	if p.cfg.Docker == nil {
		return errors.New("stacks need Docker, which is not available on this server")
	}
	for _, s := range plan.Services {
		if a := p.get(s.App); a != nil && a.spec.Stack != name {
			return fmt.Errorf("an app named %s already exists outside this stack; delete it or rename the service", s.App)
		}
		if s.Build != nil && s.Spec.Image == "" {
			if _, pinned := pins[s.Name]; !pinned {
				return fmt.Errorf("service %s is built from %s: deploy this file with `xu stack deploy %s -f FILE` from the folder it's in, so xu can build it first",
					s.Name, s.Build.Context, t.Stack)
			}
		}
	}

	p.mu.Lock()
	if _, _, err := p.ensureEnvLocked(t.Env); err != nil {
		p.mu.Unlock()
		return err
	}
	keepBuilt := map[string]string{}
	for _, s := range plan.Services {
		if ref, ok := built[s.Name]; ok && s.Build != nil {
			keepBuilt[s.Name] = ref
		}
	}
	rec := &stackRecord{Name: name, Stack: t.Stack, Environment: environmentField(t.Env), YAML: string(src),
		Built: keepBuilt, Files: t.Files, Status: "deploying", DeployedAt: time.Now()}
	if t.Stack == name {
		rec.Stack = ""
	}
	p.stacks[name] = rec
	p.mu.Unlock()
	_ = p.save()
	where := ""
	if t.Env != DefaultEnvironment {
		where = " in " + t.Env
	}
	p.event(name, "deploy", "deploying stack%s: %s", where, strings.Join(plan.Order, ", "))

	byName := map[string]PlanService{}
	for _, s := range plan.Services {
		byName[s.Name] = s
	}
	newPins := map[string]PinnedImage{}
	for _, svc := range plan.Order {
		spec := byName[svc].Spec
		if pin, ok := pins[svc]; ok {
			spec.ImageID, spec.ImageDigest = pin.ID, pin.Digest
			if spec.Image == "" {
				spec.Image = pin.ID // a rollback of a built service runs the exact image it ran then
			}
		}
		pinned, err := p.deployApp(ctx, spec)
		if err != nil {
			p.setStack(rec, "failed", fmt.Sprintf("service %s: %v", svc, err))
			p.event(name, "error", "stack deploy stopped at %s: %v", svc, err)
			return fmt.Errorf("service %s: %w", svc, err)
		}
		newPins[svc] = PinnedImage{ID: pinned.ImageID, Digest: pinned.ImageDigest}
	}
	p.mu.Lock()
	rel.YAML, rel.Pins = string(src), newPins
	p.mu.Unlock()

	keep := map[string]bool{}
	for _, s := range plan.Services {
		keep[s.App] = true
	}
	for _, app := range p.appsInStack(name) {
		if !keep[app] {
			_ = p.deleteApp(app)
		}
	}
	// x-xu share: true/false turns tunnel sharing on or off; without it,
	// sharing set by hand in the dashboard is left alone.
	for _, s := range plan.Services {
		if s.Share != nil {
			if err := p.SetTunnel(s.App, *s.Share); err != nil {
				p.event(name, "error", "sharing %s: %v", s.App, err)
			}
		}
	}
	p.setStack(rec, "running", "")
	p.event(name, "ready", "stack is running (%d services)", len(plan.Services))
	return nil
}

func (p *Platform) setStack(rec *stackRecord, status, errMsg string) {
	p.mu.Lock()
	rec.Status, rec.Error = status, errMsg
	p.mu.Unlock()
	_ = p.save()
}

func (p *Platform) appsInStack(stack string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []string
	for name, a := range p.apps {
		if a.spec.Stack == stack {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// DeleteStack stops every service, then removes the private network.
// Named volumes are kept so data survives; remove them with docker volume rm.
func (p *Platform) DeleteStack(ctx context.Context, name string) error {
	p.stackMu.Lock()
	defer p.stackMu.Unlock()

	p.mu.RLock()
	_, ok := p.stacks[name]
	p.mu.RUnlock()
	if !ok {
		return fmt.Errorf("no stack %q", name)
	}
	var specs []AppSpec
	for _, app := range p.appsInStack(name) {
		if a := p.get(app); a != nil {
			specs = append(specs, a.spec)
		}
		_ = p.deleteApp(app)
	}
	p.mu.Lock()
	for id, s := range p.sources {
		if s.Kind == "stack" && s.Target == name {
			delete(p.sources, id)
		}
	}
	p.mu.Unlock()

	done := make(chan struct{})
	go func() { p.stops.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(45 * time.Second):
	case <-ctx.Done():
	}
	if nr, ok := p.cfg.Docker.(runtime.NetworkRemover); ok {
		for _, n := range stackNetworkNames(name, specs) {
			if err := nr.RemoveNetwork(context.Background(), n); err != nil {
				p.log.Printf("stack %s: remove network %s: %v", name, n, err)
			}
		}
	}

	p.mu.Lock()
	delete(p.stacks, name)
	p.mu.Unlock()
	p.event(name, "delete", "stack deleted (named volumes are kept)")
	return p.save()
}

// Stacks lists deployed stacks with their plans.
func (p *Platform) Stacks() []StackStatus {
	p.mu.RLock()
	recs := make([]stackRecord, 0, len(p.stacks))
	for _, r := range p.stacks {
		recs = append(recs, *r)
	}
	p.mu.RUnlock()
	sort.Slice(recs, func(i, j int) bool { return recs[i].Name < recs[j].Name })
	out := make([]StackStatus, 0, len(recs))
	for _, r := range recs {
		t := stackTarget{Stack: r.stackName(), Env: envOrDefault(r.Environment), Files: r.Files}
		out = append(out, StackStatus{stackRecord: r, Plan: p.planStack(t, []byte(r.YAML), r.Built)})
	}
	return out
}

// Stack returns one stack.
func (p *Platform) Stack(name string) (StackStatus, bool) {
	for _, s := range p.Stacks() {
		if s.Name == name {
			return s, true
		}
	}
	return StackStatus{}, false
}
