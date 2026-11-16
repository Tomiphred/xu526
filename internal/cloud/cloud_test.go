package cloud

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// AWS's published SigV4 test suite, case "get-vanilla".
func TestSignV4AWSVector(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://example.amazonaws.com/", nil)
	now, _ := time.Parse("20060102T150405Z", "20150830T123600Z")
	SignV4(req, sha256Hex(nil), Credentials{AccessKey: "AKIDEXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}, "us-east-1", "service", now)
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// AWS test suite case "get-vanilla-query-order-key-case".
func TestSignV4QueryOrder(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://example.amazonaws.com/?Param2=value2&Param1=value1", nil)
	now, _ := time.Parse("20060102T150405Z", "20150830T123600Z")
	SignV4(req, sha256Hex(nil), Credentials{AccessKey: "AKIDEXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}, "us-east-1", "service", now)
	if !strings.HasSuffix(req.Header.Get("Authorization"), "Signature=b97d918cfa904a5beff61c982a1b6f458b799221646efd99d3219ec94cdf2500") {
		t.Fatalf("got %s", req.Header.Get("Authorization"))
	}
}

func TestPutS3Compatible(t *testing.T) {
	var got struct{ path, auth, body, sha string }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.path, got.auth, got.body, got.sha = r.URL.Path, r.Header.Get("Authorization"), string(b), r.Header.Get("X-Amz-Content-Sha256")
	}))
	defer srv.Close()
	c := StorageConfig{Type: "s3", Endpoint: srv.URL, Bucket: "logs", Prefix: "xu", AccessKey: "AK", SecretKey: "SK"}
	if err := c.Put(context.Background(), "host/app/2026/11/11/chunk 1.log.gz", []byte("data"), "application/gzip"); err != nil {
		t.Fatal(err)
	}
	if got.path != "/logs/xu/host/app/2026/11/11/chunk 1.log.gz" || !strings.Contains(got.auth, "Credential=AK/") || got.body != "data" || got.sha != sha256Hex([]byte("data")) {
		t.Fatalf("%+v", got)
	}
}

func TestPutAzureSignsRequest(t *testing.T) {
	key := base64.StdEncoding.EncodeToString([]byte("secret-key"))
	var auth, blobType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, blobType = r.Header.Get("Authorization"), r.Header.Get("x-ms-blob-type")
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	c := StorageConfig{Type: "azure", Endpoint: srv.URL + "/acct", Account: "acct", AccountKey: key, Bucket: "logs"}
	if err := c.Put(context.Background(), "a.log.gz", []byte("x"), "application/gzip"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(auth, "SharedKey acct:") || blobType != "BlockBlob" {
		t.Fatalf("auth=%q type=%q", auth, blobType)
	}
}

func TestECRLogin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Amz-Target") == "" || !strings.Contains(r.Header.Get("Authorization"), "/ecr/aws4_request") {
			w.WriteHeader(403)
			return
		}
		tok := base64.StdEncoding.EncodeToString([]byte("AWS:pw123"))
		io.WriteString(w, `{"authorizationData":[{"authorizationToken":"`+tok+`","expiresAt":1900000000}]}`)
	}))
	defer srv.Close()
	u, p, exp, err := ecrLogin(context.Background(), srv.URL, Credentials{AccessKey: "a", SecretKey: "b"}, "eu-west-1")
	if err != nil || u != "AWS" || p != "pw123" || exp.Unix() != 1900000000 {
		t.Fatalf("%s %s %v %v", u, p, exp, err)
	}
}
