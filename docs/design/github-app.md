# GitHub App integration: design proposal

Status: **implemented** (approved with all four recommendations). Changes
made while building it are listed under "Changes since the proposal" at the end.

## Goal

Replace personal-access-token (PAT) git sources with a GitHub App, so that:

- users choose "all repositories" or "only select repositories" on GitHub's
  own install screen, and can change or revoke that at any time on GitHub
- xud never holds a long-lived credential that can read code, only an app
  private key that it uses to mint short-lived, narrowly scoped tokens
- every token mint and every clone leaves an audit record

## What changes from today

| Today (PAT sources) | Proposed (GitHub App) |
|---|---|
| A PAT stored in plaintext in `apps.json` | No repository credential stored. Installation tokens live in memory only, for at most 1 hour |
| The PAT can read every repo its owner can | Each clone uses a token scoped to **one repository**, `contents:read` only |
| Token passed to git with `-c http.extraHeader=…`, visible in `ps` | Token passed through git's environment config (`GIT_CONFIG_*`), never in argv, URLs or `.git/config` |
| A webhook created on each repo, needing a PAT with `admin:repo_hook` | One app-level webhook. No per-repo hooks, no webhook-admin permission |
| Repos identified by URL (breaks on rename or transfer) | Repos identified by GitHub's immutable numeric repository ID |
| GitHub access tied to a stored token | GitHub is optional. Signing in (web or CLI) links your GitHub account and is needed only for the GitHub endpoints; everything else works as before |

## GitHub App registration

Each self-hosted xu526 registers **its own** GitHub App. A shared,
centrally hosted app would mean one private key reading everyone's code.

**Permissions, the minimum requested:**

| Permission | Access | Why |
|---|---|---|
| Contents | Read | Clone the code; also needed to receive `push` events |
| Metadata | Read | Mandatory for every GitHub App: repository names, IDs, default branch |

**Events subscribed:** `push`. The `installation`, `installation_repositories`
and `github_app_authorization` events are always delivered to GitHub Apps
and need no permission.

**Deliberately not requested:**

- Administration or webhooks: there are no per-repo hooks, because the app webhook covers every installed repo.
- Pull requests, Issues, Actions, Packages, and anything that writes.
- Checks or Commit statuses (write) would let xu show a ✓/✗ for each deploy on the commit. That is a nice-to-have, but it's write access. If wanted later, it would be added as a separate, opt-in step with its own justification. It is not part of this work.

**App settings:**

- Callback URL: `https://<xud public host>/github/callback`
- "Request user authorization (OAuth) during installation": on
- "Expire user authorization tokens": on, which gives 8-hour user tokens plus 6-month refresh tokens
- Device flow: enabled
- Setup URL: `https://<xud public host>/github/setup`
- Webhook URL: `https://<xud public host>/github/webhook`, with a secret

**Secrets, from the environment or a secret manager and never committed:**

| Variable | Contents |
|---|---|
| `XU_GITHUB_APP_ID`, `XU_GITHUB_CLIENT_ID` | identifiers (not secret) |
| `XU_GITHUB_APP_SLUG` | for the install link |
| `XU_GITHUB_PRIVATE_KEY_FILE` (or `XU_GITHUB_PRIVATE_KEY`) | the app's RS256 PEM key |
| `XU_GITHUB_WEBHOOK_SECRET_FILE` (or `…_SECRET`) | the webhook HMAC secret |
| `XU_GITHUB_CLIENT_SECRET_FILE` (or `…_SECRET`) | for the OAuth code exchange and token refresh |
| `XU_SECRET_KEY_FILE` | a 32-byte key that encrypts refresh tokens at rest |

The `*_FILE` forms are preferred: they work with systemd `LoadCredential=`,
Docker or Kubernetes secrets, and Vault agent templates, and keep secrets out
of `/proc/<pid>/environ`. xud refuses to start if a key file is readable by
group or others. Two private keys may be configured during a key rotation: it
signs with the first and keeps the second as a fallback.

## Endpoints

On the **public** listener (only GitHub must reach this):

