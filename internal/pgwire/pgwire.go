// Package pgwire is a small PostgreSQL driver for database/sql, enough for
// xu526's own tables: protocol 3.0, password sign-in (SCRAM-SHA-256, MD5,
// cleartext over TLS), optional TLS, parameterised queries in text format,
// and transactions. It registers as "xupg".
//
// It deliberately skips what xu526 doesn't use (COPY, LISTEN, binary
// formats, arrays). For more, any full driver such as pgx plugs into the
// same database/sql code.
package pgwire

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"crypto/pbkdf2"
)

func init() { sql.Register("xupg", Driver{}) }

// Driver implements driver.Driver and driver.DriverContext.
type Driver struct{}

func (Driver) Open(dsn string) (driver.Conn, error) {
	c, err := Driver{}.OpenConnector(dsn)
	if err != nil {
		return nil, err
	}
	return c.Connect(context.Background())
}

func (Driver) OpenConnector(dsn string) (driver.Connector, error) {
	cfg, err := ParseURL(dsn)
	if err != nil {
		return nil, err
	}
	return &connector{cfg: cfg}, nil
}

// Config is a parsed connection URL.
type Config struct {
	Host, Port, User, Password, Database string
	SSLMode                              string // disable, require, verify-full
	ConnectTimeout                       time.Duration
	TLS                                  *tls.Config // overrides the default for verify-full (tests)
}

// ParseURL reads postgres://user:password@host:port/db?sslmode=require.
func ParseURL(s string) (*Config, error) {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return nil, errors.New("pgwire: the database URL must look like postgres://user:password@host:5432/dbname")
	}
	cfg := &Config{Host: u.Hostname(), Port: u.Port(), Database: strings.TrimPrefix(u.Path, "/"), SSLMode: "prefer", ConnectTimeout: 10 * time.Second}
	if u.User != nil {
		cfg.User = u.User.Username()
		cfg.Password, _ = u.User.Password()
	}
	if cfg.Host == "" {
		cfg.Host = "localhost"
	}
	if cfg.Port == "" {
		cfg.Port = "5432"
	}
	q := u.Query()
	if m := q.Get("sslmode"); m != "" {
		switch m {
		case "disable", "prefer", "require", "verify-full":
			cfg.SSLMode = m
		default:
			return nil, fmt.Errorf("pgwire: sslmode %q isn't supported (disable, prefer, require, verify-full)", m)
		}
	}
	if t := q.Get("connect_timeout"); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil {
			return nil, fmt.Errorf("pgwire: connect_timeout: %w", err)
		}
		cfg.ConnectTimeout = time.Duration(n) * time.Second
	}
	if cfg.User == "" || cfg.Database == "" {
		return nil, errors.New("pgwire: the database URL needs a user and a database name")
	}
	return cfg, nil
}

type connector struct{ cfg *Config }

func (c *connector) Driver() driver.Driver { return Driver{} }

func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	cn, err := Connect(ctx, c.cfg)
	if err != nil {
		return nil, err
	}
	return cn, nil
}

// Error is an error reported by the server.
type Error struct {
	Severity, Code, Message, Detail, Constraint string
}

func (e *Error) Error() string {
	s := "postgres: " + e.Message + " (SQLSTATE " + e.Code + ")"
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	return s
}

// IsUniqueViolation reports a duplicate key error.
func IsUniqueViolation(err error) bool {
	var pe *Error
	return errors.As(err, &pe) && pe.Code == "23505"
}

// conn is one connection. database/sql serialises use of a conn.
type conn struct {
	net    net.Conn
	r      *reader
	params map[string]string
	broken bool
	inTx   bool
}

