package platform

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"xu526/internal/dotenv"

	"xu526/third_party/yaml"
)

// StackPlan is what a compose file turns into: the services the platform will
// run, how they connect, and anything in the file it can't honour. The
// dashboard draws it before anything is deployed.
type StackPlan struct {
	Stack       string        `json:"stack"`       // key: name, or name-environment outside production
	Name        string        `json:"name"`        // the stack's own name
	Environment string        `json:"environment"` // where it deploys
	Network     string        `json:"network"`
	Services    []PlanService `json:"services"`
	Edges       []PlanEdge    `json:"edges"`
	Volumes     []string      `json:"volumes"`
	// BuildSecrets is the file's top-level secrets:, for builds that use them.
	BuildSecrets map[string]any `json:"build_secrets,omitempty"`
	Order        []string       `json:"order"`    // deploy order (dependencies first)
	Errors       []string       `json:"errors"`   // block deployment
	Warnings     []string       `json:"warnings"` // parts of the file that are ignored
	Notes        []string       `json:"notes"`    // how the platform adapts the file
}

// PlanService is one compose service.
type PlanService struct {
	Name      string        `json:"name"` // service name, also its address on the network
	App       string        `json:"app"`  // platform app name
	Image     string        `json:"image"`
	Public    bool          `json:"public"`
	URL       string        `json:"url,omitempty"`
	Port      int           `json:"port,omitempty"`
	Scaling   string        `json:"scaling"`
	Min       int           `json:"min"`
	Max       int           `json:"max"`
	Target    int           `json:"target"`
	Volumes   []VolumeMount `json:"volumes,omitempty"`
	EnvKeys   []string      `json:"env_keys,omitempty"`
	DependsOn []string      `json:"depends_on,omitempty"`
	Kind      string        `json:"kind"`                 // database, cache, backend or web
	Build     *PlanBuild    `json:"build,omitempty"`      // built from source by `xu stack deploy`
	Share     *bool         `json:"share,omitempty"`      // x-xu share: also public through xu526 tunnel
	Networks  []string      `json:"networks,omitempty"`   // the file's networks it's on
	SharedURL string        `json:"shared_url,omitempty"` // its tunnel address, when known
	Spec      AppSpec       `json:"-"`
}

// PlanBuild is a service the CLI builds before deploying: with the
// folder's Dockerfile (docker or podman build), or else with xu build.
type PlanBuild struct {
	Context    string            `json:"context"`              // relative to the stack file's folder
	Dockerfile string            `json:"dockerfile,omitempty"` // relative to Context
	Target     string            `json:"target,omitempty"`     // Dockerfile stage
	Args       map[string]string `json:"args,omitempty"`       // build arguments, references resolved
	// Extra holds the rest of the file's build: section (secrets, ssh,
	// additional_contexts, cache_from, ...), passed to docker compose build.
	Extra map[string]any `json:"extra,omitempty"`
}

// HasDockerfile reports whether the service builds from a Dockerfile (or
// an inline one), which docker compose build can do; otherwise xu build
// builds it.
func (b *PlanBuild) HasDockerfile(contextDir string) bool {
	if _, ok := b.Extra["dockerfile_inline"]; ok {
		return true
	}
	df := b.Dockerfile
	if df == "" {
		df = "Dockerfile"
	}
	if !filepath.IsAbs(df) {
		df = filepath.Join(contextDir, filepath.FromSlash(df))
	}
	st, err := os.Stat(df)
	return err == nil && !st.IsDir()
}

// publicPrefixes mark variables that front-end frameworks bake into the
// browser code while building. A service's environment values with these
// prefixes are also given to its build, so NEXT_PUBLIC_API_URL works
// whether it's set under environment: or build.args.
var publicPrefixes = []string{"NEXT_PUBLIC_", "VITE_", "REACT_APP_", "NUXT_PUBLIC_", "PUBLIC_", "EXPO_PUBLIC_", "GATSBY_", "VUE_APP_"}

// PlanEdge is a connection between services. Kind "depends_on" comes from the
// file; kind "uses" is inferred from environment values that mention another
// service by name (e.g. DATABASE_URL=postgres://db:5432/app).
type PlanEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
	Via  string `json:"via,omitempty"` // env var that revealed a "uses" edge
}

type composeFile struct {
	Name     string                     `yaml:"name"`
	Services map[string]composeService  `yaml:"services"`
	Volumes  map[string]yaml.Node       `yaml:"volumes"`
	Networks map[string]*composeNetwork `yaml:"networks"`
	Version  string                     `yaml:"version"`
	Other    map[string]yaml.Node       `yaml:",inline"`
}

type composeService struct {
	Image       string      `yaml:"image"`
	Build       yaml.Node   `yaml:"build"`
	Command     yaml.Node   `yaml:"command"`
	Ports       []yaml.Node `yaml:"ports"`
	Expose      []yaml.Node `yaml:"expose"`
	Environment yaml.Node   `yaml:"environment"`
	DependsOn   yaml.Node   `yaml:"depends_on"`
	Volumes     []yaml.Node `yaml:"volumes"`
	EnvFile     yaml.Node   `yaml:"env_file"`
	Profiles    []string    `yaml:"profiles"`
	Entrypoint  yaml.Node   `yaml:"entrypoint"`
	WorkingDir  string      `yaml:"working_dir"`
	Networks    yaml.Node   `yaml:"networks"`
	User        string      `yaml:"user"`
	Healthcheck struct {
		Test    yaml.Node `yaml:"test"`
		Disable bool      `yaml:"disable"`
	} `yaml:"healthcheck"`
	Deploy struct {
		Replicas  *int `yaml:"replicas"`
		Resources struct {
			Limits struct {
				CPUs   yaml.Node `yaml:"cpus"`
				Memory string    `yaml:"memory"`
			} `yaml:"limits"`
		} `yaml:"resources"`
	} `yaml:"deploy"`
	XXu struct {
		Public      *bool  `yaml:"public"`
		Share       *bool  `yaml:"share"`
		Kind        string `yaml:"kind"`
		MinScale    *int   `yaml:"min_scale"`
		MaxScale    *int   `yaml:"max_scale"`
		Target      *int   `yaml:"target_concurrency"`
		IdleTimeout *int   `yaml:"idle_timeout_sec"`
		HealthPath  string `yaml:"health_path"`
		ProfilePath string `yaml:"profile_path"`
	} `yaml:"x-xu"`
	Other map[string]yaml.Node `yaml:",inline"`
}