| Endpoint | Purpose |
|---|---|
| `POST /github/webhook` | Webhooks: signature check, de-duplication, then async handling |

On the **dashboard's own origin** (the private API). These only involve
browser redirects, so GitHub never has to reach them, and the session
cookie belongs to the dashboard:

| Endpoint | Purpose |
|---|---|
| `GET /github/login` | Starts sign-in: a `state` cookie and a PKCE challenge, then a redirect to GitHub |
| `GET /github/install` | Starts installation: sets a `state` cookie, redirects to `github.com/apps/<slug>/installations/new?state=…` |
| `GET /github/setup` | GitHub's post-install redirect (`installation_id`, `setup_action`). **Not trusted:** it only sends the user on to sign in, and the installation is confirmed from the user's token, never from this parameter |
| `GET /github/callback` | OAuth callback: checks `state` and the PKCE verifier, exchanges `code` for user tokens, creates a session, then links the installations the user's token can see |
| `GET /github/manifest` | One-click app creation, available only while no app is configured |

On the **private** API. The rest of the API is unchanged and needs no GitHub
account; only the endpoints that act with someone's GitHub identity (listing
installations, connecting or migrating a source, whoami, logout) require an
xu session:

| Endpoint | Purpose |
|---|---|
| `POST /v1/auth/device` | CLI login starts. xud asks GitHub for a device code (it holds the client ID) and returns `user_code` + `verification_uri` |
| `POST /v1/auth/device/poll` | xud polls GitHub. On success it stores the refresh token (encrypted) and returns an **xu session token**. GitHub tokens never reach the CLI |
| `POST /v1/auth/logout` | Ends the session. `--everywhere` also revokes the GitHub grant |
| `GET /v1/github/installations` | Installations and repositories this user can access (checked live with their user token) |
| `POST /v1/sources` (changed) | Connect `{installation_id, repo_id, branch, path}`, allowed only if the user's token can see that repo through that installation |

The CLI gets `xu login` (device flow), `xu logout`, `xu whoami`,
`xu github repos`, and `xu source connect <owner/repo> --app NAME`.

## Data model

Stored in xud's state, with tokens kept separate:

```go
// Installation mirrors a GitHub App installation. Written from webhooks and
// confirmed against GET /app/installations/{id}; payloads alone are not trusted.
type Installation struct {
    ID          int64             // GitHub installation ID
    AccountID   int64             // user or org that installed the app
    AccountLogin string
    AccountType string            // "User" | "Organization"
    Selection   string            // "all" | "selected"
    Repos       []RepoRef         // known repos (for "all": filled lazily from the API)
    Permissions map[string]string // as granted, e.g. {"contents":"read","metadata":"read"}
    SuspendedAt *time.Time
    CreatedAt, UpdatedAt time.Time
}

type RepoRef struct {
    ID       int64  // immutable; sources bind to this, not to the name
    FullName string // owner/name, display only, refreshed on every event
    Private  bool
}

// User is a person who signed in with GitHub.
type User struct {
    ID          string // xu user ID (random)
    GitHubID    int64  // immutable GitHub user ID; login names can change
    Login       string
    // Expiring user-to-server token: used only to check which installations
    // and repos this person can access. Never used to clone.
    RefreshToken     Sealed    // AES-256-GCM with XU_SECRET_KEY; 6-month life
    RefreshExpiresAt time.Time
    CreatedAt, LastSeenAt time.Time
    // The 8-hour access token is NOT stored: it is refreshed into memory when needed.
}

// Session is what the CLI and dashboard present to the API.
type Session struct {
    TokenHash  [32]byte  // SHA-256 of the opaque token; the token itself is never stored
    UserID     string
    Kind       string    // "cli" | "web"
    CreatedAt, ExpiresAt time.Time // 30 days for CLI, 12 hours for web
}

// Source (changed): GitHub sources point at an installation and repo ID.
type Source struct {
    // … existing fields (Kind, Target, Branch, Path, App, …)
    InstallationID int64
    RepoID         int64
    RepoFullName   string // display only
    ConnectedBy    string // xu user ID; recorded for audit
    Disabled       string // why builds are paused: "installation deleted", "repo removed", …
    // Token and per-repo webhook secret fields are gone for App sources.
}
```

