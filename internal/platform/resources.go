package platform

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"xu526/internal/bundle"
	"xu526/internal/runtime"
)

// Databases, caches and object storage that xu526 creates and runs
// itself. Each is an app on its environment's shared network, with its
// data in a named volume and generated credentials. Apps that list it in
// Links get its connection settings as variables.

// engine describes one kind of database, cache or object store.
type engine struct {
	Kind, Label, Image, DataPath string
	Port                         int
	// UserKey and PasswordKey are the Env entries holding the user (or
	// access key) and password (or secret key).
	UserKey, PasswordKey string
	User                 string // fixed user name; "" = generated (object stores' access keys)
	Database             string
	Public               bool // reachable through the router (object stores: browsers load files)
	env                  func(user, pw string) map[string]string
	entrypoint, args     []string
	url                  func(host string, port int, user, pw string) string
	vars                 []string // what linked apps get, besides <NAME>_URL (all set to the URL)

	// Object stores' web interface.
	console      int                         // container port of its own console page, served at <name>-console
	consoleLogin bool                        // the console has no sign-in: xu526 asks for the keys first
	consoleApp   func(store AppSpec) AppSpec // a separate container serves the console
	signIn       string                      // how to sign in to the console
	runEnv       map[string]string           // settings applied at every start (also to stores made by older versions)
	keys         func() (access, secret string)
	secrets      func() map[string]string // more generated values kept in Env (secret)
	ports        []int                    // more ports xu526 itself talks to (an admin API)
	files        func(spec AppSpec) []runtime.File
	setup        func(ctx context.Context, p *Platform, spec AppSpec, in *runtime.Instance) error
}

