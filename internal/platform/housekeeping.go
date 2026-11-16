package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"xu526/internal/runtime"
)

// ---- images ----

// XuImage is an image built or loaded by xu (named xu/…), with what uses it.
type XuImage struct {
	runtime.ImageSummary
	App      string   `json:"app"`      // from the name xu/<app>:<tag>
	Live     []string `json:"live"`     // apps or stack services running it now
	Releases []string `json:"releases"` // releases that can roll back to it, e.g. "pof v3"
	Status   string   `json:"status"`   // live | rollback | unused
}

type relRef struct {
	key     string // app name, or stack/service
	version int
	ids     []string
	refs    []string
}

// imageRefs collects what every running app and every kept release uses.
func (p *Platform) imageRefs() (live map[string][]string, rels []relRef) {
	live = map[string][]string{}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for name, a := range p.apps {
		a.mu.Lock()
		s := a.spec
		a.mu.Unlock()
		who := name
		if s.Stack != "" {
			who = s.Stack + "/" + firstNonEmpty(s.Service, name)
		}
		for _, k := range []string{s.ImageID, runtime.ShortName(s.Image)} {
			if k != "" {
				live[k] = append(live[k], who)
			}
		}
	}
	for key, list := range p.releases {
		_, target, _ := strings.Cut(key, ":")
		for _, r := range list {
			rr := relRef{key: target, version: r.Version}
			if r.Spec != nil {
				rr.ids = append(rr.ids, r.Spec.ImageID)
				rr.refs = append(rr.refs, runtime.ShortName(r.Spec.Image))
			}
			for _, pin := range r.Pins {
				rr.ids = append(rr.ids, pin.ID)
			}
			rels = append(rels, rr)
		}
	}
	return live, rels
}

// classify fills in who uses each image.
func classify(imgs []runtime.ImageSummary, live map[string][]string, rels []relRef) []XuImage {
	out := make([]XuImage, 0, len(imgs))
	for _, im := range imgs {
		x := XuImage{ImageSummary: im, Live: []string{}, Releases: []string{}}
		keys := append([]string{im.ID}, im.Tags...)
		seen := map[string]bool{}
		for _, k := range keys {
			for _, who := range live[k] {
				if !seen[who] {
					seen[who] = true
					x.Live = append(x.Live, who)
				}
			}
		}
		for _, r := range rels {
			hit := false
			for _, id := range r.ids {
				hit = hit || (id != "" && id == im.ID)
			}
			for _, ref := range r.refs {
				for _, t := range im.Tags {
					hit = hit || (ref != "" && ref == t)
				}
			}
			if hit {
				x.Releases = append(x.Releases, fmt.Sprintf("%s v%d", r.key, r.version))
			}
		}
		for _, t := range im.Tags {
			if rest, ok := strings.CutPrefix(t, "xu/"); ok {
				x.App, _, _ = strings.Cut(rest, ":")
				break
			}
		}
		switch {
		case len(x.Live) > 0:
			x.Status = "live"
		case len(x.Releases) > 0:
			x.Status = "rollback"
		default:
			x.Status = "unused"
		}
		sort.Strings(x.Releases)
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].App != out[j].App {
			return out[i].App < out[j].App
		}
		return out[i].Added.After(out[j].Added)
	})
	return out
}

// planPrune picks images to remove: never one that is running, and never
// one of the newest keep releases of any app or stack (so rollback keeps
// working); everything else built by xu goes.
func planPrune(imgs []XuImage, rels []relRef, keep int) []XuImage {
	protected := map[string]bool{}
	byKey := map[string][]relRef{}
	for _, r := range rels {
		byKey[r.key] = append(byKey[r.key], r)
	}
	for _, list := range byKey {
		sort.Slice(list, func(i, j int) bool { return list[i].version > list[j].version })
		for i, r := range list {
			if i >= keep {
				break
			}
			for _, id := range r.ids {
				protected[id] = true
			}
			for _, ref := range r.refs {
				protected[ref] = true
			}
		}
	}
	var out []XuImage
	for _, im := range imgs {
		if len(im.Live) > 0 || protected[im.ID] {
			continue
		}
		keepIt := false
		for _, t := range im.Tags {
			keepIt = keepIt || protected[t]
		}
		if !keepIt {
			out = append(out, im)
		}
	}
	return out
}

