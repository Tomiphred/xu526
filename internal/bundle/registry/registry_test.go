package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"xu526/internal/bundle/registry/regtest"
)

func TestParseRef(t *testing.T) {
	d := "sha256:" + strings.Repeat("a", 64)
	cases := map[string]string{
		"node":    "docker.io/library/node:latest",
		"node:22": "docker.io/library/node:22",
		"me/app":  "docker.io/me/app:latest",
		"gcr.io/distroless/nodejs22-debian13:nonroot": "gcr.io/distroless/nodejs22-debian13:nonroot",
		"localhost:5000/app":                          "localhost:5000/app:latest",
		"ghcr.io/o/r@" + d:                            "ghcr.io/o/r@" + d,
		"ghcr.io/o/r:v1@" + d:                         "ghcr.io/o/r:v1@" + d,
		"123.dkr.ecr.us-east-1.amazonaws.com/api:7":   "123.dkr.ecr.us-east-1.amazonaws.com/api:7",
	}
	for in, want := range cases {
		r, err := ParseRef(in)
		if err != nil || r.String() != want {
			t.Errorf("%s -> %s %v, want %s", in, r, err, want)
		}
	}
	if r, _ := ParseRef("node"); r.Registry != "registry-1.docker.io" || r.Repo != "library/node" {
		t.Errorf("docker hub api host: %+v", r)
	}
	for _, bad := range []string{"", "Up/Per", "a@sha256:short", "a b"} {
		if _, err := ParseRef(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseChallenge(t *testing.T) {
	s, p := parseChallenge(`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/node:pull"`)
	if s != "bearer" || p["realm"] != "https://auth.docker.io/token" || p["service"] != "registry.docker.io" || p["scope"] != "repository:library/node:pull" {
		t.Fatalf("%s %v", s, p)
	}
	if s, p := parseChallenge(`Basic realm="Registry"`); s != "basic" || p["realm"] != "Registry" {
		t.Fatalf("%s %v", s, p)
	}
}

func TestDockerConfig(t *testing.T) {
	auth := base64.StdEncoding.EncodeToString([]byte("bob:s3cret"))
	cfg := []byte(`{"auths":{"https://index.docker.io/v1/":{"auth":"` + auth + `"},"ghcr.io":{"auth":"` + auth + `"}},
		"credHelpers":{"123.dkr.ecr.us-east-1.amazonaws.com":"ecr-login"}}`)
	helper := func(h, server string) (string, string, error) { return "AWS", "from-" + h + "-" + server, nil }
	for host, want := range map[string]string{
		"registry-1.docker.io": "bob:s3cret", "ghcr.io": "bob:s3cret",
		"123.dkr.ecr.us-east-1.amazonaws.com": "AWS:from-ecr-login-123.dkr.ecr.us-east-1.amazonaws.com", "quay.io": ":",
	} {
		u, p, err := fromConfig(cfg, host, helper)
		if err != nil || u+":"+p != want {
			t.Errorf("%s -> %s:%s %v, want %s", host, u, p, err, want)
		}
	}
}

func setup(t *testing.T, reg *regtest.Registry) (Ref, string) {
	layer := []byte("layer-bytes")
	cfg := []byte(`{"architecture":"arm64","os":"linux","config":{"Env":["PATH=/bin"]}}`)
	ld, cd := reg.PutBlob(layer), reg.PutBlob(cfg)
	m, _ := json.Marshal(ImageManifest{SchemaVersion: 2, MediaType: OCIManifest,
		Config: Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: cd, Size: int64(len(cfg))},
		Layers: []Descriptor{{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Digest: ld, Size: int64(len(layer))}}})
	md := reg.PutManifest("base/node", "arm-only", OCIManifest, m)
	idx, _ := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": OCIIndex, "manifests": []Descriptor{
		{MediaType: OCIManifest, Digest: "sha256:" + strings.Repeat("0", 64), Size: 1, Platform: &Platform{OS: "linux", Architecture: "amd64"}},
		{MediaType: OCIManifest, Digest: md, Size: int64(len(m)), Platform: &Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}},
	}})
	reg.PutManifest("base/node", "22", OCIIndex, idx)
	r, _ := ParseRef(reg.Host() + "/base/node:22")
	return r, md
}

