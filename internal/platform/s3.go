package platform

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A small S3 client for xu526's own object stores: list and create
// buckets. Requests are signed with AWS Signature Version 4, which RustFS,
// SeaweedFS and Silo all check.

const s3Region = "us-east-1"

type s3Client struct {
	endpoint     string // http://127.0.0.1:PORT
	access, secr string
	http         *http.Client
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key []byte, s string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return m.Sum(nil)
}

// sign adds SigV4 headers to a path-style request. The body is empty,
// unless x-amz-content-sha256 is already set (UNSIGNED-PAYLOAD for uploads).
func (c *s3Client) sign(req *http.Request, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	day := amzDate[:8]
	payload := req.Header.Get("x-amz-content-sha256")
	if payload == "" {
		payload = sha256Hex(nil)
	}
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payload)
	host := req.URL.Host
	signed := "host;x-amz-content-sha256;x-amz-date"
	canonHeaders := "host:" + host + "\nx-amz-content-sha256:" + payload + "\nx-amz-date:" + amzDate + "\n"
	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	canonical := strings.Join([]string{req.Method, path, canonicalQuery(req.URL.Query()), canonHeaders, signed, payload}, "\n")
	scope := day + "/" + s3Region + "/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha256Hex([]byte(canonical))
	k := hmacSHA256([]byte("AWS4"+c.secr), day)
	k = hmacSHA256(k, s3Region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", c.access, scope, signed, sig))
}

// awsEscape encodes s the way SigV4 wants: everything but unreserved
// characters (and / in object keys) as %XX.
func awsEscape(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' || c == '/' && keepSlash {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// canonicalQuery is the query string sorted and encoded for SigV4.
func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, awsEscape(k, false)+"="+awsEscape(v, false))
		}
	}
	return strings.Join(parts, "&")
}

// request makes a signed request for /bucket/key?query. The response body
// is the caller's to close.
func (c *s3Client) request(ctx context.Context, method, bucket, key string, q url.Values, body io.Reader, size int64) (*http.Response, error) {
	req, err := c.newRequest(ctx, method, bucket, key, q, body, size)
	if err != nil {
		return nil, err
	}
	c.sign(req, time.Now())
	return c.http.Do(req)
}

// newRequest is an unsigned request for /bucket/key?query.
func (c *s3Client) newRequest(ctx context.Context, method, bucket, key string, q url.Values, body io.Reader, size int64) (*http.Request, error) {
	p := "/"
	if bucket != "" {
		p += awsEscape(bucket, false)
		if key != "" {
			p += "/" + awsEscape(key, true)
		}
	}
	u := c.endpoint + p
	if len(q) > 0 {
		u += "?" + canonicalQuery(q)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
		req.Header.Set("x-amz-content-sha256", "UNSIGNED-PAYLOAD")
	}
	return req, nil
}

func (c *s3Client) do(ctx context.Context, method, path string) (*http.Response, []byte, error) {
	bucket, key, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	resp, err := c.request(ctx, method, bucket, key, nil, nil, 0)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp, body, nil
}

func s3Error(resp *http.Response, body []byte) error {
	var e struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if xml.Unmarshal(body, &e) == nil && e.Code != "" {
		return fmt.Errorf("%s: %s", e.Code, e.Message)
	}
	return fmt.Errorf("the object store answered %s", resp.Status)
}

// Bucket is a bucket in an object store.
type Bucket struct {
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
}

func (c *s3Client) listBuckets(ctx context.Context) ([]Bucket, error) {
	resp, body, err := c.do(ctx, "GET", "/")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, s3Error(resp, body)
	}
	var out struct {
		Buckets []struct {
			Name         string `xml:"Name"`
			CreationDate string `xml:"CreationDate"`
		} `xml:"Buckets>Bucket"`
	}
	if err := xml.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	list := []Bucket{}
	for _, b := range out.Buckets {
		t, _ := time.Parse(time.RFC3339, b.CreationDate)
		list = append(list, Bucket{Name: b.Name, Created: t})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list, nil
}

func (c *s3Client) createBucket(ctx context.Context, name string) error {
	resp, body, err := c.do(ctx, "PUT", "/"+name)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	if e := s3Error(resp, body); strings.Contains(e.Error(), "BucketAlreadyOwnedByYou") || strings.Contains(e.Error(), "BucketAlreadyExists") {
		return nil
	} else {
		return e
	}
}

