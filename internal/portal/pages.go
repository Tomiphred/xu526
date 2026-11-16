package portal

import (
	"bytes"
	"html/template"
	"net/http"
)

type pages struct{ t map[string]*template.Template }

const layout = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="same-origin">
<title>{{block "title" .}}{{.Site}}{{end}}</title>
<link rel="stylesheet" href="/style.css">
</head>
<body class="{{block "class" .}}narrow{{end}}">
<header class="bar">
  <a class="site" href="/">{{.Site}}</a>
  {{block "bar" .}}{{end}}
</header>
<main>
{{if .Error}}<p class="alert" role="alert">{{.Error}}</p>{{end}}
{{if .Notice}}<p class="notice" role="status">{{.Notice}}</p>{{end}}
{{block "content" .}}{{end}}
</main>
</body>
</html>`

var pageText = map[string]string{
	"signin": `{{define "title"}}Sign in · {{.Site}}{{end}}
{{define "content"}}
<h1>Sign in</h1>
<p class="lede">Get a token to put apps running on your own computer on the internet.</p>
<form method="post" action="/signin">
  <input type="hidden" name="csrf" value="{{.CSRF}}">
  <label>Email <input type="email" name="email" value="{{.Email}}" autocomplete="email" required autofocus></label>
  <label>Password <input type="password" name="password" autocomplete="current-password" required></label>
  <button>Sign in</button>
</form>
<p class="aside">New here? <a href="/signup">Create an account</a>. <a href="/forgot">Forgot your password?</a></p>
{{end}}`,

	"signup": `{{define "title"}}Create an account · {{.Site}}{{end}}
{{define "content"}}
<h1>Create an account</h1>
<p class="lede">We'll email you a code to confirm your address. After that, you can get your tunnel token.</p>
<form method="post" action="/signup">
  <input type="hidden" name="csrf" value="{{.CSRF}}">
  <label>Email <input type="email" name="email" value="{{.Email}}" autocomplete="email" required autofocus></label>
  <label>Password <input type="password" name="password" autocomplete="new-password" minlength="10" maxlength="128" required>
    <small>At least 10 characters.</small></label>
  <label>Password again <input type="password" name="confirm" autocomplete="new-password" required></label>
  <button>Create account</button>
</form>
<p class="aside">Already have an account? <a href="/">Sign in</a>.</p>
{{end}}`,

	"verify": `{{define "title"}}Check your email · {{.Site}}{{end}}
{{define "content"}}
<h1>Check your email</h1>
<p class="lede">We sent you a 6-digit code. Enter it here to continue. It expires in 10 minutes.</p>
<form method="post" action="/verify">
  <input type="hidden" name="csrf" value="{{.CSRF}}">
  <label>Code <input class="code" name="code" inputmode="numeric" autocomplete="one-time-code" pattern="[0-9 ]{6,7}" maxlength="7" required autofocus></label>
  <button>Continue</button>
</form>
<form method="post" action="/resend" class="inline">
  <input type="hidden" name="csrf" value="{{.CSRF}}">
  <p class="aside">No email after a minute? Check spam, or <button class="link">send a new code</button>. <a href="/">Start over</a>.</p>
</form>
{{end}}`,

	"forgot": `{{define "title"}}Reset your password · {{.Site}}{{end}}
{{define "content"}}
<h1>Reset your password</h1>
<p class="lede">Enter your email. If it has an account, we'll send a code to set a new password.</p>
<form method="post" action="/forgot">
  <input type="hidden" name="csrf" value="{{.CSRF}}">
  <label>Email <input type="email" name="email" value="{{.Email}}" autocomplete="email" required autofocus></label>
  <button>Send code</button>
</form>
<p class="aside"><a href="/">Back to sign in</a></p>
{{end}}`,

	"reset": `{{define "title"}}Choose a new password · {{.Site}}{{end}}
{{define "content"}}
<h1>Choose a new password</h1>
<p class="lede">If that email has an account, we sent it a 6-digit code. Saving a new password signs you out everywhere else.</p>
<form method="post" action="/reset">
  <input type="hidden" name="csrf" value="{{.CSRF}}">
  <label>Code <input class="code" name="code" inputmode="numeric" autocomplete="one-time-code" pattern="[0-9 ]{6,7}" maxlength="7" required autofocus></label>
  <label>New password <input type="password" name="password" autocomplete="new-password" minlength="10" maxlength="128" required>
    <small>At least 10 characters.</small></label>
  <label>New password again <input type="password" name="confirm" autocomplete="new-password" required></label>
  <button>Save password</button>
