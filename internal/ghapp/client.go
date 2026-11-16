package ghapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// APIError is a non-2xx answer from GitHub.
type APIError struct {
	Status  int
	Message string
	Path    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("GitHub %s: %d %s", e.Path, e.Status, e.Message)
}

// NotFound reports whether GitHub answered 404.
func NotFound(err error) bool {
	e, ok := err.(*APIError)
	return ok && e.Status == http.StatusNotFound
}

func (s *Service) send(ctx context.Context, method, rawURL, auth string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "xu526")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		json.Unmarshal(b, &e)
		if e.Message == "" {
			e.Message = http.StatusText(resp.StatusCode)
		}
		u, _ := url.Parse(rawURL)
		return &APIError{Status: resp.StatusCode, Message: e.Message, Path: u.Path}
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

// appDo calls the API as the app (JWT). If GitHub rejects the first key
// (after a rotation), the fallback key is tried.
func (s *Service) appDo(ctx context.Context, method, path string, body, out any) error {
	var err error
	for i, k := range s.cfg.Keys {
		jwt, jerr := AppJWT(k, s.cfg.ClientID, s.now())
		if jerr != nil {
			return jerr
		}
		err = s.send(ctx, method, s.cfg.APIURL+path, "Bearer "+jwt, body, out)
		if e, ok := err.(*APIError); ok && e.Status == http.StatusUnauthorized && i+1 < len(s.cfg.Keys) {
			s.log.Printf("github: key %d rejected, trying the fallback key", i+1)
			continue
		}
		return err
	}
	return err
}

// tokenDo calls the API with a user or installation token.
func (s *Service) tokenDo(ctx context.Context, token, method, path string, body, out any) error {
	return s.send(ctx, method, s.cfg.APIURL+path, "Bearer "+token, body, out)
}

// paginate fetches every page of a list endpoint. field names the array in
// wrapped responses ({"total_count":…,"repositories":[…]}), or "" for bare arrays.
func paginate[T any](ctx context.Context, s *Service, token, path, field string) ([]T, error) {
	var all []T
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	for page := 1; page <= 50; page++ {
		var raw json.RawMessage
		if err := s.tokenDo(ctx, token, "GET", path+sep+"per_page=100&page="+strconv.Itoa(page), nil, &raw); err != nil {
			return nil, err
		}
		var items []T
		if field == "" {
			if err := json.Unmarshal(raw, &items); err != nil {
				return nil, err
			}
		} else {
			var wrap map[string]json.RawMessage
			if err := json.Unmarshal(raw, &wrap); err != nil {
				return nil, err
			}
			if err := json.Unmarshal(wrap[field], &items); err != nil {
				return nil, err
			}
		}
		all = append(all, items...)
		if len(items) < 100 {
			break
		}
	}
	return all, nil
}

// oauthPost calls github.com/login/oauth/* (form encoded, JSON answer).
// OAuth errors come back as 200 with an "error" field.
func (s *Service) oauthPost(ctx context.Context, path string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, "POST", s.cfg.WebURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "xu526")
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, Message: strings.TrimSpace(string(b)), Path: path}
	}
	return json.Unmarshal(b, out)
}

type ghInstallation struct {
	ID      int64 `json:"id"`
	Account struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"account"`
	RepositorySelection string            `json:"repository_selection"`
	Permissions         map[string]string `json:"permissions"`
	SuspendedAt         *string           `json:"suspended_at"`
	AppID               int64             `json:"app_id"`
}

type ghRepo struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
}

func (r ghRepo) ref() RepoRef {
	return RepoRef{ID: r.ID, FullName: r.FullName, Private: r.Private, DefaultBranch: r.DefaultBranch}
}

// confirmInstallation fetches an installation as the app, the only source
// of truth for its account and settings. A 404 means it's gone.
func (s *Service) confirmInstallation(ctx context.Context, id int64) (*ghInstallation, error) {
	var in ghInstallation
	if err := s.appDo(ctx, "GET", "/app/installations/"+strconv.FormatInt(id, 10), nil, &in); err != nil {
		return nil, err
	}
	if in.AppID != 0 && in.AppID != s.cfg.AppID {
		return nil, fmt.Errorf("installation %d belongs to app %d, not this app", id, in.AppID)
	}
	return &in, nil
}

// upsertInstallation records the confirmed installation.
func (s *Service) upsertInstallation(in *ghInstallation) *Installation {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.st.Installations[in.ID]
	if cur == nil {
		cur = &Installation{ID: in.ID, CreatedAt: s.now()}
		s.st.Installations[in.ID] = cur
	}
	cur.AccountID, cur.AccountLogin, cur.AccountType = in.Account.ID, in.Account.Login, in.Account.Type
	cur.Selection, cur.Permissions, cur.UpdatedAt = in.RepositorySelection, in.Permissions, s.now()
	cur.SuspendedAt = nil
	if in.SuspendedAt != nil && *in.SuspendedAt != "" {
		t := s.now()
		cur.SuspendedAt = &t
	}
	s.saveLocked()
	cp := *cur
	return &cp
}
