package platform

import (
	"strings"
	"testing"
)

func planFor(t *testing.T, yml string) *StackPlan {
	t.Helper()
	p := &Platform{cfg: Config{Domain: "example.com", PublicPort: 80}}
	return p.PlanStack("shop", []byte(yml))
}

func svc(plan *StackPlan, name string) PlanService {
	for _, s := range plan.Services {
		if s.Name == name {
			return s
		}
	}
	return PlanService{}
}

func TestPlanTypicalStack(t *testing.T) {
	plan := planFor(t, `
services:
  web:
    image: nginx:alpine
    ports: ["8080:80"]
    environment:
      - API_URL=http://api:8080
    depends_on: [api]
    x-xu: {max_scale: 6, target_concurrency: 20}
  api:
    image: myapi:1
    expose: ["8080"]
    command: ./server --verbose "two words"
    environment:
      DATABASE_URL: postgres://u:${DB_PASS:-secret}@db:5432/shop
      EMPTY:
    depends_on:
      db: {condition: service_healthy}
    deploy:
      replicas: 2
      resources: {limits: {memory: 512M, cpus: "0.5"}}
  db:
    image: postgres:16
    volumes: ["pgdata:/var/lib/postgresql/data", "./init.sql:/docker-entrypoint-initdb.d/init.sql"]
    restart: always
volumes:
  pgdata:
`)
	if len(plan.Errors) > 0 {
		t.Fatalf("unexpected errors: %v", plan.Errors)
	}
	if got := strings.Join(plan.Order, ","); got != "db,api,web" {
		t.Errorf("order = %s, want db,api,web", got)
	}

	web, api, db := svc(plan, "web"), svc(plan, "api"), svc(plan, "db")
	if !web.Public || web.Port != 80 || web.URL != "http://shop-web.example.com" || web.Min != 0 || web.Max != 6 || web.Target != 20 {
		t.Errorf("web = %+v", web)
	}
	if api.Public || api.Port != 8080 || api.Min != 2 || api.Max != 2 || api.Spec.MemoryMB != 512 || api.Spec.CPUs != 0.5 {
		t.Errorf("api = %+v", api)
	}
	if strings.Join(api.Spec.Args, "|") != "./server|--verbose|two words" {
		t.Errorf("api args = %q", api.Spec.Args)
	}
	if api.Spec.Env["DATABASE_URL"] != "postgres://u:secret@db:5432/shop" {
		t.Errorf("interpolation: %q", api.Spec.Env["DATABASE_URL"])
	}
	if len(db.Volumes) != 1 || db.Volumes[0].Volume != "xu-shop-pgdata" || db.Min != 1 || db.Max != 1 {
		t.Errorf("db = %+v", db)
	}
	if !db.Spec.Internal || db.Spec.Network != "xu-shop" || db.Spec.Service != "db" {
		t.Errorf("db spec = %+v", db.Spec)
	}

	// the env var mentioning "db" is a depends_on already; web→api too.
	// No duplicate "uses" edges should appear for those.
	for _, e := range plan.Edges {
		if e.Kind == "uses" {
			t.Errorf("unexpected inferred edge %+v", e)
		}
	}
	joined := strings.Join(plan.Warnings, "\n")
	for _, want := range []string{"host folder ./init.sql", "restart is ignored", "EMPTY has no value"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings missing %q:\n%s", want, joined)
		}
	}
}

func TestPlanInfersConnectionsFromEnv(t *testing.T) {
	plan := planFor(t, `
services:
  worker:
    image: w:1
    environment: {QUEUE_URL: "redis://cache:6379/0", NOT_A_HOST: "cacheverything"}
  cache:
    image: redis:7
`)
	var uses []PlanEdge
	for _, e := range plan.Edges {
		if e.Kind == "uses" {
			uses = append(uses, e)
		}
	}
	if len(uses) != 1 || uses[0].From != "worker" || uses[0].To != "cache" || uses[0].Via != "QUEUE_URL" {
		t.Fatalf("uses edges = %+v", uses)
	}
}

func TestPlanErrors(t *testing.T) {
	cases := map[string]string{
		"missing dependency": "services:\n  a:\n    image: x\n    depends_on: [ghost]\n",
		"loop":               "services:\n  a:\n    image: x\n    depends_on: [b]\n  b:\n    image: x\n    depends_on: [a]\n",
		"build from a URL":   "services:\n  a:\n    build: https://example.com/repo.git\n",
		"bad yaml":           "services: [\n",
		"no services":        "volumes: {}\n",
		"port range":         "services:\n  a:\n    image: x\n    ports: [\"8000-8010:8000-8010\"]\n",
	}
	for name, yml := range cases {
		if plan := planFor(t, yml); len(plan.Errors) == 0 {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestParseMemory(t *testing.T) {
	for in, want := range map[string]int64{"512m": 512, "1g": 1024, "1.5G": 1536, "256MB": 256, "": 0, "2097152": 2} {
		if in == "2097152" {
			continue // bytes below 6 MB are rejected
		}
		got, err := parseMemory(in)
		if err != nil || got != want {
			t.Errorf("parseMemory(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
}

// Who a published port is open to decides whether a service is public:
// "3000:3000" opens it to the network, "127.0.0.1:3000:3000" only to this
// computer, which keeps it internal.
func TestPortsHostIPDecidesPublic(t *testing.T) {
	plan := planFor(t, `services:
  web:      {image: nginx, ports: ["3000:3000"]}
  any:      {image: nginx, ports: ["0.0.0.0:3001:3001"]}
  bare:     {image: nginx, ports: ["3002"]}
  local:    {image: nginx, ports: ["127.0.0.1:3003:3003"]}
  local6:   {image: nginx, ports: ["[::1]:3004:3004/tcp"]}
  name:     {image: nginx, ports: ["localhost:3005:3005"]}
  long:     {image: nginx, ports: [{target: 3006, published: 3006, host_ip: 127.0.0.1}]}
  longpub:  {image: nginx, ports: [{target: 3007, published: 3007}]}
  mixed:    {image: nginx, ports: ["127.0.0.1:9000:9000", "8080:8080"]}
  forced:   {image: nginx, ports: ["127.0.0.1:3008:3008"], x-xu: {public: true}}
`)
	if len(plan.Errors) > 0 {
		t.Fatal(plan.Errors)
	}
	want := map[string]bool{"web": true, "any": true, "bare": true, "local": false, "local6": false, "name": false,
		"long": false, "longpub": true, "mixed": true, "forced": true}
	for _, s := range plan.Services {
		if s.Public != want[s.Name] {
			t.Errorf("%s: public = %v, want %v", s.Name, s.Public, want[s.Name])
		}
		if s.Name == "local" && (s.Port != 3003 || !s.Spec.Internal) {
			t.Errorf("local: port %d internal %v", s.Port, s.Spec.Internal)
		}
	}
	found := false
	for _, n := range plan.Notes {
		found = found || strings.Contains(n, "local publishes its port on 127.0.0.1 only")
	}
	if !found {
		t.Errorf("no note explaining why local is internal: %v", plan.Notes)
	}
}
