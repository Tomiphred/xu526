package ghapp

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// cachedToken is an installation token held only in memory.
type cachedToken struct {
	token        string
	expires      time.Time
	installation int64
	repos        []int64
}

// evictBefore is how long before GitHub's expiry a cached token is dropped.
const evictBefore = 5 * time.Minute

func tokenKey(inst int64, repos []int64, perms map[string]string) string {
	r := append([]int64(nil), repos...)
	sort.Slice(r, func(i, j int) bool { return r[i] < r[j] })
	var p []string
	for k, v := range perms {
		p = append(p, k+"="+v)
	}
	sort.Strings(p)
	var rs []string
	for _, id := range r {
		rs = append(rs, strconv.FormatInt(id, 10))
	}
	return fmt.Sprintf("%d|%s|%s", inst, strings.Join(rs, ","), strings.Join(p, ","))
}

// InstallationToken returns a token for exactly these repositories and
// permissions, minting one if no cached token is valid for at least five
// more minutes. Tokens are never persisted. Every mint is audited.
func (s *Service) InstallationToken(ctx context.Context, inst int64, repos []int64, perms map[string]string, actor string) (string, error) {
	if in, ok := s.Installation(inst); ok && in.SuspendedAt != nil {
		return "", fmt.Errorf("installation %d (%s) is suspended", inst, in.AccountLogin)
	}
	key := tokenKey(inst, repos, perms)
	s.tokMu.Lock()
	if t, ok := s.tokens[key]; ok && s.now().Before(t.expires) {
		s.tokMu.Unlock()
		return t.token, nil
	}
	s.tokMu.Unlock()

	var resp struct {
		Token        string            `json:"token"`
		ExpiresAt    time.Time         `json:"expires_at"`
		Permissions  map[string]string `json:"permissions"`
		Repositories []struct {
			ID int64 `json:"id"`
		} `json:"repositories"`
	}
	body := map[string]any{"permissions": perms}
	if len(repos) > 0 {
		body["repository_ids"] = repos
	}
	fields := map[string]any{"actor": actor, "installation": inst, "repos": repos, "perms": perms}
	err := s.appDo(ctx, "POST", "/app/installations/"+strconv.FormatInt(inst, 10)+"/access_tokens", body, &resp)
	if err == nil {
		err = checkGrant(perms, repos, resp.Permissions, resp.Repositories)
	}
	if err == nil && resp.Token == "" {
		err = fmt.Errorf("GitHub returned no token")
	}
	if err != nil {
		fields["result"], fields["error"] = "error", err.Error()
		s.audit.Log("token.mint", fields)
		return "", fmt.Errorf("installation token: %w", err)
	}
	fields["result"], fields["expires"], fields["token_fp"] = "ok", resp.ExpiresAt.UTC().Format(time.RFC3339), fingerprint(resp.Token)
	s.audit.Log("token.mint", fields)
	s.tokMu.Lock()
	s.tokens[key] = cachedToken{token: resp.Token, expires: resp.ExpiresAt.Add(-evictBefore), installation: inst, repos: repos}
	s.tokMu.Unlock()
	return resp.Token, nil
}

// checkGrant refuses tokens wider than requested. metadata:read is implied
// for every installation token, so it is allowed.
func checkGrant(want map[string]string, repos []int64, got map[string]string, gotRepos []struct {
	ID int64 `json:"id"`
}) error {
	for k, v := range got {
		if w, ok := want[k]; ok && w == v {
			continue
		}
		if k == "metadata" && v == "read" {
			continue
		}
		return fmt.Errorf("GitHub granted %s:%s, which wasn't requested; refusing the token", k, v)
	}
	if len(repos) > 0 {
		allowed := map[int64]bool{}
		for _, r := range repos {
			allowed[r] = true
		}
		if len(gotRepos) == 0 {
			return fmt.Errorf("GitHub returned a token for all repositories instead of %v; refusing it", repos)
		}
		for _, r := range gotRepos {
			if !allowed[r.ID] {
				return fmt.Errorf("GitHub returned a token that also covers repository %d; refusing it", r.ID)
			}
		}
	}
	return nil
}

