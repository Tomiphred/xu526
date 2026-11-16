package platform

import (
	"strings"
	"testing"

	"xu526/third_party/yaml"
)

const netStack = `
services:
  web:
    image: nginx
    ports: ["8080:80"]
    networks: [front]
    environment:
      API_URL: http://api:8000
      DB_URL: postgres://db:5432/app
  api:
    image: api
    expose: ["8000"]
    networks:
      front:
      back:
        aliases: [backend]
  db:
    image: postgres
    networks: [back]
  tool:
    image: busybox
networks:
  front:
  back:
    internal: true
  shared:
    external: true
    name: proxy-net
`

func TestStackNetworks(t *testing.T) {
	p := envPlatform(t)
	plan := p.PlanStackIn("shop", "staging", []byte(netStack))
	if len(plan.Errors) > 0 {
		t.Fatalf("errors: %v", plan.Errors)
	}
	svc := map[string]PlanService{}
	for _, s := range plan.Services {
		svc[s.Name] = s
	}
	api := svc["api"].Spec
	if api.Network != "xu-shop-staging-front" || len(api.Networks) != 2 || api.Networks[1].Name != "xu-shop-staging-back" ||
		!api.Networks[1].Internal || strings.Join(api.Networks[1].Aliases, ",") != "api,backend" {
		t.Errorf("api networks: %s %+v", api.Network, api.Networks)
	}
	if tool := svc["tool"].Spec; tool.Network != "xu-shop-staging" || len(tool.Networks) != 1 {
		t.Errorf("a service with no networks is on default: %s %+v", tool.Network, tool.Networks)
	}
	if strings.Join(svc["web"].Networks, ",") != "front" {
		t.Errorf("web: %v", svc["web"].Networks)
	}
	warned := strings.Join(plan.Warnings, "\n")
	if !strings.Contains(warned, "web can't reach db") || strings.Contains(warned, "web can't reach api") {
		t.Errorf("warnings: %s", warned)
	}
	if !strings.Contains(strings.Join(plan.Notes, "\n"), "back (internal: no internet)") {
		t.Errorf("notes: %v", plan.Notes)
	}

	// An unknown network is an error, as with docker compose.
	bad := p.PlanStackIn("shop", "", []byte("services:\n  a:\n    image: x\n    networks: [nope]\n"))
	if !strings.Contains(strings.Join(bad.Errors, "\n"), `network "nope" isn't declared`) {
		t.Errorf("errors: %v", bad.Errors)
	}
	// No networks: one network for the stack, as before.
	plain := p.PlanStackIn("shop", "", []byte("services:\n  a:\n    image: x\n"))
	if s := plain.Services[0].Spec; s.Network != "xu-shop" || s.Networks != nil {
		t.Errorf("plain: %+v", s)
	}
	if got := stackNetworkNames("shop-staging", []AppSpec{api, {Networks: []NetworkRef{{Name: "proxy-net", External: true}}}}); strings.Join(got, ",") != "xu-shop-staging,xu-shop-staging-back,xu-shop-staging-front" {
		t.Errorf("networks removed with the stack: %v", got)
	}
}

func TestComposeBuildFile(t *testing.T) {
	p := envPlatform(t)
	src := `
services:
  api:
    build:
      context: ./api
      dockerfile: Dockerfile.prod
      target: release
      args:
        PRICE: "$$5"
      secrets: [npmrc]
      ssh: [default]
      additional_contexts:
        shared: ../shared
  web:
    build: ./web
  db:
    image: postgres
secrets:
  npmrc:
    file: ./.npmrc
`
	plan := p.PlanStackIn("shop", "", []byte(src))
	if len(plan.Errors) > 0 {
		t.Fatal(plan.Errors)
	}
	file, refs, err := ComposeBuildFile(plan, "/src/shop", "build", []string{"api"})
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Services map[string]struct {
			Image string         `yaml:"image"`
			Build map[string]any `yaml:"build"`
		} `yaml:"services"`
		Secrets map[string]any `yaml:"secrets"`
	}
	if err := yaml.Unmarshal(file, &f); err != nil {
		t.Fatalf("%v\n%s", err, file)
	}
	api := f.Services["api"]
	if len(f.Services) != 1 || refs["api"] != "xu/shop-api:build" || api.Image != refs["api"] ||
		api.Build["context"] != "/src/shop/api" || api.Build["dockerfile"] != "Dockerfile.prod" || api.Build["target"] != "release" ||
		api.Build["args"].(map[string]any)["PRICE"] != "$$5" || api.Build["ssh"] == nil || api.Build["additional_contexts"] == nil ||
		f.Secrets["npmrc"] == nil {
		t.Errorf("build file:\n%s", file)
	}
}