// Object is a file in a bucket.
type Object struct {
	Key      string    `json:"key"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// Listing is one folder of a bucket.
type Listing struct {
	Prefix  string   `json:"prefix"`
	Folders []string `json:"folders"` // full prefixes, ending in /
	Objects []Object `json:"objects"`
	Next    string   `json:"next,omitempty"` // continuation token: there's more
}

func (c *s3Client) list(ctx context.Context, bucket, prefix, token, delimiter string, max int) (Listing, error) {
	q := url.Values{"list-type": {"2"}, "prefix": {prefix}, "max-keys": {strconv.Itoa(max)}}
	if delimiter != "" {
		q.Set("delimiter", delimiter)
	}
	if token != "" {
		q.Set("continuation-token", token)
	}
	resp, err := c.request(ctx, "GET", bucket, "", q, nil, 0)
	if err != nil {
		return Listing{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode != http.StatusOK {
		return Listing{}, s3Error(resp, body)
	}
	var out struct {
		Contents []struct {
			Key          string `xml:"Key"`
			Size         int64  `xml:"Size"`
			LastModified string `xml:"LastModified"`
		} `xml:"Contents"`
		Prefixes  []string `xml:"CommonPrefixes>Prefix"`
		Truncated bool     `xml:"IsTruncated"`
		NextToken string   `xml:"NextContinuationToken"`
	}
	if err := xml.Unmarshal(body, &out); err != nil {
		return Listing{}, err
	}
	l := Listing{Prefix: prefix, Folders: out.Prefixes, Objects: []Object{}}
	if l.Folders == nil {
		l.Folders = []string{}
	}
	for _, o := range out.Contents {
		if strings.HasSuffix(o.Key, "/") { // a folder marker
			if o.Key != prefix && delimiter != "" && !slices.Contains(l.Folders, o.Key) {
				l.Folders = append(l.Folders, o.Key)
			}
			if delimiter != "" || o.Key == prefix {
				continue
			}
		}
		t, _ := time.Parse(time.RFC3339, o.LastModified)
		l.Objects = append(l.Objects, Object{Key: o.Key, Size: o.Size, Modified: t})
	}
	if out.Truncated {
		l.Next = out.NextToken
	}
	return l, nil
}

func (c *s3Client) simple(ctx context.Context, method, bucket, key string, ok ...int) error {
	resp, err := c.request(ctx, method, bucket, key, nil, nil, 0)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	for _, s := range ok {
		if resp.StatusCode == s {
			return nil
		}
	}
	return s3Error(resp, body)
}

// deleteAll deletes a key, or everything under a folder (prefix ending in /).
func (c *s3Client) deleteAll(ctx context.Context, bucket, key string) (int, error) {
	if !strings.HasSuffix(key, "/") {
		return 1, c.simple(ctx, "DELETE", bucket, key, http.StatusNoContent, http.StatusOK)
	}
	n, token := 0, ""
	for {
		l, err := c.list(ctx, bucket, key, token, "", 1000)
		if err != nil {
			return n, err
		}
		for _, o := range l.Objects {
			if err := c.simple(ctx, "DELETE", bucket, o.Key, http.StatusNoContent, http.StatusOK); err != nil {
				return n, err
			}
			n++
		}
		if l.Next == "" {
			break
		}
		token = l.Next
	}
	// The folder's own marker, if it has one.
	_ = c.simple(ctx, "DELETE", bucket, key, http.StatusNoContent, http.StatusOK, http.StatusNotFound)
	return n, nil
}

var bucketRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// s3For returns a client for one of xu526's object stores, through its
// running instance.
func (p *Platform) s3For(name string) (*s3Client, error) {
	a := p.get(name)
	if a == nil {
		return nil, fmt.Errorf("no app %s", name)
	}
	c, ok := p.connectionOf(a.spec)
	if !ok || engines[a.spec.Engine].Kind != "storage" {
		return nil, fmt.Errorf("%s isn't an object store made by xu526", name)
	}
	a.mu.Lock()
	addr := ""
	for _, in := range a.instances {
		addr = in.Addr
		break
	}
	a.mu.Unlock()
	if addr == "" {
		return nil, errors.New("it isn't running yet")
	}
	return &s3Client{endpoint: "http://" + addr, access: c.User, secr: c.Password,
		http: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}}, nil
}

// CreateBucket makes a bucket, retrying while the store finishes starting.
func (p *Platform) CreateBucket(ctx context.Context, store, bucket string) error {
	if !bucketRE.MatchString(bucket) {
		return fmt.Errorf("bucket name %q: use 3-63 lowercase letters, digits, dots and dashes", bucket)
	}
	var last error
	for i := 0; i < 20; i++ {
		c, err := p.s3For(store)
		if err == nil {
			if err = c.createBucket(ctx, bucket); err == nil {
				return nil
			}
		}
		last = err
		select {
		case <-ctx.Done():
			return last
		case <-time.After(time.Second):
		}
	}
	return last
}

// storageAPI serves buckets.
//
//	GET  /v1/apps/{name}/buckets
//	POST /v1/apps/{name}/buckets {name}
func (p *Platform) storageAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/apps/{name}/buckets", func(w http.ResponseWriter, r *http.Request) {
		c, err := p.s3For(r.PathValue("name"))
		if err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		list, err := c.listBuckets(r.Context())
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, list)
	})
	mux.HandleFunc("POST /v1/apps/{name}/buckets", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Name string }
		if err := decodeJSON(w, r, &req); err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		if err := p.CreateBucket(ctx, r.PathValue("name"), strings.TrimSpace(req.Name)); err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("DELETE /v1/apps/{name}/buckets/{bucket}", func(w http.ResponseWriter, r *http.Request) {
		c, ok := p.s3Of(w, r)
		if !ok {
			return
		}
		if err := c.simple(r.Context(), "DELETE", r.PathValue("bucket"), "", http.StatusNoContent, http.StatusOK); err != nil {
			if strings.Contains(err.Error(), "BucketNotEmpty") {
				err = errors.New("the bucket isn't empty: delete its files first")
			}
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// The file browser.
	mux.HandleFunc("GET /v1/apps/{name}/objects", func(w http.ResponseWriter, r *http.Request) {
		c, ok := p.s3Of(w, r)
		if !ok {
			return
		}
		q := r.URL.Query()
		l, err := c.list(r.Context(), q.Get("bucket"), q.Get("prefix"), q.Get("token"), "/", 500)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, l)
	})
	mux.HandleFunc("GET /v1/apps/{name}/objects/download", func(w http.ResponseWriter, r *http.Request) {
		c, ok := p.s3Of(w, r)
		if !ok {
			return
		}
		q := r.URL.Query()
		resp, err := c.request(r.Context(), "GET", q.Get("bucket"), q.Get("key"), nil, nil, 0)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			writeErr(w, http.StatusBadGateway, s3Error(resp, body).Error())
			return
		}
		for _, h := range []string{"Content-Type", "Content-Length", "Last-Modified", "ETag"} {
			if v := resp.Header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
		base := q.Get("key")
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[i+1:]
		}
		disp := "attachment"
		if q.Get("inline") == "1" {
			disp = "inline"
			// Shown on the dashboard's own address: don't let a stored page run scripts there.
			w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self' data:; media-src 'self'; style-src 'unsafe-inline'")
			w.Header().Set("X-Content-Type-Options", "nosniff")
		}
		w.Header().Set("Content-Disposition", disp+"; filename*=UTF-8''"+awsEscape(base, false))
		_, _ = io.Copy(w, resp.Body)
	})
	mux.HandleFunc("PUT /v1/apps/{name}/objects", func(w http.ResponseWriter, r *http.Request) {
		c, ok := p.s3Of(w, r)
		if !ok {
			return
		}
		q := r.URL.Query()
		key := strings.TrimPrefix(q.Get("key"), "/")
		if key == "" || strings.Contains("/"+key+"/", "/../") || strings.Contains("/"+key+"/", "/./") || strings.Contains(key, "//") {
			writeErr(w, http.StatusBadRequest, "give the file a name (no empty, . or .. parts)")
			return
		}
		var body io.Reader = http.NoBody
		size := r.ContentLength
		if size < 0 {
			writeErr(w, http.StatusLengthRequired, "the upload's size is needed")
			return
		}
		if size > 0 {
			body = r.Body
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Hour)
		defer cancel()
		c.http.Timeout = 0 // big files: the context limits it
		resp, err := c.request(ctx, "PUT", q.Get("bucket"), key, nil, body, size)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		defer resp.Body.Close()
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode != http.StatusOK {
			writeErr(w, http.StatusBadGateway, s3Error(resp, rb).Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"key": key, "size": size})
	})
	mux.HandleFunc("DELETE /v1/apps/{name}/objects", func(w http.ResponseWriter, r *http.Request) {
		c, ok := p.s3Of(w, r)
		if !ok {
			return
		}
		q := r.URL.Query()
		if q.Get("key") == "" {
			writeErr(w, http.StatusBadRequest, "which file? (key)")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
		defer cancel()
		n, err := c.deleteAll(ctx, q.Get("bucket"), q.Get("key"))
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]int{"deleted": n})
	})
}

// s3Of is the store's client for an API request, with its bucket checked.
func (p *Platform) s3Of(w http.ResponseWriter, r *http.Request) (*s3Client, bool) {
	c, err := p.s3For(r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return nil, false
	}
	b := r.PathValue("bucket")
	if b == "" {
		b = r.URL.Query().Get("bucket")
	}
	if !bucketRE.MatchString(b) {
		writeErr(w, http.StatusBadRequest, "which bucket? (3-63 lowercase letters, digits, dots and dashes)")
		return nil, false
	}
	return c, true
}
