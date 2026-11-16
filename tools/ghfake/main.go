// ghfake runs a fake GitHub for working on xu526's GitHub App integration
// without registering a real app:
//
//	go run ./tools/ghfake -addr 127.0.0.1:9099 -repo ./path/to/your/app -secrets /tmp/ghfake
//	set -a; . /tmp/ghfake/github.env; set +a; xud -host-builds ...
//	xu login            # approved automatically as "dev"
//
// With -setup there is no app yet: Connect GitHub in the dashboard creates
// one (the manifest flow), installs it and signs in, all against the fake.
//
// With -shared it plays the one public xu526 app instead: the env file has
// only the client ID and addresses (no private key, client secret or
// webhook), the way a downloaded xu526 runs.
//
// It serves the given folder as the git repository dev/<folder name>,
// installed for the app, and writes the app's secrets (0600) plus an env
// file for xud. Everything lives in memory.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"

	"xu526/internal/ghapp/ghtest"
)

func run(dir string, args ...string) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Dev", "GIT_AUTHOR_EMAIL=dev@example.com", "GIT_COMMITTER_NAME=Dev", "GIT_COMMITTER_EMAIL=dev@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func main() {
	addr := flag.String("addr", "127.0.0.1:9099", "address to serve the fake GitHub on")
	repo := flag.String("repo", "", "folder to serve as dev/<name> (copied into a git repository)")
	secrets := flag.String("secrets", "", "folder for the app's secrets and github.env")
	api := flag.String("xud-api", "http://127.0.0.1:7070", "xud's API address (for the callback URL)")
	shared := flag.Bool("shared", false, "act as the shared xu526 app: client ID only")
	setup := flag.Bool("setup", false, "no app yet: the env file only points xu526 at this fake, and Connect GitHub creates the app")
	flag.Parse()
	if *repo == "" || *secrets == "" {
		log.Fatal("give -repo and -secrets")
	}
	f := ghtest.NewHandler()
	f.URL = "http://" + *addr
	abs, _ := filepath.Abs(*repo)
	name := filepath.Base(abs)
	root, _ := os.MkdirTemp("", "ghfake-")
	bare := filepath.Join(root, "dev", name+".git")
	os.MkdirAll(filepath.Dir(bare), 0o755)
	run(root, "init", "-q", "--bare", "-b", "main", bare)
	work := filepath.Join(root, "work")
	run(root, "clone", "-q", bare, work)
	run(root, "-C", work, "checkout", "-q", "-b", "main")
	if out, err := exec.Command("cp", "-r", abs+"/.", work).CombinedOutput(); err != nil {
		log.Fatalf("copy: %v %s", err, out)
	}
	run(work, "add", ".")
	run(work, "commit", "-qm", "import "+name)
	run(work, "push", "-q", "origin", "main")
	f.Repos[1001] = &ghtest.Repo{ID: 1001, FullName: "dev/" + name, Private: true, GitDir: bare}
	f.Repos[1002] = &ghtest.Repo{ID: 1002, FullName: "dev/not-yours", Private: true}
	f.Installations[77] = &ghtest.Installation{ID: 77, Account: "dev", AccountID: 1, Selection: "selected", Repos: []int64{1001, 1002}}
	f.Users[1] = &ghtest.User{ID: 1, Login: "dev", Access: []int64{1001}}
	f.AuthAs, f.AutoApprove = 1, 1

	os.MkdirAll(*secrets, 0o700)
	key := make([]byte, 32)
	rand.Read(key)
	write := func(n, v string) string {
		p := filepath.Join(*secrets, n)
		os.WriteFile(p, []byte(v), 0o600)
		return p
	}
	env := fmt.Sprintf(`XU_GITHUB_APP_ID=%d
XU_GITHUB_CLIENT_ID=%s
XU_GITHUB_APP_SLUG=xu526-dev
XU_GITHUB_URL=%s
XU_GITHUB_API_URL=%s
XU_GITHUB_CALLBACK_URL=%s/github/callback
XU_GITHUB_USERS=dev
XU_GITHUB_PRIVATE_KEY_FILE=%s
XU_GITHUB_CLIENT_SECRET_FILE=%s
XU_GITHUB_WEBHOOK_SECRET_FILE=%s
XU_SECRET_KEY_FILE=%s
`, f.AppID, f.ClientID, f.URL, f.URL, *api,
		write("github-app.pem", string(f.PEM())), write("github-client-secret", f.ClientSecret),
		write("github-webhook-secret", "ghfake-webhook-secret"), write("xu-secret-key", hex.EncodeToString(key)))
	if *setup {
		env = fmt.Sprintf("XU_GITHUB_URL=%s\nXU_GITHUB_API_URL=%s\n", f.URL, f.URL)
	} else if *shared {
		env = fmt.Sprintf("XU_GITHUB_CLIENT_ID=%s\nXU_GITHUB_APP_SLUG=xu526-dev\nXU_GITHUB_URL=%s\nXU_GITHUB_API_URL=%s\nXU_GITHUB_USERS=dev\n", f.ClientID, f.URL, f.URL)
	}
	write("github.env", env)
	log.Printf("working copy (commit and push here to test new-commit checks): %s", work)
	log.Printf("fake GitHub on %s: repo dev/%s (id 1001) in installation 77; user dev signs in automatically", f.URL, name)
	log.Printf("xud environment: %s", filepath.Join(*secrets, "github.env"))
	log.Fatal(http.ListenAndServe(*addr, f.Handler()))
}
