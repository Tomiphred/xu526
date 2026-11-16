package platform

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"

	"xu526/internal/bundle"
	"xu526/internal/bundle/cache"
	"xu526/internal/bundle/plugins"
	"xu526/internal/bundle/registry"
	"xu526/internal/bundle/runner"
	"xu526/internal/runtime"
)

// hasDockerfile reports whether a build context has the Dockerfile a source names.
func hasDockerfile(ctxDir, dockerfile string) bool {
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	p, err := safeJoin(ctxDir, dockerfile)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

var errHostBuildsOff = errors.New("this folder has no Dockerfile. xu526 can build it with `xu build` " +
	"(Go, Node, Python, C and static sites), but that runs the repository's own build steps (npm install, make, …) " +
	"directly on this server, so it's off by default: start the daemon with -host-builds (in /etc/default/xu526) to allow it, or add a Dockerfile")

// viaUser: a repository someone connected with their own GitHub sign-in,
// the way they'd pick a folder.
func (s *Source) viaUser() bool { return s.InstallationID != 0 && s.ConnectedBy != "" }

// hostBuildOK: may this source be built with xu build, which runs its own
// build steps on this computer? Always with -host-builds; otherwise for
// repositories their owner connected from the dashboard (like a folder),
// unless switched off on the Source tab.
func (p *Platform) hostBuildOK(s *Source) bool {
	return p.cfg.HostBuilds || (s != nil && s.viaUser() && !s.NoHostBuild)
}

// hostBuild builds ctxDir with xu build for this server's architecture and
// loads the image into Docker as ref. It returns the image ID.
func (p *Platform) hostBuild(ctx context.Context, d *runtime.Docker, s *Source, ctxDir, ref string, logw io.Writer) (string, error) {
	if !p.hostBuildOK(s) {
		if s != nil && s.viaUser() {
			return "", errors.New("this repository has no Dockerfile, and building without one is turned off for this repository. " +
				"Turn on \"Build without a Dockerfile\" on its Source tab, or add a Dockerfile")
		}
		return "", errHostBuildsOff
	}
	store, err := cache.Open(filepath.Join(p.dataDir, "buildcache"))
	if err != nil {
		return "", err
	}
	mirrors, err := bundle.ParseMirrors(os.Getenv("XU_MIRRORS"))
	if err != nil {
		return "", fmt.Errorf("XU_MIRRORS: %w", err)
	}
	fmt.Fprintln(logw, "no Dockerfile: building with xu build")
	res, err := bundle.Build(ctx, bundle.Request{
		Dir: ctxDir, Tag: ref, Plugins: plugins.All(), Cache: store, Runner: runner.Local{}, Log: logw,
		Registry: registry.New(), Mirrors: mirrors,
		Options: bundle.Options{Platform: bundle.Platform{OS: "linux", Arch: goruntime.GOARCH}},
	})
	if err != nil {
		return "", err
	}
	fmt.Fprintf(logw, "image %s %s\n", ref, res.Image.Digest())
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(res.Image.WriteTar(pw, ref)) }()
	id, err := d.Load(ctx, pr, ref)
	if err != nil {
		return "", err
	}
	if id != res.Image.ConfigDigest() && id != res.Image.Digest() {
		return "", fmt.Errorf("docker loaded %s, not the built image %s", id, res.Image.Digest())
	}
	return id, nil
}
