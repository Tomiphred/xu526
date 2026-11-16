package node

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"xu526/internal/bundle"
)

// pkgJSON is the part of package.json the plugin reads.
type pkgJSON struct {
	Name            string            `json:"name"`
	Main            string            `json:"main"`
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
	// Optional and peer dependencies only matter for finding local packages
	// in a monorepo.
	OptionalDependencies map[string]string `json:"optionalDependencies"`
	PeerDependencies     map[string]string `json:"peerDependencies"`
	Engines              struct {
		Node string `json:"node"`
	} `json:"engines"`
	Workspaces     json.RawMessage `json:"workspaces"`
	PackageManager string          `json:"packageManager"`
}

func readPkg(src *bundle.Source) (*pkgJSON, error) {
	b, err := fs.ReadFile(src.FS, "package.json")
	if err != nil {
		return nil, err
	}
	var p pkgJSON
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("package.json: %w", err)
	}
	return &p, nil
}

func (p *pkgJSON) has(dep string) bool {
	_, a := p.Dependencies[dep]
	_, b := p.DevDependencies[dep]
	return a || b
}

// manager is a package manager and how to drive it.
type manager struct {
	Name     string   // npm, pnpm, yarn
	Lockfile string   // the lockfile that pins dependencies
	Prod     []string // install production dependencies only
	All      []string // install everything (to build)
}

func detectManager(src *bundle.Source, p *pkgJSON) (*manager, error) {
	if (len(p.Workspaces) > 0 && string(p.Workspaces) != "null") || src.Has("pnpm-workspace.yaml") {
		return nil, fmt.Errorf("this is a monorepo root; build one of its apps: xu build apps/web (or xu build . --app apps/web)")
	}
	return managerFor(src)
}

// managerFor picks the package manager from the lockfile in src.
func managerFor(src *bundle.Source) (*manager, error) {
	switch {
	case src.Has("pnpm-lock.yaml"):
		hoisted := "--config.node-linker=hoisted" // a plain node_modules tree, no symlink farm
		return &manager{"pnpm", "pnpm-lock.yaml",
			[]string{"pnpm", "install", "--frozen-lockfile", "--prod", hoisted},
			[]string{"pnpm", "install", "--frozen-lockfile", hoisted}}, nil
	case src.Has("yarn.lock"):
		b, _ := fs.ReadFile(src.FS, "yarn.lock")
		if strings.Contains(string(b), "__metadata:") || src.Has(".yarnrc.yml") {
			return nil, fmt.Errorf("Yarn 2+ (Berry) isn't supported yet; yarn 1, npm and pnpm are")
		}
		return &manager{"yarn", "yarn.lock",
			[]string{"yarn", "install", "--frozen-lockfile", "--production", "--non-interactive"},
			[]string{"yarn", "install", "--frozen-lockfile", "--non-interactive"}}, nil
	case src.Has("package-lock.json"), src.Has("npm-shrinkwrap.json"):
		lock := "package-lock.json"
		if src.Has("npm-shrinkwrap.json") {
			lock = "npm-shrinkwrap.json"
		}
		return &manager{"npm", lock,
			[]string{"npm", "ci", "--omit=dev", "--no-audit", "--no-fund"},
			[]string{"npm", "ci", "--no-audit", "--no-fund"}}, nil
	case src.Has("bun.lock"), src.Has("bun.lockb"):
		return nil, fmt.Errorf("Bun isn't supported yet; npm, pnpm and yarn 1 are")
	}
	return nil, fmt.Errorf("no lockfile: run `npm install` (or pnpm/yarn) and commit the lockfile so builds are reproducible")
}

// Supported Node majors, each with a distroless runtime.
var supported = []int{22, 24, 26}

const defaultMajor = 24

func baseFor(major int) string {
	return fmt.Sprintf("gcr.io/distroless/nodejs%d-debian13:nonroot", major)
}

func hostNode() (string, int) {
	out, err := exec.Command("node", "--version").Output()
	if err != nil {
		return "", 0
	}
	v := strings.TrimSpace(string(out))
	m, _ := strconv.Atoi(strings.SplitN(strings.TrimPrefix(v, "v"), ".", 2)[0])
	return v, m
}

