package platform

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"xu526/internal/bundle"
	"xu526/internal/bundle/cache"
	"xu526/internal/bundle/plugins"
	"xu526/internal/bundle/registry"
	"xu526/internal/bundle/runner"
	"xu526/internal/dotenv"
	"xu526/internal/runtime"
)

// Folder sources: an app or stack built from a folder on this computer,
// the way `xu build . --deploy NAME` and `xu stack deploy` work, but started
// from the dashboard. Redeploy rebuilds from the same folder.
//
// Unlike git sources, folders without a Dockerfile are built with xu build
// even without -host-builds: the folder was picked by the person running
// this computer, so it isn't someone else's code.

// FolderRequest connects a folder.
type FolderRequest struct {
	Dir         string            `json:"dir"`
	Kind        string            `json:"kind"` // app (default) or stack
	Name        string            `json:"name"` // app or stack name, without the environment
	Environment string            `json:"environment"`
	AppKind     string            `json:"app_kind"` // api, web or worker ("" = detect)
	Links       []string          `json:"links"`    // databases and caches (app names)
	Env         map[string]string `json:"env"`
	StackFile   string            `json:"stack_file"` // stack: the file in Dir (default xu.stack.yml, then compose files)
}

// ConnectFolder starts building and deploying a folder.
func (p *Platform) ConnectFolder(req FolderRequest) (Source, error) {
	dir, err := filepath.Abs(strings.TrimSpace(req.Dir))
	if err != nil || req.Dir == "" {
		return Source{}, errors.New("choose a folder")
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return Source{}, fmt.Errorf("%s isn't a folder on this computer", dir)
	}
	s, err := p.newTarget(req.Kind, req.Name, req.Environment, req.AppKind, req.Links, req.Env)
	if err != nil {
		return Source{}, err
	}
	env := envOrDefault(s.Environment)
	s.Dir, s.Branch, s.AutoDeploy = dir, "-", false
	if s.Kind == "stack" {
		file := req.StackFile
		if file == "" {
			file = findStackFile(dir)
		}
		if file == "" {
			return Source{}, fmt.Errorf("%s has no xu.stack.yml or compose file", dir)
		}
		if _, err := safeJoin(dir, file); err != nil {
			return Source{}, err
		}
		s.Path = file
	}

	p.mu.Lock()
	if env != DefaultEnvironment {
		if _, _, err := p.ensureEnvLocked(env); err != nil {
			p.mu.Unlock()
			return Source{}, err
		}
	}
	for _, o := range p.sources {
		if o.Kind == s.Kind && o.Target == s.Target {
			s.ID = o.ID
		}
	}
	if s.ID == "" {
		s.ID = randomHex(8)
	}
	cp := s
	p.sources[s.ID] = &cp
	p.mu.Unlock()
	_ = p.save()
	p.event(s.Target, "deploy", "connected to the folder %s", dir)
	p.TriggerBuild(s.ID, ReleaseMeta{Source: "folder"})
	return cp.redacted(), nil
}

