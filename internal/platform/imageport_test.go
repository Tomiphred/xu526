package platform

import (
	"strings"
	"testing"
)

func TestImageRepo(t *testing.T) {
	for in, want := range map[string]string{
		"postgres": "postgres", "postgres:17-alpine": "postgres", "docker.io/library/redis:7": "redis",
		"ghcr.io/me/app:v1": "me/app", "localhost:5000/x@sha256:abc": "x", "postgis/postgis:16-3.4": "postgis/postgis",
	} {
		if got := imageRepo(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

// Official images without ports: get their port; a public one whose port
// isn't known yet waits for deploy instead of failing.
func TestComposePortFromImage(t *testing.T) {
	p := envPlatform(t)
	plan := p.PlanStackIn("shop", "", []byte(`
services:
  db:
    image: postgres:17
    x-xu: {public: true}
  cache:
    image: redis:7-alpine
  admin:
    image: someone/unknown-admin
    x-xu: {public: true}
  worker:
    image: someone/unknown-admin
  api:
    image: myapi
    environment:
      DB_PORT: ${{ services.db.port }}
`))
	if len(plan.Errors) > 0 {
		t.Fatal(plan.Errors)
	}
	got := map[string]PlanService{}
	for _, s := range plan.Services {
		got[s.Name] = s
	}
	if got["db"].Port != 5432 || !got["db"].Public || got["cache"].Port != 6379 || got["admin"].Port != 0 || got["api"].Spec.Env["DB_PORT"] != "5432" {
		t.Errorf("ports: db %d cache %d admin %d api %v", got["db"].Port, got["cache"].Port, got["admin"].Port, got["api"].Spec.Env)
	}
	if !strings.Contains(strings.Join(plan.Notes, "\n"), "admin's port is read from its image") {
		t.Errorf("notes: %v", plan.Notes)
	}
}
