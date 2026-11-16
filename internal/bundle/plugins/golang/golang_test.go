package golang

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"xu526/internal/bundle"
	"xu526/internal/bundle/cache"
	"xu526/internal/bundle/oci"
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

const mainSrc = "package main\nfunc main(){}\n"

func TestDetect(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		mains string
		pick  string
		err   string
	}{
		{"root main", map[string]string{"go.mod": "module x\n", "main.go": mainSrc}, ".", ".", ""},
		{"cmd dir", map[string]string{"go.mod": "module x\n", "lib.go": "package x", "cmd/srv/main.go": mainSrc}, "./cmd/srv", "./cmd/srv", ""},
		{"module-named cmd wins", map[string]string{"go.mod": "module example.com/api\n", "cmd/api/main.go": mainSrc, "cmd/tool/main.go": mainSrc}, "./cmd/api,./cmd/tool", "./cmd/api", ""},
		{"ambiguous", map[string]string{"go.mod": "module x\n", "cmd/a/main.go": mainSrc, "cmd/b/main.go": mainSrc}, "./cmd/a,./cmd/b", "", "--main"},
		{"library only", map[string]string{"go.mod": "module x\n", "lib.go": "package x"}, "", "", "no main package"},
		{"skips vendor, testdata, tests and nested modules", map[string]string{"go.mod": "module x\n", "main.go": mainSrc,
			"vendor/v/main.go": mainSrc, "testdata/main.go": mainSrc, "tools/go.mod": "module t\n", "tools/main.go": mainSrc,
			"pkg/a_test.go": "package main"}, ".", ".", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := project(t, c.files)
			d, err := Plugin{}.Detect(src)
			if err != nil || d == nil {
				t.Fatalf("detect: %v %v", d, err)
			}
			if d.Details["mains"] != c.mains {
				t.Fatalf("mains = %q, want %q", d.Details["mains"], c.mains)
			}
			var mains []string
			if c.mains != "" {
				mains = strings.Split(c.mains, ",")
			}
			got, err := chooseMain(d.Details["module"], mains, "")
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("want error with %q, got %v", c.err, err)
				}
				return
			}
			if got != c.pick {
				t.Fatalf("picked %q, want %q (err %v)", got, c.pick, err)
			}
		})
	}
	if d, _ := (Plugin{}).Detect(project(t, map[string]string{"package.json": "{}"})); d != nil {
		t.Fatal("claimed a non-Go project")
	}
}

func TestMainOverride(t *testing.T) {
	m := []string{"./cmd/a", "./cmd/b"}
	for _, in := range []string{"cmd/b", "./cmd/b", "cmd/b/"} {
		if got, err := chooseMain("x", m, in); err != nil || got != "./cmd/b" {
			t.Errorf("%q -> %q %v", in, got, err)
		}
	}
	if _, err := chooseMain("x", m, "cmd/c"); err == nil {
		t.Error("unknown --main accepted")
	}
}

func TestPlanVendor(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain")
	}
	src := project(t, map[string]string{"go.mod": "module x\n", "main.go": mainSrc, "vendor/modules.txt": ""})
	d, _ := Plugin{}.Detect(src)
	p, err := Plugin{}.Plan(src, d, bundle.Options{Platform: bundle.Platform{OS: "linux", Arch: "arm64"}})
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Join(p.Steps[0].Env, " ")
	for _, want := range []string{"CGO_ENABLED=0", "GOARCH=arm64", "-mod=vendor"} {
		if !strings.Contains(env, want) {
			t.Errorf("env %q missing %s", env, want)
		}
	}
	if p.Base != "scratch" || p.Entrypoint[0] != "/app" || p.User == "" {
		t.Errorf("plan %+v", p)
	}
}

// TestBuildReproducibleAndCached compiles a real program twice into a fresh
// cache and once with --no-cache, and checks every run gives the same image.
func TestBuildReproducibleAndCached(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain")
	}
	if testing.Short() {
		t.Skip("compiles a program")
	}
	src := project(t, map[string]string{"go.mod": "module demo\n\ngo 1.21\n", "main.go": "package main\nimport \"fmt\"\nfunc main(){ fmt.Println(\"hi\") }\n"})
	store, _ := cache.Open(t.TempDir())
	run := func(noCache bool, out string) *bundle.Result {
		var log bytes.Buffer
		res, err := bundle.Build(context.Background(), bundle.Request{Dir: src.Dir, Tag: "demo:latest", Output: out,
			NoCache: noCache, Plugins: []bundle.Plugin{Plugin{}}, Cache: store, Runner: runner.Local{}, Log: &log})
		if err != nil {
			t.Fatalf("%v\n%s", err, log.String())
		}
		return res
	}
	dir := t.TempDir()
	a := run(false, filepath.Join(dir, "a.tar"))
	b := run(false, filepath.Join(dir, "b.tar"))
	c := run(true, filepath.Join(dir, "c.tar"))
	if !a.Ran || b.Ran || !c.Ran {
		t.Fatalf("ran: %v %v %v (second build should be a pure cache hit)", a.Ran, b.Ran, c.Ran)
	}
	if a.Image.Digest() != b.Image.Digest() || a.Image.Digest() != c.Image.Digest() {
		t.Fatalf("digests differ: %s %s %s", a.Image.Digest(), b.Image.Digest(), c.Image.Digest())
	}
	ta, _ := os.ReadFile(filepath.Join(dir, "a.tar"))
	tc, _ := os.ReadFile(filepath.Join(dir, "c.tar"))
	if !bytes.Equal(ta, tc) {
		t.Fatal("tarballs differ byte-for-byte")
	}
	app := a.Image.Layers[len(a.Image.Layers)-1]
	h, _ := oci.ReadLayerFiles(app.Blob)
	if h["app"] == nil || h["app"].Mode != 0o755 {
		t.Fatalf("app layer entries: %v", h)
	}
	// Writing the tarball inside the project must not bust the cache.
	in1 := run(false, filepath.Join(src.Dir, "demo.tar"))
	if in2 := run(false, filepath.Join(src.Dir, "demo.tar")); in2.Ran || in1.Image.Digest() != a.Image.Digest() {
		t.Fatal("output tarball inside the project was treated as an input")
	}
	// An edit invalidates the app layer.
	os.WriteFile(filepath.Join(src.Dir, "main.go"), []byte("package main\nfunc main(){}\n"), 0o644)
	if d := run(false, ""); !d.Ran || d.Image.Digest() == a.Image.Digest() {
		t.Fatal("source edit did not trigger a rebuild")
	}
}