// newTarget works out the app or stack a new source deploys to, from the
// name (without the environment) and settings the dashboard sends: the
// same for folders and repositories.
func (p *Platform) newTarget(kind, name, envName, appKind string, links []string, vars map[string]string) (Source, error) {
	env := envOrDefault(envName)
	if env != DefaultEnvironment && !envNameRE.MatchString(env) {
		return Source{}, fmt.Errorf("environment name %q isn't valid", env)
	}
	s := Source{Environment: environmentField(env)}
	switch kind {
	case "", "app":
		if !resourceNameRE.MatchString(name) {
			return Source{}, fmt.Errorf("name %q: use 1-20 lowercase letters, digits and dashes, starting with a letter", name)
		}
		s.Kind, s.Target = "app", resourceKey(name, env)
		switch appKind {
		case "", "api", "web", "worker":
		case "backend":
			appKind = "api"
		default:
			return Source{}, errors.New("app_kind must be api, web or worker")
		}
		for _, l := range links {
			a := p.get(l)
			if a == nil || a.spec.Engine == "" {
				return Source{}, fmt.Errorf("%s isn't a database or cache", l)
			}
			if envOrDefault(a.spec.Environment) != env {
				return Source{}, fmt.Errorf("%s is in the %s environment, not %s", l, envOrDefault(a.spec.Environment), env)
			}
		}
		spec := AppSpec{Name: s.Target, Image: "xu/" + s.Target, Display: name, Kind: appKind,
			Environment: environmentField(env), Links: links, Env: vars}
		if old := p.get(s.Target); old != nil { // reconnecting keeps the app's settings
			o := old.spec
			if o.Stack != "" {
				return Source{}, fmt.Errorf("%s is part of the stack %s", s.Target, o.Stack)
			}
			spec.Port, spec.MinScale, spec.MaxScale, spec.Target = o.Port, o.MinScale, o.MaxScale, o.Target
			spec.IdleTimeoutSec, spec.MemoryMB, spec.CPUs, spec.HealthPath = o.IdleTimeoutSec, o.MemoryMB, o.CPUs, o.HealthPath
			if spec.Env == nil {
				spec.Env = o.Env
			}
			if links == nil {
				spec.Links = o.Links
			}
		}
		check := spec
		if check.Port == 0 {
			check.Port = 8080 // the real port comes from the build
		}
		if err := check.Normalize(); err != nil {
			return Source{}, err
		}
		s.App = &spec
	case "stack":
		if !stackNameRE.MatchString(name) {
			return Source{}, errors.New("stack name: use 1-20 lowercase letters, digits and dashes")
		}
		s.Kind, s.Target, s.StackName = "stack", stackKey(name, env), name
	default:
		return Source{}, errors.New("kind must be app or stack")
	}
	return s, nil
}

func findStackFile(dir string) string {
	for _, c := range []string{"xu.stack.yml", "xu.stack.yaml", "compose.yml", "compose.yaml", "docker-compose.yml", "docker-compose.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, c)); err == nil {
			return c
		}
	}
	return ""
}

