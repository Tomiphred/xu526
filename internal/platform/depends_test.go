package platform

import (
	"context"
	"strings"
	"testing"
	"time"
)

// frontend → backend → db, all scaling to zero. A cold start of frontend
// wakes backend and db first; backend and db stay up while frontend runs,
// and go to zero after it does.
func TestDependsOnWakesAndKeepsDependencies(t *testing.T) {
	p, srv := newTestPlatform(t)
	ctx := context.Background()
	zero := func(name string, deps ...string) AppSpec {
		s := AppSpec{Name: name, Command: []string{helloBin}, MinScale: 0, MaxScale: 3, IdleTimeoutSec: 1, ScaleDownSec: 1}
		for _, d := range deps {
			s.DependsOn = append(s.DependsOn, Dependency{App: d})
		}
		return s
	}
	for _, s := range []AppSpec{zero("db"), zero("backend", "db"), zero("frontend", "backend")} {
		if err := p.Deploy(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	// Keep requesting frontend only: backend and db get no requests, but
	// must stay up while frontend runs.
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(200 * time.Millisecond):
				get(t, srv, "frontend.test", "/")
			}
		}
	}()
	time.Sleep(3 * time.Second) // well past backend's and db's 1s idle timeout
	if running(p, "backend") == 0 || running(p, "db") == 0 {
		close(stop)
		t.Fatalf("a dependency went to zero while frontend runs: backend %d, db %d", running(p, "backend"), running(p, "db"))
	}
	close(stop)

	waitFor(t, "all three to scale to zero", 15*time.Second, func() bool {
		return running(p, "frontend") == 0 && running(p, "backend") == 0 && running(p, "db") == 0
	})

	// Cold start: the request to frontend brings up db, then backend, then frontend.
	if _, code := get(t, srv, "frontend.test", "/"); code != 200 {
		t.Fatalf("cold start: status %d", code)
	}
	if running(p, "backend") == 0 || running(p, "db") == 0 {
		t.Fatalf("frontend started before what it depends on: backend %d, db %d", running(p, "backend"), running(p, "db"))
	}
	be, _ := p.Status("backend")
	fe, _ := p.Status("frontend")
	if len(be.Instances) == 0 || len(fe.Instances) == 0 {
		t.Fatal("no instances")
	}
}

// A dependency that can't start stops the app from starting, with a reason;
// an optional one (required: false) doesn't.
func TestDependsOnFailure(t *testing.T) {
	p, _ := newTestPlatform(t)
	ctx := context.Background()
	err := p.Deploy(ctx, AppSpec{Name: "web", Command: []string{helloBin}, DependsOn: []Dependency{{App: "missing"}}})
	if err == nil || !strings.Contains(err.Error(), "missing: it isn't deployed") {
		t.Fatalf("missing dependency: %v", err)
	}
	if err := p.Deploy(ctx, AppSpec{Name: "web2", Command: []string{helloBin}, DependsOn: []Dependency{{App: "missing", Optional: true}}}); err != nil {
		t.Fatalf("optional dependency: %v", err)
	}
}

func TestComposeDependsOnConditions(t *testing.T) {
	p := envPlatform(t)
	plan := p.PlanStackIn("shop", "", []byte(`
services:
  web:
    image: web
    ports: ["80"]
    depends_on:
      api: {condition: service_healthy}
      cache: {condition: service_started, required: false}
      migrate: {condition: service_completed_successfully}
  api:
    image: api
    depends_on: [db]
  db:
    image: postgres
  cache:
    image: redis
  migrate:
    image: api
`))
	if len(plan.Errors) > 0 {
		t.Fatal(plan.Errors)
	}
	var web, api AppSpec
	for _, s := range plan.Services {
		switch s.Name {
		case "web":
			web = s.Spec
		case "api":
			api = s.Spec
		}
	}
	want := []Dependency{{App: "shop-api"}, {App: "shop-cache", Optional: true}, {App: "shop-migrate"}}
	if len(web.DependsOn) != 3 || web.DependsOn[0] != want[0] || web.DependsOn[1] != want[1] || web.DependsOn[2] != want[2] {
		t.Errorf("web depends on %+v", web.DependsOn)
	}
	if len(api.DependsOn) != 1 || api.DependsOn[0].App != "shop-db" {
		t.Errorf("api depends on %+v", api.DependsOn)
	}
	if !strings.Contains(strings.Join(plan.Warnings, "\n"), "service_completed_successfully is treated as service_healthy") {
		t.Errorf("warnings %v", plan.Warnings)
	}
}

// Connections found in settings count too, and loops are broken in favour
// of databases, then APIs, then front ends.
func TestPlanDepsFromSettings(t *testing.T) {
	p := envPlatform(t)
	plan := p.PlanStackIn("dex", "", []byte(`
services:
  frontend:
    image: web
    ports: ["3000"]
    environment:
      API_URL: http://backend:8000
  backend:
    image: api
    ports: ["8000"]
    environment:
      DATABASE_URL: postgres://u:p@db:5432/app
      CORS_ORIGINS: http://frontend:3000
  db:
    image: postgres
`))
	if len(plan.Errors) > 0 {
		t.Fatal(plan.Errors)
	}
	deps := planDeps(plan)
	if len(deps["dex-frontend"]) != 1 || deps["dex-frontend"][0].App != "dex-backend" {
		t.Errorf("frontend needs %+v", deps["dex-frontend"])
	}
	if len(deps["dex-backend"]) != 1 || deps["dex-backend"][0].App != "dex-db" {
		t.Errorf("backend needs %+v (no loop back to frontend)", deps["dex-backend"])
	}
	if len(deps["dex-db"]) != 0 {
		t.Errorf("db needs %+v", deps["dex-db"])
	}
}
