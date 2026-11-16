package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strconv"
	"strings"

	"xu526/internal/bundle"
	"xu526/internal/bundle/cache"
	"xu526/internal/bundle/plugins"
	"xu526/internal/bundle/registry"
	"xu526/internal/bundle/runner"
	"xu526/internal/dotenv"
	"xu526/internal/platform"
)

const buildUsage = `xu build: turn a project into a container image, no Dockerfile needed

  xu build [DIR] [options]

options:
  -t NAME:TAG          image name (default: the folder name, tag latest)
  -o FILE              where to write the image tarball (default: NAME.tar)
  --platform OS/ARCH   linux/amd64 or linux/arm64 (default: the server's CPU with
                       --deploy, otherwise this machine's)
  --lang NAME          skip detection and use this plugin (go, node, python, c, static)
  --main PATH          which program to package when a project has several (Go, C)
  --app DIR            in a monorepo: the app to build, relative to DIR (the root);
                       ("xu build apps/web" finds the workspace by itself)
  --no-cache           rebuild every layer
  --dry-run            show the plan without building
  --push REF           push to a registry, e.g. ghcr.io/me/api:1.2 (no tarball unless -o)
  --deploy NAME        load the image into the daemon and deploy it as app NAME (no tarball unless -o);
                       an existing app keeps its scaling, env and domains, only the image changes
  --env K=V            with --deploy: set an environment variable (repeatable)
  --env-file FILE      with --deploy: read more variables from FILE (repeatable)
  --no-env-file        with --deploy: don't read the project's .env files
  -e, --environment E  with --deploy: deploy into environment E as NAME-E (created if needed)
  --build-arg K=V      a value the build can read (e.g. NEXT_PUBLIC_API_URL), repeatable;
                       front-end frameworks bake these into the code they build

With --deploy, the project's .env, .env.production, .env.local and
.env.production.local are read (later files win) and given to the app as
environment variables. They are never copied into the image.
  --update-bases       re-resolve base image tags instead of using xu.lock
  --plain-http HOST    reach this registry over http (local registries)

The tarball loads with: docker load -i FILE   or   podman load -i FILE
Registry logins come from ~/.docker/config.json (docker login, credential helpers).
Base images are pinned per project in xu.lock; commit it with your code.
XU_MIRRORS="gcr.io=mirror.local:5000" pulls base images through a mirror.
SOURCE_DATE_EPOCH sets the timestamp written into the image (default 0).
`

var nonName = regexp.MustCompile(`[^a-z0-9._-]+`)

