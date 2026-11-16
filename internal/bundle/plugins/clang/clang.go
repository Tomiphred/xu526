// Package clang builds C projects into one static binary on scratch, using
// `zig cc` with musl as a cross compiler. That works the same on Linux,
// macOS and Windows hosts and for amd64 and arm64 targets, with no system
// toolchain or libc headers to match.
package clang

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"xu526/internal/bundle"
)

// Plugin is the C plugin.
type Plugin struct{}

func (Plugin) Name() string    { return "c" }
func (Plugin) Version() string { return "1" }

// Build systems the plugin drives.
const (
	plain = "sources" // no build system: compile the .c files directly
	make_ = "make"
	cmake = "cmake"
)

var cmakeLangs = regexp.MustCompile(`(?is)project\s*\(([^)]*)\)`)

func (Plugin) Detect(src *bundle.Source) (*bundle.Detection, error) {
	sources := cSources(src, "")
	det := &bundle.Detection{Details: map[string]string{}}
	switch {
	case src.Has("CMakeLists.txt"):
		b, _ := fs.ReadFile(src.FS, "CMakeLists.txt")
		if m := cmakeLangs.FindSubmatch(b); m != nil && len(sources) == 0 {
			words := strings.Fields(strings.ToUpper(string(m[1])))
			hasC, hasCXX := false, false
			for _, w := range words[1:] {
				hasC = hasC || w == "C"
				hasCXX = hasCXX || w == "CXX"
			}
			if hasCXX && !hasC {
				return nil, nil // a C++ project
			}
		}
		det.Details["system"], det.Markers = cmake, []string{"CMakeLists.txt"}
	case src.Has("Makefile") || src.Has("makefile") || src.Has("GNUmakefile"):
		if len(sources) == 0 {
			return nil, nil // a Makefile alone could drive anything
		}
		det.Details["system"], det.Markers = make_, []string{"Makefile"}
	case src.Has("configure") || src.Has("configure.ac"):
		if len(sources) == 0 {
			return nil, nil
		}
		det.Details["system"], det.Markers = "autotools", []string{"configure"}
	case len(sources) > 0:
		det.Details["system"], det.Markers = plain, []string{fmt.Sprintf("%d .c files", len(sources))}
		det.Confidence = 40
		return det, nil
	default:
		return nil, nil
	}
	det.Confidence = 60
	return det, nil
}

// skipDirs are never compiled in sources mode.
var skipDirs = map[string]bool{"test": true, "tests": true, "example": true, "examples": true, "bench": true, "benchmarks": true,
	"build": true, "node_modules": true}

