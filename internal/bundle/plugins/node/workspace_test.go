package node

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"xu526/internal/bundle"
	"xu526/internal/bundle/cache"
	"xu526/internal/bundle/oci"
	"xu526/internal/bundle/registry"
	"xu526/internal/bundle/registry/regtest"
	"xu526/internal/bundle/runner"
)

func TestYAMLList(t *testing.T) {
	doc := "# pnpm\npackages:\n  - 'apps/*'\n  - \"packages/**\" # all\n  - '!**/test/**'\ncatalog:\n  react: ^19\n"
	got := yamlList(doc, "packages")
	want := []string{"apps/*", "packages/**", "!**/test/**"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestWorkspaceGlobs(t *testing.T) {
	globs := []string{"apps/*", "packages/**", "tools/cli", "!packages/legacy"}
	for rel, want := range map[string]bool{
		"apps/web": true, "apps/web/src": false, "packages/ui": true, "packages/a/b": true,
		"tools/cli": true, "tools/other": false, "packages/legacy": false, "docs": false,
	} {
		if got := matchesAny(globs, rel); got != want {
			t.Errorf("%s: got %v", rel, got)
		}
	}
}

// monorepo writes a workspace for pm: apps/api uses packages/util (which
// has a build step) at runtime and packages/devtool only in development;
// apps/other is unrelated. No registry packages, so it installs offline.
func monorepo(t *testing.T, pm string) string {
	t.Helper()
	spec := "*"
	if pm == "pnpm" {
		spec = "workspace:*"
	}
	root := t.TempDir()
	files := map[string]string{
		"apps/api/package.json": `{"name":"api","version":"1.0.0","private":true,"scripts":{"start":"node index.js"},
			"dependencies":{"@repo/util":"` + spec + `"},"devDependencies":{"@repo/devtool":"` + spec + `"}}`,
		"apps/api/index.js":             `console.log("api says " + require("@repo/util").hello())`,
		"apps/api/.env":                 "GREETING=hi\n",
		"apps/other/package.json":       `{"name":"other","version":"1.0.0","private":true}`,
		"apps/other/index.js":           `console.log("other")`,
		"packages/util/package.json":    `{"name":"@repo/util","version":"1.0.0","main":"dist/index.js","scripts":{"build":"node build.js"}}`,
		"packages/util/src.js":          `exports.hello = () => "hello from util"`,
		"packages/util/build.js":        `require("fs").mkdirSync("dist",{recursive:true}); require("fs").copyFileSync("src.js","dist/index.js")`,
		"packages/devtool/package.json": `{"name":"@repo/devtool","version":"1.0.0"}`,
		"README.md":                     "monorepo",
	}
	switch pm {
	case "pnpm":
		files["package.json"] = `{"name":"root","private":true}`
		files["pnpm-workspace.yaml"] = "packages:\n  - 'apps/*'\n  - 'packages/*'\n"
	default:
		files["package.json"] = `{"name":"root","private":true,"workspaces":["apps/*","packages/*"]}`
	}
	for p, s := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755)
		os.WriteFile(filepath.Join(root, p), []byte(s), 0o644)
	}
	var cmd *exec.Cmd
	switch pm {
	case "npm":
		cmd = exec.Command("npm", "install", "--package-lock-only", "--no-audit", "--no-fund", "--offline")
	case "pnpm":
		cmd = exec.Command("pnpm", "install", "--lockfile-only", "--offline")
	case "yarn":
		cmd = exec.Command("yarn", "install", "--offline", "--silent")
	}
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("%s can't make a lockfile here: %v\n%s", pm, err, out)
	}
	os.RemoveAll(filepath.Join(root, "node_modules")) // yarn installs; builds start clean
	return root
}

func TestWorkspaceRootAndClosure(t *testing.T) {
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not installed")
	}
	root := monorepo(t, "npm")
	got, ok := Plugin{}.WorkspaceRoot(filepath.Join(root, "apps", "api"))
	if !ok || got != root {
		t.Fatalf("WorkspaceRoot = %q, %v", got, ok)
	}
	if _, ok := (Plugin{}).WorkspaceRoot(root); ok {
		t.Error("the root itself isn't an app of the workspace")
	}
	ws, err := loadWorkspace(root, "apps/api")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ws.Local, []string{"packages/devtool", "packages/util"}) || !reflect.DeepEqual(ws.LocalProd, []string{"packages/util"}) {
		t.Fatalf("local %v, prod %v", ws.Local, ws.LocalProd)
	}
	if !reflect.DeepEqual(ws.excluded(), []string{"apps/other"}) {
		t.Fatalf("excluded %v", ws.excluded())
	}
}

