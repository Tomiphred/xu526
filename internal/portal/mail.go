package portal

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// Mailer sends plain-text email.
type Mailer interface {
	Send(ctx context.Context, to, subject, body string) error
}

// SMTP sends through an SMTP server such as Hostinger's
// (smtp.hostinger.com, port 465 with TLS, or 587 with STARTTLS). It never
// sends a password over an unencrypted connection.
type SMTP struct {
	Host     string
	Port     int
	User     string
	Password string
	From     string // the address mail comes from; Hostinger requires the mailbox you sign in with
	FromName string
	// ImplicitTLS connects with TLS from the start (port 465, "SSL");
	// otherwise STARTTLS is required (port 587).
	ImplicitTLS bool
	// TLSConfig is for tests (a server with a test certificate).
	TLSConfig *tls.Config
}

func (s *SMTP) tlsConfig() *tls.Config {
	if s.TLSConfig != nil {
		return s.TLSConfig
	}
	return &tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}
}

func (s *SMTP) Send(ctx context.Context, to, subject, body string) error {
	for _, v := range []string{to, subject, s.From, s.FromName} {
		if strings.ContainsAny(v, "\r\n") {
			return errors.New("mail: newline in a header")
		}
	}
	msg, err := s.message(to, subject, body)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	addr := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	dialer := &net.Dialer{Deadline: deadline}
	var conn net.Conn
	implicit := s.ImplicitTLS
	if implicit {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, s.tlsConfig())
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("mail: connect to %s: %w", addr, err)
	}
	conn.SetDeadline(deadline)
	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("mail: %w", err)
	}
	defer c.Close()
	if !implicit {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fmt.Errorf("mail: %s doesn't offer STARTTLS; refusing to send the password unencrypted", addr)
		}
		if err := c.StartTLS(s.tlsConfig()); err != nil {
			return fmt.Errorf("mail: STARTTLS: %w", err)
		}
	}
	if s.User != "" {
		if err := c.Auth(smtp.PlainAuth("", s.User, s.Password, s.Host)); err != nil {
			return fmt.Errorf("mail: sign-in to %s as %s: %w", s.Host, s.User, err)
		}
	}
	if err := c.Mail(s.From); err != nil {
		return fmt.Errorf("mail: MAIL FROM: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("mail: RCPT TO: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("mail: DATA: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("mail: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mail: %w", err)
	}
	return c.Quit()
}

func (s *SMTP) message(to, subject, body string) ([]byte, error) {
	id := make([]byte, 12)
	rand.Read(id)
	domain := s.From[strings.LastIndex(s.From, "@")+1:]
	from := s.From
	if s.FromName != "" {
		from = mime.QEncoding.Encode("utf-8", s.FromName) + " <" + s.From + ">"
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s@%s>\r\n", hex.EncodeToString(id), domain)
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n")
	b.WriteString("Auto-Submitted: auto-generated\r\n\r\n")
	qp := quotedprintable.NewWriter(&b)
	if _, err := qp.Write([]byte(strings.ReplaceAll(body, "\n", "\r\n"))); err != nil {
		return nil, err
	}
	qp.Close()
	return b.Bytes(), nil
}