// Keys the platform deliberately ignores, with the reason shown to the user.
var ignoredKeys = map[string]string{
	"container_name":    "the platform names containers itself",
	"restart":           "crashed instances are always replaced",
	"hostname":          "services are reached by their service name",
	"labels":            "",
	"logging":           "logs are collected by the platform",
	"stop_grace_period": "",
	"init":              "",
	"stdin_open":        "",
	"tty":               "",
	"pull_policy":       "images are pulled when missing",
	"platform":          "the server's own architecture is used",
}

var (
	varRE       = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:?[-?])?([^}]*)\}`)
	svcNameRE   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	stackNameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,18}[a-z0-9])?$`)
)

// stackTarget is a stack in an environment. Files holds the stack
// folder's .env and env_file contents (path as written → text), read by the
// CLI or from the connected folder; a pasted file has none.
type stackTarget struct {
	Stack, Env string
	Files      map[string]string
}

func (t stackTarget) key() string { return stackKey(t.Stack, t.Env) }

// targetOf finds which stack and environment a stored stack key belongs
// to; an unknown key is a production stack of that name.
func (p *Platform) targetOf(key string) stackTarget {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if r := p.stacks[key]; r != nil {
		return stackTarget{Stack: r.stackName(), Env: envOrDefault(r.Environment), Files: r.Files}
	}
	return stackTarget{Stack: key, Env: DefaultEnvironment}
}

// PlanStack parses a compose file for a production stack. It always
// returns a plan; problems that prevent deployment are in plan.Errors.
func (p *Platform) PlanStack(stack string, src []byte) *StackPlan {
	return p.planStack(stackTarget{Stack: stack, Env: DefaultEnvironment}, src, nil)
}

// PlanStackIn is PlanStack for a stack in an environment.
func (p *Platform) PlanStackIn(stack, env string, src []byte) *StackPlan {
	return p.planStack(stackTarget{Stack: stack, Env: envOrDefault(env)}, src, nil)
}