// cSources lists .c files (relative paths), skipping tests, examples and
// build output. Only the root and src/ hold sources in sources mode.
func cSources(src *bundle.Source, root string) []string {
	var out []string
	if root == "" {
		root = "."
	}
	fs.WalkDir(src.FS, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			n := d.Name()
			if p != root && (strings.HasPrefix(n, ".") || skipDirs[n]) {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".c") && !strings.HasSuffix(p, "_test.c") && !strings.HasPrefix(path.Base(p), "test_") {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

var hasMain = regexp.MustCompile(`(?m)^\s*(int|void)\s+main\s*\(`)

// zig finds the compiler: `zig` on PATH, else the ziglang package from
// PyPI (`pip install ziglang`), run as `python3 -m ziglang`.
func zig() ([]string, string, error) {
	try := func(cmd ...string) ([]string, string, bool) {
		out, err := exec.Command(cmd[0], append(cmd[1:], "version")...).Output()
		if err != nil {
			return nil, "", false
		}
		return cmd, strings.TrimSpace(string(out)), true
	}
	if p, err := exec.LookPath("zig"); err == nil {
		if c, v, ok := try(p); ok {
			return c, v, nil
		}
	}
	for _, py := range []string{"python3", "python"} {
		if p, err := exec.LookPath(py); err == nil {
			if c, v, ok := try(p, "-m", "ziglang"); ok {
				return c, v, nil
			}
		}
	}
	return nil, "", fmt.Errorf("Zig is needed to build C (it cross-compiles static Linux binaries from any OS): install it from https://ziglang.org/download or run `pip install ziglang`")
}

func triple(arch string) string {
	if arch == "arm64" {
		return "aarch64-linux-musl"
	}
	return "x86_64-linux-musl"
}

func (Plugin) Plan(src *bundle.Source, d *bundle.Detection, opt bundle.Options) (*bundle.Plan, error) {
	system := d.Details["system"]
	if system == "autotools" {
		return nil, fmt.Errorf("./configure (autotools) projects aren't supported yet; Makefile, CMake and plain .c files are")
	}
	zcmd, zver, err := zig()
	if err != nil {
		return nil, err
	}
	target := triple(opt.Platform.Arch)
	var steps []bundle.Step
	summary := []string{fmt.Sprintf("compile with zig %s for %s: a static musl binary, nothing else in the image", zver, target)}
	switch system {
	case plain:
		files, err := plainSources(src, opt.Main)
		if err != nil {
			return nil, err
		}
		args := []string{"$CC", "-O2", "-std=gnu11", "-o", "$OUT/app"}
		for _, inc := range []string{"include", "src", "."} {
			if inc == "." || src.Has(inc) {
				args = append(args, "-I", inc)
			}
		}
		args = append(args, files...)
		args = append(args, "-lm", "-lpthread")
		steps = append(steps, bundle.Step{Name: "compile", Cmd: args})
		summary = append(summary, fmt.Sprintf("no build system: compiling %d source files with -O2", len(files)))
	case make_:
		steps = append(steps, bundle.Step{Name: "make", Cmd: []string{"make", "-j", fmt.Sprint(runtime.NumCPU()), "CC=$CC", "AR=$AR", "RANLIB=$RANLIB"}})
		summary = append(summary, "runs make with CC set to zig cc (the Makefile's own flags are kept)")
	case cmake:
		if _, err := exec.LookPath("cmake"); err != nil {
			return nil, fmt.Errorf("this is a CMake project and cmake isn't installed")
		}
		cpu := "x86_64"
		if opt.Platform.Arch == "arm64" {
			cpu = "aarch64"
		}
		steps = append(steps,
			bundle.Step{Name: "configure", Cmd: []string{"cmake", "-S", ".", "-B", "_xu_build", "-DCMAKE_BUILD_TYPE=Release",
				"-DCMAKE_C_COMPILER=$CC", "-DCMAKE_AR=$AR", "-DCMAKE_RANLIB=$RANLIB", "-DCMAKE_SYSTEM_NAME=Linux",
				"-DCMAKE_SYSTEM_PROCESSOR=" + cpu, "-DBUILD_SHARED_LIBS=OFF", "-DBUILD_TESTING=OFF"}},
			bundle.Step{Name: "build", Cmd: []string{"cmake", "--build", "_xu_build", "--parallel"}})
		summary = append(summary, "configures CMake in Release mode with zig cc as the compiler")
	}
	if opt.Main != "" && system != plain {
		summary = append(summary, "the program to package: "+opt.Main)
	}
	summary = append(summary,
		"reproducible: build paths mapped to '.', __DATE__/__TIME__ pinned to SOURCE_DATE_EPOCH, symbols stripped",
		"only libc, libm and pthreads are available; other libraries must be built from source in the project",
		"runs as user 65532 (non-root); PORT=8080 is set for servers")
	for i := range steps {
		steps[i].Env = []string{fmt.Sprintf("SOURCE_DATE_EPOCH=%d", opt.Epoch)}
	}
	p := &bundle.Plan{
		Strategy: bundle.StaticBinary, Base: "scratch", Steps: steps,
		Entrypoint: []string{"/app"}, Env: map[string]string{"PORT": "8080"}, User: "65532:65532", Port: 8080,
		Summary: summary,
	}
	if ca := fileHash(caBundle); ca != "" {
		p.Layers = append(p.Layers, bundle.LayerPlan{Name: "certs", Salt: ca})
	}
	p.Layers = append(p.Layers, bundle.LayerPlan{Name: "app", Inputs: []string{"**/*"},
		Salt: strings.Join([]string{zver, target, system, opt.Main, strings.Join(zcmd[1:], " "), fmt.Sprint(opt.Epoch)}, "|")})
	return p, nil
}

// plainSources picks the files for sources mode: everything under the
// root and src/ with exactly one main(), or the file named by --main plus
// the files without a main.
func plainSources(src *bundle.Source, main string) ([]string, error) {
	files := cSources(src, "")
	var mains, rest []string
	for _, f := range files {
		b, _ := fs.ReadFile(src.FS, f)
		if hasMain.Match(b) {
			mains = append(mains, f)
		} else {
			rest = append(rest, f)
		}
	}
	if main != "" {
		m := path.Clean(filepath.ToSlash(main))
		for _, f := range mains {
			if f == m {
				return append([]string{f}, rest...), nil
			}
		}
		return nil, fmt.Errorf("--main %s: not a .c file with main() (found: %s)", main, strings.Join(mains, ", "))
	}
	switch len(mains) {
	case 0:
		return nil, fmt.Errorf("no main() found in %d .c files", len(files))
	case 1:
		return append(mains, rest...), nil
	}
	return nil, fmt.Errorf("several files define main() (%s); choose one with --main, or add a Makefile", strings.Join(mains, ", "))
}

var caBundle = bundle.HostCABundle()

func fileHash(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// wrappers writes cc, ar and ranlib scripts that call zig for the target
// with the reproducibility flags, so any build system can use them.
func wrappers(dir string, zcmd []string, target, srcDir string) (map[string]string, error) {
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	var z []string
	for _, c := range zcmd {
		z = append(z, quote(c))
	}
	zig := strings.Join(z, " ")
	scripts := map[string]string{
		"CC": `#!/bin/sh
# zig cc for ` + target + `, written by xu build
link=1
for a in "$@"; do
  case "$a" in -c|-S|-E|-M|-MM) link=0 ;; esac
done
if [ "$link" = 1 ]; then set -- "$@" -s; fi
exec ` + zig + ` cc -target ` + target + ` -ffile-prefix-map=` + quote(srcDir) + `=. -Wno-error=date-time -Wno-date-time "$@"
`,
		"AR":     "#!/bin/sh\nexec " + zig + " ar \"$@\"\n",
		"RANLIB": "#!/bin/sh\nexec " + zig + " ranlib \"$@\"\n",
	}
	out := map[string]string{}
	for name, body := range scripts {
		p := filepath.Join(dir, "xu-"+strings.ToLower(name))
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			return nil, err
		}
		out[name] = p
	}
	return out, nil
}

func skipCopy(rel string, d fs.DirEntry) bool {
	n := d.Name()
	if d.IsDir() {
		return n == ".git" || n == "_xu_build" || n == "node_modules"
	}
	// Leftover objects from the host compiler must not be linked in.
	for _, ext := range []string{".o", ".obj", ".a", ".so", ".tar", ".tar.tmp"} {
		if strings.HasSuffix(n, ext) {
			return true
		}
	}
	return false
}

func (Plugin) Build(ctx context.Context, env *bundle.BuildEnv, p *bundle.Plan) (*bundle.Output, error) {
	zcmd, _, err := zig()
	if err != nil {
		return nil, err
	}
	srcDir := filepath.Join(env.WorkDir, "src")
	if err := bundle.CopyTree(env.Source.Dir, srcDir, func(rel string, d fs.DirEntry) bool {
		if skipCopy(rel, d) {
			return true
		}
		// Executables left over from a local build would look like outputs.
		return !d.IsDir() && isELF(filepath.Join(env.Source.Dir, rel))
	}, nil); err != nil {
		return nil, err
	}
	outDir := filepath.Join(env.WorkDir, "out")
	os.MkdirAll(outDir, 0o755)
	tools, err := wrappers(env.WorkDir, zcmd, triple(env.Options.Platform.Arch), srcDir)
	if err != nil {
		return nil, err
	}
	start := time.Now().Add(-time.Second)
	for _, s := range p.Steps {
		s.Dir = srcDir
		s.Cmd = append([]string(nil), s.Cmd...)
		for i, a := range s.Cmd {
			for k, v := range tools {
				a = strings.ReplaceAll(a, "$"+k, v)
			}
			s.Cmd[i] = strings.ReplaceAll(a, "$OUT", outDir)
		}
		if err := env.Runner.Run(ctx, s, env.Log); err != nil {
			return nil, err
		}
	}
	bin := filepath.Join(outDir, "app")
	if _, err := os.Stat(bin); err != nil {
		if bin, err = findProgram(srcDir, start, env.Options.Main); err != nil {
			return nil, err
		}
	}
	if err := checkBinary(bin, env.Options.Platform.Arch); err != nil {
		return nil, err
	}
	return &bundle.Output{Files: map[string]string{"app": bin}}, nil
}

func isELF(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	magic := make([]byte, 4)
	n, _ := f.Read(magic)
	return n == 4 && bytes.Equal(magic, []byte("\x7fELF"))
}

// findProgram locates the executable a Makefile or CMake build produced:
// the --main path if given, else the only new ELF executable.
func findProgram(dir string, since time.Time, main string) (string, error) {
	if main != "" {
		p := filepath.Join(dir, filepath.FromSlash(main))
		if !isELF(p) {
			p = filepath.Join(dir, "_xu_build", filepath.FromSlash(main))
		}
		if !isELF(p) {
			return "", fmt.Errorf("--main %s: the build didn't produce that program", main)
		}
		return p, nil
	}
	var found []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.Contains(p, "CMakeFiles") {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Mode()&0o111 == 0 || info.ModTime().Before(since) {
			return nil
		}
		if isELF(p) {
			if f, err := elf.Open(p); err == nil {
				if f.Type == elf.ET_EXEC || (f.Type == elf.ET_DYN && !sharedLib.MatchString(d.Name())) {
					rel, _ := filepath.Rel(dir, p)
					found = append(found, filepath.ToSlash(rel))
				}
				f.Close()
			}
		}
		return nil
	})
	sort.Strings(found)
	switch len(found) {
	case 0:
		return "", fmt.Errorf("the build finished but produced no program")
	case 1:
		return filepath.Join(dir, filepath.FromSlash(found[0])), nil
	}
	return "", fmt.Errorf("the build produced several programs (%s); choose one with --main", strings.Join(found, ", "))
}

// sharedLib matches libfoo.so and libfoo.so.1.2.
var sharedLib = regexp.MustCompile(`\.so(\.[0-9]+)*$`)

// checkBinary makes sure the program will run on scratch: static, Linux,
// right architecture.
func checkBinary(p, arch string) error {
	f, err := elf.Open(p)
	if err != nil {
		return fmt.Errorf("%s is not a Linux program: %w", filepath.Base(p), err)
	}
	defer f.Close()
	want := elf.EM_X86_64
	if arch == "arm64" {
		want = elf.EM_AARCH64
	}
	if f.Machine != want {
		return fmt.Errorf("%s was built for %s, not %s (the build may ignore CC)", filepath.Base(p), f.Machine, arch)
	}
	for _, prog := range f.Progs {
		if prog.Type == elf.PT_INTERP {
			return fmt.Errorf("%s is dynamically linked, so it won't run on an empty image (does the build add -shared or use a system library?)", filepath.Base(p))
		}
	}
	return nil
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
