package platform

import (
	"strings"
	"testing"
)

const refStack = `
services:
  db:
    image: postgres:17
    environment:
      POSTGRES_PASSWORD: ${{ secrets.DB_PASSWORD }}
    volumes: [pgdata:/var/lib/postgresql/data]
  api:
    build: ./backend
    ports: ["8000"]
    environment:
      DATABASE_URL: postgres://app:${{ secrets.DB_PASSWORD }}@${{ services.db.host }}:5432/app
      SITE: ${{ services.web.url }}
      WHERE: ${{ environment.name }}/${{ stack.name }}
      MODE: ${{ vars.MODE }}
  web:
    build:
      context: ./web
      args: {BUILT_FOR: "${{ environment.name }}"}
    ports: ["3000"]
    environment:
      NEXT_PUBLIC_API_URL: ${{ services.api.url }}
      SERVER_ONLY: ${{ services.api.internal_url }}
volumes:
  pgdata:
`

func envPlatform(t *testing.T) *Platform {
	t.Helper()
	p := &Platform{cfg: Config{Domain: "example.com", PublicPort: 80}, envs: map[string]*Environment{},
		stacks: map[string]*stackRecord{}, apps: map[string]*app{}, agent: newAgentState()}
	pw, mode := "s3cret", "fast"
	if err := p.SetEnvironmentValues("staging", map[string]*string{"MODE": &mode}, map[string]*string{"DB_PASSWORD": &pw}); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStackInEnvironment(t *testing.T) {
	p := envPlatform(t)
	plan := p.PlanStackIn("shop", "staging", []byte(refStack))
	if len(plan.Errors) > 0 {
		t.Fatalf("errors: %v", plan.Errors)
	}
	if plan.Stack != "shop-staging" || plan.Name != "shop" || plan.Environment != "staging" || plan.Network != "xu-shop-staging" {
		t.Fatalf("plan names: %+v", plan)
	}
	api, web, db := svc(plan, "api"), svc(plan, "web"), svc(plan, "db")
	if api.App != "shop-staging-api" || api.Spec.Environment != "staging" {
		t.Errorf("api app %s env %q", api.App, api.Spec.Environment)
	}
	if db.Volumes[0].Volume != "xu-shop-staging-pgdata" {
		t.Errorf("volume %s isn't per environment", db.Volumes[0].Volume)
	}
	env := api.Spec.Env
	if env["DATABASE_URL"] != "postgres://app:s3cret@db:5432/app" || env["MODE"] != "fast" || env["WHERE"] != "staging/shop" ||
		env["SITE"] != "http://shop-staging-web.example.com" {
		t.Errorf("api env = %v", env)
	}
	if strings.Join(api.Spec.SecretKeys, ",") != "DATABASE_URL" || strings.Join(db.Spec.SecretKeys, ",") != "POSTGRES_PASSWORD" {
		t.Errorf("secret keys api=%v db=%v", api.Spec.SecretKeys, db.Spec.SecretKeys)
	}
	if web.Spec.Env["SERVER_ONLY"] != "http://api:8000" {
		t.Errorf("internal url = %q", web.Spec.Env["SERVER_ONLY"])
	}
	// Builds: the public variable and the explicit arg reach the build.
	if api.Build == nil || api.Build.Context != "./backend" || web.Build == nil {
		t.Fatalf("builds: api=%+v web=%+v", api.Build, web.Build)
	}
	if web.Build.Args["NEXT_PUBLIC_API_URL"] != "http://shop-staging-api.example.com" || web.Build.Args["BUILT_FOR"] != "staging" {
		t.Errorf("web build args = %v", web.Build.Args)
	}
	if _, ok := web.Build.Args["SERVER_ONLY"]; ok {
		t.Error("a server-only variable was passed to the build")
	}
	if api.Image != "built from ./backend" || api.Spec.Image != "" {
		t.Errorf("unbuilt image %q / %q", api.Image, api.Spec.Image)
	}

	// Production: same file, plain names, its own (missing) secret.
	prod := p.PlanStackIn("shop", "", []byte(refStack))
	if prod.Stack != "shop" || svc(prod, "api").App != "shop-api" {
		t.Errorf("production names: %s %s", prod.Stack, svc(prod, "api").App)
	}
	if len(prod.Errors) == 0 || !strings.Contains(strings.Join(prod.Errors, " "), "no secret DB_PASSWORD") {
		t.Errorf("missing secret not reported: %v", prod.Errors)
	}

	// With the images built, they're used.
	built := p.planStack(stackTarget{Stack: "shop", Env: "staging"}, []byte(refStack), map[string]string{"api": "xu/shop-staging-api:abc", "web": "xu/shop-staging-web:def"})
	if svc(built, "api").Spec.Image != "xu/shop-staging-api:abc" || len(built.Errors) > 0 {
		t.Errorf("built: %q %v", svc(built, "api").Spec.Image, built.Errors)
	}
}

func TestReferenceProblems(t *testing.T) {
	p := envPlatform(t)
	cases := map[string]string{
		"unknown service": "services:\n  a:\n    image: x\n    environment: {U: '${{ services.nope.url }}'}\n",
		"unknown kind":    "services:\n  a:\n    image: x\n    environment: {U: '${{ github.token }}'}\n",
		"secret in build": "services:\n  a:\n    build: {context: ., args: {K: '${{ secrets.DB_PASSWORD }}'}}\n",
		"public secret":   "services:\n  a:\n    build: .\n    environment: {NEXT_PUBLIC_K: '${{ secrets.DB_PASSWORD }}'}\n",
		"secret command":  "services:\n  a:\n    image: x\n    command: run --password ${{ secrets.DB_PASSWORD }}\n",
		"missing var":     "services:\n  a:\n    image: x\n    environment: {U: '${{ vars.NOPE }}'}\n",
		"bad env name":    "services:\n  a:\n    image: x\n",
		"share internal":  "services:\n  a:\n    image: x\n    x-xu: {share: true}\n",
	}
	for name, yml := range cases {
		env := "staging"
		if name == "bad env name" {
			env = "Not_OK"
		}
		if plan := p.PlanStackIn("s", env, []byte(yml)); len(plan.Errors) == 0 {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestSharedURL(t *testing.T) {
	p := envPlatform(t)
	yml := "services:\n  api:\n    image: x\n    ports: ['80']\n    x-xu: {share: true}\n  web:\n    image: y\n    ports: ['80']\n    environment: {API: '${{ services.api.url }}'}\n"
	// Not connected to a relay: the local address, with a warning.
	plan := p.PlanStackIn("shop", "", []byte(yml))
	if got := svc(plan, "web").Spec.Env["API"]; got != "http://shop-api.example.com" || len(plan.Warnings) == 0 {
		t.Errorf("unconnected: %q %v", got, plan.Warnings)
	}
	// Connected: the relay's address for the app.
	p.agent.cfg.URL, p.agent.user, p.agent.pattern = "https://relay.example.net", "ada", "https://{app}-ada.example.net"
	plan = p.PlanStackIn("shop", "", []byte(yml))
	if got := svc(plan, "web").Spec.Env["API"]; got != "https://shop-api-ada.example.net" {
		t.Errorf("connected: %q", got)
	}
	if svc(plan, "api").SharedURL != "https://shop-api-ada.example.net" {
		t.Errorf("shared url %q", svc(plan, "api").SharedURL)
	}
	// An older relay that sends no pattern: learnt from another shared app.
	p.agent.pattern = ""
	p.agent.urls = map[string]string{"blog": "https://blog-ada.example.net"}
	if u, ok := p.tunnelURLFor("shop-api"); !ok || u != "https://shop-api-ada.example.net" {
		t.Errorf("derived %q %v", u, ok)
	}
}

func TestEnvironmentsList(t *testing.T) {
	p := envPlatform(t)
	p.stacks["shop-staging"] = &stackRecord{Name: "shop-staging", Stack: "shop", Environment: "staging"}
	p.stacks["blog"] = &stackRecord{Name: "blog"}
	envs := p.Environments()
	if len(envs) != 2 || envs[0].Name != "production" || !envs[0].Default || envs[1].Name != "staging" {
		t.Fatalf("envs = %+v", envs)
	}
	if strings.Join(envs[0].Stacks, ",") != "blog" || strings.Join(envs[1].Stacks, ",") != "shop" {
		t.Errorf("stacks: %v / %v", envs[0].Stacks, envs[1].Stacks)
	}
	if strings.Join(envs[1].Secrets, ",") != "DB_PASSWORD" || envs[1].Variables["MODE"] != "fast" {
		t.Errorf("values: %+v", envs[1])
	}
	if err := p.CreateEnvironment("staging"); err == nil {
		t.Error("created a duplicate")
	}
	if err := p.CreateEnvironment("Bad Name"); err == nil {
		t.Error("accepted a bad name")
	}
}

func TestReferenceEdges(t *testing.T) {
	p := envPlatform(t)
	plan := p.PlanStackIn("shop", "staging", []byte(refStack))
	var got []string
	for _, e := range plan.Edges {
		got = append(got, e.From+">"+e.To+":"+e.Kind)
	}
	// api reaches db through its host; web reaches api inside the stack too;
	// the browser links (services.web.url, services.api.url) draw nothing.
	want := "api>db:uses,web>api:uses"
	if strings.Join(got, ",") != want {
		t.Errorf("edges = %v, want %s", got, want)
	}
}