// extract unpacks the image's layers into a folder, the way a container
// runtime would.
func extract(t *testing.T, im *oci.Image) string {
	t.Helper()
	dir := t.TempDir()
	for _, l := range im.Layers {
		zr, err := gzip.NewReader(bytes.NewReader(l.Blob))
		if err != nil {
			t.Fatal(err)
		}
		tr := tar.NewReader(zr)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(dir, filepath.FromSlash(h.Name))
			os.MkdirAll(filepath.Dir(p), 0o755)
			switch h.Typeflag {
			case tar.TypeDir:
				os.MkdirAll(p, 0o755)
			case tar.TypeSymlink:
				os.Symlink(h.Linkname, p)
			case tar.TypeReg:
				b, _ := io.ReadAll(tr)
				os.WriteFile(p, b, os.FileMode(h.Mode))
			}
		}
	}
	return dir
}

func fakeNodeBase(t *testing.T, major int) *regtest.Registry {
	t.Helper()
	reg := regtest.New()
	base, _ := oci.BuildLayer([]oci.File{{Data: []byte("node"), Dst: "/nodejs/bin/node", Mode: 0o755}}, 0)
	im := &oci.Image{OS: "linux", Arch: "amd64", Layers: []*oci.Layer{base}, Config: oci.Config{Entrypoint: []string{"/nodejs/bin/node"}}}
	im.Seal()
	reg.PutBlob(base.Blob)
	reg.PutBlob(im.ConfigJSON())
	reg.PutManifest(strings.TrimPrefix(strings.SplitN(baseFor(major), ":", 2)[0], "gcr.io/"), "nonroot", oci.MediaManifest, im.Manifest())
	return reg
}

func TestMonorepoBuild(t *testing.T) {
	_, host := hostNode()
	if !isSupported(host) {
		t.Skipf("Node %d has no distroless image", host)
	}
	for _, pm := range []string{"npm", "pnpm", "yarn"} {
		t.Run(pm, func(t *testing.T) {
			if _, err := exec.LookPath(pm); err != nil {
				t.Skip(pm + " not installed")
			}
			root := monorepo(t, pm)
			reg := fakeNodeBase(t, host)
			defer reg.Close()
			store, _ := cache.Open(t.TempDir())
			build := func() (*bundle.Result, string) {
				var log bytes.Buffer
				res, err := bundle.Build(context.Background(), bundle.Request{
					Dir: filepath.Join(root, "apps", "api"), Tag: "api", Plugins: []bundle.Plugin{Plugin{}},
					Cache: store, Runner: runner.Local{}, Log: &log, Registry: registry.New(),
					Options: bundle.Options{Platform: bundle.Platform{OS: "linux", Arch: "amd64"}},
					Mirrors: map[string]string{"gcr.io": reg.Host()}})
				if err != nil {
					t.Fatalf("%v\n%s", err, log.String())
				}
				return res, log.String()
			}
			res, log := build()
			if !strings.Contains(log, "monorepo at "+root+"; building apps/api") {
				t.Errorf("log:\n%s", log)
			}
			cfg := res.Plan
			if cfg.WorkingDir != "/app/apps/api" || strings.Join(cfg.Cmd, " ") != "index.js" {
				t.Errorf("workdir %s cmd %v", cfg.WorkingDir, cfg.Cmd)
			}
			fsroot := extract(t, res.Image)
			for _, gone := range []string{"app/apps/other", "app/packages/devtool/package.json", "app/apps/api/.env", "app/README.md"} {
				if _, err := os.Lstat(filepath.Join(fsroot, gone)); err == nil {
					t.Errorf("%s shouldn't be in the image", gone)
				}
			}
			if _, err := os.Stat(filepath.Join(fsroot, "app/packages/util/dist/index.js")); err != nil {
				t.Errorf("util's build output is missing: %v", err)
			}
			// Run it as the container would: node index.js in /app/apps/api.
			run := exec.Command("node", "index.js")
			run.Dir = filepath.Join(fsroot, "app", "apps", "api")
			out, err := run.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "api says hello from util") {
				t.Fatalf("running the image's app: %v\n%s", err, out)
			}

			// Another app changing doesn't rebuild this one.
			os.WriteFile(filepath.Join(root, "apps/other/index.js"), []byte(`console.log("changed")`), 0o644)
			res2, _ := build()
			for _, l := range res2.Layers {
				if !l.Cached {
					t.Errorf("layer %s rebuilt after an unrelated app changed", l.Name)
				}
			}
			// A local package it uses does.
			os.WriteFile(filepath.Join(root, "packages/util/src.js"), []byte(`exports.hello = () => "v2"`), 0o644)
			res3, _ := build()
			rebuilt := map[string]bool{}
			for _, l := range res3.Layers {
				rebuilt[l.Name] = !l.Cached
			}
			if !rebuilt["app"] || rebuilt["deps"] {
				t.Errorf("after changing packages/util: %v", rebuilt)
			}
		})
	}
}

func TestMonorepoRootRefused(t *testing.T) {
	src := project(t, map[string]string{"package.json": `{"name":"r","workspaces":["apps/*"]}`, "package-lock.json": "{}"})
	_, err := detectManager(src, pkg(t, src))
	if err == nil || !strings.Contains(err.Error(), "xu build apps/web") {
		t.Fatalf("got %v", err)
	}
}
