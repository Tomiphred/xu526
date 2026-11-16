package node

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"xu526/internal/bundle"
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

func pkg(t *testing.T, src *bundle.Source) *pkgJSON {
	p, err := readPkg(src)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMode(t *testing.T) {
	cases := []struct {
		name, pkg string
		files     map[string]string
		static    bool
		spa       bool
	}{
		{"express", `{"dependencies":{"express":"^4"}}`, nil, false, false},
		{"plain start", `{"scripts":{"start":"node index.js"}}`, nil, false, false},
		{"vite", `{"scripts":{"dev":"vite","build":"vite build"},"devDependencies":{"vite":"^6"}}`, nil, true, true},
		{"cra start is a dev server", `{"scripts":{"start":"react-scripts start","build":"react-scripts build"},"dependencies":{"react-scripts":"5"}}`, nil, true, true},
		{"astro", `{"scripts":{"build":"astro build"},"dependencies":{"astro":"^5"}}`, nil, true, false},
		{"next server", `{"scripts":{"build":"next build","start":"next start"},"dependencies":{"next":"15"}}`, nil, false, false},
		{"next export", `{"scripts":{"build":"next build"},"dependencies":{"next":"15"}}`, map[string]string{"next.config.mjs": "export default { output: 'export' }"}, true, false},
		{"vite plus express is a server", `{"scripts":{"build":"vite build","start":"node server.js"},"dependencies":{"express":"4"},"devDependencies":{"vite":"6"}}`, nil, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			files := map[string]string{"package.json": c.pkg}
			for k, v := range c.files {
				files[k] = v
			}
			src := project(t, files)
			st, spa, why := mode(src, pkg(t, src))
			if st != c.static || spa != c.spa {
				t.Fatalf("static=%v spa=%v (%s), want %v %v", st, spa, why, c.static, c.spa)
			}
		})
	}
}

func TestManager(t *testing.T) {
	cases := []struct {
		files map[string]string
		want  string
		err   string
	}{
		{map[string]string{"package-lock.json": "{}"}, "npm", ""},
		{map[string]string{"pnpm-lock.yaml": "lockfileVersion: '9.0'"}, "pnpm", ""},
		{map[string]string{"yarn.lock": "# yarn lockfile v1\n"}, "yarn", ""},
		{map[string]string{"yarn.lock": "__metadata:\n  version: 8\n"}, "", "Berry"},
		{map[string]string{"bun.lockb": ""}, "", "Bun"},
		{map[string]string{}, "", "no lockfile"},
	}
	for _, c := range cases {
		c.files["package.json"] = `{}`
		src := project(t, c.files)
		m, err := detectManager(src, pkg(t, src))
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%v: want error %q, got %v", c.files, c.err, err)
			}
			continue
		}
		if err != nil || m.Name != c.want {
			t.Errorf("%v: got %v %v", c.files, m, err)
		}
	}
	src := project(t, map[string]string{"package.json": `{"workspaces":["packages/*"]}`, "package-lock.json": "{}"})
	if _, err := detectManager(src, pkg(t, src)); err == nil {
		t.Error("workspaces accepted")
	}
}

