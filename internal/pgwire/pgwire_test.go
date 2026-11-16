package pgwire

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests need a PostgreSQL server:
//
//	XU_TEST_POSTGRES_URL=postgres://postgres:secret@127.0.0.1:5432/xu_test?sslmode=disable
//
// Optional: XU_TEST_POSTGRES_MD5_URL (a role with an MD5 password) and
// XU_TEST_POSTGRES_CA (the server's certificate, to test verify-full).
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("XU_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set XU_TEST_POSTGRES_URL to run against PostgreSQL")
	}
	db, err := sql.Open("xupg", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestParseURL(t *testing.T) {
	c, err := ParseURL("postgres://xu:p%40ss@db.internal:6543/portal?sslmode=require&connect_timeout=3")
	if err != nil || c.User != "xu" || c.Password != "p@ss" || c.Host != "db.internal" || c.Port != "6543" || c.Database != "portal" || c.SSLMode != "require" || c.ConnectTimeout != 3*time.Second {
		t.Fatalf("%+v %v", c, err)
	}
	for _, bad := range []string{"mysql://a@b/c", "postgres://host/db", "postgres://u@host/", "postgres://u@h/d?sslmode=allow"} {
		if _, err := ParseURL(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestValuesRoundTrip(t *testing.T) {
	db := testDB(t)
	now := time.Date(2026, 11, 12, 9, 30, 15, 123456000, time.UTC)
	var (
		i    int64
		f    float64
		b    bool
		s    string
		raw  []byte
		ts   time.Time
		null sql.NullString
	)
	text := `it's "quoted", 日本語, emoji 🚇, back\slash`
	err := db.QueryRow(`SELECT $1::bigint, $2::float8, $3::bool, $4::text, $5::bytea, $6::timestamptz, NULL::text`,
		int64(-42), 3.25, true, text, []byte{0, 1, 2, 255}, now).Scan(&i, &f, &b, &s, &raw, &ts, &null)
	if err != nil {
		t.Fatal(err)
	}
	if i != -42 || f != 3.25 || !b || s != text || !bytes.Equal(raw, []byte{0, 1, 2, 255}) || !ts.Equal(now) || null.Valid {
		t.Fatalf("got %d %v %v %q %v %v %v", i, f, b, s, raw, ts, null)
	}
	// SQL injection through a parameter stays data.
	var echoed string
	if err := db.QueryRow(`SELECT $1::text`, `'; DROP TABLE x; --`).Scan(&echoed); err != nil || echoed != `'; DROP TABLE x; --` {
		t.Fatal(echoed, err)
	}
}

func TestTablesTransactionsAndErrors(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	db.Exec(`DROP TABLE IF EXISTS pgwire_t`)
	if _, err := db.Exec(`CREATE TABLE pgwire_t (id BIGSERIAL PRIMARY KEY, email TEXT UNIQUE NOT NULL, at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec(`DROP TABLE IF EXISTS pgwire_t`) })
	var id int64
	if err := db.QueryRow(`INSERT INTO pgwire_t (email) VALUES ($1) RETURNING id`, "a@x.io").Scan(&id); err != nil || id != 1 {
		t.Fatal(id, err)
	}
	_, err := db.Exec(`INSERT INTO pgwire_t (email) VALUES ($1)`, "a@x.io")
	if !IsUniqueViolation(err) {
		t.Fatalf("want a unique violation, got %v", err)
	}
	// The connection still works after an error.
	res, err := db.Exec(`UPDATE pgwire_t SET email = $1 WHERE id = $2`, "b@x.io", id)
	if n, _ := res.RowsAffected(); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	// Rollback leaves no trace; commit does.
	tx, _ := db.BeginTx(ctx, nil)
	tx.Exec(`INSERT INTO pgwire_t (email) VALUES ('rolled@back')`)
	tx.Rollback()
	tx, _ = db.BeginTx(ctx, nil)
	tx.Exec(`INSERT INTO pgwire_t (email) VALUES ('kept@x.io')`)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var n int
	db.QueryRow(`SELECT count(*) FROM pgwire_t`).Scan(&n)
	if n != 2 {
		t.Fatalf("%d rows", n)
	}
	// Many rows, read fully and abandoned half-way.
	for i := 0; i < 500; i++ {
		db.Exec(`INSERT INTO pgwire_t (email) VALUES ($1)`, fmt.Sprintf("u%d@x.io", i))
	}
	rows, err := db.Query(`SELECT id, email FROM pgwire_t ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for rows.Next() {
		var id int64
		var e string
		rows.Scan(&id, &e)
		count++
		if count == 10 {
			break
		}
	}
	rows.Close()
	if err := db.QueryRow(`SELECT count(*) FROM pgwire_t`).Scan(&n); err != nil || n != 502 {
		t.Fatalf("after an abandoned result: %d %v", n, err)
	}
	// A syntax error is reported, not hung on.
	_, err = db.Exec(`SELEC 1`)
	var pe *Error
	if !errors.As(err, &pe) || pe.Code != "42601" {
		t.Fatalf("syntax error: %v", err)
	}
}

func TestConcurrentUse(t *testing.T) {
	db := testDB(t)
	db.SetMaxOpenConns(4)
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var got int64
			if err := db.QueryRow(`SELECT $1::bigint * 2`, int64(i)).Scan(&got); err != nil || got != int64(i*2) {
				errs <- fmt.Errorf("%d: %d %v", i, got, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestQueryTimeout(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := db.ExecContext(ctx, `SELECT pg_sleep(5)`); err == nil {
		t.Fatal("a query past its deadline returned no error")
	}
	// The pool replaces the broken connection.
	var one int64
	if err := db.QueryRow(`SELECT 1`).Scan(&one); err != nil || one != 1 {
		t.Fatal(one, err)
	}
}

func TestWrongPassword(t *testing.T) {
	dsn := os.Getenv("XU_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set XU_TEST_POSTGRES_URL")
	}
	u, _ := url.Parse(dsn)
	u.User = url.UserPassword(u.User.Username(), "definitely-wrong")
	db, _ := sql.Open("xupg", u.String())
	defer db.Close()
	err := db.Ping()
	var pe *Error
	if !errors.As(err, &pe) || pe.Code != "28P01" {
		t.Fatalf("want an authentication failure, got %v", err)
	}
}

func TestMD5(t *testing.T) {
	dsn := os.Getenv("XU_TEST_POSTGRES_MD5_URL")
	if dsn == "" {
		t.Skip("set XU_TEST_POSTGRES_MD5_URL")
	}
	db, _ := sql.Open("xupg", dsn)
	defer db.Close()
	var s string
	if err := db.QueryRow(`SELECT current_user`).Scan(&s); err != nil {
		t.Fatal(err)
	}
}

func TestTLS(t *testing.T) {
	dsn := os.Getenv("XU_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set XU_TEST_POSTGRES_URL")
	}
	u, _ := url.Parse(dsn)
	q := u.Query()
	q.Set("sslmode", "require")
	u.RawQuery = q.Encode()
	db, _ := sql.Open("xupg", u.String())
	defer db.Close()
	var ssl bool
	if err := db.QueryRow(`SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()`).Scan(&ssl); err != nil {
		if strings.Contains(err.Error(), "doesn't offer TLS") {
			t.Skip("server has no TLS")
		}
		t.Fatal(err)
	}
	if !ssl {
		t.Fatal("sslmode=require but the connection isn't encrypted")
	}
	// verify-full checks the certificate.
	ca := os.Getenv("XU_TEST_POSTGRES_CA")
	if ca == "" {
		return
	}
	pem, err := os.ReadFile(ca)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	cfg, _ := ParseURL(u.String())
	cfg.SSLMode = "verify-full"
	cfg.TLS = &tls.Config{RootCAs: pool, ServerName: "localhost"}
	c, err := Connect(context.Background(), cfg)
	if err != nil {
		t.Fatalf("verify-full with the right CA: %v", err)
	}
	c.Close()
	cfg.TLS = &tls.Config{RootCAs: x509.NewCertPool(), ServerName: "localhost"}
	if _, err := Connect(context.Background(), cfg); err == nil {
		t.Fatal("verify-full accepted an unknown certificate")
	}
}

func TestSeveralStatements(t *testing.T) {
	db := testDB(t)
	_, err := db.Exec(`CREATE TEMP TABLE multi_a (x int); CREATE TEMP TABLE multi_b (y int); INSERT INTO multi_a VALUES (1), (2);`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`SELECT 1; SELEC broken; SELECT 2`); err == nil {
		t.Fatal("an error in the middle of several statements was lost")
	}
	var n int64
	if err := db.QueryRow(`SELECT 41 + 1`).Scan(&n); err != nil || n != 42 {
		t.Fatal(n, err)
	}
}
