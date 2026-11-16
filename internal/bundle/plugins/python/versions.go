package python

import (
	"fmt"
	"io/fs"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"xu526/internal/bundle"
)

// Release is a python-build-standalone CPython build: the newest patch of
// each minor version, as published by Astral (the builds uv installs).
type Release struct {
	Version string
	Tag     string
	// SHA256 by architecture, when pinned in code. Empty entries are
	// verified against the release's SHA256SUMS on first use and recorded
	// in xu.lock (see tools/pythons to fill these in).
	SHA256 map[string]string
}

// Releases maps minor version (3.x) to its build.
var Releases = map[int]Release{
	8:  {Version: "3.8.20", Tag: "20241002"},
	9:  {Version: "3.9.25", Tag: "20251031"},
	10: {Version: "3.10.20", Tag: "20260718"},
	11: {Version: "3.11.15", Tag: "20260718"},
	12: {Version: "3.12.13", Tag: "20260718"},
	13: {Version: "3.13.14", Tag: "20260718"},
	14: {Version: "3.14.6", Tag: "20260718"},
}

const defaultMinor = 13

// ReleaseBase is where python-build-standalone publishes builds. Mirror it
// with XU_MIRRORS="https://github.com/astral-sh/python-build-standalone/releases/download=https://mirror/...".
const ReleaseBase = "https://github.com/astral-sh/python-build-standalone/releases/download"

func cpu(arch string) string {
	if arch == "arm64" {
		return "aarch64"
	}
	return "x86_64"
}

// download describes the runtime tarball for a minor version and arch.
func download(minor int, arch string) bundle.Download {
	r := Releases[minor]
	name := fmt.Sprintf("cpython-%s%%2B%s-%s-unknown-linux-gnu-install_only_stripped.tar.gz", r.Version, r.Tag, cpu(arch))
	return bundle.Download{Name: "python", URL: ReleaseBase + "/" + r.Tag + "/" + name,
		SHA256: r.SHA256[arch], SumsURL: ReleaseBase + "/" + r.Tag + "/SHA256SUMS"}
}

func minors() []int {
	var out []int
	for m := 8; m <= 20; m++ {
		if _, ok := Releases[m]; ok {
			out = append(out, m)
		}
	}
	return out
}

var verRe = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)

// parseVersion reads "3.11", "3.11.4", "python-3.11.4", "cpython@3.12".
func parseVersion(s string) (minor int, exact string, ok bool) {
	m := verRe.FindStringSubmatch(s)
	if m == nil || m[1] != "3" {
		return 0, "", false
	}
	minor, _ = strconv.Atoi(m[2])
	return minor, m[0], true
}