**Held only in memory, never persisted:**

```go
type cachedToken struct {
    Token     string    // installation access token
    ExpiresAt time.Time // from GitHub (1 hour); evicted 5 minutes before
    Key       string    // installationID + sorted repo IDs + permissions
}
```

Installation tokens are cached per (installation, repository set,
permissions), so a token for repo A is never handed out for repo B. They are
dropped when their installation is deleted or suspended, when a repository
is removed from it, and when xud stops.

**Files:**

- `github.json` (0600): installations, users (with sealed refresh tokens), sessions (hashes only)
- `apps.json`: sources reference installation and repo IDs
- `audit.log` (0600, append-only): see below

## Flows

**Minting an installation token:**

1. Build the app JWT with RS256: `iss` = client ID, `iat` = now − 60 s (to allow for clock skew), `exp` = now + 9 min (GitHub allows at most 10).
2. `POST /app/installations/{id}/access_tokens` with `{"repository_ids":[repo], "permissions":{"contents":"read"}}`, which narrows the token to that one repository.
3. Check that the response's `permissions` and `repositories` are no wider than requested.
4. Cache the token until `expires_at` − 5 min, and write the audit record.

**Cloning:** `https://github.com/<full_name from the API>.git`, with the token passed through `GIT_CONFIG_COUNT/KEY/VALUE` as an `http.https://github.com/.extraheader`. It never appears in argv, the URL or `.git/config`. git's output is redacted before it reaches build logs. The token is never in the environment of build steps: the clone finishes before the build starts, and the build runner already passes only an allowlist of variables.

**Push webhook to build:**

1. Verify the signature.
2. Look up sources by `repository.id` and `installation.id`. Both must match the source, and the ref must match its branch.
3. Queue a build (the existing build queue already merges bursts into one).

**Installation webhooks:**

- `installation.created`, `new_permissions_accepted`: upsert the installation after confirming it with `GET /app/installations/{id}` (app JWT), and record the repo list.
- `installation.deleted`: delete the installation, disable its sources ("app uninstalled"), clear its cached tokens, write an audit record.
- `installation.suspend` / `unsuspend`: disable or re-enable its sources, clear cached tokens.
- `installation_repositories.added` / `removed`: update the repo list. Sources for removed repos are disabled and their cached tokens dropped.
- `github_app_authorization.revoked`: delete that user's refresh token and sessions.

**User tokens:**

- Refreshed on demand when the 8-hour access token has expired, using the stored refresh token and the client secret on the server.
- On refresh, GitHub rotates the refresh token, so the new one is sealed and the old one discarded.
- A failed refresh (revoked or expired) ends the user's sessions. The user signs in again with `xu login`.

## Audit log

Each event is one JSON line, appended with `O_APPEND`, mode 0600, next to the
state file. Every line includes the SHA-256 of the previous line, so editing
or deleting a line breaks the chain, and `xu audit verify` checks it.

```json
{"t":"2026-11-11T19:30:02Z","action":"token.mint","actor":"system:build","installation":51234,
 "repos":[880011],"perms":{"contents":"read"},"expires":"2026-11-11T20:30:02Z",
 "token_fp":"a3f9c2d1","source":"api","result":"ok","prev":"9b1e…"}
{"t":"2026-11-11T19:30:04Z","action":"repo.clone","actor":"system:build","installation":51234,
 "repo":880011,"repo_name":"acme/api","ref":"refs/heads/main","commit":"b0d216e3…",
 "release":"api/v7","trigger":"webhook:5b2c…","result":"ok","prev":"4c07…"}
```

**Actions logged:**

- tokens: `token.mint` (and failures), `token.evict`
- clones: `repo.clone` (and failures)
- installations: `installation.created`, `installation.deleted`, `installation.suspend`, `installation.repos_added`, `installation.repos_removed`
- sources: `source.connect`, `source.disable`
- users: `user.login`, `user.refresh`, `user.revoked`, `session.end`
- webhooks: `webhook.rejected` (with reason: bad signature, replay, too big)

