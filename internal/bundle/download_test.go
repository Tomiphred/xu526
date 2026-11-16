package bundle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xu526/internal/bundle/cache"
)

type withDownload struct {
	fake
	url, sums string
	got       *string
}

func (w withDownload) Plan(s *Source, d *Detection, opt Options) (*Plan, error) {
	p, _ := w.fake.Plan(s, d, opt)
	p.Downloads = []Download{{Name: "rt", URL: w.url, SumsURL: w.sums}}
	return p, nil
}

func (w withDownload) Build(_ context.Context, env *BuildEnv, _ *Plan) (*Output, error) {
	b, _ := os.ReadFile(env.Downloads["rt"])
	*w.got = string(b)
	return &Output{}, nil
}

func TestDownloads(t *testing.T) {
	content := []byte("python runtime bytes")
	sum := sha256.Sum256(content)
	served := content
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		switch r.URL.Path {
		case "/rel/1/cpython-3.12.1+1-x86_64.tar.gz":
			w.Write(served)
		case "/rel/1/SHA256SUMS":
			w.Write([]byte(hex.EncodeToString(sum[:]) + "  cpython-3.12.1+1-x86_64.tar.gz\n" + strings.Repeat("0", 64) + "  other.tar.gz\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	// The plan names the canonical URL; XU_MIRRORS sends it to the test server.
	canonical := "https://github.com/astral-sh/pbs/releases/download"
	mirrors := map[string]string{canonical: srv.URL + "/rel"}
	var got string
	pl := withDownload{fake{"a", 1}, canonical + "/1/cpython-3.12.1%2B1-x86_64.tar.gz", canonical + "/1/SHA256SUMS", &got}
	dir := t.TempDir()
	store, _ := cache.Open(t.TempDir())
	run := func(noCache bool) error {
		_, err := Build(context.Background(), Request{Dir: dir, Plugins: []Plugin{pl}, Cache: store, Mirrors: mirrors, NoCache: noCache, Log: &bytes.Buffer{}})
		return err
	}
	if err := run(false); err != nil {
		t.Fatal(err)
	}
	lock, _ := os.ReadFile(filepath.Join(dir, LockFile))
	if got != string(content) || !strings.Contains(string(lock), hex.EncodeToString(sum[:])) || !strings.Contains(string(lock), canonical) {
		t.Fatalf("got %q lock %s", got, lock)
	}
	// Rebuilding (layers not cached) uses the cached download: no requests.
	hits = 0
	if err := run(true); err != nil || hits != 0 {
		t.Fatalf("rebuild made %d requests: %v", hits, err)
	}
	// A changed file at the same URL is refused once the checksum is locked.
	os.RemoveAll(store.Dir)
	store, _ = cache.Open(store.Dir)
	served = []byte("tampered")
	if err := run(true); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("tampered download accepted: %v", err)
	}
	if mirrorURL("https://x/y/z", map[string]string{"https://x": "http://m/", "https://x/y": "http://n"}) != "http://n/z" {
		t.Fatal("longest mirror prefix should win")
	}
	if sumFor([]byte("ABC  f\n"+strings.Repeat("a", 64)+" *f.tgz\n"), "f.tgz") != strings.Repeat("a", 64) {
		t.Fatal("sumFor")
	}
}
