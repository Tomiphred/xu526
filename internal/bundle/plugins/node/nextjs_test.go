package node

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"xu526/internal/bundle"
	"xu526/internal/bundle/cache"
	"xu526/internal/bundle/oci"
	"xu526/internal/bundle/registry"
	"xu526/internal/bundle/registry/regtest"
	"xu526/internal/bundle/runner"
)

// fakeBuild writes what `next build` produces with output: "standalone".
const fakeBuild = `const fs = require("fs");
const w = (p, s) => { fs.mkdirSync(require("path").dirname(p), { recursive: true }); fs.writeFileSync(p, s); };
w(".next/standalone/server.js", "require('http').createServer((q,s)=>s.end('next standalone on '+process.env.HOSTNAME+':'+process.env.PORT)).listen(process.env.PORT, process.env.HOSTNAME)");
w(".next/standalone/package.json", "{}");
w(".next/standalone/.next/BUILD_ID", "abc123");
w(".next/standalone/.next/server/app/page.js", "module.exports={}");
w(".next/standalone/node_modules/next/package.json", "{\"name\":\"next\"}");
w(".next/standalone/.env.production", "SECRET=leak");
w(".next/static/chunks/main-abc.js", "console.log(1)");
w(".next/cache/webpack/huge.pack", "x".repeat(1000));
`

func TestNextStandalone(t *testing.T) {
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not installed")
	}
	_, host := hostNode()
	if !isSupported(host) {
		t.Skipf("Node %d has no distroless image", host)
	}
	src := project(t, map[string]string{
		"package.json":                `{"name":"site","version":"1.0.0","private":true,"scripts":{"build":"node build.js","start":"next start"},"dependencies":{"next":"file:./stub-next"}}`,
		"stub-next/package.json":      `{"name":"next","version":"15.0.0","bin":{"next":"bin.js"}}`,
		"stub-next/bin.js":            "",
		"build.js":                    fakeBuild,
		"next.config.ts":              "const nextConfig = { output: \"standalone\" };\nexport default nextConfig;\n",
		"public/logo.svg":             "<svg/>",
		"app/page.tsx":                "export default function Page() { return null }",
		".env":                        "SECRET=leak",
		".next/stale-local-build.txt": "from npm run dev",
	})
	lock := exec.Command("npm", "install", "--package-lock-only", "--no-audit", "--no-fund", "--offline")
	lock.Dir = src.Dir
	if out, err := lock.CombinedOutput(); err != nil {
		t.Skipf("npm can't write a lockfile offline here: %v\n%s", err, out)
	}
	p, _ := readPkg(src)
	if !isNextStandalone(src, p) {
		t.Fatal("standalone not detected")
	}

	reg := regtest.New()
	defer reg.Close()
	base, _ := oci.BuildLayer([]oci.File{{Data: []byte("node"), Dst: "/nodejs/bin/node", Mode: 0o755}}, 0)
	im := &oci.Image{OS: "linux", Arch: "amd64", Layers: []*oci.Layer{base},
		Config: oci.Config{Entrypoint: []string{"/nodejs/bin/node"}, User: "65532"}}
	im.Seal()
	reg.PutBlob(base.Blob)
	reg.PutBlob(im.ConfigJSON())
	repo := strings.TrimPrefix(strings.SplitN(baseFor(host), ":", 2)[0], "gcr.io/")
	reg.PutManifest(repo, "nonroot", oci.MediaManifest, im.Manifest())

	var log bytes.Buffer
	store, _ := cache.Open(t.TempDir())
	res, err := bundle.Build(context.Background(), bundle.Request{Dir: src.Dir, Tag: "site", Plugins: []bundle.Plugin{Plugin{}},
		Cache: store, Runner: runner.Local{}, Log: &log, Registry: registry.New(),
		Options: bundle.Options{Platform: bundle.Platform{OS: "linux", Arch: "amd64"}},
		Mirrors: map[string]string{"gcr.io": reg.Host()}})
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if len(res.Image.Layers) != 2 {
		t.Fatalf("want base + one app layer, got %d", len(res.Image.Layers))
	}
	files, _ := oci.ReadLayerFiles(res.Image.Layers[1].Blob)
	for _, want := range []string{"app/server.js", "app/.next/BUILD_ID", "app/.next/server/app/page.js", "app/node_modules/next/package.json",
		"app/.next/static/chunks/main-abc.js", "app/public/logo.svg"} {
		if files[want] == nil {
			t.Errorf("missing %s", want)
		}
	}
	for name := range files {
		if strings.Contains(name, ".env") || strings.Contains(name, ".next/cache") || strings.Contains(name, "stale-local-build") || strings.Contains(name, "app/page.tsx") {
			t.Errorf("%s should not be in the image", name)
		}
	}
	var cfg struct {
		Config struct {
			Cmd, Env   []string
			WorkingDir string
		} `json:"config"`
	}
	json.Unmarshal(res.Image.ConfigJSON(), &cfg)
	env := strings.Join(cfg.Config.Env, " ")
	if strings.Join(cfg.Config.Cmd, " ") != "server.js" || cfg.Config.WorkingDir != "/app" || !strings.Contains(env, "HOSTNAME=0.0.0.0") || !strings.Contains(env, "PORT=8080") {
		t.Fatalf("config: %+v", cfg.Config)
	}
	if !strings.Contains(log.String(), "Next.js standalone output") {
		t.Fatalf("plan didn't say standalone:\n%s", log.String())
	}
	os.WriteFile(filepath.Join(t.TempDir(), "x"), nil, 0o644)
}