// planStack also accepts images already built for services that use build:.
func (p *Platform) planStack(t stackTarget, src []byte, built map[string]string) *StackPlan {
	t.Env = envOrDefault(t.Env)
	stack := t.key()
	plan := &StackPlan{Stack: stack, Name: t.Stack, Environment: t.Env, Network: "xu-" + stack,
		Services: []PlanService{}, Edges: []PlanEdge{}, Volumes: []string{}, Order: []string{},
		Errors: []string{}, Warnings: []string{}, Notes: []string{}}
	errf := func(f string, a ...any) { plan.Errors = append(plan.Errors, fmt.Sprintf(f, a...)) }
	warnf := func(f string, a ...any) { plan.Warnings = append(plan.Warnings, fmt.Sprintf(f, a...)) }

	if !stackNameRE.MatchString(t.Stack) {
		errf("stack name %q: use 1-20 lowercase letters, digits and dashes", t.Stack)
	}
	if t.Env != DefaultEnvironment && !envNameRE.MatchString(t.Env) {
		errf("environment name %q: use 1-15 lowercase letters, digits and dashes, starting with a letter", t.Env)
	}
	vars, secrets, envExists := p.envValues(t.Env)
	if !envExists {
		plan.Notes = append(plan.Notes, fmt.Sprintf("Deploying creates the environment %s.", t.Env))
	}

	// Compose variable interpolation, from the .env file next to the stack
	// file, as docker compose does. The server's own environment is never
	// read: a tenant's file must not be able to pull secrets out of xud.
	dotEnv := map[string]string{}
	if text, ok := t.Files[".env"]; ok {
		if m, err := dotenv.Parse(text, nil); err != nil {
			errf(".env: %v", err)
		} else {
			dotEnv = m
		}
	}
	text := varRE.ReplaceAllStringFunc(string(src), func(m string) string {
		g := varRE.FindStringSubmatch(m)
		v, set := dotEnv[g[1]]
		switch g[2] {
		case ":-":
			if v == "" {
				return g[3]
			}
		case "-":
			if !set {
				return g[3]
			}
		case ":?", "?":
			if !set || (g[2] == ":?" && v == "") {
				errf("${%s} must be set: %s (put it in the .env file next to the stack file)", g[1], g[3])
			}
		default:
			if !set {
				warnf("${%s} isn't in a .env file and has no default, so it was left empty. Add it to .env or give a default: ${%s:-value}", g[1], g[1])
			}
		}
		return v
	})
	text = strings.ReplaceAll(text, "$$", "$")

	var f composeFile
	if err := yaml.Unmarshal([]byte(text), &f); err != nil {
		errf("this isn't valid YAML: %v", err)
		return plan
	}
	if len(f.Services) == 0 {
		errf("the file has no services")
		return plan
	}
	if n, ok := f.Other["secrets"]; ok {
		_ = n.Decode(&plan.BuildSecrets) // used by builds; services' secrets: aren't supported
		delete(f.Other, "secrets")
	}
	for k := range f.Other {
		if !strings.HasPrefix(k, "x-") {
			warnf("top-level %q is ignored", k)
		}
	}
	nets := stackNetworks(stack, f.Networks, errf, warnf)
	customNets := len(f.Networks) > 0
	usedNets := map[string]bool{}

	names := make([]string, 0, len(f.Services))
	for n := range f.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	known := map[string]bool{}
	for _, n := range names {
		known[n] = true
	}
	volumes := map[string]bool{}

	refs := &refResolver{p: p, env: t.Env, stack: t.Stack, vars: vars, secrets: secrets, svcs: map[string]svcRef{}}
	for _, name := range names {
		refs.svcs[name] = serviceRef(p, stack, name, f.Services[name])
	}

	for _, name := range names {
		cs := f.Services[name]
		ps := PlanService{Name: name, App: stack + "-" + appSafe(name), Image: cs.Image, Share: cs.XXu.Share}
		if len(cs.Profiles) > 0 {
			plan.Notes = append(plan.Notes, fmt.Sprintf("%s is only in the profile %s, so it isn't deployed (as with docker compose up without --profile).",
				name, strings.Join(cs.Profiles, ", ")))
			continue
		}
		where := "service " + name
		if !svcNameRE.MatchString(name) {
			errf("%s: invalid service name", where)
			continue
		}
		if !nameRE.MatchString(ps.App) {
			errf("%s: the app name %q is too long; shorten the stack or service name", where, ps.App)
		}

		resolve := func(what, s string) (string, bool) {
			v, secret, problems := refs.resolve(s)
			for _, pr := range problems {
				errf("%s: %s: %s", where, what, pr)
			}
			return v, secret
		}

		var netRefs []NetworkRef
		ps.Networks, netRefs = serviceNetworks(name, cs.Networks, nets, where, errf, warnf)
		customNets = customNets || !cs.Networks.IsZero()
		for _, n := range ps.Networks {
			usedNets[n] = true
		}

		// image / build
		if cs.Image != "" {
			cs.Image, _ = resolve("image", cs.Image)
			ps.Image = cs.Image
		}
		pendingBuild := false
		if !cs.Build.IsZero() {
			b, err := parseBuild(cs.Build)
			if err != nil {
				errf("%s: build: %v", where, err)
			} else {
				ps.Build = b
			}
		}
		if img, ok := built[name]; ok {
			cs.Image, ps.Image = img, img
		} else if ps.Build != nil {
			pendingBuild = true
			cs.Image = ""
			ps.Image = "built from " + ps.Build.Context
		}
		if cs.Image == "" && ps.Build == nil && cs.Build.IsZero() {
			errf("%s has no image", where)
		}

		for k := range cs.Other {
			if strings.HasPrefix(k, "x-") {
				continue
			}
			if why, ok := ignoredKeys[k]; ok {
				if why != "" {
					warnf("%s: %s is ignored (%s)", where, k, why)
				} else {
					warnf("%s: %s is ignored", where, k)
				}
			} else {
				warnf("%s: %s isn't supported and is ignored", where, k)
			}
		}

		// ports: the published (host) side is ignored; the router owns it.
		// What it says about who may connect is kept: "3000:3000" (or
		// "0.0.0.0:3000:3000") opens the port to the network, so the service
		// is public; "127.0.0.1:3000:3000" opens it only to this computer,
		// so the service stays internal, as with docker compose.
		var containerPorts []int
		open, loopOnly := 0, 0
		for _, n := range cs.Ports {
			port, err := parsePort(n)
			if err != nil {
				errf("%s: port %s", where, err)
				continue
			}
			containerPorts = append(containerPorts, port)
			if loopbackHost(portHostIP(n)) {
				loopOnly++
			} else {
				open++
			}
		}
		for _, n := range cs.Expose {
			if port, err := parsePort(n); err == nil {
				containerPorts = append(containerPorts, port)
			} else {
				errf("%s: expose %s", where, err)
			}
		}
		ps.Public = open > 0
		if loopOnly > 0 && open == 0 && cs.XXu.Public == nil {
			plan.Notes = append(plan.Notes, fmt.Sprintf("%s publishes its port on 127.0.0.1 only, so it's internal: other services reach it inside the stack, not from the internet. Use \"%d:%d\" to make it public.", name, containerPorts[0], containerPorts[0]))
		}
		if cs.XXu.Public != nil {
			ps.Public = *cs.XXu.Public
		}
		if len(containerPorts) > 0 {
			ps.Port = containerPorts[0]
		}
		fromImage := len(containerPorts) == 0 && ps.Build == nil && cs.Image != "" && cs.XXu.Kind != "worker"
		if fromImage {
			// No ports: listens on what its image EXPOSEs, as with docker compose.
			if port := p.knownImagePort(cs.Image); port > 0 {
				ps.Port = port
				plan.Notes = append(plan.Notes, fmt.Sprintf("%s listens on port %d, from its image (EXPOSE).", name, port))
			}
		}
		if ps.Public && ps.Port == 0 {
			if fromImage {
				plan.Notes = append(plan.Notes, fmt.Sprintf("%s's port is read from its image (EXPOSE) when it's deployed.", name))
			} else {
				errf("%s is public but has no port; add ports: [\"<port it listens on>\"]", where)
			}
		}
		if len(cs.Ports) > 1 {
			warnf("%s: only port %d is served on the web; other ports are reachable inside the stack", where, ps.Port)
		}

		env, keys := parseEnv(cs.Environment, where, warnf)
		var secretKeys []string
		// env_file: values under environment: win, as in docker compose.
		// They often hold passwords, so the API shows them masked.
		for _, ef := range envFiles(cs.EnvFile, where, errf) {
			text, ok := t.Files[ef.path]
			if !ok {
				if ef.required {
					if t.Files == nil {
						errf("%s: env_file %s can't be read from a pasted file; deploy from its folder (Monolith → Connect folder, or xu stack deploy)", where, ef.path)
					} else {
						errf("%s: env_file %s doesn't exist next to the stack file", where, ef.path)
					}
				}
				continue
			}
			m, err := dotenv.Parse(text, nil)
			if err != nil {
				errf("%s: env_file %s: %v", where, ef.path, err)
				continue
			}
			for k, v := range m {
				if _, set := env[k]; set {
					continue
				}
				env[k] = v
				keys = append(keys, k)
				secretKeys = append(secretKeys, k)
			}
		}
		sort.Strings(keys)
		ps.EnvKeys = keys
		refEdges := map[string]string{} // service → first variable that refers to it
		for _, k := range keys {
			refs.used = map[string]bool{}
			v, secret := resolve(k, env[k])
			env[k] = v
			if secret {
				secretKeys = append(secretKeys, k)
			}
			for other := range refs.used {
				if _, seen := refEdges[other]; !seen && other != name {
					refEdges[other] = k
				}
			}
		}
		refs.used = nil
		for _, other := range sortedKeys(refEdges) {
			if !hasEdge(plan.Edges, name, other) {
				plan.Edges = append(plan.Edges, PlanEdge{From: name, To: other, Kind: "uses", Via: refEdges[other]})
			}
		}
		if ps.Build != nil {
			for k, v := range ps.Build.Args {
				rv, secret := resolve("build arg "+k, v)
				if secret {
					errf("%s: build arg %s uses a secret, which would be baked into the image where anyone with the image can read it; pass it under environment: instead", where, k)
				}
				ps.Build.Args[k] = rv
			}
			for _, k := range keys {
				if _, set := ps.Build.Args[k]; set || !hasPublicPrefix(k) {
					continue
				}
				if slicesContains(secretKeys, k) {
					errf("%s: %s is baked into the browser code when it's built, so it can't come from a secret", where, k)
					continue
				}
				if ps.Build.Args == nil {
					ps.Build.Args = map[string]string{}
				}
				ps.Build.Args[k] = env[k]
			}
		}

		args, err := parseCommand(cs.Command)
		if err != nil {
			errf("%s: command %v", where, err)
		}
		entrypoint, err := parseCommand(cs.Entrypoint)
		if err != nil {
			errf("%s: entrypoint %v", where, err)
		}
		for i, a := range args {
			if v, secret := resolve("command", a); secret {
				errf("%s: the command can't use secrets (it's shown in the dashboard); read them from the environment instead", where)
			} else {
				args[i] = v
			}
		}

		deps, err := parseDependsOn(cs.DependsOn)
		if err != nil {
			errf("%s: depends_on %v", where, err)
		}
		var runDeps []Dependency
		for _, d := range deps {
			if !known[d.name] {
				if !d.optional {
					errf("%s depends on %q, which isn't a service in this file", where, d.name)
				}
				continue
			}
			if d.condition == "service_completed_successfully" {
				warnf("%s: depends_on %s: condition service_completed_successfully is treated as service_healthy (%s must be running and healthy); run one-off jobs such as migrations in the service's own command", where, d.name, d.name)
			}
			ps.DependsOn = append(ps.DependsOn, d.name)
			plan.Edges = append(plan.Edges, PlanEdge{From: name, To: d.name, Kind: "depends_on"})
			runDeps = append(runDeps, Dependency{App: stack + "-" + appSafe(d.name), Optional: d.optional})
		}

		for i, n := range cs.Volumes {
			vm, skip, err := parseVolume(n, stack, name, i)
			if err != nil {
				errf("%s: volume %v", where, err)
				continue
			}
			if skip != "" {
				warnf("%s: %s", where, skip)
				continue
			}
			ps.Volumes = append(ps.Volumes, vm)
			volumes[vm.Volume] = true
		}

		memMB, err := parseMemory(cs.Deploy.Resources.Limits.Memory)
		if err != nil {
			errf("%s: memory limit %v", where, err)
		}
		var cpus float64
		if !cs.Deploy.Resources.Limits.CPUs.IsZero() {
			if cpus, err = strconv.ParseFloat(cs.Deploy.Resources.Limits.CPUs.Value, 64); err != nil {
				errf("%s: cpus limit %q is not a number", where, cs.Deploy.Resources.Limits.CPUs.Value)
			}
		}

		// Scaling. Only public services are measured by the router, so only
		// they autoscale. Services with volumes keep a fixed count.
		replicas := 1
		if cs.Deploy.Replicas != nil {
			replicas = max(1, *cs.Deploy.Replicas)
		}
		x := cs.XXu
		hasX := x.MinScale != nil || x.MaxScale != nil || x.Target != nil
		switch {
		case len(ps.Volumes) > 0:
			if replicas > 1 {
				warnf("%s has volumes, so it runs as exactly 1 instance (replicas: %d ignored)", where, replicas)
			}
			ps.Min, ps.Max = 1, 1
		case !ps.Public:
			if hasX {
				warnf("%s: x-xu scaling only applies to public services; it runs %d fixed instance(s)", where, replicas)
			}
			ps.Min, ps.Max = replicas, replicas
		case hasX:
			ps.Min, ps.Max = 0, 10
			if x.MinScale != nil {
				ps.Min = *x.MinScale
			}
			if x.MaxScale != nil {
				ps.Max = *x.MaxScale
			}
		case cs.Deploy.Replicas != nil:
			ps.Min, ps.Max = replicas, replicas
		default:
			ps.Min, ps.Max = 0, 10
		}
		ps.Target = 50
		if x.Target != nil {
			ps.Target = *x.Target
		}
		ps.Scaling = describeScaling(ps)

		ps.Spec = AppSpec{
			Name: ps.App, Image: cs.Image, Port: ps.Port, Env: env, Args: args,
			MemoryMB: memMB, CPUs: cpus,
			MinScale: ps.Min, MaxScale: ps.Max, Target: ps.Target,
			HealthPath: x.HealthPath, ProfilePath: x.ProfilePath,
			Stack: stack, Service: name, Network: plan.Network, Networks: netRefs, DependsOn: runDeps,
			Internal: !ps.Public, Volumes: ps.Volumes,
			Environment: environmentField(t.Env), SecretKeys: secretKeys,
			Entrypoint: entrypoint, WorkingDir: cs.WorkingDir, User: cs.User,
		}
		// healthcheck: an HTTP check's path becomes the health path.
		if x.HealthPath == "" && !cs.Healthcheck.Disable {
			if path := healthPathOf(cs.Healthcheck.Test); path != "" {
				ps.Spec.HealthPath = path
			}
		}
		if x.IdleTimeout != nil {
			ps.Spec.IdleTimeoutSec = *x.IdleTimeout
		}
		switch x.Kind {
		case "", "database", "cache", "api", "web", "worker":
		case "backend":
			x.Kind = "api"
		default:
			errf("%s: x-xu kind %q: use database, cache, api, web or worker", where, x.Kind)
		}
		ps.Spec.Kind = x.Kind
		if ps.Spec.Kind == "" && ps.Build != nil && !ps.Public && ps.Port == 0 {
			ps.Spec.Kind = "worker" // built, but serves nothing: celery, queue consumers
		}
		if ps.Spec.Kind == "" && ps.Build != nil {
			ps.Spec.Kind = "api" // a built service with public build values is a front end
			for k := range ps.Build.Args {
				if hasPublicPrefix(k) {
					ps.Spec.Kind = "web"
				}
			}
		}
		ps.Kind = kindOf(ps.Spec)
		if err := ps.Spec.Normalize(); err != nil && cs.Image != "" && !(fromImage && ps.Port == 0) {
			errf("%s: %v", where, err)
		}
		if ps.Public {
			ps.URL = p.appURL(ps.App)
		}
		if ps.Share != nil && *ps.Share {
			switch {
			case !ps.Public:
				errf("%s: share: true needs a public service; add ports:", where)
			default:
				if u, ok := p.tunnelURLFor(ps.App); ok {
					ps.SharedURL = u
				} else {
					warnf("%s: share: true, but this computer isn't connected to xu526 tunnel yet (dashboard: Settings → Connect with xu526 tunnel). Until it is, ${{ services.%s.url }} is the local address", where, name)
				}
			}
		}
		if pendingBuild {
			plan.Notes = append(plan.Notes, fmt.Sprintf("%s is built from %s when you run xu stack deploy in the folder with this file.", name, ps.Build.Context))
		}
		plan.Services = append(plan.Services, ps)
	}

	// Connections inferred from environment values (DATABASE_URL=...@db:5432).
	for i := range plan.Services {
		s := &plan.Services[i]
		for _, key := range s.EnvKeys {
			val := s.Spec.Env[key]
			for _, other := range names {
				if other == s.Name || !mentionsHost(val, other) || hasEdge(plan.Edges, s.Name, other) {
					continue
				}
				plan.Edges = append(plan.Edges, PlanEdge{From: s.Name, To: other, Kind: "uses", Via: key})
			}
		}
	}

	if customNets {
		// A connection found in the environment between services that share
		// no network won't work, as with docker compose.
		netsOf := map[string][]string{}
		for _, s := range plan.Services {
			netsOf[s.Name] = s.Networks
		}
		for _, e := range plan.Edges {
			if e.Kind == "uses" && !shareNetwork(netsOf[e.From], netsOf[e.To]) {
				warnf("%s can't reach %s (%s mentions it): they share no network. %s is on %s, %s on %s",
					e.From, e.To, e.Via, e.From, strings.Join(netsOf[e.From], ", "), e.To, strings.Join(netsOf[e.To], ", "))
			}
		}
		for i := range plan.Services {
			if sp := &plan.Services[i].Spec; len(sp.Networks) > 0 {
				sp.Network = sp.Networks[0].Name
			}
		}
		plan.Notes = append(plan.Notes, describeNetworks(nets, usedNets))
		for _, s := range plan.Services {
			if onlyInternal(s.Spec.Networks) {
				plan.Notes = append(plan.Notes, fmt.Sprintf("%s is only on internal networks, so it has no way to the internet, and can't reach databases, caches or storage made in the dashboard.", s.Name))
			}
		}
	} else {
		for i := range plan.Services {
			plan.Services[i].Spec.Networks = nil // one network: as before
			plan.Services[i].Networks = nil
		}
	}

	for v := range volumes {
		plan.Volumes = append(plan.Volumes, v)
	}
	sort.Strings(plan.Volumes)

	order, cycle := deployOrder(plan.Services, nil)
	if cycle != "" {
		errf("depends_on has a loop: %s", cycle)
	} else {
		// Also start services found in the environment first (api's CACHE_URL
		// points at cache, so cache goes first), unless that creates a loop.
		soft := map[string][]string{}
		for _, e := range plan.Edges {
			if e.Kind == "uses" {
				soft[e.From] = append(soft[e.From], e.To)
			}
		}
		if better, c := deployOrder(plan.Services, soft); c == "" {
			order = better
		}
	}
	plan.Order = order
	if len(plan.Errors) == 0 && !customNets {
		plan.Notes = append(plan.Notes, "Services reach each other by service name on the private network "+plan.Network+".")
	}
	return plan
}

