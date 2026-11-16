// Package bundle turns a code base into an OCI image without a Dockerfile.
//
// Each language is a Plugin: Detect → Plan → Build → Layers. Plugins only
// decide what to run and which files go where; the core does caching,
// reproducible tar writing and image output, so every language gets the same
// guarantees.
package bundle

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"xu526/internal/bundle/runner"
)

// Strategy is how a project becomes an image.
type Strategy string

const (
	StaticBinary Strategy = "static-binary" // Go, C: one binary on scratch
	RuntimeDeps  Strategy = "runtime+deps"  // Node, Python: distroless + deps layer + app layer
	StaticAssets Strategy = "static-assets" // HTML/CSS/JS served by a tiny embedded server
)

// Source is the project being built.
type Source struct {
	Dir string // absolute path
	FS  fs.FS
}

// NewSource opens a project directory.
func NewSource(dir string) (*Source, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return nil, &fs.PathError{Op: "open", Path: dir, Err: fs.ErrNotExist}
	}
	return &Source{Dir: abs, FS: os.DirFS(abs)}, nil
}

// Has reports whether a file exists in the project.
func (s *Source) Has(name string) bool {
	_, err := fs.Stat(s.FS, name)
	return err == nil
}

// Detection is a plugin's claim on a project.
type Detection struct {
	Plugin     string
	Confidence int               // higher wins when several plugins match
	Markers    []string          // files that identified the project
	Details    map[string]string // plugin-specific facts (e.g. main package)
}

// Options are the user's build settings.
type Options struct {
	Platform Platform
	Main     string // override: which package/entry to build
	App      string // in a monorepo: the app's folder relative to the workspace root
	Epoch    int64  // SOURCE_DATE_EPOCH for timestamps (default 0)
	// BuildEnv ("NAME=value") is given to every build step, for values a
	// framework bakes in while building (NEXT_PUBLIC_API_URL, VITE_…). It
	// is part of the cache key; it never goes into the image's environment.
	BuildEnv []string
}

// Platform is a target OS/architecture.
type Platform struct{ OS, Arch string }

func (p Platform) String() string { return p.OS + "/" + p.Arch }

// Step is one command a Runner executes.
type Step = runner.Step

// LayerPlan names a layer and the inputs that decide whether it can come
// from the cache. Plans declare these before anything runs, so a fully
// cached build skips the toolchain entirely.
type LayerPlan struct {
	Name   string
	Inputs []string // globs relative to the project ("**" matches any depth)
	Files  []string // or an exact list of project files, when the plugin already knows them
	Salt   string   // anything else that changes the output (toolchain version, flags)
	// Exclude leaves folders (relative, e.g. "apps/other") out of Inputs:
	// in a monorepo, the other apps don't change this one.
	Exclude []string
}

// Workspacer is implemented by plugins that can build one app out of a
// monorepo. WorkspaceRoot reports the workspace root above dir, when dir is
// one of its packages.
type Workspacer interface {
	WorkspaceRoot(dir string) (root string, ok bool)
}

// Plan is what a plugin will do. It is printed before anything runs.
type Plan struct {
	Plugin     string
	Strategy   Strategy
	Base       string // "scratch" or a pinned image reference
	Steps      []Step
	Layers     []LayerPlan
	Entrypoint []string
	Cmd        []string
	Env        map[string]string
	WorkingDir string
	User       string
	Port       int
	Summary    []string
	Downloads  []Download // files to fetch and verify before Build
}

// BuildEnv is what Build gets to work with.
type BuildEnv struct {
	Source  *Source
	WorkDir string // scratch space for outputs
	Runner  runner.Runner
	Log     io.Writer
	Options Options
	// Cached names the layers that came from the cache, so a plugin can
	// skip the steps that only feed those layers.
	Cached map[string]bool
	// Downloads maps each planned Download's name to a verified local file.
	Downloads map[string]string
}

// Output is what Build produced (paths in WorkDir).
type Output struct {
	Files map[string]string
}

// Entry is one file in a layer.
type Entry struct {
	Src  string      // host path ("" with Data for generated content)
	Data []byte      // inline content
	Dst  string      // absolute path in the image
	Mode fs.FileMode // permission bits
	Link string      // make a symlink to this target instead of a file
}

// Layer is the content of one image layer.
type Layer struct {
	Name  string
	Files []Entry
}

// Plugin turns one kind of project into image layers.
type Plugin interface {
	Name() string
	// Version changes whenever the plugin's output for the same inputs changes,
	// which invalidates cached layers.
	Version() string
	Detect(src *Source) (*Detection, error)
	Plan(src *Source, d *Detection, opt Options) (*Plan, error)
	Build(ctx context.Context, env *BuildEnv, p *Plan) (*Output, error)
	Layers(p *Plan, out *Output) ([]Layer, error)
}
