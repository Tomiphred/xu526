package node

// Monorepos: building one app out of an npm, yarn 1 or pnpm workspace,
// optionally orchestrated by Turborepo or Nx.
//
// The build copies the workspace root's files, the app, and the local
// packages it uses (with their sources); every other workspace package
// contributes only its package.json, so the lockfile still matches. One
// install at the root, then the app and its local packages are built
// (turbo/nx, or each package's build script in dependency order).
//
// The image mirrors the workspace under /app: the app at /app/<app> (the
// working directory), the local packages it needs at runtime at their own
// paths, and a production install filtered to the app, whose links to local
// packages stay relative and inside /app.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"xu526/internal/bundle"
)

type workspace struct {
	Root    string // absolute
	App     string // the app's folder, relative, slash-separated
	AppPkg  *pkgJSON
	RootPkg *pkgJSON
	Pkgs    map[string]string // workspace package name → folder
	Dirs    []string          // every workspace package folder, sorted
	// Local are the folders of local packages the app uses, transitively,
	// dependencies first. LocalProd is the subset reached through
	// dependencies (not devDependencies): what runs in the image.
	Local, LocalProd []string
	Tool             string // "turbo", "nx" or ""
}

// WorkspaceRoot implements bundle.Workspacer: dir is a package of a
// workspace declared in an ancestor folder (package.json "workspaces" or
// pnpm-workspace.yaml). The search stops at the repository root (.git).
func (Plugin) WorkspaceRoot(dir string) (string, bool) {
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err != nil {
		return "", false
	}
	if hasLockfile(dir) {
		return "", false // a project of its own, even if nested
	}
	for d := filepath.Dir(dir); ; d = filepath.Dir(d) {
		if globs, ok := workspaceGlobs(d); ok {
			rel, err := filepath.Rel(d, dir)
			if err != nil || !matchesAny(globs, filepath.ToSlash(rel)) {
				return "", false
			}
			return d, true
		}
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil || d == filepath.Dir(d) {
			return "", false
		}
	}
}

func hasLockfile(dir string) bool {
	for _, f := range []string{"package-lock.json", "npm-shrinkwrap.json", "pnpm-lock.yaml", "yarn.lock"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			return true
		}
	}
	return false
}

// workspaceGlobs reads a workspace's package patterns. ok is false when
// dir isn't a workspace root.
func workspaceGlobs(dir string) ([]string, bool) {
	if b, err := os.ReadFile(filepath.Join(dir, "pnpm-workspace.yaml")); err == nil {
		return yamlList(string(b), "packages"), true
	}
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return nil, false
	}
	var p struct {
		Workspaces json.RawMessage `json:"workspaces"`
	}
	if json.Unmarshal(b, &p) != nil || len(p.Workspaces) == 0 || string(p.Workspaces) == "null" {
		return nil, false
	}
	var list []string
	if json.Unmarshal(p.Workspaces, &list) != nil {
		var obj struct{ Packages []string } // yarn: {"packages": [...], "nohoist": [...]}
		if json.Unmarshal(p.Workspaces, &obj) != nil {
			return nil, false
		}
		list = obj.Packages
	}
	return list, true
}

// yamlList reads a top-level list ("key:" followed by "- item" lines) from
// simple YAML such as pnpm-workspace.yaml.
func yamlList(doc, key string) []string {
	var out []string
	in := false
	sc := bufio.NewScanner(strings.NewReader(doc))
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, " #"); i >= 0 {
			line = line[:i]
		}
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "-") {
			in = strings.TrimSuffix(t, ":") == key && strings.HasSuffix(t, ":")
			continue
		}
		if in && strings.HasPrefix(t, "-") {
			v := strings.TrimSpace(strings.TrimPrefix(t, "-"))
			out = append(out, strings.Trim(v, `"'`))
		}
	}
	return out
}