// environmentField is what AppSpec.Environment holds: "" for production,
// so apps from before environments existed and new ones look the same.
func environmentField(env string) string {
	if env == DefaultEnvironment {
		return ""
	}
	return env
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func hasPublicPrefix(k string) bool {
	for _, p := range publicPrefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

func slicesContains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// parseBuild reads build: as a context path or {context, dockerfile, target, args}.
func parseBuild(n yaml.Node) (*PlanBuild, error) {
	b := &PlanBuild{}
	switch n.Kind {
	case yaml.ScalarNode:
		b.Context = n.Value
	case yaml.MappingNode:
		var long struct {
			Context    string    `yaml:"context"`
			Dockerfile string    `yaml:"dockerfile"`
			Target     string    `yaml:"target"`
			Args       yaml.Node `yaml:"args"`
		}
		if err := n.Decode(&long); err != nil {
			return nil, err
		}
		var all map[string]any
		if err := n.Decode(&all); err == nil {
			for k, v := range all {
				switch k {
				case "context", "dockerfile", "target", "args":
				default:
					if !strings.HasPrefix(k, "x-") {
						if b.Extra == nil {
							b.Extra = map[string]any{}
						}
						b.Extra[k] = v
					}
				}
			}
		}
		b.Context, b.Dockerfile, b.Target = long.Context, long.Dockerfile, long.Target
		switch long.Args.Kind {
		case 0:
		case yaml.MappingNode:
			b.Args = map[string]string{}
			for i := 0; i+1 < len(long.Args.Content); i += 2 {
				b.Args[long.Args.Content[i].Value] = long.Args.Content[i+1].Value
			}
		case yaml.SequenceNode:
			b.Args = map[string]string{}
			for _, item := range long.Args.Content {
				k, v, _ := strings.Cut(item.Value, "=")
				b.Args[k] = v
			}
		default:
			return nil, fmt.Errorf("args must be a map or a list of NAME=value")
		}
	default:
		return nil, fmt.Errorf("must be a folder or {context: folder}")
	}
	if b.Context == "" {
		b.Context = "."
	}
	if strings.HasPrefix(b.Context, "http://") || strings.HasPrefix(b.Context, "https://") || strings.HasPrefix(b.Context, "git@") {
		return nil, fmt.Errorf("context %q: only folders next to the stack file can be built", b.Context)
	}
	return b, nil
}

func appSafe(s string) string {
	return strings.Trim(strings.NewReplacer("_", "-", ".", "-").Replace(strings.ToLower(s)), "-")
}

func describeScaling(ps PlanService) string {
	switch {
	case ps.Min == ps.Max:
		return fmt.Sprintf("%d fixed", ps.Min)
	case ps.Min == 0:
		return fmt.Sprintf("0 to %d, scales to zero", ps.Max)
	default:
		return fmt.Sprintf("%d to %d", ps.Min, ps.Max)
	}
}

func hasEdge(edges []PlanEdge, from, to string) bool {
	for _, e := range edges {
		if e.From == from && e.To == to {
			return true
		}
	}
	return false
}

// mentionsHost reports whether value refers to host as a network name, e.g.
// "postgres://u:p@db:5432/x", "http://api:8080", "cache:6379" or just "db".
func mentionsHost(value, host string) bool {
	re := regexp.MustCompile(`(?i)(^|[/@=,\s])` + regexp.QuoteMeta(host) + `($|[:/,\s])`)
	return re.MatchString(value)
}

// deployOrder sorts services so dependencies start first.
func deployOrder(svcs []PlanService, extra map[string][]string) ([]string, string) {
	deps := map[string][]string{}
	for _, s := range svcs {
		deps[s.Name] = append(append([]string(nil), s.DependsOn...), extra[s.Name]...)
	}
	var order []string
	state := map[string]int{} // 1 visiting, 2 done
	var visit func(n string, path []string) string
	visit = func(n string, path []string) string {
		switch state[n] {
		case 1:
			return strings.Join(append(path, n), " → ")
		case 2:
			return ""
		}
		state[n] = 1
		ds := append([]string(nil), deps[n]...)
		sort.Strings(ds)
		for _, d := range ds {
			if c := visit(d, append(path, n)); c != "" {
				return c
			}
		}
		state[n] = 2
		order = append(order, n)
		return ""
	}
	for _, s := range svcs {
		if c := visit(s.Name, nil); c != "" {
			return order, c
		}
	}
	return order, ""
}

// portHostIP is the host address a ports: entry publishes on: "127.0.0.1"
// in "127.0.0.1:3000:3000", "::1" in "[::1]:3000:3000", host_ip in the long
// form, "" when it doesn't say (all addresses).
func portHostIP(n yaml.Node) string {
	if n.Kind == yaml.MappingNode {
		var long struct {
			HostIP string `yaml:"host_ip"`
		}
		_ = n.Decode(&long)
		return strings.Trim(long.HostIP, "[]")
	}
	raw := strings.SplitN(n.Value, "/", 2)[0]
	if strings.HasPrefix(raw, "[") { // [::1]:3000:3000
		if end := strings.Index(raw, "]"); end > 0 {
			return raw[1:end]
		}
	}
	parts := strings.Split(raw, ":")
	if len(parts) == 3 {
		return parts[0]
	}
	if len(parts) > 3 { // an IPv6 address without brackets: ::1:3000:3000
		return strings.Join(parts[:len(parts)-2], ":")
	}
	return ""
}

// loopbackHost: an address only this computer can reach.
func loopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// parsePort returns the container side of "8080:80", "80", "127.0.0.1:80:80/tcp",
// a bare number, or the long form {target: 80}.
func parsePort(n yaml.Node) (int, error) {
	raw := n.Value
	if n.Kind == yaml.MappingNode {
		var long struct {
			Target int `yaml:"target"`
		}
		if err := n.Decode(&long); err != nil || long.Target == 0 {
			return 0, fmt.Errorf("needs a target port")
		}
		return long.Target, nil
	}
	raw = strings.SplitN(raw, "/", 2)[0]
	parts := strings.Split(raw, ":")
	last := parts[len(parts)-1]
	if strings.Contains(last, "-") {
		return 0, fmt.Errorf("%q: port ranges aren't supported", n.Value)
	}
	port, err := strconv.Atoi(last)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("%q isn't a valid port", n.Value)
	}
	return port, nil
}

func parseEnv(n yaml.Node, where string, warnf func(string, ...any)) (map[string]string, []string) {
	env := map[string]string{}
	set := func(k, v string, hasValue bool) {
		if !hasValue {
			warnf("%s: %s has no value; it is set to empty (values are never taken from the server)", where, k)
		}
		env[k] = v
	}
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			v := n.Content[i+1]
			set(n.Content[i].Value, v.Value, v.Tag != "!!null") // "KEY:" with nothing after it
		}
	case yaml.SequenceNode:
		for _, item := range n.Content {
			k, v, ok := strings.Cut(item.Value, "=")
			set(k, v, ok)
		}
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return env, keys
}

