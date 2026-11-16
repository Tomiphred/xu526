package python

import (
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strings"

	"xu526/internal/bundle"
)

// servers maps commands to the module that runs them with `python -m`,
// since console-script launchers don't survive being installed for another
// machine and the image has no shell.
var servers = map[string]string{
	"gunicorn": "gunicorn", "uvicorn": "uvicorn", "hypercorn": "hypercorn", "daphne": "daphne",
	"waitress-serve": "waitress", "flask": "flask", "streamlit": "streamlit", "fastapi": "fastapi",
	"celery": "celery", "granian": "granian", "gradio": "gradio", "panel": "panel", "rq": "rq",
}

var portVar = regexp.MustCompile(`\$\{?PORT\}?`)

// procfileCommand reads the web process (or the only process) of a Procfile.
func procfileCommand(src *bundle.Source) (string, bool) {
	b, err := fs.ReadFile(src.FS, "Procfile")
	if err != nil {
		return "", false
	}
	var first string
	for _, line := range strings.Split(string(b), "\n") {
		name, cmd, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || strings.HasPrefix(name, "#") {
			continue
		}
		if strings.TrimSpace(name) == "web" {
			return strings.TrimSpace(cmd), true
		}
		if first == "" {
			first = strings.TrimSpace(cmd)
		}
	}
	return first, first != ""
}

// commandArgs turns a command line into python arguments.
func commandArgs(cmdline string) ([]string, map[string]string, error) {
	cmdline = portVar.ReplaceAllString(cmdline, "8080")
	words, err := bundle.SplitCommand(cmdline)
	if err != nil {
		return nil, nil, err
	}
	env := map[string]string{}
	for len(words) > 0 && strings.Contains(words[0], "=") && !strings.HasPrefix(words[0], "-") {
		k, v, _ := strings.Cut(words[0], "=")
		env[k] = v
		words = words[1:]
	}
	if len(words) == 0 {
		return nil, nil, fmt.Errorf("empty command")
	}
	w := words[0]
	if w == "python" || w == "python3" || regexp.MustCompile(`^python3\.\d+$`).MatchString(w) {
		if len(words) == 1 {
			return nil, nil, fmt.Errorf("runs python with no script")
		}
		return words[1:], env, nil
	}
	if mod, ok := servers[w]; ok {
		return append([]string{"-m", mod}, words[1:]...), env, nil
	}
	return nil, nil, fmt.Errorf("%q isn't a command xu knows how to run without a shell; use `python -m <module> ...`", w)
}

var (
	appVar      = regexp.MustCompile(`(?m)^(\w+)\s*=\s*(FastAPI|Starlette|Flask|Quart|Litestar|Sanic|Falcon\.App|falcon\.App|falcon\.asgi\.App)\(`)
	djangoProj  = regexp.MustCompile(`DJANGO_SETTINGS_MODULE["']\s*,\s*["']([\w.]+)\.settings`)
	candidates  = []string{"main.py", "app.py", "server.py", "api.py", "wsgi.py", "asgi.py", "app/main.py", "app/app.py", "app/__init__.py", "src/main.py", "src/app.py"}
	asgiKinds   = map[string]bool{"FastAPI": true, "Starlette": true, "Quart": true, "Litestar": true, "falcon.asgi.App": true}
	scriptFiles = []string{"main.py", "app.py", "server.py", "run.py", "__main__.py"}
)

func module(file string) string {
	m := strings.TrimSuffix(file, ".py")
	m = strings.TrimSuffix(m, "/__init__")
	m = strings.TrimPrefix(m, "src/")
	return strings.ReplaceAll(m, "/", ".")
}