// folderBuildAndDeploy is buildAndDeploy for folder sources.
func (p *Platform) folderBuildAndDeploy(ctx context.Context, s *Source, rel *Release, logw io.Writer) error {
	d, ok := p.cfg.Docker.(*runtime.Docker)
	if !ok {
		return errors.New("building needs Docker or Podman")
	}
	if st, err := os.Stat(s.Dir); err != nil || !st.IsDir() {
		return fmt.Errorf("the folder %s isn't there any more", s.Dir)
	}
	tag := time.Now().UTC().Format("20060102-150405")
	switch s.Kind {
	case "app":
		spec := *s.App
		// Values front ends bake in while building come from the app's settings.
		args := map[string]string{}
		for k, v := range spec.Env {
			if hasPublicPrefix(k) {
				args[k] = v
			}
		}
		ref, id, plan, err := p.folderBuild(ctx, d, s.Dir, "", "", s.Target, tag, args, logw)
		if err != nil {
			return err
		}
		spec.Image, spec.ImageID, spec.ImageDigest = ref, id, ""
		if spec.Kind == "worker" {
			// A worker serves nothing: no port, no address, always running.
			spec.Port, spec.Internal = 0, true
			spec.MinScale = max(1, spec.MinScale)
			spec.MaxScale = max(spec.MinScale, spec.MaxScale)
		} else {
			if plan != nil {
				if spec.Port == 0 {
					spec.Port = plan.Port
				}
				if spec.Kind == "" {
					spec.Kind = GuessKind(plan)
				}
			} else if spec.Port == 0 {
				spec.Port = d.ImagePortOf(ctx, id)
			}
			if spec.Port == 0 {
				spec.Port = 8080
			}
			if spec.Kind == "" {
				spec.Kind = "api"
			}
		}
		if vars, read, err := dotenv.Load(s.Dir, dotenv.ProductionFiles); err != nil {
			return fmt.Errorf("read env files: %w", err)
		} else if len(read) > 0 {
			merged := map[string]string{}
			for k, v := range vars {
				merged[k] = v
			}
			for k, v := range spec.Env { // settings from the dashboard win
				merged[k] = v
			}
			spec.Env = merged
			fmt.Fprintf(logw, "\n== %s\n", dotenv.Summary(vars, read))
		}
		p.setReleaseStatus(rel, "deploying")
		fmt.Fprintf(logw, "\n== deploying %s\n", ref)
		pinned, err := p.deployApp(ctx, spec)
		if err == nil {
			p.mu.Lock()
			rel.Spec = &pinned
			p.mu.Unlock()
			// Remember what was worked out, so the next build starts from it.
			p.setSource(s.ID, func(x *Source) {
				if x.App != nil {
					x.App.Port, x.App.Kind = spec.Port, spec.Kind
				}
			})
		}
		p.finishRelease(rel, err)
		return err

	case "stack":
		file, err := safeJoin(s.Dir, s.Path)
		if err != nil {
			return err
		}
		yml, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("read %s: %w", s.Path, err)
		}
		t := stackTarget{Stack: s.StackName, Env: envOrDefault(s.Environment), Files: StackFiles(filepath.Dir(file), yml)}
		if t.Stack == "" {
			t.Stack = s.Target
		}
		plan := p.planStack(t, yml, nil)
		if len(plan.Errors) > 0 {
			return fmt.Errorf("the stack file has problems: %s", strings.Join(plan.Errors, "; "))
		}
		built := map[string]string{}
		// Services with a Dockerfile: one docker compose build for all of
		// them, when docker compose is installed.
		var withDockerfile []string
		for _, svc := range plan.Services {
			if svc.Build == nil {
				continue
			}
			ctxDir, err := safeJoin(s.Dir, svc.Build.Context)
			if err != nil {
				return fmt.Errorf("%s: %w", svc.Name, err)
			}
			if svc.Build.HasDockerfile(ctxDir) {
				withDockerfile = append(withDockerfile, svc.Name)
			}
		}
		if len(withDockerfile) > 0 {
			file, refs, err := ComposeBuildFile(plan, s.Dir, tag, withDockerfile)
			if err != nil {
				return err
			}
			fmt.Fprintf(logw, "\n== building %s with docker compose\n", strings.Join(withDockerfile, ", "))
			switch _, err := d.ComposeBuild(ctx, file, s.Dir, "xu-"+plan.Stack, refs, logw); {
			case errors.Is(err, runtime.ErrNoCompose):
				fmt.Fprintln(logw, "docker compose isn't installed here: building each service on its own")
			case err != nil:
				return err
			default:
				for svc, ref := range refs {
					built[svc] = ref
				}
			}
		}
		for _, svc := range plan.Services {
			if svc.Build == nil || built[svc.Name] != "" {
				continue
			}
			ctxDir, err := safeJoin(s.Dir, svc.Build.Context)
			if err != nil {
				return fmt.Errorf("%s: %w", svc.Name, err)
			}
			fmt.Fprintf(logw, "\n== building %s from %s\n", svc.Name, svc.Build.Context)
			ref, _, _, err := p.folderBuild(ctx, d, ctxDir, svc.Build.Dockerfile, svc.Build.Target, svc.App, tag, svc.Build.Args, logw)
			if err != nil {
				return fmt.Errorf("build %s: %w", svc.Name, err)
			}
			built[svc.Name] = ref
		}
		p.setReleaseStatus(rel, "deploying")
		fmt.Fprintf(logw, "\n== deploying stack %s\n", t.Stack)
		_, err = p.deployStackTo(ctx, t, yml, nil, built, ReleaseMeta{Source: "folder"}, rel)
		return err
	}
	return fmt.Errorf("unknown source kind %q", s.Kind)
}

