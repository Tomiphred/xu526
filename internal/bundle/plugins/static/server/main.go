// xu-static serves a folder of files. `xu build` puts it in images for
// static sites, so a site needs no nginx and no base image.
//
//	PORT            port to listen on (default 8080)
//	XU_ROOT         folder to serve (default /site)
//	XU_SPA=1        serve index.html for unknown paths (single-page apps)
//	XU_ACCESS_LOG=1 log every request
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"path"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

func init() {
	for ext, typ := range map[string]string{
		".ico": "image/x-icon", ".txt": "text/plain; charset=utf-8", ".md": "text/markdown; charset=utf-8",
		".csv": "text/csv; charset=utf-8", ".woff": "font/woff", ".woff2": "font/woff2", ".ttf": "font/ttf",
		".otf": "font/otf", ".map": "application/json", ".webmanifest": "application/manifest+json",
		".mp4": "video/mp4", ".webm": "video/webm", ".mp3": "audio/mpeg", ".ogg": "audio/ogg",
		".json": "application/json", ".xml": "application/xml", ".wasm": "application/wasm",
	} {
		mime.AddExtensionType(ext, typ)
	}
}

func main() {
	port := env("PORT", "8080")
	root := env("XU_ROOT", "/site")
	h := newHandler(os.DirFS(root), os.Getenv("XU_SPA") == "1")
	var handler http.Handler = h
	if os.Getenv("XU_ACCESS_LOG") == "1" {
		handler = accessLog(h)
	}
	srv := &http.Server{Addr: ":" + port, Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGTERM, os.Interrupt)
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()
	log.Printf("serving %s on :%s (spa=%v)", root, port, h.spa)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type handler struct {
	fs  fs.FS
	spa bool

	mu    sync.Mutex
	etags map[string]string // files never change inside an image
}

func newHandler(fsys fs.FS, spa bool) *handler {
	return &handler{fs: fsys, spa: spa, etags: map[string]string{}}
}

// hashedPart finds a fingerprint like app.3f9a2c1b.js or index-CpS-6B6Y.js.
var hashedPart = regexp.MustCompile(`[.-]([A-Za-z0-9_-]{8,})\.[a-z0-9]+$`)

// isHashed reports whether a file name carries a content fingerprint, which
// makes it safe to cache forever. The fingerprint must mix letters and
// digits, so names like my-long-filename.js don't qualify.
func isHashed(name string) bool {
	m := hashedPart.FindStringSubmatch(path.Base(name))
	if m == nil {
		return false
	}
	return strings.ContainsAny(m[1], "0123456789") && strings.IndexFunc(m[1], func(r rune) bool { return r >= 'A' && r <= 'z' && (r <= 'Z' || r >= 'a') }) >= 0
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	upath := path.Clean("/" + r.URL.Path)
	for _, seg := range strings.Split(upath, "/") {
		if strings.HasPrefix(seg, ".") && seg != ".well-known" {
			h.notFound(w, r)
			return
		}
	}
	name := strings.TrimPrefix(upath, "/")
	if name == "" {
		name = "."
	}
	if st, err := fs.Stat(h.fs, name); err == nil && st.IsDir() {
		if !strings.HasSuffix(r.URL.Path, "/") {
			http.Redirect(w, r, path.Base(upath)+"/", http.StatusMovedPermanently)
			return
		}
		name = path.Join(name, "index.html")
	}
	if h.serve(w, r, name, http.StatusOK) {
		return
	}
	if path.Ext(name) == "" && h.serve(w, r, name+".html", http.StatusOK) { // /about → about.html
		return
	}
	if h.spa && path.Ext(name) == "" && h.serve(w, r, "index.html", http.StatusOK) {
		return
	}
	h.notFound(w, r)
}

func (h *handler) notFound(w http.ResponseWriter, r *http.Request) {
	if !h.serve(w, r, "404.html", http.StatusNotFound) {
		http.Error(w, "404 page not found", http.StatusNotFound)
	}
}

// serve writes a regular file, preferring a precompressed .br or .gz copy.
// It reports false if the file doesn't exist.
func (h *handler) serve(w http.ResponseWriter, r *http.Request, name string, status int) bool {
	st, err := fs.Stat(h.fs, name)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	ctype := mime.TypeByExtension(path.Ext(name))
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	hdr := w.Header()
	hdr.Set("Content-Type", ctype)
	switch {
	case strings.HasPrefix(ctype, "text/html"):
		hdr.Set("Cache-Control", "no-cache")
	case isHashed(name):
		hdr.Set("Cache-Control", "public, max-age=31536000, immutable")
	default:
		hdr.Set("Cache-Control", "public, max-age=300")
	}
	file := name
	accept := r.Header.Get("Accept-Encoding")
	for _, enc := range []struct{ token, ext string }{{"br", ".br"}, {"gzip", ".gz"}} {
		if strings.Contains(accept, enc.token) {
			if s, err := fs.Stat(h.fs, name+enc.ext); err == nil && s.Mode().IsRegular() {
				file = name + enc.ext
				hdr.Set("Content-Encoding", enc.token)
				break
			}
		}
	}
	if file != name || h.hasCompressed(name) {
		hdr.Add("Vary", "Accept-Encoding")
	}
	f, err := h.fs.Open(file)
	if err != nil {
		return false
	}
	defer f.Close()
	if status != http.StatusOK {
		hdr.Del("Cache-Control")
		w.WriteHeader(status)
		if r.Method != http.MethodHead {
			io.Copy(w, f)
		}
		return true
	}
	hdr.Set("ETag", h.etag(file))
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		b, _ := io.ReadAll(f)
		rs = strings.NewReader(string(b))
	}
	http.ServeContent(w, r, name, time.Time{}, rs) // image timestamps are fixed, so no Last-Modified
	return true
}

func (h *handler) hasCompressed(name string) bool {
	for _, ext := range []string{".br", ".gz"} {
		if _, err := fs.Stat(h.fs, name+ext); err == nil {
			return true
		}
	}
	return false
}

func (h *handler) etag(name string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e, ok := h.etags[name]; ok {
		return e
	}
	f, err := h.fs.Open(name)
	if err != nil {
		return ""
	}
	defer f.Close()
	sum := sha256.New()
	io.Copy(sum, f)
	e := `"` + hex.EncodeToString(sum.Sum(nil))[:16] + `"`
	h.etags[name] = e
	return e
}

type recorder struct {
	http.ResponseWriter
	status int
	n      int64
}

func (r *recorder) WriteHeader(s int) { r.status = s; r.ResponseWriter.WriteHeader(s) }
func (r *recorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.n += int64(n)
	return n, err
}

func accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s %d %dB %s", r.Method, r.URL.Path, rec.status, rec.n, time.Since(start).Round(time.Microsecond))
	})
}
