package platform

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"xu526/internal/telemetry"
)

// Telemetry: the collector accepts
//
//	POST /v1/traces   OpenTelemetry OTLP/HTTP, protobuf or JSON (any OTel SDK)
//	POST /ingest      the Pyroscope ingest API (Pyroscope SDKs, py-spy output)
//
// Each app authenticates with its own token, injected into its environment,
// so an app can only send data as itself.

// telemetryToken returns (creating if needed) an app's ingest token.
func (p *Platform) telemetryToken(app string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.tokens[app]
	if t == "" {
		b := make([]byte, 20)
		_, _ = rand.Read(b)
		t = hex.EncodeToString(b)
		p.tokens[app] = t
	}
	return t
}

func (p *Platform) appForToken(tok string) string {
	if tok == "" {
		return ""
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for app, t := range p.tokens {
		if subtle.ConstantTimeCompare([]byte(t), []byte(tok)) == 1 {
			return app
		}
	}
	return ""
}

// telemetryEnv is added to every instance so standard SDKs need no setup:
// OpenTelemetry reads the OTEL_* variables, Pyroscope SDKs the PYROSCOPE_* ones.
func (p *Platform) telemetryEnv(spec AppSpec, inDocker bool) map[string]string {
	if p.cfg.CollectorPort == 0 {
		return spec.Env
	}
	host := "127.0.0.1"
	if inDocker {
		host = "host.docker.internal" // mapped to the host by the runtime
	}
	endpoint := "http://" + host + ":" + strconv.Itoa(p.cfg.CollectorPort)
	tok := p.telemetryToken(spec.Name)
	env := map[string]string{
		"OTEL_SERVICE_NAME":             spec.Name,
		"OTEL_EXPORTER_OTLP_ENDPOINT":   endpoint,
		"OTEL_EXPORTER_OTLP_PROTOCOL":   "http/protobuf",
		"OTEL_EXPORTER_OTLP_HEADERS":    "x-xu-token=" + tok,
		"OTEL_TRACES_EXPORTER":          "otlp",
		"OTEL_METRICS_EXPORTER":         "none", // xu measures requests itself
		"OTEL_LOGS_EXPORTER":            "none", // stdout is already collected
		"OTEL_PROPAGATORS":              "tracecontext",
		"PYROSCOPE_SERVER_ADDRESS":      endpoint,
		"PYROSCOPE_AUTH_TOKEN":          tok,
		"PYROSCOPE_BASIC_AUTH_USERNAME": "xu",
		"PYROSCOPE_BASIC_AUTH_PASSWORD": tok,
		"PYROSCOPE_APPLICATION_NAME":    spec.Name,
		"XU_TELEMETRY_ENDPOINT":         endpoint,
		"XU_TELEMETRY_TOKEN":            tok,
	}
	for k, v := range spec.Env { // the app's own settings win
		env[k] = v
	}
	return env
}

// Collector returns the telemetry ingest handler.
func (p *Platform) Collector() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/traces", p.ingestTraces)
	accept := func(w http.ResponseWriter, r *http.Request) { // metrics/logs: accepted and dropped
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 8<<20))
		w.Header().Set("Content-Type", r.Header.Get("Content-Type"))
		w.WriteHeader(http.StatusOK)
	}
	mux.HandleFunc("POST /v1/metrics", accept)
	mux.HandleFunc("POST /v1/logs", accept)
	mux.HandleFunc("POST /ingest", p.ingestProfile)                  // Pyroscope agents before 1.0
	mux.HandleFunc("POST /push.v1.PusherService/Push", p.ingestPush) // current Pyroscope agents
	return mux
}

func tokenFrom(r *http.Request) string {
	if t := r.Header.Get("x-xu-token"); t != "" {
		return t
	}
	if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		return strings.TrimPrefix(a, "Bearer ")
	}
	if _, pw, ok := r.BasicAuth(); ok {
		return pw
	}
	return r.URL.Query().Get("token")
}