// folderBuild builds one folder: its Dockerfile with docker/podman build
// (the engine API's builder as a fallback), or else xu build. plan is set
// for xu builds.
func (p *Platform) folderBuild(ctx context.Context, d *runtime.Docker, dir, dockerfile, target, name, tag string,
	args map[string]string, logw io.Writer) (ref, id string, plan *bundle.Plan, err error) {
	ref = "xu/" + name + ":" + tag
	fmt.Fprintf(logw, "\n== building %s from %s\n", ref, dir)
	if hasDockerfile(dir, dockerfile) {
		id, err = d.CLIBuild(ctx, dir, dockerfile, ref, target, args, logw)
		if errors.Is(err, runtime.ErrNoCLI) {
			fmt.Fprintln(logw, "no docker or podman command here: building with the engine's own builder (no RUN --mount)")
			id, err = d.Build(ctx, dir, dockerfile, ref, p.registryConfig(ctx), logw)
		}
		return ref, id, nil, err
	}
	if dockerfile != "" && dockerfile != "Dockerfile" {
		return "", "", nil, fmt.Errorf("%s not found in %s", dockerfile, dir)
	}
	store, err := cache.Open(filepath.Join(p.dataDir, "buildcache"))
	if err != nil {
		return "", "", nil, err
	}
	mirrors, err := bundle.ParseMirrors(os.Getenv("XU_MIRRORS"))
	if err != nil {
		return "", "", nil, fmt.Errorf("XU_MIRRORS: %w", err)
	}
	var benv []string
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		benv = append(benv, k+"="+args[k])
	}
	if path := toolPath(dir); path != "" {
		benv = append(benv, "PATH="+path)
	}
	fmt.Fprintln(logw, "no Dockerfile: building with xu build")
	arch := d.Arch()
	if arch == "" {
		arch = goruntime.GOARCH
	}
	res, err := bundle.Build(ctx, bundle.Request{
		Dir: dir, Tag: ref, Plugins: plugins.All(), Cache: store, Runner: runner.Local{}, Log: logw,
		Registry: registry.New(), Mirrors: mirrors,
		Options: bundle.Options{Platform: bundle.Platform{OS: "linux", Arch: arch}, BuildEnv: benv},
	})
	if err != nil {
		return "", "", nil, err
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(res.Image.WriteTar(pw, ref)) }()
	id, err = d.Load(ctx, pr, ref)
	if err != nil {
		return "", "", nil, err
	}
	return ref, id, res.Plan, nil
}

// toolPath adds the folder owner's own tool folders (nvm's Node, pnpm,
// Go, …) to PATH: the daemon often runs as a service whose PATH has only
// the system's tools, while people install Node in their home folder.
func toolPath(dir string) string {
	home := ownerHome(dir)
	var extra []string
	add := func(d string) {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			extra = append(extra, d)
		}
	}
	if home != "" {
		// nvm: the default version if set, else the newest installed.
		nvm := filepath.Join(home, ".nvm", "versions", "node")
		if alias, err := os.ReadFile(filepath.Join(home, ".nvm", "alias", "default")); err == nil {
			want := strings.TrimPrefix(strings.TrimSpace(string(alias)), "v")
			if entries, _ := os.ReadDir(nvm); len(entries) > 0 {
				for i := len(entries) - 1; i >= 0; i-- {
					if strings.HasPrefix(strings.TrimPrefix(entries[i].Name(), "v"), want) {
						add(filepath.Join(nvm, entries[i].Name(), "bin"))
						break
					}
				}
			}
		}
		if entries, _ := os.ReadDir(nvm); len(entries) > 0 {
			sort.Slice(entries, func(i, j int) bool { return versionLess(entries[j].Name(), entries[i].Name()) })
			add(filepath.Join(nvm, entries[0].Name(), "bin"))
		}
		for _, d := range []string{".volta/bin", ".local/share/pnpm", "Library/pnpm", ".bun/bin", ".local/bin", "go/bin", ".cargo/bin", ".fnm/aliases/default/bin"} {
			add(filepath.Join(home, filepath.FromSlash(d)))
		}
	}
	for _, d := range []string{"/usr/local/go/bin", "/opt/homebrew/bin", "/usr/local/bin"} {
		add(d)
	}
	if len(extra) == 0 {
		return ""
	}
	return strings.Join(append(extra, os.Getenv("PATH")), string(os.PathListSeparator))
}