func parseCommand(n yaml.Node) ([]string, error) {
	switch n.Kind {
	case 0:
		return nil, nil
	case yaml.SequenceNode:
		var out []string
		err := n.Decode(&out)
		return out, err
	default:
		return shellSplit(n.Value)
	}
}

// shellSplit splits like a shell would, without running one.
func shellSplit(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	var quote rune
	inWord := false
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("has an unclosed quote")
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out, nil
}

type dependsOn struct {
	name      string
	condition string // service_started, service_healthy, service_completed_successfully
	optional  bool   // required: false
}

// parseDependsOn reads depends_on: a list of services, or a map of
// service → {condition, required, restart}. xu526 waits until each is
// healthy (its health check passes) whatever the condition says, which is
// what service_healthy asks and more than service_started does.
func parseDependsOn(n yaml.Node) ([]dependsOn, error) {
	switch n.Kind {
	case 0:
		return nil, nil
	case yaml.SequenceNode:
		var names []string
		if err := n.Decode(&names); err != nil {
			return nil, err
		}
		out := make([]dependsOn, len(names))
		for i, s := range names {
			out[i] = dependsOn{name: s}
		}
		return out, nil
	case yaml.MappingNode:
		var out []dependsOn
		for i := 0; i+1 < len(n.Content); i += 2 {
			d := dependsOn{name: n.Content[i].Value}
			var long struct {
				Condition string `yaml:"condition"`
				Required  *bool  `yaml:"required"`
			}
			if v := n.Content[i+1]; v.Kind == yaml.MappingNode {
				if err := v.Decode(&long); err != nil {
					return nil, fmt.Errorf("%s: %v", d.name, err)
				}
				switch long.Condition {
				case "", "service_started", "service_healthy", "service_completed_successfully":
				default:
					return nil, fmt.Errorf("%s: condition %q: use service_started, service_healthy or service_completed_successfully", d.name, long.Condition)
				}
				d.condition = long.Condition
				d.optional = long.Required != nil && !*long.Required
			}
			out = append(out, d)
		}
		return out, nil
	}
	return nil, fmt.Errorf("must be a list or a map")
}

