package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
)

// fakeEngine serves just enough of the Docker API to start a container.
type fakeEngine struct {
	info    map[string]string
	version string // /version body
	mu      sync.Mutex
	create  map[string]any
	stopped bool // the container exits at once
	late    int  // inspections before the published port shows
	inspect int
}

func (f *fakeEngine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/info":
		json.NewEncoder(w).Encode(f.info)
	case p == "/version" && f.version != "":
		io.WriteString(w, f.version)
	case strings.HasPrefix(p, "/containers/create"):
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		json.Unmarshal(b, &f.create)
		f.mu.Unlock()
		io.WriteString(w, `{"Id":"0123456789abcdef"}`)
	case strings.HasSuffix(p, "/json") && strings.HasPrefix(p, "/containers/"):
		f.mu.Lock()
		published := f.create["HostConfig"].(map[string]any)["PortBindings"] != nil
		f.inspect++
		late, stopped := f.inspect <= f.late, f.stopped
		f.mu.Unlock()
		ports := map[string]any{}
		if stopped {
			json.NewEncoder(w).Encode(map[string]any{"State": map[string]any{"Running": false, "ExitCode": 1}})
			return
		}
		if published && !late {
			ports["5432/tcp"] = []map[string]string{{"HostIp": "127.0.0.1", "HostPort": "49153"}}
		}
		json.NewEncoder(w).Encode(map[string]any{"State": map[string]any{"Running": true}, "NetworkSettings": map[string]any{
			"Ports": ports, "Networks": map[string]any{"xu-db": map[string]string{"IPAddress": "172.18.0.5"}}}})
	case strings.HasSuffix(p, "/logs"):
		msg := "FATAL: password authentication is not configured\n"
		w.Write(append([]byte{2, 0, 0, 0, 0, 0, 0, byte(len(msg))}, msg...))
	case strings.HasSuffix(p, "/wait"):
		<-r.Context().Done()
	default:
		io.WriteString(w, `{}`)
	}
}

func startEngine(t *testing.T, info map[string]string) (*fakeEngine, string) {
	sock := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip("no unix sockets:", err)
	}
	f := &fakeEngine{info: info}
	srv := &http.Server{Handler: f}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return f, sock
}

func TestInternalServiceAddress(t *testing.T) {
	spec := Spec{App: "db", Image: "postgres", ImageID: "sha256:abc", Port: 5432, Internal: true, Network: "xu-db", Stack: "s"}
	for _, c := range []struct {
		name, os, addr string
	}{
		{"native Linux engine: container IP", "Ubuntu 24.04 LTS", "172.18.0.5:5432"},
		{"Docker Desktop: loopback port", "Docker Desktop", "127.0.0.1:49153"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, sock := startEngine(t, map[string]string{"OperatingSystem": c.os, "Architecture": "aarch64"})
			d, err := NewDocker(context.Background(), sock)
			if err != nil {
				t.Fatal(err)
			}
			if d.Arch() != "arm64" {
				t.Errorf("arch %q", d.Arch())
			}
			inst, err := d.Start(context.Background(), spec)
			if err != nil {
				t.Fatal(err)
			}
			if inst.Addr != c.addr {
				t.Errorf("addr %q, want %q", inst.Addr, c.addr)
			}
		})
	}
}

func TestEngineInVM(t *testing.T) {
	for _, c := range []struct {
		goos, os, name, kernel string
		want                   bool
	}{
		{"linux", "Ubuntu 22.04.4 LTS", "srv", "6.8.0-45-generic", false},
		{"linux", "Docker Desktop", "docker-desktop", "6.10.14-linuxkit", true},
		{"linux", "Ubuntu", "colima", "6.8.0", true},
		{"darwin", "", "", "", true},
		{"darwin", "OrbStack", "orbstack", "6.11.0-orbstack", true},
	} {
		if got := engineInVM(c.goos, c.os, c.name, c.kernel); got != c.want {
			t.Errorf("%+v: got %v", c, got)
		}
	}
}

