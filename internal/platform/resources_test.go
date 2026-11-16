package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"xu526/internal/bundle"
)

func TestConnections(t *testing.T) {
	p := envPlatform(t)
	db := AppSpec{Name: "main-staging", Display: "main", Engine: "postgres", Env: map[string]string{"POSTGRES_PASSWORD": "pw1"}, Environment: "staging"}
	cache := AppSpec{Name: "cache-staging", Display: "cache", Engine: "redis", Env: map[string]string{"REDIS_PASSWORD": "pw2"}}
	docs := AppSpec{Name: "docs", Engine: "mongodb", Env: map[string]string{"MONGO_INITDB_ROOT_PASSWORD": "pw3"}}
	for _, s := range []AppSpec{db, cache, docs} {
		p.apps[s.Name] = &app{spec: s}
	}
	c, ok := p.connectionOf(db)
	if !ok || c.URL != "postgres://app:pw1@main-staging:5432/app" || c.Host != "main-staging" {
		t.Fatalf("postgres connection %+v", c)
	}
	if m := c.masked(); strings.Contains(m.URL, "pw1") || m.Password == "pw1" {
		t.Errorf("masked still shows the password: %+v", m)
	}
	env := p.linkEnv(AppSpec{Links: []string{"main-staging", "cache-staging", "docs", "gone"}})
	want := map[string]string{
		"DATABASE_URL": "postgres://app:pw1@main-staging:5432/app", "MAIN_URL": "postgres://app:pw1@main-staging:5432/app",
		"REDIS_URL": "redis://:pw2@cache-staging:6379/0", "CACHE_URL": "redis://:pw2@cache-staging:6379/0",
		"MONGODB_URI": "mongodb://app:pw3@docs:27017/app?authSource=admin", "DOCS_URL": "mongodb://app:pw3@docs:27017/app?authSource=admin",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
	if len(env) != len(want) {
		t.Errorf("env = %v", env)
	}

	// Stack files reach them by name in their environment.
	yml := "services:\n  api:\n    image: x\n    environment:\n      DB: ${{ databases.main.url }}\n      R: ${{ caches.cache.host }}\n"
	plan := p.PlanStackIn("shop", "staging", []byte(yml))
	if len(plan.Errors) > 0 {
		t.Fatal(plan.Errors)
	}
	api := svc(plan, "api")
	if api.Spec.Env["DB"] != "postgres://app:pw1@main-staging:5432/app" || api.Spec.Env["R"] != "cache-staging" {
		t.Errorf("refs: %v", api.Spec.Env)
	}
	if strings.Join(api.Spec.SecretKeys, ",") != "DB" {
		t.Errorf("a URL with a password must be secret: %v", api.Spec.SecretKeys)
	}
	if plan := p.PlanStackIn("shop", "", []byte(yml)); len(plan.Errors) == 0 {
		t.Error("production has no database main, yet no error")
	}
}

func TestKinds(t *testing.T) {
	for img, want := range map[string]string{
		"postgres:17-alpine": "database", "docker.io/library/mysql:8": "database", "mongo:8": "database",
		"redis:7": "cache", "valkey/valkey": "cache", "nginx:alpine": "web", "xu/api:1": "api", "ghcr.io/me/postgres-exporter:1": "api",
	} {
		if got := kindOf(AppSpec{Image: img}); got != want {
			t.Errorf("%s: %s, want %s", img, got, want)
		}
	}
	if kindOf(AppSpec{Image: "x", Env: map[string]string{"NEXT_PUBLIC_API": "u"}}) != "web" {
		t.Error("public build values mean a front end")
	}
	if GuessKind(&bundle.Plan{Strategy: bundle.StaticAssets}) != "web" ||
		GuessKind(&bundle.Plan{Cmd: []string{"node_modules/.bin/next", "start"}}) != "web" ||
		GuessKind(&bundle.Plan{Entrypoint: []string{"/app/server"}}) != "api" {
		t.Error("GuessKind")
	}
	// Stack services are sorted too.
	p := envPlatform(t)
	plan := p.PlanStack("s", []byte("services:\n  db:\n    image: postgres:17\n  web:\n    build: ./web\n    ports: ['80']\n    environment: {VITE_API: x}\n  api:\n    build: ./api\n    ports: ['80']\n  q:\n    image: rabbitmq\n    x-xu: {kind: cache}\n"))
	for name, want := range map[string]string{"db": "database", "web": "web", "api": "api", "q": "cache"} {
		if got := svc(plan, name).Kind; got != want {
			t.Errorf("%s: %s, want %s", name, got, want)
		}
	}
}

func TestFolderBrowser(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"My App/package.json", "api/go.mod", "api/Dockerfile", "node_modules/x/package.json", ".git/HEAD"} {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0o755)
		os.WriteFile(filepath.Join(dir, f), []byte("{}"), 0o644)
	}
	l, err := ListFolder(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range l.Folders {
		names = append(names, f.Name+":"+strings.Join(f.Markers, "+"))
	}
	if got := strings.Join(names, ","); got != "api:Dockerfile+go.mod,My App:package.json" {
		t.Errorf("folders = %s", got)
	}
	if l.Parent != filepath.Dir(dir) {
		t.Errorf("parent %s", l.Parent)
	}
	for in, want := range map[string]string{"My App": "my-app", "shortlet-management-main": "shortlet-management-mai", "123": "app", "Web_Site 2": "web-site-2"} {
		if got := suggestName(in); got != want && !(in == "shortlet-management-main" && len(got) <= 20) {
			t.Errorf("suggestName(%q) = %q, want %q", in, got, want)
		}
	}
	fi, err := InspectFolder(filepath.Join(dir, "api"))
	if err != nil || !fi.Docker || fi.Name != "api" {
		t.Errorf("inspect %+v %v", fi, err)
	}
}

func TestFolderRequestChecks(t *testing.T) {
	p := envPlatform(t)
	p.sources = map[string]*Source{}
	for name, req := range map[string]FolderRequest{
		"no folder": {Name: "a"},
		"bad name":  {Dir: t.TempDir(), Name: "Bad Name"},
		"bad link":  {Dir: t.TempDir(), Name: "a", Links: []string{"nope"}},
		"bad kind":  {Dir: t.TempDir(), Name: "a", AppKind: "db"},
		"no stack":  {Dir: t.TempDir(), Name: "a", Kind: "stack"},
		"not a dir": {Dir: "/definitely/not/here", Name: "a"},
	} {
		if _, err := p.ConnectFolder(req); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