// Connect opens and authenticates a connection.
func Connect(ctx context.Context, cfg *Config) (*conn, error) {
	d := net.Dialer{Timeout: cfg.ConnectTimeout}
	nc, err := d.DialContext(ctx, "tcp", net.JoinHostPort(cfg.Host, cfg.Port))
	if err != nil {
		return nil, fmt.Errorf("pgwire: connect to %s:%s: %w", cfg.Host, cfg.Port, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		nc.SetDeadline(dl)
	} else {
		nc.SetDeadline(time.Now().Add(cfg.ConnectTimeout))
	}
	secure := false
	if cfg.SSLMode != "disable" {
		// SSLRequest: length 8, code 80877103.
		if _, err := nc.Write([]byte{0, 0, 0, 8, 0x04, 0xd2, 0x16, 0x2f}); err != nil {
			nc.Close()
			return nil, err
		}
		b := make([]byte, 1)
		if _, err := io.ReadFull(nc, b); err != nil {
			nc.Close()
			return nil, err
		}
		switch {
		case b[0] == 'S':
			tc := cfg.TLS
			if tc == nil {
				tc = &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}
				if cfg.SSLMode != "verify-full" {
					// "require"/"prefer" encrypt without checking the
					// certificate, as libpq does.
					tc.InsecureSkipVerify = true
				}
			}
			t := tls.Client(nc, tc)
			if err := t.HandshakeContext(ctx); err != nil {
				nc.Close()
				return nil, fmt.Errorf("pgwire: TLS: %w", err)
			}
			nc, secure = t, true
		case cfg.SSLMode == "require" || cfg.SSLMode == "verify-full":
			nc.Close()
			return nil, errors.New("pgwire: the server doesn't offer TLS (sslmode=" + cfg.SSLMode + ")")
		}
	}
	c := &conn{net: nc, r: &reader{r: nc}, params: map[string]string{}}
	// StartupMessage: protocol 3.0 and parameters.
	var m msg
	m.int32(196608)
	for _, kv := range [][2]string{{"user", cfg.User}, {"database", cfg.Database}, {"client_encoding", "UTF8"}, {"DateStyle", "ISO"}, {"application_name", "xu526"}} {
		m.str(kv[0])
		m.str(kv[1])
	}
	m.b = append(m.b, 0)
	if err := c.send(m.untyped()); err != nil {
		nc.Close()
		return nil, err
	}
	if err := c.auth(cfg, secure); err != nil {
		nc.Close()
		return nil, err
	}
	nc.SetDeadline(time.Time{})
	return c, nil
}

func (c *conn) auth(cfg *Config, secure bool) error {
	var scram *scramClient
	for {
		t, body, err := c.r.next()
		if err != nil {
			return err
		}
		switch t {
		case 'R':
			code := binary.BigEndian.Uint32(body)
			switch code {
			case 0: // AuthenticationOk
			case 3: // cleartext
				if !secure && !isLocal(cfg.Host) {
					return errors.New("pgwire: the server asks for the password in clear text over an unencrypted connection; use sslmode=require or SCRAM")
				}
				var m msg
				m.str(cfg.Password)
				if err := c.send(m.typed('p')); err != nil {
					return err
				}
			case 5: // MD5
				salt := body[4:8]
				h := md5.Sum([]byte(cfg.Password + cfg.User))
				inner := hex.EncodeToString(h[:])
				h2 := md5.Sum(append([]byte(inner), salt...))
				var m msg
				m.str("md5" + hex.EncodeToString(h2[:]))
				if err := c.send(m.typed('p')); err != nil {
					return err
				}
			case 10: // SASL: mechanisms list
				if !strings.Contains(string(body[4:]), "SCRAM-SHA-256\x00") {
					return errors.New("pgwire: the server offers no supported SASL mechanism")
				}
				scram = newScram(cfg.User, cfg.Password)
				first := scram.clientFirst()
				var m msg
				m.str("SCRAM-SHA-256")
				m.int32(int32(len(first)))
				m.b = append(m.b, first...)
				if err := c.send(m.typed('p')); err != nil {
					return err
				}
			case 11: // SASLContinue
				final, err := scram.clientFinal(body[4:])
				if err != nil {
					return err
				}
				var m msg
				m.b = append(m.b, final...)
				if err := c.send(m.typed('p')); err != nil {
					return err
				}
			case 12: // SASLFinal
				if err := scram.verifyServer(body[4:]); err != nil {
					return err
				}
			default:
				return fmt.Errorf("pgwire: unsupported authentication method %d", code)
			}
		case 'S':
			k, rest := cstr(body)
			v, _ := cstr(rest)
			c.params[k] = v
		case 'K': // BackendKeyData
		case 'E':
			return parseError(body)
		case 'N': // notice
		case 'Z':
			return nil
		default:
			return fmt.Errorf("pgwire: unexpected message %q during startup", t)
		}
	}
}

