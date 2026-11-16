package platform

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHasDockerfile(t *testing.T) {
	d := t.TempDir()
	if hasDockerfile(d, "") {
		t.Fatal("empty folder has no Dockerfile")
	}
	os.WriteFile(filepath.Join(d, "Dockerfile"), nil, 0o644)
	os.MkdirAll(filepath.Join(d, "ops"), 0o755)
	os.WriteFile(filepath.Join(d, "ops", "web.Dockerfile"), nil, 0o644)
	if !hasDockerfile(d, "") || !hasDockerfile(d, "ops/web.Dockerfile") {
		t.Fatal("hasDockerfile")
	}
	// Paths are clamped to the repository: ../Dockerfile can't reach outside it.
	sub := filepath.Join(d, "ops")
	if hasDockerfile(sub, "../Dockerfile") {
		t.Fatal("../Dockerfile escaped the build folder")
	}
}

func TestHostBuildsOffByDefault(t *testing.T) {
	p := &Platform{}
	if _, err := p.hostBuild(context.Background(), nil, nil, t.TempDir(), "xu/a:1", nil); !errors.Is(err, errHostBuildsOff) {
		t.Fatalf("want errHostBuildsOff, got %v", err)
	}
}

func TestUploadRefs(t *testing.T) {
	p := &Platform{}
	for _, bad := range []string{"nginx:latest", "xu/../x:1", "xu/App:1", "docker.io/xu/a:1", "xu/a", "xu/a:"} {
		if _, err := p.LoadImage(context.Background(), strings.NewReader(""), bad); err == nil || !strings.Contains(err.Error(), "xu/<name>:<tag>") {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
	if _, err := p.LoadImage(context.Background(), strings.NewReader(""), "xu/web-1:0836fe631e37"); err == nil || !strings.Contains(err.Error(), "needs Docker") {
		t.Errorf("valid ref should get as far as Docker: %v", err)
	}
}

// Repositories someone connected with their own GitHub sign-in build like
// a folder unless switched off; others need -host-builds.
func TestHostBuildOKForConnectedRepos(t *testing.T) {
	p := &Platform{}
	mine := &Source{InstallationID: 7, ConnectedBy: "u1"}
	if !p.hostBuildOK(mine) {
		t.Fatal("a repository you connected should build without a Dockerfile")
	}
	if p.hostBuildOK(&Source{InstallationID: 7, ConnectedBy: "u1", NoHostBuild: true}) {
		t.Fatal("switched off, yet allowed")
	}
	if p.hostBuildOK(&Source{Repo: "https://github.com/x/y", Token: "t"}) || p.hostBuildOK(nil) {
		t.Fatal("a token or webhook source must need -host-builds")
	}
	p.cfg.HostBuilds = true
	if !p.hostBuildOK(&Source{Repo: "https://github.com/x/y"}) {
		t.Fatal("-host-builds allows everything")
	}
}