func TestStartCommand(t *testing.T) {
	lock := `{"packages":{"":{},"node_modules/next":{"bin":{"next":"dist/bin/next"}},
		"node_modules/tsx":{"bin":{"tsx":"./dist/cli.mjs"},"dev":true},
		"node_modules/@nestjs/cli":{"bin":{"nest":"bin/nest.js"}},
		"node_modules/a/node_modules/next":{"bin":{"next":"old"}}}}`
	cases := []struct {
		start string
		args  string
		env   string
		err   string
	}{
		{"node server.js", "server.js", "", ""},
		{"node --enable-source-maps dist/main.js --port 1", "--enable-source-maps dist/main.js --port 1", "", ""},
		{"NODE_OPTIONS=--max-old-space-size=512 node index.js", "index.js", "NODE_OPTIONS=--max-old-space-size=512", ""},
		{"cross-env NODE_ENV=production node app.js", "app.js", "NODE_ENV=production", ""},
		{"next start -p $PORT", "", "", "shell"},
		{"next start", "node_modules/next/dist/bin/next start", "", ""},
		{"nest start", "node_modules/@nestjs/cli/bin/nest.js start", "", ""},
		{"tsx src/index.ts", "", "", "devDependency"},
		{"npm run serve", "", "", "package manager"},
		{"node build && node dist", "", "", "shell"},
		{"node 'my server.js'", "my server.js", "", ""},
		{"mystery serve", "", "", "can't find"},
	}
	for _, c := range cases {
		src := project(t, map[string]string{"package.json": `{"scripts":{"start":` + jsonQuote(c.start) + `},"dependencies":{"next":"15"},"devDependencies":{"tsx":"4"}}`, "package-lock.json": lock})
		args, env, _, err := startCommand(src, pkg(t, src))
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%q: want error %q, got %v %v", c.start, c.err, args, err)
			}
			continue
		}
		var envs []string
		for k, v := range env {
			envs = append(envs, k+"="+v)
		}
		if err != nil || strings.Join(args, " ") != c.args || strings.Join(envs, " ") != c.env {
			t.Errorf("%q: got %q %v %v", c.start, args, envs, err)
		}
	}
	// No start script: main, then common file names.
	src := project(t, map[string]string{"package.json": `{"main":"lib/app.js"}`})
	if args, _, _, _ := startCommand(src, pkg(t, src)); args[0] != "lib/app.js" {
		t.Errorf("main: %v", args)
	}
	src = project(t, map[string]string{"package.json": `{}`, "server.js": ""})
	if args, _, _, _ := startCommand(src, pkg(t, src)); args[0] != "server.js" {
		t.Errorf("server.js: %v", args)
	}
	src = project(t, map[string]string{"package.json": `{}`})
	if _, _, _, err := startCommand(src, pkg(t, src)); err == nil {
		t.Error("nothing to start accepted")
	}
}

func jsonQuote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }

func TestNodeVersion(t *testing.T) {
	for r, want := range map[string]int{"22": 22, "^24.1.0": 24, "~26": 26, "24.x": 24, ">=22": 24, ">=20 <23": 22, "20 || 22": 22, ">= 25": 26, ">22.1": 24} {
		got, err := majorForRange(r, 0)
		if err != nil || got != want {
			t.Errorf("%q -> %d %v, want %d", r, got, err, want)
		}
	}
	if got, _ := majorForRange(">=22", 26); got != 26 {
		t.Errorf("should prefer the host's major when allowed, got %d", got)
	}
	if _, err := majorForRange("18", 0); err == nil {
		t.Error("unsupported major accepted")
	}
	src := project(t, map[string]string{"package.json": `{"engines":{"node":">=18"}}`, ".nvmrc": "v22.11.0\n"})
	if m, from, _ := chooseMajor(src, pkg(t, src), 24); m != 22 || from != ".nvmrc" {
		t.Errorf(".nvmrc wins: %d %s", m, from)
	}
	src = project(t, map[string]string{"package.json": `{}`})
	if m, _, _ := chooseMajor(src, pkg(t, src), 18); m != defaultMajor {
		t.Errorf("unsupported host falls back to default, got %d", m)
	}
}

func TestCheckNative(t *testing.T) {
	nm := filepath.Join(t.TempDir(), "node_modules")
	os.MkdirAll(filepath.Join(nm, "bcrypt", "build", "Release"), 0o755)
	os.WriteFile(filepath.Join(nm, "bcrypt", "build", "Release", "bcrypt_lib.node"), nil, 0o644)
	other := "arm64"
	if runtime.GOARCH == "arm64" {
		other = "amd64"
	}
	err := checkNative(nm, bundle.Platform{OS: "linux", Arch: other}, baseFor(24), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "bcrypt") {
		t.Fatalf("cross-arch native module accepted: %v", err)
	}
	if err := checkNative(t.TempDir(), bundle.Platform{OS: "linux", Arch: other}, baseFor(24), io.Discard); err != nil {
		t.Fatalf("no native modules: %v", err)
	}
}

