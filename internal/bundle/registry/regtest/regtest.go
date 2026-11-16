// Package regtest is an in-memory OCI registry for tests: enough of the
// distribution API for pulls and monolithic pushes, with optional token
// auth that behaves like Docker Hub's.
package regtest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// Registry is a running test registry.
type Registry struct {
	*httptest.Server
	User, Pass string // if set, token auth is required

	mu        sync.Mutex
	blobs     map[string][]byte
	manifests map[string]manifest // repo + "@" + tag or digest
	uploads   map[string]string   // upload id → repo
	n         int
	// Requests counts requests by "METHOD kind" (kind: blob, manifest, upload, token).
	Requests map[string]int
}

type manifest struct {
	mediaType string
	body      []byte
}

// New starts a registry on a random local port. Close it when done.
func New() *Registry {
	r := NewHandler()
	r.Server = httptest.NewServer(r)
	return r
}

// NewHandler returns a registry without starting a server, to serve on a
// fixed address with http.ListenAndServe. Token auth needs New.
func NewHandler() *Registry {
	return &Registry{blobs: map[string][]byte{}, manifests: map[string]manifest{}, uploads: map[string]string{}, Requests: map[string]int{}}
}

func (r *Registry) ServeHTTP(w http.ResponseWriter, req *http.Request) { r.serve(w, req) }

// Host is host:port, usable in image references.
func (r *Registry) Host() string { return strings.TrimPrefix(r.URL, "http://") }

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// PutManifest stores a manifest directly (to set up pulls).
func (r *Registry) PutManifest(repo, ref, mediaType string, body []byte) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := digest(body)
	r.manifests[repo+"@"+ref] = manifest{mediaType, body}
	r.manifests[repo+"@"+d] = manifest{mediaType, body}
	return d
}

// PutBlob stores a blob directly.
func (r *Registry) PutBlob(b []byte) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := digest(b)
	r.blobs[d] = b
	return d
}

// GetManifest returns a stored manifest.
func (r *Registry) GetManifest(repo, ref string) ([]byte, string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.manifests[repo+"@"+ref]
	return m.body, m.mediaType, ok
}

// HasBlob reports whether a blob was stored.
func (r *Registry) HasBlob(d string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.blobs[d]
	return ok
}

const token = "test-token"

func (r *Registry) count(k string) {
	r.mu.Lock()
	r.Requests[k]++
	r.mu.Unlock()
}

func (r *Registry) serve(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/token" {
		r.count("GET token")
		if u, p, ok := req.BasicAuth(); r.User != "" && (!ok || u != r.User || p != r.Pass) {
			http.Error(w, `{"details":"incorrect username or password"}`, http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"token": token})
		return
	}
	if r.User != "" && req.Header.Get("Authorization") != "Bearer "+token {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="regtest"`, r.URL))
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}
	p := strings.TrimPrefix(req.URL.Path, "/v2/")
	if req.URL.Path == "/v2/" {
		return
	}
	var repo, kind, rest string
	for _, k := range []string{"/blobs/uploads/", "/blobs/", "/manifests/"} {
		if i := strings.LastIndex(p, k); i >= 0 {
			repo, kind, rest = p[:i], strings.Trim(k, "/"), p[i+len(k):]
			break
		}
	}
	if repo == "" {
		writeErr(w, http.StatusNotFound, "NAME_UNKNOWN", "bad path")
		return
	}
	short := strings.Split(kind, "/")[len(strings.Split(kind, "/"))-1]
	r.count(req.Method + " " + map[string]string{"uploads": "upload", "blobs": "blob", "manifests": "manifest"}[short])
	switch kind {
	case "blobs":
		r.mu.Lock()
		b, ok := r.blobs[rest]
		r.mu.Unlock()
		if !ok {
			writeErr(w, http.StatusNotFound, "BLOB_UNKNOWN", rest)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(b)))
		w.Header().Set("Docker-Content-Digest", rest)
		if req.Method == http.MethodGet {
			w.Write(b)
		}
	case "blobs/uploads":
		switch req.Method {
		case http.MethodPost:
			r.mu.Lock()
			r.n++
			id := fmt.Sprint(r.n)
			r.uploads[id] = repo
			r.mu.Unlock()
			w.Header().Set("Location", "/v2/"+repo+"/blobs/uploads/"+id+"?state=abc")
			w.WriteHeader(http.StatusAccepted)
		case http.MethodPut:
			b, _ := io.ReadAll(req.Body)
			want := req.URL.Query().Get("digest")
			if req.URL.Query().Get("state") != "abc" {
				writeErr(w, http.StatusBadRequest, "BLOB_UPLOAD_INVALID", "lost the upload state")
				return
			}
			if digest(b) != want {
				writeErr(w, http.StatusBadRequest, "DIGEST_INVALID", "digest mismatch")
				return
			}
			r.PutBlob(b)
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	case "manifests":
		switch req.Method {
		case http.MethodGet, http.MethodHead:
			r.mu.Lock()
			m, ok := r.manifests[repo+"@"+rest]
			r.mu.Unlock()
			if !ok {
				writeErr(w, http.StatusNotFound, "MANIFEST_UNKNOWN", rest)
				return
			}
			w.Header().Set("Content-Type", m.mediaType)
			w.Header().Set("Docker-Content-Digest", digest(m.body))
			if req.Method == http.MethodGet {
				w.Write(m.body)
			}
		case http.MethodPut:
			b, _ := io.ReadAll(req.Body)
			var m struct {
				Config struct{ Digest string }
				Layers []struct{ Digest string }
			}
			json.Unmarshal(b, &m)
			for _, d := range append([]string{m.Config.Digest}, func() (ds []string) {
				for _, l := range m.Layers {
					ds = append(ds, l.Digest)
				}
				return
			}()...) {
				if d != "" && !r.HasBlob(d) {
					writeErr(w, http.StatusBadRequest, "MANIFEST_BLOB_UNKNOWN", d)
					return
				}
			}
			d := r.PutManifest(repo, rest, req.Header.Get("Content-Type"), b)
			w.Header().Set("Docker-Content-Digest", d)
			w.WriteHeader(http.StatusCreated)
		}
	}
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": code, "message": msg}}})
}
