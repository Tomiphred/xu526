package cloud

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// StorageConfig describes where log chunks go. Type is one of:
//
//	s3     AWS S3 or any S3-compatible store (R2, MinIO, Spaces, Wasabi)
//	gcs    Google Cloud Storage through its S3-compatible XML API (HMAC keys)
//	azure  Azure Blob Storage (storage account key)
//	local  a folder on this server (or a mounted network share)
type StorageConfig struct {
	Type       string `json:"type"`
	Bucket     string `json:"bucket,omitempty"` // bucket (s3, gcs) or container (azure)
	Prefix     string `json:"prefix,omitempty"`
	Region     string `json:"region,omitempty"`
	Endpoint   string `json:"endpoint,omitempty"` // custom endpoint, e.g. https://<id>.r2.cloudflarestorage.com
	AccessKey  string `json:"access_key,omitempty"`
	SecretKey  string `json:"secret_key,omitempty"`
	Account    string `json:"account,omitempty"`     // azure storage account
	AccountKey string `json:"account_key,omitempty"` // azure key (base64)
	Path       string `json:"path,omitempty"`        // local folder
}

// Redacted returns a copy safe to show in the API.
func (c StorageConfig) Redacted() StorageConfig {
	if c.SecretKey != "" {
		c.SecretKey = "••••" + tail(c.SecretKey, 4)
	}
	if c.AccountKey != "" {
		c.AccountKey = "••••" + tail(c.AccountKey, 4)
	}
	return c
}

func tail(s string, n int) string {
	if len(s) <= n {
		return ""
	}
	return s[len(s)-n:]
}

// Describe is a human summary like "s3://bucket/prefix".
func (c StorageConfig) Describe() string {
	switch c.Type {
	case "local":
		return c.Path
	case "azure":
		return "azure://" + c.Account + "/" + c.Bucket + "/" + c.Prefix
	case "":
		return "not configured"
	}
	return c.Type + "://" + c.Bucket + "/" + c.Prefix
}

// Validate checks required fields.
func (c StorageConfig) Validate() error {
	switch c.Type {
	case "s3", "gcs":
		if c.Bucket == "" || c.AccessKey == "" || c.SecretKey == "" {
			return errors.New("bucket, access key and secret key are required")
		}
	case "azure":
		if c.Account == "" || c.AccountKey == "" || c.Bucket == "" {
			return errors.New("storage account, account key and container are required")
		}
		if _, err := base64.StdEncoding.DecodeString(c.AccountKey); err != nil {
			return errors.New("the account key should be the base64 key from the Azure portal")
		}
	case "local":
		if !filepath.IsAbs(c.Path) {
			return errors.New("the folder must be an absolute path")
		}
	default:
		return fmt.Errorf("unknown storage type %q (use s3, gcs, azure or local)", c.Type)
	}
	return nil
}

// Put uploads data as key (the prefix is added here).
func (c StorageConfig) Put(ctx context.Context, key string, data []byte, contentType string) error {
	if c.Prefix != "" {
		key = strings.Trim(c.Prefix, "/") + "/" + key
	}
	switch c.Type {
	case "s3", "gcs":
		return c.putS3(ctx, key, data, contentType)
	case "azure":
		return c.putAzure(ctx, key, data, contentType)
	case "local":
		dst := filepath.Join(c.Path, filepath.FromSlash(key))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		tmp := dst + ".part"
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			return err
		}
		return os.Rename(tmp, dst)
	}
	return fmt.Errorf("storage is not configured")
}

var client = &http.Client{Timeout: 2 * time.Minute}

