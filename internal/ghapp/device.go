package ghapp

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// deviceFlow is a CLI login in progress. xud talks to GitHub; the CLI only
// ever sees the user code and, at the end, an xu session token.
type deviceFlow struct {
	deviceCode string
	interval   time.Duration
	lastPoll   time.Time
	expires    time.Time
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// APIRoutes adds the sign-in and GitHub endpoints of the private API:
//
//	POST /v1/auth/device         start a CLI login (no session needed)
//	POST /v1/auth/device/poll    finish it: {"id":…} → {"status":"ok","token":…}
//	POST /v1/auth/logout         {"everywhere":bool}
//	GET  /v1/auth/whoami
//	GET  /v1/github/installations  what the signed-in user can connect
//	GET  /v1/github/repos/{id}/branches
func (s *Service) APIRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/auth/device", func(w http.ResponseWriter, r *http.Request) {
		var resp struct {
			DeviceCode      string `json:"device_code"`
			UserCode        string `json:"user_code"`
			VerificationURI string `json:"verification_uri"`
			ExpiresIn       int    `json:"expires_in"`
			Interval        int    `json:"interval"`
			Error           string `json:"error"`
			ErrorDesc       string `json:"error_description"`
		}
		if err := s.oauthPost(r.Context(), "/login/device/code", url.Values{"client_id": {s.cfg.ClientID}}, &resp); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		if resp.Error != "" {
			msg := firstNonEmpty(resp.ErrorDesc, resp.Error)
			if resp.Error == "device_flow_disabled" {
				msg = `device flow is off for this GitHub App: turn on "Enable Device Flow" in its settings`
			}
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": msg})
			return
		}
		id := randomToken(32)
		if resp.Interval < 5 {
			resp.Interval = 5
		}
		s.flowMu.Lock()
		s.devices[id] = &deviceFlow{deviceCode: resp.DeviceCode, interval: time.Duration(resp.Interval) * time.Second,
			expires: s.now().Add(time.Duration(resp.ExpiresIn) * time.Second)}
		s.flowMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "user_code": resp.UserCode, "verification_uri": resp.VerificationURI,
			"expires_in": resp.ExpiresIn, "interval": resp.Interval})
	})
	mux.HandleFunc("POST /v1/auth/device/poll", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID  string `json:"id"`
			Web bool   `json:"web"` // the dashboard: the session goes in a cookie
		}
		json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
		s.flowMu.Lock()
		f := s.devices[req.ID]
		if f == nil || s.now().After(f.expires) {
			delete(s.devices, req.ID)
			s.flowMu.Unlock()
			writeJSON(w, http.StatusGone, map[string]string{"status": "expired", "error": "this login expired; run xu login again"})
			return
		}
		if s.now().Sub(f.lastPoll) < f.interval {
			s.flowMu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"status": "pending", "interval": int(f.interval.Seconds())})
			return
		}
		f.lastPoll = s.now()
		code := f.deviceCode
		s.flowMu.Unlock()

		var tok tokenResponse
		err := s.oauthPost(r.Context(), "/login/oauth/access_token", url.Values{"client_id": {s.cfg.ClientID}, "device_code": {code},
			"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}}, &tok)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		switch tok.Error {
		case "":
		case "authorization_pending":
			writeJSON(w, http.StatusOK, map[string]any{"status": "pending", "interval": int(f.interval.Seconds())})
			return
		case "slow_down":
			s.flowMu.Lock()
			f.interval += 5 * time.Second
			s.flowMu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{"status": "pending", "interval": int(f.interval.Seconds())})
			return
		default:
			s.flowMu.Lock()
			delete(s.devices, req.ID)
			s.flowMu.Unlock()
			writeJSON(w, http.StatusForbidden, map[string]string{"status": tok.Error, "error": firstNonEmpty(tok.ErrorDescription, tok.Error)})
			return
		}
		s.flowMu.Lock()
		delete(s.devices, req.ID) // single use
		s.flowMu.Unlock()
		kind := "cli"
		if req.Web {
			kind = "web"
		}
		u, session, err := s.completeLogin(r.Context(), &tok, kind)
		if err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"status": "denied", "error": err.Error()})
			return
		}
		if req.Web {
			http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: session, Path: "/", HttpOnly: true,
				Secure: s.secureCookies(), SameSite: http.SameSiteLaxMode, MaxAge: int(s.sessionTTL(kind).Seconds())})
			writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "login": u.Login})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "token": session, "login": u.Login, "expires_in": int(cliSessionTTL.Seconds())})
	})
	mux.HandleFunc("POST /v1/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Everywhere bool `json:"everywhere"`
		}
		json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
		if err := s.Logout(r.Context(), r, req.Everywhere); err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/auth/whoami", func(w http.ResponseWriter, r *http.Request) {
		u := UserFrom(r.Context())
		if u == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"login": u.Login, "github_id": u.GitHubID, "id": u.ID})
	})
	mux.HandleFunc("GET /v1/github/installations", func(w http.ResponseWriter, r *http.Request) {
		u := UserFrom(r.Context())
		if u == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
			return
		}
		list, err := s.UserInstallations(r.Context(), u.ID)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		if list == nil {
			list = []UserInstallation{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"installations": list, "install_url": s.cfg.WebURL + "/apps/" + s.cfg.Slug + "/installations/new"})
	})
	mux.HandleFunc("GET /v1/github/repos/{id}/branches", func(w http.ResponseWriter, r *http.Request) {
		u := UserFrom(r.Context())
		if u == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
			return
		}
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad repository id"})
			return
		}
		list, err := s.Branches(r.Context(), u.ID, id)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"branches": list})
	})
}
