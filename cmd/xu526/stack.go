package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"xu526/internal/bundle"
	"xu526/internal/bundle/cache"
	"xu526/internal/bundle/plugins"
	"xu526/internal/bundle/registry"
	"xu526/internal/bundle/runner"
	"xu526/internal/platform"
	"xu526/internal/runtime"
)

const stackUsage = `xu stack: deploy a whole project (database, backend, frontends) from one file

  xu stack plan NAME [-f FILE] [-e ENV]     show what will be built and deployed
  xu stack deploy NAME [-f FILE] [-e ENV]   build the services that need it, then deploy
  xu stack ls [-e ENV]                      list stacks
  xu stack rm NAME [-e ENV]                 delete a stack (volumes are kept)

FILE defaults to xu.stack.yml, then compose.yml, then docker-compose.yml.
-e (--environment) picks the environment; it's created if it doesn't exist,
and production is the default. The same file deploys into any environment:
each gets its own apps (NAME-ENV-SERVICE), network and volumes.

Services with build: are built on this computer before deploying: with
their folder's Dockerfile (docker or podman build), or else with xu build.
Their images are uploaded to the daemon, so XU_API can point at another
server. --no-cache rebuilds every layer of xu builds.

Inside the file, ${{ secrets.NAME }}, ${{ vars.NAME }} and
${{ services.api.url }} are filled in at deploy time; see xu env.
`

func stackFile(f string) (string, error) {
	if f != "" {
		return f, nil
	}
	for _, c := range []string{"xu.stack.yml", "xu.stack.yaml", "compose.yml", "compose.yaml", "docker-compose.yml", "docker-compose.yaml"} {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", errors.New("no stack file here: write xu.stack.yml (or give one with -f FILE)")
}

func stack(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Print(stackUsage)
		return nil
	}
	sub, rest := args[0], args[1:]
	name := ""
	if sub != "ls" && sub != "list" {
		var err error
		if name, rest, err = needName(rest); err != nil {
			return errors.New("give the stack a name, e.g. xu stack " + sub + " shop")
		}
	}
	fs := flag.NewFlagSet("stack "+sub, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, stackUsage) }
	file := fs.String("f", "", "")
	env := fs.String("e", "", "")
	fs.StringVar(env, "environment", "", "")
	noCache := fs.Bool("no-cache", false, "")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected %q (flags go after the stack name)", fs.Arg(0))
	}

	switch sub {
	case "ls", "list":
		var stacks []platform.StackStatus
		if err := call("GET", "/v1/stacks", nil, &stacks); err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "STACK\tENVIRONMENT\tSTATUS\tSERVICES\tDEPLOYED\tERROR")
		for _, s := range stacks {
			if *env != "" && s.Plan.Environment != *env {
				continue
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", s.Plan.Name, s.Plan.Environment, s.Status, strings.Join(s.Plan.Order, ", "),
				s.DeployedAt.Format("2006-01-02 15:04"), s.Error)
		}
		return tw.Flush()
	case "rm", "delete":
		key := platform.StackKey(name, *env)
		if err := call("DELETE", "/v1/stacks/"+key, nil, nil); err != nil {
			return err
		}
		fmt.Println("deleted stack", key, "(named volumes are kept)")
		return nil
	case "plan", "deploy":
	default:
		return fmt.Errorf("unknown stack command %q (see xu stack help)", sub)
	}

	path, err := stackFile(*file)
	if err != nil {
		return err
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return err
	}
	// .env and env_file are read here and sent along, so the daemon can be on another computer.
	body := map[string]any{"name": name, "yaml": string(src), "environment": *env, "files": platform.StackFiles(dir, src)}
	var plan platform.StackPlan
	if err := call("POST", "/v1/stacks/plan", body, &plan); err != nil {
		return err
	}
	printPlan(&plan)
	if len(plan.Errors) > 0 {
		return fmt.Errorf("fix the errors above before deploying")
	}
	if sub == "plan" {
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	built, err := composeBuild(ctx, &plan, dir, *noCache)
	if err != nil {
		return err
	}
	for _, s := range plan.Services {
		if s.Build == nil || built[s.Name] != "" {
			continue
		}
		fmt.Printf("\n== building %s from %s\n", s.Name, s.Build.Context)
		ref, err := buildService(ctx, dir, s, *noCache)
		if err != nil {
			return fmt.Errorf("building %s: %w", s.Name, err)
		}
		built[s.Name] = ref
	}
	if len(built) > 0 {
		body["built"] = built
	}

	where := ""
	if plan.Environment != platform.DefaultEnvironment {
		where = " in " + plan.Environment
	}
	fmt.Printf("\ndeploying %s%s: %s ...\n", plan.Name, where, strings.Join(plan.Order, " -> "))
	var out struct {
		platform.StackPlan
		Error string `json:"error"`
	}
	if err := call("PUT", "/v1/stacks/"+name, body, &out.StackPlan); err != nil {
		return err
	}
	fmt.Printf("stack %s is running%s\n", plan.Name, where)
	for _, s := range out.Services {
		switch {
		case s.SharedURL != "":
			fmt.Printf("  %-12s %s  (also %s)\n", s.Name, s.SharedURL, s.URL)
		case s.Public:
			fmt.Printf("  %-12s %s\n", s.Name, s.URL)
		}
	}
	return nil
}

