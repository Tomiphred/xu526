// xu526 is the program people run on their computer: the background
// service (xu526 daemon) and the commands that talk to it. It is also
// installed as xu, the short name the commands use.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"xu526/internal/daemon"
	"xu526/internal/dotenv"
	"xu526/internal/platform"
	"xu526/internal/version"
)

const usage = `xu526 (also installed as xu): deploy and manage apps on your computer or server

  xu526 daemon [flags]                                 run the background service (installed as a service; xu526 daemon -h)
  xu version                                           print the version

  xu deploy NAME --image IMAGE [--port PORT] [options] deploy or update an app (port: from the image when left out)
  xu ls                                                list apps
  xu status NAME                                       show an app's instances
  xu rm NAME                                           delete an app
  xu load URL [-c 50] [-d 20s] [-host NAME.DOMAIN]     generate traffic to watch scaling

  xu releases NAME [-stack]                            list releases
  xu rollback NAME VERSION [-stack]                    redeploy an earlier release
  xu logs NAME [-n 100] [-q text]                      recent logs (all instances + access log)

  xu share NAME | xu unshare NAME                    give an app a public address through your relay
  xu tunnel login                                      connect this computer through xu526 tunnel (approve on its website)
  xu tunnel connect RELAY_URL TOKEN                    connect this computer to a relay with a token
  xu tunnel status | xu tunnel disconnect
  xu tunnel scan                                       programs listening on this computer that can be shared
  xu tunnel share NAME PORT|HOST:PORT                  share a program running on this computer (not deployed on xu526)

  xu build [DIR] [-t NAME:TAG] [-o FILE]               build an image without a Dockerfile (xu build -h)
  xu build [DIR] --deploy NAME                         build and deploy it in one step

  xu login | xu logout [--everywhere] | xu whoami      link your GitHub account (only for connecting repositories)
  xu github repos                                      repositories you can connect
  xu source connect OWNER/REPO --app NAME [--port N]   deploy every push to the branch
  xu source ls | xu source migrate NAME | xu source rm NAME
  xu audit verify                                      check the audit log is intact

  xu images [APP]                                      images built by xu and what uses them (xu images help)
  xu images inspect|rm|prune|tag|push|save|load|df     inspect, remove, clean up, copy
  xu volumes [ls|rm|prune]                             data volumes left by deleted stacks
  xu cache [df|clean]                                  the xu build cache on this machine
  xu stats [APP] [--watch]                             live CPU, memory and traffic per instance
  xu system df | xu system prune                       disk use overall, and clean up everything at once

  xu stack plan NAME [-f FILE] [-e ENV]                show what a stack file will deploy
  xu stack deploy NAME [-f FILE] [-e ENV]              build what it needs, then deploy or update it
  xu stack ls [-e ENV]                                 list stacks
  xu stack rm NAME [-e ENV]                            delete a stack (volumes are kept)
  xu env ls | create ENV | rm ENV                      environments (staging, preview, ...)
  xu env set ENV K=V ... | unset ENV K ...             an environment's variables (${{ vars.K }})
  xu env secret set ENV K | secret rm ENV K            its secrets (${{ secrets.K }}); the value is read from stdin

deploy options:
  --env K=V        environment variable (repeatable)
  --env-file FILE  read variables from a .env file (repeatable; --env wins)
  --min N          instances kept when idle (default 0 = scale to zero)
  --max N          maximum instances (default 10)
  --target N       concurrent requests per instance (default 50)
  --idle SECONDS   idle time before scaling to zero (default 300)
  --memory MB      memory limit per instance
  --cpus N         CPU limit per instance, e.g. 0.5
  --cmd "COMMAND"  run a local command instead of an image (needs xu526 daemon -allow-process)

The relay and its sign-up site are a separate program, xu526-server.
Set XU_API to reach a daemon other than http://127.0.0.1:7070 (and XU_TOKEN to use a session token directly).
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "daemon":
		err = daemon.Main(args)
	case "version", "--version", "-v":
		fmt.Println("xu526", version.Version)
		return
	case "deploy":
		err = deploy(args)
	case "ls", "list":
		err = list()
	case "status":
		err = status(args)
	case "rm", "delete":
		err = remove(args)
	case "load":
		err = load(args)
	case "stack":
		err = stack(args)
	case "env", "environment", "environments":
		err = envCmd(args)
	case "share", "unshare":
		err = share(cmd == "share", args)
	case "tunnel":
		err = tunnelCmd(args)
	case "relay":
		err = fmt.Errorf("relay users are managed on the relay with xu526-server (xu526-server users | add-user | rm-user)")
	case "releases":
		err = releases(args)
	case "rollback":
		err = rollback(args)
	case "logs":
		err = logs(args)
	case "build":
		err = build(args)
	case "login":
		err = login(args)
	case "logout":
		err = logout(args)
	case "whoami":
		err = whoami()
	case "github":
		err = githubCmd(args)
	case "source", "sources":
		err = sourceCmd(args)
	case "audit":
		err = auditCmd(args)
	case "images", "image":
		err = imagesCmd(args)
	case "volumes", "volume":
		err = volumesCmd(args)
	case "cache":
		err = cacheCmd(args)
	case "stats":
		err = statsCmd(args)
	case "system":
		err = systemCmd(args)
	default:
		fmt.Print(usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func apiBase() string {
	if v := os.Getenv("XU_API"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://127.0.0.1:7070"
}

func call(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, apiBase()+path, body)
	authorize(req)
	resp, err := (&http.Client{Timeout: 3 * time.Minute}).Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach the xu526 daemon (is it running? sudo systemctl start xu526): %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized && !strings.HasPrefix(path, "/v1/auth/") {
		return fmt.Errorf("this needs your GitHub account: run xu login first")
	}
	if resp.StatusCode >= 300 {
		var e struct{ Error string }
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("%s", e.Error)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

type envFlag map[string]string

// list returns the values as sorted NAME=value strings.
func (e envFlag) list() []string {
	out := make([]string, 0, len(e))
	for k, v := range e {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// listFlag collects a repeatable string flag.
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

// envFromFiles reads .env-style files and reports what it loaded (names
// only, never values).
func envFromFiles(dir string, files []string) (map[string]string, error) {
	vars, read, err := dotenv.Load(dir, files)
	if err != nil {
		return nil, err
	}
	if s := dotenv.Summary(vars, read); s != "" {
		fmt.Println(s)
	}
	return vars, nil
}

func (e envFlag) String() string { return "" }
func (e envFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok {
		return fmt.Errorf("want K=V, got %q", v)
	}
	e[k] = val
	return nil
}

func needName(args []string) (string, []string, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", nil, fmt.Errorf("app name required")
	}
	return args[0], args[1:], nil
}

func deploy(args []string) error {
	name, rest, err := needName(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("deploy", flag.ExitOnError)
	spec := platform.AppSpec{Name: name, Env: envFlag{}}
	fs.StringVar(&spec.Image, "image", "", "")
	fs.IntVar(&spec.Port, "port", 0, "")
	fs.Var(envFlag(spec.Env), "env", "")
	fs.IntVar(&spec.MinScale, "min", 0, "")
	fs.IntVar(&spec.MaxScale, "max", 10, "")
	fs.IntVar(&spec.Target, "target", 50, "")
	fs.IntVar(&spec.IdleTimeoutSec, "idle", 300, "")
	fs.Int64Var(&spec.MemoryMB, "memory", 0, "")
	fs.Float64Var(&spec.CPUs, "cpus", 0, "")
	command := fs.String("cmd", "", "")
	var envFiles listFlag
	fs.Var(&envFiles, "env-file", "")
	_ = fs.Parse(rest)
	if len(envFiles) > 0 {
		vars, err := envFromFiles(".", envFiles)
		if err != nil {
			return err
		}
		for k, v := range vars {
			if _, set := spec.Env[k]; !set { // --env wins over files
				spec.Env[k] = v
			}
		}
	}
	if *command != "" {
		spec.Command = strings.Fields(*command)
	}

	fmt.Printf("deploying %s ...\n", name)
	var st platform.AppStatus
	if err := call("PUT", "/v1/apps/"+name, spec, &st); err != nil {
		return err
	}
	fmt.Printf("live at %s  (%d instance running, scales %d-%d at %d concurrent requests each)\n",
		st.URL, st.Running, st.Spec.MinScale, st.Spec.MaxScale, st.Spec.Target)
	return nil
}

func list() error {
	var apps []platform.AppStatus
	if err := call("GET", "/v1/apps", nil, &apps); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSOURCE\tRUNNING\tSTARTING\tIN-FLIGHT\tSCALE\tURL\tLAST ERROR")
	for _, a := range apps {
		src := a.Spec.Image
		if src == "" {
			src = strings.Join(a.Spec.Command, " ")
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d-%d\t%s\t%s\n", a.Spec.Name, src, a.Running,
			a.Starting, a.InFlight, a.Spec.MinScale, a.Spec.MaxScale, a.URL, a.LastError)
	}
	return tw.Flush()
}

func status(args []string) error {
	name, _, err := needName(args)
	if err != nil {
		return err
	}
	var a platform.AppStatus
	if err := call("GET", "/v1/apps/"+name, nil, &a); err != nil {
		return err
	}
	fmt.Printf("%s  %s  runtime=%s  in-flight=%d  scale=%d-%d target=%d idle=%ds\n",
		a.Spec.Name, a.URL, a.Runtime, a.InFlight, a.Spec.MinScale, a.Spec.MaxScale,
		a.Spec.Target, a.Spec.IdleTimeoutSec)
	if a.LastError != "" {
		fmt.Println("last error:", a.LastError)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "INSTANCE\tADDRESS\tIN-FLIGHT\tUPTIME")
	for _, in := range a.Instances {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", in.ID, in.Addr, in.InFlight, in.Uptime)
	}
	if a.Starting > 0 {
		fmt.Fprintf(tw, "(+%d starting)\t\t\t\n", a.Starting)
	}
	return tw.Flush()
}

func remove(args []string) error {
	name, _, err := needName(args)
	if err != nil {
		return err
	}
	if err := call("DELETE", "/v1/apps/"+name, nil, nil); err != nil {
		return err
	}
	fmt.Println("deleted", name)
	return nil
}

// load fires concurrent requests and reports which instances served them.
func load(args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("URL required, e.g. xu load http://127.0.0.1:8080/ -host hello.localhost")
	}
	url := args[0]
	fs := flag.NewFlagSet("load", flag.ExitOnError)
	conc := fs.Int("c", 50, "concurrent clients")
	dur := fs.Duration("d", 20*time.Second, "duration")
	host := fs.String("host", "", "Host header (app name.domain)")
	_ = fs.Parse(args[1:])

	client := &http.Client{Timeout: time.Minute, Transport: &http.Transport{MaxIdleConnsPerHost: *conc}}
	var ok, fail atomic.Int64
	var mu sync.Mutex
	served := map[string]int{}
	deadline := time.Now().Add(*dur)
	var wg sync.WaitGroup
	for range *conc {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				req, _ := http.NewRequest("GET", url, nil)
				if *host != "" {
					req.Host = *host
				}
				resp, err := client.Do(req)
				if err != nil {
					fail.Add(1)
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode >= 300 {
					fail.Add(1)
					continue
				}
				ok.Add(1)
				mu.Lock()
				served[resp.Header.Get("X-Xu-Instance")]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	fmt.Printf("%d ok, %d failed, %.0f req/s over %v\n", ok.Load(), fail.Load(),
		float64(ok.Load())/dur.Seconds(), *dur)
	ids := make([]string, 0, len(served))
	for id := range served {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	fmt.Printf("served by %d instance(s):\n", len(ids))
	for _, id := range ids {
		fmt.Printf("  %-14s %d\n", id, served[id])
	}
	return nil
}

// ---- stacks ---------------------------------------------------------------

func printPlan(p *platform.StackPlan) {
	in := ""
	if p.Environment != "" && p.Environment != platform.DefaultEnvironment {
		in = " in " + p.Environment
	}
	name := p.Name
	if name == "" {
		name = p.Stack
	}
	custom := false
	for _, s := range p.Services {
		custom = custom || len(s.Networks) > 0
	}
	if custom {
		fmt.Printf("stack %s%s: %d services, on the networks in the file\n", name, in, len(p.Services))
	} else {
		fmt.Printf("stack %s%s: %d services on private network %s\n", name, in, len(p.Services), p.Network)
	}
	if len(p.Order) > 0 {
		fmt.Printf("deploy order: %s\n\n", strings.Join(p.Order, " -> "))
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	for _, s := range p.Services {
		where := "internal"
		if s.SharedURL != "" {
			where = "public " + s.SharedURL
		} else if s.Public {
			where = "public " + s.URL
		} else if s.Port > 0 {
			where = fmt.Sprintf("internal %s:%d", s.Name, s.Port)
		}
		var links []string
		for _, e := range p.Edges {
			if e.From == s.Name {
				l := e.To
				if e.Kind == "uses" {
					l += " (via " + e.Via + ")"
				}
				links = append(links, l)
			}
		}
		extra := ""
		if len(links) > 0 {
			extra = "talks to " + strings.Join(links, ", ")
		}
		if len(s.Networks) > 0 {
			extra += "  networks " + strings.Join(s.Networks, ", ")
		}
		for _, v := range s.Volumes {
			extra += fmt.Sprintf("  volume %s at %s", v.Volume, v.Path)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", s.Name, s.Image, where, s.Scaling, extra)
	}
	tw.Flush()
	for _, w := range p.Warnings {
		fmt.Println("warning:", w)
	}
	for _, e := range p.Errors {
		fmt.Println("error:  ", e)
	}
}

func kindPath(isStack bool) string {
	if isStack {
		return "stacks"
	}
	return "apps"
}

func releases(args []string) error {
	name, rest, err := needName(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("releases", flag.ExitOnError)
	isStack := fs.Bool("stack", false, "a stack, not an app")
	_ = fs.Parse(rest)
	var rels []platform.Release
	if err := call("GET", "/v1/"+kindPath(*isStack)+"/"+name+"/releases", nil, &rels); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "RELEASE\tSTATUS\tSOURCE\tCOMMIT\tCREATED\tNOTE")
	for _, r := range rels {
		note := r.CommitMsg
		if r.RollbackOf > 0 {
			note = fmt.Sprintf("rollback to v%d", r.RollbackOf)
		}
		if r.Error != "" {
			note = r.Error
		}
		commit := r.Commit
		if len(commit) > 7 {
			commit = commit[:7]
		}
		fmt.Fprintf(tw, "v%d\t%s\t%s\t%s\t%s\t%s\n", r.Version, r.Status, r.Source, commit, r.CreatedAt.Format("2006-01-02 15:04"), note)
	}
	return tw.Flush()
}

func rollback(args []string) error {
	name, rest, err := needName(args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("usage: xu rollback NAME VERSION")
	}
	v, err := strconv.Atoi(strings.TrimPrefix(rest[0], "v"))
	if err != nil {
		return fmt.Errorf("version must be a number like 3 or v3")
	}
	fs := flag.NewFlagSet("rollback", flag.ExitOnError)
	isStack := fs.Bool("stack", false, "a stack, not an app")
	_ = fs.Parse(rest[1:])
	var r platform.Release
	if err := call("POST", "/v1/"+kindPath(*isStack)+"/"+name+"/rollback", map[string]int{"version": v}, &r); err != nil {
		return err
	}
	fmt.Printf("v%d is live, running the image of v%d\n", r.Version, v)
	return nil
}

func logs(args []string) error {
	name, rest, err := needName(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	n := fs.Int("n", 100, "lines")
	q := fs.String("q", "", "only lines containing this text")
	_ = fs.Parse(rest)
	var out struct{ Lines []string }
	if err := call("GET", fmt.Sprintf("/v1/apps/%s/logs?tail=%d&q=%s", name, *n, url.QueryEscape(*q)), nil, &out); err != nil {
		return err
	}
	for _, l := range out.Lines {
		fmt.Println(l)
	}
	return nil
}

func share(on bool, args []string) error {
	name, _, err := needName(args)
	if err != nil {
		return err
	}
	if err := call("PUT", "/v1/apps/"+name+"/tunnel", map[string]bool{"on": on}, nil); err != nil {
		return err
	}
	if !on {
		fmt.Println(name, "is no longer shared")
		return nil
	}
	for i := 0; i < 20; i++ { // wait for the relay to answer
		var st platform.TunnelStatus
		if err := call("GET", "/v1/tunnel", nil, &st); err != nil {
			return err
		}
		if !st.Configured {
			return fmt.Errorf("this computer isn't connected to a relay: run xu tunnel login, or use the dashboard's Settings")
		}
		if u := st.URLs[name]; u != "" {
			fmt.Printf("%s is public at %s\n", name, u)
			return nil
		}
		if e := st.Errors[name]; e != "" {
			return fmt.Errorf("the relay refused %s: %s", name, e)
		}
		time.Sleep(250 * time.Millisecond)
	}
	fmt.Println(name, "will be shared as soon as the relay connection is up")
	return nil
}

func tunnelCmd(args []string) error {
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "scan":
		var ports []platform.LocalPort
		if err := call("GET", "/v1/local/scan", nil, &ports); err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "PORT\tPROGRAM\tWHERE\tWHAT\tSHARE")
		for _, l := range ports {
			what := l.Title
			if what == "" && l.HTTP {
				what = "web server"
			} else if what == "" {
				what = "not a web server"
			}
			share := "xu tunnel share NAME " + l.Upstream
			switch {
			case l.SharedAs != "":
				share = "shared as " + l.SharedAs
			case l.Upstream == "" || !l.HTTP:
				share = l.Hint
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", l.Port, l.Process, l.Side, what, share)
		}
		return tw.Flush()
	case "share":
		if len(args) < 3 {
			return fmt.Errorf("usage: xu tunnel share NAME PORT (or HOST:PORT)")
		}
		up := args[2]
		if !strings.Contains(up, ":") {
			up = "127.0.0.1:" + up
		}
		var out struct {
			URL      string `json:"url"`
			LocalURL string `json:"local_url"`
		}
		if err := call("POST", "/v1/local/share", map[string]string{"name": args[1], "upstream": up}, &out); err != nil {
			return err
		}
		if out.URL != "" {
			fmt.Printf("%s is shared at %s (forwarding to %s)\n", args[1], out.URL, up)
		} else {
			fmt.Printf("%s forwards to %s at %s; connect this computer with xu tunnel login to give it a public address\n", args[1], up, out.LocalURL)
		}
		fmt.Printf("stop with: xu rm %s\n", args[1])
		return nil
	case "connect":
		if len(args) < 3 {
			return fmt.Errorf("usage: xu tunnel connect RELAY_URL TOKEN")
		}
		if err := call("PUT", "/v1/tunnel", platform.TunnelConfig{URL: args[1], Token: args[2]}, nil); err != nil {
			return err
		}
		time.Sleep(1500 * time.Millisecond)
	case "disconnect":
		return call("PUT", "/v1/tunnel", platform.TunnelConfig{}, nil)
	case "login":
		return tunnelLogin()
	}
	var st platform.TunnelStatus
	if err := call("GET", "/v1/tunnel", nil, &st); err != nil {
		return err
	}
	switch {
	case !st.Configured:
		fmt.Println("not connected to a relay")
	case st.Connected:
		fmt.Printf("connected to %s as %s (round trip %s)\n", st.URL, st.User, st.Latency)
	default:
		fmt.Printf("connecting to %s ... %s\n", st.URL, st.Error)
	}
	for app, u := range st.URLs {
		fmt.Printf("  %s -> %s\n", app, u)
	}
	return nil
}

// tunnelLogin connects through the tunnel service: the person approves a
// code on its website and the token arrives by itself.
func tunnelLogin() error {
	var st platform.PairStatus
	if err := call("POST", "/v1/tunnel/pair", nil, &st); err != nil {
		return err
	}
	fmt.Printf("Open %s\nsign up or sign in, and check that it shows the code\n\n    %s\n\nWaiting for you to approve (Ctrl+C to stop) ...\n", st.VerifyURL, st.UserCode)
	for {
		time.Sleep(2 * time.Second)
		if err := call("GET", "/v1/tunnel/pair", nil, &st); err != nil {
			return err
		}
		switch st.State {
		case "waiting":
			continue
		case "connected":
			time.Sleep(1500 * time.Millisecond)
			var ts platform.TunnelStatus
			if call("GET", "/v1/tunnel", nil, &ts) == nil {
				fmt.Printf("connected to %s. Share an app with: xu share APP\n", ts.URL)
			}
			return nil
		default:
			return fmt.Errorf("%s", st.Error)
		}
	}
}