func isSupported(m int) bool {
	for _, s := range supported {
		if s == m {
			return true
		}
	}
	return false
}

var number = regexp.MustCompile(`\d+`)

// chooseMajor picks the Node major for the image from .nvmrc,
// .node-version or engines.node, falling back to this machine's Node (if
// supported) and then the current LTS. It reports where the choice came from.
func chooseMajor(src *bundle.Source, p *pkgJSON, host int) (int, string, error) {
	for _, f := range []string{".nvmrc", ".node-version"} {
		b, err := fs.ReadFile(src.FS, f)
		if err != nil {
			continue
		}
		v := strings.TrimSpace(string(b))
		if strings.HasPrefix(v, "lts") || v == "node" || v == "" {
			break
		}
		m, _ := strconv.Atoi(number.FindString(v))
		if !isSupported(m) {
			return 0, "", fmt.Errorf("%s asks for Node %s; supported: %v", f, v, supported)
		}
		return m, f, nil
	}
	if r := strings.TrimSpace(p.Engines.Node); r != "" {
		m, err := majorForRange(r, host)
		return m, "engines.node " + strconv.Quote(r), err
	}
	if isSupported(host) {
		return host, "this machine's Node", nil
	}
	return defaultMajor, "default (current LTS)", nil
}

// majorForRange handles the common engines forms: "22", "^22.1", "~24",
// "22.x", ">=20", ">=20 <25", "20 || 22".
func majorForRange(r string, host int) (int, error) {
	var options []int
	for _, alt := range strings.Split(r, "||") {
		alt = strings.TrimSpace(alt)
		lo, hi := 0, 1000
		exact := -1
		for _, part := range strings.Fields(strings.ReplaceAll(alt, ">= ", ">=")) {
			n, _ := strconv.Atoi(number.FindString(part))
			switch {
			case strings.HasPrefix(part, ">="):
				lo = n
			case strings.HasPrefix(part, ">"):
				lo = n + 1 // ">20" excludes 20; ">20.1" still allows 20.x
				if strings.Contains(part, ".") {
					lo = n
				}
			case strings.HasPrefix(part, "<="):
				hi = n
			case strings.HasPrefix(part, "<"):
				hi = n - 1
				if strings.Contains(part, ".") && !strings.HasSuffix(part, ".0") && !strings.HasSuffix(part, ".0.0") {
					hi = n
				}
			default:
				exact = n
			}
		}
		for _, s := range supported {
			if (exact >= 0 && s == exact) || (exact < 0 && s >= lo && s <= hi) {
				options = append(options, s)
			}
		}
	}
	if len(options) == 0 {
		return 0, fmt.Errorf("engines.node %q matches no supported Node (%v)", r, supported)
	}
	sort.Ints(options)
	for _, o := range options {
		if o == host {
			return o, nil // matching this machine keeps native modules compatible
		}
	}
	for _, o := range options {
		if o == defaultMajor {
			return o, nil
		}
	}
	return options[len(options)-1], nil
}

var envAssign = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// startCommand turns the start script (or main) into the node arguments
// the image runs, plus any VAR=value prefixes as environment.
func startCommand(src *bundle.Source, p *pkgJSON) (args []string, env map[string]string, desc string, err error) {
	return startCommandBins(src, src, p)
}