func readBody(r *http.Request, limit int64) ([]byte, error) {
	var rd io.Reader = http.MaxBytesReader(nil, r.Body, limit)
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(rd)
		if err != nil {
			return nil, err
		}
		rd = io.LimitReader(zr, limit*4)
	}
	return io.ReadAll(rd)
}

func (p *Platform) ingestTraces(w http.ResponseWriter, r *http.Request) {
	app := p.appForToken(tokenFrom(r))
	if app == "" {
		http.Error(w, "missing or unknown x-xu-token", http.StatusUnauthorized)
		return
	}
	b, err := readBody(r, 16<<20)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var spans []telemetry.Span
	isJSON := strings.HasPrefix(r.Header.Get("Content-Type"), "application/json")
	if isJSON {
		spans, err = telemetry.DecodeOTLPJSON(b)
	} else {
		spans, err = telemetry.DecodeOTLPProto(b)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p.traces.AddSpans(app, spans)
	p.spansIn.Add(uint64(len(spans)))
	if isJSON {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, "{}")
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK) // an empty ExportTraceServiceResponse
}

// ingestProfile implements Pyroscope's /ingest: name, from, until, format
// (folded, lines, pprof), sampleRate, units.
func (p *Platform) ingestProfile(w http.ResponseWriter, r *http.Request) {
	app := p.appForToken(tokenFrom(r))
	if app == "" {
		http.Error(w, "missing or unknown token", http.StatusUnauthorized)
		return
	}
	q := r.URL.Query()
	b, err := readBody(r, 32<<20)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	at := time.Now()
	if u, err := strconv.ParseInt(q.Get("until"), 10, 64); err == nil && u > 0 && u <= at.Unix()+60 {
		at = time.Unix(u, 0)
	}
	typ := "cpu"
	if name := q.Get("name"); strings.Contains(name, ".") {
		n := name
		if i := strings.IndexByte(n, '{'); i >= 0 {
			n = n[:i]
		}
		typ = n[strings.LastIndexByte(n, '.')+1:]
	}
	unit := q.Get("units")
	if unit == "" {
		unit = "samples"
	}
	switch q.Get("format") {
	case "pprof":
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			b, err = multipartFile(r.Header.Get("Content-Type"), b, "profile")
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		profs, err := telemetry.ParsePprof(b)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for _, pr := range profs {
			p.profiles.Add(app, pr.Type, pr.Unit, "pyroscope", at, pr.Stacks)
		}
	default: // folded / lines / text
		stacks, err := telemetry.ParseFolded(bytes.NewReader(b))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if rate, _ := strconv.ParseFloat(q.Get("sampleRate"), 64); rate > 0 && typ == "cpu" && unit == "samples" {
			for k, v := range stacks { // samples → nanoseconds of CPU time
				stacks[k] = int64(float64(v) * 1e9 / rate)
			}
			unit = "nanoseconds"
		}
		p.profiles.Add(app, typ, unit, "pyroscope", at, stacks)
	}
	w.WriteHeader(http.StatusOK)
}

func multipartFile(contentType string, body []byte, field string) ([]byte, error) {
	_, params, _ := strings.Cut(contentType, "boundary=")
	mr := multipart.NewReader(bytes.NewReader(body), strings.Trim(params, `"`))
	for {
		part, err := mr.NextPart()
		if err != nil {
			return nil, fmt.Errorf("multipart: no %q part", field)
		}
		if part.FormName() == field {
			return io.ReadAll(part)
		}
	}
}

// ---- pull profiling for Go apps (net/http/pprof) ----

// profileLoop takes a 10-second CPU profile and a heap profile from one
// instance of each app that sets profile_path, once a minute.
func (p *Platform) profileLoop() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	var turn atomic.Uint64
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-t.C:
		}
		p.mu.RLock()
		var apps []*app
		for _, a := range p.apps {
			if a.spec.ProfilePath != "" {
				apps = append(apps, a)
			}
		}
		p.mu.RUnlock()
		for _, a := range apps {
			a.mu.Lock()
			var addr string
			if n := len(a.instances); n > 0 {
				addr = a.instances[int(turn.Add(1))%n].Addr
			}
			a.mu.Unlock()
			if addr != "" {
				go p.scrapeProfiles(a.spec.Name, "http://"+addr+strings.TrimRight(a.spec.ProfilePath, "/"))
			}
		}
	}
}