func TestNextBuildOutputKept(t *testing.T) {
	// Non-standalone Next apps run `next start`, which needs .next (but not its cache).
	d := t.TempDir()
	for p, s := range map[string]string{".next/BUILD_ID": "1", ".next/cache/x": "big", ".env.local": "SECRET", ".git/HEAD": "", "server.js": "", ".nuxt/a.js": ""} {
		os.MkdirAll(filepath.Dir(filepath.Join(d, p)), 0o755)
		os.WriteFile(filepath.Join(d, p), []byte(s), 0o644)
	}
	entries, err := appFiles(d)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Dst] = true
	}
	if !got["/app/.next/BUILD_ID"] || !got["/app/server.js"] || !got["/app/.nuxt/a.js"] || got["/app/.next/cache/x"] || got["/app/.env.local"] || got["/app/.git/HEAD"] {
		t.Fatalf("%v", got)
	}
}

// TestNextStandaloneCross builds for the other CPU: next build runs with
// this machine's tools, then the traced node_modules is replaced by a
// production install for the image's system.
func TestNextStandaloneCross(t *testing.T) {
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not installed")
	}
	_, host := hostNode()
	if !isSupported(host) {
		t.Skipf("Node %d has no distroless image", host)
	}
	other := "arm64"
	if runtime.GOARCH == "arm64" {
		other = "amd64"
	}
	src := project(t, map[string]string{
		"package.json":           `{"name":"site","version":"1.0.0","private":true,"scripts":{"build":"node build.js","start":"next start"},"devDependencies":{"next":"file:./stub-next"}}`,
		"stub-next/package.json": `{"name":"next","version":"15.0.0","bin":{"next":"bin.js"}}`,
		"stub-next/bin.js":       "",
		"build.js":               fakeBuild,
		"next.config.js":         "module.exports = { output: 'standalone' }\n",
		"public/a.txt":           "a",
	})
	lock := exec.Command("npm", "install", "--package-lock-only", "--no-audit", "--no-fund", "--offline")
	lock.Dir = src.Dir
	if out, err := lock.CombinedOutput(); err != nil {
		t.Skipf("npm can't write a lockfile offline here: %v\n%s", err, out)
	}
	reg := regtest.New()
	defer reg.Close()
	base, _ := oci.BuildLayer([]oci.File{{Data: []byte("node"), Dst: "/nodejs/bin/node", Mode: 0o755}}, 0)
	im := &oci.Image{OS: "linux", Arch: other, Layers: []*oci.Layer{base}, Config: oci.Config{Entrypoint: []string{"/nodejs/bin/node"}}}
	im.Seal()
	reg.PutBlob(base.Blob)
	reg.PutBlob(im.ConfigJSON())
	reg.PutManifest(strings.TrimPrefix(strings.SplitN(baseFor(host), ":", 2)[0], "gcr.io/"), "nonroot", oci.MediaManifest, im.Manifest())

	var log bytes.Buffer
	store, _ := cache.Open(t.TempDir())
	res, err := bundle.Build(context.Background(), bundle.Request{Dir: src.Dir, Tag: "site", Plugins: []bundle.Plugin{Plugin{}},
		Cache: store, Runner: runner.Local{}, Log: &log, Registry: registry.New(),
		Options: bundle.Options{Platform: bundle.Platform{OS: "linux", Arch: other}},
		Mirrors: map[string]string{"gcr.io": reg.Host()}})
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	files, _ := oci.ReadLayerFiles(res.Image.Layers[1].Blob)
	if files["app/server.js"] == nil || files["app/public/a.txt"] == nil {
		t.Fatalf("missing app files: %v", files)
	}
	if files["app/node_modules/next/package.json"] != nil {
		t.Error("traced node_modules (this machine's) should have been replaced by the Linux install")
	}
	if !strings.Contains(log.String(), "installs production dependencies for linux/"+other) || !strings.Contains(log.String(), "npm_config_cpu") && !strings.Contains(log.String(), "npm ci") {
		t.Fatalf("log:\n%s", log.String())
	}
}

