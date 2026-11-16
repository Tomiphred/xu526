// Package python builds Python apps on any CPython from 3.8 up: a
// standalone CPython build (python-build-standalone) on distroless/cc,
// with dependencies installed as Linux wheels for the target, in their own
// layer, whatever OS the build runs on.
package python

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"xu526/internal/bundle"
)

// Plugin is the Python plugin.
type Plugin struct{}

func (Plugin) Name() string    { return "python" }
func (Plugin) Version() string { return "1" }

// Base is the runtime image under the Python layer: glibc, CA certificates,
// tzdata and a nonroot user, nothing else.
const Base = "gcr.io/distroless/cc-debian13:nonroot"

const prefix = "/opt/python"

var strong = []string{"requirements.txt", "pyproject.toml", "Pipfile", "Pipfile.lock", "uv.lock", "poetry.lock", "manage.py", "setup.py"}

func (Plugin) Detect(src *bundle.Source) (*bundle.Detection, error) {
	var markers []string
	for _, f := range strong {
		if src.Has(f) {
			markers = append(markers, f)
		}
	}
	if len(markers) > 0 {
		return &bundle.Detection{Confidence: 85, Markers: markers}, nil
	}
	for _, f := range scriptFiles {
		if src.Has(f) {
			return &bundle.Detection{Confidence: 30, Markers: []string{f}}, nil
		}
	}
	return nil, nil
}

// platformTags are the manylinux wheel tags the image's glibc (2.41 in
// Debian 13) can run.
func platformTags(arch string) []string {
	c := cpu(arch)
	tags := []string{"manylinux2014_" + c}
	for minor := 17; minor <= 41; minor++ {
		tags = append(tags, fmt.Sprintf("manylinux_2_%d_%s", minor, c))
	}
	return tags
}

func pipVersion() (string, error) {
	out, err := exec.Command("python3", "-m", "pip", "--version").Output()
	if err != nil {
		return "", fmt.Errorf("pip is needed on this machine to fetch dependencies (python3 -m pip); install Python 3 with pip")
	}
	return strings.Fields(string(out))[1], nil
}

