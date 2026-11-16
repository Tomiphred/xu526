package platform

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"xu526/third_party/yaml"
)

// References in stack files: ${{ expression }}. They're resolved when a
// stack is planned or deployed, in environment values, build arguments,
// command arguments and image names.
//
//	${{ secrets.NAME }}               the environment's secret
//	${{ vars.NAME }}                  the environment's variable
//	${{ services.api.url }}           where browsers reach api: its tunnel address
//	                                  with x-xu share: true (once connected), else its
//	                                  local address; for internal services, http://api:PORT
//	${{ services.api.local_url }}     its address on this computer
//	${{ services.api.internal_url }}  http://api:PORT, inside the stack
//	${{ services.api.host }}          api (its name on the stack's network)
//	${{ services.api.port }}          the port it listens on
//	${{ databases.main.url }}         a database made in the dashboard (also host,
//	                                  port, user, password, database); caches.NAME too
//	${{ environment.name }}           staging, production, …
//	${{ stack.name }}                 the stack's name
var refRE = regexp.MustCompile(`\$\{\{\s*([^{}]*?)\s*\}\}`)

type svcRef struct {
	App    string
	Public bool
	Port   int
	Share  bool
}

type refResolver struct {
	p       *Platform
	env     string
	stack   string
	vars    map[string]string
	secrets map[string]string
	svcs    map[string]svcRef
	used    map[string]bool // services reached inside the stack, referenced since the last reset
}

// serviceRef is what other services may learn about a service.
func serviceRef(p *Platform, key, name string, cs composeService) svcRef {
	r := svcRef{App: key + "-" + appSafe(name), Public: len(cs.Ports) > 0}
	if cs.XXu.Public != nil {
		r.Public = *cs.XXu.Public
	}
	for _, n := range append(append([]yaml.Node{}, cs.Ports...), cs.Expose...) {
		if port, err := parsePort(n); err == nil {
			r.Port = port
			break
		}
	}
	if r.Port == 0 && cs.Build.IsZero() && cs.XXu.Kind != "worker" {
		r.Port = p.knownImagePort(cs.Image) // its image's EXPOSE
	}
	r.Share = cs.XXu.Share != nil && *cs.XXu.Share
	return r
}

// resolve replaces every reference in s. secret reports whether any value
// came from a secret; problems lists references that can't be resolved.
func (rr *refResolver) resolve(s string) (out string, secret bool, problems []string) {
	if !strings.Contains(s, "${{") {
		return s, false, nil
	}
	out = refRE.ReplaceAllStringFunc(s, func(m string) string {
		expr := refRE.FindStringSubmatch(m)[1]
		v, isSecret, err := rr.lookup(expr)
		if err != nil {
			problems = append(problems, err.Error())
			return ""
		}
		secret = secret || isSecret
		return v
	})
	if strings.Contains(out, "${{") {
		problems = append(problems, fmt.Sprintf("%q has an unclosed ${{", s))
	}
	return out, secret, problems
}

func (rr *refResolver) lookup(expr string) (string, bool, error) {
	parts := strings.Split(expr, ".")
	switch {
	case len(parts) == 2 && parts[0] == "secrets":
		v, ok := rr.secrets[parts[1]]
		if !ok {
			return "", false, fmt.Errorf("the %s environment has no secret %s; add it in the dashboard (Environments) or with: xu env secret set %s %s",
				rr.env, parts[1], rr.env, parts[1])
		}
		return v, true, nil
	case len(parts) == 2 && (parts[0] == "vars" || parts[0] == "variables"):
		v, ok := rr.vars[parts[1]]
		if !ok {
			return "", false, fmt.Errorf("the %s environment has no variable %s; add it in the dashboard (Environments) or with: xu env set %s %s=VALUE",
				rr.env, parts[1], rr.env, parts[1])
		}
		return v, false, nil
	case len(parts) == 2 && (parts[0] == "environment" || parts[0] == "env") && parts[1] == "name":
		return rr.env, false, nil
	case len(parts) == 2 && parts[0] == "stack" && parts[1] == "name":
		return rr.stack, false, nil
	case len(parts) == 3 && (parts[0] == "databases" || parts[0] == "caches" || parts[0] == "storage"):
		kind := map[string]string{"databases": "database", "caches": "cache", "storage": "storage"}[parts[0]]
		v, err := rr.p.resourceRef(kind, parts[1], parts[2], rr.env)
		// url and password carry the password: treated as secrets.
		return v, err == nil && (parts[2] == "password" || parts[2] == "secret_key" || (parts[2] == "url" && kind != "storage")), err
	case len(parts) == 3 && parts[0] == "services":
		s, ok := rr.svcs[parts[1]]
		// Connections are drawn for what a service reaches inside the stack;
		// public addresses are links for browsers, not connections.
		if inside := parts[2] != "url" && parts[2] != "local_url" || !s.Public; ok && inside && rr.used != nil {
			rr.used[parts[1]] = true
		}
		if !ok {
			return "", false, fmt.Errorf("${{ %s }}: there's no service %s in this file", expr, parts[1])
		}
		internal := "http://" + parts[1]
		if s.Port != 0 {
			internal += ":" + strconv.Itoa(s.Port)
		}
		switch parts[2] {
		case "url":
			if s.Share {
				if u, ok := rr.p.tunnelURLFor(s.App); ok {
					return u, false, nil
				}
			}
			if s.Public {
				return rr.p.appURL(s.App), false, nil
			}
			return internal, false, nil
		case "local_url":
			if !s.Public {
				return "", false, fmt.Errorf("${{ %s }}: %s is internal (no ports:), so it has no local address; use services.%s.internal_url", expr, parts[1], parts[1])
			}
			return rr.p.appURL(s.App), false, nil
		case "internal_url":
			return internal, false, nil
		case "host":
			return parts[1], false, nil
		case "port":
			if s.Port == 0 {
				return "", false, fmt.Errorf("${{ %s }}: %s has no port (add ports: or expose:)", expr, parts[1])
			}
			return strconv.Itoa(s.Port), false, nil
		}
	}
	return "", false, fmt.Errorf("${{ %s }} isn't something xu526 knows; use secrets.NAME, vars.NAME, services.NAME.url (or local_url, internal_url, host, port), databases.NAME.url, caches.NAME.url, environment.name or stack.name", expr)
}

// tunnelURLFor predicts the address an app gets when it's shared through
// xu526 tunnel: https://<app>-<user>.<relay domain>. It's known once this
// computer has connected to the relay at least once.
func (p *Platform) tunnelURLFor(app string) (string, bool) {
	ag := p.agent
	if ag == nil {
		return "", false
	}
	ag.mu.Lock()
	defer ag.mu.Unlock()
	if ag.cfg.URL == "" {
		return "", false
	}
	if u := ag.urls[app]; u != "" {
		return u, true
	}
	pattern := ag.pattern
	if pattern == "" && ag.user != "" { // older relays don't send the pattern: learn it from any shared app
		for a, u := range ag.urls {
			if i := strings.Index(u, "://"+a+"-"+ag.user+"."); i >= 0 {
				pattern = u[:i+3] + "{app}" + u[i+3+len(a):]
				break
			}
		}
	}
	if pattern == "" {
		return "", false
	}
	return strings.Replace(pattern, "{app}", app, 1), true
}
