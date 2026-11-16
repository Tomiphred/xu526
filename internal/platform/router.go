package platform

import (
	"context"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"xu526/internal/telemetry"
	"xu526/internal/tunnel"
)

// ServeHTTP is the public edge router. It picks the app from the Host header
// (a custom domain, or <name>.<domain>), counts the request for the
// autoscaler and monitoring, waits through a cold start if needed, proxies to
// the least-busy instance, and writes an access log line.
//
// xu.<domain> is the platform's own public endpoint (GitHub webhooks).
func (p *Platform) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if r.URL.Path == tunnel.Path && p.relay != nil && (strings.EqualFold(host, "xu."+p.cfg.Domain) || net.ParseIP(host) != nil) {
		p.relayAccept(w, r) // an agent connecting its tunnel
		return
	}
	if strings.EqualFold(host, "xu."+p.cfg.Domain) {
		p.publicEndpoints().ServeHTTP(w, r)
		return
	}
	if p.portal != nil && strings.EqualFold(host, p.portalHost) {
		p.portal.ServeHTTP(w, r)
		return
	}
	if p.relay != nil && p.relay.serve(w, r, host) {
		return // a visitor for an app on someone's computer
	}

	name := p.appForHost(r.Host)
	a := p.get(name)
	if base, ok := strings.CutSuffix(name, "-console"); ok && a == nil {
		// An object store's own console page, at <store>-console.
		if s := p.get(base); s != nil && consolePortOf(s.spec) > 0 {
			if !engines[s.spec.Engine].consoleLogin || p.storeLogin(w, r, s) {
				p.serveVia(w, r, s, true)
			}
			return
		}
	}
	if a == nil || !p.isPublic(a) {
		http.Error(w, "no app is deployed at "+r.Host, http.StatusNotFound)
		return
	}
	p.serveApp(w, r, a)
}

// serveApp handles one request for an app, whether it arrived directly or
// through a tunnel.
func (p *Platform) serveApp(w http.ResponseWriter, r *http.Request, a *app) {
	if a.spec.LoginOf != "" {
		s := p.get(a.spec.LoginOf)
		if s == nil {
			http.Error(w, "the object store "+a.spec.LoginOf+" is gone", http.StatusNotFound)
			return
		}
		if !p.storeLogin(w, r, s) {
			return
		}
	}
	p.serveVia(w, r, a, false)
}

// serveVia proxies to an instance: its app port, or its console page.
func (p *Platform) serveVia(w http.ResponseWriter, r *http.Request, a *app, console bool) {
	name := a.spec.Name
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w}

	// Tracing: continue the caller's trace or start one, and hand it to the
	// app in the W3C traceparent header so its own spans join this trace.
	traceID, parentSpan := parseTraceparent(r.Header.Get("traceparent"))
	if traceID == "" {
		traceID = randHex(16)
	}
	spanID := randHex(8)
	r.Header.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")
	rec.Header().Set("X-Trace-Id", traceID)
	var instID string
	var wait time.Duration
	var cold bool

	defer func() {
		d := time.Since(start)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		p.metricsFor(name).observe(status, d)
		route := telemetry.RouteOf(r.URL.Path)
		spans := []telemetry.Span{{TraceID: traceID, SpanID: spanID, ParentID: parentSpan, Name: r.Method + " " + route,
			Service: "router", Kind: "server", Start: start.UnixNano(), Duration: int64(d), Error: status >= 500,
			Attrs: map[string]string{"http.method": r.Method, "http.target": r.URL.RequestURI(), "http.status_code": strconv.Itoa(status),
				"instance": instID, "client.ip": clientIP(r), "response.bytes": strconv.FormatInt(rec.bytes, 10)}}}
		if wait > time.Millisecond {
			what := "waiting for a free instance"
			if cold {
				what = "cold start: starting an instance"
			}
			spans = append(spans, telemetry.Span{TraceID: traceID, SpanID: randHex(8), ParentID: spanID, Name: what,
				Service: "router", Kind: "internal", Start: start.UnixNano(), Duration: int64(wait)})
		}
		p.traces.AddRequest(&telemetry.Trace{ID: traceID, App: name, Method: r.Method, Path: r.URL.Path, Route: route,
			Status: status, Instance: instID, Start: start.UnixNano(), Duration: int64(d), Wait: int64(wait),
			Error: status >= 500, Spans: spans, Client: clientIP(r), UserAgent: truncate(r.UserAgent(), 160)})
		if p.logs != nil {
			p.logs.Write(name, "router", "access", r.Method+" "+r.URL.RequestURI()+" "+strconv.Itoa(status)+" "+
				d.Round(time.Millisecond).String()+" "+strconv.FormatInt(rec.bytes, 10)+"B "+clientIP(r))
		}
	}()

	a.lastReq.Store(start.UnixNano())
	storeMax(&a.peak, a.inflight.Add(1))
	defer a.inflight.Add(-1)

	ctx, cancel := context.WithTimeout(r.Context(), p.depWait(a.spec)) // a cold start per level of depends_on
	in, err := a.acquire(ctx)
	cancel()
	wait = time.Since(start)
	if in != nil {
		instID, cold = in.ID, in.started.After(start)
	}
	if err != nil {
		http.Error(rec, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer in.inflight.Add(-1)
	proxy := in.proxy
	if console {
		proxy = in.console
	}
	if proxy == nil {
		http.Error(rec, "this service has no port to serve", http.StatusBadGateway)
		return
	}
	rec.Header().Set("X-Xu-Instance", in.ID)
	proxy.ServeHTTP(rec, r)
}

func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

func (p *Platform) appFromHost(host string) string { return p.appForHost(host) }

func storeMax(v *atomic.Int64, n int64) {
	for {
		cur := v.Load()
		if n <= cur || v.CompareAndSwap(cur, n) {
			return
		}
	}
}

// parseTraceparent reads a W3C traceparent header: 00-<trace>-<span>-<flags>.
func parseTraceparent(h string) (trace, span string) {
	parts := strings.Split(strings.TrimSpace(h), "-")
	if len(parts) != 4 || len(parts[1]) != 32 || len(parts[2]) != 16 || !isHex(parts[1]) || !isHex(parts[2]) ||
		strings.Trim(parts[1], "0") == "" {
		return "", ""
	}
	return strings.ToLower(parts[1]), strings.ToLower(parts[2])
}

func isHex(s string) bool {
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

func randHex(n int) string {
	const digits = "0123456789abcdef"
	b := make([]byte, n*2)
	for i := 0; i < len(b); i += 16 {
		v := rand.Uint64()
		for j := 0; j < 16 && i+j < len(b); j++ {
			b[i+j] = digits[v&15]
			v >>= 4
		}
	}
	return string(b)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
