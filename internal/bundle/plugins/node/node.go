// Package node builds Node.js projects. Server apps run on a distroless
// Node runtime with production dependencies in their own layer; front-end
// projects are built and their output served by xu-static on scratch.
package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"xu526/internal/bundle"
	"xu526/internal/bundle/plugins/static"
)

// Plugin is the Node plugin.
type Plugin struct{}

func (Plugin) Name() string { return "node" }

// Version is part of the build cache key: bump it whenever build output
// changes, so layers cached by older builds aren't reused.
func (Plugin) Version() string { return "4" }

func (Plugin) Detect(src *bundle.Source) (*bundle.Detection, error) {
	if !src.Has("package.json") {
		return nil, nil
	}
	return &bundle.Detection{Confidence: 80, Markers: []string{"package.json"}}, nil
}

// Server frameworks mean the app needs Node at runtime.
var serverDeps = []string{"express", "fastify", "koa", "@hapi/hapi", "@nestjs/core", "next", "nuxt", "@remix-run/serve",
	"@remix-run/node", "@sveltejs/adapter-node", "@hono/node-server", "socket.io", "ws", "restify", "@adonisjs/core", "sails", "@react-router/serve"}

// staticTools build a site that needs no server. spa marks single-page
// apps, which get index.html for unknown paths.
var staticTools = []struct {
	dep string
	spa bool
}{
	{"vite", true}, {"react-scripts", true}, {"@angular/cli", true}, {"@vue/cli-service", true}, {"parcel", true},
	{"astro", false}, {"gatsby", false}, {"@docusaurus/core", false}, {"vitepress", false}, {"vuepress", false},
	{"@11ty/eleventy", false}, {"@sveltejs/adapter-static", false},
}

var devServer = regexp.MustCompile(`\b(react-scripts start|vite( |$)|vite dev|vite preview|webpack serve|webpack-dev-server|ng serve|gatsby develop|astro dev|parcel( |$)|docusaurus start|eleventy --serve)`)

var (
	nextExport     = regexp.MustCompile(`output\s*:\s*["']export["']`)
	nextStandalone = regexp.MustCompile(`output\s*:\s*["']standalone["']`)
)

// isNextStandalone reports a Next.js app built with output: "standalone",
// which traces exactly the files the server needs into .next/standalone.
func isNextStandalone(src *bundle.Source, p *pkgJSON) bool {
	if !p.has("next") || p.Scripts["build"] == "" {
		return false
	}
	for _, f := range []string{"next.config.js", "next.config.mjs", "next.config.ts", "next.config.cjs", "next.config.mts"} {
		if b, err := fs.ReadFile(src.FS, f); err == nil && nextStandalone.Match(b) {
			return true
		}
	}
	return false
}

// mode decides between a Node server and a static build.
func mode(src *bundle.Source, p *pkgJSON) (static bool, spa bool, why string) {
	if p.has("next") {
		for _, f := range []string{"next.config.js", "next.config.mjs", "next.config.ts", "next.config.cjs"} {
			if b, err := fs.ReadFile(src.FS, f); err == nil && nextExport.Match(b) {
				return true, false, "Next.js with output: 'export'"
			}
		}
		return false, false, "Next.js server"
	}
	for _, d := range serverDeps {
		if p.Dependencies[d] != "" {
			return false, false, d + " server"
		}
	}
	if start := p.Scripts["start"]; start != "" && !devServer.MatchString(start) {
		return false, false, "start script"
	}
	if p.Scripts["build"] != "" {
		for _, t := range staticTools {
			if p.has(t.dep) {
				return true, t.spa, t.dep + " build"
			}
		}
	}
	return false, false, "Node app"
}