</form>
<form method="post" action="/resend" class="inline">
  <input type="hidden" name="csrf" value="{{.CSRF}}">
  <p class="aside"><button class="link">Send a new code</button> · <a href="/">Back to sign in</a></p>
</form>
{{end}}`,

	"dashboard": `{{define "title"}}Your tunnel · {{.Site}}{{end}}
{{define "class"}}wide{{end}}
{{define "bar"}}
<form method="post" action="/signout" class="who">
  <input type="hidden" name="csrf" value="{{.CSRF}}">
  <span>{{.D.Email}}</span> <button class="link">Sign out</button>
</form>
{{end}}
{{define "content"}}{{with .D}}
{{if .Token}}
<section class="ticket" aria-labelledby="t">
  <h2 id="t">Your token</h2>
  <p>{{if .MailError}}We couldn't email it to you, so copy it now: this page is the only place it's shown.{{else}}Copy it now: this is the only time it's shown here. We also emailed it to you.{{end}}</p>
  <pre class="token">{{.Token}}</pre>
  <p>Connect your computer with:</p>
  <pre class="cmd">xu tunnel connect {{.Connect}} {{.Token}}</pre>
</section>
{{end}}

{{if .HasToken}}
<h1>Your tunnel</h1>
<dl class="facts">
  <div><dt>Name</dt><dd><code>{{.Name}}</code></dd></div>
  <div><dt>Computer</dt><dd>{{if .Online}}<span class="on">Connected</span>{{else}}Not connected{{end}}</dd></div>
  <div><dt>Public addresses</dt><dd>{{.Used}} of {{.Max}} in use</dd></div>
</dl>
{{if .Apps}}<ul class="apps">{{range .Apps}}<li><a href="{{index . 1}}" rel="noopener">{{index . 1}}</a> <span>{{index . 0}}</span></li>{{end}}</ul>{{end}}

<h2>Use it</h2>
<p>On your computer, connect once, then share an app. Each shared app gets an address like
<code>https://APP-{{.Name}}.{{.Domain}}</code>.</p>
<pre class="cmd">xu tunnel connect {{.Connect}} YOUR_TOKEN
xu share APP</pre>

<h2>Lost your token, or think it leaked?</h2>
<form method="post" action="/token">
  <input type="hidden" name="csrf" value="{{$.CSRF}}">
  <p>A new token replaces the old one at once: your computer disconnects until you run <code>xu tunnel connect</code> with the new token. Your name and addresses stay the same.</p>
  <button class="quiet">Regenerate token</button>
</form>
{{else}}
<h1>Get your tunnel token</h1>
{{if .Removed}}<p class="notice">The server's admin removed your previous token. You can get a new one.</p>{{end}}
<p class="lede">Your token lets one computer put up to {{.Max}} apps online. Pick the name that goes in their addresses.</p>
<form method="post" action="/token">
  <input type="hidden" name="csrf" value="{{$.CSRF}}">
  <label>Name
    <span class="affix"><span>APP-</span><input name="name" value="{{if .Name}}{{.Name}}{{else}}{{.Suggested}}{{end}}" pattern="[a-z0-9][a-z0-9-]{1,18}[a-z0-9]" maxlength="20" autocapitalize="none" spellcheck="false" required><span>.{{.Domain}}</span></span>
    <small>3 to 20 lowercase letters, digits and dashes. You can't change it later.</small></label>
  <button>Get token</button>
</form>
{{end}}
{{end}}{{end}}`,

	"pair": `{{define "title"}}Connect your computer · {{.Site}}{{end}}
{{define "content"}}
{{if .Device}}
<h1>Connect {{.Device}}?</h1>
<p class="lede">A computer asked to connect to {{.Site}} {{.Ago}}, from {{.IP}}. Approve only if you started this yourself on your xu526 dashboard.</p>
<p class="pair-label">Check that your dashboard shows this code:</p>
<p class="paircode">{{.Code}}</p>
<form method="post" action="/pair">
  <input type="hidden" name="csrf" value="{{.CSRF}}">
  <input type="hidden" name="code" value="{{.Code}}">
  {{if .Name}}
  <p>It will use your tunnel <code>{{.Name}}</code>.{{if .Replaces}} Your other computer that's connected now will be disconnected: one computer uses the tunnel at a time.{{end}}</p>
  {{else}}
  <label>Name in your addresses
    <span class="affix"><span>APP-</span><input name="name" value="{{.Suggested}}" pattern="[a-z0-9][a-z0-9-]{1,18}[a-z0-9]" maxlength="20" autocapitalize="none" spellcheck="false" required><span>.{{.Domain}}</span></span>
    <small>3 to 20 lowercase letters, digits and dashes. You can't change it later.</small></label>
  {{end}}
  <div class="buttons"><button name="action" value="approve">Approve and connect</button>
  <button name="action" value="deny" class="quiet" formnovalidate>Deny</button></div>
