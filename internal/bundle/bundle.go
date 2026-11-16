package bundle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"xu526/internal/bundle/cache"
	"xu526/internal/bundle/oci"
	"xu526/internal/bundle/registry"
	"xu526/internal/bundle/runner"
)

// formatVersion is part of every cache key; bump it when the core changes
// how layers are written.
const formatVersion = "xu-build/1"

// Request is one `xu build`.
type Request struct {
	Dir     string
	Tag     string // e.g. "go-hello:latest"
	Output  string // where to write the image tarball ("" = don't write)
	Lang    string // force a plugin by name
	Options Options
	NoCache bool
	DryRun  bool
	Plugins []Plugin
	Cache   *cache.Store
	Runner  runner.Runner
	Log     io.Writer
	// Registry pulls base images; nil means registry.New().
	Registry *registry.Client
	// UpdateBases re-resolves base tags instead of using xu.lock.
	UpdateBases bool
	// Mirrors sends base image pulls for a registry host elsewhere, and
	// downloads under a URL prefix to another URL.
	Mirrors map[string]string
	// HTTP downloads plugin files; nil means a default client.
	HTTP *http.Client
}

// LayerResult reports how a layer was produced.
type LayerResult struct {
	Name   string
	Digest string
	Size   int64
	Cached bool
}

// Result is what a build produced.
type Result struct {
	Detection *Detection
	Plan      *Plan
	Image     *oci.Image
	Layers    []LayerResult
	Ran       bool   // whether the toolchain ran (false = everything came from cache)
	Base      string // base reference with digest, "" for scratch
	Duration  time.Duration
}

// Detect asks every plugin and returns the most confident match.
func Detect(src *Source, plugins []Plugin, force string) (Plugin, *Detection, error) {
	type match struct {
		p Plugin
		d *Detection
	}
	var ms []match
	for _, p := range plugins {
		if force != "" && p.Name() != force {
			continue
		}
		d, err := p.Detect(src)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", p.Name(), err)
		}
		if d != nil {
			d.Plugin = p.Name()
			ms = append(ms, match{p, d})
		}
	}
	if len(ms) == 0 {
		if force != "" {
			return nil, nil, fmt.Errorf("this doesn't look like a %s project", force)
		}
		names := make([]string, len(plugins))
		for i, p := range plugins {
			names[i] = p.Name()
		}
		msg := fmt.Sprintf("couldn't tell what kind of project %s is (supported: %s)", src.Dir, strings.Join(names, ", "))
		if src.Has("Dockerfile") {
			msg += "; it has a Dockerfile, so build it with `docker build` instead"
		}
		return nil, nil, errors.New(msg)
	}
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].d.Confidence > ms[j].d.Confidence })
	if len(ms) > 1 && ms[0].d.Confidence == ms[1].d.Confidence {
		return nil, nil, fmt.Errorf("this looks like both a %s and a %s project; choose with --lang", ms[0].p.Name(), ms[1].p.Name())
	}
	return ms[0].p, ms[0].d, nil
}

