package python

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
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

func TestAllowed(t *testing.T) {
	cases := []struct {
		spec string
		yes  []int
		no   []int
	}{
		{">=3.10", []int{10, 11, 14}, []int{8, 9}},
		{">=3.9,<3.12", []int{9, 10, 11}, []int{8, 12}},
		{"<3.12.2", []int{11, 12}, []int{13}},
		{"~=3.11", []int{11, 13}, []int{10}},
		{"~=3.10.2", []int{10}, []int{11}},
		{"==3.12.*", []int{12}, []int{11, 13}},
		{"^3.10", []int{10, 14}, []int{9}},
		{">3.10", []int{11}, []int{10}},
		{">3.10.1", []int{10, 11}, []int{9}},
		{"!=3.11.*,>=3.10", []int{10, 12}, []int{11}},
		{"3.9 || 3.12", []int{9, 12}, []int{10}},
		{">=3.8,<4", []int{8, 14}, nil},
	}
	for _, c := range cases {
		for _, m := range c.yes {
			if !allowed(c.spec, m) {
				t.Errorf("%q should allow 3.%d", c.spec, m)
			}
		}
		for _, m := range c.no {
			if allowed(c.spec, m) {
				t.Errorf("%q should not allow 3.%d", c.spec, m)
			}
		}
	}
}

func TestChooseMinor(t *testing.T) {
	cases := []struct {
		files map[string]string
		host  int
		want  int
		err   string
	}{
		{map[string]string{".python-version": "3.11.4\n"}, 13, 11, ""},
		{map[string]string{".python-version": "3.8"}, 13, 8, ""},
		{map[string]string{"runtime.txt": "python-3.12.1"}, 13, 12, ""},
		{map[string]string{"Pipfile": "[requires]\npython_version = \"3.10\"\n"}, 13, 10, ""},
		{map[string]string{"pyproject.toml": "[project]\nrequires-python = \">=3.9,<3.12\"\n"}, 13, 11, ""},
		{map[string]string{"pyproject.toml": "[project]\nrequires-python = \">=3.9\"\n"}, 10, 10, ""},
		{map[string]string{"pyproject.toml": "[tool.poetry.dependencies]\npython = \"^3.12\"\nflask = \"*\"\n"}, 0, 13, ""},
		{map[string]string{}, 12, 12, ""},
		{map[string]string{}, 6, defaultMinor, ""},
		{map[string]string{".python-version": "3.7"}, 13, 0, "available"},
		{map[string]string{".python-version": "pypy3.10"}, 13, 0, "CPython"},
		{map[string]string{"pyproject.toml": "requires-python = \">=3.20\""}, 13, 0, "matches no"},
	}
	for _, c := range cases {
		m, from, _, err := chooseMinor(project(t, c.files), c.host)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%v: want error %q, got %d %v", c.files, c.err, m, err)
			}
			continue
		}
		if err != nil || m != c.want {
			t.Errorf("%v host 3.%d: got 3.%d (%s) %v, want 3.%d", c.files, c.host, m, from, err, c.want)
		}
	}
}

func TestDetectDeps(t *testing.T) {
	cases := []struct {
		files map[string]string
		kind  string
		err   string
	}{
		{map[string]string{"uv.lock": "", "pyproject.toml": ""}, "uv", ""},
		{map[string]string{"poetry.lock": "", "pyproject.toml": ""}, "poetry", ""},
		{map[string]string{"Pipfile.lock": "{}"}, "pipenv", ""},
		{map[string]string{"requirements.txt": "flask==3.0.0\n", "requirements-dev.txt": ""}, "requirements", ""},
		{map[string]string{"requirements.txt": "-e .\n"}, "", "local package"},
		{map[string]string{"pyproject.toml": "[project]\ndependencies = [\"httpx\"]\n"}, "pyproject", ""},
		{map[string]string{"setup.py": ""}, "", "setup.py"},
		{map[string]string{"main.py": ""}, "none", ""},
	}
	for _, c := range cases {
		ds, err := detectDeps(project(t, c.files))
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%v: want error %q, got %v", c.files, c.err, err)
			}
			continue
		}
		if err != nil || ds.Kind != c.kind {
			t.Errorf("%v: got %v %v, want %s", c.files, ds, err, c.kind)
		}
	}
	ds, _ := detectDeps(project(t, map[string]string{"requirements.txt": "", "requirements-prod.txt": "", "constraints.txt": "", "notes.txt": ""}))
	if strings.Join(ds.Files, " ") != "requirements.txt constraints.txt requirements-prod.txt" {
		t.Errorf("inputs: %v", ds.Files)
	}
}