</form>
{{else}}
<h1>Connect your computer</h1>
<p class="lede">Enter the code your xu526 dashboard shows. On the dashboard, open Settings and choose Connect with {{.Site}}.</p>
<form method="post" action="/pair">
  <input type="hidden" name="csrf" value="{{.CSRF}}">
  <input type="hidden" name="lookup" value="1">
  <label>Code <input class="code pair" name="code" value="{{.Code}}" autocomplete="off" autocapitalize="characters" spellcheck="false" maxlength="9" required autofocus></label>
  <button>Continue</button>
</form>
{{end}}
<p class="aside"><a href="/">Go to your tunnel</a></p>
{{end}}`,

	"paired": `{{define "title"}}{{if .Denied}}Request denied{{else}}Computer connected{{end}} · {{.Site}}{{end}}
{{define "content"}}
{{if .Denied}}
<h1>Request denied</h1>
<p class="lede">{{.Device}} won't be connected. If you didn't start this, nothing else is needed: your account and token haven't changed.</p>
{{else}}
<h1>You're connected</h1>
<p class="lede">{{.Device}} is receiving its token and will connect by itself in a few seconds. Go back to the xu526 dashboard on that computer: you can now share apps on the internet.</p>
<p>We've also emailed you the token, in case you ever need to connect a computer by hand.</p>
{{end}}
<p class="aside"><a href="/">Go to your tunnel</a></p>
{{end}}`,

	"notfound": `{{define "title"}}Not found · {{.Site}}{{end}}
{{define "content"}}<h1>There's nothing here</h1><p><a href="/">Go to the start page</a></p>{{end}}`,

	"error": `{{define "title"}}Error · {{.Site}}{{end}}
{{define "content"}}<h1>That didn't work</h1><p>{{.Message}}</p><p><a href="/">Go to the start page</a></p>{{end}}`,
}

