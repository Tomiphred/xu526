package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ErrNoCompose means docker compose isn't installed: services are then
// built one by one.
var ErrNoCompose = errors.New("docker compose isn't installed")

// FindCompose returns the command for docker compose: "docker compose"
// (the plugin), "docker-compose", or "podman compose".
func FindCompose(ctx context.Context, preferPodman bool) ([]string, error) {
	cands := [][]string{{"docker", "compose"}, {"docker-compose"}, {"podman", "compose"}}
	if preferPodman {
		cands = [][]string{{"podman", "compose"}, {"docker-compose"}, {"docker", "compose"}}
	}
	for _, c := range cands {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := exec.CommandContext(cctx, c[0], append(c[1:], "version")...).Run()
		cancel()
		if err == nil {
			return c, nil
		}
	}
	return nil, ErrNoCompose
}

// ComposeEngine is the command that inspects and saves what a compose
// command built: docker, or podman for podman compose.
func ComposeEngine(compose []string) string {
	if compose[0] == "podman" {
		return "podman"
	}
	return "docker"
}

// ComposeBuild runs `compose build` for a generated compose file, with
// projectDir as the project folder (where relative paths start).
func ComposeBuild(ctx context.Context, compose, env []string, file []byte, projectDir, project string, services []string, noCache bool, logw io.Writer) error {
	tmp, err := os.MkdirTemp("", "xu-compose-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	path := filepath.Join(tmp, "compose.build.yml")
	if err := os.WriteFile(path, file, 0o600); err != nil {
		return err
	}
	args := append(append([]string{}, compose[1:]...), "-f", path, "--project-directory", projectDir, "-p", project, "build")
	if noCache {
		args = append(args, "--no-cache")
	}
	args = append(args, services...)
	fmt.Fprintf(logw, "$ %s %s\n", compose[0], strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, compose[0], args...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = projectDir, logw, logw
	cmd.Env = append(os.Environ(), env...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s build failed: %w", strings.Join(compose, " "), err)
	}
	return nil
}

// ComposeBuild builds on the engine xu526 runs containers on, and returns
// each image's ID.
func (d *Docker) ComposeBuild(ctx context.Context, file []byte, projectDir, project string, refs map[string]string, logw io.Writer) (map[string]string, error) {
	compose, err := FindCompose(ctx, d.podman)
	if err != nil {
		return nil, err
	}
	var env []string
	if compose[0] != "podman" && !d.podman {
		if h := d.dockerHost(); h != "" {
			env = append(env, "DOCKER_HOST="+h) // the same engine the daemon runs containers on
		}
	}
	services := make([]string, 0, len(refs))
	for s := range refs {
		services = append(services, s)
	}
	sort.Strings(services)
	if err := ComposeBuild(ctx, compose, env, file, projectDir, project, services, false, logw); err != nil {
		return nil, err
	}
	ids := map[string]string{}
	for svc, ref := range refs {
		var img struct{ Id string }
		if err := d.do(ctx, "GET", "/images/"+ref+"/json", nil, &img); err != nil {
			return nil, fmt.Errorf("the image built for %s (%s) isn't in the engine xu526 uses: %w", svc, ref, err)
		}
		ids[svc] = img.Id
	}
	return ids, nil
}
