package python

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xu526/internal/bundle"
	"xu526/internal/bundle/cache"
	"xu526/internal/bundle/oci"
	"xu526/internal/bundle/registry"
	"xu526/internal/bundle/registry/regtest"
	"xu526/internal/bundle/runner"
)

// TestPipeline builds a project end to end with a fake runtime tarball
// served as a "release" and a fake distroless base from a test registry.
func TestPipeline(t *testing.T) {
	if _, err := pipVersion(); err != nil {
		t.Skip(err)
	}
	reg := regtest.New()
	defer reg.Close()
	baseLayer, _ := oci.BuildLayer([]oci.File{{Data: []byte("libc"), Dst: "/lib/x86_64-linux-gnu/libc.so.6"}}, 0)
	base := &oci.Image{OS: "linux", Arch: "amd64", Layers: []*oci.Layer{baseLayer},
		Config: oci.Config{User: "65532", Env: []string{"PATH=/usr/bin:/bin", "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt"}}}
	base.Seal()
	reg.PutBlob(baseLayer.Blob)
	reg.PutBlob(base.ConfigJSON())
	reg.PutManifest("distroless/cc-debian13", "nonroot", oci.MediaManifest, base.Manifest())

	dl := download(12, "amd64")
	tb, _ := os.ReadFile(tarball(t, map[string]string{"python/bin/python3.12": "elf", "python/lib/python3.12/os.py": "",
		"python/lib/python3.12/site-packages/pip/__init__.py": ""}, map[string]string{"python/bin/python3": "python3.12"}))
	sum := sha256.Sum256(tb)
	name := strings.ReplaceAll(dl.URL[strings.LastIndex(dl.URL, "/")+1:], "%2B", "+")
	rel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/SHA256SUMS"):
			w.Write([]byte(hex.EncodeToString(sum[:]) + "  " + name + "\n"))
		case strings.HasSuffix(r.URL.Path, "install_only_stripped.tar.gz"):
			w.Write(tb)
		default:
			http.NotFound(w, r)
		}
	}))
	defer rel.Close()

	src := project(t, map[string]string{".python-version": "3.12", "main.py": "print('hi')\n", "lib/util.py": "", ".env": "SECRET=1",
		"lib/__pycache__/util.cpython-312.pyc": "", ".venv/pyvenv.cfg": "", "venv2/pyvenv.cfg": "", "venv2/bin/python": ""})
	store, _ := cache.Open(t.TempDir())
	build := func() *bundle.Result {
		var log bytes.Buffer
		res, err := bundle.Build(context.Background(), bundle.Request{Dir: src.Dir, Tag: "py", Plugins: []bundle.Plugin{Plugin{}},
			Cache: store, Runner: runner.Local{}, Log: &log, Registry: registry.New(),
			Mirrors: map[string]string{"gcr.io": reg.Host(), ReleaseBase: rel.URL}})
		if err != nil {
			t.Fatalf("%v\n%s", err, log.String())
		}
		return res
	}
	a := build()
	im := a.Image
	if len(im.Layers) != 4 || im.BaseLayers != 1 {
		t.Fatalf("%d layers", len(im.Layers))
	}
	py, _ := oci.ReadLayerFiles(im.Layers[1].Blob)
	if py["opt/python/bin/python3.12"] == nil || py["opt/python/bin/python3"] == nil || py["opt/python/lib/python3.12/site-packages/pip/__init__.py"] != nil {
		t.Fatalf("python layer: %v", py)
	}
	app, _ := oci.ReadLayerFiles(im.Layers[3].Blob)
	if app["app/main.py"] == nil || app["app/lib/util.py"] == nil || app["app/.env"] != nil || app["app/venv2/bin/python"] != nil ||
		app["app/lib/__pycache__/util.cpython-312.pyc"] != nil || app["app/xu.lock"] != nil {
		t.Fatalf("app layer: %v", app)
	}
	var cfg struct {
		Config struct {
			Entrypoint, Cmd, Env []string
			User, WorkingDir     string
		} `json:"config"`
	}
	json.Unmarshal(im.ConfigJSON(), &cfg)
	c := cfg.Config
	env := strings.Join(c.Env, " ")
	if c.Entrypoint[0] != "/opt/python/bin/python3" || c.Cmd[0] != "main.py" || c.User != "65532" || c.WorkingDir != "/app" ||
		!strings.Contains(env, "PATH=/opt/python/bin:") || !strings.Contains(env, "PYTHONUNBUFFERED=1") {
		t.Fatalf("config %+v", c)
	}
	lock, _ := os.ReadFile(filepath.Join(src.Dir, bundle.LockFile))
	if !strings.Contains(string(lock), hex.EncodeToString(sum[:])) || !strings.Contains(string(lock), ReleaseBase) {
		t.Fatalf("lock: %s", lock)
	}
	if b := build(); b.Ran || b.Image.Digest() != im.Digest() {
		t.Fatal("unchanged rebuild should be a cache hit")
	}
}