func (c StorageConfig) putS3(ctx context.Context, key string, data []byte, contentType string) error {
	region, endpoint := c.Region, strings.TrimRight(c.Endpoint, "/")
	var u string
	switch {
	case c.Type == "gcs":
		if endpoint == "" {
			endpoint = "https://storage.googleapis.com"
		}
		if region == "" {
			region = "auto"
		}
		u = endpoint + "/" + c.Bucket + "/" + uriEncode(key, true)
	case endpoint != "": // S3-compatible: path-style
		if region == "" {
			region = "auto"
		}
		u = endpoint + "/" + c.Bucket + "/" + uriEncode(key, true)
	default: // AWS: virtual-hosted style
		if region == "" {
			region = "us-east-1"
		}
		u = "https://" + c.Bucket + ".s3." + region + ".amazonaws.com/" + uriEncode(key, true)
	}
	req, err := http.NewRequestWithContext(ctx, "PUT", u, bytes.NewReader(data))
	if err != nil {
		return err
	}
	hash := sha256Hex(data)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Amz-Content-Sha256", hash)
	req.ContentLength = int64(len(data))
	SignV4(req, hash, Credentials{AccessKey: c.AccessKey, SecretKey: c.SecretKey}, region, "s3", time.Now())
	return do(req)
}

func (c StorageConfig) putAzure(ctx context.Context, key string, data []byte, contentType string) error {
	base := strings.TrimRight(c.Endpoint, "/")
	if base == "" {
		base = "https://" + c.Account + ".blob.core.windows.net"
	}
	u := base + "/" + c.Bucket + "/" + uriEncode(key, true)
	req, err := http.NewRequestWithContext(ctx, "PUT", u, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(data))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("x-ms-date", time.Now().UTC().Format(http.TimeFormat))
	req.Header.Set("x-ms-version", "2021-08-06")
	req.Header.Set("x-ms-blob-type", "BlockBlob")
	if err := signAzure(req, c.Account, c.AccountKey); err != nil {
		return err
	}
	err = do(req)
	var se *StorageError
	if errors.As(err, &se) && se.Code == "ContainerNotFound" {
		// First upload: create the (private) container, then retry once.
		creq, _ := http.NewRequestWithContext(ctx, "PUT", base+"/"+c.Bucket+"?restype=container", nil)
		creq.Header.Set("x-ms-date", time.Now().UTC().Format(http.TimeFormat))
		creq.Header.Set("x-ms-version", "2021-08-06")
		if err := signAzure(creq, c.Account, c.AccountKey); err != nil {
			return err
		}
		if cerr := do(creq); cerr != nil {
			var ce *StorageError
			if !errors.As(cerr, &ce) || ce.Code != "ContainerAlreadyExists" {
				return fmt.Errorf("create container %s: %w", c.Bucket, cerr)
			}
		}
		req2, _ := http.NewRequestWithContext(ctx, "PUT", u, bytes.NewReader(data))
		req2.ContentLength = int64(len(data))
		req2.Header.Set("Content-Type", contentType)
		req2.Header.Set("x-ms-date", time.Now().UTC().Format(http.TimeFormat))
		req2.Header.Set("x-ms-version", "2021-08-06")
		req2.Header.Set("x-ms-blob-type", "BlockBlob")
		if err := signAzure(req2, c.Account, c.AccountKey); err != nil {
			return err
		}
		return do(req2)
	}
	return err
}

// signAzure implements the Blob service SharedKey scheme.
func signAzure(req *http.Request, account, key string) error {
	k, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return err
	}
	length := ""
	if req.ContentLength > 0 {
		length = strconv.FormatInt(req.ContentLength, 10)
	}
	h := req.Header
	var ms []string
	for name := range h {
		if l := strings.ToLower(name); strings.HasPrefix(l, "x-ms-") {
			ms = append(ms, l)
		}
	}
	sort.Strings(ms)
	var canonHeaders strings.Builder
	for _, l := range ms {
		canonHeaders.WriteString(l + ":" + strings.TrimSpace(h.Get(l)) + "\n")
	}
	resource := "/" + account + req.URL.EscapedPath()
	q := req.URL.Query()
	qk := make([]string, 0, len(q))
	for k := range q {
		qk = append(qk, k)
	}
	sort.Strings(qk)
	for _, k := range qk {
		resource += "\n" + strings.ToLower(k) + ":" + strings.Join(q[k], ",")
	}
	toSign := strings.Join([]string{
		req.Method, h.Get("Content-Encoding"), h.Get("Content-Language"), length, h.Get("Content-MD5"),
		h.Get("Content-Type"), "", h.Get("If-Modified-Since"), h.Get("If-Match"), h.Get("If-None-Match"),
		h.Get("If-Unmodified-Since"), h.Get("Range"),
	}, "\n") + "\n" + canonHeaders.String() + resource
	m := hmac.New(sha256.New, k)
	m.Write([]byte(toSign))
	req.Header.Set("Authorization", "SharedKey "+account+":"+base64.StdEncoding.EncodeToString(m.Sum(nil)))
	return nil
}