`token_fp` is the first 8 hex digits of the token's SHA-256. It lets two
records be correlated without revealing the token, and **tokens are never
logged**. The audit log can also be shipped with the existing log shipper
(S3, Blob, GCS).

## Threat model

**Assets, highest value first:**

1. The app **private key**: it can mint read tokens for every repository in every installation.
2. Users' **refresh tokens**: they can act as that user, with the app's permissions only.
3. The **webhook secret**: forged events could trigger builds or rewrite installation state.
4. The **client secret**: needed for code exchange and refresh.
5. Installation tokens: one repo, read-only, at most 1 hour.
6. Source code on disk.
7. The build host.

**Who might attack, and how they reach xud:**

- anyone on the internet, through the public webhook and OAuth endpoints
- a signed-in xu user trying to reach repos they shouldn't
- a malicious repository's contents
- another local user on the server
- someone who obtains a copy of the state file or a backup

| # | Threat | Mitigation |
|---|---|---|
| T1 | **Forged or replayed webhooks** trigger builds or delete installations | HMAC-SHA256 over the raw body with `X-Hub-Signature-256`, constant-time compare, rejected if missing. Body capped at 25 MB. `X-GitHub-Delivery` IDs remembered for 72 h to drop replays. Destructive installation events re-checked against the API before acting. Rejections audited |
| T2 | **Installation-ID spoofing**: someone crafts `/github/setup?installation_id=<victim's>` to attach another account's installation | The parameter is never trusted. Installations are linked only if they appear in `GET /user/installations` for the signed-in user's token |
| T3 | **CSRF and login CSRF** on the OAuth flow | `state`: random, single use, 10-minute life, bound to an HttpOnly, SameSite=Lax cookie. PKCE (S256) on the code exchange. Session cookie: HttpOnly, Secure, SameSite=Strict |
| T4 | **Confused deputy**: a signed-in user connects a repo they can't access, just because the app happens to be installed there | At connect time, the repo must appear in `GET /user/installations/{id}/repositories` with *that user's* token. Builds then use the app's own identity. Access is rechecked daily and on every installation event; if the connecting user has lost access, the source is disabled |
| T5 | **Installation token leakage** through logs, argv, `.git/config`, build steps or crash dumps | Memory only, one repo, `contents:read`, 1 hour. Passed through git's environment config. git output redacted. Not present in the build environment (allowlist). Never written to disk. The audit log holds only a fingerprint |
| T6 | **Private key theft** | Loaded from a file or secret manager; startup refused if the file is group- or world-readable. Never written to state, logs or API responses. Two-key rotation. If the key is compromised: revoke it in the GitHub App settings, which invalidates every token minted with it within an hour |
| T7 | **State-file theft**: backups, a stolen disk | Refresh tokens are sealed with AES-256-GCM using `XU_SECRET_KEY`, which is not stored with the state. Sessions are stored as hashes. There are no installation tokens to steal. A stolen `github.json` alone is useless |
| T8 | **A malicious repository** abuses the build | A Dockerfile build runs in Docker. A host build (`-host-builds`) runs repo code on the host, which is documented as trusted repositories only. The token is gone before the build starts. The clone URL comes from the API's `full_name`, so a repo can't redirect the clone elsewhere |
| T9 | **Token sent to the wrong host** (SSRF) | Clone URLs are built only from `https://github.com/` + the API's `full_name`, never from user input. The auth header is scoped to `https://github.com/`. A GitHub Enterprise base URL is set once, by the operator |
| T10 | **Repo rename or transfer** points a source at different code | Sources bind to the repo ID plus the installation ID. A transfer to another account changes the installation, so the source is disabled until it's reconnected |
| T11 | **Webhook flooding** | Signature checked before any parsing. Per-installation rate limit. Builds already merge bursts into one. Handlers answer 202 quickly and work in the background |
| T12 | **Stolen CLI session** | A 30-day opaque token in `~/.config/xu/credentials` (0600), stored only as a hash on the server. `xu logout` and `xu logout --everywhere` revoke it, and a GitHub-side revocation ends every session |
| T13 | **Audit log tampering** | Append-only, 0600, hash-chained, checked with `xu audit verify`. Optional shipping off the box |
| T14 | **Clock skew** makes JWTs invalid, or valid too long | `iat` backdated 60 s, `exp` 9 min, so up to a minute of skew is tolerated |
| T15 | **An unexpected GitHub account is linked** | Signing in needs the private API (localhost or `-api-hosts`), and whoever reaches it already controls xud, so a session adds no power. For tighter control, the optional `XU_GITHUB_USERS` allowlist (logins or numeric IDs) limits who may link an account. A login is pinned to the first numeric ID that uses it, so a renamed-and-reclaimed login stays locked out. Refused sign-ins are audited and their grant revoked |
| T16 | **Smuggled IDs**: a request to the legacy `POST /v1/sources` carries `installation_id`/`repo_id` and skips the access check | The legacy endpoint clears every App field. Only `POST /v1/sources/github`, which checks access with the user's token first, can set them |

