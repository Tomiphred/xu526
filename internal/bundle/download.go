package bundle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Download is a file a plugin needs, such as a language runtime tarball.
// Its checksum comes from the plugin (pinned in code), else from xu.lock,
// else from the release's checksum file (SumsURL), and is then recorded in
// xu.lock so every later build must get the same bytes.
type Download struct {
	Name    string // key for BuildEnv.Downloads
	URL     string
	SHA256  string // hex; "" if not pinned in code
	SumsURL string // a SHA256SUMS-style file listing "<hex>  <filename>"
}

// mirrorURL applies the URL-prefix entries of XU_MIRRORS
// ("https://github.com/org/repo/releases/download=http://mirror/x").
func mirrorURL(u string, mirrors map[string]string) string {
	best := ""
	for k := range mirrors {
		if (strings.HasPrefix(k, "http://") || strings.HasPrefix(k, "https://")) && strings.HasPrefix(u, k) && len(k) > len(best) {
			best = k
		}
	}
	if best == "" {
		return u
	}
	return strings.TrimSuffix(mirrors[best], "/") + "/" + strings.TrimPrefix(u[len(best):], "/")
}

func httpGet(ctx context.Context, c *http.Client, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "xu-build")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// sumFor finds a file's checksum in a SHA256SUMS listing.
func sumFor(sums []byte, name string) string {
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name && len(f[0]) == 64 {
			return strings.ToLower(f[0])
		}
	}
	return ""
}

// fetchDownloads makes every planned download available as a local file,
// verified against its pinned checksum.
func fetchDownloads(ctx context.Context, req *Request, src *Source, plan *Plan, work string, log io.Writer) (map[string]string, error) {
	if len(plan.Downloads) == 0 {
		return nil, nil
	}
	lock, err := readLock(src.Dir)
	if err != nil {
		return nil, err
	}
	client := req.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Minute}
	}
	out := map[string]string{}
	dirty := false
	for _, d := range plan.Downloads {
		sum, from := d.SHA256, "pinned in xu"
		if sum == "" {
			sum, from = lock.Downloads[d.URL], LockFile
		}
		if sum != "" && req.Cache != nil {
			if p := req.Cache.BlobPath("sha256:" + sum); fileExists(p) {
				out[d.Name] = p
				continue
			}
		}
		u := mirrorURL(d.URL, req.Mirrors)
		fmt.Fprintf(log, "downloading %s ...\n", u)
		body, err := httpGet(ctx, client, u)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", d.Name, err)
		}
		if sum == "" {
			if d.SumsURL == "" {
				return nil, fmt.Errorf("%s has no checksum to verify against", d.URL)
			}
			sums, err := httpGet(ctx, client, mirrorURL(d.SumsURL, req.Mirrors))
			if err != nil {
				return nil, fmt.Errorf("%s checksums: %w", d.Name, err)
			}
			pu, _ := url.Parse(d.URL)
			name, _ := url.PathUnescape(path.Base(pu.Path))
			if sum = sumFor(sums, name); sum == "" {
				return nil, fmt.Errorf("%s isn't listed in %s", name, d.SumsURL)
			}
			from = "release checksums, saved to " + LockFile
			if lock.Downloads == nil {
				lock.Downloads = map[string]string{}
			}
			lock.Downloads[d.URL] = sum
			dirty = true
		}
		got := sha256.Sum256(body)
		if hex.EncodeToString(got[:]) != sum {
			return nil, fmt.Errorf("%s: checksum mismatch (expected %s from %s, got %s); refusing to use it", u, sum[:12], from, hex.EncodeToString(got[:])[:12])
		}
		fmt.Fprintf(log, "  verified sha256 %s (%s)\n", sum[:12], from)
		if req.Cache != nil {
			if err := req.Cache.PutBlob("sha256:"+sum, body); err != nil {
				return nil, err
			}
			out[d.Name] = req.Cache.BlobPath("sha256:" + sum)
		} else {
			p := filepath.Join(work, "download-"+d.Name)
			if err := os.WriteFile(p, body, 0o644); err != nil {
				return nil, err
			}
			out[d.Name] = p
		}
	}
	if dirty {
		if err := lock.write(src.Dir); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
