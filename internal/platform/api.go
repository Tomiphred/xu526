package platform

import (
	_ "embed"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
)

//go:embed ui/index.html
var dashboardHTML []byte

//go:embed ui/logo.png
var logoPNG []byte

//go:embed ui/mark.png
var markPNG []byte

// API returns the control-plane HTTP API and the dashboard.
//
//	GET    /                       dashboard
//	GET    /v1/apps                list apps
//	PUT    /v1/apps/{name}         deploy or update (body: AppSpec)
//	GET    /v1/apps/{name}         app status
//	DELETE /v1/apps/{name}         delete
//	GET    /v1/apps/{name}/logs    recent output (?tail=200)
//	GET    /v1/events              activity feed (?since=<seq>)
//	GET    /v1/node                server resources
//	POST   /v1/images?ref=&id=…    load an image tarball from `xu build`
//
// allowedHosts lists extra Host names (besides loopback) the API answers to.
func (p *Platform) API(allowedHosts ...string) http.Handler {
	mux := http.NewServeMux()
	for path, img := range map[string][]byte{"/logo.png": logoPNG, "/favicon.png": markPNG} {
		img := img
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.Write(img)
		})
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
		_, _ = w.Write(dashboardHTML)
	})
	mux.HandleFunc("GET /v1/apps", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, p.List())
	})
	mux.HandleFunc("GET /v1/apps/{name}", func(w http.ResponseWriter, r *http.Request) {
		s, ok := p.Status(r.PathValue("name"))
		if !ok {
			writeErr(w, http.StatusNotFound, "no app "+r.PathValue("name"))
			return
		}
		writeJSON(w, http.StatusOK, s)
	})
	mux.HandleFunc("PUT /v1/apps/{name}", func(w http.ResponseWriter, r *http.Request) {
		var spec AppSpec
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&spec); err != nil {
			writeErr(w, http.StatusBadRequest, "bad spec: "+err.Error())
			return
		}
		spec.Name = r.PathValue("name")
		if err := p.Deploy(r.Context(), spec); err != nil {
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		s, _ := p.Status(spec.Name)
		writeJSON(w, http.StatusOK, s)
	})
	mux.HandleFunc("DELETE /v1/apps/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := p.Delete(r.PathValue("name")); err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	// ---- stacks (compose files) ----
	type stackReq struct {
		Name        string            `json:"name"`
		YAML        string            `json:"yaml"`
		Environment string            `json:"environment"`     // "" = production
		Built       map[string]string `json:"built,omitempty"` // service → uploaded image (xu stack deploy)
		Files       map[string]string `json:"files,omitempty"` // .env and env_file contents from the stack's folder
	}
	readStack := func(w http.ResponseWriter, r *http.Request) (stackReq, bool) {
		var req stackReq
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "bad request: "+err.Error())
			return req, false
		}
		return req, true
	}
	mux.HandleFunc("POST /v1/stacks/plan", func(w http.ResponseWriter, r *http.Request) {
		if req, ok := readStack(w, r); ok {
			t := stackTarget{Stack: req.Name, Env: envOrDefault(req.Environment), Files: req.Files}
			writeJSON(w, http.StatusOK, p.planStack(t, []byte(req.YAML), p.builtFor(t.key(), req.Built)))
		}
	})
	mux.HandleFunc("GET /v1/stacks", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, p.Stacks())
	})
	mux.HandleFunc("GET /v1/stacks/{name}", func(w http.ResponseWriter, r *http.Request) {
		s, ok := p.Stack(r.PathValue("name"))
		if !ok {
			writeErr(w, http.StatusNotFound, "no stack "+r.PathValue("name"))
			return
		}
		writeJSON(w, http.StatusOK, s)
	})
	// PUT /v1/stacks/{name} deploys the stack called name into the body's
	// environment; it's stored as name (production) or name-environment.
	mux.HandleFunc("PUT /v1/stacks/{name}", func(w http.ResponseWriter, r *http.Request) {
		req, ok := readStack(w, r)
		if !ok {
			return
		}
		plan, err := p.DeployStackIn(r.Context(), r.PathValue("name"), req.Environment, []byte(req.YAML), req.Built, req.Files)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error(), "plan": plan})
			return
		}
		writeJSON(w, http.StatusOK, plan)
	})
	mux.HandleFunc("DELETE /v1/stacks/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := p.DeleteStack(r.Context(), r.PathValue("name")); err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
		writeJSON(w, http.StatusOK, p.Events(since))
	})
	mux.HandleFunc("GET /v1/node", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, p.Node())
	})
	p.environmentAPI(mux)
	p.resourceAPI(mux)
	p.folderAPI(mux)
	p.localAPI(mux)
	p.storageAPI(mux)
	p.netsecAPI(mux)
	p.opsRoutes(mux)
	p.imageAPI(mux)
	p.housekeepingAPI(mux)
	return hostGuard(p.withGitHub(mux), allowedHosts)
}

// hostGuard blocks DNS-rebinding: a hostile web page can point its own domain
// at 127.0.0.1, but it can't change the Host header the browser sends.
func hostGuard(next http.Handler, extra []string) http.Handler {
	allowed := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
	for _, h := range extra {
		if h = strings.TrimSpace(strings.ToLower(h)); h != "" {
			allowed[h] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if !allowed[strings.ToLower(strings.Trim(host, "[]"))] {
			http.Error(w, "host not allowed: start the daemon with -api-hosts "+host, http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// decodeJSON reads a small JSON body, answering 400 itself on failure.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request: "+err.Error())
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
