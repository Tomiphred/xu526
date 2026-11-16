package platform

import (
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"xu526/internal/cloud"
)

func body(t *testing.T, srv *httptest.Server, host, path string) (string, int) {
	req, _ := http.NewRequest("GET", srv.URL+path, nil)
	req.Host = host
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func TestReleasesAndRollback(t *testing.T) {
	p, srv := newTestPlatform(t)
	ctx := context.Background()
	for _, g := range []string{"first", "second"} {
		if err := p.Deploy(ctx, AppSpec{Name: "hello", Command: []string{helloBin}, Env: map[string]string{"GREETING": g}}); err != nil {
			t.Fatal(err)
		}
	}
	if b, _ := body(t, srv, "hello.test", "/"); !strings.Contains(b, "second") {
		t.Fatalf("v2 not live: %q", b)
	}
	rels := p.Releases("app", "hello")
	if len(rels) != 2 || rels[0].Version != 2 || rels[0].Status != "live" || rels[1].Status != "superseded" {
		t.Fatalf("releases: %+v", rels)
	}
	rel, err := p.Rollback(ctx, "app", "hello", 1)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != 3 || rel.RollbackOf != 1 || rel.Source != "rollback" {
		t.Fatalf("rollback release: %+v", rel)
	}
	if b, _ := body(t, srv, "hello.test", "/"); !strings.Contains(b, "first") {
		t.Fatalf("rollback didn't restore v1: %q", b)
	}
}

func TestPublicToggleAndCustomDomains(t *testing.T) {
	p, srv := newTestPlatform(t)
	if err := p.Deploy(context.Background(), AppSpec{Name: "site", Command: []string{helloBin}, MinScale: 1}); err != nil {
		t.Fatal(err)
	}
	if err := p.SetRoute("site", nil, []string{"WWW.Example.com.", "https://shop.example.org"}); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"site.test", "www.example.com", "shop.example.org:8080"} {
		if _, code := body(t, srv, host, "/"); code != 200 {
			t.Fatalf("%s: status %d", host, code)
		}
	}
	off := false
	if err := p.SetRoute("site", &off, []string{"www.example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, code := body(t, srv, "www.example.com", "/"); code != 404 {
		t.Fatalf("private app still served: %d", code)
	}
	if err := p.Deploy(context.Background(), AppSpec{Name: "other", Command: []string{helloBin}}); err != nil {
		t.Fatal(err)
	}
	if err := p.SetRoute("other", nil, []string{"www.example.com"}); err == nil {
		t.Fatal("a domain attached to two apps should be refused")
	}
	if err := p.SetRoute("other", nil, []string{"x.test"}); err == nil {
		t.Fatal("a domain under the platform domain should be refused")
	}
}

func TestLogRotationCompressionAndShipping(t *testing.T) {
	dir, dst := t.TempDir(), t.TempDir()
	s := NewLogStore(dir, 2048, time.Hour, "server1")
	s.SetStorage(cloud.StorageConfig{Type: "local", Path: dst, Prefix: "xu"})
	for i := 0; i < 200; i++ {
		s.Write("api", "abc123", "stdout", "request handled "+strings.Repeat("x", 20))
	}
	s.Write("api", "abc123", "stderr", "the last line ERROR here")
	s.Flush()
	// rotated files compress in the background; wait until none are left raw
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		raw, _ := filepath.Glob(filepath.Join(dir, "api", "2*.log"))
		if len(raw) == 0 && len(s.Chunks("api")) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	chunks := s.Chunks("api")
	if len(chunks) < 2 {
		t.Fatalf("expected rotated chunks, got %d", len(chunks))
	}
	if err := s.ShipNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	var shipped []string
	filepath.Walk(dst, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			shipped = append(shipped, p)
		}
		return nil
	})
	if len(shipped) != len(chunks) || !strings.Contains(shipped[0], filepath.Join("xu", "server1", "api")) {
		t.Fatalf("shipped %v", shipped)
	}
	f, _ := os.Open(shipped[0])
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	b, _ := io.ReadAll(zr)
	if !strings.Contains(string(b), "abc123 stdout request handled") {
		t.Fatalf("chunk content: %.100q", b)
	}
	if st := s.Status(); st.Pending != 0 || st.Shipped != len(chunks) {
		t.Fatalf("status %+v", st)
	}
	if got := s.Tail("api", 5, "error"); len(got) != 1 || !strings.Contains(got[0], "ERROR") {
		t.Fatalf("search: %v", got)
	}
}

func TestWebhookSignature(t *testing.T) {
	m := hmac.New(sha256.New, []byte("s3cret"))
	m.Write([]byte(`{"ref":"x"}`))
	sig := "sha256=" + hex.EncodeToString(m.Sum(nil))
	if !validSignature("s3cret", []byte(`{"ref":"x"}`), sig) {
		t.Fatal("valid signature rejected")
	}
	if validSignature("other", []byte(`{"ref":"x"}`), sig) || validSignature("s3cret", []byte(`{"ref":"y"}`), sig) || validSignature("s3cret", nil, "") {
		t.Fatal("bad signature accepted")
	}
}

func TestWebhookEndpointRejectsUnsigned(t *testing.T) {
	p, srv := newTestPlatform(t)
	p.mu.Lock()
	p.sources["abc"] = &Source{ID: "abc", Kind: "app", Target: "x", Branch: "main", Secret: "s"}
	p.mu.Unlock()
	req, _ := http.NewRequest("POST", srv.URL+"/hooks/github/abc", strings.NewReader(`{}`))
	req.Host = "xu.test"
	req.Header.Set("X-GitHub-Event", "push")
	resp, _ := srv.Client().Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unsigned webhook: %d", resp.StatusCode)
	}
	// ping with a valid signature
	m := hmac.New(sha256.New, []byte("s"))
	m.Write([]byte(`{}`))
	req, _ = http.NewRequest("POST", srv.URL+"/hooks/github/abc", strings.NewReader(`{}`))
	req.Host = "xu.test"
	req.Header.Set("X-GitHub-Event", "ping")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(m.Sum(nil)))
	resp, _ = srv.Client().Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("signed ping: %d", resp.StatusCode)
	}
}

func TestMetricsCountRequests(t *testing.T) {
	p, srv := newTestPlatform(t)
	if err := p.Deploy(context.Background(), AppSpec{Name: "m", Command: []string{helloBin}, MinScale: 1}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		body(t, srv, "m.test", "/?work=5")
	}
	p.sample(p.get("m"), time.Now())
	s, err := p.Metrics("m", "15m")
	if err != nil || s.Requests != 20 || len(s.Points) != 1 || s.Points[0].P50 < 5 || s.Points[0].Instances != 1 {
		t.Fatalf("%+v %v", s, err)
	}
	var sb strings.Builder
	p.WritePrometheus(&sb)
	if !strings.Contains(sb.String(), `xu_requests_total{app="m",code="2xx"} 20`) {
		t.Fatalf("prometheus output:\n%s", sb.String())
	}
	if lines := p.logs.Tail("m", 50, "GET /?work=5 200"); len(lines) != 20 {
		t.Fatalf("access log lines: %d", len(lines))
	}
}
