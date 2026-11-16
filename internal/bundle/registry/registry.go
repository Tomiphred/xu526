// Package registry is a small OCI distribution client: pull manifests and
// blobs (choosing the right platform from an index) and push images. It
// supports anonymous and token auth (Docker Hub, gcr, ghcr), basic auth,
// and credentials from ~/.docker/config.json including credential helpers
// (so ECR, GCR and ACR logins made with their CLIs work).
package registry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Media types the client understands.
const (
	OCIIndex       = "application/vnd.oci.image.index.v1+json"
	OCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	DockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
	DockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
)

// Ref is a parsed image reference.
type Ref struct {
	Registry string // API host, e.g. "registry-1.docker.io", "gcr.io", "localhost:5000"
	Repo     string // e.g. "library/node", "distroless/nodejs22-debian13"
	Tag      string
	Digest   string
}

// ParseRef parses references the way Docker does: "node" is
// docker.io/library/node:latest; a first part containing "." or ":" (or
// "localhost") is a registry host.
func ParseRef(s string) (Ref, error) {
	var r Ref
	if s == "" || strings.ContainsAny(s, " \t") {
		return r, fmt.Errorf("invalid image reference %q", s)
	}
	if name, d, ok := strings.Cut(s, "@"); ok {
		if !strings.HasPrefix(d, "sha256:") || len(d) != 71 {
			return r, fmt.Errorf("invalid digest in %q", s)
		}
		s, r.Digest = name, d
	}
	if i := strings.LastIndex(s, ":"); i > strings.LastIndex(s, "/") {
		s, r.Tag = s[:i], s[i+1:]
	}
	first, rest, hasSlash := strings.Cut(s, "/")
	if hasSlash && (strings.ContainsAny(first, ".:") || first == "localhost") {
		r.Registry, r.Repo = first, rest
	} else {
		r.Registry, r.Repo = "docker.io", s
	}
	if r.Registry == "docker.io" || r.Registry == "index.docker.io" {
		r.Registry = "registry-1.docker.io"
		if !strings.Contains(r.Repo, "/") {
			r.Repo = "library/" + r.Repo
		}
	}
	if r.Repo == "" || r.Repo != strings.ToLower(r.Repo) {
		return r, fmt.Errorf("invalid repository in %q (names are lowercase)", s)
	}
	if r.Tag == "" && r.Digest == "" {
		r.Tag = "latest"
	}
	return r, nil
}

// String is the canonical form, with docker.io shown as docker.io.
func (r Ref) String() string {
	host := r.Registry
	if host == "registry-1.docker.io" {
		host = "docker.io"
	}
	s := host + "/" + r.Repo
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	if r.Digest != "" {
		s += "@" + r.Digest
	}
	return s
}

func (r Ref) reference() string {
	if r.Digest != "" {
		return r.Digest
	}
	return r.Tag
}

// Credentials finds a username and password for a registry host. Empty
// strings mean anonymous.
type Credentials func(host string) (user, pass string, err error)

// Client talks to registries.
type Client struct {
	HTTP  *http.Client
	Creds Credentials
	// PlainHTTP lists hosts reached over http instead of https. localhost
	// and 127.0.0.1 are always plain.
	PlainHTTP map[string]bool
	UserAgent string

	mu     sync.Mutex
	tokens map[string]string // host + scope → bearer token
	basic  map[string]bool   // hosts that asked for basic auth
}

// New returns a client using ~/.docker/config.json for credentials.
func New() *Client {
	return &Client{HTTP: &http.Client{Timeout: 10 * time.Minute}, Creds: DockerConfig, UserAgent: "xu-build"}
}

func (c *Client) base(host string) string {
	h := strings.Split(host, ":")[0]
	if c.PlainHTTP[host] || h == "localhost" || h == "127.0.0.1" {
		return "http://" + host
	}
	return "https://" + host
}

// Error is a registry error response.
type Error struct {
	Status int
	Method string
	URL    string
	Detail string
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("%s %s: %d %s", e.Method, e.URL, e.Status, http.StatusText(e.Status))
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	switch e.Status {
	case http.StatusUnauthorized, http.StatusForbidden:
		msg += " (log in with `docker login` or your registry's CLI)"
	}
	return msg
}

func readError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var body struct {
		Errors []struct{ Code, Message string } `json:"errors"`
	}
	detail := strings.TrimSpace(string(b))
	if json.Unmarshal(b, &body) == nil && len(body.Errors) > 0 {
		var parts []string
		for _, e := range body.Errors {
			parts = append(parts, strings.TrimSpace(e.Code+" "+e.Message))
		}
		detail = strings.Join(parts, "; ")
	}
	if len(detail) > 300 {
		detail = detail[:300]
	}
	u := resp.Request.URL
	return &Error{Status: resp.StatusCode, Method: resp.Request.Method, URL: u.Scheme + "://" + u.Host + u.Path, Detail: detail}
}