func toolVersion(name string) string {
	out, err := exec.Command(name, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (Plugin) Plan(src *bundle.Source, d *bundle.Detection, opt bundle.Options) (*bundle.Plan, error) {
	if opt.App != "" {
		return planWorkspace(src, opt)
	}
	p, err := readPkg(src)
	if err != nil {
		return nil, err
	}
	pm, err := detectManager(src, p)
	if err != nil {
		return nil, err
	}
	hostV, hostMajor := hostNode()
	if hostV == "" {
		return nil, fmt.Errorf("Node isn't installed or isn't on PATH (https://nodejs.org)")
	}
	pmV := toolVersion(pm.Name)
	if pmV == "" {
		return nil, fmt.Errorf("%s isn't installed (the project uses %s); `corepack enable` provides pnpm and yarn", pm.Name, pm.Lockfile)
	}
	isStatic, spa, why := mode(src, p)
	toolchain := fmt.Sprintf("node %s, %s %s", hostV, pm.Name, pmV)
	summary := []string{fmt.Sprintf("%s project (%s), dependencies pinned by %s", pm.Name, why, pm.Lockfile)}
	installEnv := []string{"CI=1", "npm_config_update_notifier=false"}

	if isStatic {
		env := map[string]string{"PORT": "8080", "XU_ROOT": "/site"}
		if spa {
			env["XU_SPA"] = "1"
		}
		bin, err := static.ServerBinary(opt.Platform.Arch)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(bin)
		summary = append(summary,
			fmt.Sprintf("build with `%s run build` using %s, then serve the output with xu-static", pm.Name, toolchain),
			"Node is not in the image: just the built files and a small file server")
		if spa {
			summary = append(summary, "single-page app: unknown paths get index.html")
		}
		return &bundle.Plan{
			Strategy: bundle.StaticAssets, Base: "scratch",
			Steps: []bundle.Step{
				{Name: "install", Cmd: pm.All, Env: installEnv},
				{Name: "build", Cmd: []string{pm.Name, "run", "build"}, Env: append(installEnv, "NODE_ENV=production")},
			},
			Entrypoint: []string{"/xu-static"}, Env: env, User: "65532:65532", Port: 8080,
			Layers: []bundle.LayerPlan{
				{Name: "server", Salt: hex.EncodeToString(sum[:])},
				{Name: "site", Inputs: []string{"**/*"}, Salt: toolchain + "|" + p.Scripts["build"]},
			},
			Summary: summary,
		}, nil
	}

	major, from, err := chooseMajor(src, p, hostMajor)
	if err != nil {
		return nil, err
	}
	if isNextStandalone(src, p) {
		summary = append(summary,
			fmt.Sprintf("runtime: Node %d on distroless (%s)", major, from),
			fmt.Sprintf("Next.js standalone output: `%s run build`, then only the traced server files, .next/static and public go in the image", pm.Name),
			"starts with: node server.js",
			"runs as user 65532 (non-root) in /app; listens on $PORT (8080) on all interfaces")
		if major != hostMajor {
			summary = append(summary, fmt.Sprintf("note: building with Node %d on this machine for a Node %d image; fine unless a dependency has native code", hostMajor, major))
		}
		steps := []bundle.Step{
			{Name: "install", Cmd: pm.All, Env: installEnv},
			{Name: "build", Cmd: []string{pm.Name, "run", "build"}, Env: append(installEnv, "NODE_ENV=production", "NEXT_TELEMETRY_DISABLED=1")},
		}
		if crossHost(opt.Platform) {
			// next build needs this machine's binaries (SWC); the image needs
			// Linux ones, so the server's node_modules is installed again for Linux.
			steps = append(steps, bundle.Step{Name: "deps", Cmd: pm.Prod, Env: append(append(installEnv, "NODE_ENV=production"), crossEnv(opt.Platform)...)})
			summary = append(summary, fmt.Sprintf("this machine is %s/%s: builds with its own tools, then installs production dependencies for %s (prebuilt Linux binaries)", runtime.GOOS, runtime.GOARCH, opt.Platform))
		}
		return &bundle.Plan{
			Strategy: bundle.RuntimeDeps, Base: baseFor(major),
			Steps: steps,
			Cmd:   []string{"server.js"}, WorkingDir: "/app", Port: 8080,
			Env: map[string]string{"PORT": "8080", "HOSTNAME": "0.0.0.0", "NODE_ENV": "production", "NEXT_TELEMETRY_DISABLED": "1"},
			Layers: []bundle.LayerPlan{
				{Name: "app", Inputs: []string{"**/*"}, Salt: fmt.Sprintf("%s|node%d|next-standalone|host %s/%s", toolchain, major, runtime.GOOS, runtime.GOARCH)},
			},
			Summary: summary,
		}, nil
	}
	args, startEnv, startDesc, err := startCommand(src, p)
	if err != nil {
		return nil, err
	}
	env := map[string]string{"PORT": "8080", "NODE_ENV": "production"}
	for k, v := range startEnv {
		env[k] = v
	}
	summary = append(summary,
		fmt.Sprintf("runtime: Node %d on distroless (%s)", major, from),
		fmt.Sprintf("dependencies: `%s` in their own layer, rebuilt only when %s changes", strings.Join(pm.Prod, " "), pm.Lockfile),
		fmt.Sprintf("starts with: node %s (%s)", strings.Join(args, " "), startDesc))
	if major != hostMajor {
		summary = append(summary, fmt.Sprintf("note: installing with Node %d on this machine for a Node %d image; fine unless a dependency has native code", hostMajor, major))
	}
	depEnv := append(append([]string{}, installEnv...), "NODE_ENV=production")
	if crossHost(opt.Platform) {
		depEnv = append(depEnv, crossEnv(opt.Platform)...)
		summary = append(summary, fmt.Sprintf("this machine is %s/%s: dependencies are installed as for %s, so packages with prebuilt binaries get the Linux ones", runtime.GOOS, runtime.GOARCH, opt.Platform))
		if pm.Name == "yarn" {
			summary = append(summary, "note: yarn 1 can't install for another system; native dependencies will be refused")
		}
	}
	steps := []bundle.Step{{Name: "deps", Cmd: pm.Prod, Env: depEnv}}
	if p.Scripts["build"] != "" {
		steps = append(steps,
			bundle.Step{Name: "install", Cmd: pm.All, Env: installEnv},
			bundle.Step{Name: "build", Cmd: []string{pm.Name, "run", "build"}, Env: append(installEnv, "NODE_ENV=production")})
		summary = append(summary, fmt.Sprintf("compiles first with `%s run build`; dev dependencies stay out of the image", pm.Name))
	}
	depInputs := []string{"package.json", pm.Lockfile, ".npmrc", ".yarnrc"}
	summary = append(summary, "runs as user 65532 (non-root) in /app; listens on $PORT (8080)")
	return &bundle.Plan{
		Strategy: bundle.RuntimeDeps, Base: baseFor(major),
		Steps: steps, Cmd: args, Env: env, WorkingDir: "/app", Port: 8080,
		Layers: []bundle.LayerPlan{
			{Name: "deps", Inputs: depInputs, Salt: fmt.Sprintf("%s|node%d|host %s/%s", toolchain, major, runtime.GOOS, runtime.GOARCH)},
			{Name: "app", Inputs: []string{"**/*"}, Salt: fmt.Sprintf("%s|node%d|%v", toolchain, major, args)},
		},
		Summary: summary,
	}, nil
}

// skipCopy leaves out what never belongs in a build copy.
func skipCopy(rel string, d fs.DirEntry) bool {
	n := d.Name()
	// .next at the root is a previous local build: the image is built fresh.
	return n == ".git" || n == "node_modules" || (d.IsDir() && rel == ".next") || strings.HasSuffix(n, ".tar") || strings.HasSuffix(n, ".tar.tmp")
}

func run(ctx context.Context, env *bundle.BuildEnv, s bundle.Step, dir string) error {
	s.Dir = dir
	return env.Runner.Run(ctx, s, env.Log)
}

func step(p *bundle.Plan, name string) (bundle.Step, bool) {
	for _, s := range p.Steps {
		if s.Name == name {
			return s, true
		}
	}
	return bundle.Step{}, false
}

func (Plugin) Build(ctx context.Context, env *bundle.BuildEnv, p *bundle.Plan) (*bundle.Output, error) {
	if env.Options.App != "" {
		return buildWorkspace(ctx, env, p)
	}
	out := &bundle.Output{Files: map[string]string{}}
	srcDir := env.Source.Dir
	if p.Strategy == bundle.StaticAssets {
		bin, err := static.ServerBinary(env.Options.Platform.Arch)
		if err != nil {
			return nil, err
		}
		server := filepath.Join(env.WorkDir, "xu-static")
		if err := os.WriteFile(server, bin, 0o755); err != nil {
			return nil, err
		}
		out.Files["server"] = server
		if env.Cached["site"] {
			return out, nil
		}
		app := filepath.Join(env.WorkDir, "app")
		if err := bundle.CopyTree(srcDir, app, skipCopy, nil); err != nil {
			return nil, err
		}
		for _, name := range []string{"install", "build"} {
			s, _ := step(p, name)
			if err := run(ctx, env, s, app); err != nil {
				return nil, err
			}
		}
		site, err := findOutput(app)
		if err != nil {
			return nil, err
		}
		out.Files["app"], out.Files["site"] = app, site
		return out, nil
	}

	if isStandalonePlan(p) {
		app := filepath.Join(env.WorkDir, "app")
		if err := bundle.CopyTree(srcDir, app, skipCopy, nil); err != nil {
			return nil, err
		}
		for _, name := range []string{"install", "build"} {
			s, _ := step(p, name)
			if err := run(ctx, env, s, app); err != nil {
				return nil, err
			}
		}
		standalone := filepath.Join(app, ".next", "standalone")
		if _, err := os.Stat(filepath.Join(standalone, "server.js")); err != nil {
			return nil, fmt.Errorf(`the build didn't produce .next/standalone/server.js; check that next.config sets output: "standalone"`)
		}
		if s, ok := step(p, "deps"); ok {
			deps, err := installDeps(ctx, env, s, srcDir)
			if err != nil {
				return nil, err
			}
			nm := filepath.Join(standalone, "node_modules")
			if err := os.RemoveAll(nm); err != nil {
				return nil, err
			}
			if err := os.MkdirAll(filepath.Join(deps, "node_modules"), 0o755); err != nil { // no dependencies at all
				return nil, err
			}
			if err := os.Rename(filepath.Join(deps, "node_modules"), nm); err != nil {
				return nil, err
			}
		}
		if err := checkNative(filepath.Join(standalone, "node_modules"), env.Options.Platform, p.Base, env.Log); err != nil {
			return nil, err
		}
		out.Files["standalone"], out.Files["static"], out.Files["public"] = standalone, filepath.Join(app, ".next", "static"), filepath.Join(app, "public")
		return out, nil
	}
	if !env.Cached["deps"] {
		s, _ := step(p, "deps")
		deps, err := installDeps(ctx, env, s, srcDir)
		if err != nil {
			return nil, err
		}
		if err := checkNative(filepath.Join(deps, "node_modules"), env.Options.Platform, p.Base, env.Log); err != nil {
			return nil, err
		}
		out.Files["deps"] = deps
	}
	if !env.Cached["app"] {
		app := srcDir
		if _, ok := step(p, "build"); ok {
			app = filepath.Join(env.WorkDir, "app")
			if err := bundle.CopyTree(srcDir, app, skipCopy, nil); err != nil {
				return nil, err
			}
			for _, name := range []string{"install", "build"} {
				s, _ := step(p, name)
				if err := run(ctx, env, s, app); err != nil {
					return nil, err
				}
			}
			if err := compileNextConfig(ctx, env, app); err != nil {
				return nil, err
			}
		}
		out.Files["app"] = app
	}
	return out, nil
}

// nextConfigJS turns a TypeScript config into JavaScript. argv: source,
// output, "esm" or "cjs". CommonJS output unwraps `export default`, as
// Next's own loader does.
const nextConfigJS = `const path = require("path"), fs = require("fs");
const [src, out, mode] = process.argv.slice(1);
const ts = require(require.resolve("typescript", { paths: [path.dirname(path.resolve(src))] }));
const r = ts.transpileModule(fs.readFileSync(src, "utf8"), { fileName: src, reportDiagnostics: false, compilerOptions: {
  module: mode === "esm" ? ts.ModuleKind.ESNext : ts.ModuleKind.CommonJS,
  target: ts.ScriptTarget.ES2022, esModuleInterop: true } });
let js = r.outputText;
if (mode !== "esm") js += "\nif (module.exports && module.exports.__esModule && \"default\" in module.exports) module.exports = module.exports.default;\n";
fs.writeFileSync(out, js);`

// compileNextConfig replaces next.config.ts/.mts in a built Next app with
// JavaScript. ` + "`next start`" + ` would otherwise need the typescript package at
// runtime (a devDependency, so not in the image) and try to npm install it.
func compileNextConfig(ctx context.Context, env *bundle.BuildEnv, app string) error {
	if _, err := os.Stat(filepath.Join(app, ".next")); err != nil {
		return nil // not a Next build
	}
	for _, name := range []string{"next.config.ts", "next.config.mts"} {
		src := filepath.Join(app, name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		code, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		mode, out := "cjs", "next.config.js"
		// import.meta exists only in ES modules: CommonJS output would leave it in,
		// and Node would then load the file as ESM, where exports is undefined.
		if name == "next.config.mts" || pkgIsModule(app) || bytes.Contains(code, []byte("import.meta")) {
			mode, out = "esm", "next.config.mjs"
		}
		if !findUp(app, filepath.Join("node_modules", "typescript")) { // hoisted to the root in a monorepo
			return fmt.Errorf("%s needs the typescript package to be read; add it to devDependencies (npm i -D typescript)", name)
		}
		fmt.Fprintf(env.Log, "converting %s to %s so the image doesn't need TypeScript\n", name, out)
		if err := env.Runner.Run(ctx, bundle.Step{Name: "next-config", Cmd: []string{"node", "-e", nextConfigJS, name, out, mode}, Dir: app,
			Show: "node <transpile " + name + " with typescript.transpileModule>"}, env.Log); err != nil {
			return fmt.Errorf("convert %s: %w", name, err)
		}
		if err := os.Remove(src); err != nil {
			return err
		}
	}
	return nil
}

// findUp reports whether rel exists in dir or one of its parents.
func findUp(dir, rel string) bool {
	for {
		if _, err := os.Stat(filepath.Join(dir, rel)); err == nil {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

func pkgIsModule(dir string) bool {
	var p struct{ Type string }
	b, _ := os.ReadFile(filepath.Join(dir, "package.json"))
	json.Unmarshal(b, &p)
	return p.Type == "module"
}

// installDeps installs production dependencies from just the manifest and
// lockfile into WorkDir/deps.
func installDeps(ctx context.Context, env *bundle.BuildEnv, s bundle.Step, srcDir string) (string, error) {
	deps := filepath.Join(env.WorkDir, "deps")
	only := map[string]bool{}
	for _, f := range []string{"package.json", "package-lock.json", "npm-shrinkwrap.json", "pnpm-lock.yaml", "yarn.lock", ".npmrc", ".yarnrc"} {
		only[f] = true
	}
	if err := bundle.CopyTree(srcDir, deps, nil, only); err != nil {
		return "", err
	}
	if err := stripScripts(filepath.Join(deps, "package.json"), env.Options.Platform); err != nil {
		return "", err
	}
	return deps, run(ctx, env, s, deps)
}

// crossHost reports whether this machine's npm would pick native packages
// for a different system than the image (macOS, Windows, another CPU, musl).
func crossHost(plat bundle.Platform) bool {
	return runtime.GOOS != "linux" || runtime.GOARCH != plat.Arch || isMusl()
}

// npmCPU maps Go architecture names to Node's process.arch.
func npmCPU(arch string) string {
	if arch == "amd64" {
		return "x64"
	}
	return arch
}

// crossEnv makes npm (and pnpm, which reads npm_config_*) choose optional
// dependencies for the image: this is how esbuild, sharp, @next/swc,
// lightningcss and friends ship their binaries.
func crossEnv(plat bundle.Platform) []string {
	return []string{"npm_config_os=linux", "npm_config_cpu=" + npmCPU(plat.Arch), "npm_config_libc=glibc"}
}

// stripScripts removes the root package's lifecycle scripts (prepare:
// husky, postinstall: build tools) from the copy used to install
// production dependencies, where their dev tools aren't present.
func stripScripts(pkgPath string, plat bundle.Platform) error {
	b, err := os.ReadFile(pkgPath)
	if err != nil {
		return err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("package.json: %w", err)
	}
	delete(m, "scripts")
	if crossHost(plat) {
		// pnpm's own setting for the same thing as crossEnv.
		var pn map[string]any
		if raw, ok := m["pnpm"]; ok {
			json.Unmarshal(raw, &pn)
		}
		if pn == nil {
			pn = map[string]any{}
		}
		pn["supportedArchitectures"] = map[string][]string{"os": {"linux"}, "cpu": {npmCPU(plat.Arch)}, "libc": {"glibc"}}
		m["pnpm"], _ = json.Marshal(pn)
	}
	b, _ = json.MarshalIndent(m, "", "  ")
	return os.WriteFile(pkgPath, b, 0o644)
}

// outputDirs are where front-end tools write their build, in the order checked.
var outputDirs = []string{"dist", "build", "out", "public", "_site", "docs/.vitepress/dist", ".vitepress/dist", "src/.vuepress/dist", "docs/.vuepress/dist"}

func findOutput(app string) (string, error) {
	for _, d := range outputDirs {
		if _, err := os.Stat(filepath.Join(app, d, "index.html")); err == nil {
			return d, nil
		}
	}
	// Angular: dist/<project>/browser/index.html or dist/<project>/index.html
	matches, _ := filepath.Glob(filepath.Join(app, "dist", "*", "browser", "index.html"))
	more, _ := filepath.Glob(filepath.Join(app, "dist", "*", "index.html"))
	for _, m := range append(matches, more...) {
		rel, _ := filepath.Rel(app, filepath.Dir(m))
		return filepath.ToSlash(rel), nil
	}
	return "", fmt.Errorf("the build finished but no index.html was found in %s", strings.Join(outputDirs, ", "))
}

// checkNative refuses native addons that won't load in the image (built
// for macOS or Windows, another CPU, musl, or another Node): they would
// crash at startup. Each .node file is inspected, not guessed from the host.
func checkNative(nodeModules string, plat bundle.Platform, base string, log io.Writer) error {
	var natives []string
	filepath.WalkDir(nodeModules, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(nodeModules, p)
		rel = filepath.ToSlash(rel)
		// .node addons everywhere; plain shared libraries only in musl variant
		// packages (e.g. @img/sharp-libvips-linuxmusl-*), which can be dropped.
		if strings.HasSuffix(d.Name(), ".node") || (sharedLib.MatchString(d.Name()) && strings.Contains(rel, "musl")) {
			natives = append(natives, rel)
		}
		return nil
	})
	if len(natives) == 0 {
		return nil
	}
	sort.Strings(natives)
	// pkgDir is the package folder (relative to nodeModules) holding rel.
	pkgDir := func(rel string) string {
		parts := strings.Split(rel, "/")
		start := 0
		if i := lastIndex(parts, "node_modules"); i >= 0 { // nested: a/node_modules/b/x.node
			start = i + 1
		}
		end := start + 1
		if strings.HasPrefix(parts[start], "@") && len(parts) > start+1 {
			end++
		}
		return strings.Join(parts[:end], "/")
	}
	pkgOf := func(rel string) string {
		d := pkgDir(rel)
		if i := strings.LastIndex(d, "node_modules/"); i >= 0 {
			d = d[i+len("node_modules/"):]
		}
		return d
	}
	bad := map[string]string{} // package → why
	pruned := map[string]bool{}
	allELF := true
	for _, n := range natives {
		if pruned[pkgDir(n)] {
			continue
		}
		why := nativeProblem(filepath.Join(nodeModules, filepath.FromSlash(n)), plat.Arch)
		if why == muslProblem && strings.Contains(pkgOf(n), "musl") {
			// The Alpine build of a package that ships one per libc (sharp,
			// @napi-rs/*, …). npm installs it when the lockfile doesn't record
			// libc; the package loads its glibc sibling at runtime, so it's
			// dead weight here.
			if err := os.RemoveAll(filepath.Join(nodeModules, filepath.FromSlash(pkgDir(n)))); err != nil {
				return err
			}
			pruned[pkgDir(n)] = true
			fmt.Fprintf(log, "left out %s (the Alpine/musl build; the image uses glibc)\n", pkgOf(n))
			continue
		}
		if why != "" && !strings.HasSuffix(n, ".node") {
			continue // a shared library in a musl-named package that isn't musl: leave it alone
		}
		if why != "" {
			if _, seen := bad[pkgOf(n)]; !seen {
				bad[pkgOf(n)] = why
			}
			allELF = false
		}
	}
	if allELF && !crossHost(plat) {
		// Built on a matching Linux: still tied to the Node ABI it was built for.
		if _, hostMajor := hostNode(); !strings.Contains(base, fmt.Sprintf("nodejs%d-", hostMajor)) {
			for _, n := range natives {
				if !strings.HasSuffix(n, ".node") || pruned[pkgDir(n)] {
					continue
				}
				bad[pkgOf(n)] = fmt.Sprintf("built for Node %d, the image runs %s", hostMajor, path.Base(base))
			}
		}
	}
	if len(bad) == 0 {
		return nil
	}
	var names []string
	for n, why := range bad {
		names = append(names, n+" ("+why+")")
	}
	sort.Strings(names)
	return fmt.Errorf("dependencies with native code won't run in the image (%s): %s. "+
		"These compile during install and can't be built for another system; build on a Linux %s machine "+
		"with the same Node major (CI, a Linux VM, WSL, or the server itself with -host-builds)",
		plat, strings.Join(names, "; "), plat.Arch)
}

func lastIndex(s []string, v string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == v {
			return i
		}
	}
	return -1
}

// nativeProblem says why the shared library at p can't load on linux/arch
// with glibc ("" when it can).
func nativeProblem(p, arch string) string {
	f, err := os.Open(p)
	if err != nil {
		return err.Error()
	}
	defer f.Close()
	magic := make([]byte, 4)
	if _, err := io.ReadFull(f, magic); err != nil {
		return "not a library"
	}
	switch {
	case bytes.Equal(magic[:2], []byte("MZ")):
		return "built for Windows"
	case isMachO(magic):
		return "built for macOS"
	case !bytes.Equal(magic, []byte("\x7fELF")):
		return "not a Linux library"
	}
	ef, err := elf.NewFile(f)
	if err != nil {
		return "unreadable ELF file"
	}
	want := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[arch]
	if ef.Machine != want {
		return fmt.Sprintf("built for %s, the image is %s", machineName(ef.Machine), arch)
	}
	libs, _ := ef.ImportedLibraries()
	for _, l := range libs {
		if strings.Contains(l, "musl") {
			return muslProblem
		}
	}
	if len(libs) == 0 {
		// No section table to read imports from: a musl-linked library still
		// carries the loader's name among its strings.
		if b, err := os.ReadFile(p); err == nil && (bytes.Contains(b, []byte("libc.musl-")) || bytes.Contains(b, []byte("ld-musl-"))) {
			return muslProblem
		}
	}
	return ""
}

var sharedLib = regexp.MustCompile(`\.so(\.\d+)*$`)

const muslProblem = "built for musl (Alpine), the image uses glibc"

func isMachO(m []byte) bool {
	for _, v := range [][]byte{{0xfe, 0xed, 0xfa, 0xce}, {0xfe, 0xed, 0xfa, 0xcf}, {0xce, 0xfa, 0xed, 0xfe}, {0xcf, 0xfa, 0xed, 0xfe}, {0xca, 0xfe, 0xba, 0xbe}} {
		if bytes.Equal(m, v) {
			return true
		}
	}
	return false
}

func machineName(m elf.Machine) string {
	switch m {
	case elf.EM_X86_64:
		return "amd64"
	case elf.EM_AARCH64:
		return "arm64"
	}
	return strings.ToLower(strings.TrimPrefix(m.String(), "EM_"))
}

func isMusl() bool {
	_, err := os.Stat("/etc/alpine-release")
	return err == nil
}

// appFiles lists what goes in the app layer: everything except
// node_modules, dotfiles (.env, .git), tarballs and xu.lock.
func appFiles(root string) ([]bundle.Entry, error) {
	return treeFiles(root, "/app", true)
}

// buildOutputDirs are dot-folders frameworks build into; they belong in the
// image even though other dotfiles (.env, .git, .npmrc) never do.
var buildOutputDirs = map[string]bool{".next": true, ".nuxt": true, ".output": true, ".svelte-kit": true}

// skipDot reports whether a dot-named entry stays out of the image.
// rel is slash-separated and relative to the tree root.
func skipDot(rel, name string, isDir bool) bool {
	if !strings.HasPrefix(name, ".") {
		return false
	}
	if isDir && buildOutputDirs[name] {
		return false
	}
	return true
}

// treeFiles lists a tree for a layer at dst: no node_modules (when
// skipModules), no dotfiles except build output, no .next/cache (Next's
// build cache, often hundreds of MB), no tarballs or xu.lock.
func treeFiles(root, dst string, skipModules bool) ([]bundle.Entry, error) {
	var entries []bundle.Entry
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		n := d.Name()
		if (skipModules && n == "node_modules") || skipDot(rel, n, d.IsDir()) || strings.HasSuffix(rel, ".next/cache") ||
			(!d.IsDir() && (strings.HasSuffix(n, ".tar") || rel == bundle.LockFile)) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		e, ok, err := bundle.FileEntry(p, dst+"/"+rel, d)
		if ok {
			entries = append(entries, e)
		}
		return err
	})
	return entries, err
}

// depFiles lists node_modules for the deps layer, keeping relative
// symlinks (.bin links) and dropping links that point outside it.
func depFiles(nodeModules string) ([]bundle.Entry, error) {
	var entries []bundle.Entry
	if _, err := os.Stat(nodeModules); os.IsNotExist(err) {
		return nil, nil // no dependencies
	}
	err := filepath.WalkDir(nodeModules, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(nodeModules, p)
		if rel == ".package-lock.json" || rel == ".modules.yaml" || rel == ".yarn-integrity" {
			return nil // install metadata with timestamps and paths
		}
		e, ok, err := bundle.FileEntry(p, "/app/node_modules/"+filepath.ToSlash(rel), d)
		if ok {
			entries = append(entries, e)
		}
		return err
	})
	return entries, err
}

func (Plugin) Layers(p *bundle.Plan, out *bundle.Output) ([]bundle.Layer, error) {
	var layers []bundle.Layer
	if p.Strategy == bundle.StaticAssets {
		layers = append(layers, bundle.Layer{Name: "server", Files: []bundle.Entry{{Src: out.Files["server"], Dst: "/xu-static", Mode: 0o755}}})
		if app := out.Files["app"]; app != "" {
			src, err := bundle.NewSource(app)
			if err != nil {
				return nil, err
			}
			dir := out.Files["site"]
			files, _, err := static.SiteFiles(src, dir)
			if err != nil {
				return nil, err
			}
			var entries []bundle.Entry
			for _, f := range files {
				entries = append(entries, bundle.Entry{Src: filepath.Join(app, filepath.FromSlash(f)), Dst: "/site/" + strings.TrimPrefix(f, dir+"/"), Mode: 0o644})
			}
			layers = append(layers, bundle.Layer{Name: "site", Files: entries})
		}
		return layers, nil
	}
	if sa := out.Files["standalone"]; sa != "" {
		// The traced server (with its own node_modules), plus what Next
		// leaves for a CDN: .next/static and public. In a monorepo the
		// server lives in /app/<app>.
		root := "/app"
		if pre := out.Files["standalone-prefix"]; pre != "" {
			root += "/" + pre
		}
		serverDir := "/app"
		if app := out.Files["ws-app"]; app != "" {
			serverDir += "/" + app
		}
		files, err := treeFiles(sa, root, false)
		if err != nil {
			return nil, err
		}
		for _, extra := range []struct{ dir, dst string }{{out.Files["static"], serverDir + "/.next/static"}, {out.Files["public"], serverDir + "/public"}} {
			if _, err := os.Stat(extra.dir); err != nil {
				continue
			}
			more, err := treeFiles(extra.dir, extra.dst, true)
			if err != nil {
				return nil, err
			}
			files = append(files, more...)
		}
		return []bundle.Layer{{Name: "app", Files: files}}, nil
	}
	if out.Files["ws-app"] != "" {
		return workspaceLayers(out)
	}
	if deps := out.Files["deps"]; deps != "" {
		files, err := depFiles(filepath.Join(deps, "node_modules"))
		if err != nil {
			return nil, err
		}
		layers = append(layers, bundle.Layer{Name: "deps", Files: files})
	}
	if app := out.Files["app"]; app != "" {
		files, err := appFiles(app)
		if err != nil {
			return nil, err
		}
		layers = append(layers, bundle.Layer{Name: "app", Files: files})
	}
	return layers, nil
}

// isStandalonePlan recognises a Next.js standalone plan.
func isStandalonePlan(p *bundle.Plan) bool {
	return len(p.Cmd) == 1 && (p.Cmd[0] == "server.js" || strings.HasSuffix(p.Cmd[0], "/server.js")) && p.Env["HOSTNAME"] == "0.0.0.0"
}
