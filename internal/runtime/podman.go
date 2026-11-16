package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	goruntime "runtime"
	"strings"
	"time"
)

// PodmanEngine makes Podman ready and returns its Docker-compatible API
// address. On Windows and macOS, Podman runs containers in a VM ("machine"),
// which is started if it's stopped; on Linux, Podman's API socket must be
// enabled (systemctl --user enable --now podman.socket, or podman.socket as
// root). podman is the program's path or name.
func PodmanEngine(ctx context.Context, podman string, logf func(string, ...any)) (string, error) {
	return podmanEngine(ctx, podman, goruntime.GOOS, logf)
}

func podmanEngine(ctx context.Context, podman, goos string, logf func(string, ...any)) (string, error) {
	run := func(timeout time.Duration, args ...string) ([]byte, error) {
		c, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd := exec.CommandContext(c, podman, args...)
		hideWindow(cmd)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = err.Error()
			}
			return out, fmt.Errorf("podman %s: %s", strings.Join(args, " "), msg)
		}
		return out, nil
	}
	if goos == "linux" {
		out, err := run(30*time.Second, "info", "--format", "{{.Host.RemoteSocket.Path}}")
		if err != nil {
			return "", err
		}
		p := strings.TrimPrefix(strings.TrimSpace(string(out)), "unix://")
		if p == "" {
			return "", errors.New("podman has no API socket; run: systemctl --user enable --now podman.socket")
		}
		return "unix://" + p, nil
	}
	type machine struct {
		Name           string
		State          string
		ConnectionInfo struct {
			PodmanSocket *struct{ Path string }
			PodmanPipe   *struct{ Path string }
		}
	}
	inspect := func() (*machine, error) {
		out, err := run(30*time.Second, "machine", "inspect")
		if err != nil {
			return nil, fmt.Errorf("%w (create the machine once with: podman machine init)", err)
		}
		var ms []machine
		if err := json.Unmarshal(out, &ms); err != nil || len(ms) == 0 {
			return nil, errors.New("podman has no machine; create it once with: podman machine init")
		}
		return &ms[0], nil
	}
	m, err := inspect()
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(m.State, "running") {
		logf("starting the Podman machine %s ...", m.Name)
		if _, err := run(5*time.Minute, "machine", "start", m.Name); err != nil && !strings.Contains(err.Error(), "already running") {
			return "", err
		}
		if m, err = inspect(); err != nil {
			return "", err
		}
	}
	switch {
	case goos == "windows" && m.ConnectionInfo.PodmanPipe != nil && m.ConnectionInfo.PodmanPipe.Path != "":
		return m.ConnectionInfo.PodmanPipe.Path, nil // \\.\pipe\podman-machine-default
	case m.ConnectionInfo.PodmanSocket != nil && m.ConnectionInfo.PodmanSocket.Path != "":
		return "unix://" + m.ConnectionInfo.PodmanSocket.Path, nil
	}
	return "", errors.New("podman didn't say where its API is (podman machine inspect has no socket or pipe)")
}
