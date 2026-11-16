// Package golang builds Go projects into a single static binary on scratch.
package golang

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"xu526/internal/bundle"
)

// Plugin is the Go plugin.
type Plugin struct{}

func (Plugin) Name() string    { return "go" }
func (Plugin) Version() string { return "1" }

// caBundle is copied into the image so HTTPS calls work on scratch.
var caBundle = bundle.HostCABundle()

const work = "$WORK" // replaced by the build directory at build time

// Detect claims projects with a go.mod and records every main package.
func (Plugin) Detect(src *bundle.Source) (*bundle.Detection, error) {
	if !src.Has("go.mod") {
		return nil, nil
	}
	mod, _ := os.ReadFile(filepath.Join(src.Dir, "go.mod"))
	module := ""
	for _, l := range strings.Split(string(mod), "\n") {
		if m, ok := strings.CutPrefix(strings.TrimSpace(l), "module "); ok {
			module = strings.Trim(strings.TrimSpace(m), `"`)
		}
	}
	mains, err := findMains(src)
	if err != nil {
		return nil, err
	}
	return &bundle.Detection{Confidence: 100, Markers: []string{"go.mod"},
		Details: map[string]string{"module": module, "mains": strings.Join(mains, ",")}}, nil
}

// findMains lists directories containing package main, as "./dir" paths.
func findMains(src *bundle.Source) ([]string, error) {
	var mains []string
	fset := token.NewFileSet()
	err := fs.WalkDir(src.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		base := d.Name()
		if p != "." && (strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_") || base == "vendor" || base == "testdata" || base == "node_modules") {
			return fs.SkipDir
		}
		if p != "." && src.Has(path.Join(p, "go.mod")) {
			return fs.SkipDir // a nested module is its own project
		}
		entries, _ := fs.ReadDir(src.FS, p)
		for _, e := range entries {
			n := e.Name()
			if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, filepath.Join(src.Dir, p, n), nil, parser.PackageClauseOnly)
			if err == nil && f.Name.Name == "main" {
				mains = append(mains, "./"+p)
				break
			}
		}
		return nil
	})
	sort.Strings(mains)
	for i, m := range mains {
		if m == "./." {
			mains[i] = "."
		}
	}
	return mains, err
}

// chooseMain picks the package to build.
func chooseMain(module string, mains []string, override string) (string, error) {
	if override != "" {
		o := "./" + strings.TrimPrefix(filepath.ToSlash(filepath.Clean(override)), "./")
		if o == "./." {
			o = "."
		}
		for _, m := range mains {
			if m == o {
				return m, nil
			}
		}
		return "", fmt.Errorf("%s is not a main package (found: %s)", override, strings.Join(mains, ", "))
	}
	switch {
	case len(mains) == 0:
		return "", fmt.Errorf("no main package found: add a package main with func main()")
	case len(mains) == 1:
		return mains[0], nil
	}
	for _, m := range mains {
		if m == "." {
			return m, nil
		}
	}
	for _, m := range mains { // cmd/<module name> is the usual convention
		if m == "./cmd/"+path.Base(module) {
			return m, nil
		}
	}
	return "", fmt.Errorf("found several programs (%s); choose one with --main", strings.Join(mains, ", "))
}

func goVersion() (string, error) {
	out, err := exec.Command("go", "env", "GOVERSION").Output()
	if err != nil {
		return "", fmt.Errorf("Go isn't installed or isn't on PATH (https://go.dev/dl)")
	}
	return strings.TrimSpace(string(out)), nil
}

func fileHash(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (Plugin) Plan(src *bundle.Source, d *bundle.Detection, opt bundle.Options) (*bundle.Plan, error) {
	var mains []string
	if d.Details["mains"] != "" {
		mains = strings.Split(d.Details["mains"], ",")
	}
	main, err := chooseMain(d.Details["module"], mains, opt.Main)
	if err != nil {
		return nil, err
	}
	ver, err := goVersion()
	if err != nil {
		return nil, err
	}
	env := []string{"CGO_ENABLED=0", "GOOS=" + opt.Platform.OS, "GOARCH=" + opt.Platform.Arch}
	modeNote := "dependencies from go.sum via the module cache"
	if src.Has("vendor/modules.txt") {
		env = append(env, "GOFLAGS=-mod=vendor")
		modeNote = "dependencies from vendor/"
	}
	cmd := []string{"go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-s -w -buildid=", "-o", work + "/app", main}
	p := &bundle.Plan{
		Strategy: bundle.StaticBinary, Base: "scratch",
		Steps:      []bundle.Step{{Name: "compile", Cmd: cmd, Dir: src.Dir, Env: env, Inputs: []string{"**/*"}}},
		Entrypoint: []string{"/app"},
		Env:        map[string]string{"PORT": "8080"},
		User:       "65532:65532",
		Port:       8080,
		Summary: []string{
			fmt.Sprintf("build %s with %s as a static binary (CGO_ENABLED=0)", main, ver),
			modeNote,
			"reproducible: -trimpath, no build ID, no VCS stamping",
			"runs as user 65532 (non-root); listens on $PORT (8080 by default)",
		},
	}
	if ca := fileHash(caBundle); ca != "" {
		p.Layers = append(p.Layers, bundle.LayerPlan{Name: "certs", Salt: ca})
		p.Summary = append(p.Summary, "includes CA certificates so HTTPS calls work")
	}
	p.Layers = append(p.Layers, bundle.LayerPlan{Name: "app", Inputs: []string{"**/*"}, Salt: ver + "|" + main})
	return p, nil
}

func (Plugin) Build(ctx context.Context, env *bundle.BuildEnv, p *bundle.Plan) (*bundle.Output, error) {
	for _, s := range p.Steps {
		for i, a := range s.Cmd {
			s.Cmd[i] = strings.ReplaceAll(a, work, env.WorkDir)
		}
		if err := env.Runner.Run(ctx, s, env.Log); err != nil {
			return nil, err
		}
	}
	bin := filepath.Join(env.WorkDir, "app")
	if _, err := os.Stat(bin); err != nil {
		return nil, fmt.Errorf("go build finished but produced no binary")
	}
	return &bundle.Output{Files: map[string]string{"app": bin}}, nil
}

func (Plugin) Layers(p *bundle.Plan, out *bundle.Output) ([]bundle.Layer, error) {
	layers := []bundle.Layer{{Name: "app", Files: []bundle.Entry{{Src: out.Files["app"], Dst: "/app", Mode: 0o755}}}}
	for _, lp := range p.Layers {
		if lp.Name == "certs" {
			layers = append(layers, bundle.Layer{Name: "certs", Files: []bundle.Entry{{Src: caBundle, Dst: bundle.ImageCABundle, Mode: 0o644}}})
		}
	}
	return layers, nil
}