func TestStripScripts(t *testing.T) {
	p := filepath.Join(t.TempDir(), "package.json")
	os.WriteFile(p, []byte(`{"name":"x","scripts":{"prepare":"husky"},"dependencies":{"a":"1"}}`), 0o644)
	stripScripts(p, bundle.Platform{OS: "linux", Arch: runtime.GOARCH})
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), "husky") || !strings.Contains(string(b), `"a": "1"`) {
		t.Fatal(string(b))
	}
}

// fakeELF is a minimal ELF64 shared-object header for machine m.
func fakeELF(m elf.Machine) []byte {
	b := make([]byte, 64)
	copy(b, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(b[16:], uint16(elf.ET_DYN))
	binary.LittleEndian.PutUint16(b[18:], uint16(m))
	binary.LittleEndian.PutUint32(b[20:], 1)
	binary.LittleEndian.PutUint16(b[52:], 64)
	binary.LittleEndian.PutUint16(b[54:], 56)
	binary.LittleEndian.PutUint16(b[58:], 64)
	return b
}

func TestNativeProblem(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, b []byte) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, b, 0o644)
		return p
	}
	cases := []struct {
		file []byte
		arch string
		want string
	}{
		{fakeELF(elf.EM_X86_64), "amd64", ""},
		{fakeELF(elf.EM_AARCH64), "arm64", ""},
		{fakeELF(elf.EM_AARCH64), "amd64", "built for arm64"},
		{[]byte{0xcf, 0xfa, 0xed, 0xfe, 0, 0}, "arm64", "macOS"},
		{[]byte{0xca, 0xfe, 0xba, 0xbe, 0, 0}, "amd64", "macOS"},
		{[]byte("MZ\x90\x00"), "amd64", "Windows"},
	}
	for i, c := range cases {
		got := nativeProblem(write(fmt.Sprint(i, ".node"), c.file), c.arch)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("case %d: got %q, want %q", i, got, c.want)
		}
	}
}

func TestCheckNativeNested(t *testing.T) {
	nm := filepath.Join(t.TempDir(), "node_modules")
	put := func(rel string, b []byte) {
		p := filepath.Join(nm, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, b, 0o644)
	}
	put("@img/sharp-linux-x64/lib/sharp.node", fakeELF(elf.EM_X86_64))
	put("a/node_modules/@scope/mac/x.node", []byte{0xcf, 0xfa, 0xed, 0xfe})
	err := checkNative(nm, bundle.Platform{OS: "linux", Arch: "amd64"}, baseFor(24), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "@scope/mac (built for macOS)") || strings.Contains(err.Error(), "sharp") {
		t.Fatalf("got %v", err)
	}
}

func TestStripScriptsCross(t *testing.T) {
	p := filepath.Join(t.TempDir(), "package.json")
	os.WriteFile(p, []byte(`{"name":"x","pnpm":{"overrides":{"a":"2"}}}`), 0o644)
	other := "arm64"
	if runtime.GOARCH == "arm64" {
		other = "amd64"
	}
	if err := stripScripts(p, bundle.Platform{OS: "linux", Arch: other}); err != nil {
		t.Fatal(err)
	}
	var m struct {
		Pnpm struct {
			Overrides              map[string]string
			SupportedArchitectures map[string][]string
		}
	}
	b, _ := os.ReadFile(p)
	json.Unmarshal(b, &m)
	if m.Pnpm.Overrides["a"] != "2" || m.Pnpm.SupportedArchitectures["cpu"][0] != npmCPU(other) || m.Pnpm.SupportedArchitectures["os"][0] != "linux" {
		t.Fatal(string(b))
	}
}