func isLocal(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *conn) send(b []byte) error {
	if _, err := c.net.Write(b); err != nil {
		c.broken = true
		return err
	}
	return nil
}

// ---- database/sql interfaces ----

func (c *conn) Prepare(query string) (driver.Stmt, error) {
	return &stmt{c: c, q: query}, nil
}

func (c *conn) Close() error {
	c.net.Write([]byte{'X', 0, 0, 0, 4})
	return c.net.Close()
}

func (c *conn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	q := "BEGIN"
	if opts.ReadOnly {
		q += " READ ONLY"
	}
	switch sql.IsolationLevel(opts.Isolation) {
	case sql.LevelDefault:
	case sql.LevelSerializable:
		q += " ISOLATION LEVEL SERIALIZABLE"
	case sql.LevelRepeatableRead:
		q += " ISOLATION LEVEL REPEATABLE READ"
	case sql.LevelReadCommitted:
		q += " ISOLATION LEVEL READ COMMITTED"
	default:
		return nil, errors.New("pgwire: unsupported isolation level")
	}
	if _, err := c.ExecContext(ctx, q, nil); err != nil {
		return nil, err
	}
	c.inTx = true
	return &tx{c}, nil
}

func (c *conn) Ping(ctx context.Context) error {
	_, err := c.ExecContext(ctx, "SELECT 1", nil)
	return err
}

// ResetSession drops connections that failed mid-protocol.
func (c *conn) ResetSession(context.Context) error {
	if c.broken {
		return driver.ErrBadConn
	}
	return nil
}

func (c *conn) IsValid() bool { return !c.broken }

type tx struct{ c *conn }

func (t *tx) Commit() error {
	_, err := t.c.ExecContext(context.Background(), "COMMIT", nil)
	t.c.inTx = false
	return err
}

func (t *tx) Rollback() error {
	_, err := t.c.ExecContext(context.Background(), "ROLLBACK", nil)
	t.c.inTx = false
	return err
}

type stmt struct {
	c *conn
	q string
}

func (s *stmt) Close() error  { return nil }
func (s *stmt) NumInput() int { return -1 }
func (s *stmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.c.ExecContext(context.Background(), s.q, named(args))
}
func (s *stmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.c.QueryContext(context.Background(), s.q, named(args))
}

func named(args []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(args))
	for i, a := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: a}
	}
	return out
}

func (c *conn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	rows, err := c.run(ctx, q, args)
	if err != nil {
		return nil, err
	}
	for {
		dest := make([]driver.Value, len(rows.cols))
		if err := rows.Next(dest); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
	}
	return result(rows.affected), nil
}

func (c *conn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	return c.run(ctx, q, args)
}

type result int64

func (r result) LastInsertId() (int64, error) {
	return 0, errors.New("pgwire: use INSERT ... RETURNING id")
}
func (r result) RowsAffected() (int64, error) { return int64(r), nil }

// CheckNamedValue accepts the Go types encode handles.
func (c *conn) CheckNamedValue(nv *driver.NamedValue) error {
	switch v := nv.Value.(type) {
	case nil, int64, float64, bool, []byte, string, time.Time:
		return nil
	case int:
		nv.Value = int64(v)
		return nil
	case int32:
		nv.Value = int64(v)
		return nil
	}
	v, err := driver.DefaultParameterConverter.ConvertValue(nv.Value)
	if err != nil {
		return err
	}
	nv.Value = v
	return nil
}