func (p *Platform) docker() (*runtime.Docker, error) {
	d, ok := p.cfg.Docker.(*runtime.Docker)
	if !ok {
		return nil, errors.New("this needs Docker")
	}
	return d, nil
}

// Images lists xu's images (optionally one app's) and what uses them.
func (p *Platform) Images(ctx context.Context, app string) ([]XuImage, error) {
	d, err := p.docker()
	if err != nil {
		return nil, err
	}
	prefix := "xu/*"
	if app != "" {
		prefix = "xu/" + app + ":*"
	}
	imgs, err := d.Images(ctx, prefix)
	if err != nil {
		return nil, err
	}
	live, rels := p.imageRefs()
	return classify(imgs, live, rels), nil
}

func (p *Platform) findImage(ctx context.Context, ref string) (*XuImage, error) {
	all, err := p.Images(ctx, "")
	if err != nil {
		return nil, err
	}
	return matchImage(all, ref)
}

// matchImage finds ref among xu images: an exact name (xu/app:tag), an
// image ID or its prefix (sha256:abc… or abc…, 12+ hex digits), or a bare
// xu/app when that app has exactly one image.
func matchImage(all []XuImage, ref string) (*XuImage, error) {
	ref = runtime.ShortName(strings.TrimSpace(ref))
	hexRef := strings.TrimPrefix(ref, "sha256:")
	isID := len(hexRef) >= 12 && strings.Trim(hexRef, "0123456789abcdef") == ""
	if !isID && !strings.HasPrefix(ref, "xu/") {
		return nil, fmt.Errorf("%s isn't an xu image (their names start with xu/)", ref)
	}
	var hits []int
	for i, im := range all {
		switch {
		case isID && strings.HasPrefix(strings.TrimPrefix(im.ID, "sha256:"), hexRef):
			hits = append(hits, i)
		case !isID:
			for _, t := range im.Tags {
				if t == ref {
					return &all[i], nil
				}
				if !strings.Contains(ref, ":") && strings.HasPrefix(t, ref+":") {
					hits = append(hits, i)
					break
				}
			}
		}
	}
	switch len(hits) {
	case 0:
		return nil, fmt.Errorf("no xu image %s (see xu images)", ref)
	case 1:
		return &all[hits[0]], nil
	}
	return nil, fmt.Errorf("%s matches %d images; give the full name with its tag (see xu images)", ref, len(hits))
}

// RemoveImage deletes one xu image tag. A running image is never removed;
// one a release can roll back to needs force.
func (p *Platform) RemoveImage(ctx context.Context, ref string, force bool) (*XuImage, error) {
	im, err := p.findImage(ctx, ref)
	if err != nil {
		return nil, err
	}
	if len(im.Live) > 0 {
		return nil, fmt.Errorf("%s is running in %s; deploy something else there first", ref, strings.Join(im.Live, ", "))
	}
	if len(im.Releases) > 0 && !force {
		return nil, fmt.Errorf("%s is what %s would roll back to; use --force to remove it anyway", ref, strings.Join(im.Releases, ", "))
	}
	d, _ := p.docker()
	targets := []string{runtime.ShortName(ref)}
	if !strings.HasPrefix(targets[0], "xu/") { // by ID: every xu/ name of it
		targets = xuTags(im.Tags)
	}
	for _, t := range targets {
		if err := d.RemoveImage(ctx, t, false); err != nil {
			return nil, err
		}
	}
	p.event(im.App, "image", "removed image %s", ref)
	return im, nil
}

// xuTags keeps only the xu/ names.
func xuTags(tags []string) []string {
	var out []string
	for _, t := range tags {
		if strings.HasPrefix(t, "xu/") {
			out = append(out, t)
		}
	}
	return out
}