func build(args []string) error {
	dir := "."
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		dir, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, buildUsage) }
	tag := fs.String("t", "", "")
	out := fs.String("o", "", "")
	plat := fs.String("platform", "", "")
	lang := fs.String("lang", "", "")
	main := fs.String("main", "", "")
	appDir := fs.String("app", "", "")
	noCache := fs.Bool("no-cache", false, "")
	dry := fs.Bool("dry-run", false, "")
	push := fs.String("push", "", "")
	updateBases := fs.Bool("update-bases", false, "")
	deployName := fs.String("deploy", "", "")
	envVars := envFlag{}
	fs.Var(envVars, "env", "")
	var envFiles listFlag
	fs.Var(&envFiles, "env-file", "")
	noEnvFile := fs.Bool("no-env-file", false, "")
	environment := fs.String("environment", "", "")
	fs.StringVar(environment, "e", "", "")
	buildArgs := envFlag{}
	fs.Var(buildArgs, "build-arg", "")
	plainHTTP := fs.String("plain-http", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		if dir != "." || fs.NArg() > 1 {
			return errors.New("give one project folder")
		}
		dir = fs.Arg(0)
	}
	if *push != "" {
		if _, err := registry.ParseRef(*push); err != nil {
			return fmt.Errorf("--push: %w", err)
		}
	}
	if *plat == "" {
		*plat = defaultPlatform(*deployName != "" && !*dry)
	}
	p, err := bundle.ParsePlatform(*plat)
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if *tag == "" {
		name := filepath.Base(abs)
		if *appDir != "" {
			name = filepath.Base(filepath.Clean(*appDir))
		}
		*tag = strings.Trim(nonName.ReplaceAllString(strings.ToLower(name), "-"), "-.") + ":latest"
	} else if !strings.Contains((*tag)[strings.LastIndex(*tag, "/")+1:], ":") {
		*tag += ":latest"
	}
	if *deployName != "" && *environment != "" && *environment != platform.DefaultEnvironment {
		*deployName += "-" + *environment
	}
	if *deployName != "" && !appName.MatchString(*deployName) {
		return fmt.Errorf("--deploy: %q isn't a valid app name (lowercase letters, digits and dashes)", *deployName)
	}
	if *out == "" && !*dry && *push == "" && *deployName == "" {
		name := (*tag)[strings.LastIndex(*tag, "/")+1:]
		*out = strings.SplitN(name, ":", 2)[0] + ".tar"
	}
	var epoch int64
	if s := os.Getenv("SOURCE_DATE_EPOCH"); s != "" {
		if epoch, err = strconv.ParseInt(s, 10, 64); err != nil {
			return fmt.Errorf("SOURCE_DATE_EPOCH must be a number of seconds")
		}
	}
	store, err := cache.Default()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	mirrors, err := bundle.ParseMirrors(os.Getenv("XU_MIRRORS"))
	if err != nil {
		return fmt.Errorf("XU_MIRRORS: %w", err)
	}
	client := registry.New()
	if *plainHTTP != "" {
		client.PlainHTTP = map[string]bool{*plainHTTP: true}
	}
	res, err := bundle.Build(ctx, bundle.Request{
		Dir: abs, Tag: *tag, Output: *out, Lang: *lang, NoCache: *noCache, DryRun: *dry,
		Options: bundle.Options{Platform: p, Main: *main, App: *appDir, Epoch: epoch, BuildEnv: buildArgs.list()},
		Plugins: plugins.All(), Cache: store, Runner: runner.Local{}, Log: os.Stdout,
		Registry: client, UpdateBases: *updateBases, Mirrors: mirrors,
	})
	if err != nil {
		return err
	}
	if *dry {
		return nil
	}
	fmt.Println()
	var total int64
	for _, l := range res.Layers {
		state := "built"
		if l.Cached {
			state = "cached"
		}
		total += l.Size
		fmt.Printf("  layer %-6s %s  %8s  %s\n", l.Name, l.Digest[:19], human(l.Size), state)
	}
	var all int64
	for _, l := range res.Image.Layers {
		all += l.Size
	}
	if all != total {
		fmt.Printf("\nimage %s  (%s compressed: %s base + %s yours, %.1fs)\n", *tag, human(all), human(all-total), human(total), res.Duration.Seconds())
	} else {
		fmt.Printf("\nimage %s  (%s compressed, %.1fs)\n", *tag, human(total), res.Duration.Seconds())
	}
	if res.Base != "" {
		fmt.Printf("  base   %s\n", res.Base)
	}
	fmt.Printf("  digest %s\n  id     %s\n", res.Image.Digest(), res.Image.ConfigDigest())
	if *out != "" {
		fmt.Printf("  wrote  %s\n", *out)
	}
	if *push != "" {
		d, err := bundle.Push(ctx, client, res.Image, *push, os.Stdout)
		if err != nil {
			return err
		}
		r, _ := registry.ParseRef(*push)
		fmt.Printf("pushed %s@%s\n", r, d)
	}
	if *deployName != "" {
		files := append([]string{}, dotenv.ProductionFiles...)
		if *noEnvFile {
			files = nil
		}
		envDir := abs // the app's own .env files, also in a monorepo
		if *appDir != "" {
			envDir = filepath.Join(abs, *appDir)
		}
		fileVars, err := envFromFiles(envDir, append(files, envFiles...))
		if err != nil {
			return err
		}
		for k, v := range envVars { // --env wins over files
			fileVars[k] = v
		}
		return deployBuilt(*deployName, *environment, res, fileVars)
	}
	if *out != "" {
		fmt.Printf("\nrun it:\n  docker load -i %s && docker run --rm -p 8080:%d %s\n", *out, res.Plan.Port, *tag)
	}
	return nil
}