**Out of scope:**

- Isolating host builds; a build sandbox is a separate piece of work.
- Multi-tenant xud: the relay's users stay separate from GitHub users.
- A compromised GitHub account.

## Testing

github.com and api.github.com can't be reached from my build environment, so
I'll test against an **in-process fake GitHub**, like the fake registry used
for push and pull. It will:

- issue installation tokens
- check JWT signatures with the app's public key
- run the OAuth, device and refresh flows
- send signed webhooks
- serve git over HTTP so clones can be checked for the `extraheader`

The real check is yours: register the app and run `xu login` against github.com.

## Open questions

1. **App registration:** document manual registration, or also offer GitHub's
   "app manifest" flow, which creates the app in one click? The manifest flow
   returns the private key once. xud would write it to a 0600 file you choose
   (e.g. `/etc/xu526/github-app.pem`) and print the env lines to set, never
   storing it in state. *Recommended: both.*
2. **API authentication:** once GitHub sign-in is configured, require a
   session for the **whole** API and dashboard, not just the GitHub endpoints?
   Today it relies on being reachable only from localhost. *Recommended: yes,
   with `-api-auth=off` kept for single-user localhost setups.*
   **Decided: no.** GitHub stays optional. Sign-in is needed only to connect
   xu526 to GitHub; the rest of the API keeps its localhost-only trust.
3. **Existing PAT sources:** keep them working, marked "legacy" with a warning
   and an `xu source migrate` command, or remove them? *Recommended: keep and
   warn for now.*
4. **GitHub Enterprise Server:** make the base URLs configurable now, but
   test only against github.com's API shape? *Recommended: yes.*

## Changes since the proposal

- **GitHub is optional (decision 2 reversed):** only the GitHub endpoints need a session; there is no `-api-auth` flag and no sign-in screen.
- **Sign-in allowlist (T15)** added: optional `XU_GITHUB_USERS`, with logins pinned to numeric IDs.
- **Browser endpoints** (`/github/login`, `/install`, `/setup`, `/callback`) are served on the dashboard's origin, not the public listener. Only the webhook is public.
- **Smuggled IDs (T16)** found and closed during implementation.
- **Shared audit log:** the platform owns it and hands it to the GitHub service, so clones of legacy token sources are audited too, with a single hash chain.
- **Delivery de-duplication** is held in memory: 72 hours, or until xud restarts. A replay after a restart can at most rebuild the same commit, because destructive installation events are re-checked against the API anyway.
- **Clock skew (T14):** handled by backdating `iat`; skew is not measured from GitHub's `Date` header as first proposed.
- **Legacy sources:** tokens no longer appear in git's argv either, and git gets a minimal environment for every source.
- **Rejected-refresh behaviour:** sources connected by a user are paused at the next daily recheck if that user signs out everywhere or loses access, because nothing can then vouch for the connection.