// run sends one query with the extended protocol (unnamed statement and
// portal, text parameters and results) and returns rows to read.
func (c *conn) run(ctx context.Context, q string, args []driver.NamedValue) (*rows, error) {
	if c.broken {
		return nil, driver.ErrBadConn
	}
	if dl, ok := ctx.Deadline(); ok {
		c.net.SetDeadline(dl)
	} else {
		c.net.SetDeadline(time.Time{})
	}
	if len(args) == 0 {
		// The simple protocol: several statements may run at once
		// (migrations), and nothing is prepared.
		var m msg
		m.str(q)
		if err := c.send(m.typed('Q')); err != nil {
			return nil, driver.ErrBadConn
		}
		r := &rows{c: c, ctx: ctx}
		if err := r.advance(); err != nil {
			return nil, err
		}
		return r, nil
	}
	var out []byte
	var m msg
	m.str("")
	m.str(q)
	m.int16(0)
	out = append(out, m.typed('P')...)
	m = msg{}
	m.str("")
	m.str("")
	m.int16(0) // all parameters in text format
	m.int16(int16(len(args)))
	for _, a := range args {
		v, isNull, err := encode(a.Value)
		if err != nil {
			return nil, err
		}
		if isNull {
			m.int32(-1)
			continue
		}
		m.int32(int32(len(v)))
		m.b = append(m.b, v...)
	}
	m.int16(0) // all results in text format
	out = append(out, m.typed('B')...)
	m = msg{}
	m.b = append(m.b, 'P')
	m.str("")
	out = append(out, m.typed('D')...)
	m = msg{}
	m.str("")
	m.int32(0)
	out = append(out, m.typed('E')...)
	out = append(out, 'S', 0, 0, 0, 4)
	if err := c.send(out); err != nil {
		return nil, driver.ErrBadConn
	}
	r := &rows{c: c, ctx: ctx}
	// Read up to the first row (or completion) so errors surface here.
	if err := r.advance(); err != nil {
		return nil, err
	}
	return r, nil
}

type column struct {
	name string
	oid  uint32
}

type rows struct {
	c        *conn
	ctx      context.Context
	cols     []column
	pending  [][]byte // the current row, already read
	hasRow   bool
	done     bool
	affected int64
	err      error
}

// advance reads messages until a DataRow, or the end of the query.
func (r *rows) advance() error {
	for {
		t, body, err := r.c.r.next()
		if err != nil {
			r.c.broken = true
			return err
		}
		switch t {
		case '1', '2', 'n', 's': // ParseComplete, BindComplete, NoData, PortalSuspended
		case 'T':
			n := int(binary.BigEndian.Uint16(body))
			body = body[2:]
			r.cols = make([]column, n)
			for i := 0; i < n; i++ {
				name, rest := cstr(body)
				r.cols[i] = column{name: name, oid: binary.BigEndian.Uint32(rest[6:10])}
				body = rest[18:]
			}
		case 'D':
			n := int(binary.BigEndian.Uint16(body))
			body = body[2:]
			row := make([][]byte, n)
			for i := 0; i < n; i++ {
				l := int32(binary.BigEndian.Uint32(body))
				body = body[4:]
				if l < 0 {
					continue
				}
				row[i] = body[:l]
				body = body[l:]
			}
			r.pending, r.hasRow = row, true
			return nil
		case 'C':
			tag, _ := cstr(body)
			if f := strings.Fields(tag); len(f) > 0 {
				r.affected, _ = strconv.ParseInt(f[len(f)-1], 10, 64)
			}
		case 'I': // EmptyQueryResponse
		case 'E':
			r.err = parseError(body)
		case 'N', 'S', 'A': // notices, parameter changes, notifications
		case 'Z':
			r.done = true
			if r.err != nil {
				return r.err
			}
			return nil
		default:
			r.c.broken = true
			return fmt.Errorf("pgwire: unexpected message %q", t)
		}
	}
}

