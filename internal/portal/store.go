package portal

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
)

// Account is someone who signed up.
type Account struct {
	ID           int64
	Email        string // lowercase
	PasswordHash string
	Verified     bool // the email address was confirmed with a code
	Created      time.Time
}

// Code is a one-time code sent by email. Only an HMAC of it is stored.
type Code struct {
	AccountID int64
	Purpose   string // "login" (sign-up and sign-in) or "reset"
	MAC       string
	Expires   time.Time
	Attempts  int
}

// Session stages.
const (
	StageCode  = "code"  // password checked (or just signed up); waiting for the emailed code
	StageReset = "reset" // forgot password; waiting for the code and a new password
	StageFull  = "full"  // signed in
)

// Session is a browser's sign-in state. The cookie holds a random ID; only
// its SHA-256 is stored.
type Session struct {
	IDHash    string
	AccountID int64 // 0 for a decoy session (see handleSignup)
	Stage     string
	Expires   time.Time
}

// Token links an account to its relay user. The token itself is never
// stored here: the relay keeps a hash of it.
type Token struct {
	AccountID int64
	RelayUser string
	Issued    time.Time
	Rotated   time.Time
}

// Pairing is a computer asking to be connected (like a TV's sign-in code):
// the dashboard shows UserCode, the person approves it on the site, and the
// computer, which holds the secret device code, collects a token.
type Pairing struct {
	DeviceHash string // SHA-256 of the computer's secret device code
	UserCode   string // e.g. KJWXBMPQ (shown as KJWX-BMPQ)
	Device     string // the computer's name, as it reported it
	IP         string // where the request came from
	Created    time.Time
	Expires    time.Time
	Status     string // pending, approved, denied, claimed
	AccountID  int64  // who approved it
	Name       string // relay name chosen at approval (first token only)
}

// Pairing states.
const (
	PairPending  = "pending"
	PairApproved = "approved"
	PairDenied   = "denied"
	PairClaimed  = "claimed"
)

// AuditEntry is one line of the portal's audit trail.
type AuditEntry struct {
	At        time.Time
	AccountID int64
	Event     string
	IP        string
	Detail    string
}

// Store keeps the portal's data. Postgres in production; memory in tests.
type Store interface {
	CreateAccount(ctx context.Context, email, passwordHash string, now time.Time) (*Account, error) // ErrExists
	AccountByEmail(ctx context.Context, email string) (*Account, error)                             // ErrNotFound
	Account(ctx context.Context, id int64) (*Account, error)
	SetVerified(ctx context.Context, id int64) error
	SetPassword(ctx context.Context, id int64, passwordHash string) error

	PutCode(ctx context.Context, c *Code) error // replaces the account's code for that purpose
	GetCode(ctx context.Context, accountID int64, purpose string) (*Code, error)
	AddCodeAttempt(ctx context.Context, accountID int64, purpose string) (int, error) // returns attempts so far
	DeleteCode(ctx context.Context, accountID int64, purpose string) error

	PutSession(ctx context.Context, s *Session) error
	GetSession(ctx context.Context, idHash string) (*Session, error)
	DeleteSession(ctx context.Context, idHash string) error
	DeleteSessions(ctx context.Context, accountID int64) error // every session of an account
	DeleteExpired(ctx context.Context, now time.Time) error    // sessions and codes

	GetToken(ctx context.Context, accountID int64) (*Token, error)
	PutToken(ctx context.Context, t *Token) error
	RelayUserTaken(ctx context.Context, name string) (bool, error)

	PutPairing(ctx context.Context, p *Pairing) error // ErrExists if the user code is taken
	PairingByUserCode(ctx context.Context, code string) (*Pairing, error)
	PairingByDevice(ctx context.Context, deviceHash string) (*Pairing, error)
	// SetPairingStatus moves a pairing from one status to another, only if
	// it's still in from (so two requests can't both claim it).
	SetPairingStatus(ctx context.Context, deviceHash, from, to string, accountID int64, name string) (bool, error)

	Audit(ctx context.Context, e AuditEntry) error
	Close()
}

// MemStore is a Store in memory, for tests and trying the portal out.
type MemStore struct {
	mu       sync.Mutex
	nextID   int64
	accounts map[int64]*Account
	codes    map[string]*Code
	sessions map[string]*Session
	tokens   map[int64]*Token
	pairings map[string]*Pairing
	audit    []AuditEntry
}

func NewMemStore() *MemStore {
	return &MemStore{accounts: map[int64]*Account{}, codes: map[string]*Code{}, sessions: map[string]*Session{}, tokens: map[int64]*Token{}, pairings: map[string]*Pairing{}}
}

func codeKey(id int64, purpose string) string { return purpose + "|" + strconv.FormatInt(id, 10) }