func TestCrossEnv(t *testing.T) {
	got := strings.Join(crossEnv(bundle.Platform{OS: "linux", Arch: "amd64"}), " ")
	if got != "npm_config_os=linux npm_config_cpu=x64 npm_config_libc=glibc" {
		t.Fatal(got)
	}
}

// fakeMuslELF is an ELF shared object for m that needs musl's libc.
func fakeMuslELF(t *testing.T, m elf.Machine) []byte {
	// Header + a PT_DYNAMIC segment whose DT_NEEDED names libc.musl.
	strtab := []byte("\x00libc.musl-x86_64.so.1\x00")
	const ehsz, phsz = 64, 56
	dynOff := ehsz + phsz
	dyn := make([]byte, 16*3)
	binary.LittleEndian.PutUint64(dyn[0:], uint64(elf.DT_NEEDED))
	binary.LittleEndian.PutUint64(dyn[8:], 1)
	binary.LittleEndian.PutUint64(dyn[16:], uint64(elf.DT_STRTAB))
	binary.LittleEndian.PutUint64(dyn[24:], uint64(dynOff+len(dyn)))
	binary.LittleEndian.PutUint64(dyn[32:], uint64(elf.DT_NULL))
	b := fakeELF(m)
	binary.LittleEndian.PutUint64(b[32:], ehsz) // e_phoff
	binary.LittleEndian.PutUint16(b[56:], 1)    // e_phnum
	ph := make([]byte, phsz)
	binary.LittleEndian.PutUint32(ph[0:], uint32(elf.PT_DYNAMIC))
	binary.LittleEndian.PutUint64(ph[8:], uint64(dynOff))  // p_offset
	binary.LittleEndian.PutUint64(ph[16:], uint64(dynOff)) // p_vaddr
	binary.LittleEndian.PutUint64(ph[32:], uint64(len(dyn)))
	binary.LittleEndian.PutUint64(ph[40:], uint64(len(dyn)))
	b = append(append(append(b, ph...), dyn...), strtab...)
	return b
}

func TestCheckNativeDropsMuslVariants(t *testing.T) {
	nm := filepath.Join(t.TempDir(), "node_modules")
	put := func(rel string, b []byte) {
		p := filepath.Join(nm, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, b, 0o644)
	}
	musl := fakeMuslELF(t, elf.EM_AARCH64)
	if why := nativeProblem(func() string { put("probe/x.node", musl); return filepath.Join(nm, "probe", "x.node") }(), "arm64"); why != muslProblem {
		t.Fatalf("fake musl library not recognised: %q", why)
	}
	os.RemoveAll(filepath.Join(nm, "probe"))
	put("@img/sharp-linux-arm64/lib/sharp-linux-arm64.node", fakeELF(elf.EM_AARCH64))
	put("@img/sharp-linuxmusl-arm64/lib/sharp-linuxmusl-arm64.node", musl)
	put("@img/sharp-linuxmusl-arm64/package.json", []byte("{}"))
	var log bytes.Buffer
	if err := checkNative(nm, bundle.Platform{OS: "linux", Arch: "arm64"}, baseFor(24), &log); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(nm, "@img", "sharp-linuxmusl-arm64")); !os.IsNotExist(err) {
		t.Error("musl variant should have been removed")
	}
	if _, err := os.Stat(filepath.Join(nm, "@img", "sharp-linux-arm64", "lib", "sharp-linux-arm64.node")); err != nil {
		t.Error("glibc variant must stay")
	}
	if !strings.Contains(log.String(), "left out @img/sharp-linuxmusl-arm64") {
		t.Errorf("log: %q", log.String())
	}
	// A musl-only build of a package that isn't a musl variant is still an error.
	put("bcrypt/build/Release/bcrypt_lib.node", musl)
	err := checkNative(nm, bundle.Platform{OS: "linux", Arch: "arm64"}, baseFor(24), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "bcrypt (built for musl") {
		t.Fatalf("got %v", err)
	}
}