// parseVolume accepts named volumes only. Host folders are skipped (with a
// reason) because tenants must never mount the server's filesystem.
func parseVolume(n yaml.Node, stack, svc string, i int) (VolumeMount, string, error) {
	var source, target string
	readOnly := false
	if n.Kind == yaml.MappingNode {
		var long struct {
			Type     string `yaml:"type"`
			Source   string `yaml:"source"`
			Target   string `yaml:"target"`
			ReadOnly bool   `yaml:"read_only"`
		}
		if err := n.Decode(&long); err != nil {
			return VolumeMount{}, "", err
		}
		if long.Type != "" && long.Type != "volume" {
			return VolumeMount{}, fmt.Sprintf("%s mount for %s is skipped; only named volumes are allowed", long.Type, long.Target), nil
		}
		source, target, readOnly = long.Source, long.Target, long.ReadOnly
	} else {
		parts := strings.Split(n.Value, ":")
		switch len(parts) {
		case 1:
			target = parts[0]
		case 2:
			source, target = parts[0], parts[1]
		default:
			source, target = parts[0], parts[1]
			readOnly = strings.Contains(parts[2], "ro")
		}
	}
	if !strings.HasPrefix(target, "/") {
		return VolumeMount{}, "", fmt.Errorf("%q: the path inside the container must start with /", n.Value)
	}
	if strings.HasPrefix(source, "/") || strings.HasPrefix(source, ".") || strings.HasPrefix(source, "~") {
		return VolumeMount{}, fmt.Sprintf("host folder %s is not mounted; use a named volume instead (e.g. data:%s)", source, target), nil
	}
	if source == "" {
		source = fmt.Sprintf("%s-data%d", appSafe(svc), i+1)
	}
	return VolumeMount{Volume: "xu-" + stack + "-" + appSafe(source), Path: target, ReadOnly: readOnly}, "", nil
}

