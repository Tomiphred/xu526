package portal

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"xu526/internal/pgwire"
)

// PGStore keeps the portal's data in PostgreSQL, through database/sql.
// It uses xu526's own driver ("xupg"); a full driver such as pgx works by
// changing the name passed to OpenPG's sql.Open.
type PGStore struct{ db *sql.DB }

// migrations run in order, once each; their index is the schema version.
var migrations = []string{
	`CREATE TABLE portal_accounts (
		id            BIGSERIAL PRIMARY KEY,
		email         TEXT NOT NULL UNIQUE CHECK (email = lower(email)),
		password_hash TEXT NOT NULL,
		verified_at   TIMESTAMPTZ,
		created_at    TIMESTAMPTZ NOT NULL
	);
	CREATE TABLE portal_codes (
		account_id BIGINT NOT NULL REFERENCES portal_accounts(id) ON DELETE CASCADE,
		purpose    TEXT NOT NULL,
		mac        TEXT NOT NULL,
		expires_at TIMESTAMPTZ NOT NULL,
		attempts   INT NOT NULL DEFAULT 0,
		PRIMARY KEY (account_id, purpose)
	);
	CREATE TABLE portal_sessions (
		id_hash    TEXT PRIMARY KEY,
		account_id BIGINT REFERENCES portal_accounts(id) ON DELETE CASCADE,
		stage      TEXT NOT NULL,
		expires_at TIMESTAMPTZ NOT NULL
	);
	CREATE INDEX portal_sessions_account ON portal_sessions (account_id);
	CREATE TABLE portal_tokens (
		account_id BIGINT PRIMARY KEY REFERENCES portal_accounts(id) ON DELETE CASCADE,
		relay_user TEXT NOT NULL UNIQUE,
		issued_at  TIMESTAMPTZ NOT NULL,
		rotated_at TIMESTAMPTZ
	);
	CREATE TABLE portal_audit (
		id         BIGSERIAL PRIMARY KEY,
		at         TIMESTAMPTZ NOT NULL,
		account_id BIGINT,
		event      TEXT NOT NULL,
		ip         TEXT NOT NULL,
		detail     TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX portal_audit_account ON portal_audit (account_id, at);`,
	`CREATE TABLE portal_pairings (
		device_hash TEXT PRIMARY KEY,
		user_code   TEXT NOT NULL UNIQUE,
		device      TEXT NOT NULL,
		ip          TEXT NOT NULL,
		created_at  TIMESTAMPTZ NOT NULL,
		expires_at  TIMESTAMPTZ NOT NULL,
		status      TEXT NOT NULL,
		account_id  BIGINT REFERENCES portal_accounts(id) ON DELETE CASCADE,
		name        TEXT NOT NULL DEFAULT ''
	);`,
}

// OpenPG connects and brings the schema up to date.
func OpenPG(ctx context.Context, url string) (*PGStore, error) {
	db, err := sql.Open("xupg", url)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("portal database: %w", err)
	}
	s := &PGStore{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("portal database: %w", err)
	}
	return s, nil
}