// composeBuild builds every service that has a Dockerfile with one
// docker compose build, when docker compose is installed, and uploads the
// images. The rest (and everything, without docker compose) is built one
// by one by buildService.
func composeBuild(ctx context.Context, plan *platform.StackPlan, dir string, noCache bool) (map[string]string, error) {
	built := map[string]string{}
	var names []string
	for _, s := range plan.Services {
		if s.Build != nil && s.Build.HasDockerfile(filepath.Join(dir, filepath.FromSlash(s.Build.Context))) {
			names = append(names, s.Name)
		}
	}
	if len(names) == 0 {
		return built, nil
	}
	compose, err := runtime.FindCompose(ctx, false)
	if err != nil {
		fmt.Println("docker compose isn't installed: building each service on its own")
		return built, nil
	}
	file, refs, err := platform.ComposeBuildFile(plan, dir, "build", names)
	if err != nil {
		return nil, err
	}
	fmt.Printf("\n== building %s with %s\n", strings.Join(names, ", "), strings.Join(compose, " "))
	env := []string{"DOCKER_DEFAULT_PLATFORM=" + defaultPlatform(true)}
	if err := runtime.ComposeBuild(ctx, compose, env, file, dir, "xu-"+plan.Stack, names, noCache, os.Stdout); err != nil {
		return nil, err
	}
	engine := runtime.ComposeEngine(compose)
	for _, name := range names {
		app := ""
		for _, s := range plan.Services {
			if s.Name == name {
				app = s.App
			}
		}
		ref, err := uploadLocalImage(ctx, engine, refs[name], app)
		if err != nil {
			return nil, fmt.Errorf("uploading %s: %w", name, err)
		}
		built[name] = ref
	}
	return built, nil
}

// buildService builds one service and uploads the image to the daemon:
// with the folder's Dockerfile when it has one, otherwise with xu build.
func buildService(ctx context.Context, dir string, s platform.PlanService, noCache bool) (string, error) {
	ctxDir := filepath.Join(dir, filepath.FromSlash(s.Build.Context))
	if st, err := os.Stat(ctxDir); err != nil || !st.IsDir() {
		return "", fmt.Errorf("the build folder %s doesn't exist", ctxDir)
	}
	dockerfile := s.Build.Dockerfile
	if dockerfile == "" {
		if _, err := os.Stat(filepath.Join(ctxDir, "Dockerfile")); err == nil {
			dockerfile = "Dockerfile"
		}
	}
	keys := make([]string, 0, len(s.Build.Args))
	for k := range s.Build.Args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > 0 {
		fmt.Printf("build values: %s\n", strings.Join(keys, ", "))
	}
	plat := defaultPlatform(true)
	if dockerfile != "" {
		return dockerBuild(ctx, s, ctxDir, filepath.Join(ctxDir, filepath.FromSlash(dockerfile)), plat, keys)
	}

	p, err := bundle.ParsePlatform(plat)
	if err != nil {
		return "", err
	}
	store, err := cache.Default()
	if err != nil {
		return "", err
	}
	mirrors, err := bundle.ParseMirrors(os.Getenv("XU_MIRRORS"))
	if err != nil {
		return "", fmt.Errorf("XU_MIRRORS: %w", err)
	}
	var benv []string
	for _, k := range keys {
		benv = append(benv, k+"="+s.Build.Args[k])
	}
	res, err := bundle.Build(ctx, bundle.Request{
		Dir: ctxDir, Tag: s.App + ":latest", NoCache: noCache,
		Options: bundle.Options{Platform: p, BuildEnv: benv},
		Plugins: plugins.All(), Cache: store, Runner: runner.Local{}, Log: os.Stdout,
		Registry: registry.New(), Mirrors: mirrors,
	})
	if err != nil {
		return "", err
	}
	return uploadBuilt(s.App, res)
}