// parseMemory understands compose sizes like 512m, 1g, 256M, 1.5G, 1024k.
func parseMemory(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, nil
	}
	s = strings.TrimSuffix(s, "b")
	mult := 1.0 / (1 << 20)
	switch {
	case strings.HasSuffix(s, "k"):
		mult, s = 1.0/1024, s[:len(s)-1]
	case strings.HasSuffix(s, "m"):
		mult, s = 1, s[:len(s)-1]
	case strings.HasSuffix(s, "g"):
		mult, s = 1024, s[:len(s)-1]
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("%q isn't a size like 512m or 1g", s)
	}
	mb := int64(v * mult)
	if mb < 6 && v > 0 {
		return 0, fmt.Errorf("must be at least 6m")
	}
	return mb, nil
}

// BuildSpec is a compose service's build: section.
type BuildSpec struct {
	Context    string
	Dockerfile string
}

// composeBuilds lists services that build from source, with their contexts.
func composeBuilds(src []byte) (map[string]BuildSpec, error) {
	var f struct {
		Services map[string]struct {
			Build yaml.Node `yaml:"build"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(src, &f); err != nil {
		return nil, err
	}
	out := map[string]BuildSpec{}
	for name, s := range f.Services {
		switch s.Build.Kind {
		case yaml.ScalarNode:
			out[name] = BuildSpec{Context: s.Build.Value}
		case yaml.MappingNode:
			var b struct {
				Context    string `yaml:"context"`
				Dockerfile string `yaml:"dockerfile"`
			}
			if err := s.Build.Decode(&b); err != nil {
				return nil, fmt.Errorf("service %s: build: %w", name, err)
			}
			if b.Context == "" {
				b.Context = "."
			}
			out[name] = BuildSpec{Context: b.Context, Dockerfile: b.Dockerfile}
		}
	}
	return out, nil
}

type envFileRef struct {
	path     string
	required bool
}

// envFiles reads env_file: a path, a list of paths, or a list of
// {path, required}.
func envFiles(n yaml.Node, where string, errf func(string, ...any)) []envFileRef {
	var out []envFileRef
	switch n.Kind {
	case 0:
	case yaml.ScalarNode:
		out = append(out, envFileRef{n.Value, true})
	case yaml.SequenceNode:
		for _, item := range n.Content {
			switch item.Kind {
			case yaml.ScalarNode:
				out = append(out, envFileRef{item.Value, true})
			case yaml.MappingNode:
				var long struct {
					Path     string `yaml:"path"`
					Required *bool  `yaml:"required"`
				}
				if err := item.Decode(&long); err != nil || long.Path == "" {
					errf("%s: env_file entries need a path", where)
					continue
				}
				out = append(out, envFileRef{long.Path, long.Required == nil || *long.Required})
			}
		}
	default:
		errf("%s: env_file must be a path or a list of paths", where)
	}
	for i := range out {
		out[i].path = cleanRel(out[i].path)
	}
	return out
}

// cleanRel normalises a path relative to the stack file ("./.env" → ".env").
func cleanRel(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	for strings.HasPrefix(p, "./") {
		p = p[2:]
	}
	return p
}

var healthURLRE = regexp.MustCompile(`https?://(?:localhost|127\.0\.0\.1|0\.0\.0\.0)(?::\d+)?(/[^\s"'|;&]*)?`)

// healthPathOf finds the path an HTTP healthcheck calls (curl/wget
// http://localhost:8000/health → /health).
func healthPathOf(test yaml.Node) string {
	var parts []string
	switch test.Kind {
	case yaml.ScalarNode:
		parts = []string{test.Value}
	case yaml.SequenceNode:
		for _, c := range test.Content {
			parts = append(parts, c.Value)
		}
	}
	m := healthURLRE.FindStringSubmatch(strings.Join(parts, " "))
	if m == nil {
		return ""
	}
	if m[1] == "" {
		return "/"
	}
	return m[1]
}

// StackFiles reads what a stack file needs from its folder: the .env file
// and every env_file it names (only files inside the folder).
func StackFiles(dir string, src []byte) map[string]string {
	files := map[string]string{}
	read := func(rel string) {
		full, err := safeJoin(dir, rel)
		if err != nil {
			return
		}
		if b, err := os.ReadFile(full); err == nil && len(b) < 1<<20 {
			files[rel] = string(b)
		}
	}
	read(".env")
	var f struct {
		Services map[string]struct {
			EnvFile yaml.Node `yaml:"env_file"`
		} `yaml:"services"`
	}
	if yaml.Unmarshal(src, &f) == nil {
		for _, svc := range f.Services {
			for _, ef := range envFiles(svc.EnvFile, "", func(string, ...any) {}) {
				read(ef.path)
			}
		}
	}
	return files
}

// ComposeBuildFile is a compose file that only builds: the given services
// of plan (ones with a Dockerfile), each tagged xu/<app>:<tag>, with their
// build values resolved. dir is the stack file's folder; docker compose
// build runs with it as the project folder, so relative paths in the
// build options work as in the original file. It returns the file and
// the image each service gets.
func ComposeBuildFile(plan *StackPlan, dir, tag string, services []string) ([]byte, map[string]string, error) {
	want := map[string]bool{}
	for _, s := range services {
		want[s] = true
	}
	svcs := map[string]any{}
	refs := map[string]string{}
	usesSecrets := false
	for _, s := range plan.Services {
		if !want[s.Name] || s.Build == nil {
			continue
		}
		b := map[string]any{}
		for k, v := range s.Build.Extra {
			b[k] = v
		}
		_, usesSecrets = b["secrets"]
		ctxDir := filepath.Join(dir, filepath.FromSlash(s.Build.Context))
		b["context"] = ctxDir
		if s.Build.Dockerfile != "" {
			b["dockerfile"] = s.Build.Dockerfile
		}
		if s.Build.Target != "" {
			b["target"] = s.Build.Target
		}
		if len(s.Build.Args) > 0 {
			args := map[string]string{}
			for k, v := range s.Build.Args {
				args[k] = strings.ReplaceAll(v, "$", "$$") // already resolved: no more interpolation
			}
			b["args"] = args
		}
		ref := "xu/" + s.App + ":" + tag
		refs[s.Name] = ref
		svcs[s.Name] = map[string]any{"image": ref, "build": b}
	}
	if len(svcs) == 0 {
		return nil, nil, fmt.Errorf("none of %s has a build to run", strings.Join(services, ", "))
	}
	file := map[string]any{"services": svcs}
	if usesSecrets && len(plan.BuildSecrets) > 0 {
		file["secrets"] = plan.BuildSecrets
	}
	out, err := yaml.Marshal(file)
	return out, refs, err
}
