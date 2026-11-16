package platform

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Release is one numbered version of an app or stack. Every deploy, build and
// rollback creates one. It pins exact image IDs, so restoring it brings back
// the same bits even if a tag was overwritten since.
type Release struct {
	Version    int                    `json:"version"`
	Kind       string                 `json:"kind"`   // app | stack
	Target     string                 `json:"target"` // app or stack name
	Status     string                 `json:"status"` // building, deploying, live, failed, superseded
	Error      string                 `json:"error,omitempty"`
	Source     string                 `json:"source"` // manual, github, rollback
	Commit     string                 `json:"commit,omitempty"`
	CommitMsg  string                 `json:"commit_message,omitempty"`
	Author     string                 `json:"author,omitempty"`
	Branch     string                 `json:"branch,omitempty"`
	RollbackOf int                    `json:"rollback_of,omitempty"`
	Spec       *AppSpec               `json:"spec,omitempty"`
	YAML       string                 `json:"yaml,omitempty"`
	Pins       map[string]PinnedImage `json:"pins,omitempty"` // stack service → image
	HasLog     bool                   `json:"has_log"`
	Trigger    string                 `json:"trigger,omitempty"` // webhook delivery ID, for the audit trail
	CreatedAt  time.Time              `json:"created_at"`
	FinishedAt time.Time              `json:"finished_at,omitempty"`
}

// PinnedImage is the exact image a stack service ran.
type PinnedImage struct {
	ID     string `json:"id"`
	Digest string `json:"digest,omitempty"`
}

// ReleaseMeta describes where a deploy came from.
type ReleaseMeta struct {
	Source, Commit, CommitMsg, Author, Branch string
	RollbackOf                                int
	Delivery                                  string `json:"-"` // GitHub webhook delivery that triggered it
}

const keepReleases = 30

func relKey(kind, target string) string { return kind + ":" + target }

func (p *Platform) newRelease(kind, target, status string, m ReleaseMeta) *Release {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := relKey(kind, target)
	list := p.releases[key]
	v := 1
	if len(list) > 0 {
		v = list[len(list)-1].Version + 1
	}
	r := &Release{Version: v, Kind: kind, Target: target, Status: status, Source: m.Source,
		Commit: m.Commit, CommitMsg: m.CommitMsg, Author: m.Author, Branch: m.Branch,
		RollbackOf: m.RollbackOf, Trigger: m.Delivery, CreatedAt: time.Now()}
	list = append(list, r)
	if len(list) > keepReleases {
		for _, old := range list[:len(list)-keepReleases] {
			_ = os.Remove(p.releaseLogPath(old))
		}
		list = list[len(list)-keepReleases:]
	}
	p.releases[key] = list
	return r
}

// finishRelease marks r live (superseding the previous live one) or failed.
func (p *Platform) finishRelease(r *Release, err error) {
	p.mu.Lock()
	r.FinishedAt = time.Now()
	if err != nil {
		r.Status, r.Error = "failed", err.Error()
	} else {
		for _, o := range p.releases[relKey(r.Kind, r.Target)] {
			if o != r && o.Status == "live" {
				o.Status = "superseded"
			}
		}
		r.Status, r.Error = "live", ""
	}
	p.mu.Unlock()
	_ = p.save()
}

func (p *Platform) setReleaseStatus(r *Release, status string) {
	p.mu.Lock()
	r.Status = status
	p.mu.Unlock()
	_ = p.save()
}

func (p *Platform) releaseLogPath(r *Release) string {
	return filepath.Join(p.dataDir, "releases", r.Kind+"-"+r.Target, fmt.Sprintf("%d.log", r.Version))
}

// releaseLog opens the build/deploy log of a release for writing.
func (p *Platform) releaseLog(r *Release) io.WriteCloser {
	path := p.releaseLogPath(r)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.Create(path)
	if err != nil {
		return nopCloser{io.Discard}
	}
	p.mu.Lock()
	r.HasLog = true
	p.mu.Unlock()
	return f
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// ReleaseLog returns the build log of a release.
func (p *Platform) ReleaseLog(kind, target string, version int) ([]byte, error) {
	r := p.findRelease(kind, target, version)
	if r == nil {
		return nil, fmt.Errorf("no release %d of %s", version, target)
	}
	return os.ReadFile(p.releaseLogPath(r))
}

func (p *Platform) findRelease(kind, target string, version int) *Release {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, r := range p.releases[relKey(kind, target)] {
		if r.Version == version {
			return r
		}
	}
	return nil
}

// Releases lists an app's or stack's releases, newest first.
func (p *Platform) Releases(kind, target string) []Release {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := []Release{}
	for _, r := range p.releases[relKey(kind, target)] {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version > out[j].Version })
	return out
}

// DeployRelease deploys an app spec as a new release.
func (p *Platform) DeployRelease(ctx context.Context, spec AppSpec, m ReleaseMeta) (*Release, error) {
	r := p.newRelease("app", spec.Name, "deploying", m)
	pinned, err := p.deployApp(ctx, spec)
	if err == nil {
		p.mu.Lock()
		r.Spec = &pinned
		p.mu.Unlock()
	}
	p.finishRelease(r, err)
	return r, err
}

// Rollback redeploys an earlier release exactly as it was.
func (p *Platform) Rollback(ctx context.Context, kind, target string, version int) (*Release, error) {
	old := p.findRelease(kind, target, version)
	if old == nil {
		return nil, fmt.Errorf("no release %d of %s", version, target)
	}
	m := ReleaseMeta{Source: "rollback", RollbackOf: version, Commit: old.Commit, CommitMsg: old.CommitMsg,
		Author: old.Author, Branch: old.Branch}
	switch {
	case kind == "app" && old.Spec != nil:
		return p.DeployRelease(ctx, *old.Spec, m)
	case kind == "stack" && old.YAML != "":
		return p.deployStack(ctx, target, []byte(old.YAML), old.Pins, nil, m, nil)
	}
	return nil, fmt.Errorf("release %d never deployed successfully, so it can't be restored", version)
}
