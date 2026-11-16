package bundle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"xu526/internal/bundle/cache"
	"xu526/internal/bundle/oci"
	"xu526/internal/bundle/registry"
)

// Pinned holds base image digests fixed in code, by reference and
// platform. When a base is listed here it wins over xu.lock. Entries are
// filled with `xu build --update-bases` on a machine that can reach the
// registries, then copied here by a maintainer.
var Pinned = map[string]map[string]string{}

// LockFile is the per-project file that records which digest each base
// tag resolved to, so later builds use exactly the same base.
const LockFile = "xu.lock"

type lockData struct {
	Bases     map[string]map[string]string `json:"bases"`
	Downloads map[string]string            `json:"downloads,omitempty"` // URL → sha256
}

func readLock(dir string) (*lockData, error) {
	l := &lockData{Bases: map[string]map[string]string{}}
	b, err := os.ReadFile(filepath.Join(dir, LockFile))
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, l); err != nil {
		return nil, fmt.Errorf("%s: %w", LockFile, err)
	}
	if l.Bases == nil {
		l.Bases = map[string]map[string]string{}
	}
	return l, nil
}

func (l *lockData) write(dir string) error {
	b, _ := json.MarshalIndent(l, "", "  ")
	return os.WriteFile(filepath.Join(dir, LockFile), append(b, '\n'), 0o644)
}

// baseImage is a pulled base: its layers and the config to build on.
type baseImage struct {
	Ref     string // tag as planned
	Digest  string // platform manifest digest
	Layers  []*oci.Layer
	History []json.RawMessage
	Config  struct {
		Env        []string          `json:"Env"`
		Entrypoint []string          `json:"Entrypoint"`
		Cmd        []string          `json:"Cmd"`
		WorkingDir string            `json:"WorkingDir"`
		User       string            `json:"User"`
		Labels     map[string]string `json:"Labels"`
	}
}

// pullBase resolves the plan's base to a digest (code pin, then xu.lock,
// then the registry) and loads its layers, from the cache when possible.
func pullBase(ctx context.Context, req *Request, src *Source, base string, log io.Writer) (*baseImage, error) {
	plat := req.Options.Platform.String()
	ref, err := registry.ParseRef(base)
	if err != nil {
		return nil, err
	}
	client := req.Registry
	if client == nil {
		client = registry.New()
	}
	lock, err := readLock(src.Dir)
	if err != nil {
		return nil, err
	}
	digest, from := Pinned[base][plat], "pinned in xu"
	if digest == "" && !req.UpdateBases {
		digest, from = lock.Bases[base][plat], LockFile
	}
	if digest == "" {
		fmt.Fprintf(log, "resolving %s for %s ...\n", base, plat)
		rr := ref
		if m := mirrorFor(ref.Registry, req.Mirrors); m != "" {
			rr.Registry = m
		}
		_, _, d, err := client.Resolve(ctx, rr, req.Options.Platform.OS, req.Options.Platform.Arch)
		if err != nil {
			return nil, fmt.Errorf("base image: %w", err)
		}
		digest, from = d, "newly resolved, saved to "+LockFile
		if lock.Bases[base] == nil {
			lock.Bases[base] = map[string]string{}
		}
		lock.Bases[base][plat] = d
		if err := lock.write(src.Dir); err != nil {
			return nil, err
		}
	}
	fmt.Fprintf(log, "base %s@%s (%s)\n", base, short(digest), from)

	pinned := ref
	pinned.Digest = digest
	mirror := mirrorFor(ref.Registry, req.Mirrors)
	if mirror != "" {
		pinned.Registry = mirror
	}
	announced := false
	fetch := func(d string, manifest bool) ([]byte, error) {
		if req.Cache != nil {
			if b, ok := req.Cache.GetBlob(d); ok {
				return b, nil
			}
		}
		if mirror != "" && !announced {
			fmt.Fprintf(log, "  pulling through mirror %s\n", mirror)
			announced = true
		}
		var b []byte
		var err error
		if manifest {
			b, _, _, err = client.Manifest(ctx, pinned)
		} else {
			b, err = client.Blob(ctx, pinned, d)
		}
		if err != nil {
			return nil, fmt.Errorf("base image: %w", err)
		}
		if req.Cache != nil {
			if err := req.Cache.PutBlob(d, b); err != nil {
				return nil, err
			}
		}
		return b, nil
	}
	mb, err := fetch(digest, true)
	if err != nil {
		return nil, err
	}
	var m registry.ImageManifest
	if err := json.Unmarshal(mb, &m); err != nil || m.Config.Digest == "" {
		return nil, fmt.Errorf("base image %s: %s is not a single-platform manifest", base, short(digest))
	}
	cb, err := fetch(m.Config.Digest, false)
	if err != nil {
		return nil, err
	}
	var cfg struct {
		OS, Architecture string
		Config           json.RawMessage   `json:"config"`
		History          []json.RawMessage `json:"history"`
		RootFS           struct {
			DiffIDs []string `json:"diff_ids"`
		} `json:"rootfs"`
	}
	if err := json.Unmarshal(cb, &cfg); err != nil {
		return nil, fmt.Errorf("base image config: %w", err)
	}
	if cfg.Architecture != "" && cfg.Architecture != req.Options.Platform.Arch {
		return nil, fmt.Errorf("base %s@%s is %s, not %s (delete its entry in %s or run --update-bases)", base, short(digest), cfg.Architecture, req.Options.Platform.Arch, LockFile)
	}
	if len(cfg.RootFS.DiffIDs) != len(m.Layers) {
		return nil, fmt.Errorf("base image %s: config lists %d layers, manifest %d", base, len(cfg.RootFS.DiffIDs), len(m.Layers))
	}
	b := &baseImage{Ref: base, Digest: digest, History: cfg.History}
	if len(cfg.Config) > 0 {
		json.Unmarshal(cfg.Config, &b.Config)
	}
	var missing int64
	for _, l := range m.Layers {
		if req.Cache == nil || !hasBlob(req.Cache, l.Digest) {
			missing += l.Size
		}
	}
	if missing > 0 {
		fmt.Fprintf(log, "pulling %d base layers (%s) ...\n", len(m.Layers), humanSize(missing))
	}
	for i, l := range m.Layers {
		data, err := fetch(l.Digest, false)
		if err != nil {
			return nil, err
		}
		b.Layers = append(b.Layers, &oci.Layer{Blob: data, Digest: l.Digest, DiffID: cfg.RootFS.DiffIDs[i], Size: int64(len(data)), MediaType: l.MediaType})
	}
	return b, nil
}