// do sends a request, answering an auth challenge once if needed. body may
// be nil; it is resent on retry.
func (c *Client) do(ctx context.Context, method, rawURL, scope string, hdr http.Header, body []byte) (*http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	host := u.Host
	send := func() (*http.Response, error) {
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
		if err != nil {
			return nil, err
		}
		for k, v := range hdr {
			req.Header[k] = v
		}
		if c.UserAgent != "" {
			req.Header.Set("User-Agent", c.UserAgent)
		}
		c.mu.Lock()
		tok, basic := c.tokens[host+" "+scope], c.basic[host]
		c.mu.Unlock()
		switch {
		case tok != "":
			req.Header.Set("Authorization", "Bearer "+tok)
		case basic && c.Creds != nil:
			if user, pass, _ := c.Creds(host); user != "" {
				req.SetBasicAuth(user, pass)
			}
		}
		return c.HTTP.Do(req)
	}
	resp, err := send()
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	resp.Body.Close()
	if err := c.authorize(ctx, host, scope, challenge); err != nil {
		return nil, err
	}
	return send()
}

// authorize answers a WWW-Authenticate challenge.
func (c *Client) authorize(ctx context.Context, host, scope, challenge string) error {
	scheme, params := parseChallenge(challenge)
	var user, pass string
	if c.Creds != nil {
		var err error
		if user, pass, err = c.Creds(host); err != nil {
			return fmt.Errorf("credentials for %s: %w", host, err)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tokens == nil {
		c.tokens, c.basic = map[string]string{}, map[string]bool{}
	}
	switch scheme {
	case "basic":
		if user == "" {
			return fmt.Errorf("%s needs a login: run `docker login %s`", host, host)
		}
		c.basic[host] = true
		return nil
	case "bearer":
	default:
		return fmt.Errorf("%s: unsupported auth challenge %q", host, challenge)
	}
	realm := params["realm"]
	if realm == "" {
		return fmt.Errorf("%s: auth challenge has no realm", host)
	}
	q := url.Values{}
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	if scope != "" {
		q.Set("scope", scope)
	} else if s := params["scope"]; s != "" {
		q.Set("scope", s)
	}
	tu := realm
	if strings.Contains(tu, "?") {
		tu += "&" + q.Encode()
	} else {
		tu += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tu, nil)
	if err != nil {
		return err
	}
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return readError(resp)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return fmt.Errorf("token from %s: %w", realm, err)
	}
	if t.Token == "" {
		t.Token = t.AccessToken
	}
	if t.Token == "" {
		return fmt.Errorf("token server %s returned no token", realm)
	}
	c.tokens[host+" "+scope] = t.Token
	return nil
}

// parseChallenge reads `Bearer realm="…",service="…",scope="…"`.
func parseChallenge(h string) (string, map[string]string) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(h), " ")
	params := map[string]string{}
	for rest != "" {
		rest = strings.TrimLeft(rest, ", ")
		k, v, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		k = strings.ToLower(strings.TrimSpace(k))
		if strings.HasPrefix(v, `"`) {
			end := strings.Index(v[1:], `"`)
			if end < 0 {
				params[k], rest = v[1:], ""
				break
			}
			params[k], rest = v[1:end+1], v[end+2:]
		} else {
			val, r, _ := strings.Cut(v, ",")
			params[k], rest = val, r
		}
	}
	return strings.ToLower(scheme), params
}

func pullScope(r Ref) string { return "repository:" + r.Repo + ":pull" }
func pushScope(r Ref) string { return "repository:" + r.Repo + ":pull,push" }

// Digest is the sha256 digest of b.
func Digest(b []byte) string {
	s := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(s[:])
}

// Manifest fetches a manifest or index as stored. If the reference has a
// digest, the content is checked against it.
func (c *Client) Manifest(ctx context.Context, r Ref) (body []byte, mediaType, digest string, err error) {
	hdr := http.Header{"Accept": {OCIIndex, OCIManifest, DockerList, DockerManifest}}
	u := fmt.Sprintf("%s/v2/%s/manifests/%s", c.base(r.Registry), r.Repo, r.reference())
	resp, err := c.do(ctx, http.MethodGet, u, pullScope(r), hdr, nil)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", "", readError(resp)
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, "", "", err
	}
	digest = Digest(body)
	if r.Digest != "" && digest != r.Digest {
		return nil, "", "", fmt.Errorf("%s: manifest content does not match its digest", r)
	}
	mediaType = strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	if mediaType == "" || mediaType == "application/json" {
		var probe struct {
			MediaType string `json:"mediaType"`
		}
		json.Unmarshal(body, &probe)
		mediaType = probe.MediaType
	}
	return body, mediaType, digest, nil
}

// Descriptor points at a blob or manifest.
type Descriptor struct {
	MediaType string            `json:"mediaType"`
	Digest    string            `json:"digest"`
	Size      int64             `json:"size"`
	Platform  *Platform         `json:"platform,omitempty"`
	Annots    map[string]string `json:"annotations,omitempty"`
}

// Platform is an index entry's platform.
type Platform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant,omitempty"`
}

// ImageManifest is a single-platform manifest.
type ImageManifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType,omitempty"`
	Config        Descriptor   `json:"config"`
	Layers        []Descriptor `json:"layers"`
}

