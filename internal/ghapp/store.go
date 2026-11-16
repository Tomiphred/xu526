package ghapp

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Installation mirrors a GitHub App installation, confirmed against the API.
type Installation struct {
	ID           int64             `json:"id"`
	AccountID    int64             `json:"account_id"`
	AccountLogin string            `json:"account_login"`
	AccountType  string            `json:"account_type"` // User | Organization
	Selection    string            `json:"repository_selection"`
	Repos        []RepoRef         `json:"repos,omitempty"`
	Permissions  map[string]string `json:"permissions"`
	SuspendedAt  *time.Time        `json:"suspended_at,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
}

// RepoRef identifies a repository by its immutable ID.
type RepoRef struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch,omitempty"`
}

// User is someone who signed in with GitHub. The 8-hour access token is
// held only in memory; the refresh token is sealed.
type User struct {
	ID               string    `json:"id"`
	GitHubID         int64     `json:"github_id"`
	Login            string    `json:"login"`
	RefreshToken     Sealed    `json:"refresh_token,omitempty"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
	CreatedAt        time.Time `json:"created_at"`
	LastSeenAt       time.Time `json:"last_seen_at"`
}

// Session is a signed-in CLI or browser. Only the token's hash is stored.
type Session struct {
	Hash      string    `json:"hash"`
	UserID    string    `json:"user_id"`
	Kind      string    `json:"kind"` // cli | web
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type state struct {
	Installations map[int64]*Installation `json:"installations"`
	Users         map[string]*User        `json:"users"`
	Sessions      map[string]*Session     `json:"sessions"`
	// Pins maps an allowed login (lowercase) to the GitHub user ID that
	// first signed in with it, so a renamed-and-reclaimed login can't sign in.
	Pins map[string]int64 `json:"pins"`
}

func newState() *state {
	return &state{Installations: map[int64]*Installation{}, Users: map[string]*User{}, Sessions: map[string]*Session{}, Pins: map[string]int64{}}
}

func loadState(path string) (*state, error) {
	st := newState()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, err
	}
	if st.Installations == nil {
		st.Installations = map[int64]*Installation{}
	}
	if st.Users == nil {
		st.Users = map[string]*User{}
	}
	if st.Sessions == nil {
		st.Sessions = map[string]*Session{}
	}
	if st.Pins == nil {
		st.Pins = map[string]int64{}
	}
	return st, nil
}

func (st *state) save(path string) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