var appName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// uploadBuilt sends an image from xu build to the daemon as
// xu/NAME:<digest>. The tag is the image's own digest, so every build that
// changes anything is a distinct release that can be rolled back to.
func uploadBuilt(name string, res *bundle.Result) (string, error) {
	ref := "xu/" + name + ":" + strings.TrimPrefix(res.Image.Digest(), "sha256:")[:12]
	return ref, uploadImage(ref, func(w io.Writer) error { return res.Image.WriteTar(w, ref) },
		res.Image.ConfigDigest(), res.Image.Digest())
}

// uploadImage streams an image tarball to the daemon, which loads it into
// its container engine and checks it's the image with one of ids.
func uploadImage(ref string, write func(io.Writer) error, ids ...string) error {
	fmt.Printf("uploading %s to the xu526 daemon at %s ...\n", ref, apiBase())
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(write(pw)) }()
	q := url.Values{"ref": {ref}, "id": ids}
	req, _ := http.NewRequest("POST", apiBase()+"/v1/images?"+q.Encode(), pr)
	req.Header.Set("Content-Type", "application/x-tar")
	authorize(req)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach the xu526 daemon (is it running? sudo systemctl start xu526): %w", err)
	}
	defer resp.Body.Close()
	var loaded struct{ Ref, ID, Error string }
	json.NewDecoder(resp.Body).Decode(&loaded)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the daemon refused the image: %s", loaded.Error)
	}
	return nil
}

// deployBuilt uploads the image to xud, then deploys it.
func deployBuilt(name, environment string, res *bundle.Result, env map[string]string) error {
	ref, err := uploadBuilt(name, res)
	if err != nil {
		return err
	}

	// Keep an existing app's settings; start new apps with the usual defaults.
	spec := platform.AppSpec{Name: name, MinScale: 0, MaxScale: 10, Target: 50, IdleTimeoutSec: 300}
	var cur platform.AppStatus
	existing := call("GET", "/v1/apps/"+name, nil, &cur) == nil
	if existing && cur.Spec.Stack != "" {
		return fmt.Errorf("%s is part of stack %s; deploy the stack instead", name, cur.Spec.Stack)
	}
	if existing {
		spec = cur.Spec
		spec.ImageID, spec.ImageDigest, spec.Command = "", "", nil
	}
	spec.Image, spec.Port = ref, res.Plan.Port
	if environment != "" && environment != platform.DefaultEnvironment {
		spec.Environment = environment
	}
	if spec.Kind == "" {
		spec.Kind = platform.GuessKind(res.Plan)
	}
	if len(env) > 0 {
		merged := map[string]string{}
		for k, v := range spec.Env {
			merged[k] = v
		}
		for k, v := range env { // the project's files and --env update the app's variables
			merged[k] = v
		}
		spec.Env = merged
	}
	verb := "deploying new app"
	if existing {
		verb = "updating"
	}
	fmt.Printf("%s %s ...\n", verb, name)
	var st platform.AppStatus
	if err := call("PUT", "/v1/apps/"+name, spec, &st); err != nil {
		return err
	}
	fmt.Printf("live at %s  (%d running, scales %d-%d at %d concurrent requests each)\n",
		st.URL, st.Running, st.Spec.MinScale, st.Spec.MaxScale, st.Spec.Target)
	fmt.Printf("roll back with: xu releases %s, then xu rollback %s VERSION\n", name, name)
	return nil
}

func human(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// defaultPlatform is linux/<the server's CPU> when deploying (so an image
// built on an Apple Silicon Mac for an x86 server just works), and
// linux/<this machine's CPU> otherwise, which is what the local Docker
// (Docker Desktop on a Mac included) runs natively.
func defaultPlatform(deploying bool) string {
	arch := goruntime.GOARCH
	if deploying {
		var n struct {
			Arch       string `json:"arch"`
			EngineArch string `json:"engine_arch"`
		}
		if call("GET", "/v1/node", nil, &n) == nil {
			switch {
			case n.EngineArch != "":
				arch = n.EngineArch
			case n.Arch != "":
				arch = n.Arch
			}
		}
	}
	if arch != "amd64" && arch != "arm64" {
		arch = "amd64"
	}
	return "linux/" + arch
}
