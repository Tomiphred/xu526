package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const realCompose = `
services:
  db:
    image: postgres:${PG_VERSION:-16}
    env_file: .env.db
    volumes: [pg:/var/lib/postgresql/data]
  api:
    build: ./backend
    ports: ["${API_PORT}:8000"]
    env_file:
      - ./backend/.env
      - path: ./optional.env
        required: false
    environment:
      DEBUG: "false"
      DATABASE_URL: postgres://app:${DB_PASSWORD}@db:5432/app
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:8000/api/health/"]
      interval: 10s
    entrypoint: ["/docker-entrypoint.sh"]
    working_dir: /app
    user: "1000"
    restart: unless-stopped
    container_name: shop-api
    depends_on:
      db: {condition: service_healthy}
  celery:
    build: ./backend
    command: celery -A config worker
    env_file: ./backend/.env
    depends_on: [db]
  adminer:
    image: adminer
    profiles: [debug]
    ports: ["8081:8080"]
volumes:
  pg:
`

func TestDockerComposeFile(t *testing.T) {
	p := envPlatform(t)
	files := map[string]string{
		".env":         "API_PORT=8000\nDB_PASSWORD=from-dotenv\n",
		".env.db":      "POSTGRES_PASSWORD=from-dotenv\nPOSTGRES_USER=app\n",
		"backend/.env": "SECRET_KEY=s3cret\nDEBUG=true\n",
	}
	plan := p.planStack(stackTarget{Stack: "shop", Env: "production", Files: files}, []byte(realCompose), nil)
	if len(plan.Errors) > 0 {
		t.Fatalf("errors: %v", plan.Errors)
	}
	if svc(plan, "adminer").Name != "" {
		t.Error("a service in a profile was deployed")
	}
	db, api, celery := svc(plan, "db"), svc(plan, "api"), svc(plan, "celery")
	if db.Spec.Image != "postgres:16" || db.Spec.Env["POSTGRES_PASSWORD"] != "from-dotenv" {
		t.Errorf("db: %s %v", db.Spec.Image, db.Spec.Env)
	}
	e := api.Spec.Env
	if e["DEBUG"] != "false" || e["SECRET_KEY"] != "s3cret" || e["DATABASE_URL"] != "postgres://app:from-dotenv@db:5432/app" {
		t.Errorf("api env %v (environment: wins over env_file; .env fills ${VARS})", e)
	}
	if strings.Join(api.Spec.SecretKeys, ",") != "SECRET_KEY" {
		t.Errorf("env_file values should be masked: %v", api.Spec.SecretKeys)
	}
	if api.Spec.HealthPath != "/api/health/" || api.Spec.Port != 8000 || strings.Join(api.Spec.Entrypoint, " ") != "/docker-entrypoint.sh" ||
		api.Spec.WorkingDir != "/app" || api.Spec.User != "1000" {
		t.Errorf("api spec %+v", api.Spec)
	}
	if celery.Kind != "worker" || api.Kind != "api" || db.Kind != "database" {
		t.Errorf("kinds: celery=%s api=%s db=%s", celery.Kind, api.Kind, db.Kind)
	}
	for _, w := range plan.Warnings {
		if strings.Contains(w, "healthcheck") || strings.Contains(w, "env_file") {
			t.Errorf("unexpected warning: %s", w)
		}
	}

	// Pasted (no files): env_file can't be read.
	pasted := p.PlanStack("shop", []byte(realCompose))
	if !strings.Contains(strings.Join(pasted.Errors, " "), "can't be read from a pasted file") {
		t.Errorf("pasted: %v", pasted.Errors)
	}
}

func TestStackFiles(t *testing.T) {
	dir := t.TempDir()
	for f, body := range map[string]string{".env": "A=1", ".env.db": "B=2", "backend/.env": "C=3", "secret.txt": "nope"} {
		os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0o755)
		os.WriteFile(filepath.Join(dir, f), []byte(body), 0o644)
	}
	got := StackFiles(dir, []byte(realCompose+"\n  evil:\n    image: x\n    env_file: ../../etc/passwd\n"))
	if len(got) != 3 || got[".env"] != "A=1" || got["backend/.env"] != "C=3" || got[".env.db"] != "B=2" {
		t.Errorf("files = %v", got)
	}
}