// Build runs the whole pipeline: detect, plan, build, layers, image.
func Build(ctx context.Context, req Request) (*Result, error) {
	start := time.Now()
	log := req.Log
	if log == nil {
		log = io.Discard
	}
	if req.Options.Platform.OS == "" {
		req.Options.Platform = Platform{OS: "linux", Arch: "amd64"}
	}
	if err := findWorkspace(&req, log); err != nil {
		return nil, err
	}
	src, err := NewSource(req.Dir)
	if err != nil {
		return nil, err
	}
	pl, det, err := Detect(src, req.Plugins, req.Lang)
	if err != nil {
		return nil, err
	}
	plan, err := pl.Plan(src, det, req.Options)
	if err != nil {
		return nil, err
	}
	plan.Plugin = pl.Name()
	res := &Result{Detection: det, Plan: plan}
	printPlan(log, src, det, plan, req.Options)
	if req.DryRun {
		return res, nil
	}
	var base *baseImage
	if plan.Base != "scratch" && plan.Base != "" {
		fmt.Fprintln(log)
		if base, err = pullBase(ctx, &req, src, plan.Base, log); err != nil {
			return nil, err
		}
		res.Base = plan.Base + "@" + base.Digest
	}

	// Cache keys are known before anything runs. The output tarball is never
	// an input, even when it is written inside the project.
	exclude := []string{LockFile}
	if req.Output != "" {
		if out, err := filepath.Abs(req.Output); err == nil {
			if rel, err := filepath.Rel(src.Dir, out); err == nil && !strings.HasPrefix(rel, "..") {
				exclude = append(exclude, filepath.ToSlash(rel), filepath.ToSlash(rel)+".tmp")
			}
		}
	}
	keys := make([]string, len(plan.Layers))
	cached := make([]*cache.Entry, len(plan.Layers))
	blobs := make([][]byte, len(plan.Layers))
	allCached := true
	for i, lp := range plan.Layers {
		var h string
		if len(lp.Files) > 0 {
			h, err = cache.HashFiles(src.Dir, lp.Files)
		} else {
			ex := exclude
			for _, d := range lp.Exclude {
				ex = append(ex, strings.TrimSuffix(d, "/")+"/") // a trailing slash excludes a folder
			}
			h, _, err = cache.HashInputs(src.Dir, lp.Inputs, ex...)
		}
		if err != nil {
			return nil, err
		}
		parts := []string{formatVersion, pl.Name(), pl.Version(), req.Options.Platform.String(), lp.Name, lp.Salt, h,
			fmt.Sprint(req.Options.Epoch)}
		if len(req.Options.BuildEnv) > 0 { // only when set, so existing cache entries stay valid
			be := append([]string(nil), req.Options.BuildEnv...)
			sort.Strings(be)
			parts = append(parts, "buildenv", strings.Join(be, "\x00"))
		}
		keys[i] = cache.Key(parts...)
		if !req.NoCache && req.Cache != nil {
			if e, b, ok := req.Cache.Get(keys[i]); ok {
				cached[i], blobs[i] = e, b
				continue
			}
		}
		allCached = false
	}

	layers := make([]*oci.Layer, len(plan.Layers))
	if allCached {
		fmt.Fprintln(log, "\nnothing changed: every layer comes from the cache")
	} else {
		work, err := os.MkdirTemp("", "xu-build-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(work)
		fmt.Fprintln(log)
		done := map[string]bool{}
		for i, lp := range plan.Layers {
			if cached[i] != nil {
				done[lp.Name] = true
			}
		}
		files, err := fetchDownloads(ctx, &req, src, plan, work, log)
		if err != nil {
			return nil, err
		}
		rn := req.Runner
		if len(req.Options.BuildEnv) > 0 {
			rn = envRunner{rn, req.Options.BuildEnv}
		}
		out, err := pl.Build(ctx, &BuildEnv{Source: src, WorkDir: work, Runner: rn, Log: log, Options: req.Options, Cached: done, Downloads: files}, plan)
		if err != nil {
			return nil, err
		}
		res.Ran = true
		built, err := pl.Layers(plan, out)
		if err != nil {
			return nil, err
		}
		byName := map[string]Layer{}
		for _, l := range built {
			byName[l.Name] = l
		}
		for i, lp := range plan.Layers {
			if cached[i] != nil {
				continue
			}
			l, ok := byName[lp.Name]
			if !ok {
				return nil, fmt.Errorf("%s plugin did not produce the %q layer it planned", pl.Name(), lp.Name)
			}
			files := make([]oci.File, 0, len(l.Files))
			for _, e := range l.Files {
				if e.Src != "" && cache.OSJunk(filepath.Base(e.Src)) {
					continue // Finder's .DS_Store and ._ files never belong in an image
				}
				files = append(files, oci.File{Src: e.Src, Data: e.Data, Dst: e.Dst, Mode: e.Mode, Link: e.Link})
			}
			ol, err := oci.BuildLayer(files, req.Options.Epoch)
			if err != nil {
				return nil, fmt.Errorf("layer %s: %w", lp.Name, err)
			}
			layers[i] = ol
			if req.Cache != nil {
				if err := req.Cache.Put(keys[i], cache.Entry{Digest: ol.Digest, DiffID: ol.DiffID, Size: ol.Size}, ol.Blob); err != nil {
					return nil, err
				}
			}
		}
	}
	var history []string
	for i, lp := range plan.Layers {
		if cached[i] != nil {
			layers[i] = &oci.Layer{Blob: blobs[i], Digest: cached[i].Digest, DiffID: cached[i].DiffID, Size: cached[i].Size}
		}
		res.Layers = append(res.Layers, LayerResult{Name: lp.Name, Digest: layers[i].Digest, Size: layers[i].Size, Cached: cached[i] != nil})
		history = append(history, lp.Name)
	}

	img := &oci.Image{OS: req.Options.Platform.OS, Arch: req.Options.Platform.Arch, LayerHistory: history, Epoch: req.Options.Epoch}
	cfg := oci.Config{Entrypoint: plan.Entrypoint, Cmd: plan.Cmd, WorkingDir: plan.WorkingDir, User: plan.User, Port: plan.Port,
		Labels: map[string]string{}}
	var baseEnv []string
	if base != nil {
		img.Layers = append(img.Layers, base.Layers...)
		img.BaseLayers, img.BaseHistory = len(base.Layers), base.History
		baseEnv = base.Config.Env
		if len(cfg.Entrypoint) == 0 {
			cfg.Entrypoint = base.Config.Entrypoint
			if len(cfg.Cmd) == 0 {
				cfg.Cmd = base.Config.Cmd
			}
		}
		if cfg.WorkingDir == "" {
			cfg.WorkingDir = base.Config.WorkingDir
		}
		if cfg.User == "" {
			cfg.User = base.Config.User
		}
		for k, v := range base.Config.Labels {
			cfg.Labels[k] = v
		}
		cfg.Labels["org.opencontainers.image.base.name"] = plan.Base
		cfg.Labels["org.opencontainers.image.base.digest"] = base.Digest
	}
	cfg.Labels["dev.xu526.builder"] = pl.Name() + "/" + pl.Version()
	cfg.Env = mergeEnv(baseEnv, plan.Env)
	img.Config = cfg
	img.Layers = append(img.Layers, layers...)
	if err := img.Seal(); err != nil {
		return nil, err
	}
	res.Image = img
	if req.Output != "" {
		if err := writeFile(req.Output, func(w io.Writer) error { return img.WriteTar(w, req.Tag) }); err != nil {
			return nil, err
		}
	}
	res.Duration = time.Since(start)
	return res, nil
}

func writeFile(path string, fn func(io.Writer) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := fn(f); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func printPlan(w io.Writer, src *Source, d *Detection, p *Plan, opt Options) {
	if opt.App != "" {
		fmt.Fprintf(w, "detected %s monorepo at %s; building %s (%s)\n", d.Plugin, src.Dir, opt.App, strings.Join(d.Markers, ", "))
	} else {
		fmt.Fprintf(w, "detected %s project in %s (%s)\n", d.Plugin, src.Dir, strings.Join(d.Markers, ", "))
	}
	fmt.Fprintf(w, "plan: %s on %s for %s\n", p.Strategy, p.Base, opt.Platform)
	for _, s := range p.Summary {
		fmt.Fprintf(w, "  - %s\n", s)
	}
	for i, s := range p.Steps {
		cmd := s.Show
		if cmd == "" {
			cmd = strings.Join(s.Cmd, " ")
		}
		fmt.Fprintf(w, "  step %d (%s): %s\n", i+1, s.Name, cmd)
	}
	var names []string
	for _, l := range p.Layers {
		names = append(names, l.Name)
	}
	fmt.Fprintf(w, "  layers: %s\n", strings.Join(names, ", "))
}

// ParsePlatform reads "linux/amd64" style strings.
func ParsePlatform(s string) (Platform, error) {
	os_, arch, ok := strings.Cut(s, "/")
	if !ok || os_ != "linux" || (arch != "amd64" && arch != "arm64") {
		return Platform{}, errors.New("platform must be linux/amd64 or linux/arm64")
	}
	return Platform{OS: os_, Arch: arch}, nil
}

// mergeEnv layers plan variables over the base image's, keeping the base
// order and adding new keys sorted.
func mergeEnv(base []string, over map[string]string) []string {
	out := make([]string, 0, len(base)+len(over))
	seen := map[string]bool{}
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if v, ok := over[k]; ok {
			kv = k + "=" + v
		}
		seen[k] = true
		out = append(out, kv)
	}
	var extra []string
	for k, v := range over {
		if !seen[k] {
			extra = append(extra, k+"="+v)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}

// findWorkspace turns `xu build apps/web` inside a monorepo into a build of
// apps/web from the workspace root, so its lockfile and local packages are
// used. With Options.App already set, Dir is the root.
func findWorkspace(req *Request, log io.Writer) error {
	if req.Options.App != "" {
		app := filepath.ToSlash(filepath.Clean(req.Options.App))
		if filepath.IsAbs(req.Options.App) || app == "." || strings.HasPrefix(app, "../") || app == ".." {
			return fmt.Errorf("--app must be a folder inside %s, like apps/web", req.Dir)
		}
		if fi, err := os.Stat(filepath.Join(req.Dir, filepath.FromSlash(app))); err != nil || !fi.IsDir() {
			return fmt.Errorf("--app %s: no such folder in %s", app, req.Dir)
		}
		req.Options.App = app
		return nil
	}
	if req.Lang != "" {
		return nil // an explicit plugin choice builds the folder as it is
	}
	for _, pl := range req.Plugins {
		w, ok := pl.(Workspacer)
		if !ok {
			continue
		}
		if root, ok := w.WorkspaceRoot(req.Dir); ok {
			rel, err := filepath.Rel(root, req.Dir)
			if err != nil {
				return err
			}
			req.Options.App = filepath.ToSlash(rel)
			req.Dir = root
			return nil
		}
	}
	return nil
}

// envRunner adds the build environment to every step.
type envRunner struct {
	runner.Runner
	env []string
}

func (r envRunner) Run(ctx context.Context, s runner.Step, log io.Writer) error {
	s.Env = append(append([]string(nil), r.env...), s.Env...) // a step's own values win
	return r.Runner.Run(ctx, s, log)
}
