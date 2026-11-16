package platform

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"xu526/internal/runtime"
)

// Object stores' web interfaces. RustFS and Silo serve their own console
// on a second port, SeaweedFS its filer page; xu526 routes each to
// <store>-console.<domain>. Garage's interface is a separate container
// (garage-webui), deployed next to it as the app <store>-console.
//
// Consoles without a sign-in of their own (SeaweedFS's filer page, Garage's
// web UI) are guarded by xu526: the browser asks for the store's access key
// and secret key.

const (
	garageAdminPort = 3903
	garageUIEngine  = "garage-webui"
)

// consolePortOf is the container port of an app's console page (0 = none).
func consolePortOf(spec AppSpec) int { return engines[spec.Engine].console }

// engineStart adds what an engine needs at every start: settings, its
// console and admin ports, and config files.
func (p *Platform) engineStart(rs *runtime.Spec, spec AppSpec) {
	if spec.Engine == garageUIEngine {
		rs.Files = garageUIFiles(spec)
		return
	}
	e, ok := engines[spec.Engine]
	if !ok {
		return
	}
	if len(e.runEnv) > 0 {
		env := copyMap(rs.Env)
		if env == nil {
			env = map[string]string{}
		}
		for k, v := range e.runEnv {
			env[k] = v
		}
		rs.Env = env
	}
	// The engine's command is xu526's to choose, so stores made by older
	// versions get the console too.
	if len(e.args) > 0 {
		rs.Args = e.args
	}
	if e.console > 0 {
		rs.Ports = append(rs.Ports, e.console)
	}
	rs.Ports = append(rs.Ports, e.ports...)
	if e.files != nil {
		rs.Files = e.files(spec)
	}
}

// engineSetup runs an engine's first-start work once the instance answers.
func (p *Platform) engineSetup(ctx context.Context, spec AppSpec, in *runtime.Instance) error {
	if e, ok := engines[spec.Engine]; ok && e.setup != nil {
		return e.setup(ctx, p, spec, in)
	}
	return nil
}

// storeLogin asks for an object store's access key and secret key (HTTP
// basic authentication). It reports whether the request may go on.
func (p *Platform) storeLogin(w http.ResponseWriter, r *http.Request, store *app) bool {
	c, ok := p.connectionOf(store.spec)
	if !ok || c.User == "" || c.Password == "" {
		http.Error(w, "this console can't be opened: its object store has no keys", http.StatusForbidden)
		return false
	}
	u, pw, given := r.BasicAuth()
	if given && subtle.ConstantTimeCompare([]byte(u), []byte(c.User)) == 1 &&
		subtle.ConstantTimeCompare([]byte(pw), []byte(c.Password)) == 1 {
		r.Header.Del("Authorization") // the console behind doesn't need it
		return true
	}
	w.Header().Set("WWW-Authenticate", `Basic realm="`+displayName(store.spec)+` console: access key and secret key", charset="UTF-8"`)
	http.Error(w, "Sign in with the object store's access key (user name) and secret key (password). They're on its page in the xu526 dashboard.", http.StatusUnauthorized)
	return false
}

// ---- Garage --------------------------------------------------------------------------

// garageKeys makes keys in Garage's format, which it checks on import:
// GK and 24 hex digits, and 64 hex digits.
func garageKeys() (string, string) { return "GK" + randomHex(12), randomHex(32) }

// garageConfig is garage.toml for one node. Secrets aren't in it: Garage
// reads them from GARAGE_RPC_SECRET and GARAGE_ADMIN_TOKEN.
const garageConfig = `metadata_dir = "/var/lib/garage/meta"
data_dir = "/var/lib/garage/data"
db_engine = "sqlite"
replication_factor = 1
rpc_bind_addr = "0.0.0.0:3901"
rpc_public_addr = "127.0.0.1:3901"

[s3_api]
s3_region = "us-east-1"
api_bind_addr = "0.0.0.0:3900"

[admin]
api_bind_addr = "0.0.0.0:3903"
`

func garageFiles(AppSpec) []runtime.File {
	return []runtime.File{{Path: "/etc/garage.toml", Data: []byte(garageConfig)}}
}

