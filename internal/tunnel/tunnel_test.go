package tunnel

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func pair(t *testing.T) (*Session, *Session) {
	a, b := net.Pipe()
	c, s := NewSession(a, true), NewSession(b, false)
	t.Cleanup(func() { c.Close(); s.Close() })
	return c, s
}

// echo serves every stream the server accepts by copying it back.
func echo(s *Session) {
	for {
		st, err := s.Accept()
		if err != nil {
			return
		}
		go func() {
			io.Copy(st, st)
			st.Close()
		}()
	}
}

func TestManyStreamsLargeTransfers(t *testing.T) {
	c, s := pair(t)
	go echo(c) // the client (agent) serves streams the server (relay) opens
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st, err := s.Open()
			if err != nil {
				t.Error(err)
				return
			}
			data := make([]byte, 600<<10+i*1000) // larger than the window
			rand.Read(data)
			go func() { st.Write(data); st.CloseWrite() }()
			got, err := io.ReadAll(st)
			if err != nil || sha256.Sum256(got) != sha256.Sum256(data) {
				t.Errorf("stream %d: %d bytes, %v", i, len(got), err)
			}
			st.Close()
		}(i)
	}
	wg.Wait()
	time.Sleep(50 * time.Millisecond)
	if n := s.NumStreams(); n != 0 {
		t.Errorf("%d streams left open", n)
	}
}

func TestSlowReaderDoesNotBlockOthers(t *testing.T) {
	c, s := pair(t)
	accepted := make(chan *Stream, 2)
	go func() {
		for {
			st, err := c.Accept()
			if err != nil {
				return
			}
			accepted <- st
		}
	}()
	slow, _ := s.Open()
	stuck := <-accepted
	go slow.Write(make([]byte, 2<<20)) // nobody reads this one yet
	fast, _ := s.Open()
	other := <-accepted
	go fast.Write([]byte("hello"))
	buf := make([]byte, 5)
	other.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(other, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("second stream blocked behind the first: %v", err)
	}
	n, _ := io.ReadFull(stuck, make([]byte, 1<<20)) // and the slow one still delivers
	if n != 1<<20 {
		t.Fatalf("slow stream read %d", n)
	}
}

func TestReadDeadlineAndSessionDeath(t *testing.T) {
	c, s := pair(t)
	go func() { st, _ := c.Accept(); _ = st }()
	st, _ := s.Open()
	st.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	_, err := st.Read(make([]byte, 1))
	if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
		t.Fatalf("want a timeout, got %v", err)
	}
	st.SetReadDeadline(time.Time{})
	go c.Close()
	if _, err := st.Read(make([]byte, 1)); err == nil {
		t.Fatal("read should fail once the session dies")
	}
	if _, err := s.Open(); err == nil {
		t.Fatal("open should fail on a dead session")
	}
}

func TestHTTPOverTunnelWithUpgradeHandshake(t *testing.T) {
	sessions := make(chan *Session, 1)
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		if s, err := Upgrade(w, r); err == nil {
			sessions <- s
		}
	}))
	defer relay.Close()

	if _, err := Dial(context.Background(), relay.URL, "wrong"); err == nil {
		t.Fatal("a bad token must be refused")
	}
	agent, err := Dial(context.Background(), relay.URL, "good")
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	rs := <-sessions
	// agent: serve HTTP on streams
	ln := &listener{s: agent}
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hi %s", r.URL.Path)
	}))
	// relay: an HTTP client whose connections are streams
	client := &http.Client{Transport: &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) { return rs.Open() }}}
	for i := 0; i < 20; i++ {
		resp, err := client.Get(fmt.Sprintf("http://tunnel/x%d", i))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !bytes.Equal(b, []byte(fmt.Sprintf("hi /x%d", i))) {
			t.Fatalf("got %q", b)
		}
	}
	if d, err := rs.Ping(time.Second); err != nil || d <= 0 {
		t.Fatalf("ping %v %v", d, err)
	}
}

type listener struct{ s *Session }

func (l *listener) Accept() (net.Conn, error) { return l.s.Accept() }
func (l *listener) Close() error              { return l.s.Close() }
func (l *listener) Addr() net.Addr            { return l.s.conn.LocalAddr() }

func TestLimiter(t *testing.T) {
	l := NewLimiter(1 << 20) // 1 MB/s, starts with a 1 MB burst
	start := time.Now()
	for i := 0; i < 20; i++ {
		l.Wait(100 << 10) // 2 MB total
	}
	if d := time.Since(start); d < 800*time.Millisecond || d > 2*time.Second {
		t.Fatalf("2 MB at 1 MB/s took %v", d)
	}
}
