package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"xu526/internal/ghapp"
)

// Watching connected repositories for new commits. A computer behind a home
// router can't receive GitHub's webhooks, so xu526 asks instead: every 30
// seconds it reads each connected branch's latest commit (`git ls-remote`,
// with the same credentials a clone uses). A new commit is shown as
// "new commit available" with a Deploy button, or built at once when the
// source's auto-deploy switch is on.

// PendingCommit is a commit on the connected branch that isn't deployed yet.
type PendingCommit struct {
	SHA     string    `json:"sha"`
	Message string    `json:"message,omitempty"`
	Author  string    `json:"author,omitempty"`
	Date    time.Time `json:"date,omitempty"`
	FoundAt time.Time `json:"found_at"`
}

// RepoCheckInterval is how often connected repositories are checked.
var RepoCheckInterval = 30 * time.Second

func (p *Platform) watchLoop(ctx context.Context) {
	t := time.NewTicker(RepoCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.checkAllSources(ctx)
		}
	}
}

func (p *Platform) checkAllSources(ctx context.Context) {
	var ids []string
	p.mu.RLock()
	for id, s := range p.sources {
		if s.Dir == "" && s.Repo != "" && s.Disabled == "" && !s.Building {
			ids = append(ids, id)
		}
	}
	p.mu.RUnlock()
	for _, id := range ids {
		cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
		_, _ = p.CheckSource(cctx, id)
		cancel()
	}
}

// CheckSource looks for a new commit on a source's branch now.
func (p *Platform) CheckSource(ctx context.Context, id string) (Source, error) {
	src := p.source(id)
	if src == nil {
		return Source{}, errors.New("no such source")
	}
	s := *src
	if s.Dir != "" {
		return s.redacted(), errors.New("folders aren't checked: rebuild them from their page")
	}
	auth, err := p.gitAuth(ctx, &s)
	if err == nil {
		var sha string
		sha, err = remoteHead(ctx, auth, s.Branch)
		if err == nil {
			p.noteHead(ctx, &s, auth, sha)
		}
	}
	p.setSource(id, func(x *Source) {
		prev := x.CheckError
		x.CheckedAt, x.CheckError = time.Now(), ""
		if err != nil {
			x.CheckError = err.Error()
			if prev != x.CheckError { // log a problem once, not every 30 seconds
				p.log.Printf("source %s/%s: checking for new commits: %v", x.Kind, x.Target, err)
			}
		}
	})
	out := p.source(id)
	if out == nil {
		return Source{}, errors.New("no such source")
	}
	return out.redacted(), err
}

// noteHead records the branch's latest commit: nothing when it's deployed
// (or already known), a build with auto-deploy, else a pending commit.
func (p *Platform) noteHead(ctx context.Context, s *Source, auth gitAuth, sha string) {
	if sha == "" || s.Building {
		return
	}
	if sameCommit(s.LastCommit, sha) { // deployed already
		if s.Pending != nil {
			p.setSource(s.ID, func(x *Source) { x.Pending = nil })
		}
		return
	}
	if s.Pending != nil && s.Pending.SHA == sha {
		return
	}
	pc := p.commitDetails(ctx, s, auth, sha)
	p.notePending(s.ID, pc, "")
}

// notePending records a new commit (from the check or a webhook) and, with
// auto-deploy on, builds it.
func (p *Platform) notePending(id string, pc PendingCommit, delivery string) {
	src := p.source(id)
	if src == nil {
		return
	}
	s := *src
	subject, _, _ := strings.Cut(pc.Message, "\n")
	if s.AutoDeploy {
		p.event(s.Target, "deploy", "new commit %s on %s: %s; deploying it", short(pc.SHA), s.Branch, firstNonEmpty(subject, "(no message)"))
		p.TriggerBuild(id, ReleaseMeta{Commit: pc.SHA, CommitMsg: subject, Author: pc.Author, Delivery: delivery})
		return
	}
	if pc.FoundAt.IsZero() {
		pc.FoundAt = time.Now()
	}
	p.setSource(id, func(x *Source) { x.Pending = &pc })
	who := ""
	if pc.Author != "" {
		who = " by " + pc.Author
	}
	p.event(s.Target, "deploy", "new commit available on %s: %s %q%s. Deploy it from %s's page", s.Branch, short(pc.SHA),
		firstNonEmpty(subject, "(no message)"), who, s.Target)
}

// commitDetails finds a commit's message and author from GitHub, when the
// repository is on GitHub; otherwise only the SHA is known.
func (p *Platform) commitDetails(ctx context.Context, s *Source, auth gitAuth, sha string) PendingCommit {
	pc := PendingCommit{SHA: sha, FoundAt: time.Now()}
	full := s.RepoFullName
	if full == "" {
		if m := ghRepoRE.FindStringSubmatch(s.Repo); m != nil {
			full = m[1] + "/" + m[2]
		}
	}
	if full == "" {
		return pc
	}
	if p.gh != nil {
		if c, err := p.gh.CommitInfo(ctx, auth.secret, full, sha); err == nil {
			pc.Message, pc.Author, pc.Date = c.Message, c.Author, c.Date
		}
		return pc
	}
	if c, err := githubCommit(ctx, auth.secret, full, sha); err == nil {
		pc.Message, pc.Author, pc.Date = c.Message, c.Author, c.Date
	}
	return pc
}

// githubCommit asks the public GitHub API about a commit (without the app).
func githubCommit(ctx context.Context, token, full, sha string) (ghapp.Commit, error) {
	var c struct {
		Commit struct {
			Message string `json:"message"`
			Author  struct {
				Name string    `json:"name"`
				Date time.Time `json:"date"`
			} `json:"author"`
		} `json:"commit"`
	}
	req, err := http.NewRequestWithContext(ctx, "GET", GitHubAPI+"/repos/"+full+"/commits/"+sha, nil)
	if err != nil {
		return ghapp.Commit{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "xu526")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return ghapp.Commit{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ghapp.Commit{}, fmt.Errorf("GitHub answered %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		return ghapp.Commit{}, err
	}
	return ghapp.Commit{SHA: sha, Message: c.Commit.Message, Author: c.Commit.Author.Name, Date: c.Commit.Author.Date}, nil
}

// remoteHead returns the commit a branch points at, without cloning.
func remoteHead(ctx context.Context, auth gitAuth, branch string) (string, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", errors.New("git is not installed")
	}
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--", auth.url, "refs/heads/"+branch)
	cmd.Env = append(gitBaseEnv(), auth.env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(redactToken(errb.String(), auth.secret))
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git ls-remote: %s", msg)
	}
	line := strings.TrimSpace(out.String())
	if line == "" {
		return "", fmt.Errorf("the branch %s doesn't exist in %s", branch, auth.display)
	}
	sha, _, _ := strings.Cut(line, "\t")
	return strings.TrimSpace(sha), nil
}

// sameCommit reports whether two SHAs (full or abbreviated) name one commit.
func sameCommit(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}
