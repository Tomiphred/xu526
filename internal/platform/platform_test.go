package platform

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"xu526/internal/runtime"
)

var helloBin string

func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "xu-test")
	helloBin = filepath.Join(dir, "hello")
	out, err := exec.Command("go", "build", "-o", helloBin, "./testdata/hello").CombinedOutput()
	if err != nil {
		log.Fatalf("build hello: %v\n%s", err, out)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func newTestPlatform(t *testing.T) (*Platform, *httptest.Server) {
	p, err := New(Config{
		Domain: "test", Process: runtime.NewProcess(), Tick: 100 * time.Millisecond,
		ColdStartTimeout: 10 * time.Second, Logger: log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(func() { srv.Close(); p.Close() })
	return p, srv
}

func get(t *testing.T, srv *httptest.Server, host, path string) (string, int) {
	req, _ := http.NewRequest("GET", srv.URL+path, nil)
	req.Host = host
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.Header.Get("X-Xu-Instance"), resp.StatusCode
}

func running(p *Platform, name string) int {
	s, _ := p.Status(name)
	return s.Running
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestDeployRouteAndUnknownHost(t *testing.T) {
	p, srv := newTestPlatform(t)
	if err := p.Deploy(context.Background(), AppSpec{Name: "hello", Command: []string{helloBin}}); err != nil {
		t.Fatal(err)
	}
	if _, code := get(t, srv, "hello.test", "/"); code != 200 {
		t.Fatalf("status %d", code)
	}
	if _, code := get(t, srv, "nope.test", "/"); code != 404 {
		t.Fatalf("unknown app: status %d, want 404", code)
	}
}

func TestBrokenDeployIsRejected(t *testing.T) {
	p, _ := newTestPlatform(t)
	err := p.Deploy(context.Background(), AppSpec{Name: "bad", Command: []string{"/bin/false"}})
	if err == nil {
		t.Fatal("deploying a crashing app should fail")
	}
	if _, ok := p.Status("bad"); ok {
		t.Fatal("broken app should not be registered")
	}
}

func TestAutoscaleUpThenToZero(t *testing.T) {
	p, srv := newTestPlatform(t)
	err := p.Deploy(context.Background(), AppSpec{
		Name: "hello", Command: []string{helloBin},
		MinScale: 0, MaxScale: 5, Target: 4, IdleTimeoutSec: 1, ScaleDownSec: 1,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 16 concurrent slow requests / target 4 => 4 instances.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := map[string]bool{}
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				id, code := get(t, srv, "hello.test", "/?work=100")
				if code == 200 {
					mu.Lock()
					seen[id] = true
					mu.Unlock()
				}
			}
		}()
	}
	waitFor(t, "scale up to 4", 10*time.Second, func() bool { return running(p, "hello") >= 4 })
	time.Sleep(500 * time.Millisecond)
	close(stop)
	wg.Wait()
	if len(seen) < 4 {
		t.Fatalf("traffic reached %d instances, want >= 4", len(seen))
	}
	if n := running(p, "hello"); n > 5 {
		t.Fatalf("exceeded max scale: %d", n)
	}

	waitFor(t, "scale to zero", 10*time.Second, func() bool { return running(p, "hello") == 0 })

	// Cold start: the next request wakes the app up.
	if _, code := get(t, srv, "hello.test", "/"); code != 200 {
		t.Fatalf("cold start request: status %d", code)
	}
}

func TestCrashedInstanceIsReplaced(t *testing.T) {
	p, srv := newTestPlatform(t)
	err := p.Deploy(context.Background(), AppSpec{Name: "hello", Command: []string{helloBin}, MinScale: 1})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := get(t, srv, "hello.test", "/")
	// kill the instance behind the platform's back, as a crash would
	a := p.get("hello")
	a.mu.Lock()
	victim := a.instances[0].Instance
	a.mu.Unlock()
	_ = a.rt.Stop(context.Background(), victim)
	waitFor(t, "replacement", 10*time.Second, func() bool {
		st, _ := p.Status("hello")
		return st.Running == 1 && st.Instances[0].ID != first
	})
}