func (p *Platform) scrapeProfiles(app, base string) {
	ctx, cancel := context.WithTimeout(p.ctx, 30*time.Second)
	defer cancel()
	for _, path := range []string{"/profile?seconds=10", "/heap"} {
		req, _ := http.NewRequestWithContext(ctx, "GET", base+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			p.setProfileErr(app, err.Error())
			return
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if resp.StatusCode != 200 {
			p.setProfileErr(app, fmt.Sprintf("%s%s answered %s", base, path, resp.Status))
			return
		}
		profs, err := telemetry.ParsePprof(b)
		if err != nil {
			p.setProfileErr(app, "not a pprof profile: "+err.Error())
			return
		}
		for _, pr := range profs {
			p.profiles.Add(app, pr.Type, pr.Unit, "pprof", time.Now(), pr.Stacks)
		}
	}
	p.setProfileErr(app, "")
}

func (p *Platform) setProfileErr(app, msg string) {
	p.mu.Lock()
	p.profileErr[app] = msg
	p.mu.Unlock()
}

// TelemetryInfo tells the dashboard how an app sends traces and profiles.
type TelemetryInfo struct {
	Endpoint     string                  `json:"endpoint"`
	Token        string                  `json:"token"`
	ProfilePath  string                  `json:"profile_path"`
	ProfileError string                  `json:"profile_error,omitempty"`
	Profiles     []telemetry.ProfileType `json:"profiles"`
	SpansIn      uint64                  `json:"spans_received"`
}

func (p *Platform) TelemetryInfo(name string) (TelemetryInfo, bool) {
	a := p.get(name)
	if a == nil {
		return TelemetryInfo{}, false
	}
	p.mu.RLock()
	perr := p.profileErr[name]
	p.mu.RUnlock()
	return TelemetryInfo{
		Endpoint: "http://<host>:" + strconv.Itoa(p.cfg.CollectorPort), Token: p.telemetryToken(name),
		ProfilePath: a.spec.ProfilePath, ProfileError: perr, Profiles: p.profiles.Types(name), SpansIn: p.spansIn.Load(),
	}, true
}

// ingestPush implements Pyroscope's v1 push API (what current Pyroscope SDKs
// use): POST /push.v1.PusherService/Push with a protobuf PushRequest:
//
//	PushRequest      { repeated RawProfileSeries series = 1; }
//	RawProfileSeries { repeated LabelPair labels = 1; repeated RawSample samples = 2; }
//	RawSample        { bytes raw_profile = 1; string ID = 2; }   // raw_profile is pprof
func (p *Platform) ingestPush(w http.ResponseWriter, r *http.Request) {
	app := p.appForToken(tokenFrom(r))
	if app == "" {
		http.Error(w, "missing or unknown token", http.StatusUnauthorized)
		return
	}
	b, err := readBody(r, 32<<20)
	if err == nil && len(b) > 2 && b[0] == 0x1f && b[1] == 0x8b {
		var zr *gzip.Reader
		if zr, err = gzip.NewReader(bytes.NewReader(b)); err == nil {
			b, err = io.ReadAll(io.LimitReader(zr, 128<<20))
		}
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	raws, err := telemetry.DecodePushRequest(b)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	for _, raw := range raws {
		profs, err := telemetry.ParsePprof(raw)
		if err != nil {
			continue
		}
		for _, pr := range profs {
			p.profiles.Add(app, pr.Type, pr.Unit, "pyroscope", time.Now(), pr.Stacks)
		}
	}
	w.Header().Set("Content-Type", r.Header.Get("Content-Type"))
	w.WriteHeader(http.StatusOK) // empty PushResponse
}