func (s *PGStore) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS portal_schema (version INT NOT NULL)`); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// One migrator at a time, even with two xud processes.
	if _, err := tx.ExecContext(ctx, `LOCK TABLE portal_schema IN EXCLUSIVE MODE`); err != nil {
		return err
	}
	var v int64
	err = tx.QueryRowContext(ctx, `SELECT version FROM portal_schema`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO portal_schema (version) VALUES (0)`); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if int(v) > len(migrations) {
		return fmt.Errorf("the database schema (version %d) is newer than this xu526-server (%d); upgrade xu526-server", v, len(migrations))
	}
	for i := int(v); i < len(migrations); i++ {
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE portal_schema SET version = $1`, int64(len(migrations))); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PGStore) Close() { s.db.Close() }

func (s *PGStore) account(row *sql.Row) (*Account, error) {
	var a Account
	var verified sql.NullTime
	if err := row.Scan(&a.ID, &a.Email, &a.PasswordHash, &verified, &a.Created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	a.Verified = verified.Valid
	return &a, nil
}

const accountCols = `id, email, password_hash, verified_at, created_at`

func (s *PGStore) CreateAccount(ctx context.Context, email, hash string, now time.Time) (*Account, error) {
	a, err := s.account(s.db.QueryRowContext(ctx,
		`INSERT INTO portal_accounts (email, password_hash, created_at) VALUES ($1, $2, $3) RETURNING `+accountCols, email, hash, now))
	if pgwire.IsUniqueViolation(err) {
		return nil, ErrExists
	}
	return a, err
}

func (s *PGStore) AccountByEmail(ctx context.Context, email string) (*Account, error) {
	return s.account(s.db.QueryRowContext(ctx, `SELECT `+accountCols+` FROM portal_accounts WHERE email = $1`, email))
}

func (s *PGStore) Account(ctx context.Context, id int64) (*Account, error) {
	return s.account(s.db.QueryRowContext(ctx, `SELECT `+accountCols+` FROM portal_accounts WHERE id = $1`, id))
}

func (s *PGStore) exec1(ctx context.Context, q string, args ...any) error {
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PGStore) SetVerified(ctx context.Context, id int64) error {
	return s.exec1(ctx, `UPDATE portal_accounts SET verified_at = COALESCE(verified_at, now()) WHERE id = $1`, id)
}

func (s *PGStore) SetPassword(ctx context.Context, id int64, hash string) error {
	return s.exec1(ctx, `UPDATE portal_accounts SET password_hash = $2 WHERE id = $1`, id, hash)
}

func (s *PGStore) PutCode(ctx context.Context, c *Code) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO portal_codes (account_id, purpose, mac, expires_at, attempts) VALUES ($1, $2, $3, $4, 0)
		ON CONFLICT (account_id, purpose) DO UPDATE SET mac = EXCLUDED.mac, expires_at = EXCLUDED.expires_at, attempts = 0`,
		c.AccountID, c.Purpose, c.MAC, c.Expires)
	return err
}

func (s *PGStore) GetCode(ctx context.Context, id int64, purpose string) (*Code, error) {
	c := Code{AccountID: id, Purpose: purpose}
	var attempts int64
	err := s.db.QueryRowContext(ctx, `SELECT mac, expires_at, attempts FROM portal_codes WHERE account_id = $1 AND purpose = $2`, id, purpose).
		Scan(&c.MAC, &c.Expires, &attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	c.Attempts = int(attempts)
	return &c, err
}

func (s *PGStore) AddCodeAttempt(ctx context.Context, id int64, purpose string) (int, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `UPDATE portal_codes SET attempts = attempts + 1 WHERE account_id = $1 AND purpose = $2 RETURNING attempts`, id, purpose).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return int(n), err
}

func (s *PGStore) DeleteCode(ctx context.Context, id int64, purpose string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM portal_codes WHERE account_id = $1 AND purpose = $2`, id, purpose)
	return err
}

func (s *PGStore) PutSession(ctx context.Context, x *Session) error {
	var acct any
	if x.AccountID != 0 {
		acct = x.AccountID
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO portal_sessions (id_hash, account_id, stage, expires_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (id_hash) DO UPDATE SET account_id = EXCLUDED.account_id, stage = EXCLUDED.stage, expires_at = EXCLUDED.expires_at`,
		x.IDHash, acct, x.Stage, x.Expires)
	return err
}

func (s *PGStore) GetSession(ctx context.Context, idHash string) (*Session, error) {
	x := Session{IDHash: idHash}
	var acct sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT account_id, stage, expires_at FROM portal_sessions WHERE id_hash = $1`, idHash).Scan(&acct, &x.Stage, &x.Expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	x.AccountID = acct.Int64
	return &x, err
}

func (s *PGStore) DeleteSession(ctx context.Context, idHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM portal_sessions WHERE id_hash = $1`, idHash)
	return err
}