// Evict drops cached tokens for an installation (all repos if repos is empty).
func (s *Service) Evict(inst int64, repos []int64, reason string) {
	drop := map[int64]bool{}
	for _, r := range repos {
		drop[r] = true
	}
	s.tokMu.Lock()
	n := 0
	for k, t := range s.tokens {
		if t.installation != inst {
			continue
		}
		hit := len(repos) == 0
		for _, r := range t.repos {
			hit = hit || drop[r]
		}
		if hit {
			delete(s.tokens, k)
			n++
		}
	}
	s.tokMu.Unlock()
	if n > 0 {
		s.audit.Log("token.evict", map[string]any{"installation": inst, "repos": repos, "count": n, "reason": reason})
	}
}

// CloneAccess is what a clone needs: the repository as GitHub reports it
// now, and a token for just that repository.
type CloneAccess struct {
	Repo  RepoRef
	Token string
}

// Clone mints (or reuses) a contents:read token for one repository and
// looks the repository up by ID, so a rename or transfer can't redirect it.
func (s *Service) Clone(ctx context.Context, inst, repo int64, actor string) (*CloneAccess, error) {
	tok, err := s.InstallationToken(ctx, inst, []int64{repo}, map[string]string{"contents": "read"}, actor)
	if err != nil {
		return nil, err
	}
	var r ghRepo
	if err := s.tokenDo(ctx, tok, "GET", "/repositories/"+strconv.FormatInt(repo, 10), nil, &r); err != nil {
		return nil, fmt.Errorf("repository %d: %w", repo, err)
	}
	return &CloneAccess{Repo: r.ref(), Token: tok}, nil
}

// CloneAs is Clone for the shared app: it reads the repository with the
// user's own token, which reaches only repositories both they and the app
// can access, with the app's permissions (Contents: read).
func (s *Service) CloneAs(ctx context.Context, uid string, repo int64) (*CloneAccess, error) {
	tok, err := s.userAccessToken(ctx, uid)
	if err != nil {
		return nil, err
	}
	var r ghRepo
	if err := s.tokenDo(ctx, tok, "GET", "/repositories/"+strconv.FormatInt(repo, 10), nil, &r); err != nil {
		return nil, fmt.Errorf("repository %d: %w", repo, err)
	}
	return &CloneAccess{Repo: r.ref(), Token: tok}, nil
}

// Branches lists a repository's branches with the user's token (the
// Metadata permission covers this), after checking they can reach it.
func (s *Service) Branches(ctx context.Context, uid string, repo int64) ([]string, error) {
	ca, err := s.CloneAs(ctx, uid, repo)
	if err != nil {
		return nil, err
	}
	list, err := paginate[struct {
		Name string `json:"name"`
	}](ctx, s, ca.Token, "/repos/"+ca.Repo.FullName+"/branches", "")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(list))
	for _, b := range list {
		out = append(out, b.Name)
	}
	return out, nil
}

// Commit describes a commit, for "new commit available".
type Commit struct {
	SHA     string    `json:"sha"`
	Message string    `json:"message"`
	Author  string    `json:"author"`
	Date    time.Time `json:"date"`
}

// CommitInfo looks a commit up with the token a clone would use.
func (s *Service) CommitInfo(ctx context.Context, token, fullName, sha string) (Commit, error) {
	var c struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
			Author  struct {
				Name string    `json:"name"`
				Date time.Time `json:"date"`
			} `json:"author"`
		} `json:"commit"`
		Author *struct {
			Login string `json:"login"`
		} `json:"author"`
	}
	if err := s.tokenDo(ctx, token, "GET", "/repos/"+fullName+"/commits/"+sha, nil, &c); err != nil {
		return Commit{SHA: sha}, err
	}
	out := Commit{SHA: c.SHA, Message: c.Commit.Message, Author: c.Commit.Author.Name, Date: c.Commit.Author.Date}
	if c.Author != nil && c.Author.Login != "" {
		out.Author = c.Author.Login
	}
	return out, nil
}

// CloneURL is the HTTPS URL for a repository, built only from GitHub's
// own full_name, never from user input.
func (s *Service) CloneURL(fullName string) string {
	return s.cfg.WebURL + "/" + fullName + ".git"
}

// GitEnv passes the token to git through environment-provided config, so
// it never appears in argv (ps), the remote URL, or .git/config. The header
// is scoped to the GitHub host only.
func (s *Service) GitEnv(token string) []string {
	cred := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return []string{"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http." + s.cfg.WebURL + "/.extraheader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + cred}
}