// garageConsole is the web UI deployed next to a Garage store.
func garageConsole(store AppSpec) AppSpec {
	return AppSpec{
		Name: store.Name + "-console", Display: displayName(store) + "-console", Image: "khairul169/garage-webui:latest", Port: 3909,
		Kind: "console", Engine: garageUIEngine, LoginOf: store.Name, Environment: store.Environment, MinScale: 1, MaxScale: 1,
		Env: map[string]string{
			"API_BASE_URL":    fmt.Sprintf("http://%s:%d", store.Name, garageAdminPort),
			"API_ADMIN_KEY":   store.Env["GARAGE_ADMIN_TOKEN"],
			"S3_ENDPOINT_URL": fmt.Sprintf("http://%s:3900", store.Name),
			"S3_REGION":       s3Region,
		},
		SecretKeys: []string{"API_ADMIN_KEY"},
	}
}

// garageUIFiles gives the web UI the store's config, which it reads for
// the admin API's address and token (the variables above win).
func garageUIFiles(spec AppSpec) []runtime.File {
	cfg := garageConfig + fmt.Sprintf("admin_token = %q\n", spec.Env["API_ADMIN_KEY"])
	return []runtime.File{{Path: "/etc/garage.toml", Data: []byte(cfg)}}
}

// garageSetup makes a fresh Garage usable: it gives the node storage (a
// one-node layout), imports the generated keys, and lets them create
// buckets. Each step is skipped when it's already done.
func garageSetup(ctx context.Context, p *Platform, spec AppSpec, in *runtime.Instance) error {
	addr := in.Ports[garageAdminPort]
	if addr == "" {
		return errors.New("garage's admin API isn't published")
	}
	ga := &garageAdmin{base: "http://" + addr, token: spec.Env["GARAGE_ADMIN_TOKEN"],
		http: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}}

	var st struct {
		Node   string `json:"node"`
		Layout struct {
			Version int `json:"version"`
			Roles   []struct {
				ID string `json:"id"`
			} `json:"roles"`
		} `json:"layout"`
	}
	var err error
	for i := 0; i < 60; i++ { // the admin API comes up a moment after S3
		if err = ga.call(ctx, "GET", "/v1/status", nil, &st); err == nil && st.Node != "" {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("garage's admin API: %w", ctx.Err())
		case <-in.Exited:
			return errors.New("garage stopped during setup")
		case <-time.After(250 * time.Millisecond):
		}
	}
	if err != nil {
		return fmt.Errorf("garage's admin API: %w", err)
	}
	assigned := false
	for _, r := range st.Layout.Roles {
		assigned = assigned || r.ID == st.Node
	}
	if !assigned {
		role := []map[string]any{{"id": st.Node, "zone": "dc1", "capacity": int64(100) << 30, "tags": []string{"xu526"}}}
		if err := ga.call(ctx, "POST", "/v1/layout", role, nil); err != nil {
			return fmt.Errorf("garage layout: %w", err)
		}
		if err := ga.call(ctx, "POST", "/v1/layout/apply", map[string]int{"version": st.Layout.Version + 1}, nil); err != nil {
			return fmt.Errorf("garage layout: %w", err)
		}
	}
	access, secret := spec.Env["GARAGE_ACCESS_KEY"], spec.Env["GARAGE_SECRET_KEY"]
	if ga.call(ctx, "GET", "/v1/key?id="+access, nil, nil) != nil {
		if err := ga.call(ctx, "POST", "/v1/key/import", map[string]string{
			"accessKeyId": access, "secretAccessKey": secret, "name": "xu526",
		}, nil); err != nil {
			return fmt.Errorf("garage keys: %w", err)
		}
	}
	if err := ga.call(ctx, "POST", "/v1/key?id="+access, map[string]any{"allow": map[string]bool{"createBucket": true}}, nil); err != nil {
		return fmt.Errorf("garage keys: %w", err)
	}
	return nil
}

type garageAdmin struct {
	base, token string
	http        *http.Client
}

func (g *garageAdmin) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(b, &e) == nil && e.Message != "" {
			return fmt.Errorf("%s %s: %s", method, path, e.Message)
		}
		return fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}
