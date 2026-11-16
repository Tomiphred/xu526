package platform

// One-click tunnel connection: this computer asks the tunnel service for a
// short code, the person approves it on the service's website, and the
// token arrives here by itself (see internal/portal/pair.go for the other
// side). Nobody copies a token.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// PairStatus is the state of a connection request, for the dashboard.
type PairStatus struct {
	State     string    `json:"state"` // idle, waiting, connected, denied, expired, error
	UserCode  string    `json:"user_code,omitempty"`
	VerifyURL string    `json:"verify_url,omitempty"`
	Site      string    `json:"site,omitempty"`
	Expires   time.Time `json:"expires,omitempty"`
	Error     string    `json:"error,omitempty"`
}

type pairState struct {
	mu     sync.Mutex
	st     PairStatus
	cancel context.CancelFunc
}

var pairClient = &http.Client{Timeout: 20 * time.Second}

func (p *Platform) serviceURL(path string) (string, error) {
	base := strings.TrimRight(p.cfg.TunnelService, "/")
	if base == "" {
		return "", errors.New("no tunnel service is set (start the daemon with -tunnel-service https://…)")
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname()))) {
		return "", fmt.Errorf("the tunnel service %q must be an https:// address", base)
	}
	return base + path, nil
}

func isLoopbackHost(h string) bool {
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

func postJSON(ctx context.Context, u string, in, out any) (int, error) {
	b, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := pairClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return resp.StatusCode, fmt.Errorf("the tunnel service answered %s", resp.Status)
	}
	return resp.StatusCode, nil
}

// StartPairing asks the tunnel service for a code and waits, in the
// background, for the person to approve it. A request already waiting is
// replaced.
func (p *Platform) StartPairing() (PairStatus, error) {
	if p.relay != nil {
		return PairStatus{}, errors.New("this server is a relay; relays don't connect to other relays")
	}
	u, err := p.serviceURL("/api/pair/start")
	if err != nil {
		return PairStatus{}, err
	}
	device, _ := os.Hostname()
	ctx, cancel := context.WithTimeout(p.ctx, 20*time.Second)
	defer cancel()
	var start struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
		VerifyURL  string `json:"verify_url"`
		Interval   int    `json:"interval"`
		ExpiresIn  int    `json:"expires_in"`
		Site       string `json:"site"`
		Error      string `json:"error"`
	}
	code, err := postJSON(ctx, u, map[string]string{"device": device}, &start)
	if err != nil {
		return PairStatus{}, fmt.Errorf("can't reach the tunnel service: %w", err)
	}
	if code != http.StatusOK || start.DeviceCode == "" {
		if start.Error == "" {
			start.Error = fmt.Sprintf("status %d", code)
		}
		return PairStatus{}, fmt.Errorf("the tunnel service refused: %s", start.Error)
	}
	if v, err := url.Parse(start.VerifyURL); err != nil || (v.Scheme != "https" && v.Scheme != "http") {
		return PairStatus{}, errors.New("the tunnel service sent a bad link")
	}
	st := PairStatus{State: "waiting", UserCode: start.UserCode, VerifyURL: start.VerifyURL, Site: start.Site,
		Expires: time.Now().Add(time.Duration(start.ExpiresIn) * time.Second)}
	interval := time.Duration(max(start.Interval, 2)) * time.Second

	pr := &p.pair
	pr.mu.Lock()
	if pr.cancel != nil {
		pr.cancel()
	}
	wctx, wcancel := context.WithDeadline(p.ctx, st.Expires.Add(5*time.Second))
	pr.cancel, pr.st = wcancel, st
	pr.mu.Unlock()
	go p.waitForApproval(wctx, wcancel, start.DeviceCode, interval)
	return st, nil
}

func (p *Platform) setPair(f func(*PairStatus)) {
	p.pair.mu.Lock()
	f(&p.pair.st)
	p.pair.mu.Unlock()
}

func (p *Platform) waitForApproval(ctx context.Context, cancel context.CancelFunc, device string, interval time.Duration) {
	defer cancel()
	u, _ := p.serviceURL("/api/pair/poll")
	t := time.NewTimer(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			p.setPair(func(s *PairStatus) {
				if s.State == "waiting" && ctx.Err() == context.DeadlineExceeded {
					s.State, s.Error = "expired", "The code expired. Start again."
				}
			})
			return
		case <-t.C:
		}
		var r struct {
			Status   string `json:"status"`
			Token    string `json:"token"`
			RelayURL string `json:"relay_url"`
			Name     string `json:"name"`
			Error    string `json:"error"`
		}
		rctx, rcancel := context.WithTimeout(ctx, 20*time.Second)
		_, err := postJSON(rctx, u, map[string]string{"device_code": device}, &r)
		rcancel()
		switch {
		case err != nil:
			// The network may blip; keep trying until the code expires.
		case r.Status == "slow_down":
			interval += time.Second
		case r.Status == "pending":
		case r.Status == "approved":
			if err := p.SetTunnelConfig(TunnelConfig{URL: r.RelayURL, Token: r.Token}); err != nil {
				p.setPair(func(s *PairStatus) { s.State, s.Error = "error", err.Error() })
				return
			}
			p.event("", "deploy", "connected to %s as %s through the tunnel service", r.RelayURL, r.Name)
			p.setPair(func(s *PairStatus) { s.State, s.Error = "connected", "" })
			return
		case r.Status == "denied":
			p.setPair(func(s *PairStatus) { s.State, s.Error = "denied", "The request was denied on the website." })
			return
		case r.Status == "expired":
			p.setPair(func(s *PairStatus) { s.State, s.Error = "expired", "The code expired. Start again." })
			return
		default:
			msg := r.Error
			if msg == "" {
				msg = "the tunnel service answered " + r.Status
			}
			p.setPair(func(s *PairStatus) { s.State, s.Error = "error", msg })
			return
		}
		t.Reset(interval)
	}
}

// PairingStatus reports the current or last connection request.
func (p *Platform) PairingStatus() PairStatus {
	p.pair.mu.Lock()
	defer p.pair.mu.Unlock()
	st := p.pair.st
	if st.State == "" {
		st.State = "idle"
	}
	return st
}

// CancelPairing stops waiting for approval.
func (p *Platform) CancelPairing() {
	p.pair.mu.Lock()
	defer p.pair.mu.Unlock()
	if p.pair.cancel != nil {
		p.pair.cancel()
		p.pair.cancel = nil
	}
	p.pair.st = PairStatus{State: "idle"}
}
