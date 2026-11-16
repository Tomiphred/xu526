package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func writeTar(t *testing.T, files map[string]string) string {
	p := filepath.Join(t.TempDir(), "img.tar")
	f, _ := os.Create(p)
	tw := tar.NewWriter(f)
	for _, name := range []string{"blobs/sha256/aaa", "index.json", "manifest.json", "oci-layout"} {
		if body, ok := files[name]; ok {
			tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))})
			tw.Write([]byte(body))
		}
	}
	tw.Close()
	f.Close()
	return p
}

func TestRetagTarball(t *testing.T) {
	p := writeTar(t, map[string]string{
		"blobs/sha256/aaa": "layer-bytes",
		"index.json":       `{"schemaVersion":2,"manifests":[{"digest":"sha256:aaa","annotations":{"io.containerd.image.name":"docker.io/library/nginx:latest"}}]}`,
		"manifest.json":    `[{"Config":"blobs/sha256/aaa","RepoTags":["nginx:latest"],"Layers":[]}]`,
		"oci-layout":       `{"imageLayoutVersion":"1.0.0"}`,
	})
	if name, err := tarballName(p); err != nil || name != "nginx:latest" {
		t.Fatalf("name %q %v", name, err)
	}
	var out bytes.Buffer
	if err := retag(p, "xu/web:v1", &out); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&out)
	got := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		if int64(len(b)) != h.Size {
			t.Fatalf("%s: header size %d, body %d", h.Name, h.Size, len(b))
		}
		got[h.Name] = string(b)
	}
	var m []struct{ RepoTags []string }
	json.Unmarshal([]byte(got["manifest.json"]), &m)
	var idx struct {
		Manifests []struct{ Annotations map[string]string }
	}
	json.Unmarshal([]byte(got["index.json"]), &idx)
	if m[0].RepoTags[0] != "xu/web:v1" || idx.Manifests[0].Annotations["io.containerd.image.name"] != "docker.io/xu/web:v1" ||
		idx.Manifests[0].Annotations["org.opencontainers.image.ref.name"] != "v1" || got["blobs/sha256/aaa"] != "layer-bytes" {
		t.Fatalf("retagged: %v", got)
	}
	multi := writeTar(t, map[string]string{"manifest.json": `[{"RepoTags":["a:1"]},{"RepoTags":["b:1"]}]`})
	if _, err := tarballName(multi); err == nil {
		t.Fatal("several images in one tarball should be refused")
	}
}