func (m *MemStore) CreateAccount(_ context.Context, email, hash string, now time.Time) (*Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.accounts {
		if a.Email == email {
			return nil, ErrExists
		}
	}
	m.nextID++
	a := &Account{ID: m.nextID, Email: email, PasswordHash: hash, Created: now}
	m.accounts[a.ID] = a
	c := *a
	return &c, nil
}

func (m *MemStore) AccountByEmail(_ context.Context, email string) (*Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.accounts {
		if a.Email == email {
			c := *a
			return &c, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MemStore) Account(_ context.Context, id int64) (*Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.accounts[id]
	if !ok {
		return nil, ErrNotFound
	}
	c := *a
	return &c, nil
}

func (m *MemStore) SetVerified(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a, ok := m.accounts[id]; ok {
		a.Verified = true
		return nil
	}
	return ErrNotFound
}

func (m *MemStore) SetPassword(_ context.Context, id int64, hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a, ok := m.accounts[id]; ok {
		a.PasswordHash = hash
		return nil
	}
	return ErrNotFound
}

func (m *MemStore) PutCode(_ context.Context, c *Code) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cc := *c
	m.codes[codeKey(c.AccountID, c.Purpose)] = &cc
	return nil
}

func (m *MemStore) GetCode(_ context.Context, id int64, purpose string) (*Code, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.codes[codeKey(id, purpose)]
	if !ok {
		return nil, ErrNotFound
	}
	cc := *c
	return &cc, nil
}

func (m *MemStore) AddCodeAttempt(_ context.Context, id int64, purpose string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.codes[codeKey(id, purpose)]
	if !ok {
		return 0, ErrNotFound
	}
	c.Attempts++
	return c.Attempts, nil
}

func (m *MemStore) DeleteCode(_ context.Context, id int64, purpose string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.codes, codeKey(id, purpose))
	return nil
}

func (m *MemStore) PutSession(_ context.Context, s *Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ss := *s
	m.sessions[s.IDHash] = &ss
	return nil
}

func (m *MemStore) GetSession(_ context.Context, idHash string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[idHash]
	if !ok {
		return nil, ErrNotFound
	}
	ss := *s
	return &ss, nil
}

func (m *MemStore) DeleteSession(_ context.Context, idHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, idHash)
	return nil
}

func (m *MemStore) DeleteSessions(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, s := range m.sessions {
		if s.AccountID == id {
			delete(m.sessions, k)
		}
	}
	return nil
}

func (m *MemStore) DeleteExpired(_ context.Context, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, s := range m.sessions {
		if !now.Before(s.Expires) {
			delete(m.sessions, k)
		}
	}
	for k, c := range m.codes {
		if !now.Before(c.Expires) {
			delete(m.codes, k)
		}
	}
	for k, p := range m.pairings {
		if !now.Before(p.Expires) {
			delete(m.pairings, k)
		}
	}
	return nil
}

func (m *MemStore) GetToken(_ context.Context, id int64) (*Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tokens[id]
	if !ok {
		return nil, ErrNotFound
	}
	tt := *t
	return &tt, nil
}

func (m *MemStore) PutToken(_ context.Context, t *Token) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, o := range m.tokens {
		if o.RelayUser == t.RelayUser && id != t.AccountID {
			return ErrExists
		}
	}
	tt := *t
	m.tokens[t.AccountID] = &tt
	return nil
}

func (m *MemStore) RelayUserTaken(_ context.Context, name string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.tokens {
		if t.RelayUser == name {
			return true, nil
		}
	}
	return false, nil
}

func (m *MemStore) PutPairing(_ context.Context, p *Pairing) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, o := range m.pairings {
		if o.UserCode == p.UserCode {
			return ErrExists
		}
	}
	pp := *p
	m.pairings[p.DeviceHash] = &pp
	return nil
}

func (m *MemStore) PairingByUserCode(_ context.Context, code string) (*Pairing, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.pairings {
		if p.UserCode == code {
			pp := *p
			return &pp, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MemStore) PairingByDevice(_ context.Context, h string) (*Pairing, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pairings[h]
	if !ok {
		return nil, ErrNotFound
	}
	pp := *p
	return &pp, nil
}

func (m *MemStore) SetPairingStatus(_ context.Context, h, from, to string, acct int64, name string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pairings[h]
	if !ok || p.Status != from {
		return false, nil
	}
	p.Status = to
	if acct != 0 {
		p.AccountID, p.Name = acct, name
	}
	return true, nil
}

func (m *MemStore) Audit(_ context.Context, e AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audit = append(m.audit, e)
	return nil
}

// AuditLog returns the audit trail (tests).
func (m *MemStore) AuditLog() []AuditEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]AuditEntry(nil), m.audit...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// Events lists the audit event names, in order (tests).
func (m *MemStore) Events() string {
	var e []string
	for _, a := range m.AuditLog() {
		e = append(e, a.Event)
	}
	return strings.Join(e, ",")
}

func (m *MemStore) Close() {}
