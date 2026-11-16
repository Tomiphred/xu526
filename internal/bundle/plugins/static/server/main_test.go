package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

var site = fstest.MapFS{
	"index.html":                {Data: []byte("<h1>home</h1>")},
	"about.html":                {Data: []byte("about")},
	"404.html":                  {Data: []byte("custom 404")},
	"docs/index.html":           {Data: []byte("docs")},
	"assets/app.3f9a2c1b.js":    {Data: []byte("js")},
	"assets/app.3f9a2c1b.js.gz": {Data: []byte("gzipped")},
	"style.css":                 {Data: []byte("body{}")},
	"font.woff2":                {Data: []byte("f")},
	".env":                      {Data: []byte("SECRET=1")},
	".well-known/security.txt":  {Data: []byte("contact")},
}

func get(h http.Handler, p string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", p, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestServe(t *testing.T) {
	h := newHandler(site, false)
	cases := []struct {
		path, body string
		status     int
		ctype      string
		cache      string
	}{
		{"/", "<h1>home</h1>", 200, "text/html", "no-cache"},
		{"/about", "about", 200, "text/html", "no-cache"},
		{"/docs/", "docs", 200, "text/html", ""},
		{"/style.css", "body{}", 200, "text/css", "max-age=300"},
		{"/assets/app.3f9a2c1b.js", "js", 200, "javascript", "immutable"},
		{"/font.woff2", "f", 200, "font/woff2", ""},
		{"/missing", "custom 404", 404, "text/html", ""},
		{"/.env", "custom 404", 404, "", ""},
		{"/x/../.env", "custom 404", 404, "", ""},
		{"/.well-known/security.txt", "contact", 200, "text/plain", ""},
	}
	for _, c := range cases {
		w := get(h, c.path)
		if w.Code != c.status || w.Body.String() != c.body {
			t.Errorf("%s: %d %q, want %d %q", c.path, w.Code, w.Body.String(), c.status, c.body)
		}
		if !strings.Contains(w.Header().Get("Content-Type"), c.ctype) || !strings.Contains(w.Header().Get("Cache-Control"), c.cache) {
			t.Errorf("%s: type %q cache %q", c.path, w.Header().Get("Content-Type"), w.Header().Get("Cache-Control"))
		}
	}
	if w := get(h, "/docs"); w.Code != 301 || w.Header().Get("Location") != "/docs/" {
		t.Errorf("/docs should redirect to docs/: %d %s", w.Code, w.Header().Get("Location"))
	}
}

func TestPrecompressedAndETag(t *testing.T) {
	h := newHandler(site, false)
	w := get(h, "/assets/app.3f9a2c1b.js", "Accept-Encoding", "gzip, br")
	if w.Body.String() != "gzipped" || w.Header().Get("Content-Encoding") != "gzip" || !strings.Contains(w.Header().Get("Content-Type"), "javascript") {
		t.Fatalf("precompressed: %q %v", w.Body.String(), w.Header())
	}
	plain := get(h, "/assets/app.3f9a2c1b.js")
	if plain.Body.String() != "js" || plain.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("plain: %q %v", plain.Body.String(), plain.Header())
	}
	tag := get(h, "/style.css").Header().Get("ETag")
	if tag == "" {
		t.Fatal("no ETag")
	}
	if w := get(h, "/style.css", "If-None-Match", tag); w.Code != 304 {
		t.Fatalf("If-None-Match gave %d", w.Code)
	}
}

func TestSPAAndMethods(t *testing.T) {
	h := newHandler(site, true)
	if w := get(h, "/app/users/42"); w.Code != 200 || w.Body.String() != "<h1>home</h1>" {
		t.Fatalf("spa fallback: %d %q", w.Code, w.Body.String())
	}
	if w := get(h, "/missing.png"); w.Code != 404 {
		t.Fatalf("missing asset should 404 even in spa mode: %d", w.Code)
	}
	r := httptest.NewRequest("POST", "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 405 {
		t.Fatalf("POST gave %d", w.Code)
	}
	bare := newHandler(fstestEmpty(), false)
	if w := get(bare, "/nope"); w.Code != 404 || !strings.Contains(w.Body.String(), "404") {
		t.Fatalf("plain 404: %d %q", w.Code, w.Body.String())
	}
}

func fstestEmpty() fstest.MapFS { return fstest.MapFS{"index.html": {Data: []byte("x")}} }

func TestIsHashed(t *testing.T) {
	for name, want := range map[string]bool{
		"assets/index-CpS-6B6Y.js": true, "static/js/main.3f9a2c1b.js": true, "index-DMZIdM9-.css": true,
		"chunk.4e5b6c7d8e9f.js": true, "my-long-filename.js": false, "jquery-3.7.1.min.js": false,
		"style.css": false, "logo.png": false, "abcdefgh.js": false,
	} {
		if got := isHashed(name); got != want {
			t.Errorf("isHashed(%q) = %v, want %v", name, got, want)
		}
	}
}