func newPages() *pages {
	p := &pages{t: map[string]*template.Template{}}
	for name, text := range pageText {
		p.t[name] = template.Must(template.Must(template.New("layout").Parse(layout)).Parse(text))
	}
	return p
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["Site"] = s.cfg.SiteName
	data["CSRF"] = s.csrf(w, r)
	for _, k := range []string{"Error", "Notice", "Email", "Message", "Code", "Device", "Name", "Suggested", "Domain", "IP", "Ago"} {
		if _, ok := data[k]; !ok {
			data[k] = ""
		}
	}
	var b bytes.Buffer
	if err := s.pages.t[name].Execute(&b, data); err != nil {
		s.log.Printf("portal: page %s: %v", name, err)
		http.Error(w, "page error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(b.Bytes())
}

const styleCSS = `:root {
  --paper: #eef1ec; --ink: #15201b; --muted: #55645d; --rule: #cbd3cd; --field: #ffffff;
  --signal: #1b5e52; --signal-ink: #ffffff; --ticket: #fbf1d2; --ticket-edge: #c8952b;
  --error: #9b2c1f; --error-bg: #f8e3df; --ok: #1b5e52;
  color-scheme: light dark;
}
@media (prefers-color-scheme: dark) {
  :root {
    --paper: #111714; --ink: #e5ece7; --muted: #96a49d; --rule: #2a3530; --field: #18201c;
    --signal: #6fc2ae; --signal-ink: #0d1512; --ticket: #29230f; --ticket-edge: #b98a2e;
    --error: #f0a093; --error-bg: #3a1c17; --ok: #6fc2ae;
  }
}
* { box-sizing: border-box; }
html { -webkit-text-size-adjust: 100%; }
body { margin: 0; background: var(--paper); color: var(--ink);
  font: 16px/1.55 system-ui, -apple-system, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif; }
.bar { display: flex; align-items: center; justify-content: space-between; gap: 1rem;
  padding: 1rem 16px; border-bottom: 1px solid var(--rule); }
.site { color: var(--ink); font-weight: 650; text-decoration: none; letter-spacing: -0.01em; white-space: nowrap; flex-shrink: 0; }
.who { display: flex; gap: .6rem; align-items: center; color: var(--muted); font-size: .9rem; min-width: 0; }
.who span { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; min-width: 0; }
.who button { white-space: nowrap; flex-shrink: 0; }
main { padding: 2.5rem 16px 4rem; margin: 0 auto; }
.narrow main { max-width: 26rem; }
.wide main { max-width: 40rem; }
h1 { font-size: 1.65rem; line-height: 1.2; letter-spacing: -0.015em; margin: 0 0 .5rem; font-weight: 650; }
h2 { font-size: 1.1rem; margin: 2.2rem 0 .5rem; font-weight: 650; }
p { margin: 0 0 1rem; }
.lede { color: var(--muted); margin-bottom: 1.6rem; }
.aside { color: var(--muted); font-size: .95rem; margin-top: 1.4rem; }
a { color: var(--signal); text-underline-offset: 2px; }
label { display: block; margin-bottom: 1.1rem; font-weight: 550; }
label small { display: block; font-weight: 400; color: var(--muted); margin-top: .3rem; }
input { display: block; width: 100%; margin-top: .35rem; padding: .65rem .75rem; font: inherit; font-weight: 400;
  color: var(--ink); background: var(--field); border: 1px solid var(--rule); border-radius: 6px; }
input:focus-visible, button:focus-visible, a:focus-visible { outline: 2px solid var(--signal); outline-offset: 2px; }
input.code { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 1.5rem; letter-spacing: .35em; max-width: 12rem; }
button { font: inherit; font-weight: 600; padding: .65rem 1.1rem; border-radius: 6px; cursor: pointer;
  background: var(--signal); color: var(--signal-ink); border: 1px solid var(--signal); }
button.quiet { background: transparent; color: var(--signal); }
button.link { background: none; border: 0; padding: 0; color: var(--signal); font-weight: inherit; text-decoration: underline; text-underline-offset: 2px; }
form.inline { display: block; }
.alert { background: var(--error-bg); color: var(--error); padding: .7rem .9rem; border-radius: 6px; }
.notice { border-left: 3px solid var(--signal); padding: .2rem 0 .2rem .8rem; }
code, pre { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: .92em; }
pre.cmd { background: var(--field); border: 1px solid var(--rule); border-radius: 6px; padding: .8rem .9rem;
  overflow-x: auto; white-space: pre; margin: 0 0 1rem; }
.ticket { position: relative; background: var(--ticket); border: 1px solid var(--ticket-edge);
  border-left: 6px dashed var(--ticket-edge); border-radius: 4px 10px 10px 4px; padding: 1.2rem 1.2rem .4rem 1.4rem; margin-bottom: 2.5rem; }
.ticket h2 { margin-top: 0; }
pre.token { font-size: 1.05rem; font-weight: 600; user-select: all; -webkit-user-select: all; white-space: pre-wrap; word-break: break-all;
  background: transparent; border: 1px dashed var(--ticket-edge); border-radius: 6px; padding: .8rem; margin: 0 0 1rem; }
.ticket pre.cmd { white-space: pre-wrap; word-break: break-all; user-select: all; -webkit-user-select: all; }
.facts { display: grid; grid-template-columns: repeat(auto-fit, minmax(10rem, 1fr)); gap: 1rem; margin: 1.2rem 0; }
.facts div { border-top: 1px solid var(--rule); padding-top: .6rem; }
.facts dt { color: var(--muted); font-size: .88rem; }
.facts dd { margin: .15rem 0 0; font-weight: 600; }
.on { color: var(--ok); }
.apps { list-style: none; padding: 0; margin: 0 0 1rem; }
.apps li { display: flex; justify-content: space-between; gap: 1rem; padding: .45rem 0; border-bottom: 1px solid var(--rule); }
.apps a { overflow-wrap: anywhere; }
.apps span { color: var(--muted); }
.affix { display: flex; align-items: stretch; margin-top: .35rem; border: 1px solid var(--rule); border-radius: 6px; background: var(--field); overflow: hidden; }
.affix span { display: flex; align-items: center; padding: 0 .6rem; color: var(--muted); font-weight: 400;
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: .9rem; white-space: nowrap; }
.affix span:last-child { overflow: hidden; text-overflow: ellipsis; min-width: 0; flex-shrink: 1; }
.affix input { margin: 0; border: 0; border-radius: 0; min-width: 6rem; flex: 1 0 6rem;
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
.pair-label { margin-bottom: .4rem; color: var(--muted); }
.paircode { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 2rem; font-weight: 650; letter-spacing: .12em;
  background: var(--ticket); border: 1px dashed var(--ticket-edge); border-radius: 8px; padding: .5rem 1rem; display: inline-block; margin: 0 0 1.4rem; }
input.code.pair { letter-spacing: .2em; max-width: 14rem; text-transform: uppercase; }
.buttons { display: flex; gap: .7rem; flex-wrap: wrap; }
@media (max-width: 30rem) { h1 { font-size: 1.4rem; } .apps li { flex-direction: column; gap: 0; } }
`
