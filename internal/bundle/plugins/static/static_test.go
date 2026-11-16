package static

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"xu526/internal/bundle"
	"xu526/internal/bundle/cache"
	"xu526/internal/bundle/oci"
	"xu526/internal/bundle/plugins/golang"
	"xu526/internal/bundle/runner"
)

func project(t *testing.T, files map[string]string) *bundle.Source {
	t.Helper()
	d := t.TempDir()
	for p, s := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(d, p)), 0o755)
		os.WriteFile(filepath.Join(d, p), []byte(s), 0o644)
	}
	src, err := bundle.NewSource(d)
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func TestDetect(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		dir   string // "" = not claimed
		spa   bool
	}{
		{"plain site", map[string]string{"index.html": "", "about.html": "", "style.css": ""}, ".", false},
		{"built output", map[string]string{"package.json": "{}", "src/main.js": "", "dist/index.html": "", "dist/assets/a.js": ""}, "dist", true},
		{"vite source, not built", map[string]string{"package.json": "{}", "index.html": "", "src/main.js": ""}, "", false},
		{"hugo public", map[string]string{"config.toml": "", "public/index.html": "", "public/blog/index.html": ""}, "public", false},
		{"200.html marks spa", map[string]string{"index.html": "", "200.html": "", "a.html": ""}, ".", true},
		{"netlify rewrite marks spa", map[string]string{"index.html": "", "_redirects": "/*  /index.html  200", "b.html": ""}, ".", true},
		{"one page without js is not spa", map[string]string{"index.html": "", "404.html": ""}, ".", false},
		{"nothing", map[string]string{"README.md": ""}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := Plugin{}.Detect(project(t, c.files))
			if err != nil {
				t.Fatal(err)
			}
			if c.dir == "" {
				if d != nil {
					t.Fatalf("claimed: %+v", d)
				}
				return
			}
			if d == nil || d.Details["dir"] != c.dir || (d.Details["spa"] == "1") != c.spa {
				t.Fatalf("got %+v, want dir %s spa %v", d, c.dir, c.spa)
			}
		})
	}
}

func TestGoProjectWithPublicFolderStaysGo(t *testing.T) {
	src := project(t, map[string]string{"go.mod": "module x\n", "main.go": "package main\nfunc main(){}\n", "public/index.html": ""})
	p, _, err := bundle.Detect(src, []bundle.Plugin{golang.Plugin{}, Plugin{}}, "")
	if err != nil || p.Name() != "go" {
		t.Fatalf("got %v %v", p, err)
	}
}

func TestSiteFilesLeavesOutSecrets(t *testing.T) {
	src := project(t, map[string]string{"index.html": "", ".env": "SECRET", ".git/config": "", ".well-known/security.txt": "",
		"node_modules/x/index.js": "", "img/logo.png": "", "site.tar": ""})
	os.Symlink("/etc/passwd", filepath.Join(src.Dir, "passwd"))
	files, skipped, err := SiteFiles(src, ".")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".well-known/security.txt", "img/logo.png", "index.html"}
	if len(files) != len(want) || skipped != 3 {
		t.Fatalf("files %v skipped %d", files, skipped)
	}
	for i := range want {
		if files[i] != want[i] {
			t.Fatalf("files %v, want %v", files, want)
		}
	}
}

func TestBuild(t *testing.T) {
	src := project(t, map[string]string{"index.html": "<h1>hi</h1>", "css/site.css": "body{}", ".env": "SECRET=1"})
	store, _ := cache.Open(t.TempDir())
	run := func(arch string) *bundle.Result {
		var log bytes.Buffer
		res, err := bundle.Build(context.Background(), bundle.Request{Dir: src.Dir, Tag: "site", Output: filepath.Join(src.Dir, "site.tar"),
			Options: bundle.Options{Platform: bundle.Platform{OS: "linux", Arch: arch}},
			Plugins: []bundle.Plugin{Plugin{}}, Cache: store, Runner: runner.Local{}, Log: &log})
		if err != nil {
			t.Fatalf("%v\n%s", err, log.String())
		}
		return res
	}
	a := run("amd64")
	b := run("amd64")
	if b.Ran || a.Image.Digest() != b.Image.Digest() {
		t.Fatal("second build should be an identical cache hit")
	}
	site, _ := oci.ReadLayerFiles(a.Image.Layers[1].Blob)
	if site["site/index.html"] == nil || site["site/css/site.css"] == nil || site["site/.env"] != nil || site["site/site.tar"] != nil {
		t.Fatalf("site layer: %v", site)
	}
	server, _ := oci.ReadLayerFiles(a.Image.Layers[0].Blob)
	if h := server["xu-static"]; h == nil || h.Mode != 0o755 || h.Size < 1<<20 {
		t.Fatalf("server layer: %v", server)
	}
	// Editing the site rebuilds only the site layer.
	os.WriteFile(filepath.Join(src.Dir, "index.html"), []byte("<h1>changed</h1>"), 0o644)
	c := run("amd64")
	if !c.Layers[0].Cached || c.Layers[1].Cached || c.Image.Digest() == a.Image.Digest() {
		t.Fatalf("after edit: %+v", c.Layers)
	}
	if d := run("arm64"); d.Layers[0].Digest == a.Layers[0].Digest || d.Image.Arch != "arm64" {
		t.Fatal("arm64 should use the arm64 server")
	}
}

// TestEmbeddedServerIsCurrent rebuilds xu-static and compares it with the
// embedded copy, so a change to server/ without `go generate` is caught.
// Only meaningful with the Go version the binaries were built with.
func TestEmbeddedServerIsCurrent(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles the server")
	}
	if runtime.Version() != ServerVersion() {
		t.Skipf("embedded server built with %s, this is %s", ServerVersion(), runtime.Version())
	}
	out := filepath.Join(t.TempDir(), "s")
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-s -w -buildid=", "-o", out, "./server")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64", "GOFLAGS=")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	fresh, _ := os.ReadFile(out)
	embedded, _ := ServerBinary("amd64")
	if !bytes.Equal(fresh, embedded) {
		t.Fatal("bin/ is out of date: run `go generate ./internal/bundle/plugins/static`")
	}
}