// ownerHome is the home folder of the user who owns dir (Unix), or this
// process's own home.
func ownerHome(dir string) string {
	if goruntime.GOOS != "windows" {
		if uid, ok := ownerUID(dir); ok && uid != 0 {
			if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
				return u.HomeDir
			}
		}
		// Folders on a Windows drive under WSL are owned by root or the
		// default user; use the user whose home is under /home.
		if sudo := os.Getenv("SUDO_USER"); sudo != "" {
			if u, err := user.Lookup(sudo); err == nil {
				return u.HomeDir
			}
		}
		if homes, _ := filepath.Glob("/home/*"); len(homes) == 1 {
			return homes[0]
		}
	}
	h, _ := os.UserHomeDir()
	return h
}

// versionLess compares v1.2.3-style names numerically.
func versionLess(a, b string) bool {
	pa := strings.Split(strings.TrimPrefix(a, "v"), ".")
	pb := strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x < y
		}
	}
	return len(pa) < len(pb)
}

// ---- folder browser -----------------------------------------------------------

// FolderEntry is a folder in the dashboard's folder browser.
type FolderEntry struct {
	Name    string   `json:"name"`
	Path    string   `json:"path"`
	Markers []string `json:"markers,omitempty"` // package.json, Dockerfile, xu.stack.yml, …
}

// FolderListing is one folder's subfolders.
type FolderListing struct {
	Path    string        `json:"path"`
	Parent  string        `json:"parent,omitempty"`
	Markers []string      `json:"markers"`
	Folders []FolderEntry `json:"folders"`
	Places  []FolderEntry `json:"places"` // home folders, drives
}

var markerFiles = []string{"xu.stack.yml", "docker-compose.yml", "compose.yml", "Dockerfile", "package.json", "go.mod",
	"pyproject.toml", "requirements.txt", "index.html", "Makefile"}

func markersIn(dir string) []string {
	var out []string
	for _, m := range markerFiles {
		if _, err := os.Stat(filepath.Join(dir, m)); err == nil {
			out = append(out, m)
		}
	}
	return out
}

// skipDir hides folders nobody deploys from.
func skipDir(name string) bool {
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "$") {
		return true
	}
	switch name {
	case "node_modules", "__pycache__", "venv", "dist", "build", "target", "proc", "sys", "dev", "boot", "lost+found",
		"System Volume Information", "AppData":
		return true
	}
	return false
}

// ListFolder lists a folder's subfolders for the folder browser.
func ListFolder(path string) (FolderListing, error) {
	if path == "" {
		path, _ = os.UserHomeDir()
		if h := ownerHome("/home"); h != "" && goruntime.GOOS == "linux" {
			path = h
		}
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return FolderListing{}, err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return FolderListing{}, fmt.Errorf("can't open %s: %v", path, err)
	}
	l := FolderListing{Path: path, Markers: markersIn(path), Folders: []FolderEntry{}, Places: places()}
	if parent := filepath.Dir(path); parent != path {
		l.Parent = parent
	}
	for _, e := range entries {
		if !e.IsDir() && e.Type()&os.ModeSymlink == 0 || skipDir(e.Name()) {
			continue
		}
		full := filepath.Join(path, e.Name())
		if st, err := os.Stat(full); err != nil || !st.IsDir() {
			continue
		}
		l.Folders = append(l.Folders, FolderEntry{Name: e.Name(), Path: full, Markers: markersIn(full)})
	}
	sort.Slice(l.Folders, func(i, j int) bool { return strings.ToLower(l.Folders[i].Name) < strings.ToLower(l.Folders[j].Name) })
	return l, nil
}