func (r *rows) Columns() []string {
	out := make([]string, len(r.cols))
	for i, c := range r.cols {
		out[i] = c.name
	}
	return out
}

func (r *rows) Close() error {
	for !r.done {
		r.hasRow = false
		if err := r.advance(); err != nil && r.c.broken {
			return err
		}
	}
	return nil
}

func (r *rows) Next(dest []driver.Value) error {
	if !r.hasRow {
		if r.done {
			return io.EOF
		}
		if err := r.advance(); err != nil {
			return err
		}
		if !r.hasRow {
			return io.EOF
		}
	}
	r.hasRow = false
	for i, raw := range r.pending {
		if raw == nil {
			dest[i] = nil
			continue
		}
		v, err := decode(r.cols[i].oid, raw)
		if err != nil {
			return err
		}
		dest[i] = v
	}
	return nil
}

// ---- values ----

func encode(v any) ([]byte, bool, error) {
	switch x := v.(type) {
	case nil:
		return nil, true, nil
	case int64:
		return []byte(strconv.FormatInt(x, 10)), false, nil
	case float64:
		return []byte(strconv.FormatFloat(x, 'g', -1, 64)), false, nil
	case bool:
		if x {
			return []byte("t"), false, nil
		}
		return []byte("f"), false, nil
	case string:
		return []byte(x), false, nil
	case []byte:
		return []byte(`\x` + hex.EncodeToString(x)), false, nil
	case time.Time:
		return []byte(x.UTC().Format("2006-01-02 15:04:05.999999Z07:00")), false, nil
	}
	return nil, false, fmt.Errorf("pgwire: can't send %T", v)
}

const (
	oidBool        = 16
	oidBytea       = 17
	oidInt8        = 20
	oidInt2        = 21
	oidInt4        = 23
	oidOID         = 26
	oidFloat4      = 700
	oidFloat8      = 701
	oidTimestamp   = 1114
	oidTimestamptz = 1184
	oidDate        = 1082
	oidNumeric     = 1700
)

func decode(oid uint32, b []byte) (driver.Value, error) {
	s := string(b)
	switch oid {
	case oidBool:
		return s == "t", nil
	case oidInt2, oidInt4, oidInt8, oidOID:
		return strconv.ParseInt(s, 10, 64)
	case oidFloat4, oidFloat8:
		return strconv.ParseFloat(s, 64)
	case oidBytea:
		if strings.HasPrefix(s, `\x`) {
			return hex.DecodeString(s[2:])
		}
		return b, nil
	case oidTimestamptz:
		return parseTime(s, true)
	case oidTimestamp:
		return parseTime(s, false)
	case oidDate:
		return time.Parse("2006-01-02", s)
	}
	return s, nil // text, varchar, numeric, json, …
}

func parseTime(s string, tz bool) (time.Time, error) {
	if !tz {
		return time.Parse("2006-01-02 15:04:05.999999999", s)
	}
	// "2026-11-12 09:00:00.123+00" or "+05:30"; Go wants a full offset.
	if i := strings.LastIndexAny(s, "+-"); i > 10 && len(s)-i == 3 {
		s += ":00"
	}
	return time.Parse("2006-01-02 15:04:05.999999999-07:00", s)
}

// ---- messages ----

type msg struct{ b []byte }

func (m *msg) int32(v int32) { m.b = binary.BigEndian.AppendUint32(m.b, uint32(v)) }
func (m *msg) int16(v int16) { m.b = binary.BigEndian.AppendUint16(m.b, uint16(v)) }
func (m *msg) str(s string)  { m.b = append(append(m.b, s...), 0) }

func (m *msg) typed(t byte) []byte {
	out := []byte{t}
	out = binary.BigEndian.AppendUint32(out, uint32(len(m.b)+4))
	return append(out, m.b...)
}

func (m *msg) untyped() []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(m.b)+4))
	return append(out, m.b...)
}

