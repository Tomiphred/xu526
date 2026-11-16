package ghapp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxWebhookBody is GitHub's own payload cap.
const maxWebhookBody = 25 << 20

// VerifySignature checks X-Hub-Signature-256 (HMAC-SHA256 of the raw body)
// in constant time. A missing or malformed header fails.
func VerifySignature(secret string, body []byte, header string) bool {
	hexSig, ok := strings.CutPrefix(header, "sha256=")
	if !ok || secret == "" {
		return false
	}
	got, err := hex.DecodeString(hexSig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// bucket is a token bucket: 60 events per installation per minute.
type bucket struct {
	tokens float64
	last   time.Time
}

const (
	bucketSize = 60
	bucketRate = 1.0 // per second
)

func (s *Service) allow(inst int64) bool {
	s.flowMu.Lock()
	defer s.flowMu.Unlock()
	now := s.now()
	b := s.buckets[inst]
	if b == nil {
		b = &bucket{tokens: bucketSize, last: now}
		s.buckets[inst] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * bucketRate
	if b.tokens > bucketSize {
		b.tokens = bucketSize
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// seen records a delivery GUID and reports whether it was already handled.
func (s *Service) seen(guid string) bool {
	s.flowMu.Lock()
	defer s.flowMu.Unlock()
	if _, ok := s.deliveries[guid]; ok {
		return true
	}
	s.deliveries[guid] = s.now()
	return false
}

func (s *Service) reject(r *http.Request, reason string) {
	s.audit.Log("webhook.rejected", map[string]any{"reason": reason, "delivery": r.Header.Get("X-GitHub-Delivery"),
		"event": r.Header.Get("X-GitHub-Event"), "remote": r.RemoteAddr})
}

// WebhookHandler receives the app's webhook (public: GitHub must reach it).
func (s *Service) WebhookHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
		if err != nil {
			s.reject(r, "body too large")
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		if !VerifySignature(s.cfg.WebhookSecret, body, r.Header.Get("X-Hub-Signature-256")) {
			s.reject(r, "bad signature")
			http.Error(w, "signature does not match", http.StatusUnauthorized)
			return
		}
		guid := r.Header.Get("X-GitHub-Delivery")
		if guid == "" {
			s.reject(r, "no delivery id")
			http.Error(w, "missing X-GitHub-Delivery", http.StatusBadRequest)
			return
		}
		if s.seen(guid) {
			s.reject(r, "replayed delivery")
			io.WriteString(w, "already handled\n")
			return
		}
		var env struct {
			Installation struct {
				ID int64 `json:"id"`
			} `json:"installation"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		if env.Installation.ID != 0 && !s.allow(env.Installation.ID) {
			s.reject(r, "rate limited")
			http.Error(w, "too many events", http.StatusTooManyRequests)
			return
		}
		event := r.Header.Get("X-GitHub-Event")
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, "ok\n")
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := s.handleEvent(ctx, event, guid, body); err != nil {
				s.log.Printf("github: %s %s: %v", event, guid, err)
			}
		}()
	})
}

type webhookRepo struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
}

func toRefs(rs []webhookRepo) []RepoRef {
	out := make([]RepoRef, len(rs))
	for i, r := range rs {
		out[i] = RepoRef{ID: r.ID, FullName: r.FullName, Private: r.Private}
	}
	return out
}

// handleEvent applies a verified event. Destructive installation events are
// confirmed against the API before anything is removed.
func (s *Service) handleEvent(ctx context.Context, event, guid string, body []byte) error {
	var ev struct {
		Action       string `json:"action"`
		Installation struct {
			ID      int64 `json:"id"`
			Account struct {
				Login string `json:"login"`
			} `json:"account"`
			RepositorySelection string `json:"repository_selection"`
		} `json:"installation"`
		Repositories        []webhookRepo `json:"repositories"`
		RepositoriesAdded   []webhookRepo `json:"repositories_added"`
		RepositoriesRemoved []webhookRepo `json:"repositories_removed"`
		Sender              struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
		} `json:"sender"`
		// push
		Ref        string      `json:"ref"`
		After      string      `json:"after"`
		Deleted    bool        `json:"deleted"`
		Repository webhookRepo `json:"repository"`
		HeadCommit struct {
			Message string `json:"message"`
			Author  struct {
				Name string `json:"name"`
			} `json:"author"`
		} `json:"head_commit"`
	}
	if err := json.Unmarshal(body, &ev); err != nil {
		return err
	}
	inst := ev.Installation.ID
	actor := "webhook:" + guid
	switch event {
	case "ping":
		return nil
	case "push":
		if inst == 0 || ev.Repository.ID == 0 {
			return nil
		}
		msg, _, _ := strings.Cut(ev.HeadCommit.Message, "\n")
		s.hooks.Push(PushEvent{Installation: inst, RepoID: ev.Repository.ID, RepoFullName: ev.Repository.FullName,
			Ref: ev.Ref, After: ev.After, Deleted: ev.Deleted, Message: msg, Author: ev.HeadCommit.Author.Name, Delivery: guid})
		return nil
	case "installation":
		switch ev.Action {
		case "created", "new_permissions_accepted", "unsuspend":
			in, err := s.confirmInstallation(ctx, inst)
			if err != nil {
				return err
			}
			cur := s.upsertInstallation(in)
			if ev.Action == "created" {
				s.setRepos(inst, toRefs(ev.Repositories))
			}
			s.audit.Log("installation."+map[string]string{"created": "created", "new_permissions_accepted": "permissions", "unsuspend": "unsuspend"}[ev.Action],
				map[string]any{"actor": actor, "installation": inst, "account": cur.AccountLogin, "selection": cur.Selection,
					"perms": cur.Permissions, "sender": ev.Sender.Login})
			if ev.Action == "unsuspend" {
				s.hooks.InstallationBack(inst)
			}
		case "deleted":
			if _, err := s.confirmInstallation(ctx, inst); err == nil {
				s.log.Printf("github: ignoring installation.deleted for %d: GitHub still lists it", inst)
				return nil
			} else if !NotFound(err) {
				return err
			}
			s.mu.Lock()
			delete(s.st.Installations, inst)
			s.saveLocked()
			s.mu.Unlock()
			s.Evict(inst, nil, "installation deleted")
			s.audit.Log("installation.deleted", map[string]any{"actor": actor, "installation": inst, "account": ev.Installation.Account.Login, "sender": ev.Sender.Login})
			s.hooks.InstallationGone(inst, "the GitHub App was uninstalled from "+ev.Installation.Account.Login)
		case "suspend":
			in, err := s.confirmInstallation(ctx, inst)
			if err != nil {
				return err
			}
			if in.SuspendedAt == nil {
				return nil
			}
			s.upsertInstallation(in)
			s.Evict(inst, nil, "installation suspended")
			s.audit.Log("installation.suspend", map[string]any{"actor": actor, "installation": inst, "sender": ev.Sender.Login})
			s.hooks.InstallationGone(inst, "the GitHub App installation on "+ev.Installation.Account.Login+" is suspended")
		}
	case "installation_repositories":
		var removed []int64
		for _, r := range ev.RepositoriesRemoved {
			removed = append(removed, r.ID)
		}
		s.mu.Lock()
		if in := s.st.Installations[inst]; in != nil {
			if ev.Installation.RepositorySelection != "" {
				in.Selection = ev.Installation.RepositorySelection
			}
			keep := in.Repos[:0]
			gone := map[int64]bool{}
			for _, id := range removed {
				gone[id] = true
			}
			for _, r := range in.Repos {
				if !gone[r.ID] {
					keep = append(keep, r)
				}
			}
			in.Repos = append(keep, toRefs(ev.RepositoriesAdded)...)
			in.UpdatedAt = s.now()
			s.saveLocked()
		}
		s.mu.Unlock()
		if len(ev.RepositoriesAdded) > 0 {
			s.audit.Log("installation.repos_added", map[string]any{"actor": actor, "installation": inst, "repos": toRefs(ev.RepositoriesAdded), "sender": ev.Sender.Login})
		}
		if len(removed) > 0 {
			s.Evict(inst, removed, "repositories removed")
			s.audit.Log("installation.repos_removed", map[string]any{"actor": actor, "installation": inst, "repos": removed, "sender": ev.Sender.Login})
			s.hooks.ReposRemoved(inst, removed)
		}
	case "github_app_authorization":
		if ev.Action == "revoked" {
			s.revokeGitHubUser(ev.Sender.ID, "authorization revoked on GitHub")
		}
	}
	return nil
}

func (s *Service) setRepos(inst int64, repos []RepoRef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if in := s.st.Installations[inst]; in != nil && len(repos) > 0 {
		in.Repos = repos
		s.saveLocked()
	}
}