// startCommandBins is startCommand where package binaries are looked up in
// binSrc (a monorepo's root, where they're hoisted to).
func startCommandBins(src, binSrc *bundle.Source, p *pkgJSON) (args []string, env map[string]string, desc string, err error) {
	env = map[string]string{}
	start := strings.TrimSpace(p.Scripts["start"])
	if start == "" {
		main := p.Main
		if main == "" {
			for _, f := range []string{"server.js", "index.js", "app.js", "server.mjs", "index.mjs"} {
				if src.Has(f) {
					main = f
					break
				}
			}
		}
		if main == "" {
			return nil, nil, "", fmt.Errorf(`no way to start the app: add "start": "node server.js" to package.json scripts`)
		}
		return []string{main}, env, "main " + main, nil
	}
	words, err := bundle.SplitCommand(start)
	if err != nil {
		return nil, nil, "", fmt.Errorf("the start script %q can't run without a shell (%v); make it `node <file>`", start, err)
	}
	for len(words) > 0 && envAssign.MatchString(words[0]) {
		k, v, _ := strings.Cut(words[0], "=")
		env[k] = v
		words = words[1:]
	}
	if len(words) > 0 && words[0] == "cross-env" { // cross-env A=1 node x
		words = words[1:]
		for len(words) > 0 && envAssign.MatchString(words[0]) {
			k, v, _ := strings.Cut(words[0], "=")
			env[k] = v
			words = words[1:]
		}
	}
	if len(words) == 0 {
		return nil, nil, "", fmt.Errorf("the start script %q has no command", start)
	}
	if words[0] == "node" || words[0] == "nodejs" {
		if len(words) == 1 {
			return nil, nil, "", fmt.Errorf("the start script runs `node` with no file")
		}
		return words[1:], env, "npm start: " + start, nil
	}
	if words[0] == "npm" || words[0] == "yarn" || words[0] == "pnpm" || words[0] == "npx" {
		return nil, nil, "", fmt.Errorf("the start script %q calls a package manager, which isn't in the image; make it `node <file>`", start)
	}
	bin, pkgName, dev, err := resolveBin(binSrc, words[0])
	if err != nil {
		return nil, nil, "", fmt.Errorf("the start script runs %q: %v", words[0], err)
	}
	if dev || (p.DevDependencies[pkgName] != "" && p.Dependencies[pkgName] == "") {
		return nil, nil, "", fmt.Errorf("the start script runs %s from %s, a devDependency, which isn't installed in the image; move it to dependencies", words[0], pkgName)
	}
	rest := words[1:]
	if words[0] == "next" {
		rest = dropPortFlag(rest)
	}
	return append([]string{bin}, rest...), env, "npm start: " + start, nil
}

// dropPortFlag removes -p N, --port N and --port=N, which would override
// $PORT: the image tells the platform which port it listens on, so `next
// start -p 3000` must listen there, not on 3000.
func dropPortFlag(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case (a == "-p" || a == "--port") && i+1 < len(args):
			i++
		case strings.HasPrefix(a, "--port="), strings.HasPrefix(a, "-p="):
		default:
			out = append(out, a)
		}
	}
	return out
}

// resolveBin finds the JS file behind a package binary (next, nest, tsx),
// relative to the app folder: from package-lock.json, else from an
// installed node_modules.
func resolveBin(src *bundle.Source, name string) (file, pkgName string, dev bool, err error) {
	for _, lockName := range []string{"package-lock.json", "npm-shrinkwrap.json"} {
		b, err := fs.ReadFile(src.FS, lockName)
		if err != nil {
			continue
		}
		var lock struct {
			Packages map[string]struct {
				Bin json.RawMessage `json:"bin"`
				Dev bool            `json:"dev"`
			} `json:"packages"`
		}
		if json.Unmarshal(b, &lock) != nil {
			continue
		}
		keys := make([]string, 0, len(lock.Packages))
		for k := range lock.Packages {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			pkgPath, ok := strings.CutPrefix(k, "node_modules/")
			if !ok || strings.Contains(pkgPath, "/node_modules/") {
				continue // only top-level packages are on the start script's PATH
			}
			e := lock.Packages[k]
			var bins map[string]string
			if json.Unmarshal(e.Bin, &bins) != nil {
				continue
			}
			if rel, ok := bins[name]; ok {
				return path.Join("node_modules", pkgPath, path.Clean(rel)), pkgPath, e.Dev, nil
			}
		}
	}
	link, err := os.Readlink(filepath.Join(src.Dir, "node_modules", ".bin", name))
	if err == nil {
		rel := path.Clean(path.Join("node_modules/.bin", filepath.ToSlash(link)))
		if strings.HasPrefix(rel, "node_modules/") && !strings.HasPrefix(rel, "node_modules/.bin/") {
			parts := strings.Split(strings.TrimPrefix(rel, "node_modules/"), "/")
			pkgName := parts[0]
			if strings.HasPrefix(pkgName, "@") && len(parts) > 1 {
				pkgName += "/" + parts[1]
			}
			return rel, pkgName, false, nil
		}
	}
	return "", "", false, fmt.Errorf("can't find which package provides it (install dependencies locally once, or start with `node <file>`)")
}
