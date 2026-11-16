package bundle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xu526/internal/bundle/cache"
	"xu526/internal/bundle/oci"
	"xu526/internal/bundle/registry"
	"xu526/internal/bundle/registry/regtest"
)

type onBase struct{ fake }

func (o onBase) Plan(s *Source, d *Detection, opt Options) (*Plan, error) {
	p, _ := o.fake.Plan(s, d, opt)
	p.Base = baseRef
	p.Env = map[string]string{"PORT": "8080", "PATH": "/custom/bin"}
	p.Cmd = []string{"server.js"}
	return p, nil
}

var baseRef string

// publishBase puts a one-layer base image in the registry and returns its manifest digest.
func publishBase(t *testing.T, reg *regtest.Registry, tag, marker string) string {
	l, err := oci.BuildLayer([]oci.File{{Data: []byte(marker), Dst: "/nodejs/bin/node", Mode: 0o755}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	im := &oci.Image{OS: "linux", Arch: "amd64", Layers: []*oci.Layer{l}, LayerHistory: []string{"base"},
		Config: oci.Config{Entrypoint: []string{"/nodejs/bin/node"}, Env: []string{"PATH=/usr/bin", "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt"}, User: "65532", WorkingDir: "/home/nonroot"}}
	im.Seal()
	reg.PutBlob(l.Blob)
	reg.PutBlob(im.ConfigJSON())
	return reg.PutManifest("distroless/node", tag, oci.MediaManifest, im.Manifest())
}

func TestBuildOnBase(t *testing.T) {
	reg := regtest.New()
	defer reg.Close()
	reg.User, reg.Pass = "u", "p"
	d1 := publishBase(t, reg, "22", "node-v1")
	baseRef = reg.Host() + "/distroless/node:22"
	client := registry.New()
	client.Creds = func(string) (string, string, error) { return "u", "p", nil }
	dir := t.TempDir()
	store, _ := cache.Open(t.TempDir())
	build := func(update bool, c *registry.Client) (*Result, string, error) {
		var log bytes.Buffer
		res, err := Build(context.Background(), Request{Dir: dir, Tag: "app", Plugins: []Plugin{onBase{fake{"a", 1}}},
			Cache: store, Registry: c, UpdateBases: update, Log: &log})
		return res, log.String(), err
	}
	res, log, err := build(false, client)
	if err != nil {
		t.Fatalf("%v\n%s", err, log)
	}
	lock, _ := os.ReadFile(filepath.Join(dir, LockFile))
	if !strings.Contains(string(lock), d1) || !strings.Contains(log, "saved to xu.lock") {
		t.Fatalf("lock not written: %s\n%s", lock, log)
	}
	im := res.Image
	if len(im.Layers) != 2 || im.BaseLayers != 1 || res.Base != baseRef+"@"+d1 {
		t.Fatalf("layers %d base %d %s", len(im.Layers), im.BaseLayers, res.Base)
	}
	var cfg struct {
		Config struct {
			Entrypoint, Cmd, Env []string
			User, WorkingDir     string
			Labels               map[string]string
		} `json:"config"`
		History []map[string]any `json:"history"`
	}
	json.Unmarshal(im.ConfigJSON(), &cfg)
	c := cfg.Config
	if c.Entrypoint[0] != "/nodejs/bin/node" || c.Cmd[0] != "server.js" || c.User != "65532" || c.WorkingDir != "/home/nonroot" {
		t.Fatalf("config not merged: %+v", c)
	}
	if strings.Join(c.Env, " ") != "PATH=/custom/bin SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt PORT=8080" {
		t.Fatalf("env: %v", c.Env)
	}
	if c.Labels["org.opencontainers.image.base.digest"] != d1 || len(cfg.History) != 2 {
		t.Fatalf("labels %v history %v", c.Labels, cfg.History)
	}

	// The registry moves the tag; xu.lock keeps the old base, offline, from the cache.
	publishBase(t, reg, "22", "node-v2")
	offline := registry.New()
	offline.HTTP.Transport = failTransport{}
	again, log, err := build(false, offline)
	if err != nil || again.Image.Digest() != im.Digest() {
		t.Fatalf("locked offline rebuild: %v\n%s", err, log)
	}
	// --update-bases follows the tag.
	updated, log, err := build(true, client)
	if err != nil || updated.Base == res.Base || updated.Image.Digest() == im.Digest() {
		t.Fatalf("update-bases: %v %s\n%s", err, updated.Base, log)
	}

	// Push the image and read it back.
	d, err := Push(context.Background(), client, im, reg.Host()+"/team/app:v1", &bytes.Buffer{})
	if err != nil || d != im.Digest() {
		t.Fatalf("push: %s %v", d, err)
	}
	if _, _, ok := reg.GetManifest("team/app", "v1"); !ok {
		t.Fatal("pushed manifest missing")
	}
}

type failTransport struct{}

func (failTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("network disabled in test")
}

func TestMirror(t *testing.T) {
	reg := regtest.New()
	defer reg.Close()
	publishBase(t, reg, "22", "node")
	baseRef = "gcr.io/distroless/node:22" // served by the mirror only
	dir := t.TempDir()
	res, err := Build(context.Background(), Request{Dir: dir, Plugins: []Plugin{onBase{fake{"a", 1}}},
		Mirrors: map[string]string{"gcr.io": reg.Host()}, Log: &bytes.Buffer{}})
	if err != nil {
		t.Fatal(err)
	}
	lock, _ := os.ReadFile(filepath.Join(dir, LockFile))
	if !strings.Contains(string(lock), `"gcr.io/distroless/node:22"`) || !strings.HasPrefix(res.Base, "gcr.io/") {
		t.Fatalf("lock should name the original registry: %s", lock)
	}
	if m, err := ParseMirrors("gcr.io=a:5000, docker.io=b"); err != nil || m["gcr.io"] != "a:5000" || mirrorFor("registry-1.docker.io", m) != "b" {
		t.Fatalf("%v %v", m, err)
	}
	if _, err := ParseMirrors("nope"); err == nil {
		t.Fatal("bad mirror accepted")
	}
}

func TestMergeEnv(t *testing.T) {
	got := mergeEnv([]string{"PATH=/bin", "LANG=C"}, map[string]string{"LANG": "en", "B": "2", "A": "1"})
	if strings.Join(got, " ") != "PATH=/bin LANG=en A=1 B=2" {
		t.Fatal(got)
	}
}