func (Plugin) Plan(src *bundle.Source, d *bundle.Detection, opt bundle.Options) (*bundle.Plan, error) {
	ds, err := detectDeps(src)
	if err != nil {
		return nil, err
	}
	minor, from, requested, err := chooseMinor(src, hostMinor())
	if err != nil {
		return nil, err
	}
	args, startEnv, startDesc, warn, err := startCommand(src)
	if err != nil {
		return nil, err
	}
	pipV, err := pipVersion()
	if err != nil {
		return nil, err
	}
	if len(ds.Export) > 0 {
		if _, err := exec.LookPath(ds.Export[0]); err != nil {
			return nil, fmt.Errorf("the project is locked with %s, which isn't installed", ds.Export[0])
		}
	}
	rel := Releases[minor]
	pyver := fmt.Sprintf("3.%d", minor)
	summary := []string{fmt.Sprintf("CPython %s (%s) from python-build-standalone %s on distroless/cc", rel.Version, from, rel.Tag)}
	if requested != "" && requested != rel.Version && strings.Count(requested, ".") == 2 {
		summary = append(summary, fmt.Sprintf("note: %s asks for %s; using %s, the newest %s patch", from, requested, rel.Version, pyver))
	}
	summary = append(summary,
		"dependencies: "+ds.Note+", installed as Linux "+cpu(opt.Platform.Arch)+" wheels for "+pyver+" in their own layer",
		"starts with: python "+strings.Join(args, " ")+" ("+startDesc+")")
	if warn != "" {
		summary = append(summary, "note: "+warn)
	}
	if ds.Kind == "requirements" {
		b, _ := fs.ReadFile(src.FS, "requirements.txt")
		if loose := unpinned(b); len(loose) > 0 {
			if len(loose) > 5 {
				loose = append(loose[:5], "…")
			}
			summary = append(summary, fmt.Sprintf("note: not pinned with == (%s): rebuilds may pick newer versions; `pip freeze > requirements.txt` or `uv lock` pins them", strings.Join(loose, ", ")))
		}
	}
	summary = append(summary, "runs as user 65532 (non-root) in /app; listens on $PORT (8080)")

	pip := []string{"python3", "-m", "pip", "install", "--target", "$DEST", "--no-compile", "--disable-pip-version-check",
		"--no-warn-script-location", "--no-warn-conflicts", "--root-user-action=ignore", "--no-input", "--only-binary=:all:", "--implementation", "cp",
		"--python-version", pyver, "--abi", fmt.Sprintf("cp3%d", minor)}
	for _, t := range platformTags(opt.Platform.Arch) {
		pip = append(pip, "--platform", t)
	}
	pip = append(pip, "-r", "$REQ")
	var steps []bundle.Step
	if len(ds.Export) > 0 {
		steps = append(steps, bundle.Step{Name: "export", Cmd: ds.Export})
	}
	if ds.Kind != "none" {
		steps = append(steps, bundle.Step{Name: "deps", Cmd: pip, Env: []string{"PIP_NO_CACHE_DIR=0"},
			Show: fmt.Sprintf("pip install --target <deps> -r requirements (cp3%d wheels for manylinux %s, glibc ≤ 2.41)", minor, cpu(opt.Platform.Arch))})
	}

	pythonpath := "/app"
	if src.Has("src") {
		pythonpath = "/app/src:/app"
	}
	env := map[string]string{
		"PATH":                    prefix + "/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"PYTHONPATH":              pythonpath,
		"PYTHONUNBUFFERED":        "1",
		"PYTHONDONTWRITEBYTECODE": "1",
		"LANG":                    "C.UTF-8",
		"SSL_CERT_FILE":           "/etc/ssl/certs/ca-certificates.crt",
		"PORT":                    "8080",
	}
	for k, v := range startEnv {
		env[k] = v
	}
	dl := download(minor, opt.Platform.Arch)
	return &bundle.Plan{
		Strategy: bundle.RuntimeDeps, Base: Base, Steps: steps, Downloads: []bundle.Download{dl},
		Entrypoint: []string{prefix + "/bin/python3"}, Cmd: args, Env: env, WorkingDir: "/app", Port: 8080,
		Layers: []bundle.LayerPlan{
			{Name: "python", Salt: dl.URL},
			{Name: "deps", Inputs: ds.Files, Salt: strings.Join([]string{ds.Kind, rel.Version, opt.Platform.Arch, "pip " + pipV}, "|")},
			{Name: "app", Inputs: []string{"**/*"}, Salt: strings.Join(args, " ")},
		},
		Summary: summary,
	}, nil
}

var minorInURL = regexp.MustCompile(`cpython-3\.(\d+)\.`)

func planMinor(p *bundle.Plan) int {
	for _, d := range p.Downloads {
		if m := minorInURL.FindStringSubmatch(d.URL); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
	}
	return 0
}

func step(p *bundle.Plan, name string) (bundle.Step, bool) {
	for _, s := range p.Steps {
		if s.Name == name {
			return s, true
		}
	}
	return bundle.Step{}, false
}

func replace(args []string, vars map[string]string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		for k, v := range vars {
			a = strings.ReplaceAll(a, k, v)
		}
		out[i] = a
	}
	return out
}