// TestCompileNextConfig checks next.config.ts becomes JavaScript Node can
// load without TypeScript (with a stand-in typescript package).
func TestCompileNextConfig(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	stub := `exports.ModuleKind = { CommonJS: 1, ESNext: 99 }; exports.ScriptTarget = { ES2022: 9 };
exports.transpileModule = (src, o) => ({ outputText: o.compilerOptions.module === 1
  ? src.replace(/^import type.*$/m, "").replace(/: NextConfig/g, "").replace("export default", "Object.defineProperty(exports, '__esModule', { value: true }); exports.default =")
  : src.replace(/^import type.*$/m, "").replace(/: NextConfig/g, "") });`
	for _, c := range []struct {
		pkg, file, want, body string
	}{
		{`{"name":"a"}`, "next.config.ts", "next.config.js", ""},
		// import.meta only works in an ES module, whatever package.json says.
		{`{"name":"a"}`, "next.config.ts", "next.config.mjs",
			"import path from \"node:path\";\nimport { fileURLToPath } from \"node:url\";\nimport type { NextConfig } from \"next\";\n" +
				"const dir = path.dirname(fileURLToPath(import.meta.url));\nconst cfg: NextConfig = { poweredByHeader: false, outputFileTracingRoot: dir };\nexport default cfg;\n"},
		{`{"name":"a","type":"module"}`, "next.config.ts", "next.config.mjs", ""},
		{`{"name":"a"}`, "next.config.mts", "next.config.mjs", ""},
	} {
		if c.body == "" {
			c.body = "import type { NextConfig } from \"next\";\nconst cfg: NextConfig = { poweredByHeader: false };\nexport default cfg;\n"
		}
		dir := t.TempDir()
		for p, s := range map[string]string{
			"package.json": c.pkg, ".next/BUILD_ID": "1", "node_modules/typescript/index.js": stub,
			c.file: c.body,
		} {
			os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755)
			os.WriteFile(filepath.Join(dir, p), []byte(s), 0o644)
		}
		var log bytes.Buffer
		env := &bundle.BuildEnv{Runner: runner.Local{}, Log: &log}
		if err := compileNextConfig(context.Background(), env, dir); err != nil {
			t.Fatalf("%s: %v\n%s", c.file, err, log.String())
		}
		if _, err := os.Stat(filepath.Join(dir, c.file)); !os.IsNotExist(err) {
			t.Errorf("%s should be gone", c.file)
		}
		// Load it the way Next does: import() and take .default.
		os.RemoveAll(filepath.Join(dir, "node_modules", "typescript"))
		load := exec.Command("node", "--input-type=module", "-e",
			`const m = await import(process.argv[1]); const c = m.default; if (c.poweredByHeader !== false) { console.error(JSON.stringify(m)); process.exit(1) }`,
			filepath.Join(dir, c.want))
		if out, err := load.CombinedOutput(); err != nil {
			t.Errorf("%s → %s doesn't load as a config: %v\n%s", c.file, c.want, err, out)
		}
	}
	// No TypeScript installed: a clear error, not a runtime surprise.
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".next"), 0o755)
	os.WriteFile(filepath.Join(dir, "next.config.ts"), []byte("export default {}"), 0o644)
	err := compileNextConfig(context.Background(), &bundle.BuildEnv{Runner: runner.Local{}, Log: io.Discard}, dir)
	if err == nil || !strings.Contains(err.Error(), "npm i -D typescript") {
		t.Fatalf("got %v", err)
	}
}