var engines = map[string]engine{
	"postgres": {
		Kind: "database", Label: "PostgreSQL", Image: "postgres:17-alpine", Port: 5432, DataPath: "/var/lib/postgresql/data",
		User: "app", Database: "app", PasswordKey: "POSTGRES_PASSWORD", vars: []string{"DATABASE_URL"},
		env: func(_, pw string) map[string]string {
			return map[string]string{"POSTGRES_USER": "app", "POSTGRES_DB": "app", "POSTGRES_PASSWORD": pw}
		},
		url: func(h string, p int, _, pw string) string {
			return fmt.Sprintf("postgres://app:%s@%s:%d/app", url.PathEscape(pw), h, p)
		},
	},
	"mysql": {
		Kind: "database", Label: "MySQL", Image: "mysql:8.4", Port: 3306, DataPath: "/var/lib/mysql",
		User: "app", Database: "app", PasswordKey: "MYSQL_PASSWORD", vars: []string{"DATABASE_URL"},
		env: func(_, pw string) map[string]string {
			return map[string]string{"MYSQL_USER": "app", "MYSQL_DATABASE": "app", "MYSQL_PASSWORD": pw, "MYSQL_ROOT_PASSWORD": pw}
		},
		url: func(h string, p int, _, pw string) string {
			return fmt.Sprintf("mysql://app:%s@%s:%d/app", url.PathEscape(pw), h, p)
		},
	},
	"mongodb": {
		Kind: "database", Label: "MongoDB", Image: "mongo:8", Port: 27017, DataPath: "/data/db",
		User: "app", Database: "app", PasswordKey: "MONGO_INITDB_ROOT_PASSWORD", vars: []string{"MONGODB_URI", "DATABASE_URL"},
		env: func(_, pw string) map[string]string {
			return map[string]string{"MONGO_INITDB_ROOT_USERNAME": "app", "MONGO_INITDB_ROOT_PASSWORD": pw, "MONGO_INITDB_DATABASE": "app"}
		},
		url: func(h string, p int, _, pw string) string {
			return fmt.Sprintf("mongodb://app:%s@%s:%d/app?authSource=admin", url.PathEscape(pw), h, p)
		},
	},
	"redis": {
		Kind: "cache", Label: "Redis", Image: "redis:7-alpine", Port: 6379, DataPath: "/data",
		PasswordKey: "REDIS_PASSWORD", vars: []string{"REDIS_URL"},
		env: func(_, pw string) map[string]string { return map[string]string{"REDIS_PASSWORD": pw} },
		// The password comes from the environment, so it isn't shown with the command.
		args: []string{"sh", "-c", `exec redis-server --requirepass "$REDIS_PASSWORD" --appendonly yes`},
		url: func(h string, p int, _, pw string) string {
			return fmt.Sprintf("redis://:%s@%s:%d/0", url.PathEscape(pw), h, p)
		},
	},

	// Object storage: S3-compatible, so any S3 SDK works with the endpoint,
	// access key and secret key. All four are open source, run from a
	// public image with one volume, and have a web interface (console.go).
	"rustfs": {
		Kind: "storage", Label: "RustFS", Image: "rustfs/rustfs:latest", Port: 9000, DataPath: "/data", Public: true,
		UserKey: "RUSTFS_ACCESS_KEY", PasswordKey: "RUSTFS_SECRET_KEY", Database: "app",
		env: func(u, pw string) map[string]string {
			return map[string]string{"RUSTFS_ACCESS_KEY": u, "RUSTFS_SECRET_KEY": pw, "RUSTFS_ADDRESS": ":9000"}
		},
		runEnv:  map[string]string{"RUSTFS_CONSOLE_ENABLE": "true", "RUSTFS_CONSOLE_ADDRESS": ":9001"},
		args:    []string{"/data"},
		url:     s3URL,
		console: 9001, signIn: "Sign in with the access key and secret key.",
	},
	"seaweedfs": {
		Kind: "storage", Label: "SeaweedFS", Image: "chrislusf/seaweedfs:latest", Port: 8333, DataPath: "/data", Public: true,
		UserKey: "S3_ACCESS_KEY", PasswordKey: "S3_SECRET_KEY", Database: "app",
		env: func(u, pw string) map[string]string {
			return map[string]string{"S3_ACCESS_KEY": u, "S3_SECRET_KEY": pw}
		},
		// SeaweedFS reads S3 keys from a file: write it from the environment
		// at start, so the keys aren't part of the command.
		entrypoint: []string{"/bin/sh", "-c"},
		args:       []string{`printf '{"identities":[{"name":"app","credentials":[{"accessKey":"%s","secretKey":"%s"}],"actions":["Admin","Read","Write","List","Tagging"]}]}' "$S3_ACCESS_KEY" "$S3_SECRET_KEY" > /tmp/s3.json && exec weed server -dir=/data -s3 -s3.port=8333 -s3.config=/tmp/s3.json -ip.bind=0.0.0.0`},
		url:        s3URL,
		// The filer's page browses and uploads files, but has no sign-in.
		console: 8888, consoleLogin: true, signIn: "Your browser asks for a user name and password: use the access key and secret key.",
	},
	"silo": {
		Kind: "storage", Label: "Silo (MinIO-compatible)", Image: "pgsty/silo:latest", Port: 9000, DataPath: "/data", Public: true,
		UserKey: "MINIO_ROOT_USER", PasswordKey: "MINIO_ROOT_PASSWORD", Database: "app",
		env: func(u, pw string) map[string]string {
			return map[string]string{"MINIO_ROOT_USER": u, "MINIO_ROOT_PASSWORD": pw}
		},
		args:    []string{"server", "/data", "--console-address", ":9001"},
		url:     s3URL,
		console: 9001, signIn: "Sign in with the access key (user name) and secret key (password).",
	},
	"garage": {
		Kind: "storage", Label: "Garage", Image: "dxflrs/garage:v1.1.0", Port: 3900, DataPath: "/var/lib/garage", Public: true,
		UserKey: "GARAGE_ACCESS_KEY", PasswordKey: "GARAGE_SECRET_KEY", Database: "app",
		// Garage reads its secrets from these, and the rest from garage.toml.
		env: func(u, pw string) map[string]string {
			return map[string]string{"GARAGE_ACCESS_KEY": u, "GARAGE_SECRET_KEY": pw}
		},
		secrets: func() map[string]string {
			return map[string]string{"GARAGE_RPC_SECRET": randomHex(32), "GARAGE_ADMIN_TOKEN": randomHex(32)}
		},
		keys:       garageKeys,
		ports:      []int{garageAdminPort},
		files:      garageFiles,
		setup:      garageSetup,
		url:        s3URL,
		consoleApp: garageConsole,
		signIn:     "Your browser asks for a user name and password: use the access key and secret key.",
	},
}

func s3URL(h string, p int, _, _ string) string { return fmt.Sprintf("http://%s:%d", h, p) }

