//go:build !windows

package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakePodman answers like podman 5: the machine is stopped until started.
func fakePodman(t *testing.T) (string, string) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	os.WriteFile(state, []byte("stopped"), 0o644)
	script := `#!/bin/sh
case "$1 $2" in
  "machine inspect") printf '[{"Name":"podman-machine-default","State":"%s","ConnectionInfo":{"PodmanSocket":{"Path":"/tmp/podman.sock"},"PodmanPipe":{"Path":"\\\\\\\\.\\\\pipe\\\\podman-machine-default"}}}]' "$(cat ` + state + `)" ;;
  "machine start") echo running > ` + state + `; echo "Machine started" ;;
  "info --format") echo /run/user/1000/podman/podman.sock ;;
  *) echo "unknown $*" >&2; exit 1 ;;
esac
`
	p := filepath.Join(dir, "podman")
	os.WriteFile(p, []byte(script), 0o755)
	return p, state
}

func TestPodmanEngine(t *testing.T) {
	podman, state := fakePodman(t)
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, f) }
	addr, err := podmanEngine(context.Background(), podman, "windows", logf)
	if err != nil || addr != `\\.\pipe\podman-machine-default` {
		t.Fatalf("windows: %q %v", addr, err)
	}
	if b, _ := os.ReadFile(state); !strings.Contains(string(b), "running") || len(logs) != 1 {
		t.Fatalf("the stopped machine wasn't started (%s, %v)", b, logs)
	}
	if addr, err = podmanEngine(context.Background(), podman, "darwin", logf); err != nil || addr != "unix:///tmp/podman.sock" {
		t.Fatalf("darwin: %q %v", addr, err)
	}
	if addr, err = podmanEngine(context.Background(), podman, "linux", logf); err != nil || addr != "unix:///run/user/1000/podman/podman.sock" {
		t.Fatalf("linux: %q %v", addr, err)
	}
	if _, err = podmanEngine(context.Background(), filepath.Join(t.TempDir(), "missing"), "windows", logf); err == nil {
		t.Fatal("missing podman: no error")
	}
}

func TestPodmanContainerSettings(t *testing.T) {
	f, sock := startEngine(t, map[string]string{"OperatingSystem": "fedora", "Architecture": "x86_64"})
	f.version = `{"Components":[{"Name":"Podman Engine"}]}`
	d, err := NewDocker(context.Background(), sock)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Podman() || !d.InVM() {
		t.Fatalf("podman %v, in VM %v", d.Podman(), d.InVM())
	}
	if _, err := d.Start(context.Background(), Spec{App: "a", Image: "x", ImageID: "sha256:abc", Port: 5432}); err != nil {
		t.Fatal(err)
	}
	hc := f.create["HostConfig"].(map[string]any)
	if _, ok := hc["ExtraHosts"]; ok {
		t.Error("Podman maps host.docker.internal itself; host-gateway must not be sent")
	}
	if lc := hc["LogConfig"].(map[string]any); lc["Type"] != "k8s-file" {
		t.Errorf("log config %v", lc)
	}
}