func (s *PGStore) DeleteSessions(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM portal_sessions WHERE account_id = $1`, id)
	return err
}

func (s *PGStore) DeleteExpired(ctx context.Context, now time.Time) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM portal_sessions WHERE expires_at <= $1`, now); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM portal_codes WHERE expires_at <= $1`, now); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM portal_pairings WHERE expires_at <= $1`, now)
	return err
}

func (s *PGStore) PutPairing(ctx context.Context, p *Pairing) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO portal_pairings (device_hash, user_code, device, ip, created_at, expires_at, status) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		p.DeviceHash, p.UserCode, p.Device, p.IP, p.Created, p.Expires, p.Status)
	if pgwire.IsUniqueViolation(err) {
		return ErrExists
	}
	return err
}

const pairingCols = `device_hash, user_code, device, ip, created_at, expires_at, status, account_id, name`

func (s *PGStore) pairing(row *sql.Row) (*Pairing, error) {
	var p Pairing
	var acct sql.NullInt64
	err := row.Scan(&p.DeviceHash, &p.UserCode, &p.Device, &p.IP, &p.Created, &p.Expires, &p.Status, &acct, &p.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	p.AccountID = acct.Int64
	return &p, err
}

func (s *PGStore) PairingByUserCode(ctx context.Context, code string) (*Pairing, error) {
	return s.pairing(s.db.QueryRowContext(ctx, `SELECT `+pairingCols+` FROM portal_pairings WHERE user_code = $1`, code))
}

func (s *PGStore) PairingByDevice(ctx context.Context, h string) (*Pairing, error) {
	return s.pairing(s.db.QueryRowContext(ctx, `SELECT `+pairingCols+` FROM portal_pairings WHERE device_hash = $1`, h))
}

func (s *PGStore) SetPairingStatus(ctx context.Context, h, from, to string, acct int64, name string) (bool, error) {
	var res sql.Result
	var err error
	if acct != 0 {
		res, err = s.db.ExecContext(ctx, `UPDATE portal_pairings SET status = $3, account_id = $4, name = $5 WHERE device_hash = $1 AND status = $2`, h, from, to, acct, name)
	} else {
		res, err = s.db.ExecContext(ctx, `UPDATE portal_pairings SET status = $3 WHERE device_hash = $1 AND status = $2`, h, from, to)
	}
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *PGStore) GetToken(ctx context.Context, id int64) (*Token, error) {
	t := Token{AccountID: id}
	var rotated sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT relay_user, issued_at, rotated_at FROM portal_tokens WHERE account_id = $1`, id).Scan(&t.RelayUser, &t.Issued, &rotated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	t.Rotated = rotated.Time
	return &t, err
}

func (s *PGStore) PutToken(ctx context.Context, t *Token) error {
	var rotated any
	if !t.Rotated.IsZero() {
		rotated = t.Rotated
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO portal_tokens (account_id, relay_user, issued_at, rotated_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (account_id) DO UPDATE SET relay_user = EXCLUDED.relay_user, issued_at = EXCLUDED.issued_at, rotated_at = EXCLUDED.rotated_at`,
		t.AccountID, t.RelayUser, t.Issued, rotated)
	if pgwire.IsUniqueViolation(err) {
		return ErrExists
	}
	return err
}

func (s *PGStore) RelayUserTaken(ctx context.Context, name string) (bool, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM portal_tokens WHERE relay_user = $1`, name).Scan(&n)
	return n > 0, err
}

func (s *PGStore) Audit(ctx context.Context, e AuditEntry) error {
	var acct any
	if e.AccountID != 0 {
		acct = e.AccountID
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO portal_audit (at, account_id, event, ip, detail) VALUES ($1, $2, $3, $4, $5)`,
		e.At, acct, e.Event, e.IP, e.Detail)
	return err
}

// AuditLog returns an account's audit entries, oldest first.
func (s *PGStore) AuditLog(ctx context.Context, accountID int64) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT at, event, ip, detail FROM portal_audit WHERE account_id = $1 ORDER BY at, id`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		e := AuditEntry{AccountID: accountID}
		if err := rows.Scan(&e.At, &e.Event, &e.IP, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
