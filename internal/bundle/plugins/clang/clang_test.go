package clang

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"xu526/internal/bundle"
	"xu526/internal/bundle/cache"
	"xu526/internal/bundle/oci"
	"xu526/internal/bundle/runner"
)

func project(t *testing.T, files map[string]string) *bundle.Source {
	t.Helper()
	d := t.TempDir()
	for p, s := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(d, p)), 0o755)
		os.WriteFile(filepath.Join(d, p), []byte(s), 0o644)
	}
	src, err := bundle.NewSource(d)
	if err != nil {
		t.Fatal(err)
	}
	return src
}

const mainC = "#include <stdio.h>\nint main(void) { puts(\"hi\"); return 0; }\n"

func TestDetect(t *testing.T) {
	cases := []struct {
		name   string
		files  map[string]string
		system string // "" = not claimed
	}{
		{"plain", map[string]string{"main.c": mainC, "util.c": "int f(void){return 1;}"}, plain},
		{"makefile", map[string]string{"Makefile": "all:", "src/main.c": mainC}, make_},
		{"makefile without C", map[string]string{"Makefile": "all:\n\techo hi"}, ""},
		{"cmake C", map[string]string{"CMakeLists.txt": "project(app C)", "main.c": mainC}, cmake},
		{"cmake default languages", map[string]string{"CMakeLists.txt": "project(app)\nadd_executable(app main.c)"}, cmake},
		{"cmake C++ only", map[string]string{"CMakeLists.txt": "project(app LANGUAGES CXX)", "main.cpp": ""}, ""},
		{"autotools", map[string]string{"configure.ac": "", "main.c": mainC}, "autotools"},
		{"nothing", map[string]string{"README": ""}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, err := Plugin{}.Detect(project(t, c.files))
			if err != nil {
				t.Fatal(err)
			}
			if c.system == "" {
				if d != nil {
					t.Fatalf("claimed: %+v", d)
				}
				return
			}
			if d == nil || d.Details["system"] != c.system {
				t.Fatalf("got %+v, want %s", d, c.system)
			}
		})
	}
}

func TestPlainSources(t *testing.T) {
	src := project(t, map[string]string{"src/main.c": mainC, "src/lib.c": "int f;", "tests/test_lib.c": mainC, "lib_test.c": mainC, "tools/gen.c": mainC})
	if _, err := plainSources(src, ""); err == nil || !strings.Contains(err.Error(), "--main") {
		t.Fatalf("two mains (src/main.c, tools/gen.c) should ask for --main: %v", err)
	}
	files, err := plainSources(src, "src/main.c")
	if err != nil || strings.Join(files, " ") != "src/main.c src/lib.c" {
		t.Fatalf("got %v %v", files, err)
	}
	if _, err := plainSources(src, "src/lib.c"); err == nil {
		t.Fatal("a file without main accepted as --main")
	}
}

func TestCheckBinary(t *testing.T) {
	if _, err := os.Stat("/bin/ls"); err == nil {
		if err := checkBinary("/bin/ls", "amd64"); err == nil || !strings.Contains(err.Error(), "dynamically linked") && !strings.Contains(err.Error(), "built for") {
			t.Fatalf("/bin/ls should be refused: %v", err)
		}
	}
	p := filepath.Join(t.TempDir(), "x")
	os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755)
	if err := checkBinary(p, "amd64"); err == nil {
		t.Fatal("a script accepted as a program")
	}
}

func build(t *testing.T, src *bundle.Source, arch string, store *cache.Store, noCache bool) *bundle.Result {
	t.Helper()
	var log bytes.Buffer
	res, err := bundle.Build(context.Background(), bundle.Request{Dir: src.Dir, Tag: "c", Plugins: []bundle.Plugin{Plugin{}},
		Cache: store, NoCache: noCache, Runner: runner.Local{}, Log: &log,
		Options: bundle.Options{Platform: bundle.Platform{OS: "linux", Arch: arch}}})
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	return res
}

func needZig(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles C")
	}
	if _, _, err := zig(); err != nil {
		t.Skip("zig not installed")
	}
}

// hasApp checks the image holds an executable /app.
func hasApp(t *testing.T, res *bundle.Result) {
	t.Helper()
	for _, l := range res.Image.Layers {
		h, _ := oci.ReadLayerFiles(l.Blob)
		if a := h["app"]; a != nil && a.Mode&0o111 != 0 && a.Size > 0 {
			return
		}
	}
	t.Fatal("no executable /app in the image")
}

func TestBuildPlainReproducible(t *testing.T) {
	needZig(t)
	files := map[string]string{"main.c": "#include <stdio.h>\n#include <assert.h>\n#include \"util.h\"\nint main(void){ assert(twice(2)==4); printf(\"%s %s\\n\", __FILE__, __DATE__); return 0; }\n",
		"util.c": "#include \"util.h\"\nint twice(int x){return 2*x;}\n", "util.h": "int twice(int);\n", "main.o": "stale host object"}
	store, _ := cache.Open(t.TempDir())
	a := build(t, project(t, files), "amd64", store, false)
	b := build(t, project(t, files), "amd64", store, true) // another folder, no cache
	if a.Image.Digest() != b.Image.Digest() {
		t.Fatal("same sources in different folders gave different images")
	}
	hasApp(t, a)
	arm := build(t, project(t, files), "arm64", store, false)
	if arm.Image.Digest() == a.Image.Digest() {
		t.Fatal("arm64 build should differ")
	}
}

func TestBuildCMake(t *testing.T) {
	needZig(t)
	if _, err := exec.LookPath("cmake"); err != nil {
		t.Skip("cmake not installed")
	}
	src := project(t, map[string]string{
		"CMakeLists.txt": "cmake_minimum_required(VERSION 3.10)\nproject(calc C)\nadd_library(mathx STATIC lib/mathx.c)\nadd_executable(calc main.c)\ntarget_link_libraries(calc mathx m)\n",
		"main.c":         "#include <stdio.h>\n#include <math.h>\nint sq(int);\nint main(void){ printf(\"%d %.1f\\n\", sq(3), sqrt(16.0)); return 0; }\n",
		"lib/mathx.c":    "int sq(int x){return x*x;}\n",
	})
	res := build(t, src, "amd64", nil, false)
	if res.Plan.Strategy != bundle.StaticBinary {
		t.Fatalf("strategy %s", res.Plan.Strategy)
	}
	hasApp(t, res)
}