func TestPullThroughIndexWithTokenAuth(t *testing.T) {
	reg := regtest.New()
	defer reg.Close()
	reg.User, reg.Pass = "u", "p"
	ref, md := setup(t, reg)
	c := New()
	c.Creds = func(string) (string, string, error) { return "u", "p", nil }
	ctx := context.Background()
	m, _, d, err := c.Resolve(ctx, ref, "linux", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if d != md || len(m.Layers) != 1 {
		t.Fatalf("resolved %s, want %s", d, md)
	}
	b, err := c.Blob(ctx, ref, m.Layers[0].Digest)
	if err != nil || string(b) != "layer-bytes" {
		t.Fatalf("blob %q %v", b, err)
	}
	if reg.Requests["GET token"] != 1 {
		t.Errorf("token fetched %d times, want once (cached)", reg.Requests["GET token"])
	}
	if _, _, _, err := c.Resolve(ctx, ref, "linux", "s390x"); err == nil || !strings.Contains(err.Error(), "linux/arm64") {
		t.Errorf("missing platform error should list what exists: %v", err)
	}
	// Pinning by digest still works and is verified.
	pinned := ref
	pinned.Tag, pinned.Digest = "", md
	if _, _, d2, err := c.Resolve(ctx, pinned, "linux", "arm64"); err != nil || d2 != md {
		t.Fatalf("pinned resolve: %s %v", d2, err)
	}
	bad := New()
	bad.Creds = func(string) (string, string, error) { return "u", "wrong", nil }
	if _, _, _, err := bad.Resolve(ctx, ref, "linux", "arm64"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("wrong password: %v", err)
	}
}

func TestPush(t *testing.T) {
	reg := regtest.New()
	defer reg.Close()
	reg.User, reg.Pass = "u", "p"
	c := New()
	c.Creds = func(string) (string, string, error) { return "u", "p", nil }
	cfg, layer := []byte(`{"os":"linux"}`), []byte("data")
	m, _ := json.Marshal(ImageManifest{SchemaVersion: 2, MediaType: OCIManifest,
		Config: Descriptor{Digest: Digest(cfg), Size: int64(len(cfg))}, Layers: []Descriptor{{Digest: Digest(layer), Size: 4}}})
	ref, _ := ParseRef(reg.Host() + "/team/app:v1")
	blobs := []Blob{{Digest(cfg), cfg}, {Digest(layer), layer}}
	var uploaded int
	d, err := c.PushImage(context.Background(), ref, blobs, OCIManifest, m, func(_ string, _ int, existed bool) {
		if !existed {
			uploaded++
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if body, mt, ok := reg.GetManifest("team/app", "v1"); !ok || mt != OCIManifest || Digest(body) != d || uploaded != 2 {
		t.Fatalf("manifest %v %s uploaded %d", ok, mt, uploaded)
	}
	// A second push uploads nothing.
	uploaded = 0
	before := reg.Requests["PUT upload"]
	if _, err := c.PushImage(context.Background(), ref, blobs, OCIManifest, m, func(_ string, _ int, existed bool) {
		if !existed {
			uploaded++
		}
	}); err != nil || uploaded != 0 || reg.Requests["PUT upload"] != before {
		t.Fatalf("re-push uploaded %d: %v", uploaded, err)
	}
	// A manifest referring to a missing blob is rejected with the registry's message.
	_, err = c.PutManifest(context.Background(), ref, OCIManifest, []byte(`{"config":{"digest":"sha256:`+strings.Repeat("1", 64)+`"}}`))
	if err == nil || !strings.Contains(err.Error(), "MANIFEST_BLOB_UNKNOWN") {
		t.Fatalf("expected registry error, got %v", err)
	}
}