func (Plugin) Build(ctx context.Context, env *bundle.BuildEnv, p *bundle.Plan) (*bundle.Output, error) {
	out := &bundle.Output{Files: map[string]string{}}
	minor := planMinor(p)
	if !env.Cached["python"] {
		dir := filepath.Join(env.WorkDir, "python")
		if err := extractPython(env.Downloads["python"], dir, minor); err != nil {
			return nil, fmt.Errorf("python runtime: %w", err)
		}
		out.Files["python"] = dir
	}
	if !env.Cached["deps"] {
		dest := filepath.Join(env.WorkDir, "deps")
		os.MkdirAll(dest, 0o755)
		if s, ok := step(p, "deps"); ok {
			req := filepath.Join(env.WorkDir, "requirements.txt")
			if e, ok := step(p, "export"); ok {
				e.Cmd, e.Dir = replace(e.Cmd, map[string]string{"$REQ": req}), env.Source.Dir
				if err := env.Runner.Run(ctx, e, env.Log); err != nil {
					return nil, err
				}
			} else {
				ds, err := detectDeps(env.Source)
				if err != nil {
					return nil, err
				}
				if ds.Kind == "requirements" {
					req = filepath.Join(env.Source.Dir, "requirements.txt") // keeps -r/-c includes working
				} else if err := writeRequirements(ds, env.Source.Dir, req); err != nil {
					return nil, err
				}
			}
			s.Cmd, s.Dir = replace(s.Cmd, map[string]string{"$REQ": req, "$DEST": dest}), env.Source.Dir
			if err := env.Runner.Run(ctx, s, env.Log); err != nil {
				native, why := nativeFallback(minor, env.Options.Platform.Arch)
				if native == "" {
					return nil, fmt.Errorf("%v\nsome dependencies have no prebuilt Linux %s wheel for Python 3.%d (see pip's message above), and %s. Pin versions that publish wheels, or build on Linux %s with python3.%d installed",
						err, cpu(env.Options.Platform.Arch), minor, why, env.Options.Platform.Arch, minor)
				}
				fmt.Fprintf(env.Log, "no wheel for everything; building the rest from source with %s on this machine\n", native)
				os.RemoveAll(dest)
				fb := bundle.Step{Name: "deps (from source)", Dir: env.Source.Dir, Env: s.Env,
					Cmd: []string{native, "-m", "pip", "install", "--target", dest, "--no-compile", "--disable-pip-version-check", "--no-warn-script-location", "--no-warn-conflicts", "--root-user-action=ignore", "--no-input", "-r", req}}
				if err := env.Runner.Run(ctx, fb, env.Log); err != nil {
					return nil, err
				}
			}
		}
		out.Files["deps"] = dest
	}
	if !env.Cached["app"] {
		out.Files["app"] = env.Source.Dir
	}
	return out, nil
}

// nativeFallback returns a local interpreter that can build source
// packages for the image, or why there isn't one.
func nativeFallback(minor int, arch string) (string, string) {
	if runtime.GOOS != "linux" {
		return "", "packages without wheels can only be built on Linux"
	}
	if runtime.GOARCH != arch {
		return "", fmt.Sprintf("this machine is %s, not %s", runtime.GOARCH, arch)
	}
	if _, err := os.Stat("/etc/alpine-release"); err == nil {
		return "", "this machine uses musl (Alpine), the image uses glibc"
	}
	name := fmt.Sprintf("python3.%d", minor)
	if p, err := exec.LookPath(name); err == nil {
		return p, ""
	}
	return "", name + " isn't installed here"
}

// skipped parts of the runtime: tests, IDLE, pip (dependencies are already
// installed), headers and man pages.
func skipRuntime(rel string, minor int) bool {
	lib := fmt.Sprintf("lib/python3.%d/", minor)
	for _, p := range []string{"include/", "share/", lib + "test/", lib + "idlelib/", lib + "turtledemo/", lib + "ensurepip/", lib + "lib2to3/tests/"} {
		if strings.HasPrefix(rel, p) {
			return true
		}
	}
	if strings.HasPrefix(rel, lib+"site-packages/pip") || strings.HasPrefix(rel, "bin/pip") || strings.HasPrefix(rel, "bin/idle") {
		return true
	}
	return strings.Contains(rel, "/__pycache__/") && strings.Contains(rel, "/test")
}