// startCommand decides how the image starts the app.
func startCommand(src *bundle.Source) (args []string, env map[string]string, desc string, warn string, err error) {
	text := depsText(src)
	if cmd, ok := procfileCommand(src); ok {
		args, env, err := commandArgs(cmd)
		if err != nil {
			return nil, nil, "", "", fmt.Errorf("Procfile: %q: %v", cmd, err)
		}
		if args[0] == "-m" && !hasDep(text, args[1]) && !(args[1] == "uvicorn" && hasDep(text, "fastapi[standard]")) {
			warn = fmt.Sprintf("the Procfile runs %s, which isn't listed in the dependencies", args[1])
		}
		return args, env, "Procfile: " + cmd, warn, nil
	}
	bind := []string{"--bind", "0.0.0.0:8080"}
	asgiServer := func(target string) ([]string, error) {
		switch {
		case hasDep(text, "uvicorn") || strings.Contains(text, "fastapi[standard"):
			return []string{"-m", "uvicorn", target, "--host", "0.0.0.0", "--port", "8080"}, nil
		case hasDep(text, "hypercorn"):
			return []string{"-m", "hypercorn", target, "--bind", "0.0.0.0:8080"}, nil
		case hasDep(text, "granian"):
			return []string{"-m", "granian", "--interface", "asgi", "--host", "0.0.0.0", "--port", "8080", target}, nil
		case hasDep(text, "gunicorn") && hasDep(text, "uvicorn-worker"):
			return append([]string{"-m", "gunicorn", target, "-k", "uvicorn_worker.UvicornWorker"}, bind...), nil
		}
		return nil, fmt.Errorf("found an ASGI app (%s) but no server to run it: add uvicorn to the dependencies", target)
	}
	if src.Has("manage.py") {
		b, _ := fs.ReadFile(src.FS, "manage.py")
		m := djangoProj.FindSubmatch(b)
		if m == nil {
			return nil, nil, "", "", fmt.Errorf("found manage.py but not DJANGO_SETTINGS_MODULE in it; add a Procfile with `web: gunicorn <project>.wsgi`")
		}
		proj := string(m[1])
		switch {
		case hasDep(text, "gunicorn"):
			return append([]string{"-m", "gunicorn", proj + ".wsgi:application"}, bind...), nil, "Django with gunicorn", "", nil
		case src.Has(strings.ReplaceAll(proj, ".", "/") + "/asgi.py"):
			args, err := asgiServer(proj + ".asgi:application")
			if err == nil {
				return args, nil, "Django (ASGI)", "", nil
			}
		}
		return nil, nil, "", "", fmt.Errorf("Django needs a production server: add gunicorn to the dependencies (runserver is for development)")
	}
	for _, f := range candidates {
		b, err := fs.ReadFile(src.FS, f)
		if err != nil {
			continue
		}
		m := appVar.FindSubmatch(b)
		if m == nil {
			continue
		}
		target := module(f) + ":" + string(m[1])
		kind := string(m[2])
		if asgiKinds[kind] {
			args, err := asgiServer(target)
			if err != nil {
				return nil, nil, "", "", err
			}
			return args, nil, kind + " app " + target, "", nil
		}
		switch {
		case hasDep(text, "gunicorn"):
			return append([]string{"-m", "gunicorn", target}, bind...), nil, kind + " app " + target + " with gunicorn", "", nil
		case hasDep(text, "waitress"):
			return []string{"-m", "waitress", "--listen=0.0.0.0:8080", target}, nil, kind + " app " + target + " with waitress", "", nil
		case kind == "Flask":
			return []string{"-m", "flask", "--app", target, "run", "--host", "0.0.0.0", "--port", "8080"}, nil, "Flask app " + target,
				"uses Flask's development server; add gunicorn to the dependencies for production", nil
		}
		return nil, nil, "", "", fmt.Errorf("found a %s app (%s) but no server: add gunicorn or waitress", kind, target)
	}
	for _, f := range scriptFiles {
		if src.Has(f) {
			return []string{path.Clean(f)}, nil, "script " + f, "", nil
		}
	}
	return nil, nil, "", "", fmt.Errorf("no way to start the app: add a Procfile (`web: gunicorn app:app`) or a main.py")
}
