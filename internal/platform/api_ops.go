package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"xu526/internal/cloud"
	"xu526/internal/telemetry"
)

// opsRoutes adds releases, rollback, sources, routing, logs, monitoring,
// registries and settings to the control API.
//
//	GET  /v1/{kind}/{name}/releases              kind = apps | stacks
//	POST /v1/{kind}/{name}/rollback              {"version": 3}
//	GET  /v1/{kind}/{name}/releases/{v}/log
//	GET  /v1/sources                             POST /v1/sources (connect)
//	PUT  /v1/sources/{id}                        {"auto_deploy": true}
//	POST /v1/sources/{id}/deploy                 POST /v1/sources/{id}/github-webhook
//	GET  /v1/sources/{id}/secret                 DELETE /v1/sources/{id}
//	GET  /v1/apps/{name}/routing                 PUT /v1/apps/{name}/routing
//	GET  /v1/apps/{name}/logs?tail=&q=           GET /v1/apps/{name}/logs/chunks[/{file}]
//	GET  /v1/apps/{name}/metrics?range=1h        GET /metrics (Prometheus)
//	GET  /v1/registries                          PUT|DELETE /v1/registries/{name}, POST …/test
//	GET  /v1/settings/logs                       PUT /v1/settings/logs, POST /v1/settings/logs/test
func (p *Platform) opsRoutes(mux *http.ServeMux) {
	kindOf := func(r *http.Request) (string, bool) {
		switch r.PathValue("kind") {
		case "apps":
			return "app", true
		case "stacks":
			return "stack", true
		}
		return "", false
	}
	mux.HandleFunc("GET /v1/{kind}/{name}/releases", func(w http.ResponseWriter, r *http.Request) {
		if k, ok := kindOf(r); ok {
			writeJSON(w, http.StatusOK, p.Releases(k, r.PathValue("name")))
		} else {
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("GET /v1/{kind}/{name}/releases/{v}/log", func(w http.ResponseWriter, r *http.Request) {
		k, ok := kindOf(r)
		v, err := strconv.Atoi(r.PathValue("v"))
		if !ok || err != nil {
			http.NotFound(w, r)
			return
		}
		b, err := p.ReleaseLog(k, r.PathValue("name"), v)
		if err != nil {
			writeErr(w, http.StatusNotFound, "this release has no build log")
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(b)
	})
	mux.HandleFunc("POST /v1/{kind}/{name}/rollback", func(w http.ResponseWriter, r *http.Request) {
		k, ok := kindOf(r)
		var req struct {
			Version int `json:"version"`
		}
		if !ok || json.NewDecoder(r.Body).Decode(&req) != nil || req.Version < 1 {
			writeErr(w, http.StatusBadRequest, `send {"version": N}`)
			return
		}
		rel, err := p.Rollback(r.Context(), k, r.PathValue("name"), req.Version)
		if err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, rel)
	})

	// ---- sources (GitHub) ----
	mux.HandleFunc("GET /v1/sources", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, p.Sources())
	})
	mux.HandleFunc("POST /v1/sources", func(w http.ResponseWriter, r *http.Request) {
		var s Source
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&s); err != nil {
			writeErr(w, http.StatusBadRequest, "bad request: "+err.Error())
			return
		}
		out, err := p.ConnectSource(s)
		if err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("PUT /v1/sources/{id}", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			AutoDeploy *bool `json:"auto_deploy"`
			HostBuild  *bool `json:"host_build"` // build without a Dockerfile (repositories you connected)
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			writeErr(w, http.StatusBadRequest, "bad request")
			return
		}
		id := r.PathValue("id")
		if p.source(id) == nil {
			writeErr(w, http.StatusNotFound, "no such source")
			return
		}
		if req.AutoDeploy != nil {
			if err := p.SetAutoDeploy(id, *req.AutoDeploy); err != nil {
				writeErr(w, http.StatusNotFound, err.Error())
				return
			}
		}
		if req.HostBuild != nil {
			p.setSource(id, func(x *Source) { x.NoHostBuild = !*req.HostBuild })
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /v1/sources/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := p.DisconnectSource(r.PathValue("id")); err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/sources/{id}/secret", func(w http.ResponseWriter, r *http.Request) {
		sec, ok := p.SourceSecret(r.PathValue("id"))
		if !ok {
			writeErr(w, http.StatusNotFound, "no such source")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"secret": sec, "url": p.webhookURL(r.PathValue("id"))})
	})
	mux.HandleFunc("POST /v1/sources/{id}/deploy", func(w http.ResponseWriter, r *http.Request) {
		s := p.source(r.PathValue("id"))
		if s == nil {
			writeErr(w, http.StatusNotFound, "no such source")
			return
		}
		m := ReleaseMeta{}
		if pc := s.Pending; pc != nil { // "New commit available" → Deploy
			subject, _, _ := strings.Cut(pc.Message, "\n")
			m = ReleaseMeta{Commit: pc.SHA, CommitMsg: subject, Author: pc.Author}
		}
		p.TriggerBuild(r.PathValue("id"), m)
		w.WriteHeader(http.StatusAccepted)
	})
	// Check the branch for a new commit now (otherwise every 30 seconds).
	mux.HandleFunc("POST /v1/sources/{id}/check", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		s, err := p.CheckSource(ctx, r.PathValue("id"))
		if err != nil && s.ID == "" {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, s)
	})
	mux.HandleFunc("POST /v1/sources/{id}/github-webhook", func(w http.ResponseWriter, r *http.Request) {
		if err := p.CreateGitHubWebhook(r.Context(), r.PathValue("id")); err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// ---- public access ----
	mux.HandleFunc("GET /v1/apps/{name}/routing", func(w http.ResponseWriter, r *http.Request) {
		st, err := p.Routing(r.Context(), r.PathValue("name"))
		if err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, st)
	})
	mux.HandleFunc("PUT /v1/apps/{name}/routing", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Public  *bool    `json:"public"`
			Domains []string `json:"domains"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			writeErr(w, http.StatusBadRequest, "bad request")
			return
		}
		if err := p.SetRoute(r.PathValue("name"), req.Public, req.Domains); err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		st, _ := p.Routing(r.Context(), r.PathValue("name"))
		writeJSON(w, http.StatusOK, st)
	})

	// ---- logs ----
	mux.HandleFunc("GET /v1/apps/{name}/logs", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if p.get(name) == nil && name != "xu" {
			writeErr(w, http.StatusNotFound, "no app "+name)
			return
		}
		tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
		if tail <= 0 || tail > 10000 {
			tail = 300
		}
		lines := p.logs.Tail(name, tail, r.URL.Query().Get("q"))
		if lines == nil {
			lines = []string{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
	})
	mux.HandleFunc("GET /v1/apps/{name}/logs/chunks", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, p.logs.Chunks(r.PathValue("name")))
	})
	mux.HandleFunc("GET /v1/apps/{name}/logs/chunks/{file}", func(w http.ResponseWriter, r *http.Request) {
		path, err := p.logs.ChunkPath(r.PathValue("name"), r.PathValue("file"))
		if err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		w.Header().Set("Content-Disposition", `attachment; filename="`+r.PathValue("name")+"-"+r.PathValue("file")+`"`)
		w.Header().Set("Content-Type", "application/gzip")
		http.ServeFile(w, r, path)
	})
	mux.HandleFunc("GET /v1/settings/logs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"storage": p.logs.Storage().Redacted(), "status": p.logs.Status()})
	})
	mux.HandleFunc("PUT /v1/settings/logs", func(w http.ResponseWriter, r *http.Request) {
		var c cloud.StorageConfig
		if json.NewDecoder(r.Body).Decode(&c) != nil {
			writeErr(w, http.StatusBadRequest, "bad request")
			return
		}
		old := p.logs.Storage()
		if strings.HasPrefix(c.SecretKey, "••") || (c.SecretKey == "" && c.Type == old.Type) {
			c.SecretKey = old.SecretKey
		}
		if strings.HasPrefix(c.AccountKey, "••") || (c.AccountKey == "" && c.Type == old.Type) {
			c.AccountKey = old.AccountKey
		}
		if c.Type != "" {
			if err := c.Validate(); err != nil {
				writeErr(w, http.StatusUnprocessableEntity, err.Error())
				return
			}
		}
		p.logs.SetStorage(c)
		_ = p.save()
		writeJSON(w, http.StatusOK, map[string]any{"storage": c.Redacted(), "status": p.logs.Status()})
	})
	mux.HandleFunc("POST /v1/settings/logs/test", func(w http.ResponseWriter, r *http.Request) {
		c := p.logs.Storage()
		if c.Type == "" {
			writeErr(w, http.StatusUnprocessableEntity, "log storage is not configured")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		key := "xu-connection-test.txt"
		if err := c.Put(ctx, key, []byte("xu can write here: "+time.Now().UTC().Format(time.RFC3339)+"\n"), "text/plain"); err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": fmt.Sprintf("Wrote %s to %s", key, c.Describe())})
	})
	mux.HandleFunc("POST /v1/settings/logs/ship", func(w http.ResponseWriter, r *http.Request) {
		if err := p.logs.ShipNow(r.Context()); err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, p.logs.Status())
	})

	// ---- monitoring ----
	mux.HandleFunc("GET /v1/apps/{name}/metrics", func(w http.ResponseWriter, r *http.Request) {
		m, err := p.Metrics(r.PathValue("name"), r.URL.Query().Get("range"))
		if err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, m)
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		p.WritePrometheus(w)
	})

	// ---- tracing and profiling ----
	mux.HandleFunc("GET /v1/apps/{name}/traces", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		minMs, _ := strconv.ParseFloat(q.Get("min_ms"), 64)
		limit, _ := strconv.Atoi(q.Get("limit"))
		writeJSON(w, http.StatusOK, p.traces.List(r.PathValue("name"), telemetry.Query{
			Sort: q.Get("sort"), Route: q.Get("route"), Search: q.Get("q"), ErrorsOnly: q.Get("errors") == "1", MinMs: minMs, Limit: limit}))
	})
	mux.HandleFunc("GET /v1/apps/{name}/endpoints", func(w http.ResponseWriter, r *http.Request) {
		m, _ := strconv.Atoi(r.URL.Query().Get("minutes"))
		if m == 0 {
			m = 60
		}
		writeJSON(w, http.StatusOK, p.traces.Endpoints(r.PathValue("name"), m))
	})
	mux.HandleFunc("GET /v1/traces/{id}", func(w http.ResponseWriter, r *http.Request) {
		t, ok := p.traces.Get(r.PathValue("id"))
		if !ok {
			writeErr(w, http.StatusNotFound, "trace not found (only recent and slow traces are kept)")
			return
		}
		writeJSON(w, http.StatusOK, t)
	})
	mux.HandleFunc("GET /v1/apps/{name}/telemetry", func(w http.ResponseWriter, r *http.Request) {
		info, ok := p.TelemetryInfo(r.PathValue("name"))
		if !ok {
			writeErr(w, http.StatusNotFound, "no app "+r.PathValue("name"))
			return
		}
		writeJSON(w, http.StatusOK, info)
	})
	mux.HandleFunc("GET /v1/apps/{name}/flame", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		d, err := time.ParseDuration(q.Get("range"))
		if err != nil || d <= 0 {
			d = 15 * time.Minute
		}
		writeJSON(w, http.StatusOK, p.profiles.Flame(r.PathValue("name"), q.Get("type"), time.Now().Add(-d), time.Now()))
	})
	// The collector also answers on the API port, for apps on this machine.
	collector := p.Collector()
	mux.Handle("POST /v1/traces", collector)
	mux.Handle("POST /ingest", collector)
	mux.Handle("POST /push.v1.PusherService/Push", collector)

	// ---- tunnels: this computer as an agent ----
	mux.HandleFunc("GET /v1/tunnel", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, p.TunnelStatus())
	})
	mux.HandleFunc("PUT /v1/tunnel", func(w http.ResponseWriter, r *http.Request) {
		var c TunnelConfig
		if json.NewDecoder(r.Body).Decode(&c) != nil {
			writeErr(w, http.StatusBadRequest, "bad request")
			return
		}
		if err := p.SetTunnelConfig(c); err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, p.TunnelStatus())
	})
	mux.HandleFunc("POST /v1/tunnel/pair", func(w http.ResponseWriter, r *http.Request) {
		st, err := p.StartPairing()
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, st)
	})
	mux.HandleFunc("GET /v1/tunnel/pair", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, p.PairingStatus())
	})
	mux.HandleFunc("DELETE /v1/tunnel/pair", func(w http.ResponseWriter, r *http.Request) {
		p.CancelPairing()
		writeJSON(w, http.StatusOK, p.PairingStatus())
	})
	mux.HandleFunc("PUT /v1/apps/{name}/tunnel", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			On bool `json:"on"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			writeErr(w, http.StatusBadRequest, "bad request")
			return
		}
		if err := p.SetTunnel(r.PathValue("name"), req.On); err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// ---- relay: users who may connect agents ----
	mux.HandleFunc("GET /v1/relay/users", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, p.RelayUsers())
	})
	mux.HandleFunc("POST /v1/relay/users", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name     string  `json:"name"`
			MaxApps  int     `json:"max_apps"`
			RateMBps float64 `json:"rate_mbps"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			writeErr(w, http.StatusBadRequest, "bad request")
			return
		}
		tok, err := p.AddRelayUser(req.Name, req.MaxApps, req.RateMBps)
		if err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"name": req.Name, "token": tok, "relay_url": p.relayAgentURL()})
	})
	mux.HandleFunc("DELETE /v1/relay/users/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := p.RemoveRelayUser(r.PathValue("name")); err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// ---- registries ----
	mux.HandleFunc("GET /v1/registries", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, p.Registries())
	})
	mux.HandleFunc("PUT /v1/registries/{name}", func(w http.ResponseWriter, r *http.Request) {
		var reg Registry
		if json.NewDecoder(r.Body).Decode(&reg) != nil {
			writeErr(w, http.StatusBadRequest, "bad request")
			return
		}
		reg.Name = r.PathValue("name")
		if err := p.PutRegistry(reg); err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, p.Registries())
	})
	mux.HandleFunc("DELETE /v1/registries/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := p.DeleteRegistry(r.PathValue("name")); err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/registries/{name}/test", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if err := p.TestRegistry(ctx, r.PathValue("name")); err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "Signed in successfully"})
	})
}