// dockerBuild runs docker build (or podman build) and uploads the result.
func dockerBuild(ctx context.Context, s platform.PlanService, ctxDir, dockerfile, plat string, keys []string) (string, error) {
	engine := ""
	for _, c := range []string{"docker", "podman"} {
		if _, err := exec.LookPath(c); err == nil {
			engine = c
			break
		}
	}
	if engine == "" {
		return "", fmt.Errorf("%s has a Dockerfile, which needs docker or podman on this computer to build", ctxDir)
	}
	tmp := "xu/" + s.App + ":build"
	args := []string{"build", "--platform", plat, "-t", tmp, "-f", dockerfile}
	if s.Build.Target != "" {
		args = append(args, "--target", s.Build.Target)
	}
	for _, k := range keys {
		args = append(args, "--build-arg", k+"="+s.Build.Args[k])
	}
	args = append(args, ctxDir)
	fmt.Printf("$ %s build -t %s %s\n", engine, tmp, ctxDir)
	cmd := exec.CommandContext(ctx, engine, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		hint := ""
		if engine == "docker" {
			hint = " (if it says --mount needs BuildKit, install buildx: sudo apt install docker-buildx)"
		}
		return "", fmt.Errorf("%s build failed%s", engine, hint)
	}
	return uploadLocalImage(ctx, engine, tmp, s.App)
}

// uploadLocalImage tags an image built on this computer with its ID and
// uploads it to the daemon.
func uploadLocalImage(ctx context.Context, engine, tmp, app string) (string, error) {
	idOut, err := exec.CommandContext(ctx, engine, "image", "inspect", "-f", "{{.Id}}", tmp).Output()
	if err != nil {
		return "", fmt.Errorf("%s image inspect: %w", engine, err)
	}
	id := strings.TrimSpace(string(idOut))
	short := strings.TrimPrefix(id, "sha256:")
	if len(short) > 12 {
		short = short[:12]
	}
	ref := "xu/" + app + ":" + short
	if err := exec.CommandContext(ctx, engine, "tag", tmp, ref).Run(); err != nil {
		return "", fmt.Errorf("%s tag: %w", engine, err)
	}
	save := func(w io.Writer) error {
		c := exec.CommandContext(ctx, engine, "save", ref)
		c.Stdout = w
		var stderr bytes.Buffer
		c.Stderr = &stderr
		if err := c.Run(); err != nil {
			return fmt.Errorf("%s save: %v %s", engine, err, strings.TrimSpace(stderr.String()))
		}
		return nil
	}
	return ref, uploadImage(ref, save, id)
}

// ---- environments ------------------------------------------------------------

const envUsage = `xu env: environments (production, staging, preview ...)

  xu env ls                         list environments with their stacks, apps and values
  xu env create ENV                 add one (deploying a stack into a new one creates it too)
  xu env rm ENV [-y]                delete it with its stacks and apps (volumes are kept)
  xu env set ENV K=V [K=V ...]      set variables, used in stack files as ${{ vars.K }}
  xu env unset ENV K [K ...]        remove variables
  xu env secret set ENV K           set a secret, used as ${{ secrets.K }}; the value is
                                    read from stdin (or typed, without echo)
  xu env secret rm ENV K            remove a secret

Secrets are never shown again; the dashboard and API list only their names.
Stacks pick up changed values the next time they're deployed.
`

