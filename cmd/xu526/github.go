package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"xu526/internal/platform"
)

// ---- stored sessions ----
//
// ~/.config/xu/credentials.json (0600) maps each xud API address to an xu
// session token. GitHub tokens never reach the CLI: xud keeps them.

func credentialsPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "xu", "credentials.json")
}

func loadCredentials() map[string]string {
	m := map[string]string{}
	if b, err := os.ReadFile(credentialsPath()); err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

func saveCredentials(m map[string]string) error {
	p := credentialsPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// sessionToken is XU_TOKEN, or the saved session for this API.
func sessionToken() string {
	if t := os.Getenv("XU_TOKEN"); t != "" {
		return t
	}
	return loadCredentials()[apiBase()]
}

func authorize(req *http.Request) {
	if t := sessionToken(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
}

const githubUsage = `GitHub is optional: everything else in xu works without it. Signing in
links your GitHub account so xu526 can read the repositories you choose.

xu login                         sign in with GitHub (device code)
xu logout [--everywhere]         sign out here (or revoke xu's GitHub access everywhere)
xu whoami                        who you're signed in as
xu github repos                  repositories you can connect through the GitHub App
xu source ls                     connected repositories
xu source connect OWNER/REPO --app NAME [--branch B] [--path DIR] [--port N] [--auto]
xu source connect OWNER/REPO --stack NAME [--path docker-compose.yml] [--auto]
                                 new commits are found every 30s; without --auto
                                 they wait for you (xu source deploy NAME)
xu source check NAME             look for a new commit now
xu source deploy NAME            deploy the newest commit (or the waiting one)
xu source migrate NAME           move a legacy token source onto the GitHub App
xu source rm NAME                disconnect (the app keeps running)
xu audit verify                  check the audit log's hash chain
`

func login(args []string) error {
	var start struct {
		ID              string `json:"id"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		Interval        int    `json:"interval"`
		ExpiresIn       int    `json:"expires_in"`
	}
	var gh struct {
		Configured bool   `json:"configured"`
		SetupURL   string `json:"setup_url"`
	}
	if err := call("GET", "/v1/github", nil, &gh); err == nil && !gh.Configured {
		msg := "this daemon has no GitHub App configured"
		if gh.SetupURL != "" {
			msg += "; create one at " + apiBase() + gh.SetupURL
		}
		return errors.New(msg)
	}
	if err := call("POST", "/v1/auth/device", nil, &start); err != nil {
		return err
	}
	fmt.Printf("Open %s and enter the code:\n\n    %s\n\nWaiting for GitHub ...\n", start.VerificationURI, start.UserCode)
	interval := time.Duration(start.Interval) * time.Second
	deadline := time.Now().Add(time.Duration(start.ExpiresIn) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(interval)
		var res struct {
			Status   string `json:"status"`
			Token    string `json:"token"`
			Login    string `json:"login"`
			Interval int    `json:"interval"`
			Error    string `json:"error"`
		}
		err := call("POST", "/v1/auth/device/poll", map[string]string{"id": start.ID}, &res)
		if err != nil {
			return err
		}
		switch res.Status {
		case "pending":
			if res.Interval > 0 {
				interval = time.Duration(res.Interval) * time.Second
			}
			continue
		case "ok":
			creds := loadCredentials()
			creds[apiBase()] = res.Token
			if err := saveCredentials(creds); err != nil {
				return err
			}
			fmt.Printf("Signed in to %s as %s.\n", apiBase(), res.Login)
			return nil
		}
		return fmt.Errorf("%s", firstNonEmptyStr(res.Error, res.Status))
	}
	return errors.New("the code expired; run xu login again")
}

func logout(args []string) error {
	everywhere := len(args) > 0 && (args[0] == "--everywhere" || args[0] == "-everywhere")
	err := call("POST", "/v1/auth/logout", map[string]bool{"everywhere": everywhere}, nil)
	creds := loadCredentials()
	delete(creds, apiBase())
	saveCredentials(creds)
	if err != nil && !strings.Contains(err.Error(), "sign in") {
		return err
	}
	if everywhere {
		fmt.Println("Signed out everywhere; xu's access to your GitHub account was revoked.")
	} else {
		fmt.Println("Signed out.")
	}
	return nil
}

func whoami() error {
	var u struct {
		Login    string `json:"login"`
		GitHubID int64  `json:"github_id"`
	}
	if err := call("GET", "/v1/auth/whoami", nil, &u); err != nil {
		return err
	}
	fmt.Printf("%s (GitHub user %d) on %s\n", u.Login, u.GitHubID, apiBase())
	return nil
}

func githubCmd(args []string) error {
	if len(args) == 0 || args[0] != "repos" {
		fmt.Print(githubUsage)
		return nil
	}
	var res struct {
		Installations []struct {
			ID           int64  `json:"id"`
			AccountLogin string `json:"account_login"`
			Selection    string `json:"repository_selection"`
			UserRepos    []struct {
				FullName string `json:"full_name"`
				Private  bool   `json:"private"`
			} `json:"user_repos"`
		} `json:"installations"`
		InstallURL string `json:"install_url"`
	}
	if err := call("GET", "/v1/github/installations", nil, &res); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "REPOSITORY\tVISIBILITY\tINSTALLED ON\tACCESS")
	n := 0
	for _, in := range res.Installations {
		for _, r := range in.UserRepos {
			vis := "public"
			if r.Private {
				vis = "private"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s repositories\n", r.FullName, vis, in.AccountLogin, in.Selection)
			n++
		}
	}
	tw.Flush()
	if n == 0 {
		fmt.Println("(none yet)")
	}
	fmt.Printf("\nTo add repositories, install the app or change its repository access: %s\n", res.InstallURL)
	return nil
}

func findSource(name string) (platform.Source, error) {
	var list []platform.Source
	if err := call("GET", "/v1/sources", nil, &list); err != nil {
		return platform.Source{}, err
	}
	for _, s := range list {
		if s.Target == name {
			return s, nil
		}
	}
	return platform.Source{}, fmt.Errorf("no source connected to %s", name)
}

func sourceCmd(args []string) error {
	if len(args) == 0 {
		fmt.Print(githubUsage)
		return nil
	}
	switch args[0] {
	case "ls", "list":
		var list []platform.Source
		if err := call("GET", "/v1/sources", nil, &list); err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "TARGET\tKIND\tREPOSITORY\tBRANCH\tAUTH\tSTATUS")
		legacy := false
		for _, s := range list {
			repo := firstNonEmptyStr(s.RepoFullName, s.Repo)
			status := firstNonEmptyStr(s.LastStatus, "-")
			if s.Disabled != "" {
				status = "paused: " + s.Disabled
			} else if s.Pending != nil {
				status += ", new commit " + shortSHA(s.Pending.SHA) + " waiting (xu source deploy " + s.Target + ")"
			}
			auth := s.Auth
			if auth == "token" {
				auth, legacy = "token (legacy)", true
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", s.Target, s.Kind, repo, s.Branch, auth, status)
		}
		tw.Flush()
		if legacy {
			fmt.Println("\nLegacy sources keep a GitHub token on the server. Move them to the GitHub App: xu source migrate NAME")
		}
		return nil
	case "connect":
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return errors.New("usage: xu source connect OWNER/REPO --app NAME")
		}
		repo := args[1]
		fs := flag.NewFlagSet("connect", flag.ContinueOnError)
		appName := fs.String("app", "", "")
		stackName := fs.String("stack", "", "")
		branch := fs.String("branch", "", "")
		path := fs.String("path", "", "")
		port := fs.Int("port", 0, "")
		autoFlag := fs.Bool("auto", false, "")
		_ = fs.Bool("manual", false, "") // the default now; kept for old scripts
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		req := platform.GitHubSourceRequest{Repo: repo, Branch: *branch, Path: *path}
		auto := *autoFlag
		req.AutoDeploy = &auto
		switch {
		case *appName != "" && *stackName == "":
			req.Kind, req.Target = "app", *appName
			req.App = &platform.AppSpec{Port: *port, MaxScale: 10, Target: 50, IdleTimeoutSec: 300} // port 0: the image's own
		case *stackName != "" && *appName == "":
			req.Kind, req.Target = "stack", *stackName
		default:
			return errors.New("give --app NAME or --stack NAME")
		}
		var s platform.Source
		if err := call("POST", "/v1/sources/github", req, &s); err != nil {
			return err
		}
		fmt.Printf("Connected %s (%s) to %s %s. Building now.\n", s.RepoFullName, s.Branch, s.Kind, s.Target)
		if auto {
			fmt.Printf("New commits on %s are deployed as soon as they're found (checked every 30 seconds).\n", s.Branch)
		} else {
			fmt.Printf("New commits on %s are checked for every 30 seconds and wait for you: xu source deploy %s\n", s.Branch, s.Target)
		}
		fmt.Println("Follow it with: xu releases", s.Target)
		return nil
	case "check", "deploy":
		if len(args) < 2 {
			return fmt.Errorf("usage: xu source %s NAME", args[0])
		}
		src, err := findSource(args[1])
		if err != nil {
			return err
		}
		if args[0] == "deploy" {
			if err := call("POST", "/v1/sources/"+src.ID+"/deploy", nil, nil); err != nil {
				return err
			}
			if src.Pending != nil {
				fmt.Printf("Building %s %q. Follow it with: xu releases %s\n", shortSHA(src.Pending.SHA), firstLine(src.Pending.Message), src.Target)
			} else {
				fmt.Printf("Building the newest commit on %s. Follow it with: xu releases %s\n", src.Branch, src.Target)
			}
			return nil
		}
		var out platform.Source
		err = call("POST", "/v1/sources/"+src.ID+"/check", nil, &out)
		switch {
		case out.Pending != nil:
			fmt.Printf("New commit on %s: %s %q by %s. Deploy it with: xu source deploy %s\n", out.Branch, shortSHA(out.Pending.SHA),
				firstLine(out.Pending.Message), firstNonEmptyStr(out.Pending.Author, "?"), out.Target)
		case err != nil:
			return err
		case out.CheckError != "":
			return fmt.Errorf("checking %s: %s", out.Target, out.CheckError)
		default:
			fmt.Printf("%s is up to date with %s (%s).\n", out.Target, out.Branch, shortSHA(out.LastCommit))
		}
		return nil
	case "migrate":
		if len(args) < 2 {
			return errors.New("usage: xu source migrate NAME")
		}
		s, err := findSource(args[1])
		if err != nil {
			return err
		}
		var out platform.Source
		if err := call("POST", "/v1/sources/"+s.ID+"/migrate", nil, &out); err != nil {
			return err
		}
		fmt.Printf("%s now builds %s through the GitHub App; its stored token was deleted.\n", out.Target, out.RepoFullName)
		fmt.Printf("Delete the old webhook in https://github.com/%s/settings/hooks, and revoke the token if nothing else uses it.\n", out.RepoFullName)
		return nil
	case "rm", "disconnect":
		if len(args) < 2 {
			return errors.New("usage: xu source rm NAME")
		}
		s, err := findSource(args[1])
		if err != nil {
			return err
		}
		if err := call("DELETE", "/v1/sources/"+s.ID, nil, nil); err != nil {
			return err
		}
		fmt.Printf("Disconnected %s; it keeps running its current release.\n", args[1])
		return nil
	}
	fmt.Print(githubUsage)
	return nil
}

func auditCmd(args []string) error {
	if len(args) == 0 || args[0] != "verify" {
		return errors.New("usage: xu audit verify")
	}
	var res struct {
		Records int    `json:"records"`
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		Path    string `json:"path"`
	}
	if err := call("GET", "/v1/audit/verify", nil, &res); err != nil {
		return err
	}
	if !res.OK {
		return fmt.Errorf("audit log %s is broken after %d records: %s", res.Path, res.Records, res.Error)
	}
	fmt.Printf("audit log %s: %d records, chain intact\n", res.Path, res.Records)
	return nil
}

func firstNonEmptyStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}
