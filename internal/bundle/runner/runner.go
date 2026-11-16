// Package runner executes build steps. Today steps run directly on the
// developer's machine; a sandboxed runner (container or microVM) can be
// added later behind the same interface, since steps declare everything
// they need: command, directory, environment and inputs.
package runner

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Step is one command to run.
type Step struct {
	Name   string
	Cmd    []string
	Dir    string   // working directory (absolute)
	Env    []string // added to a minimal inherited environment
	Inputs []string // globs the step reads (used for cache keys and future sandboxes)
	Show   string   // short form shown in plans and logs instead of the full command
}

// Runner executes steps.
type Runner interface {
	Run(ctx context.Context, s Step, log io.Writer) error
}

// Local runs steps as ordinary processes on this machine.
type Local struct{}

// inherited are the host variables toolchains need to find themselves and
// their caches. Everything else is left out so builds don't depend on
// whatever happens to be in the developer's shell.
var inherited = []string{"PATH", "HOME", "USER", "TMPDIR", "GOPATH", "GOCACHE", "GOMODCACHE", "GOPROXY",
	"GOPRIVATE", "GONOSUMDB", "GOFLAGS", "GOTOOLCHAIN", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "no_proxy", "SSL_CERT_FILE", "UV_CACHE_DIR", "XDG_CACHE_HOME",
	"NODE_EXTRA_CA_CERTS", "PNPM_HOME", "YARN_CACHE_FOLDER", "COREPACK_HOME", "PIP_INDEX_URL", "PIP_CACHE_DIR",
	// Windows: programs (npm, git, go) need these to find themselves and the system.
	"SystemRoot", "SystemDrive", "windir", "ComSpec", "PATHEXT", "USERPROFILE", "APPDATA", "LOCALAPPDATA",
	"ProgramData", "ProgramFiles", "ProgramFiles(x86)", "TEMP", "TMP", "HOMEDRIVE", "HOMEPATH",
	"NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE"}

// inheritedPrefixes cover registry and cache settings (npm_config_registry,
// NPM_CONFIG_CACHE, …) that developers set in their shells.
var inheritedPrefixes = []string{"npm_config_", "NPM_CONFIG_"}

func (Local) Run(ctx context.Context, s Step, log io.Writer) error {
	if len(s.Cmd) == 0 {
		return fmt.Errorf("step %q has no command", s.Name)
	}
	if _, err := exec.LookPath(s.Cmd[0]); err != nil {
		return fmt.Errorf("%s is not installed (needed for %s)", s.Cmd[0], s.Name)
	}
	cmd := exec.CommandContext(ctx, s.Cmd[0], s.Cmd[1:]...)
	cmd.Dir = s.Dir
	for _, k := range inherited {
		if v, ok := os.LookupEnv(k); ok {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	for _, kv := range os.Environ() {
		for _, p := range inheritedPrefixes {
			if strings.HasPrefix(kv, p) {
				cmd.Env = append(cmd.Env, kv)
			}
		}
	}
	cmd.Env = append(cmd.Env, s.Env...)
	cmd.Stdout, cmd.Stderr = log, log
	if s.Show != "" {
		fmt.Fprintf(log, "$ %s\n", s.Show)
	} else {
		fmt.Fprintf(log, "$ %s\n", strings.Join(s.Cmd, " "))
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s failed: %w", s.Name, err)
	}
	return nil
}
