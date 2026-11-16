package node

import (
	"bytes"
	"context"
	"encoding/json"
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

// TestServerBuild runs the real pipeline: npm ci on this machine, a base
// pulled from a test registry standing in for gcr.io, and checks the image.
func TestServerBuild(t *testing.T) {
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not installed")
	}
	_, host := hostNode()
	if !isSupported(host) {
		t.Skipf("this machine's Node %d has no distroless image", host)
	}
	reg := regtest.New()
	defer reg.Close()
	base, _ := oci.BuildLayer([]oci.File{{Data: []byte("node"), Dst: "/nodejs/bin/node", Mode: 0o755}}, 0)
	im := &oci.Image{OS: "linux", Arch: "amd64", Layers: []*oci.Layer{base},
		Config: oci.Config{Entrypoint: []string{"/nodejs/bin/node"}, User: "65532", Env: []string{"PATH=/bin"}}}
	im.Seal()
	reg.PutBlob(base.Blob)
	reg.PutBlob(im.ConfigJSON())
	repo := strings.TrimPrefix(strings.SplitN(baseFor(host), ":", 2)[0], "gcr.io/")
	reg.PutManifest(repo, "nonroot", oci.MediaManifest, im.Manifest())

	src := project(t, map[string]string{
		"package.json":      `{"name":"x","version":"1.0.0","scripts":{"start":"NODE_OPTIONS=--enable-source-maps node index.js","prepare":"husky"}}`,
		"package-lock.json": `{"name":"x","version":"1.0.0","lockfileVersion":3,"requires":true,"packages":{"":{"name":"x","version":"1.0.0"}}}`,
		"index.js":          `require("http").createServer((q, s) => s.end("hi")).listen(process.env.PORT)`,
		".env":              "SECRET=1",
	})
	store, _ := cache.Open(t.TempDir())
	build := func() *bundle.Result {
		var log bytes.Buffer
		res, err := bundle.Build(context.Background(), bundle.Request{Dir: src.Dir, Tag: "x", Plugins: []bundle.Plugin{Plugin{}},
			Cache: store, Runner: runner.Local{}, Log: &log, Registry: registry.New(),
			Options: bundle.Options{Platform: bundle.Platform{OS: "linux", Arch: "amd64"}},
			Mirrors: map[string]string{"gcr.io": reg.Host()}})
		if err != nil {
			t.Fatalf("%v\n%s", err, log.String())
		}
		return res
	}
	a := build()
	if a.Plan.Strategy != bundle.RuntimeDeps || len(a.Image.Layers) != 3 || a.Image.BaseLayers != 1 {
		t.Fatalf("plan %s layers %d", a.Plan.Strategy, len(a.Image.Layers))
	}
	app, _ := oci.ReadLayerFiles(a.Image.Layers[2].Blob)
	if app["app/index.js"] == nil || app["app/package.json"] == nil || app["app/.env"] != nil || app["app/xu.lock"] != nil {
		t.Fatalf("app layer: %v", app)
	}
	var cfg struct {
		Config struct {
			Entrypoint, Cmd, Env []string
			User, WorkingDir     string
		} `json:"config"`
	}
	json.Unmarshal(a.Image.ConfigJSON(), &cfg)
	c := cfg.Config
	env := strings.Join(c.Env, " ")
	if c.Entrypoint[0] != "/nodejs/bin/node" || strings.Join(c.Cmd, " ") != "index.js" || c.WorkingDir != "/app" || c.User != "65532" ||
		!strings.Contains(env, "NODE_OPTIONS=--enable-source-maps") || !strings.Contains(env, "NODE_ENV=production") {
		t.Fatalf("config: %+v", c)
	}
	if b := build(); b.Ran || b.Image.Digest() != a.Image.Digest() {
		t.Fatal("unchanged rebuild should be a cache hit with the same digest")
	}
}

// TestCrossArchDeps installs a package with prebuilt native binaries for the
// other CPU and checks the Linux build for the image's CPU lands in the
// deps layer. It talks to the npm registry, so it runs only with
// XU_NET_TESTS=1.
func TestCrossArchDeps(t *testing.T) {
	if os.Getenv("XU_NET_TESTS") == "" {
		t.Skip("set XU_NET_TESTS=1 (needs the npm registry)")
	}
	_, host := hostNode()
	if !isSupported(host) {
		t.Skipf("this machine's Node %d has no distroless image", host)
	}
	other, cpu := "arm64", "arm64"
	if runtime.GOARCH == "arm64" {
		other, cpu = "amd64", "x64"
	}
	reg := regtest.New()
	defer reg.Close()
	base, _ := oci.BuildLayer([]oci.File{{Data: []byte("node"), Dst: "/nodejs/bin/node", Mode: 0o755}}, 0)
	im := &oci.Image{OS: "linux", Arch: other, Layers: []*oci.Layer{base}, Config: oci.Config{Entrypoint: []string{"/nodejs/bin/node"}}}
	im.Seal()
	reg.PutBlob(base.Blob)
	reg.PutBlob(im.ConfigJSON())
	reg.PutManifest(strings.TrimPrefix(strings.SplitN(baseFor(host), ":", 2)[0], "gcr.io/"), "nonroot", oci.MediaManifest, im.Manifest())

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"x","version":"1.0.0","dependencies":{"@node-rs/xxhash":"1.7.6"}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "index.js"), []byte(`require("@node-rs/xxhash")`), 0o644)
	if out, err := exec.Command("npm", "install", "--package-lock-only", "--prefix", dir).CombinedOutput(); err != nil {
		t.Skipf("npm registry unreachable: %v\n%s", err, out)
	}
	var log bytes.Buffer
	store, _ := cache.Open(t.TempDir())
	res, err := bundle.Build(context.Background(), bundle.Request{Dir: dir, Tag: "x", Plugins: []bundle.Plugin{Plugin{}},
		Cache: store, Runner: runner.Local{}, Log: &log, Registry: registry.New(),
		Options: bundle.Options{Platform: bundle.Platform{OS: "linux", Arch: other}},
		Mirrors: map[string]string{"gcr.io": reg.Host()}})
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	deps, _ := oci.ReadLayerFiles(res.Image.Layers[1].Blob)
	var natives []string
	for f := range deps {
		if strings.HasSuffix(f, ".node") {
			natives = append(natives, f)
		}
	}
	gnu, wrong := false, false
	for _, n := range natives {
		gnu = gnu || strings.Contains(n, "linux-"+cpu+"-gnu")
		wrong = wrong || !strings.Contains(n, "linux-"+cpu) // npm can't filter libc without lockfile data; extra musl copies are unused
	}
	if !gnu || wrong {
		t.Fatalf("native files in deps layer: %v\n%s", natives, log.String())
	}
}
