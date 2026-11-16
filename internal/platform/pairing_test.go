package platform

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"xu526/internal/runtime"
)

// fakeService answers like the portal's /api/pair endpoints: pending a few
// times, then the given final answer.
func fakeService(t *testing.T, final map[string]string) *httptest.Server {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		switch r.URL.Path {
		case "/api/pair/start":
			if in["device"] == "" {
				t.Error("no device name sent")
			}
			json.NewEncoder(w).Encode(map[string]any{"device_code": "secret-device", "user_code": "KJWX-BMPQ",
				"verify_url": "https://tunnel.example.com/pair?code=KJWX-BMPQ", "interval": 2, "expires_in": 900, "site": "xu526 tunnel"})
		case "/api/pair/poll":
			if in["device_code"] != "secret-device" {
				t.Errorf("poll with %q", in["device_code"])
			}
			if polls.Add(1) < 2 {
				json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
				return
			}
			json.NewEncoder(w).Encode(final)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func pairingPlatform(t *testing.T, service string) *Platform {
	p, err := New(Config{Domain: "test", Process: runtime.NewProcess(), Tick: 100 * time.Millisecond,
		ColdStartTimeout: 10 * time.Second, Logger: log.New(io.Discard, "", 0), TunnelService: service})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func waitPair(t *testing.T, p *Platform, want string) PairStatus {
	t.Helper()
	var st PairStatus
	for i := 0; i < 100; i++ {
		if st = p.PairingStatus(); st.State == want {
			return st
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("pairing state %q, want %q (%s)", st.State, want, st.Error)
	return st
}

func TestPairingConnects(t *testing.T) {
	svc := fakeService(t, map[string]string{"status": "approved", "relay_url": "https://xu.example.com", "token": "xu_from_service", "name": "ada"})
	p := pairingPlatform(t, svc.URL)
	if p.TunnelStatus().Service != svc.URL {
		t.Fatal("the dashboard isn't told about the service")
	}
	st, err := p.StartPairing()
	if err != nil || st.State != "waiting" || st.UserCode != "KJWX-BMPQ" || st.VerifyURL == "" {
		t.Fatalf("%+v %v", st, err)
	}
	waitPair(t, p, "connected")
	p.agent.mu.Lock()
	cfg := p.agent.cfg
	p.agent.mu.Unlock()
	if cfg.URL != "https://xu.example.com" || cfg.Token != "xu_from_service" {
		t.Fatalf("tunnel config %+v", cfg)
	}
}

func TestPairingDenied(t *testing.T) {
	svc := fakeService(t, map[string]string{"status": "denied"})
	p := pairingPlatform(t, svc.URL)
	if _, err := p.StartPairing(); err != nil {
		t.Fatal(err)
	}
	if st := waitPair(t, p, "denied"); st.Error == "" {
		t.Fatal("no explanation")
	}
	if p.TunnelStatus().Configured {
		t.Fatal("a denied request must not connect")
	}
}

func TestPairingNeedsHTTPS(t *testing.T) {
	p := pairingPlatform(t, "http://tunnel.example.com")
	if _, err := p.StartPairing(); err == nil {
		t.Fatal("a plain-http service on the internet was accepted")
	}
	p = pairingPlatform(t, "")
	if _, err := p.StartPairing(); err == nil {
		t.Fatal("no service, but pairing started")
	}
}
