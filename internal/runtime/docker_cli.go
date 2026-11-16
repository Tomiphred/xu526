package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// ErrNoCLI means neither docker nor podman is installed next to the engine.
var ErrNoCLI = errors.New("no docker or podman command on this computer")

// CLIBuild builds a Dockerfile with the engine's own command (docker build
// or podman build), which has BuildKit features the engine API's builder
// lacks (RUN --mount, heredocs). It returns the built image's ID, or
// ErrNoCLI when there's no command to run.
func (d *Docker) CLIBuild(ctx context.Context, dir, dockerfile, tag, target string, args map[string]string, logw io.Writer) (string, error) {
	tools := []string{"docker", "podman"}
	if d.podman {
		tools = []string{"podman", "docker"}
	}
	tool := ""
	for _, t := range tools {
		if _, err := exec.LookPath(t); err == nil {
			tool = t
			break
		}
	}
	if tool == "" {
		return "", ErrNoCLI
	}
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	cmdArgs := []string{"build", "-t", tag, "-f", dockerfile}
	if target != "" {
		cmdArgs = append(cmdArgs, "--target", target)
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cmdArgs = append(cmdArgs, "--build-arg", k+"="+args[k])
	}
	cmdArgs = append(cmdArgs, ".")
	fmt.Fprintf(logw, "$ %s %s\n", tool, strings.Join(cmdArgs, " "))
	cmd := exec.CommandContext(ctx, tool, cmdArgs...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, logw, logw
	cmd.Env = os.Environ()
	if tool == "docker" && !d.podman {
		// The same engine the daemon runs containers on.
		if h := d.dockerHost(); h != "" {
			cmd.Env = append(cmd.Env, "DOCKER_HOST="+h)
		}
	}
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s build failed: %w", tool, err)
	}
	var img struct{ Id string }
	if err := d.do(ctx, "GET", "/images/"+tag+"/json", nil, &img); err != nil {
		return "", fmt.Errorf("the built image %s isn't in the engine xu526 uses: %w", tag, err)
	}
	return img.Id, nil
}

func (d *Docker) dockerHost() string {
	a := d.addr
	switch {
	case a == "":
		return ""
	case strings.Contains(a, "://"):
		return a
	case strings.HasPrefix(a, "/"):
		return "unix://" + a
	}
	return ""
}
