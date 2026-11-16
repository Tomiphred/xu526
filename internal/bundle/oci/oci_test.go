package oci

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func sample(t *testing.T) []File {
	t.Helper()
	p := filepath.Join(t.TempDir(), "bin")
	os.WriteFile(p, []byte("binary"), 0o600)
	return []File{
		{Src: p, Dst: "/app", Mode: 0o755},
		{Data: []byte("ca"), Dst: "/etc/ssl/certs/ca.crt"},
	}
}

func TestLayerIsReproducible(t *testing.T) {
	f := sample(t)
	a, err := BuildLayer(f, 0)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := BuildLayer([]File{f[1], f[0]}, 0) // order must not matter
	if a.Digest != b.Digest || a.DiffID != b.DiffID {
		t.Fatalf("digests differ: %s vs %s", a.Digest, b.Digest)
	}
	c, _ := BuildLayer(f, 1)
	if c.Digest == a.Digest {
		t.Fatal("epoch should change the digest")
	}
}

func TestLayerEntries(t *testing.T) {
	l, err := BuildLayer(sample(t), 42)
	if err != nil {
		t.Fatal(err)
	}
	h, err := ReadLayerFiles(l.Blob)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"etc/", "etc/ssl/", "etc/ssl/certs/"} {
		if h[d] == nil || h[d].Typeflag != tar.TypeDir {
			t.Errorf("missing parent dir %s", d)
		}
	}
	app := h["app"]
	if app == nil || app.Mode != 0o755 || app.Uid != 0 || app.ModTime.Unix() != 42 {
		t.Fatalf("bad /app header: %+v", app)
	}
	if h["etc/ssl/certs/ca.crt"].Mode != 0o644 {
		t.Error("default mode should be 0644")
	}
	if _, err := BuildLayer([]File{{Data: nil, Dst: "/a"}, {Data: nil, Dst: "a"}}, 0); err == nil {
		t.Error("duplicate destinations should fail")
	}
}

func TestImageTarball(t *testing.T) {
	l, _ := BuildLayer(sample(t), 0)
	im := &Image{OS: "linux", Arch: "amd64", Layers: []*Layer{l}, LayerHistory: []string{"app"},
		Config: Config{Entrypoint: []string{"/app"}, Env: []string{"PORT=8080"}, User: "65532", Port: 8080}}
	if err := im.Seal(); err != nil {
		t.Fatal(err)
	}
	var a, b bytes.Buffer
	im.WriteTar(&a, "demo:1")
	im.WriteTar(&b, "demo:1")
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("tarball not reproducible")
	}
	files := map[string][]byte{}
	tr := tar.NewReader(&a)
	var order []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		order = append(order, h.Name)
		files[h.Name], _ = io.ReadAll(tr)
	}
	for _, n := range []string{"oci-layout", "index.json", "manifest.json"} {
		if files[n] == nil {
			t.Fatalf("missing %s (have %v)", n, order)
		}
	}
	// every blob is stored under its own digest
	for n, b := range files {
		if len(n) > 13 && n[:13] == "blobs/sha256/" && Digest(b) != "sha256:"+n[13:] {
			t.Errorf("%s does not match its content", n)
		}
	}
	var dm []struct {
		Config   string
		RepoTags []string
		Layers   []string
	}
	json.Unmarshal(files["manifest.json"], &dm)
	if len(dm) != 1 || dm[0].RepoTags[0] != "demo:1" || files[dm[0].Config] == nil || files[dm[0].Layers[0]] == nil {
		t.Fatalf("bad docker manifest: %s", files["manifest.json"])
	}
	var cfg struct {
		Architecture string
		Config       struct {
			Entrypoint   []string
			ExposedPorts map[string]any
		}
		RootFS struct {
			DiffIDs []string `json:"diff_ids"`
		} `json:"rootfs"`
	}
	json.Unmarshal(files[dm[0].Config], &cfg)
	if cfg.Architecture != "amd64" || cfg.Config.Entrypoint[0] != "/app" || cfg.Config.ExposedPorts["8080/tcp"] == nil || cfg.RootFS.DiffIDs[0] != l.DiffID {
		t.Fatalf("bad config: %s", files[dm[0].Config])
	}
	var idx struct {
		Manifests []struct {
			Digest      string
			Annotations map[string]string
		}
	}
	json.Unmarshal(files["index.json"], &idx)
	if idx.Manifests[0].Digest != im.Digest() || idx.Manifests[0].Annotations["org.opencontainers.image.ref.name"] != "1" {
		t.Fatalf("bad index: %s", files["index.json"])
	}
}

func TestNormalizeRef(t *testing.T) {
	for in, want := range map[string]string{
		"app": "docker.io/library/app:latest", "app:1": "docker.io/library/app:1", "me/app:1": "docker.io/me/app:1",
		"ghcr.io/me/app": "ghcr.io/me/app:latest", "localhost:5000/app:2": "localhost:5000/app:2", "localhost/app": "localhost/app:latest",
	} {
		if got := NormalizeRef(in); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
}