// globRegexp turns a workspace pattern ("apps/*", "packages/**") into a
// regexp over slash-separated folder paths.
func globRegexp(g string) *regexp.Regexp {
	g = strings.TrimSuffix(strings.TrimPrefix(g, "./"), "/")
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(g); i++ {
		switch c := g[i]; {
		case c == '*' && i+1 < len(g) && g[i+1] == '*':
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

func matchesAny(globs []string, rel string) bool {
	in := false
	for _, g := range globs {
		if neg, ok := strings.CutPrefix(g, "!"); ok {
			if globRegexp(neg).MatchString(rel) {
				return false
			}
			continue
		}
		in = in || globRegexp(g).MatchString(rel)
	}
	return in
}

// loadWorkspace reads the workspace at root and works out what app needs.
func loadWorkspace(root, app string) (*workspace, error) {
	globs, ok := workspaceGlobs(root)
	if !ok {
		return nil, fmt.Errorf("%s isn't a workspace root (no \"workspaces\" in package.json, no pnpm-workspace.yaml)", root)
	}
	ws := &workspace{Root: root, App: app, Pkgs: map[string]string{}}
	var err error
	if ws.RootPkg, err = readPkgFile(filepath.Join(root, "package.json")); err != nil {
		return nil, err
	}
	pkgs := map[string]*pkgJSON{}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if n := d.Name(); n == "node_modules" || strings.HasPrefix(n, ".") || strings.Count(rel, "/") > 6 {
			return fs.SkipDir
		}
		if !matchesAny(globs, rel) {
			return nil
		}
		pj, err := readPkgFile(filepath.Join(p, "package.json"))
		if err != nil {
			return nil // a matching folder without a package.json isn't a package
		}
		ws.Dirs = append(ws.Dirs, rel)
		pkgs[rel] = pj
		if pj.Name != "" {
			if other, dup := ws.Pkgs[pj.Name]; dup {
				return fmt.Errorf("two workspace packages are named %s: %s and %s", pj.Name, other, rel)
			}
			ws.Pkgs[pj.Name] = rel
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(ws.Dirs)
	if ws.AppPkg = pkgs[app]; ws.AppPkg == nil {
		return nil, fmt.Errorf("%s isn't one of this workspace's packages (patterns: %s)", app, strings.Join(globs, ", "))
	}

	// Local packages, dependencies first (depth-first, post-order).
	walk := func(start string, withDev bool) ([]string, error) {
		var order []string
		state := map[string]int{} // 1 visiting, 2 done
		var visit func(dir string, chain []string) error
		visit = func(dir string, chain []string) error {
			switch state[dir] {
			case 1:
				return fmt.Errorf("local packages depend on each other in a loop: %s", strings.Join(append(chain, dir), " → "))
			case 2:
				return nil
			}
			state[dir] = 1
			pj := pkgs[dir]
			names := depNames(pj, withDev || dir == start)
			for _, n := range names {
				if d, ok := ws.Pkgs[n]; ok && d != start {
					if err := visit(d, append(chain, dir)); err != nil {
						return err
					}
				}
			}
			state[dir] = 2
			if dir != start {
				order = append(order, dir)
			}
			return nil
		}
		return order, visit(start, nil)
	}
	if ws.Local, err = walk(app, true); err != nil {
		return nil, err
	}
	// Runtime: the app's dependencies (not devDependencies), transitively.
	ws.LocalProd = walkProd(pkgs, ws.Pkgs, ws.AppPkg, app)

	switch {
	case exists(filepath.Join(root, "turbo.json")) && ws.RootPkg.has("turbo"):
		ws.Tool = "turbo"
	case exists(filepath.Join(root, "nx.json")) && ws.RootPkg.has("nx"):
		ws.Tool = "nx"
	}
	return ws, nil
}

// walkProd lists the local packages app reaches through runtime
// dependencies only, dependencies first.
func walkProd(pkgs map[string]*pkgJSON, byName map[string]string, app *pkgJSON, appDir string) []string {
	var order []string
	seen := map[string]bool{appDir: true}
	var visit func(p *pkgJSON)
	visit = func(p *pkgJSON) {
		for _, n := range depNames(p, false) {
			d, ok := byName[n]
			if !ok || seen[d] {
				continue
			}
			seen[d] = true
			visit(pkgs[d])
			order = append(order, d)
		}
	}
	visit(app)
	return order
}

// depNames lists a package's dependency names, sorted (dev ones too when
// withDev).
func depNames(p *pkgJSON, withDev bool) []string {
	var n []string
	for _, m := range []map[string]string{p.Dependencies, p.OptionalDependencies, p.PeerDependencies} {
		for k := range m {
			n = append(n, k)
		}
	}
	if withDev {
		for k := range p.DevDependencies {
			n = append(n, k)
		}
	}
	sort.Strings(n)
	return n
}

func readPkgFile(p string) (*pkgJSON, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var pj pkgJSON
	if err := json.Unmarshal(b, &pj); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return &pj, nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// included are the package folders whose sources the build needs.
func (ws *workspace) included() map[string]bool {
	in := map[string]bool{ws.App: true}
	for _, d := range ws.Local {
		in[d] = true
	}
	return in
}

// excluded are the other package folders: only their package.json is used.
func (ws *workspace) excluded() []string {
	in := ws.included()
	var out []string
	for _, d := range ws.Dirs {
		if !in[d] {
			out = append(out, d)
		}
	}
	return out
}

// manifests are every package.json the install reads, plus the files that
// configure it.
func (ws *workspace) manifests(pm *manager) []string {
	m := []string{"package.json", pm.Lockfile, ".npmrc", ".yarnrc", "pnpm-workspace.yaml"}
	for _, d := range ws.Dirs {
		m = append(m, d+"/package.json")
	}
	return m
}

// name is how package managers and turbo/nx select the app.
func (ws *workspace) name() string {
	if ws.AppPkg.Name != "" {
		return ws.AppPkg.Name
	}
	return "./" + ws.App
}

// pkgName is the name of the package in folder dir.
func (ws *workspace) pkgName(dir string) string {
	for n, d := range ws.Pkgs {
		if d == dir {
			return n
		}
	}
	return "./" + dir
}

// buildSteps builds the app and the local packages it uses.
func (ws *workspace) buildSteps(pm *manager, installEnv []string, extraEnv ...string) []bundle.Step {
	env := append(append(append([]string{}, installEnv...), "NODE_ENV=production"), extraEnv...)
	exec := func(args ...string) []string {
		switch pm.Name {
		case "pnpm":
			return append([]string{"pnpm", "exec"}, args...)
		case "yarn":
			return append([]string{"yarn", "run", "--silent"}, args...)
		}
		return append([]string{"npx", "--no-install"}, args...)
	}
	runIn := func(dir string) []string {
		switch pm.Name {
		case "pnpm":
			return []string{"pnpm", "--filter", ws.pkgName(dir), "run", "build"}
		case "yarn":
			return []string{"yarn", "workspace", ws.pkgName(dir), "run", "build"}
		}
		return []string{"npm", "run", "build", "--workspace", dir}
	}
	switch ws.Tool {
	case "turbo":
		return []bundle.Step{{Name: "build", Cmd: exec("turbo", "run", "build", "--filter="+ws.name()+"..."),
			Env: append(env, "TURBO_TELEMETRY_DISABLED=1", "TURBO_NO_UPDATE_NOTIFIER=1", "DO_NOT_TRACK=1")}}
	case "nx":
		// Every project that has a build target, the app last: nx only
		// follows dependsOn from targets it runs, and the app may have none.
		var projects []string
		for _, d := range append(append([]string{}, ws.Local...), ws.App) {
			if ws.hasBuild(d) {
				projects = append(projects, ws.pkgName(d))
			}
		}
		if len(projects) == 0 {
			return nil
		}
		return []bundle.Step{{Name: "build", Cmd: exec("nx", "run-many", "-t", "build", "-p", strings.Join(projects, ",")),
			Env: append(env, "NX_DAEMON=false", "NX_NO_CLOUD=true", "NX_SKIP_NX_CACHE=true", "NX_TUI=false")}}
	}
	var steps []bundle.Step
	for _, d := range ws.Local {
		if pj, err := readPkgFile(filepath.Join(ws.Root, d, "package.json")); err == nil && pj.Scripts["build"] != "" {
			steps = append(steps, bundle.Step{Name: "build " + d, Cmd: runIn(d), Env: env})
		}
	}
	if ws.AppPkg.Scripts["build"] != "" {
		steps = append(steps, bundle.Step{Name: "build", Cmd: runIn(ws.App), Env: env})
	}
	return steps
}

// hasBuild reports whether the package in dir has a build script, or an Nx
// project.json that may define a build target.
func (ws *workspace) hasBuild(dir string) bool {
	if pj, err := readPkgFile(filepath.Join(ws.Root, dir, "package.json")); err == nil && pj.Scripts["build"] != "" {
		return true
	}
	b, err := os.ReadFile(filepath.Join(ws.Root, dir, "project.json"))
	return err == nil && strings.Contains(string(b), `"build"`)
}

// prodInstall installs the app's production dependencies (and links its
// local packages) from the workspace lockfile.
func (ws *workspace) prodInstall(pm *manager) []string {
	switch pm.Name {
	case "pnpm":
		return []string{"pnpm", "install", "--frozen-lockfile", "--prod", "--filter", ws.name() + "...", "--config.node-linker=hoisted"}
	case "yarn": // yarn 1 can't filter: every workspace's production dependencies
		return []string{"yarn", "install", "--frozen-lockfile", "--production", "--non-interactive"}
	}
	return []string{"npm", "ci", "--omit=dev", "--workspace", ws.App, "--no-audit", "--no-fund"}
}

// copyForBuild copies the workspace into dst: everything at the root, the
// app and its local packages in full, and only package.json (and
// project.json, for Nx) of the others.
func (ws *workspace) copyForBuild(dst string) error {
	ex := map[string]bool{}
	for _, d := range ws.excluded() {
		ex[d] = true
	}
	err := bundle.CopyTree(ws.Root, dst, func(rel string, d fs.DirEntry) bool {
		return skipCopy(rel, d) || (d.IsDir() && ex[rel]) || (d.IsDir() && strings.HasSuffix(rel, "/.next"))
	}, nil)
	if err != nil {
		return err
	}
	for d := range ex {
		for _, f := range []string{"package.json", "project.json"} {
			if b, err := os.ReadFile(filepath.Join(ws.Root, d, f)); err == nil {
				if err := os.MkdirAll(filepath.Join(dst, d), 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(dst, d, f), b, 0o644); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// installProd makes WorkDir/deps: the manifests and lockfile only, with
// scripts removed, then the filtered production install.
func (ws *workspace) installProd(ctx context.Context, env *bundle.BuildEnv, s bundle.Step) (string, error) {
	deps := filepath.Join(env.WorkDir, "deps")
	for _, f := range []string{"package.json", "package-lock.json", "npm-shrinkwrap.json", "pnpm-lock.yaml", "yarn.lock", ".npmrc", ".yarnrc", "pnpm-workspace.yaml"} {
		if b, err := os.ReadFile(filepath.Join(ws.Root, f)); err == nil {
			if err := writeFile(filepath.Join(deps, f), b); err != nil {
				return "", err
			}
		}
	}
	if err := stripScripts(filepath.Join(deps, "package.json"), env.Options.Platform); err != nil {
		return "", err
	}
	for _, d := range ws.Dirs {
		b, err := os.ReadFile(filepath.Join(ws.Root, d, "package.json"))
		if err != nil {
			return "", err
		}
		p := filepath.Join(deps, d, "package.json")
		if err := writeFile(p, b); err != nil {
			return "", err
		}
		if err := removeScripts(p); err != nil {
			return "", err
		}
	}
	return deps, run(ctx, env, s, deps)
}

func writeFile(p string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

// removeScripts drops "scripts" (install hooks of packages whose sources
// aren't there) and nothing else.
func removeScripts(pkgPath string) error {
	b, err := os.ReadFile(pkgPath)
	if err != nil {
		return err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	delete(m, "scripts")
	b, _ = json.MarshalIndent(m, "", "  ")
	return os.WriteFile(pkgPath, b, 0o644)
}

// runtimeDirs are the folders of the image's /app tree: the app and the
// local packages it uses at runtime.
func (ws *workspace) runtimeDirs() []string {
	return append([]string{ws.App}, ws.LocalProd...)
}

// nodeModulesDirs lists the node_modules folders a filtered install made
// that matter at runtime: the root's and the runtime packages' own.
func (ws *workspace) nodeModulesDirs(root string) []string {
	var out []string
	for _, d := range append([]string{"."}, ws.runtimeDirs()...) {
		if fi, err := os.Stat(filepath.Join(root, d, "node_modules")); err == nil && fi.IsDir() {
			out = append(out, d)
		}
	}
	return out
}

// wsDepFiles lists the production node_modules for the image under /app.
// Links stay when they point inside node_modules or at a runtime package;
// links to packages not in the image (another app, a dev-only tool) go.
func (ws *workspace) wsDepFiles(root string) ([]bundle.Entry, error) {
	allowed := []string{}
	for _, d := range ws.nodeModulesDirs(root) {
		allowed = append(allowed, path.Join(d, "node_modules"))
	}
	allowed = append(allowed, ws.runtimeDirs()...)
	inside := func(rel string) bool {
		for _, a := range allowed {
			if rel == a || strings.HasPrefix(rel, a+"/") {
				return true
			}
		}
		return false
	}
	var entries []bundle.Entry
	for _, d := range ws.nodeModulesDirs(root) {
		nm := filepath.Join(root, d, "node_modules")
		err := filepath.WalkDir(nm, func(p string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			switch path.Base(rel) {
			case ".package-lock.json", ".modules.yaml", ".yarn-integrity":
				return nil
			}
			if e.Type()&fs.ModeSymlink != 0 {
				link, err := os.Readlink(p)
				if err != nil {
					return err
				}
				target := path.Clean(path.Join(path.Dir(rel), filepath.ToSlash(link)))
				if filepath.IsAbs(link) || !inside(target) {
					return nil
				}
			}
			en, ok, err := bundle.FileEntry(p, "/app/"+rel, e)
			if ok {
				entries = append(entries, en)
			}
			return err
		})
		if err != nil {
			return nil, err
		}
	}
	return entries, nil
}

// wsAppFiles lists the built app and its runtime local packages (without
// node_modules) and the root package.json, under /app.
func (ws *workspace) wsAppFiles(built string) ([]bundle.Entry, error) {
	var entries []bundle.Entry
	if fi, err := os.Stat(filepath.Join(built, "package.json")); err == nil && fi.Mode().IsRegular() {
		entries = append(entries, bundle.Entry{Src: filepath.Join(built, "package.json"), Dst: "/app/package.json", Mode: 0o644})
	}
	for _, d := range ws.runtimeDirs() {
		files, err := treeFiles(filepath.Join(built, filepath.FromSlash(d)), "/app/"+d, true)
		if err != nil {
			return nil, err
		}
		entries = append(entries, files...)
	}
	return entries, nil
}