// PruneResult reports a prune.
type PruneResult struct {
	Removed []XuImage `json:"removed"`
	Freed   int64     `json:"freed_bytes"`
	Kept    int       `json:"kept"`
	DryRun  bool      `json:"dry_run"`
	Errors  []string  `json:"errors,omitempty"`
}

// PruneImages removes xu images nothing runs and no recent release needs.
func (p *Platform) PruneImages(ctx context.Context, keep int, dryRun bool) (*PruneResult, error) {
	if keep < 1 {
		return nil, errors.New("keep at least 1 release per app, so rollback still works")
	}
	all, err := p.Images(ctx, "")
	if err != nil {
		return nil, err
	}
	_, rels := p.imageRefs()
	plan := planPrune(all, rels, keep)
	res := &PruneResult{DryRun: dryRun, Kept: len(all) - len(plan), Removed: []XuImage{}}
	d, _ := p.docker()
	for _, im := range plan {
		if !dryRun {
			failed := false
			for _, t := range xuTags(im.Tags) { // other names stay, and keep the image
				if err := d.RemoveImage(ctx, t, false); err != nil {
					res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", t, err))
					failed = true
				}
			}
			if failed {
				continue
			}
		}
		res.Removed = append(res.Removed, im)
		if len(xuTags(im.Tags)) == len(im.Tags) {
			res.Freed += im.Unique()
		}
	}
	if !dryRun && len(res.Removed) > 0 {
		p.event("", "image", "pruned %d images (%s)", len(res.Removed), humanBytes(res.Freed))
	}
	return res, nil
}

// TagImage copies an xu image under another xu/ name.
func (p *Platform) TagImage(ctx context.Context, src, dst string) error {
	im, err := p.findImage(ctx, src)
	if err != nil {
		return err
	}
	dst = runtime.ShortName(dst)
	if !uploadRef.MatchString(dst) {
		return fmt.Errorf("the new name must look like xu/<name>:<tag>")
	}
	repo, tag, _ := strings.Cut(dst, ":")
	d, _ := p.docker()
	return d.TagImage(ctx, im.ID, repo, tag)
}

// PushImage copies an xu image to a registry, using xud's saved logins.
func (p *Platform) PushImage(ctx context.Context, src, dest string, w io.Writer) error {
	im, err := p.findImage(ctx, src)
	if err != nil {
		return err
	}
	repo, tag := dest, "latest"
	if i := strings.LastIndex(dest, ":"); i > strings.LastIndex(dest, "/") {
		repo, tag = dest[:i], dest[i+1:]
	}
	if repo == "" || strings.ContainsAny(repo, " @") {
		return fmt.Errorf("invalid destination %q (e.g. ghcr.io/me/app:1.0)", dest)
	}
	auth, err := p.authFor(ctx, repo)
	if err != nil {
		return err
	}
	d, _ := p.docker()
	fmt.Fprintf(w, "pushing %s to %s:%s\n", src, repo, tag)
	if err := d.Push(ctx, im.ID, repo, tag, auth, w); err != nil {
		return err
	}
	_ = d.RemoveImage(ctx, repo+":"+tag, false) // drop the local copy of the registry name
	p.event(im.App, "image", "pushed %s to %s:%s", src, repo, tag)
	return nil
}

// ---- volumes ----

// XuVolume is a named volume xu526 created, for a stack, database, cache
// or object store.
type XuVolume struct {
	runtime.VolumeInfo
	Stack       string   `json:"stack"`
	UsedBy      []string `json:"used_by"`     // apps that mount it
	Environment string   `json:"environment"` // of those apps
	InUse       bool     `json:"in_use"`      // an app or its stack still exists
	Status      string   `json:"status"`      // in use | orphaned
}