func envCmd(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Print(envUsage)
		return nil
	}
	sub, rest := args[0], args[1:]
	need := func(n int, usage string) error {
		if len(rest) < n {
			return errors.New("usage: xu env " + usage)
		}
		return nil
	}
	switch sub {
	case "ls", "list":
		var envs []platform.EnvironmentStatus
		if err := call("GET", "/v1/environments", nil, &envs); err != nil {
			return err
		}
		for _, e := range envs {
			def := ""
			if e.Default {
				def = " (default)"
			}
			fmt.Printf("%s%s\n", e.Name, def)
			fmt.Printf("  stacks:    %s\n", listOrNone(e.Stacks))
			fmt.Printf("  apps:      %s\n", listOrNone(e.Apps))
			vars := make([]string, 0, len(e.Variables))
			for k, v := range e.Variables {
				vars = append(vars, k+"="+v)
			}
			sort.Strings(vars)
			fmt.Printf("  variables: %s\n", listOrNone(vars))
			fmt.Printf("  secrets:   %s\n", listOrNone(e.Secrets))
		}
		return nil
	case "create", "new", "add":
		if err := need(1, "create ENV"); err != nil {
			return err
		}
		if err := call("POST", "/v1/environments", map[string]string{"name": rest[0]}, nil); err != nil {
			return err
		}
		fmt.Println("created environment", rest[0])
		fmt.Printf("deploy into it with: xu stack deploy NAME -e %s\n", rest[0])
		return nil
	case "rm", "delete":
		if err := need(1, "rm ENV [-y]"); err != nil {
			return err
		}
		if !(len(rest) > 1 && (rest[1] == "-y" || rest[1] == "--yes")) {
			fmt.Printf("This deletes the environment %s with all its stacks and apps (their volumes are kept). Continue? [y/N] ", rest[0])
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
				fmt.Println("nothing deleted")
				return nil
			}
		}
		if err := call("DELETE", "/v1/environments/"+rest[0], nil, nil); err != nil {
			return err
		}
		fmt.Println("deleted environment", rest[0])
		return nil
	case "set":
		if err := need(2, "set ENV K=V [K=V ...]"); err != nil {
			return err
		}
		vars := map[string]*string{}
		for _, kv := range rest[1:] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return fmt.Errorf("%q: write NAME=value", kv)
			}
			vars[k] = &v // a new v each iteration (Go 1.22+)
		}
		return patchEnv(rest[0], vars, nil, "set "+keysOf(vars))
	case "unset":
		if err := need(2, "unset ENV K [K ...]"); err != nil {
			return err
		}
		vars := map[string]*string{}
		for _, k := range rest[1:] {
			vars[k] = nil
		}
		return patchEnv(rest[0], vars, nil, "removed "+keysOf(vars))
	case "secret", "secrets":
		if len(rest) < 3 || (rest[0] != "set" && rest[0] != "rm" && rest[0] != "unset") {
			return errors.New("usage: xu env secret set ENV NAME   or   xu env secret rm ENV NAME")
		}
		env, k := rest[1], rest[2]
		if rest[0] != "set" {
			return patchEnv(env, nil, map[string]*string{k: nil}, "removed secret "+k)
		}
		v, err := readSecret(k)
		if err != nil {
			return err
		}
		return patchEnv(env, nil, map[string]*string{k: &v}, "set secret "+k)
	}
	return fmt.Errorf("unknown env command %q (see xu env help)", sub)
}

func patchEnv(env string, vars, secrets map[string]*string, done string) error {
	if err := call("PATCH", "/v1/environments/"+env, map[string]any{"variables": vars, "secrets": secrets}, nil); err != nil {
		return err
	}
	fmt.Printf("%s: %s. Deploy its stacks again to use the new values.\n", env, done)
	return nil
}

// readSecret reads a secret's value without echoing it, or from a pipe:
// printf %s "$PASSWORD" | xu env secret set staging DB_PASSWORD
func readSecret(name string) (string, error) {
	if st, err := os.Stdin.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
		fmt.Fprintf(os.Stderr, "value for %s (not shown): ", name)
		// Turn echo off where stty exists (Linux, macOS, WSL); Windows
		// consoles show what's typed, so piping the value in is better there.
		quiet := exec.Command("stty", "-echo")
		quiet.Stdin = os.Stdin
		if quiet.Run() == nil {
			defer func() {
				on := exec.Command("stty", "echo")
				on.Stdin = os.Stdin
				_ = on.Run()
				fmt.Fprintln(os.Stderr)
			}()
		}
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", err
		}
		v := strings.TrimRight(line, "\r\n")
		if v == "" {
			return "", errors.New("empty value; nothing set")
		}
		return v, nil
	}
	b, err := io.ReadAll(io.LimitReader(os.Stdin, 64<<10))
	if err != nil {
		return "", err
	}
	v := strings.TrimRight(string(b), "\r\n")
	if v == "" {
		return "", errors.New("empty value on stdin; nothing set")
	}
	return v, nil
}

func listOrNone(l []string) string {
	if len(l) == 0 {
		return "-"
	}
	return strings.Join(l, ", ")
}

func keysOf(m map[string]*string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}
