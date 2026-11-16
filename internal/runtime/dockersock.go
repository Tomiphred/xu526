package runtime

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
)

// DockerSocket finds the Docker Engine socket: DOCKER_HOST (unix://…) if
// set, then the places Docker Desktop, OrbStack, Colima, Rancher Desktop
// and Podman use on macOS and Linux, then /var/run/docker.sock.
func DockerSocket() string {
	h := os.Getenv("DOCKER_HOST")
	if strings.HasPrefix(h, "unix://") {
		return strings.TrimPrefix(h, "unix://")
	}
	if strings.HasPrefix(h, "npipe:") || strings.HasPrefix(h, "tcp://") {
		return h
	}
	if goruntime.GOOS == "windows" {
		// Docker Desktop, or Podman when Docker Desktop isn't installed;
		// otherwise pass -podman (which asks Podman for its pipe) or
		// -docker-sock npipe:////./pipe/podman-machine-default.
		return "npipe:////./pipe/docker_engine"
	}
	for _, c := range dockerSocketCandidates(os.Getenv("HOME"), os.Getenv("XDG_RUNTIME_DIR")) {
		if fi, err := os.Stat(c); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return c
		}
	}
	return "/var/run/docker.sock"
}

func dockerSocketCandidates(home, xdg string) []string {
	c := []string{"/var/run/docker.sock"}
	if home != "" {
		c = append(c,
			filepath.Join(home, ".docker", "run", "docker.sock"),                                             // Docker Desktop (macOS)
			filepath.Join(home, ".orbstack", "run", "docker.sock"),                                           // OrbStack
			filepath.Join(home, ".colima", "default", "docker.sock"),                                         // Colima
			filepath.Join(home, ".colima", "docker.sock"),                                                    // Colima (older)
			filepath.Join(home, ".rd", "docker.sock"),                                                        // Rancher Desktop
			filepath.Join(home, ".docker", "desktop", "docker.sock"),                                         // Docker Desktop (Linux)
			filepath.Join(home, ".local", "share", "containers", "podman", "machine", "podman.sock"),         // Podman machine (macOS)
			filepath.Join(home, ".local", "share", "containers", "podman", "machine", "qemu", "podman.sock"), // Podman machine (macOS, older)
		)
	}
	c = append(c, "/run/podman/podman.sock") // Podman as root (Linux)
	if xdg != "" {
		c = append(c, filepath.Join(xdg, "docker.sock"), filepath.Join(xdg, "podman", "podman.sock"))
	}
	return c
}
