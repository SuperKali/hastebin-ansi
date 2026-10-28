# Phase 2 plan: GitHub accounts, dashboard, editing, MCP tokens, admin

Base: tag v1.1.0. Working design document for the builders; decisions marked "user" are fixed.

## User decisions (fixed)
- Optional GitHub login. Anonymous use (web, curl /log, MCP, armbianmonitor) unchanged.
- Registered users manage all their pastes in a dashboard: list (search/filter/sort own pastes only, 25 per page), view, title, language, visibility link/private, per-paste expiry, delete single and bulk, download, duplicate, stats, API tokens, log out all, delete account.
- Registered pastes never expire even when anonymous EXPIRATION is set; higher limits for registered users.
- Editing: same link only within EDIT_WINDOW (600 s) after creation AND until a non-owner opens it; "edited at HH:MM" shown; /raw sends X-Paste-Edited; history kept. Otherwise Edit creates a new paste with a new link, versions linked, option to delete the previous one.
- Personal tokens for curl (Authorization: Bearer) and MCP (Claude Code, Cursor, VS Code). Full MCP OAuth for claude.ai later (design for it, don't build).
- No global listing or search across users, ever. Admin page always on; moderation is report-driven (by key/URL), never browsing anonymous pastes.

## 0. Facts from the current code
- Pastes are KV in storage.Storage (Set/Get/Size/Close + optional Opener, Writer, SpaceChecker). No Delete, no listing, no metadata. File storage names files by md5(key); expiry by mtime; FileStorage.static is an in-memory never-expire set lost on restart.
- The paste page is static index.html via the `/*` catch-all; content only via XHR to /documents/<key>. Unfurlers fetching /<key> never see content.
- Every route cuts the key at the first dot (handler/document.go, mcpserver ParseKey/keyPattern). Store keys containing a dot are unreachable from outside: used for revisions.
- MCP runs stateless with a per-request server func: anonymous and authenticated tool sets can differ.
- Fix in M0: /raw and /documents send no Cache-Control; Cloudflare caches .js/.css by extension. Send `Cache-Control: private, no-store` on all paste responses.
- CGO is off (distroless static): database must be pure Go.

## 1. Data model and storage
SQLite via modernc.org/sqlite, one file: `<storage.file_path>/hastebin.db` (other backends: `./hastebin.db`), mode 0600. Janitor only deletes 32-hex names and .upload-/.write-test- prefixes, so the db, -wal, -shm are safe.
- Writer handle MaxOpenConns=1, reader pool 4. Pragmas: WAL, synchronous=NORMAL, foreign_keys=ON, busy_timeout=5000, cache_size=-8000 (8 MiB outside memory_budget; account for it in runtime headroom).
- Migrations embedded in internal/accounts/migrations/NNNN_*.sql, tracked with PRAGMA user_version, one transaction at startup; DB newer than binary -> exit with a clear message.
- Backup: `hastebin --backup-db <out>` runs VACUUM INTO, safe while running. README: copying the .db while running is not a backup.
- Anonymous and existing pastes get no row and are unchanged.

Schema 0001_init.sql:
```
users(id INTEGER PK, github_id INTEGER UNIQUE NOT NULL, login TEXT NOT NULL, created_at, last_login_at, last_seen_at,
      disabled_at NULL, disabled_reason, default_visibility INTEGER DEFAULT 0, abuse_strikes INTEGER DEFAULT 0)
sessions(id_hash BLOB PK /*sha256 of cookie*/, user_id FK CASCADE, csrf BLOB, created_at, auth_at, last_seen_at, expires_at)
api_tokens(id INTEGER PK, user_id FK CASCADE, kind TEXT DEFAULT 'personal', name TEXT, prefix TEXT, hash BLOB UNIQUE,
      scope TEXT DEFAULT 'paste', client_id NULL, created_at, last_used_at, expires_at NULL)
pastes(key TEXT PK, user_id FK, title TEXT, language TEXT, visibility INTEGER /*0 link,1 private*/, created_at, expires_at NULL,
      size INTEGER, views INTEGER DEFAULT 0, last_opened_at NULL, seen_at NULL /*first non-owner read*/, edited_at NULL,
      edit_count INTEGER DEFAULT 0, root_key TEXT, version INTEGER DEFAULT 1, prev_key TEXT NULL REFERENCES pastes ON DELETE SET NULL)
      INDEX(user_id, created_at DESC), INDEX(user_id, last_opened_at), INDEX(root_key), INDEX(expires_at)
paste_revisions(paste_key FK CASCADE, rev INTEGER, store_key TEXT /*"<key>.r<rev>"*/, size, created_at, PK(paste_key, rev))
tombstones(key PK, reason, visibility, removed_at, admin_user_id)
delete_queue(store_key PK, enqueued_at, attempts)
ip_bans(client_key PK /*ClientKey form*/, reason, created_at, expires_at, admin_user_id)
audit_log(id PK, at, admin_user_id, admin_login, action, target, detail)  -- triggers RAISE(ABORT) on UPDATE/DELETE
```
- Revisions: old content copied to store key `<key>.r<n>`; unreachable from /raw, /documents, MCP (test asserts it).
- New optional interfaces storage.Deleter{Delete(key)} (all six backends) and storage.Keeper{Keep(key, keep bool)}. File backend: `static` becomes a `pinned` set, filled at startup from pastes + revision keys, used by expired() and the janitor. Redis/Postgres/Mongo: registered pastes written with skip_expiration. S3: README notes lifecycle rules would delete them. Memcached: refuse to start with accounts on.
- Per-paste expiry: read path denies after expires_at; accounts.Sweeper every minute moves expired rows to delete_queue and drains it (bounded batches, retries).
- Ordering: upload = write content then insert row. Delete = delete row in a transaction first, then store objects via delete_queue. Views counted in memory, flushed every 15 s; seen_at written at once.

## 2. Authentication
- GitHub OAuth App via golang.org/x/oauth2. Scope empty (id + login). State 32 random bytes + PKCE S256. Callback exactly BASE_URL + "/auth/github/callback". GitHub token used once for GET /user, never stored. Account keyed on github_id; login is display only, refreshed each login. Outbound timeout 10 s (node needs egress to github.com, api.github.com).
- OAuth state in memory (bounded 10k, TTL 10 min, single use) {pkce_verifier, return_to, reauth}, bound by cookie `__Host-hb_oauth` (HttpOnly, Secure, SameSite=Lax, Max-Age 600).
- return_to: must start with `/`, not `//` or `/\`, no scheme or backslash, <= 512 bytes, else /dashboard. Stored server-side only.
- Server-side sessions (no SESSION_KEY): cookie `__Host-hb_session` 32 random bytes, HttpOnly, Secure, Path=/, SameSite=Lax; DB stores sha256. Absolute 30 days, idle 7 days. New id at every login. Secure and redirect URI from BASE_URL, never from the request. With login configured, refuse to start if BASE_URL is empty or not https (except http://localhost, which drops `__Host-`).
- CSRF on every cookie-authenticated state change: JSON body (or raw body on content routes) + header X-CSRF-Token == session token (constant time) + Origin, when present, == BASE_URL origin. Token from GET /api/v1/session. No CORS. On POST /documents the cookie counts only with a valid X-CSRF-Token, else anonymous as today. Bearer requests exempt; if both present, Bearer wins.
- Personal tokens: `hbp_` + base62(32 random bytes) + 6-char base62 CRC32. Shown once; stored sha256 + display prefix (hbp_ab12cd). Max 20 per user. last_used_at updated at most once a minute. Never in query strings. Present but invalid/revoked -> 401 with `WWW-Authenticate: Bearer realm="hastebin", error="invalid_token"`, never anonymous fallback.
- Per-user limits (httprate.LimitBy on u:<id>) on top of per-IP: API reads 120/min, writes 30/min, token creation 10/h, account deletion 3/day; login start 20 per 10 min per IP.
- POST /api/v1/session/logout; logout-all deletes all sessions, optionally revokes all tokens.
- Account deletion: type login to confirm; one transaction deletes sessions, tokens, paste rows; queues all store keys (pastes, revisions, versions). 202.
- Admin always on: ADMIN_GITHUB_IDS numeric ids, checked per request from config.

## 3. Authorization and read path
handler.AccessGate interface `Check(ctx, key, *auth.Principal, Op) (*Meta, error)` on DocumentHandler.Access; nil when accounts are off (today's behaviour and tests).

| Route | Anonymous | Owner (cookie or token) | Other user |
|---|---|---|---|
| POST /documents, /log | as today | owned, never expires, user quota/limits; headers X-Paste-Title, X-Paste-Visibility: private, X-Paste-Expires: RFC3339 | owned by them |
| GET/HEAD /documents/{k}, /raw/{k} (Range) | public served; private 404 | served; JSON adds owner:true, editable_until, server_now | public served; private 404 |
| MCP read/search | as above | private works with token | 404-style tool error |
| /api/v1/* | 401 | own rows only (WHERE user_id=?) | 404 |

- Private denial identical to a missing key: same body, NotFound.Miss(ip) called, Store.Size(key) still called for timing. Same for tombstoned private pastes. Tombstoned public paste: 410 "removed by moderator".
- Non-owner detection: principal vs meta.user_id. Counts only content delivery: GET /documents, GET /raw (full or Range), MCP read/search, admin content view. HEAD and the static /<key> page don't count. Owner never counts. Owner's own curl without token does count (README).
- Edit window close: if now < created_at+EDIT_WINDOW && seen_at IS NULL, the reader takes a per-key lock, writes seen_at with synchronous commit, releases, then opens content. The editor holds the same lock while re-checking and swapping content atomically. Past the window: no lock, no write.
- Bots: page links never close the window (unfurlers get static HTML; keep no content-based og tags). A shared /raw link closes it deliberately. no-store prevents edge copies. UI hint while open: "Share the page link; opening /raw from another client ends editing." No user-agent special cases.

## 4. Dashboard API (/api/v1, JSON)
Errors `{"error":"<code>","message":"..."}`: 400 invalid_input, 401 unauthenticated, 403 csrf/disabled/reauth_required, 404 not_found, 409 edit_window_closed{reason:seen|expired}, 413 too_large, 429 rate_limited (Retry-After), 503 busy/db_busy, 507 no_space. Existing routes keep {"message"}.
Paste item: key, title, language, visibility, size, created_at, expires_at, edited_at, edit_count, views, last_opened_at, version, root_key, prev_key, next_key, editable_until.
```
GET    /auth/github/login?return=/path      302 to GitHub         GET /auth/github/callback   302 to return path
GET    /api/v1/session                      {user:{id,login,admin}|null, csrf, limits}
POST   /api/v1/session/logout | /logout-all {revoke_tokens?:bool}                      204
GET    /api/v1/pastes?q=&visibility=&language=&has_versions=&sort=-created|created|title|size|views|last_opened&page=
                                            {items[25], page, pages, total}   (q matches title/key/language, not content)
GET    /api/v1/pastes/{key}                 item + revisions[{rev,size,created_at}] + versions chain
PATCH  /api/v1/pastes/{key}                 {title<=200, language<=32, visibility, expires_at|null}   item
PUT    /api/v1/pastes/{key}/content         raw body (same slot/budget/quota/streaming path)   item | 409
POST   /api/v1/pastes/{key}/versions?delete_previous=0|1   raw body              201 new item (prev_key=key)
POST   /api/v1/pastes/{key}/duplicate                                           201 new item (quota-charged)
GET    /api/v1/pastes/{key}/revisions/{n}   text/plain (owner only)
DELETE /api/v1/pastes/{key}                                                     204
POST   /api/v1/pastes/delete                {keys[<=100]}  {deleted[], not_found[]}
GET    /raw/{key}?download=1                Content-Disposition: attachment; filename="<key>.<ext>"
GET|POST /api/v1/tokens   DELETE /api/v1/tokens/{id}   POST returns {id,name,prefix,token} once
GET|PATCH /api/v1/account {default_visibility}   GET /api/v1/account/export (metadata JSON, streamed)
DELETE /api/v1/account                      {confirm:"<login>"}                 202
```
- Refactor saveDocument/receive into `ReceiveInto(w, r, Uploader{IP, QuotaKey, Owner, Pin, MaxLength}, key)` shared by /documents, /log, content edits, new versions.
- Extend redactPath to /api/v1/pastes/{key}..., /api/v1/admin/pastes/{key}..., /dashboard/p/{key}.

## 5. Frontend
- New embedded files: dashboard.html, dashboard.js, dashboard.css, admin.html, admin.js. Routes /dashboard, /dashboard/*, /admin, /admin/* before `/*`; exclude dashboard, admin, auth in isPastePage. jQuery 3.7.1 same SRI, no inline scripts/handlers, .text()/textContent only, solarized dark + Fira Code. Don't depend on style-src 'unsafe-inline'.
- Button bar: `.account` button (person SVG). Logged out: GET link to /auth/github/login?return=<path> (form-action 'self' forbids a form). Logged in: shows login, opens /dashboard. app.loadInfo calls /api/v1/session only when /api/info has "login": true.
- Owner strip on the paste view: `Edit (8:41 left) | Private/Link | Dashboard`; everyone sees "edited at HH:MM"; countdown from editable_until - server_now.
- Dashboard list: 25 rows fixed, bulk delete confirms the count.
```
+-----------------------------------------------------------------------------------+
| hastebin  [My pastes] [Tokens] [Account]                  superkali  [Log out]    |
| [search title/key____] [Visibility v] [Language v] [Sort: newest v]  [+ New]     |
| [ ] Title / key            Lang  Vis   Size    Views Last opened  Created   ...   |
| [ ] build log armbian  v2  ans   link  1.2 MB    14  10:02        Oct 27    [:]   |
| [ ] cfg dump  (editable 7m) json  priv  3 KB      0  -            11:58     [:]   |
| [Delete selected (2)]                          < Prev   Page 2 of 9   Next >      |
+-----------------------------------------------------------------------------------+
```
  390 px: two-line cards (title + badge; size, views, date), checkbox left, [:] menu (View, Edit, Duplicate, Download, Delete); filters in a "Filters" disclosure; pagination one row.
- Paste detail /dashboard/p/{key}: metadata form (title, language, visibility, expiry never/date), stats, versions chain, edit history (time, size, view), textarea editor. Window open: Save same link; closed: "Save as new version" + "Delete previous version" checkbox.
- Tokens: table (name, prefix, created, last used, Revoke) + create form; new token in a one-time box with Copy and curl/Claude Code/Cursor/VS Code snippets with the header filled in (reuse mcp_panel copy/modal pattern).
- Account: login, GitHub id, created, paste count/bytes vs limits, default visibility, log out all (+ revoke tokens), metadata export, danger zone delete account (type login).

## 6. MCP
- NewHandler resolves Authorization: Bearer via auth.TokenVerifier before the SDK; principal in context; bad token -> HTTP 401 + WWW-Authenticate.
- Server func picks anonServer (unchanged, golden test) or userServer (built once): upload_log with optional title, visibility, expires_at (owned, user quota); read_paste/search_paste with private access; list_my_pastes(query?, page?) 20 per page, metadata only; instructions name the owner and default visibility.
- No delete/edit tools this phase (prompt-injection risk). Later delete_paste behind an explicit token scope.
- Later OAuth: /mcp stays anonymous with optional Bearer; reserve /mcp/u (auth-required, will serve /.well-known/oauth-protected-resource); api_tokens.kind/client_id/expires_at ready.

## 7. Config / egg
| Env | Default | Notes |
|---|---|---|
| GITHUB_CLIENT_ID | empty | empty = login off; DB still loaded so private stays private |
| GITHUB_CLIENT_SECRET | empty | egg user_viewable:false; redacted everywhere |
| ACCOUNTS_DB | <file_path>/hastebin.db | |
| EDIT_WINDOW | 600 | 0 disables same-link editing |
| SESSION_TTL / SESSION_IDLE | 2592000 / 604800 | |
| ADMIN_GITHUB_IDS | empty | comma-separated numeric ids |
| ADMIN_REAUTH | 43200 | destructive admin actions need auth_at younger |
| REGISTERED_MAX_LENGTH | 2 x max_length | non-file backends keep 8 MiB |
| REGISTERED_UPLOAD_QUOTA | 1 GiB/h per user | replaces per-IP quota for owned uploads |
| USER_STORAGE_LIMIT / USER_MAX_PASTES | 5 GiB / 10000 | |
| EDIT_MAX_REVISIONS / MAX_TOKENS_PER_USER | 20 / 20 | |
| REGISTER_MIN_ACCOUNT_AGE | 7 days | GitHub account age, against throwaways |
- /api/info gains "login": bool. Dockerfile: -ldflags "-X main.version=${VERSION}".
- README: Accounts (GitHub app setup, callback URL, what is stored), Tokens and curl, MCP with headers, Editing rules, Admin, Backup, Privacy. llms.txt: token usage.

## 8. Security
| Threat | Mitigation |
|---|---|
| Session fixation | new id each login, old cookie dropped, __Host- prefix |
| CSRF | SameSite=Lax + header token + Origin check + JSON only; logout is POST |
| Token leakage | no header logging, never in query, prefix-only logs, hbp_ prefix, hashed, shown once |
| Open redirect | server-side relative-only return_to |
| OAuth CSRF/mix-up | cookie-bound single-use state, PKCE, one IdP, exact redirect URI |
| Private leak (/raw, Range, HEAD, 304, MCP, revisions, CDN) | AccessGate before store on every content route; dotted revision keys; no-store; ETag/304 after the gate |
| Existence oracle | identical 404 + Miss(); Size() for timing; API scoped by user_id |
| Username reuse | numeric GitHub id |
| Token brute force | 256 bits + checksum + per-IP rate limit |
| Disabled user | checked on every request |
| Admin abuse | no admin tokens, re-auth, append-only audit, private content only by key with reason |
| DB theft | 0600, only hashes, no GitHub tokens or e-mails |
Privacy: GitHub id, login, timestamps, settings, hashed sessions/tokens, per-paste counters (no viewer identity, no IPs); IPs only in manual bans. GDPR: account deletion immediate; backups per operator retention (README); audit rows keep id/login (about.md).

## 9. Edge cases
- Account deletion with versions/revisions: all queued; prev_key SET NULL shows "v1 (deleted)".
- Token revoked mid-upload: rechecked at commit; content deleted, 401. Same for session.
- Edit races serialized by per-key lock; boundary checked inside the lock on server time; over EDIT_MAX_REVISIONS -> 409, UI offers new version.
- Private with expiry: 404 for everyone after expiry; dashboard "expired, deleting"; past date rejected.
- Quota/storage: Content-Length first, then while streaming; temp aborted; duplicates and versions count.
- Edit outage: copy old to .r<n>, write new, DB tx; tx fails -> restore, 503; failed store deletes retried by delete_queue.
- DB busy: 5 s then 503 db_busy; read gate fails closed (500).
- Existing anonymous pastes: unchanged, cannot be claimed.
- Clock: server time only; ISO UTC + server_now.
- Cookies behind cloudflared over http: Secure from BASE_URL; never X-Forwarded-Proto.
- Login turned off after use: DB still opened; login routes 404.
- 507 during owned upload: nothing left behind.
- about and other static docs: no row, public.
- Bulk delete: unowned keys in not_found (scoped, no oracle).

## 10. Milestones (login stays off until GITHUB_CLIENT_ID is set; do NOT enable login in production before M4)
| # | Milestone | Size | Acceptance |
|---|---|---|---|
| M0 | Deleter + Keeper on all backends; no-store on paste routes; version ldflags; ReceiveInto refactor; redactPath extended; HEAD / fix | M | suite green; testcontainers Delete per backend; pinned file survives janitor; `curl -I /raw/k.js` no-store; HEAD / 200 |
| M1 | internal/accounts (open, migrations, --backup-db), internal/auth (OAuth mocked with httptest, sessions, CSRF), /api/v1/session, login button, audit_log, admin Status page | M | login/logout/logout-all; fixation test; state reuse refused; bad return_to -> /dashboard; non-admin 404 on /admin; status shows disk/budget/slots |
| M2 | ownership via cookie and token on /documents, /log; tokens page; dashboard list, delete, bulk; user quota/limits; never-expire; account deletion | L | token upload owned; revoked token 401; EXPIRATION=60 leaves owned pastes; delete removes store objects; lists bounded |
| M3 | AccessGate: private, HEAD, Range, MCP; ban parity; per-paste expiry + sweeper; views flush | M | private 404 + Miss for anon/other/MCP without token; owner 200, Range 206; expiry hides then deletes |
| M4 | Admin moderation, users, bans (persisted manual), events, audit viewer | L | delete/abuse by URL; disable blocks at once; manual /64 ban; re-auth enforced; audit immutable |
| M5 | Editing: window, seen_at, history, X-Paste-Edited, versions, duplicate, download, title/language | L | -race 100 goroutines never shows a non-owner two contents; page fetch keeps window, anonymous /raw closes it; 409 -> new version with prev_key |
| M6 | MCP user server, list_my_pastes, upload params, 401 on bad Bearer, docs/llms.txt | S | anonymous tools/list identical to v1.1.0 (golden); token lists/reads private |
| M7 | Egg variables, README, mobile polish, export | S | egg imports; secret hidden; 390 px screenshots |
Live: pterodactyl target as uid 988, egg env; DB in data/ mode 0600; sessions and pins survive restart; --backup-db during uploads; 300 MiB limit; janitor ignores DB files; real GitHub login via a staging hostname with a separate OAuth app.
Chromium: zero CSP violations on /, /dashboard, /admin; login and return; owned save; countdown/edit; private 404 in a second context; bulk delete; token once; keyboard/focus; 390x844 screenshots.

## 12. Admin (always on)
- Admin session + CSRF; 404 for non-admins; no Bearer on /api/v1/admin/*; destructive actions (delete, abuse, disable, delete user, ban, private content view) need auth_at younger than ADMIN_REAUTH else 403 reauth_required -> /auth/github/login?reauth=1&return=/admin/...; every action -> audit_log + info log with hashed keys.
```
GET  /status                         version, uptime, storage type, disk free vs min_free_disk, file paste count/bytes (cached
                                     scan, at most every 10 min, "as of"), registered count/bytes, read/upload budget, slots,
                                     inflight clients, MCP in-use + counters, effective config (secrets "set"/"unset")
GET  /pastes/{key|url}               metadata: exists, size, mtime, owner|anonymous, visibility, expiry, tombstone
POST /pastes/{key}/content           {reason}: first 256 KiB; reason required; audited; closes the edit window
DELETE /pastes/{key}                 {reason}      POST /pastes/{key}/abuse {reason}: delete + tombstone (410) + owner strike
GET  /users?q=&sort=created|last_seen|bytes|pastes&page=     GET /users/{id} (+ paste metadata paged, no content)
POST /users/{id}/disable|enable {reason}   POST /users/{id}/revoke   DELETE /users/{id} {confirm,reason}
GET  /bans   POST /bans {target, reason, expires_in}   DELETE /bans/{kind}/{target}
GET  /events?page=   GET /audit?page=
```
- Limiter support: NotFoundLimiter.Snapshot/Unban, UploadQuota.Snapshot/Reset; httprate 429s recorded as events. Manual-ban middleware after realIP, before inflight, 403 {"message":"Blocked"} except /health, in-memory map loaded from ip_bans keyed by ClientKey.
- Persisted: manual bans, audit log. In memory: automatic bans/quota, abuse events ring of 500 (each kind at most once a minute per client).
- Tabs: Moderation, Users, Abuse, Status, Audit; 50 rows per page; tables become cards at 390 px.
```
+---------------------------------------------------------------------------------+
| hastebin admin   [Moderation] [Users] [Abuse] [Status] [Audit]       superkali  |
| Paste key or URL: [https://paste.superkali.me/abcdefghij____] [Look up]         |
| key #3fa2b1c0  owner: octocat (#583231)  private  1.4 MB  created Oct 27 10:02  |
| [Show content (logged, reason: ________)] [Delete] [Mark as abuse]              |
| Users  [search login__]  sort [last seen v]                                     |
| login      id       created   pastes  bytes   last seen  state    [actions]    |
+---------------------------------------------------------------------------------+
```

## 13. Tags (user decision 2026-10-28)
Registered pastes carry up to 10 tags, used to find "attachments" (logs referenced from external memory notes).
- Schema (in 0001_init.sql): `paste_tags(paste_key FK CASCADE, tag TEXT, PK(paste_key, tag))`, INDEX(tag, paste_key). Tag format: lowercase `[a-z0-9][a-z0-9._-]{0,31}`, normalized (trim, lowercase), max 10 per paste, duplicates dropped.
- Upload: header `X-Paste-Tags: a,b,c` on /documents and /log (owned uploads only; ignored for anonymous); MCP userServer upload_log gets optional `tags: []string`.
- API: item gains `tags[]`; PATCH accepts `tags`; GET /api/v1/pastes gains `tag=` (repeatable, AND); GET /api/v1/tags returns the user's tags with counts (bounded 200).
- MCP: list_my_pastes gains optional `tags` filter; results include tags.
- Dashboard: tag chips on rows, tag filter, edit tags in the detail form. Tags never visible to non-owners.
- Milestones: schema in M1, API/dashboard in M2, MCP in M6.

## 11. Open questions: ALL APPROVED by the user 2026-10-28 as recommended
1. Default visibility of registered uploads: link, per-user setting.
2. Registered limits: 2 x max_length, 1 GiB/h, 5 GiB, 10k pastes per user.
3. Registration: anyone with GitHub, REGISTER_MIN_ACCOUNT_AGE 7 days.
4. Dashboard search: title, key, language only (no content search).
5. Edit history visible only to the owner; viewers see "edited at HH:MM".
6. Abuse strikes: manual only, shown on the user row.