// allowed evaluates a version specifier ("<3.13,>=3.10", "~=3.11",
// "^3.10", "==3.12.*", "3.11") for 3.minor, at minor-version precision.
func allowed(spec string, minor int) bool {
	spec = strings.TrimSpace(spec)
	if spec == "" || spec == "*" {
		return true
	}
	for _, alt := range strings.Split(spec, "||") { // poetry allows ||
		ok := true
		for _, c := range strings.FieldsFunc(alt, func(r rune) bool { return r == ',' }) {
			if !clause(strings.TrimSpace(c), minor) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func clause(c string, minor int) bool {
	ops := []string{"~=", "==", "!=", ">=", "<=", ">", "<", "^", "~", "="}
	op := ""
	for _, o := range ops {
		if strings.HasPrefix(c, o) {
			op, c = o, strings.TrimSpace(c[len(o):])
			break
		}
	}
	m := verRe.FindStringSubmatch(c)
	if m == nil {
		return true // "*" or something we can't read: don't exclude
	}
	if m[1] != "3" {
		n, _ := strconv.Atoi(m[1])
		switch op {
		case ">=", ">":
			return n < 3
		case "<", "<=":
			return n > 3
		}
		return false
	}
	v, _ := strconv.Atoi(m[2])
	hasPatch := m[3] != "" && m[3] != "0"
	switch op {
	case "", "==", "=", "~":
		if op == "~" && !strings.Contains(c, ".") {
			return true
		}
		return minor == v
	case "!=":
		return !(minor == v && (strings.HasSuffix(c, ".*") || m[3] == ""))
	case ">=":
		return minor >= v
	case ">":
		if hasPatch || strings.Count(c, ".") >= 2 {
			return minor >= v
		}
		return minor > v
	case "<=":
		return minor <= v
	case "<":
		if hasPatch {
			return minor <= v
		}
		return minor < v
	case "~=":
		if strings.Count(c, ".") >= 2 { // ~=3.10.2 means 3.10.*
			return minor == v
		}
		return minor >= v
	case "^":
		return minor >= v
	}
	return true
}

var (
	requiresPython = regexp.MustCompile(`(?m)^\s*requires-python\s*=\s*["']([^"']+)["']`)
	poetryPython   = regexp.MustCompile(`(?ms)\[tool\.poetry\.dependencies\][^\[]*?^\s*python\s*=\s*["']([^"']+)["']`)
	pipfilePython  = regexp.MustCompile(`(?m)^\s*python_(?:full_)?version\s*=\s*["']([^"']+)["']`)
)

func hostMinor() int {
	out, err := exec.Command("python3", "-c", "import sys;print(sys.version_info[1])").Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

// chooseMinor picks the image's Python: an exact request (.python-version,
// runtime.txt, Pipfile) wins; a range (requires-python, poetry) picks this
// machine's version if allowed, else the default, else the newest allowed.
func chooseMinor(src *bundle.Source, host int) (minor int, from, requested string, err error) {
	read := func(f string) string {
		b, _ := fs.ReadFile(src.FS, f)
		return strings.TrimSpace(string(b))
	}
	exact := func(v, f string) (int, string, string, error) {
		m, ex, ok := parseVersion(v)
		if !ok {
			return 0, "", "", fmt.Errorf("%s asks for %q; only CPython 3.%d to 3.%d are available", f, v, minors()[0], minors()[len(minors())-1])
		}
		if _, ok := Releases[m]; !ok {
			return 0, "", "", fmt.Errorf("%s asks for Python %s; available: 3.%d to 3.%d", f, ex, minors()[0], minors()[len(minors())-1])
		}
		return m, f, ex, nil
	}
	if v := strings.SplitN(read(".python-version"), "\n", 2)[0]; v != "" && v != "system" {
		if strings.HasPrefix(v, "pypy") || strings.HasPrefix(v, "graalpy") {
			return 0, "", "", fmt.Errorf(".python-version asks for %s; only CPython is supported", v)
		}
		return exact(v, ".python-version")
	}
	if v := read("runtime.txt"); v != "" {
		return exact(v, "runtime.txt")
	}
	if m := pipfilePython.FindStringSubmatch(read("Pipfile")); m != nil {
		return exact(m[1], "Pipfile")
	}
	var spec, specFrom string
	pyproject := read("pyproject.toml")
	if m := requiresPython.FindStringSubmatch(pyproject); m != nil {
		spec, specFrom = m[1], "requires-python "+strconv.Quote(m[1])
	} else if m := poetryPython.FindStringSubmatch(pyproject); m != nil {
		spec, specFrom = m[1], "poetry python "+strconv.Quote(m[1])
	}
	if spec == "" {
		if _, ok := Releases[host]; ok {
			return host, "this machine's Python", "", nil
		}
		return defaultMinor, "default", "", nil
	}
	var ok []int
	for _, m := range minors() {
		if allowed(spec, m) {
			ok = append(ok, m)
		}
	}
	if len(ok) == 0 {
		return 0, "", "", fmt.Errorf("%s matches no available Python (3.%d to 3.%d)", specFrom, minors()[0], minors()[len(minors())-1])
	}
	for _, pick := range []int{host, defaultMinor} {
		for _, m := range ok {
			if m == pick {
				return m, specFrom, "", nil
			}
		}
	}
	return ok[len(ok)-1], specFrom, "", nil
}