// Connection is how apps reach a database, cache or object store.
type Connection struct {
	Engine    string `json:"engine"`
	Host      string `json:"host"` // its name on the environment's network
	Port      int    `json:"port"`
	User      string `json:"user,omitempty"`     // object stores: the access key
	Password  string `json:"password"`           // object stores: the secret key
	Database  string `json:"database,omitempty"` // object stores: the bucket made for apps
	URL       string `json:"url"`                // object stores: the S3 endpoint inside the environment
	PublicURL string `json:"public_url,omitempty"`
	// Object stores: their web interface, and how to sign in to it.
	ConsoleURL string            `json:"console_url,omitempty"`
	SignIn     string            `json:"sign_in,omitempty"`
	Vars       []string          `json:"vars"` // the variables linked apps get
	Env        map[string]string `json:"env"`  // ... and their values
}

// envNetwork is the network every app of an environment joins.
func envNetwork(env string) string { return "xu-env-" + envOrDefault(env) }

func resourceKey(name, env string) string { return stackKey(name, env) }

var resourceNameRE = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,18}[a-z0-9])?$`)

func newPassword() string {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b) // letters and digits only: safe inside any URL
}

// connectionOf returns a database's, cache's or object store's connection settings.
func (p *Platform) connectionOf(spec AppSpec) (Connection, bool) {
	e, ok := engines[spec.Engine]
	if !ok {
		return Connection{}, false
	}
	user, pw := e.User, spec.Env[e.PasswordKey]
	if e.UserKey != "" {
		user = spec.Env[e.UserKey]
	}
	c := Connection{Engine: spec.Engine, Host: spec.Name, Port: e.Port, User: user, Password: pw, Database: e.Database,
		URL: e.url(spec.Name, e.Port, user, pw), Env: map[string]string{}}
	own := envVarName(displayName(spec)) + "_URL"
	if e.Kind == "storage" {
		c.PublicURL = p.appURL(spec.Name)
		if e.console > 0 || e.consoleApp != nil {
			c.ConsoleURL, c.SignIn = p.appURL(spec.Name+"-console"), e.signIn
		}
		for k, v := range map[string]string{
			"S3_ENDPOINT": c.URL, "S3_PUBLIC_URL": c.PublicURL, "S3_BUCKET": c.Database, "S3_REGION": "us-east-1",
			"S3_ACCESS_KEY_ID": user, "S3_SECRET_ACCESS_KEY": pw, "S3_FORCE_PATH_STYLE": "true",
			// read by the AWS SDKs and CLI as they are
			"AWS_ACCESS_KEY_ID": user, "AWS_SECRET_ACCESS_KEY": pw, "AWS_REGION": "us-east-1", "AWS_ENDPOINT_URL_S3": c.URL,
			own: c.URL,
		} {
			c.Env[k] = v
		}
	} else {
		for _, v := range append(append([]string(nil), e.vars...), own) {
			c.Env[v] = c.URL
		}
	}
	for k := range c.Env {
		c.Vars = append(c.Vars, k)
	}
	sort.Strings(c.Vars)
	return c, true
}

// masked hides the password.
func (c Connection) masked() Connection {
	if c.Password == "" {
		return c
	}
	hide := func(s string) string {
		s = strings.Replace(s, url.PathEscape(c.Password), maskedValue, 1)
		return strings.Replace(s, c.Password, maskedValue, 1)
	}
	c.URL = hide(c.URL)
	env := map[string]string{}
	for k, v := range c.Env {
		env[k] = hide(v)
	}
	c.Env = env
	c.Password = maskedValue
	return c
}

func displayName(spec AppSpec) string {
	if spec.Display != "" {
		return spec.Display
	}
	return spec.Name
}

func envVarName(s string) string {
	return strings.ToUpper(strings.ReplaceAll(s, "-", "_"))
}

// CreateResource creates and starts a database or cache in an environment.
func (p *Platform) CreateResource(ctx context.Context, name, engineName, env string) (AppSpec, error) {
	e, ok := engines[engineName]
	if !ok {
		return AppSpec{}, fmt.Errorf("unknown engine %q: choose postgres, mysql, mongodb, redis, rustfs, seaweedfs, silo or garage", engineName)
	}
	if !resourceNameRE.MatchString(name) {
		return AppSpec{}, fmt.Errorf("name %q: use 1-20 lowercase letters, digits and dashes, starting with a letter", name)
	}
	env = envOrDefault(env)
	app := resourceKey(name, env)
	if p.get(app) != nil {
		return AppSpec{}, fmt.Errorf("%s already exists", app)
	}
	if e.Kind == "storage" && p.get(app+"-console") != nil {
		return AppSpec{}, fmt.Errorf("%s-console already exists; that address is for this store's console", app)
	}
	user, pw := e.User, newPassword()
	if e.UserKey != "" {
		user = "xu" + newPassword()[:18] // object stores: a generated access key
	}
	if e.keys != nil {
		user, pw = e.keys()
	}
	spec := AppSpec{
		Name: app, Display: name, Image: e.Image, Port: e.Port, Internal: !e.Public, Kind: e.Kind, Engine: engineName,
		Env: e.env(user, pw), Entrypoint: e.entrypoint, Args: e.args, MinScale: 1, MaxScale: 1,
		Environment: environmentField(env),
		Volumes:     []VolumeMount{{Volume: "xu-" + app + "-data", Path: e.DataPath}},
	}
	spec.SecretKeys = []string{e.PasswordKey}
	if e.secrets != nil {
		for k, v := range e.secrets() {
			spec.Env[k] = v
			spec.SecretKeys = append(spec.SecretKeys, k)
		}
		sort.Strings(spec.SecretKeys[1:])
	}
	if engineName == "mysql" {
		spec.SecretKeys = append(spec.SecretKeys, "MYSQL_ROOT_PASSWORD")
	}
	if err := p.Deploy(ctx, spec); err != nil {
		return spec, err
	}
	p.event(app, "deploy", "created %s %s", e.Label, name)
	if e.Kind == "storage" {
		// The bucket apps get as S3_BUCKET.
		if err := p.CreateBucket(ctx, app, e.Database); err != nil {
			p.event(app, "error", "creating the bucket %s: %v", e.Database, err)
		}
		if e.consoleApp != nil {
			if err := p.Deploy(ctx, e.consoleApp(spec)); err != nil {
				p.event(app, "error", "starting its console: %v", err)
			}
		}
	}
	return spec, nil
}

// linkEnv is what an app gets from the databases and caches it links to:
// DATABASE_URL (or MONGODB_URI) for the first database, REDIS_URL for the
// first cache, and <NAME>_URL for each one.
func (p *Platform) linkEnv(spec AppSpec) map[string]string {
	if len(spec.Links) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, l := range spec.Links {
		a := p.get(l)
		if a == nil {
			continue
		}
		c, ok := p.connectionOf(a.spec)
		if !ok {
			continue
		}
		for k, v := range c.Env {
			if _, set := out[k]; !set {
				out[k] = v
			}
		}
	}
	return out
}

// resourceRef resolves ${{ databases.NAME.FIELD }} and ${{ caches.NAME.FIELD }}.
func (p *Platform) resourceRef(kind, name, field, env string) (string, error) {
	a := p.get(resourceKey(name, env))
	if a == nil || a.spec.Engine == "" || engines[a.spec.Engine].Kind != kind {
		return "", fmt.Errorf("the %s environment has no %s called %s; create it in the dashboard (%s → New)", env, kind, name,
			map[string]string{"database": "Databases", "cache": "Cache", "storage": "Storage"}[kind])
	}
	c, _ := p.connectionOf(a.spec)
	switch field {
	case "url":
		return c.URL, nil
	case "host":
		return c.Host, nil
	case "port":
		return strconv.Itoa(c.Port), nil
	case "user":
		return c.User, nil
	case "password":
		return c.Password, nil
	case "database", "name", "bucket":
		return c.Database, nil
	case "access_key":
		return c.User, nil
	case "secret_key":
		return c.Password, nil
	case "public_url":
		return c.PublicURL, nil
	}
	return "", fmt.Errorf("%s has no %q; use url, host, port, user, password or database (storage: url, public_url, bucket, access_key, secret_key)", name, field)
}

// imageKind works out the dashboard section of an app from its image.
var (
	dbImageRE    = regexp.MustCompile(`(^|/)(postgres|postgis|timescaledb|mysql|mariadb|mongo|mongodb|cockroach|clickhouse|cassandra|neo4j|couchdb)([:@]|$)`)
	cacheImageRE = regexp.MustCompile(`(^|/)(redis|valkey|keydb|memcached|dragonfly)([:@]|$)`)
	storeImageRE = regexp.MustCompile(`(^|/)(minio|silo|rustfs|seaweedfs|garage|cloudserver|s3proxy|versitygw)([:@]|$)`)
	webImageRE   = regexp.MustCompile(`(^|/)(nginx|caddy|httpd)([:@]|$)`)
)

// kindOf is an app's dashboard section.
func kindOf(spec AppSpec) string {
	if spec.Upstream != "" {
		return "tunnel"
	}
	switch spec.Kind {
	case "backend":
		return "api" // the name before Web, API and Worker
	case "":
	default:
		return spec.Kind
	}
	img := strings.ToLower(spec.Image)
	switch {
	case dbImageRE.MatchString(img):
		return "database"
	case cacheImageRE.MatchString(img):
		return "cache"
	case storeImageRE.MatchString(img):
		return "storage"
	case webImageRE.MatchString(img):
		return "web"
	}
	for k := range spec.Env {
		if hasPublicPrefix(k) {
			return "web"
		}
	}
	if spec.Internal && spec.Port == 0 && spec.Image != "" {
		return "worker"
	}
	return "api"
}

// resourceAPI serves databases and caches.
//
//	POST   /v1/resources                     {name, engine, environment}
//	GET    /v1/apps/{name}/connection        its connection settings, password included
func (p *Platform) resourceAPI(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/resources", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Name, Engine, Environment string }
		if err := decodeJSON(w, r, &req); err != nil {
			return
		}
		spec, err := p.CreateResource(r.Context(), strings.TrimSpace(req.Name), req.Engine, req.Environment)
		if err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"name": spec.Name})
	})
	// An app's environment variables with their values (the dashboard hides
	// them until asked): its own, the ones from secrets, and the ones it
	// gets from the databases, caches and storage it links to.
	mux.HandleFunc("GET /v1/apps/{name}/env", func(w http.ResponseWriter, r *http.Request) {
		a := p.get(r.PathValue("name"))
		if a == nil {
			writeErr(w, http.StatusNotFound, "no app "+r.PathValue("name"))
			return
		}
		writeJSON(w, http.StatusOK, p.envVars(a.spec))
	})
	mux.HandleFunc("GET /v1/apps/{name}/connection", func(w http.ResponseWriter, r *http.Request) {
		a := p.get(r.PathValue("name"))
		if a == nil {
			writeErr(w, http.StatusNotFound, "no app "+r.PathValue("name"))
			return
		}
		c, ok := p.connectionOf(a.spec)
		if !ok {
			writeErr(w, http.StatusNotFound, "this app isn't a database, cache or object store made by xu526")
			return
		}
		writeJSON(w, http.StatusOK, c)
	})
}

// frontEndRE matches start commands of front-end frameworks.
var frontEndRE = regexp.MustCompile(`(^|[/\s])(next|nuxt|nuxi|remix-serve|astro|vite|react-router-serve|svelte-kit)(\s|$)`)

// GuessKind says whether a build is a web front end or an API: static
// sites and apps served by a front-end framework are web, the rest api.
// (Workers can't be told apart from APIs by their build.)
func GuessKind(plan *bundle.Plan) string {
	if plan == nil {
		return "api"
	}
	if plan.Strategy == bundle.StaticAssets {
		return "web"
	}
	cmd := strings.Join(append(append([]string{}, plan.Entrypoint...), plan.Cmd...), " ")
	if frontEndRE.MatchString(cmd) || strings.Contains(cmd, "/.next/") || strings.Contains(cmd, "/next/dist/") ||
		strings.Contains(cmd, ".output/server") {
		return "web"
	}
	for _, s := range plan.Summary {
		if l := strings.ToLower(s); strings.Contains(l, "next.js") || strings.Contains(l, "nuxt") || strings.Contains(l, "standalone server") {
			return "web"
		}
	}
	return "api"
}

// EnvVar is one environment variable an app runs with.
type EnvVar struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Secret bool   `json:"secret,omitempty"` // from the environment's secrets, or a password
	From   string `json:"from,omitempty"`   // the database, cache or store it comes from ("" = set on the app)
}

func (p *Platform) envVars(spec AppSpec) []EnvVar {
	secret := map[string]bool{}
	for _, k := range spec.SecretKeys {
		secret[k] = true
	}
	out := []EnvVar{}
	for k, v := range spec.Env {
		out = append(out, EnvVar{Key: k, Value: v, Secret: secret[k]})
	}
	for _, l := range spec.Links {
		la := p.get(l)
		if la == nil {
			continue
		}
		c, ok := p.connectionOf(la.spec)
		if !ok {
			continue
		}
		for k, v := range c.Env {
			if _, own := spec.Env[k]; own || containsKey(out, k) {
				continue // the app's own value, or an earlier link's, wins
			}
			out = append(out, EnvVar{Key: k, Value: v, From: displayName(la.spec), Secret: c.Password != "" && strings.Contains(v, c.Password)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func containsKey(vs []EnvVar, k string) bool {
	for _, v := range vs {
		if v.Key == k {
			return true
		}
	}
	return false
}
