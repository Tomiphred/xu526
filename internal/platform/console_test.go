package platform

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"xu526/internal/runtime"
)

// A fresh Garage gets a layout, the generated keys and bucket creation; a
// second start (data kept) changes nothing.
func TestGarageSetup(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	version, assigned, imported := 0, false, false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(403)
			return
		}
		b, _ := io.ReadAll(r.Body)
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		switch r.URL.Path {
		case "/v1/status":
			roles := "[]"
			if assigned {
				roles = `[{"id":"n1"}]`
			}
			io.WriteString(w, `{"node":"n1","layout":{"version":`+string(rune('0'+version))+`,"roles":`+roles+`}}`)
		case "/v1/layout/apply":
			var v struct{ Version int }
			json.Unmarshal(b, &v)
			if v.Version != version+1 {
				w.WriteHeader(400)
			}
			version, assigned = v.Version, true
		case "/v1/key/import":
			imported = true
		case "/v1/key":
			if r.Method == "GET" && !imported {
				w.WriteHeader(404)
			}
		}
	}))
	defer srv.Close()

	access, secret := garageKeys()
	if !regexp.MustCompile(`^GK[0-9a-f]{24}$`).MatchString(access) || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(secret) {
		t.Fatalf("keys in Garage's format: %s %s", access, secret)
	}
	spec := AppSpec{Name: "media", Engine: "garage", Env: map[string]string{"GARAGE_ADMIN_TOKEN": "tok", "GARAGE_ACCESS_KEY": access, "GARAGE_SECRET_KEY": secret}}
	in := &runtime.Instance{Ports: map[int]string{garageAdminPort: strings.TrimPrefix(srv.URL, "http://")}, Exited: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := garageSetup(ctx, nil, spec, in); err != nil {
		t.Fatal(err)
	}
	want := []string{"GET /v1/status", "POST /v1/layout", "POST /v1/layout/apply", "GET /v1/key?id=" + access, "POST /v1/key/import", "POST /v1/key?id=" + access}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Errorf("first start:\n got %v\nwant %v", calls, want)
	}
	calls = nil
	if err := garageSetup(ctx, nil, spec, in); err != nil {
		t.Fatal(err)
	}
	want = []string{"GET /v1/status", "GET /v1/key?id=" + access, "POST /v1/key?id=" + access}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Errorf("second start:\n got %v\nwant %v", calls, want)
	}
}

func TestEngineStartAndConsole(t *testing.T) {
	p := envPlatform(t)
	// A RustFS made by v1.5 had its console turned off and no console port.
	old := AppSpec{Name: "files", Engine: "rustfs", Env: map[string]string{"RUSTFS_ACCESS_KEY": "k", "RUSTFS_SECRET_KEY": "s", "RUSTFS_CONSOLE_ENABLE": "false"}}
	rs := runtime.Spec{Env: old.Env}
	p.engineStart(&rs, old)
	if rs.Env["RUSTFS_CONSOLE_ENABLE"] != "true" || old.Env["RUSTFS_CONSOLE_ENABLE"] != "false" || len(rs.Ports) != 1 || rs.Ports[0] != 9001 {
		t.Errorf("rustfs start: %v %v (spec env must stay as it was)", rs.Env, rs.Ports)
	}
	rs = runtime.Spec{}
	p.engineStart(&rs, AppSpec{Name: "s", Engine: "silo"})
	if strings.Join(rs.Args, " ") != "server /data --console-address :9001" {
		t.Errorf("silo args %v", rs.Args)
	}
	rs = runtime.Spec{}
	p.engineStart(&rs, AppSpec{Name: "media", Engine: "garage"})
	if len(rs.Files) != 1 || rs.Files[0].Path != "/etc/garage.toml" || strings.Contains(string(rs.Files[0].Data), "token") || rs.Ports[0] != garageAdminPort {
		t.Errorf("garage start: %+v", rs)
	}

	c, _ := p.connectionOf(AppSpec{Name: "files", Engine: "rustfs", Env: map[string]string{"RUSTFS_ACCESS_KEY": "k", "RUSTFS_SECRET_KEY": "s"}})
	if c.ConsoleURL != "http://files-console.example.com" || c.SignIn == "" {
		t.Errorf("console %q %q", c.ConsoleURL, c.SignIn)
	}
	store := AppSpec{Name: "media", Display: "media", Engine: "garage", Env: map[string]string{"GARAGE_ADMIN_TOKEN": "tok"}}
	ui := garageConsole(store)
	if ui.Name != "media-console" || ui.LoginOf != "media" || ui.Env["API_BASE_URL"] != "http://media:3903" || ui.Env["API_ADMIN_KEY"] != "tok" || kindOf(ui) != "console" {
		t.Errorf("garage console %+v", ui)
	}
}

func TestStoreLogin(t *testing.T) {
	p := envPlatform(t)
	store := &app{spec: AppSpec{Name: "files", Engine: "seaweedfs", Env: map[string]string{"S3_ACCESS_KEY": "key", "S3_SECRET_KEY": "sec"}}}
	for _, tc := range []struct {
		user, pw string
		ok       bool
	}{{"", "", false}, {"key", "nope", false}, {"key", "sec", true}} {
		r := httptest.NewRequest("GET", "/", nil)
		if tc.user != "" {
			r.SetBasicAuth(tc.user, tc.pw)
		}
		w := httptest.NewRecorder()
		if got := p.storeLogin(w, r, store); got != tc.ok {
			t.Errorf("%s:%s: %v", tc.user, tc.pw, got)
		}
		if !tc.ok && (w.Code != 401 || w.Header().Get("WWW-Authenticate") == "") {
			t.Errorf("%s:%s: %d %v", tc.user, tc.pw, w.Code, w.Header())
		}
		if tc.ok && r.Header.Get("Authorization") != "" {
			t.Error("the keys are passed on to the console")
		}
	}
}