func hasBlob(s *cache.Store, d string) bool {
	_, err := os.Stat(filepath.Join(s.Dir, "blobs", "sha256", d[len("sha256:"):]))
	return err == nil
}

func short(d string) string {
	if len(d) > 19 {
		return d[:19]
	}
	return d
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// Push uploads a built image (base layers included) to ref.
func Push(ctx context.Context, client *registry.Client, im *oci.Image, ref string, log io.Writer) (string, error) {
	r, err := registry.ParseRef(ref)
	if err != nil {
		return "", err
	}
	if r.Digest != "" {
		return "", fmt.Errorf("push to a tag, not a digest")
	}
	if client == nil {
		client = registry.New()
	}
	blobs := []registry.Blob{{Digest: im.ConfigDigest(), Data: im.ConfigJSON()}}
	for _, l := range im.Layers {
		blobs = append(blobs, registry.Blob{Digest: l.Digest, Data: l.Blob})
	}
	fmt.Fprintf(log, "\npushing %s\n", r)
	return client.PushImage(ctx, r, blobs, oci.MediaManifest, im.Manifest(), func(d string, n int, existed bool) {
		state := "uploaded"
		if existed {
			state = "already there"
		}
		fmt.Fprintf(log, "  %s  %8s  %s\n", short(d), humanSize(int64(n)), state)
	})
}

// mirrorFor returns the mirror host for a registry ("" for none). Keys may
// use the Docker Hub alias docker.io.
func mirrorFor(host string, mirrors map[string]string) string {
	if m := mirrors[host]; m != "" {
		return m
	}
	if host == "registry-1.docker.io" {
		return mirrors["docker.io"]
	}
	return ""
}

// ParseMirrors reads "gcr.io=localhost:5000,docker.io=mirror.corp:443".
func ParseMirrors(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		from, to, ok := strings.Cut(part, "=")
		if !ok || from == "" || to == "" {
			return nil, fmt.Errorf("mirror %q should look like gcr.io=mirror.example.com", part)
		}
		out[from] = to
	}
	return out, nil
}
