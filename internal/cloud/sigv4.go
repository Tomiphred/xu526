// Package cloud talks to object storage and registries with only the standard
// library: AWS Signature V4 (S3, ECR, and S3-compatible stores such as Google
// Cloud Storage, Cloudflare R2, MinIO, DigitalOcean Spaces) and Azure Blob
// Storage SharedKey.
package cloud

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Credentials are an access key pair.
type Credentials struct {
	AccessKey    string
	SecretKey    string
	SessionToken string
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// uriEncode encodes per AWS rules (RFC 3986 unreserved characters kept).
func uriEncode(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && keepSlash:
			b.WriteByte(c)
		default:
			b.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}

// SignV4 signs req in place. payloadHash is the hex SHA-256 of the body.
func SignV4(req *http.Request, payloadHash string, creds Credentials, region, service string, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	date := amzDate[:8]
	req.Header.Set("X-Amz-Date", amzDate)
	if creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", creds.SessionToken)
	}
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}

	// Canonical headers: host, content-type and every x-amz-* header.
	hdrs := map[string]string{"host": host}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-") || lk == "content-type" {
			hdrs[lk] = strings.TrimSpace(strings.Join(v, ","))
		}
	}
	names := make([]string, 0, len(hdrs))
	for k := range hdrs {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + hdrs[k] + "\n")
	}
	signed := strings.Join(names, ";")

	// Canonical query string.
	q := req.URL.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var qs []string
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			qs = append(qs, uriEncode(k, false)+"="+uriEncode(v, false))
		}
	}
	path := req.URL.EscapedPath()
	if p, err := url.PathUnescape(path); err == nil {
		path = uriEncode(p, true)
	}
	if path == "" {
		path = "/"
	}

	canonical := strings.Join([]string{req.Method, path, strings.Join(qs, "&"), canonHeaders.String(), signed, payloadHash}, "\n")
	scope := date + "/" + region + "/" + service + "/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha256Hex([]byte(canonical))

	k := hmacSHA256([]byte("AWS4"+creds.SecretKey), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))

	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+creds.AccessKey+"/"+scope+
		", SignedHeaders="+signed+", Signature="+sig)
}