// Resolve fetches the manifest for one platform, following an index if
// the reference points at one. It returns the platform manifest's digest,
// which is what a pinned base reference should name.
func (c *Client) Resolve(ctx context.Context, r Ref, os, arch string) (*ImageManifest, []byte, string, error) {
	body, mt, digest, err := c.Manifest(ctx, r)
	if err != nil {
		return nil, nil, "", err
	}
	if mt == OCIIndex || mt == DockerList {
		var idx struct {
			Manifests []Descriptor `json:"manifests"`
		}
		if err := json.Unmarshal(body, &idx); err != nil {
			return nil, nil, "", fmt.Errorf("%s: bad index: %w", r, err)
		}
		var have []string
		var pick *Descriptor
		for i, m := range idx.Manifests {
			if m.Platform == nil {
				continue
			}
			have = append(have, m.Platform.OS+"/"+m.Platform.Architecture)
			if m.Platform.OS == os && m.Platform.Architecture == arch && (arch != "arm64" || m.Platform.Variant == "" || m.Platform.Variant == "v8") {
				pick = &idx.Manifests[i]
				break
			}
		}
		if pick == nil {
			return nil, nil, "", fmt.Errorf("%s has no %s/%s image (it has: %s)", r, os, arch, strings.Join(have, ", "))
		}
		pr := r
		pr.Tag, pr.Digest = "", pick.Digest
		body, mt, digest, err = c.Manifest(ctx, pr)
		if err != nil {
			return nil, nil, "", err
		}
	}
	if mt != OCIManifest && mt != DockerManifest {
		return nil, nil, "", fmt.Errorf("%s: unsupported manifest type %q", r, mt)
	}
	var m ImageManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, nil, "", fmt.Errorf("%s: bad manifest: %w", r, err)
	}
	return &m, body, digest, nil
}

// Blob downloads a blob and checks it against its digest.
func (c *Client) Blob(ctx context.Context, r Ref, digest string) ([]byte, error) {
	u := fmt.Sprintf("%s/v2/%s/blobs/%s", c.base(r.Registry), r.Repo, digest)
	resp, err := c.do(ctx, http.MethodGet, u, pullScope(r), nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, readError(resp)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if Digest(b) != digest {
		return nil, fmt.Errorf("blob %s from %s is corrupt (digest mismatch)", digest, r.Registry)
	}
	return b, nil
}

// HasBlob reports whether the registry already has a blob.
func (c *Client) HasBlob(ctx context.Context, r Ref, digest string) (bool, error) {
	u := fmt.Sprintf("%s/v2/%s/blobs/%s", c.base(r.Registry), r.Repo, digest)
	resp, err := c.do(ctx, http.MethodHead, u, pushScope(r), nil, nil)
	if err != nil {
		return false, err
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	}
	return false, readError(resp)
}

// PutBlob uploads a blob in one request (POST to start, PUT with digest).
func (c *Client) PutBlob(ctx context.Context, r Ref, digest string, b []byte) error {
	u := fmt.Sprintf("%s/v2/%s/blobs/uploads/", c.base(r.Registry), r.Repo)
	resp, err := c.do(ctx, http.MethodPost, u, pushScope(r), nil, []byte{})
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return readError(resp)
	}
	loc, err := resp.Request.URL.Parse(resp.Header.Get("Location"))
	if err != nil || resp.Header.Get("Location") == "" {
		return fmt.Errorf("%s: upload has no location", r.Registry)
	}
	q := loc.Query()
	q.Set("digest", digest)
	loc.RawQuery = q.Encode()
	hdr := http.Header{"Content-Type": {"application/octet-stream"}}
	resp, err = c.do(ctx, http.MethodPut, loc.String(), pushScope(r), hdr, b)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return readError(resp)
	}
	return nil
}

// PutManifest uploads a manifest under the reference's tag (or digest).
func (c *Client) PutManifest(ctx context.Context, r Ref, mediaType string, body []byte) (string, error) {
	u := fmt.Sprintf("%s/v2/%s/manifests/%s", c.base(r.Registry), r.Repo, r.reference())
	resp, err := c.do(ctx, http.MethodPut, u, pushScope(r), http.Header{"Content-Type": {mediaType}}, body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", readError(resp)
	}
	return Digest(body), nil
}

// Blob is content to push.
type Blob struct {
	Digest string
	Data   []byte
}

// PushImage uploads blobs the registry lacks, then the manifest. progress
// (optional) is told about each blob.
func (c *Client) PushImage(ctx context.Context, r Ref, blobs []Blob, manifestType string, manifest []byte, progress func(digest string, size int, existed bool)) (string, error) {
	for _, b := range blobs {
		ok, err := c.HasBlob(ctx, r, b.Digest)
		if err != nil {
			return "", err
		}
		if !ok {
			if err := c.PutBlob(ctx, r, b.Digest, b.Data); err != nil {
				return "", err
			}
		}
		if progress != nil {
			progress(b.Digest, len(b.Data), ok)
		}
	}
	return c.PutManifest(ctx, r, manifestType, manifest)
}
