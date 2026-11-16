package platform

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Signatures checked against botocore's SigV4 signer for the same request.
func TestS3Signature(t *testing.T) {
	c := &s3Client{endpoint: "http://127.0.0.1:9000", access: "AKID", secr: "SECRET"}
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ method, path, sig string }{
		{"GET", "/", "e55957e7f2dfba35ac80a20f69cd7d8850f8517f6e15f73c67d814711d022b05"},
		{"PUT", "/app", "b54e112a7e591abc8d80fdb5346200f229b7c8e452b8b40b368020313b0f165d"},
	} {
		req, _ := http.NewRequest(tc.method, c.endpoint+tc.path, nil)
		c.sign(req, now)
		if got := req.Header.Get("Authorization"); !strings.HasSuffix(got, "Signature="+tc.sig) {
			t.Errorf("%s %s: %s", tc.method, tc.path, got)
		}
	}
}

// A listing with a query and an upload with a key that needs escaping,
// checked against botocore (the upload: its canonical request, signed by
// hand with UNSIGNED-PAYLOAD, which botocore only uses over HTTPS).
func TestS3SignatureQueryAndKey(t *testing.T) {
	c := &s3Client{endpoint: "http://127.0.0.1:9000", access: "AKID", secr: "SECRET"}
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	q := url.Values{"list-type": {"2"}, "prefix": {"my docs/"}, "delimiter": {"/"}, "max-keys": {"500"}, "continuation-token": {"a+b="}}
	req, _ := c.newRequest(context.Background(), "GET", "app", "", q, nil, 0)
	c.sign(req, now)
	if got := req.Header.Get("Authorization"); !strings.HasSuffix(got, "Signature=6b9a04b44bc609da902999bae1ec959f5ef33904645640c90c0700fddc6c231c") {
		t.Errorf("list: %s (%s)", got, req.URL)
	}
	req, _ = c.newRequest(context.Background(), "PUT", "app", "my docs/résumé (1).pdf", nil, strings.NewReader("x"), 1)
	c.sign(req, now)
	if got := req.Header.Get("Authorization"); !strings.HasSuffix(got, "Signature=72024a34d166455a7059a41511b00adda76d1c9e812fdda5370fe3aa1438fd3b") {
		t.Errorf("put: %s (%s)", got, req.URL.EscapedPath())
	}
	if req.URL.EscapedPath() != "/app/my%20docs/r%C3%A9sum%C3%A9%20%281%29.pdf" {
		t.Errorf("path sent: %s", req.URL.EscapedPath())
	}
}

func TestStorageConnection(t *testing.T) {
	p := envPlatform(t)
	spec := AppSpec{Name: "files", Display: "files", Engine: "seaweedfs", Env: map[string]string{"S3_ACCESS_KEY": "xuKEY", "S3_SECRET_KEY": "sec"}}
	c, ok := p.connectionOf(spec)
	if !ok || c.URL != "http://files:8333" || c.Env["S3_BUCKET"] != "app" || c.Env["AWS_SECRET_ACCESS_KEY"] != "sec" ||
		c.Env["S3_PUBLIC_URL"] != "http://files.example.com" || c.Env["FILES_URL"] != "http://files:8333" {
		t.Errorf("connection %+v", c)
	}
	if m := c.masked(); m.Env["AWS_SECRET_ACCESS_KEY"] != maskedValue || m.Env["S3_ACCESS_KEY_ID"] != "xuKEY" {
		t.Errorf("masked %+v", m.Env)
	}
	if kindOf(AppSpec{Image: "pgsty/silo:latest"}) != "storage" || kindOf(spec) != "api" && kindOf(AppSpec{Image: "rustfs/rustfs"}) != "storage" {
		t.Error("storage kind")
	}
}