func TestDockerSocket(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///custom/docker.sock")
	if got := DockerSocket(); got != "/custom/docker.sock" {
		t.Errorf("DOCKER_HOST: %s", got)
	}
	t.Setenv("DOCKER_HOST", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_RUNTIME_DIR", "")
	want := filepath.Join(home, ".docker", "run", "docker.sock")
	os.MkdirAll(filepath.Dir(want), 0o755)
	ln, err := net.Listen("unix", want)
	if err != nil {
		t.Skip(err)
	}
	defer ln.Close()
	got := DockerSocket()
	if _, err := os.Stat("/var/run/docker.sock"); err == nil {
		want = "/var/run/docker.sock" // a real engine on this machine comes first
	}
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestEngineAddresses(t *testing.T) {
	if got := PipePath("npipe:////./pipe/docker_engine"); got != `\\.\pipe\docker_engine` {
		t.Errorf("PipePath: %s", got)
	}
	t.Setenv("DOCKER_HOST", "npipe:////./pipe/dockerDesktopLinuxEngine")
	if got := DockerSocket(); got != "npipe:////./pipe/dockerDesktopLinuxEngine" {
		t.Errorf("DOCKER_HOST npipe: %s", got)
	}
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:2375")
	if got := DockerSocket(); got != "tcp://127.0.0.1:2375" {
		t.Errorf("DOCKER_HOST tcp: %s", got)
	}
	// tcp:// and unix:// both reach an engine.
	_, sock := startEngine(t, map[string]string{"OperatingSystem": "Ubuntu", "Architecture": "x86_64"})
	if _, err := NewDocker(context.Background(), "unix://"+sock); err != nil {
		t.Fatalf("unix://: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	srv := &http.Server{Handler: &fakeEngine{info: map[string]string{"OperatingSystem": "Docker Desktop"}}}
	go srv.Serve(ln)
	defer srv.Close()
	d, err := NewDocker(context.Background(), "tcp://"+ln.Addr().String())
	if err != nil || !d.InVM() {
		t.Fatalf("tcp://: %v", err)
	}
	if _, err := dialEngine(context.Background(), "npipe:////./pipe/x"); goruntime.GOOS != "windows" && err == nil {
		t.Fatal("named pipes off Windows should fail clearly")
	}
}

// On Docker Desktop: a container that exits at once reports why; a port
// reported late is waited for; a service only on internal networks gets no
// published port (Docker can't publish one there) and no address.
func TestStartOnVMEngine(t *testing.T) {
	spec := Spec{App: "db", Image: "postgres", ImageID: "sha256:abc", Port: 5432, Internal: true, Network: "xu-db", Stack: "s"}
	desktop := map[string]string{"OperatingSystem": "Docker Desktop"}

	f, sock := startEngine(t, desktop)
	f.stopped = true
	d, _ := NewDocker(context.Background(), sock)
	if _, err := d.Start(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "stopped right after starting (exit code 1)") ||
		!strings.Contains(err.Error(), "password authentication is not configured") {
		t.Errorf("stopped: %v", err)
	}

	f, sock = startEngine(t, desktop)
	f.late = 3
	d, _ = NewDocker(context.Background(), sock)
	if inst, err := d.Start(context.Background(), spec); err != nil || inst.Addr != "127.0.0.1:49153" {
		t.Errorf("late port: %v %+v", err, inst)
	}

	f, sock = startEngine(t, desktop)
	d, _ = NewDocker(context.Background(), sock)
	internal := spec
	internal.NetworkInternal = true
	inst, err := d.Start(context.Background(), internal)
	if err != nil || inst.Addr != "" || f.create["HostConfig"].(map[string]any)["PortBindings"] != nil {
		t.Errorf("internal only: %v %+v %v", err, inst, f.create["HostConfig"])
	}
}