// places are good starting points: home folders, and on WSL the Windows
// user folders.
func places() []FolderEntry {
	var out []FolderEntry
	seen := map[string]bool{}
	add := func(name, p string) {
		if seen[p] {
			return
		}
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			seen[p] = true
			out = append(out, FolderEntry{Name: name, Path: p})
		}
	}
	if h := ownerHome("/home"); h != "" {
		add("Home", h)
	}
	if h, err := os.UserHomeDir(); err == nil {
		add("Home", h)
	}
	if goruntime.GOOS == "windows" {
		for _, d := range "CDEFG" {
			add(string(d)+":", string(d)+`:\`)
		}
	}
	if users, _ := filepath.Glob("/mnt/c/Users/*"); len(users) > 0 { // WSL
		for _, u := range users {
			b := filepath.Base(u)
			if b == "Public" || b == "Default" || b == "All Users" || b == "Default User" || strings.HasSuffix(b, ".ini") {
				continue
			}
			add("Windows: "+b, u)
		}
	}
	return out
}

// InspectFolder says what xu526 would build from a folder.
type FolderInfo struct {
	Path      string   `json:"path"`
	Name      string   `json:"name"` // suggested app name
	Markers   []string `json:"markers"`
	StackFile string   `json:"stack_file,omitempty"`
	Docker    bool     `json:"dockerfile"`
	Detected  string   `json:"detected,omitempty"` // e.g. "node", "python"
	Summary   []string `json:"summary,omitempty"`
	Kind      string   `json:"kind"` // api or web
	Port      int      `json:"port,omitempty"`
	Problem   string   `json:"problem,omitempty"`
}

func InspectFolder(dir string) (FolderInfo, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return FolderInfo{}, err
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return FolderInfo{}, fmt.Errorf("%s isn't a folder", dir)
	}
	fi := FolderInfo{Path: dir, Name: suggestName(filepath.Base(dir)), Markers: markersIn(dir), StackFile: findStackFile(dir),
		Docker: hasDockerfile(dir, ""), Kind: "api"}
	if fi.Docker {
		fi.Detected = "Dockerfile"
		return fi, nil
	}
	res, err := bundle.Build(context.Background(), bundle.Request{Dir: dir, DryRun: true, Plugins: plugins.All(),
		Options: bundle.Options{Platform: bundle.Platform{OS: "linux", Arch: goruntime.GOARCH}}})
	if err != nil {
		fi.Problem = err.Error()
		return fi, nil
	}
	if res.Detection != nil {
		fi.Detected = res.Detection.Plugin
	}
	fi.Summary, fi.Port, fi.Kind = res.Plan.Summary, res.Plan.Port, GuessKind(res.Plan)
	return fi, nil
}

func suggestName(base string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(base) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	for len(s) > 0 && (s[0] < 'a' || s[0] > 'z') {
		s = s[1:]
	}
	if len(s) > 20 {
		s = strings.Trim(s[:20], "-")
	}
	if s == "" {
		s = "app"
	}
	return s
}

// folderAPI serves the folder browser and folder connections. These read
// this computer's folder names, so they're for the dashboard's owner only,
// like every /v1 route.
//
//	GET  /v1/fs?path=…        subfolders
//	GET  /v1/fs/inspect?path=… what would be built
//	POST /v1/folders          connect a folder (FolderRequest)
func (p *Platform) folderAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/fs", func(w http.ResponseWriter, r *http.Request) {
		l, err := ListFolder(r.URL.Query().Get("path"))
		if err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, l)
	})
	mux.HandleFunc("GET /v1/fs/inspect", func(w http.ResponseWriter, r *http.Request) {
		fi, err := InspectFolder(r.URL.Query().Get("path"))
		if err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, fi)
	})
	mux.HandleFunc("POST /v1/folders", func(w http.ResponseWriter, r *http.Request) {
		var req FolderRequest
		if err := decodeJSON(w, r, &req); err != nil {
			return
		}
		s, err := p.ConnectFolder(req)
		if err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, s)
	})
}