// extractPython unpacks a python-build-standalone install_only tarball
// (everything under python/) into dir, refusing paths that escape it.
func extractPython(tarball, dir string, minor int) error {
	f, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	n := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := path.Clean(h.Name)
		rel, ok := strings.CutPrefix(name, "python/")
		if !ok || strings.HasPrefix(rel, "../") || rel == ".." {
			if name == "python" {
				continue
			}
			return fmt.Errorf("unexpected entry %q (not a python-build-standalone install_only tarball?)", h.Name)
		}
		if skipRuntime(rel, minor) {
			continue
		}
		target := filepath.Join(dir, filepath.FromSlash(rel))
		switch h.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(target, 0o755)
		case tar.TypeReg:
			os.MkdirAll(filepath.Dir(target), 0o755)
			w, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fs.FileMode(h.Mode)&0o755|0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(w, tr); err != nil {
				w.Close()
				return err
			}
			w.Close()
			n++
		case tar.TypeSymlink:
			if filepath.IsAbs(h.Linkname) {
				continue
			}
			os.MkdirAll(filepath.Dir(target), 0o755)
			os.Symlink(h.Linkname, target)
		case tar.TypeLink:
			linkRel, _ := strings.CutPrefix(path.Clean(h.Linkname), "python/")
			b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(linkRel)))
			if err != nil {
				return fmt.Errorf("hard link %s: %w", h.Name, err)
			}
			os.MkdirAll(filepath.Dir(target), 0o755)
			if err := os.WriteFile(target, b, fs.FileMode(h.Mode)&0o755|0o600); err != nil {
				return err
			}
		}
	}
	if n == 0 {
		return fmt.Errorf("the tarball holds no files")
	}
	if _, err := os.Stat(filepath.Join(dir, "bin", "python3")); err != nil {
		return fmt.Errorf("the tarball has no bin/python3")
	}
	return nil
}

// walk lists entries under root for a layer at dst, skipping what skip says.
func walk(root, dst string, skip func(rel string, d fs.DirEntry) bool) ([]bundle.Entry, error) {
	var entries []bundle.Entry
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if skip != nil && skip(rel, d) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		e, ok, err := bundle.FileEntry(p, dst+"/"+rel, d)
		if ok {
			entries = append(entries, e)
		}
		return err
	})
	return entries, err
}

func isVenv(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "pyvenv.cfg"))
	return err == nil
}

func (Plugin) Layers(p *bundle.Plan, out *bundle.Output) ([]bundle.Layer, error) {
	minor := planMinor(p)
	var layers []bundle.Layer
	if dir := out.Files["python"]; dir != "" {
		files, err := walk(dir, prefix, nil)
		if err != nil {
			return nil, err
		}
		layers = append(layers, bundle.Layer{Name: "python", Files: files})
	}
	if dir := out.Files["deps"]; dir != "" {
		files, err := walk(dir, fmt.Sprintf("%s/lib/python3.%d/site-packages", prefix, minor), func(rel string, d fs.DirEntry) bool {
			// bin/ holds launchers whose #! points at this machine's Python.
			return rel == "bin" || d.Name() == "__pycache__"
		})
		if err != nil {
			return nil, err
		}
		layers = append(layers, bundle.Layer{Name: "deps", Files: files})
	}
	if dir := out.Files["app"]; dir != "" {
		files, err := walk(dir, "/app", func(rel string, d fs.DirEntry) bool {
			n := d.Name()
			if strings.HasPrefix(n, ".") || n == "__pycache__" || n == "node_modules" {
				return true
			}
			if d.IsDir() {
				return isVenv(filepath.Join(dir, filepath.FromSlash(rel)))
			}
			return strings.HasSuffix(n, ".pyc") || strings.HasSuffix(n, ".tar") || strings.HasSuffix(n, ".tar.tmp") || rel == bundle.LockFile
		})
		if err != nil {
			return nil, err
		}
		layers = append(layers, bundle.Layer{Name: "app", Files: files})
	}
	return layers, nil
}