// Volumes lists the volumes stacks created and whether their stack exists.
func (p *Platform) Volumes(ctx context.Context) ([]XuVolume, error) {
	d, err := p.docker()
	if err != nil {
		return nil, err
	}
	vols, err := d.Volumes(ctx, "xu.managed=true")
	if err != nil {
		return nil, err
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := []XuVolume{}
	users := map[string][]string{}
	envOf := map[string]string{}
	for n, a := range p.apps {
		for _, m := range a.spec.Volumes {
			users[m.Volume] = append(users[m.Volume], n)
			envOf[m.Volume] = envOrDefault(a.spec.Environment)
		}
	}
	for _, v := range vols {
		x := XuVolume{VolumeInfo: v, Stack: v.Labels["xu.stack"], UsedBy: users[v.Name], Environment: envOf[v.Name]}
		if x.UsedBy == nil {
			x.UsedBy = []string{}
		}
		sort.Strings(x.UsedBy)
		if r := p.stacks[x.Stack]; x.Environment == "" && r != nil {
			x.Environment = envOrDefault(r.Environment)
		}
		_, x.InUse = p.stacks[x.Stack]
		x.InUse = x.InUse || v.RefCount > 0 || len(x.UsedBy) > 0
		x.Status = map[bool]string{true: "in use", false: "orphaned"}[x.InUse]
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// RemoveVolume deletes a stack volume whose stack is gone. Its data is lost.
func (p *Platform) RemoveVolume(ctx context.Context, name string) error {
	vols, err := p.Volumes(ctx)
	if err != nil {
		return err
	}
	for _, v := range vols {
		if v.Name != name {
			continue
		}
		if v.InUse {
			if len(v.UsedBy) > 0 {
				return fmt.Errorf("%s is used by %s; delete that first", name, strings.Join(v.UsedBy, ", "))
			}
			return fmt.Errorf("%s belongs to stack %s, which still exists; delete the stack first", name, v.Stack)
		}
		d, _ := p.docker()
		if err := d.RemoveVolume(ctx, name); err != nil {
			return err
		}
		p.event(v.Stack, "delete", "removed volume %s", name)
		return nil
	}
	return fmt.Errorf("no xu volume %s (see xu volumes)", name)
}

// ---- disk usage and xud's build cache ----

// DiskUsage summarises what xu keeps on this server.
type DiskUsage struct {
	Images            int   `json:"images"`
	ImagesBytes       int64 `json:"images_bytes"`
	LiveBytes         int64 `json:"live_bytes"`
	RollbackBytes     int64 `json:"rollback_bytes"`
	ReclaimableBytes  int64 `json:"reclaimable_bytes"` // what `xu images prune` (keep 5) would free
	Volumes           int   `json:"volumes"`
	VolumesBytes      int64 `json:"volumes_bytes"`
	OrphanVolumes     int   `json:"orphan_volumes"`
	OrphanVolumeBytes int64 `json:"orphan_volume_bytes"`
	BuildCacheBytes   int64 `json:"build_cache_bytes"` // git checkouts and xu build layers
	LogsBytes         int64 `json:"logs_bytes"`
}

func dirSize(p string) int64 {
	var n int64
	filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// Usage measures images, volumes, build cache and logs.
func (p *Platform) Usage(ctx context.Context) (*DiskUsage, error) {
	u := &DiskUsage{
		BuildCacheBytes: dirSize(filepath.Join(p.dataDir, "builds")) + dirSize(filepath.Join(p.dataDir, "buildcache")),
		LogsBytes:       dirSize(filepath.Join(p.dataDir, "logs")),
	}
	if _, err := p.docker(); err != nil {
		return u, nil
	}
	imgs, err := p.Images(ctx, "")
	if err != nil {
		return nil, err
	}
	_, rels := p.imageRefs()
	for _, im := range imgs {
		u.Images++
		u.ImagesBytes += im.Unique()
		switch im.Status {
		case "live":
			u.LiveBytes += im.Unique()
		case "rollback":
			u.RollbackBytes += im.Unique()
		}
	}
	for _, im := range planPrune(imgs, rels, 5) {
		u.ReclaimableBytes += im.Unique()
	}
	vols, err := p.Volumes(ctx)
	if err == nil {
		for _, v := range vols {
			u.Volumes++
			if v.Size > 0 {
				u.VolumesBytes += v.Size
			}
			if !v.InUse {
				u.OrphanVolumes++
				if v.Size > 0 {
					u.OrphanVolumeBytes += v.Size
				}
			}
		}
	}
	return u, nil
}

// CleanBuildCache removes git checkouts and xu build layers kept between
// git builds. The next build of each source starts from scratch.
func (p *Platform) CleanBuildCache() (int64, error) {
	p.bmu.Lock()
	defer p.bmu.Unlock()
	for _, q := range p.builds {
		if q.running {
			return 0, errors.New("a build is running; try again when it finishes")
		}
	}
	freed := dirSize(filepath.Join(p.dataDir, "builds")) + dirSize(filepath.Join(p.dataDir, "buildcache"))
	for _, d := range []string{"builds", "buildcache"} {
		if err := os.RemoveAll(filepath.Join(p.dataDir, d)); err != nil {
			return 0, err
		}
	}
	p.event("", "image", "cleared the build cache (%s)", humanBytes(freed))
	return freed, nil
}

// ---- live stats ----

// InstanceStats is one running instance's resource use right now.
type InstanceStats struct {
	ID       string  `json:"id"`
	CPU      float64 `json:"cpu_percent"` // 100 = one core
	MemBytes uint64  `json:"mem_bytes"`
	InFlight int64   `json:"in_flight"`
	Uptime   string  `json:"uptime"`
}

// AppStats is an app's instances plus its recent traffic.
type AppStats struct {
	App       string          `json:"app"`
	Instances []InstanceStats `json:"instances"`
	RPS       float64         `json:"rps"`
	P95       float64         `json:"p95_ms"`
	ErrorRate float64         `json:"error_rate"`
	InFlight  int64           `json:"in_flight"`
	Requests  uint64          `json:"requests_total"`
	MemLimit  int64           `json:"memory_limit_mb,omitempty"`
}

// Stats samples every instance's CPU over about a second.
func (p *Platform) Stats(ctx context.Context, only string) ([]AppStats, error) {
	type probe struct {
		app  string
		in   *instance
		rt   runtime.Runtime
		a, b runtime.Usage
		ok   bool
	}
	var probes []*probe
	var out []AppStats
	p.mu.RLock()
	names := make([]string, 0, len(p.apps))
	for n := range p.apps {
		names = append(names, n)
	}
	p.mu.RUnlock()
	sort.Strings(names)
	for _, n := range names {
		if only != "" && n != only {
			continue
		}
		a := p.get(n)
		if a == nil {
			continue
		}
		a.mu.Lock()
		st := AppStats{App: n, Instances: []InstanceStats{}, InFlight: a.inflight.Load(), MemLimit: a.spec.MemoryMB}
		for _, in := range a.instances {
			probes = append(probes, &probe{app: n, in: in, rt: a.rt})
		}
		a.mu.Unlock()
		if m, err := p.Metrics(n, "15m"); err == nil {
			st.Requests = m.Requests
			if len(m.Points) > 0 {
				last := m.Points[len(m.Points)-1]
				st.RPS, st.P95, st.ErrorRate = last.RPS, last.P95, last.ErrorRate
			}
		}
		out = append(out, st)
	}
	if only != "" && len(out) == 0 {
		return nil, fmt.Errorf("no app %q", only)
	}
	sample := func(pr *probe) (runtime.Usage, bool) {
		s, ok := pr.rt.(interface {
			Stats(context.Context, *runtime.Instance) (runtime.Usage, error)
		})
		if !ok {
			return runtime.Usage{}, false
		}
		u, err := s.Stats(ctx, pr.in.Instance)
		return u, err == nil
	}
	var wg sync.WaitGroup
	for _, pr := range probes {
		wg.Add(1)
		go func(pr *probe) {
			defer wg.Done()
			if pr.a, pr.ok = sample(pr); !pr.ok {
				return
			}
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				pr.ok = false
				return
			}
			pr.b, pr.ok = sample(pr)
		}(pr)
	}
	wg.Wait()
	idx := map[string]int{}
	for i, s := range out {
		idx[s.App] = i
	}
	for _, pr := range probes {
		is := InstanceStats{ID: pr.in.ID, InFlight: pr.in.inflight.Load(), Uptime: time.Since(pr.in.started).Round(time.Second).String()}
		if pr.ok {
			if sys := pr.b.SystemTotal - pr.a.SystemTotal; sys > 0 && pr.b.CPUTotal >= pr.a.CPUTotal {
				is.CPU = float64(pr.b.CPUTotal-pr.a.CPUTotal) / float64(sys) * float64(pr.b.CPUs) * 100
			}
			is.MemBytes = pr.b.MemBytes
		}
		i := idx[pr.app]
		out[i].Instances = append(out[i].Instances, is)
	}
	return out, nil
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// ---- HTTP ----

func (p *Platform) housekeepingAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/images", func(w http.ResponseWriter, r *http.Request) {
		list, err := p.Images(r.Context(), r.URL.Query().Get("app"))
		if err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, list)
	})
	mux.HandleFunc("GET /v1/images/inspect", func(w http.ResponseWriter, r *http.Request) {
		im, err := p.findImage(r.Context(), r.URL.Query().Get("ref"))
		if err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		d, _ := p.docker()
		det, err := d.InspectImage(r.Context(), im.ID)
		if err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"image": im, "details": det})
	})
	mux.HandleFunc("DELETE /v1/images", func(w http.ResponseWriter, r *http.Request) {
		force, _ := strconv.ParseBool(r.URL.Query().Get("force"))
		im, err := p.RemoveImage(r.Context(), r.URL.Query().Get("ref"), force)
		if err != nil {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, im)
	})
	mux.HandleFunc("POST /v1/images/prune", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Keep   int  `json:"keep"`
			DryRun bool `json:"dry_run"`
		}
		json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
		if req.Keep == 0 {
			req.Keep = 5
		}
		res, err := p.PruneImages(r.Context(), req.Keep, req.DryRun)
		if err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("POST /v1/images/tag", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Src, Dst string }
		json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
		if err := p.TagImage(r.Context(), req.Src, req.Dst); err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/images/push", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Ref, Dest string }
		json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fw := flushWriter{w}
		if err := p.PushImage(r.Context(), req.Ref, req.Dest, fw); err != nil {
			fmt.Fprintf(fw, "\nERROR: %v\n", err)
			return
		}
		fmt.Fprintln(fw, "\npushed")
	})
	mux.HandleFunc("GET /v1/images/save", func(w http.ResponseWriter, r *http.Request) {
		im, err := p.findImage(r.Context(), r.URL.Query().Get("ref"))
		if err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		d, _ := p.docker()
		ref := r.URL.Query().Get("ref")
		if !strings.HasPrefix(runtime.ShortName(ref), "xu/") {
			ref = im.Tags[0]
		}
		rc, err := d.SaveImage(r.Context(), runtime.ShortName(ref))
		if err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		defer rc.Close()
		w.Header().Set("Content-Type", "application/x-tar")
		io.Copy(w, rc)
	})
	mux.HandleFunc("GET /v1/volumes", func(w http.ResponseWriter, r *http.Request) {
		vols, err := p.Volumes(r.Context())
		if err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, vols)
	})
	mux.HandleFunc("DELETE /v1/volumes/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := p.RemoveVolume(r.Context(), r.PathValue("name")); err != nil {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/system/df", func(w http.ResponseWriter, r *http.Request) {
		u, err := p.Usage(r.Context())
		if err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, u)
	})
	mux.HandleFunc("POST /v1/system/buildcache/clean", func(w http.ResponseWriter, r *http.Request) {
		freed, err := p.CleanBuildCache()
		if err != nil {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]int64{"freed_bytes": freed})
	})
	mux.HandleFunc("GET /v1/stats", func(w http.ResponseWriter, r *http.Request) {
		st, err := p.Stats(r.Context(), r.URL.Query().Get("app"))
		if err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, st)
	})
}

type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(b []byte) (int, error) {
	n, err := f.w.Write(b)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}