func TestPipfileAndPins(t *testing.T) {
	lines, err := pipfileRequirements([]byte(`{"default":{"flask":{"version":"==3.0.0","hashes":["sha256:aa","sha256:bb"]},
		"uvicorn":{"version":"==0.30.0","extras":["standard"],"markers":"python_version >= '3.8'"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := "flask==3.0.0 --hash=sha256:aa --hash=sha256:bb|uvicorn[standard]==0.30.0 ; python_version >= '3.8'"
	if strings.Join(lines, "|") != want {
		t.Fatalf("%q", lines)
	}
	got := unpinned([]byte("flask==3.0\nrequests>=2\n# comment\nnumpy\n-r base.txt\npkg @ https://x/y.whl\nattrs ; python_version<'3.12'\n"))
	if strings.Join(got, ",") != "requests,numpy,attrs" {
		t.Fatalf("%v", got)
	}
}

func TestStartCommand(t *testing.T) {
	fastapi := "from fastapi import FastAPI\napi = FastAPI()\n"
	flask := "from flask import Flask\napp = Flask(__name__)\n"
	cases := []struct {
		name  string
		files map[string]string
		args  string
		warn  bool
		err   string
	}{
		{"procfile gunicorn with $PORT", map[string]string{"Procfile": "web: gunicorn app:app --bind 0.0.0.0:$PORT\nworker: celery -A app worker", "requirements.txt": "gunicorn==22\n"},
			"-m gunicorn app:app --bind 0.0.0.0:8080", false, ""},
		{"procfile python", map[string]string{"Procfile": "web: python -u bot.py"}, "-u bot.py", false, ""},
		{"procfile missing dep warns", map[string]string{"Procfile": "web: uvicorn main:api", "requirements.txt": "fastapi==0.115\n"}, "-m uvicorn main:api", true, ""},
		{"procfile shell", map[string]string{"Procfile": "web: cd src && gunicorn app:app"}, "", false, "shell"},
		{"procfile unknown tool", map[string]string{"Procfile": "web: mytool serve"}, "", false, "python -m"},
		{"django gunicorn", map[string]string{"manage.py": `os.environ.setdefault("DJANGO_SETTINGS_MODULE", "shop.settings")`, "requirements.txt": "Django==5.1\ngunicorn==23.0\n"},
			"-m gunicorn shop.wsgi:application --bind 0.0.0.0:8080", false, ""},
		{"django without server", map[string]string{"manage.py": `os.environ.setdefault('DJANGO_SETTINGS_MODULE', 'shop.settings')`, "requirements.txt": "Django==5.1\n"}, "", false, "gunicorn"},
		{"fastapi uvicorn", map[string]string{"main.py": fastapi, "requirements.txt": "fastapi==0.115\nuvicorn==0.32\n"}, "-m uvicorn main:api --host 0.0.0.0 --port 8080", false, ""},
		{"fastapi standard extra", map[string]string{"app/main.py": fastapi, "requirements.txt": "fastapi[standard]==0.115\n"}, "-m uvicorn app.main:api --host 0.0.0.0 --port 8080", false, ""},
		{"fastapi no server", map[string]string{"main.py": fastapi, "requirements.txt": "fastapi==0.115\n"}, "", false, "uvicorn"},
		{"flask gunicorn", map[string]string{"app.py": flask, "requirements.txt": "Flask==3.0\ngunicorn==23\n"}, "-m gunicorn app:app --bind 0.0.0.0:8080", false, ""},
		{"flask dev server warns", map[string]string{"app.py": flask, "requirements.txt": "Flask==3.0\n"}, "-m flask --app app:app run --host 0.0.0.0 --port 8080", true, ""},
		{"plain script", map[string]string{"main.py": "print('hi')", "requirements.txt": "requests==2\n"}, "main.py", false, ""},
		{"nothing", map[string]string{"requirements.txt": "x==1"}, "", false, "no way to start"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args, _, _, warn, err := startCommand(project(t, c.files))
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("want error %q, got %v %v", c.err, args, err)
				}
				return
			}
			if err != nil || strings.Join(args, " ") != c.args || (warn != "") != c.warn {
				t.Fatalf("got %q warn=%q err=%v, want %q", args, warn, err, c.args)
			}
		})
	}
}

func TestDetect(t *testing.T) {
	if d, _ := (Plugin{}).Detect(project(t, map[string]string{"requirements.txt": ""})); d == nil || d.Confidence != 85 {
		t.Fatalf("%+v", d)
	}
	if d, _ := (Plugin{}).Detect(project(t, map[string]string{"main.py": ""})); d == nil || d.Confidence != 30 {
		t.Fatalf("%+v", d)
	}
	if d, _ := (Plugin{}).Detect(project(t, map[string]string{"index.html": ""})); d != nil {
		t.Fatal("claimed a static site")
	}
	if u := download(11, "arm64").URL; u != ReleaseBase+"/20260718/cpython-3.11.15%2B20260718-aarch64-unknown-linux-gnu-install_only_stripped.tar.gz" {
		t.Fatal(u)
	}
}

func tarball(t *testing.T, entries map[string]string, links map[string]string) string {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range entries {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	for name, target := range links {
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: target})
	}
	tw.Close()
	zw.Close()
	p := filepath.Join(t.TempDir(), "py.tar.gz")
	os.WriteFile(p, buf.Bytes(), 0o644)
	return p
}

func TestExtractPython(t *testing.T) {
	tb := tarball(t, map[string]string{"python/bin/python3.12": "elf", "python/lib/python3.12/os.py": "",
		"python/lib/python3.12/test/test_os.py": "", "python/lib/python3.12/site-packages/pip/__init__.py": "", "python/include/Python.h": ""},
		map[string]string{"python/bin/python3": "python3.12"})
	dir := filepath.Join(t.TempDir(), "out")
	if err := extractPython(tb, dir, 12); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"bin/python3.12", "lib/python3.12/os.py"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("missing %s", want)
		}
	}
	for _, gone := range []string{"lib/python3.12/test", "lib/python3.12/site-packages/pip", "include"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); err == nil {
			t.Errorf("%s should be left out", gone)
		}
	}
	if l, _ := os.Readlink(filepath.Join(dir, "bin/python3")); l != "python3.12" {
		t.Errorf("symlink: %q", l)
	}
	evil := tarball(t, map[string]string{"python/../../etc/x": "pwned"}, nil)
	if err := extractPython(evil, filepath.Join(t.TempDir(), "e"), 12); err == nil {
		t.Fatal("path traversal accepted")
	}
}
