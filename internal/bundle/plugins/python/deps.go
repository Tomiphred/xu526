package python

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"xu526/internal/bundle"
)

// depSource says where dependencies are declared and how to turn that
// into a pip requirements file.
type depSource struct {
	Kind   string   // requirements, uv, poetry, pipenv, pyproject, none
	Files  []string // project files that decide the dependencies (cache inputs)
	Export []string // command that writes $REQ ("" = handled in Go)
	Note   string
}

func detectDeps(src *bundle.Source) (*depSource, error) {
	has := src.Has
	switch {
	case has("uv.lock"):
		return &depSource{Kind: "uv", Files: []string{"uv.lock", "pyproject.toml"},
			Export: []string{"uv", "export", "--frozen", "--no-dev", "--no-emit-project", "--no-header", "--format", "requirements-txt", "-o", "$REQ"},
			Note:   "uv.lock (exported with uv, dev dependencies excluded)"}, nil
	case has("poetry.lock"):
		return &depSource{Kind: "poetry", Files: []string{"poetry.lock", "pyproject.toml"},
			Export: []string{"poetry", "export", "--format", "requirements.txt", "--only", "main", "--output", "$REQ"},
			Note:   "poetry.lock (exported with poetry, main group only)"}, nil
	case has("Pipfile.lock"):
		return &depSource{Kind: "pipenv", Files: []string{"Pipfile.lock"}, Note: "Pipfile.lock (default packages, with hashes)"}, nil
	case has("requirements.txt"):
		files := []string{"requirements.txt"}
		entries, _ := fs.ReadDir(src.FS, ".")
		for _, e := range entries {
			n := e.Name()
			if !e.IsDir() && strings.HasSuffix(n, ".txt") && n != "requirements.txt" && (strings.HasPrefix(n, "requirements") || strings.HasPrefix(n, "constraints")) {
				files = append(files, n)
			}
		}
		if has("requirements") {
			files = append(files, "requirements/**/*.txt")
		}
		b, _ := fs.ReadFile(src.FS, "requirements.txt")
		for _, line := range strings.Split(string(b), "\n") {
			l := strings.TrimSpace(line)
			if strings.HasPrefix(l, "-e ") || strings.HasPrefix(l, "--editable") || l == "." {
				return nil, fmt.Errorf("requirements.txt installs a local package (%q); list its dependencies instead, since the app's own code is copied as is", l)
			}
		}
		return &depSource{Kind: "requirements", Files: files, Note: "requirements.txt"}, nil
	case has("pyproject.toml"):
		b, _ := fs.ReadFile(src.FS, "pyproject.toml")
		if regexp.MustCompile(`(?m)^\s*dependencies\s*=`).Match(b) {
			return &depSource{Kind: "pyproject", Files: []string{"pyproject.toml"}, Note: "pyproject.toml [project] dependencies (no lockfile)"}, nil
		}
	case has("setup.py") || has("setup.cfg"):
		return nil, fmt.Errorf("dependencies are declared in setup.py; add a requirements.txt (pip freeze) or a lockfile (uv lock) so builds are reproducible")
	}
	return &depSource{Kind: "none", Note: "no dependencies declared"}, nil
}

// writeRequirements produces the requirements file for the kinds handled
// in Go (Pipfile.lock, pyproject.toml, requirements.txt).
func writeRequirements(ds *depSource, srcDir, out string) error {
	switch ds.Kind {
	case "requirements":
		b, err := os.ReadFile(filepath.Join(srcDir, "requirements.txt"))
		if err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o644)
	case "pipenv":
		b, err := os.ReadFile(filepath.Join(srcDir, "Pipfile.lock"))
		if err != nil {
			return err
		}
		lines, err := pipfileRequirements(b)
		if err != nil {
			return err
		}
		return os.WriteFile(out, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	case "pyproject":
		// tomllib needs Python 3.11+ on this machine.
		script := `import sys, tomllib
d = tomllib.load(open(sys.argv[1], "rb"))
print("\n".join(d.get("project", {}).get("dependencies", [])))`
		cmd := exec.Command("python3", "-c", script, filepath.Join(srcDir, "pyproject.toml"))
		b, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("reading pyproject.toml dependencies needs Python 3.11+ on this machine (or add requirements.txt / uv.lock): %v", err)
		}
		return os.WriteFile(out, b, 0o644)
	case "none":
		return os.WriteFile(out, nil, 0o644)
	}
	return nil
}

// pipfileRequirements turns Pipfile.lock's default section into pinned,
// hashed requirement lines.
func pipfileRequirements(b []byte) ([]string, error) {
	var lock struct {
		Default map[string]struct {
			Version string   `json:"version"`
			Extras  []string `json:"extras"`
			Markers string   `json:"markers"`
			Hashes  []string `json:"hashes"`
		} `json:"default"`
	}
	if err := json.Unmarshal(b, &lock); err != nil {
		return nil, fmt.Errorf("Pipfile.lock: %w", err)
	}
	names := make([]string, 0, len(lock.Default))
	for n := range lock.Default {
		names = append(names, n)
	}
	sort.Strings(names)
	var lines []string
	for _, n := range names {
		p := lock.Default[n]
		if p.Version == "" {
			return nil, fmt.Errorf("Pipfile.lock: %s has no pinned version (git or path dependencies aren't supported)", n)
		}
		l := n
		if len(p.Extras) > 0 {
			l += "[" + strings.Join(p.Extras, ",") + "]"
		}
		l += p.Version
		if p.Markers != "" {
			l += " ; " + p.Markers
		}
		for _, h := range p.Hashes {
			l += " --hash=" + h
		}
		lines = append(lines, l)
	}
	return lines, nil
}

// unpinned counts requirement lines without an exact == pin.
func unpinned(req []byte) []string {
	var out []string
	for _, line := range strings.Split(string(req), "\n") {
		l := strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
		if l == "" || strings.HasPrefix(l, "-") || strings.HasPrefix(l, "--") {
			continue
		}
		spec := strings.SplitN(l, ";", 2)[0]
		if !strings.Contains(spec, "==") && !strings.Contains(spec, "===") && !strings.Contains(spec, "@") {
			name := strings.FieldsFunc(spec, func(r rune) bool { return strings.ContainsRune("<>=!~[ ", r) })
			if len(name) > 0 {
				out = append(out, name[0])
			}
		}
	}
	return out
}

// depsText is everything that names dependencies, lowercased, used to see
// which server packages a project has.
func depsText(src *bundle.Source) string {
	var sb strings.Builder
	for _, f := range []string{"requirements.txt", "pyproject.toml", "Pipfile", "Pipfile.lock", "uv.lock", "poetry.lock", "requirements/base.txt", "requirements/prod.txt", "requirements/production.txt"} {
		b, _ := fs.ReadFile(src.FS, f)
		sb.Write(b)
		sb.WriteByte('\n')
	}
	return strings.ToLower(sb.String())
}

// hasDep reports whether a package name appears as a dependency.
func hasDep(text, name string) bool {
	re := regexp.MustCompile(`(?m)(^|["'\s\[,])` + regexp.QuoteMeta(name) + `(\[[^\]]*\])?\s*([<>=!~;,"'\]]|$|\s)`)
	return re.MatchString(text)
}