type reader struct {
	r   io.Reader
	hdr [5]byte
}

const maxMessage = 64 << 20

func (r *reader) next() (byte, []byte, error) {
	if _, err := io.ReadFull(r.r, r.hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(r.hdr[1:])
	if n < 4 || n > maxMessage {
		return 0, nil, fmt.Errorf("pgwire: bad message length %d", n)
	}
	body := make([]byte, n-4)
	if _, err := io.ReadFull(r.r, body); err != nil {
		return 0, nil, err
	}
	return r.hdr[0], body, nil
}

func cstr(b []byte) (string, []byte) {
	for i, c := range b {
		if c == 0 {
			return string(b[:i]), b[i+1:]
		}
	}
	return string(b), nil
}

func parseError(body []byte) error {
	e := &Error{}
	for len(body) > 0 && body[0] != 0 {
		f := body[0]
		var v string
		v, body = cstr(body[1:])
		switch f {
		case 'S':
			e.Severity = v
		case 'C':
			e.Code = v
		case 'M':
			e.Message = v
		case 'D':
			e.Detail = v
		case 'n':
			e.Constraint = v
		}
	}
	return e
}

// ---- SCRAM-SHA-256 (RFC 5802, 7677), without channel binding ----

type scramClient struct {
	user, password  string
	nonce           string
	clientFirstBare string
	serverSig       []byte
}

func newScram(user, password string) *scramClient {
	b := make([]byte, 18)
	rand.Read(b)
	return &scramClient{user: user, password: password, nonce: base64.RawStdEncoding.EncodeToString(b)}
}

func (s *scramClient) clientFirst() []byte {
	// The user name is sent in the startup message; SCRAM's is left empty.
	s.clientFirstBare = "n=,r=" + s.nonce
	return []byte("n,," + s.clientFirstBare)
}

func (s *scramClient) clientFinal(serverFirst []byte) ([]byte, error) {
	var nonce, salt64 string
	iter := 0
	for _, part := range strings.Split(string(serverFirst), ",") {
		switch {
		case strings.HasPrefix(part, "r="):
			nonce = part[2:]
		case strings.HasPrefix(part, "s="):
			salt64 = part[2:]
		case strings.HasPrefix(part, "i="):
			iter, _ = strconv.Atoi(part[2:])
		}
	}
	if !strings.HasPrefix(nonce, s.nonce) || len(nonce) == len(s.nonce) || iter < 1 || iter > 1_000_000 {
		return nil, errors.New("pgwire: SCRAM: bad server-first message")
	}
	salt, err := base64.StdEncoding.DecodeString(salt64)
	if err != nil {
		return nil, errors.New("pgwire: SCRAM: bad salt")
	}
	salted, err := pbkdf2.Key(sha256.New, s.password, salt, iter, 32)
	if err != nil {
		return nil, err
	}
	clientKey := hmacSHA(salted, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	finalNoProof := "c=biws,r=" + nonce
	authMsg := s.clientFirstBare + "," + string(serverFirst) + "," + finalNoProof
	sig := hmacSHA(storedKey[:], authMsg)
	proof := make([]byte, len(clientKey))
	for i := range clientKey {
		proof[i] = clientKey[i] ^ sig[i]
	}
	serverKey := hmacSHA(salted, "Server Key")
	s.serverSig = hmacSHA(serverKey, authMsg)
	return []byte(finalNoProof + ",p=" + base64.StdEncoding.EncodeToString(proof)), nil
}

func (s *scramClient) verifyServer(serverFinal []byte) error {
	v, ok := strings.CutPrefix(string(serverFinal), "v=")
	if !ok {
		return errors.New("pgwire: SCRAM: bad server-final message")
	}
	got, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
	if err != nil || !hmac.Equal(got, s.serverSig) {
		return errors.New("pgwire: SCRAM: the server's signature doesn't match (wrong server?)")
	}
	return nil
}

func hmacSHA(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}
