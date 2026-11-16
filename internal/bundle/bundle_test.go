package bundle

import (
	"context"
	"io"
	"strings"
	"testing"

	"xu526/internal/bundle/cache"
)

type fake struct {
	name string
	conf int
}

func (f fake) Name() string    { return f.name }
func (f fake) Version() string { return "0" }
func (f fake) Detect(*Source) (*Detection, error) {
	if f.conf == 0 {
		return nil, nil
	}
	return &Detection{Confidence: f.conf, Markers: []string{f.name}}, nil
}
func (f fake) Plan(*Source, *Detection, Options) (*Plan, error) {
	return &Plan{Strategy: StaticAssets, Base: "scratch", Layers: []LayerPlan{{Name: "site"}}}, nil
}
func (f fake) Build(context.Context, *BuildEnv, *Plan) (*Output, error) { return &Output{}, nil }
func (f fake) Layers(*Plan, *Output) ([]Layer, error) {
	return []Layer{{Name: "site", Files: []Entry{{Data: []byte("x"), Dst: "/index.html"}}}}, nil
}

func TestDetectChoosesHighestConfidence(t *testing.T) {
	src, _ := NewSource(t.TempDir())
	p, _, err := Detect(src, []Plugin{fake{"a", 10}, fake{"b", 90}, fake{"c", 0}}, "")
	if err != nil || p.Name() != "b" {
		t.Fatalf("got %v %v", p, err)
	}
	if _, _, err := Detect(src, []Plugin{fake{"a", 50}, fake{"b", 50}}, ""); err == nil || !strings.Contains(err.Error(), "--lang") {
		t.Fatalf("tie should ask for --lang: %v", err)
	}
	if p, _, _ := Detect(src, []Plugin{fake{"a", 10}, fake{"b", 90}}, "a"); p.Name() != "a" {
		t.Fatal("--lang ignored")
	}
	if _, _, err := Detect(src, []Plugin{fake{"a", 0}}, ""); err == nil {
		t.Fatal("no match should fail")
	}
}

func TestBuildWithoutCache(t *testing.T) {
	res, err := Build(context.Background(), Request{Dir: t.TempDir(), Plugins: []Plugin{fake{"a", 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Layers) != 1 || res.Image.Digest() == "" || res.Image.Arch != "amd64" {
		t.Fatalf("%+v", res)
	}
	if _, err := ParsePlatform("windows/amd64"); err == nil {
		t.Fatal("bad platform accepted")
	}
}

// envFake runs one step and records the environment it was given.
type envFake struct{ fake }

func (envFake) Build(ctx context.Context, env *BuildEnv, p *Plan) (*Output, error) {
	return &Output{}, env.Runner.Run(ctx, Step{Name: "build", Cmd: []string{"x"}, Env: []string{"OWN=1"}}, io.Discard)
}

type recordRunner struct{ got *[]string }

func (r recordRunner) Run(_ context.Context, s Step, _ io.Writer) error { *r.got = s.Env; return nil }

func TestBuildEnvReachesStepsAndCache(t *testing.T) {
	dir := t.TempDir()
	store, _ := cache.Open(t.TempDir())
	var got []string
	build := func(env ...string) bool {
		res, err := Build(context.Background(), Request{Dir: dir, Plugins: []Plugin{envFake{fake{"a", 1}}}, Cache: store,
			Runner: recordRunner{&got}, Log: io.Discard, Options: Options{BuildEnv: env}})
		if err != nil {
			t.Fatal(err)
		}
		return res.Ran
	}
	if !build("NEXT_PUBLIC_API_URL=https://a") || strings.Join(got, " ") != "NEXT_PUBLIC_API_URL=https://a OWN=1" {
		t.Fatalf("step env = %v", got)
	}
	if build("NEXT_PUBLIC_API_URL=https://a") {
		t.Error("same values rebuilt instead of using the cache")
	}
	if !build("NEXT_PUBLIC_API_URL=https://b") {
		t.Error("a changed value came from the cache")
	}
}