// StorageError is a readable error from S3, GCS or Azure.
type StorageError struct {
	Status  int
	Code    string
	Message string
}

func (e *StorageError) Error() string {
	hint := map[string]string{
		"NoSuchBucket":          "the bucket doesn't exist; create it first",
		"InvalidAccessKeyId":    "the access key is wrong",
		"SignatureDoesNotMatch": "the secret key is wrong",
		"AccessDenied":          "these keys aren't allowed to write to this bucket",
		"AuthenticationFailed":  "the account key is wrong",
		"AuthorizationFailure":  "this key isn't allowed to write here",
	}[e.Code]
	if hint != "" {
		return hint + " (" + e.Code + ")"
	}
	if e.Code != "" {
		return e.Code + ": " + e.Message
	}
	return fmt.Sprintf("storage answered %d %s", e.Status, e.Message)
}

var (
	xmlCode = regexp.MustCompile(`<Code>([^<]+)</Code>`)
	xmlMsg  = regexp.MustCompile(`<Message>([^<\n]+)`)
)

func do(req *http.Request) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		se := &StorageError{Status: resp.StatusCode, Code: resp.Header.Get("x-ms-error-code")}
		if m := xmlCode.FindSubmatch(b); m != nil && se.Code == "" {
			se.Code = string(m[1])
		}
		if m := xmlMsg.FindSubmatch(b); m != nil {
			se.Message = strings.TrimSpace(string(m[1]))
		} else {
			se.Message = strings.TrimSpace(string(b))
		}
		return se
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// ECRLogin returns Docker credentials for an AWS ECR registry. Tokens last 12 hours.
func ECRLogin(ctx context.Context, creds Credentials, region string) (user, pass string, expires time.Time, err error) {
	return ecrLogin(ctx, "https://api.ecr."+region+".amazonaws.com/", creds, region)
}

func ecrLogin(ctx context.Context, endpoint string, creds Credentials, region string) (string, string, time.Time, error) {
	body := []byte("{}")
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return "", "", time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "AmazonEC2ContainerRegistry_V20150921.GetAuthorizationToken")
	SignV4(req, sha256Hex(body), creds, region, "ecr", time.Now())
	resp, err := client.Do(req)
	if err != nil {
		return "", "", time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", "", time.Time{}, fmt.Errorf("ECR login: %s %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var out struct {
		AuthorizationData []struct {
			AuthorizationToken string  `json:"authorizationToken"`
			ExpiresAt          float64 `json:"expiresAt"`
		} `json:"authorizationData"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || len(out.AuthorizationData) == 0 {
		return "", "", time.Time{}, errors.New("ECR login: unexpected response")
	}
	raw, err := base64.StdEncoding.DecodeString(out.AuthorizationData[0].AuthorizationToken)
	if err != nil {
		return "", "", time.Time{}, err
	}
	user, pass, _ := strings.Cut(string(raw), ":")
	return user, pass, time.Unix(int64(out.AuthorizationData[0].ExpiresAt), 0), nil
}

// ObjectKey builds the storage path for a log chunk.
func ObjectKey(host, app string, t time.Time, file string) string {
	t = t.UTC()
	return fmt.Sprintf("%s/%s/%04d/%02d/%02d/%s", url.PathEscape(host), app, t.Year(), t.Month(), t.Day(), file)
}
