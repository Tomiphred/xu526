// Package runtime starts and stops app instances.
//
// A Runtime is the only part of the platform that knows *how* an instance
// runs. Today there are two: Docker (real containers) and Process (a plain
// local process, handy for development and tests). Later, your own runtime
// (namespaces + cgroups + overlayfs) and a microVM runtime (Firecracker) slot
// in behind the same interface without touching the router or autoscaler.
package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
)

// Spec is everything a runtime needs to start one instance.
type Spec struct {
	App     string
	Image   string            // Docker image, e.g. "nginx:alpine"
	Command []string          // process runtime: binary + args
	Port    int               // port the app listens on inside its sandbox
	Env     map[string]string // extra environment
	Memory  int64             // memory limit in MB (0 = none)
	CPUs    float64           // CPU limit in cores (0 = none)

	Args    []string // override the image's command
	Stack   string   // owning stack, for labels
	Network string   // private network to join ("" = default bridge)
	// NetworkInternal makes Network an internal one (no way out) when it's
	// created; NetworkExternal means it must already exist.
	NetworkInternal, NetworkExternal bool
	Aliases                          []string // names other containers on the network use
	// Extra networks the instance also joins, each with its own names
	// (an environment's shared network, where databases live).
	Extra    []NetworkAttach
	Internal bool // don't publish a port to the host
	Volumes  []Volume
	ImageID  string // pinned image ID (preferred over Image when present)
	Upstream string // External: the address of what's already running
	// Entrypoint, WorkingDir and User override the image's.
	Entrypoint []string
	WorkingDir string
	User       string
	Auth       string // registry credentials for pulling Image
	// Ports are more container ports to publish on loopback, such as an
	// object store's console page or admin API. Instance.Ports has where.
	Ports []int
	// Files are written into the container before it starts (a config
	// file for an image without a shell).
	Files []File
}

// File is a file put into an instance before it starts.
type File struct {
	Path string // absolute path inside the container
	Data []byte
	Mode int64 // 0 = 0644
}

// NetworkAttach is one more network for an instance.
type NetworkAttach struct {
	Name     string
	Aliases  []string
	Internal bool // created as an internal network (no way out)
	External bool // made outside xu526: must exist, never created
}

// Volume is a named volume mounted into the instance.
type Volume struct {
	Name     string
	Path     string
	ReadOnly bool
}

// Instance is a running copy of an app.
type Instance struct {
	ID     string
	Addr   string          // host:port to reach it ("" if it has no port)
	Ports  map[int]string  // Spec.Ports: container port → host:port
	Exited <-chan struct{} // closed when the instance stops for any reason
}

// Runtime runs instances.
type Runtime interface {
	Name() string
	Start(ctx context.Context, s Spec) (*Instance, error)
	Stop(ctx context.Context, inst *Instance) error
	// Cleanup removes anything left behind by a previous run of the daemon.
	Cleanup(ctx context.Context) error
}

// NetworkRemover is implemented by runtimes that create private networks.
type NetworkRemover interface {
	RemoveNetwork(ctx context.Context, name string) error
}

// LogReader is implemented by runtimes that can return an instance's output.
type LogReader interface {
	Logs(ctx context.Context, inst *Instance, tail int) (string, error)
}

func shortID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("find free port: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
