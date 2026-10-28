# hastebin-ansi

An evolved hastebin for logs: ANSI colours, AI assistants over MCP and abuse protection built in.

Classic hastebin ([seejohnrun/haste-server](https://github.com/seejohnrun/haste-server)) is a simple paste site: you paste text and get a short link. hastebin-ansi keeps that workflow and adds what a public log service needs: terminal colours, uploads from a shell, access for AI assistants, and limits that stop one client from filling the server.

Public instance: <https://paste.superkali.me>

## What it adds over classic hastebin

- ANSI colour rendering, so logs look the way they did in your terminal.
- Uploads from any shell: `curl --data-binary @file https://<host>/log` prints the link.
- A public MCP server: AI assistants can upload logs, read pastes in pages and search them.
- Large logs. With file storage, uploads are streamed to disk up to `max_length` (32 MiB in the Pterodactyl egg). Pastes over 1 MiB open in a light plain text view with a link to the raw file.
- Abuse protection: per-client request and upload limits, upload slots, an hourly upload quota and a ban for clients that guess keys. A client is an IPv4 address or an IPv6 /64. In a container, the memory budget is capped at 40 percent of its memory limit.
- The real client IP behind Cloudflare, cloudflared or another trusted proxy.
- Security headers with a Content Security Policy, SRI on the CDN scripts, paste keys hidden in the access log, and `noindex` for search engines.
- Six storage backends: file, Redis, Memcached, MongoDB, PostgreSQL and S3.
- Optional GitHub login. Logged in users get a dashboard of their pastes, private pastes, editing under the same link for a few minutes, API tokens for curl and MCP, and an export of their data. Admins get a page for moderation, users, bans, server status and an audit log. Anonymous use stays as it is.
- Docker, docker compose and a Pterodactyl egg, with images for amd64 and arm64.
- Written in Go: a single binary with the web page built in.

## Quick use

### Browser

Open the site, type or paste, and press Save (Ctrl+S). The address bar then holds the link to share. Ctrl+N starts a new paste.

Without an extension, or with one the highlighter does not know (like `.log`), a paste is shown with its ANSI colours. With a known extension (`/<key>.sh`, `/<key>.py`, `/<key>.json`) it is shown with syntax highlighting instead, and `.txt` shows plain text. Pastes over 1 MiB are shown as plain text, cut to the first 1 MiB, with a link to the full raw text.

### From a shell

Upload a file. The server prints the link:

```sh
curl --data-binary @build.log https://paste.superkali.me/log
```

Pipe command output:

```sh
dmesg | curl --data-binary @- https://paste.superkali.me/log
make 2>&1 | curl --data-binary @- https://paste.superkali.me/log
```

Use `--data-binary`, not `-d`: `-d` strips newlines. Many tools drop colours when their output goes to a pipe, so force them on (`--color=always`), or run the command under `script -qc 'make' /dev/null`.

`/log` accepts POST and PUT. The answer is plain text: an empty line, the paste URL, another empty line.

Read a paste as plain text:

```sh
curl https://paste.superkali.me/raw/<key>
```

### JSON API

| Request | Answer |
|---|---|
| `POST /documents` with the text as the body | `{"key": "<key>"}` |
| `GET /documents/<key>` | `{"data": "<text>", "key": "<key>"}`. A paste over 1 MiB comes back cut to its first 1 MiB, with `"truncated": true` and its full `"size"` in bytes |
| `GET /raw/<key>` | the whole text, `text/plain; charset=UTF-8`. With file storage it is streamed and `Range` requests work |
| `GET /api/info` | `{"mcp": true, "login": false, "max_length": 4000000, "mcp_max_length": 4000000}` |
| `GET /health` | `200` with an empty body |

```sh
curl --data-binary @build.log https://paste.superkali.me/documents
curl https://paste.superkali.me/documents/<key>
```

`POST /documents` and `/log` also accept a `multipart/form-data` body with the text in a field named `data`, sent as a value (`curl -F 'data=<build.log' ...`) or as a file (`curl -F 'data=@build.log' ...`). The part headers of one form may take 64 KiB in all. `/documents/<key>` and `/raw/<key>` ignore an extension on the key and also answer HEAD.

Errors come back as `{"message": "..."}` with one of these status codes:

| Code | When |
|---|---|
| 400 | The request body could not be read, it is empty, or the part headers of a multipart form are over 64 KiB |
| 404 | No paste with that key, or it expired |
| 408 | The upload arrived too slowly (see `upload_timeout`) |
| 413 | The paste is larger than `max_length`. A body whose `Content-Length` is already over it is refused before it is read |
| 429 | Rate limit reached (plain text body), this client has too many requests in progress (see `max_inflight_per_client`), it looked up too many unknown keys (see key guessing below), or it used up its `upload_quota`. `Retry-After` tells you when to retry |
| 503 | All large upload slots are busy, this client already has its share (see `max_concurrent_uploads`), or the `memory_budget` is full. `Retry-After: 10` tells you when to retry |
| 507 | The server is low on disk space (see `min_free_disk`) and takes no uploads for now |
| 500 | The storage backend failed |

## Use with AI assistants (MCP)

The MCP endpoint is `https://paste.superkali.me/mcp`. It uses the streamable HTTP transport and needs no login.

Claude Code:

```sh
claude mcp add --transport http hastebin https://paste.superkali.me/mcp
```

Claude Desktop or claude.ai: open Settings > Connectors > Add custom connector, name it `hastebin` and use the URL `https://paste.superkali.me/mcp`.

Cursor (`~/.cursor/mcp.json`). Many other clients use the same shape:

```json
{
  "mcpServers": {
    "hastebin": { "url": "https://paste.superkali.me/mcp" }
  }
}
```

VS Code (`.vscode/mcp.json` in the project, or your user MCP config):

```json
{
  "servers": {
    "hastebin": { "type": "http", "url": "https://paste.superkali.me/mcp" }
  }
}
```

The AI / MCP button on the web page shows the same setup with the address of the server you are on. `/llms.txt` has a short version for agents.

### Tools

| Tool | Arguments | Returns and limits |
|---|---|---|
| `upload_log` | `content` (required): the text. `extension` (optional): letters and digits only, up to 16, added to the link for highlighting. Leave it empty for logs with ANSI colours. | `url`, `raw_url`, `key`, `size` (bytes), `lines`. The content can be up to `mcp_max_length` bytes. Over that the tool refuses it and points to the `curl` upload. |
| `read_paste` | `key_or_url` (required): a key, or a paste URL such as `https://host/<key>.log` or `https://host/raw/<key>`. `offset_line` (default 1). `max_lines` (default 400, at most 2000). `keep_ansi` (default false). | Numbered lines, plus `total_lines`, `total_bytes` and `next_offset` for the next page. At most 256 KiB per call. A longer line is cut, and the reply points to `/raw/<key>` for the whole line. ANSI codes are removed unless `keep_ansi` is true. |
| `search_paste` | `key_or_url` (required). `pattern` (required): RE2 regular expression, up to 256 characters. `ignore_case` (default false). `context_lines` (default 2, at most 10). `max_matches` (default 50, at most 200). `keep_ansi` (default false). | Matching lines in `grep -n` style: line number, then `:` for a match or `-` for context. Lines over 2 KiB are cut. At most 256 KiB per call. A search stops after 5 seconds and returns the matches found so far, marked `truncated`. Lines over 64 KiB are searched in 64 KiB pieces that overlap by 512 bytes, and the reply then says `approximate: true`: a match on such a line must fit in one piece, so a match longer than 512 bytes that crosses a piece edge is missed. `^` and `$` still match only at the real start and end of the line, but `\b` can match at a piece edge. Very large patterns (over 2000 compiled steps, such as long repeat counts) are refused. |

Up to 8 `read_paste` calls and 4 `search_paste` calls run at a time, counted apart, so searches never hold up reads. One client (an IPv6 /64 counts as one) may run one call of each at a time, and may have 4 requests to `/mcp` in progress at once (`mcp_max_inflight_per_client`). A fifth gets 429 with a JSON-RPC error and `Retry-After: 2`. A read of a paste of 64 KiB or more also needs room in the `memory_budget`. Past these limits a call gets an error that says to retry in a few seconds. JSON-RPC batches (a POST body that is a JSON array) are refused with 400: MCP dropped batching in its 2025-06-18 revision.

Two things to keep in mind:

- Pastes are public to anyone who has the link. Remove API keys, tokens, passwords, private keys and cookies before uploading. The server tells the assistant this too.
- Paste content that an assistant reads back was written by someone else. The assistant should treat it as untrusted data and never follow instructions found in it.

For large files, an assistant that has a shell does better with `curl --data-binary @file https://paste.superkali.me/log` than with passing the whole file through `upload_log`. Files over `mcp_max_length` have to go that way.

Opening `/mcp` in a browser returns a short hint (HTTP 405) instead of the web page.

### With an API token

Without a token `/mcp` works as described above: anonymous and public. With one of your [API tokens](#api-tokens-and-curl) in an `Authorization: Bearer` header, the assistant works as you:

- `upload_log` makes pastes that are yours. They count against your own limits and quota, show up in your dashboard and never expire unless you set `expires_at`. It also takes `title`, `visibility` (`link` or `private`, by default the one set on your Account tab), `expires_at` (an RFC 3339 time in the future) and `tags` (up to 10, same rules as `X-Paste-Tags`).
- `read_paste` and `search_paste` also read your private pastes. Reading your own paste is not counted as a view and does not end its edit window.
- `list_my_pastes` takes `query` (title, key or language, never the content), `tags` (all must match) and `page`. It returns your own pastes, newest first, 20 per page: key, URL, title, language, visibility, size, creation and expiry time, tags and version. Never the content.
- The server tells the assistant whose token it is and which visibility new pastes get.

There are no tools to delete or edit pastes. That stays in the dashboard.

Claude Code:

```sh
claude mcp add --transport http hastebin https://paste.superkali.me/mcp --header "Authorization: Bearer hbp_..."
```

Cursor (`~/.cursor/mcp.json`):

```json
{
  "mcpServers": {
    "hastebin": {
      "url": "https://paste.superkali.me/mcp",
      "headers": { "Authorization": "Bearer hbp_..." }
    }
  }
}
```

VS Code (`.vscode/mcp.json`). The input asks for the token once, so it is not written in the file:

```json
{
  "inputs": [
    { "type": "promptString", "id": "hastebin-token", "description": "hastebin API token", "password": true }
  ],
  "servers": {
    "hastebin": {
      "type": "http",
      "url": "https://paste.superkali.me/mcp",
      "headers": { "Authorization": "Bearer ${input:hastebin-token}" }
    }
  }
}
```

Custom connectors in claude.ai and Claude Desktop cannot send a header, so they stay anonymous until MCP login with OAuth arrives. `/mcp/u` is kept for it and answers 404 for now.

A token that is malformed, unknown, revoked or belongs to a disabled account gets 401 with `WWW-Authenticate: Bearer realm="hastebin", error="invalid_token"`, never an anonymous answer. The token is read only from the header, never from the URL, and a login cookie does not count on `/mcp`. Each account may send 120 requests a minute to `/mcp`, on top of the limits per client; more get 429 with `Retry-After`.

## Run it

### Docker

Images are published at `ghcr.io/superkali/ansi-hastebin` for amd64 and arm64. The default image runs as uid 65532, so the data folder has to belong to that user:

```sh
mkdir -p data
sudo chown -R 65532:65532 data
docker run -d --name hastebin --restart unless-stopped \
  -p 7777:7777 \
  -v "$PWD/data:/data" \
  -e STORAGE_FILE_PATH=/data \
  -e BASE_URL=https://paste.example.com \
  ghcr.io/superkali/ansi-hastebin:latest
```

The image ships no `config.yaml`. It reads `/app/config.yaml` if you mount one, otherwise it starts with the defaults plus your environment variables. The about page built into the binary is served at `/about`. To serve your own page instead, mount it and name it:

```sh
  -v "$PWD/about.md:/app/about.md:ro" -e DOCUMENTS_about=/app/about.md
```

### docker compose

`docker-compose.yaml` builds the image from this repository and mounts `config.yaml` and `./data`:

```sh
mkdir -p data
sudo chown -R 65532:65532 data
docker compose up -d
```

The server checks at startup that it can write to the data folder. If it cannot, it exits with the `chown` command to run.

### From source

You need Go 1.26 (see `go.mod`).

```sh
git clone https://github.com/SuperKali/hastebin-ansi.git
cd hastebin-ansi
go build -o hastebin ./cmd/main.go
./hastebin --config config.yaml
```

With the sample `config.yaml`, pastes go to `./data`. Without `--config`, the server reads `config.yaml` from the current folder, and a missing file just means defaults.

### Pterodactyl

The egg in `pterodactyl/egg-hastebin-ansi.json` offers two images, both built by GitHub Actions, so nothing is compiled on the node:

- `ghcr.io/superkali/ansi-hastebin:pterodactyl-v1.1.0`, shown as "release v1.1.0", the default: a release that never changes. Wings pulls it again on restart, but gets the same image. To update, pick the image of a newer release in the server's Startup tab.
- `ghcr.io/superkali/ansi-hastebin:pterodactyl`, shown as "latest (follows main)": rebuilt on every push to `main`, so a restart brings whatever is on `main` now.

1. Import the egg in the panel (Admin > Nests > Import Egg).
2. Create a server with it. The install script writes `config.yaml` and creates `data/`. Egg variables (storage, limits, proxy, MCP) are passed as environment variables and override `config.yaml`.

The egg sets `max_length` to 32 MiB, `base_url` to `https://paste.superkali.me` (change it to your address), and keeps rate limiting on (500 requests per 15 seconds). Give the server 640 MiB of RAM or more to keep the default `memory_budget` of 256 MiB: the server uses at most 40 percent of its memory limit as `memory_budget`, and keeps the rest for itself and for the kernel (see `memory_budget`). With less RAM it lowers `memory_budget` by itself, so 300 MiB still works, with smaller limits. The egg sets `EXPIRATION` to 31536000 (1 year): an anonymous paste is deleted after a year without being read or written. Pastes of logged in users are not affected. With file storage this also deletes the anonymous paste files you already have that are older than that, so back up the data folder before you import the egg on an existing server, or set `EXPIRATION` to 0 to keep everything. The code default stays 0 (never), so Docker and binary installs keep their old behaviour. `min_free_disk` stops uploads before the disk is full either way. The egg also trusts `172.18.0.1` as a proxy with `CF-Connecting-IP` as the client IP header. Read the reverse proxy section below before you expose the port, and bind the allocation to `127.0.0.1` when you keep that setting.

Moving a server from the old egg that built from source:

1. Switch the server to this egg.
2. In Startup, set the Startup Command to `hastebin --config config.yaml`.
3. Press Reinstall. The script removes the old build files, saves your old config as `config.yaml.bak.<date>-<time>` and writes a new `config.yaml`. The `data/` folder is kept.

## Configuration

Settings come from `config.yaml` (or the file given with `--config`). An environment variable, when set, overrides the file. A setting left at 0 or empty falls back to its default. Booleans take `true` or `false`.

| yaml key | Env var | Default | Meaning |
|---|---|---|---|
| `host` | `HOST` | `0.0.0.0` | Address to listen on |
| `port` | `PORT` | `7777` | Port to listen on |
| `base_url` | `BASE_URL` | empty | Public URL used in the links that `/log` and the MCP tools return. Empty means `https://` plus the request's Host header, and the server logs a warning at startup |
| `key_length` | `KEY_LENGTH` | `16` | Length of new paste keys, at least 1. Older, shorter keys keep working |
| `key_generator` | `KEY_GENERATOR` | `phonetic` | `phonetic` (consonants and vowels in turn, easy to read out) or `random` |
| `key_space` | `KEY_SPACE` | `a` to `z` | Characters the `random` generator uses |
| `max_length` | `MAX_LENGTH` | `4000000` | Largest paste, in bytes. A negative value stops the server at startup |
| `memory_budget` | `MEMORY_BUDGET` | `268435456` | Bytes that paste reads and uploads may hold in memory at the same time: half for reads, half for uploads. A request that does not fit gets 503. In a container, at most 40 percent of the memory limit. A negative value stops the server at startup |
| `min_free_disk` | `MIN_FREE_DISK` | `1073741824` | With `file` storage, bytes that must stay free on the disk of the data folder. An upload that would leave less gets 507. `-1` turns the check off |
| `upload_quota` | `UPLOAD_QUOTA` | `268435456` | Bytes one client may upload per hour. Over it uploads get 429. `-1` turns the quota off |
| `max_concurrent_uploads` | `MAX_CONCURRENT_UPLOADS` | `8` | Large uploads received at the same time. `-1` means no limit |
| `max_uploads_per_ip` | `MAX_UPLOADS_PER_IP` | `1` | Large uploads one client may send at the same time. `-1` means no limit |
| `max_inflight_per_client` | `MAX_INFLIGHT_PER_CLIENT` | `16` | Requests one client may have in progress at the same time, on all routes together. More get 429. `-1` means no limit |
| `mcp_max_inflight_per_client` | `MCP_MAX_INFLIGHT_PER_CLIENT` | `4` | Requests to `/mcp` one client may have in progress at the same time. More get 429. `-1` means no limit |
| `upload_timeout` | `UPLOAD_TIMEOUT` | `300` | Seconds an upload may take. `-1` keeps the normal 60 second server timeouts |
| `not_found_limit` | `NOT_FOUND_LIMIT` | `30` | Lookups of unknown keys one client may make within `not_found_window` before it is banned from lookups. `-1` turns the ban off |
| `not_found_window` | `NOT_FOUND_WINDOW` | `900` | Seconds over which unknown key lookups are counted |
| `not_found_ban` | `NOT_FOUND_BAN` | `3600` | Seconds a banned client gets 429 on lookups |
| `expiration` | `EXPIRATION` | `0` | Lifetime of anonymous pastes in seconds, counted from the last read or write. `0` means they never expire. The Pterodactyl egg sets 1 year. With `file` storage, a value above 0 deletes old paste files (see below) |
| `mcp_enable` | `MCP_ENABLE` | `true` | Serve the MCP endpoint at `/mcp` |
| `mcp_max_length` | `MCP_MAX_LENGTH` | `4194304` | Largest text `upload_log` accepts over MCP, in bytes. Never more than `max_length`. A negative value stops the server at startup |
| `trusted_proxies` | `TRUSTED_PROXIES` | empty | Proxy IPs or CIDRs allowed to set the client IP. The env var is comma separated |
| `real_ip_header` | `REAL_IP_HEADER` | empty | The one header read for the client IP. Empty means `X-Forwarded-For` |
| `metrics_addr` | `METRICS_ADDR` | empty | Address of a separate Prometheus listener, for example `127.0.0.1:9100`. Empty means off |
| `storage.type` | `STORAGE_TYPE` | `file` | `file`, `redis`, `memcached`, `mongodb`, `postgres` or `s3` |
| `storage.file_path` | `STORAGE_FILE_PATH` | `data` | Folder for the `file` backend |
| `storage.host` | `STORAGE_HOST` | empty | Backend host |
| `storage.port` | `STORAGE_PORT` | none | Backend port. Always set it for network backends |
| `storage.username` | `STORAGE_USERNAME` | empty | Backend user (for `s3`, the access key ID) |
| `storage.password` | `STORAGE_PASSWORD` | empty | Backend password (for `s3`, the secret key) |
| `storage.database` | `STORAGE_DATABASE` | empty | Database name for `postgres` and `mongodb` |
| `storage.bucket` | `STORAGE_BUCKET` | empty | Bucket for `s3` |
| `storage.aws_region` | `STORAGE_AWS_REGION` | empty | Region for `s3` |
| `storage.tls` | `STORAGE_TLS` | `false` | Use `https://` for the `s3` endpoint |
| `documents` | `DOCUMENTS_<key>=<path>` | built-in about page | Fixed pages loaded from files at startup, served at `/<key>` and never expiring. A list of `key` and `path`. Env entries are added to the ones in the file. With none configured, the about page built into the binary is served at `/about` |
| `rate_limiting.enable` | `RATE_LIMITING_ENABLE` | `true` | Limit requests per client. Only an explicit `false` turns it off |
| `rate_limiting.limit` | `RATE_LIMITING_LIMIT` | `500` | Requests allowed per window |
| `rate_limiting.window` | `RATE_LIMITING_WINDOW` | `15` | Window length in seconds |
| `logging.level` | `LOGGING_LEVEL` | `info` | `trace`, `debug`, `info`, `warn` or `error`. Paste keys are only logged at `debug` |
| `logging.colorize` | `LOGGING_COLORIZE` | `false` | Readable console output instead of JSON lines |
| `github_client_id` | `GITHUB_CLIENT_ID` | empty | Client ID of the GitHub OAuth App for login. Empty means login is off. See Accounts below |
| `github_client_secret` | `GITHUB_CLIENT_SECRET` | empty | Its client secret. Never logged; the admin status shows only `set` or `unset` |
| `accounts_db` | `ACCOUNTS_DB` | `<storage.file_path>/hastebin.db` | SQLite file for accounts and sessions. With a backend other than `file`, `./hastebin.db` |
| `session_ttl` | `SESSION_TTL` | `2592000` | Seconds a login lasts at most (30 days) |
| `session_idle` | `SESSION_IDLE` | `604800` | Seconds a login lasts without being used (7 days) |
| `admin_github_ids` | `ADMIN_GITHUB_IDS` | empty | Numeric GitHub ids of the admins. A list in the file, comma separated in the env var |
| `admin_reauth` | `ADMIN_REAUTH` | `43200` | Seconds after a login an admin may still take destructive actions without logging in again |
| `register_min_account_age` | `REGISTER_MIN_ACCOUNT_AGE` | `604800` | Seconds a GitHub account must exist before it can register. Admins are exempt. `-1` turns the check off |
| `registered_max_length` | `REGISTERED_MAX_LENGTH` | twice `max_length` | Largest paste a logged in user may upload, in bytes. Storage other than `file` caps it at 8 MiB, like `max_length` |
| `registered_upload_quota` | `REGISTERED_UPLOAD_QUOTA` | `1073741824` | Bytes one logged in user may upload per hour (1 GiB). It counts per account and replaces the per address quota for their uploads. `-1` turns it off |
| `user_storage_limit` | `USER_STORAGE_LIMIT` | `5368709120` | Bytes the pastes of one user may take in all (5 GiB). `-1` means no limit |
| `user_max_pastes` | `USER_MAX_PASTES` | `10000` | Pastes one user may keep. `-1` means no limit |
| `max_tokens_per_user` | `MAX_TOKENS_PER_USER` | `20` | API tokens one user may have, 1 to 1000 |
| `edit_window` | `EDIT_WINDOW` | `600` | Seconds after creation the owner may change a registered paste under the same link, until someone else opens it. `0` turns same-link editing off |
| `edit_max_revisions` | `EDIT_MAX_REVISIONS` | `20` | Older texts one registered paste keeps from same-link edits, 1 to 1000. Past it, changes go to a new version |

**expiration.** Seconds, 0 or more. Each view starts the lifetime again, so a paste goes away after that many seconds without being read. The `redis`, `memcached`, `mongodb` and `postgres` backends hand it to the database. The `file` backend checks the file's modification time: a read refreshes it, an older file is no longer served, and every 10 minutes a janitor deletes those files. **Turning `EXPIRATION` on with `file` storage deletes the pastes you already have that are older than that age**, so back up the data folder first if you want to keep them. The `s3` backend ignores it: use a bucket lifecycle rule. Pages from `documents` and pastes of logged in users never expire this way; with `file` storage their files, the older texts kept by edits and the accounts database are never touched by the janitor.

**max_length.** The code default is 4000000 bytes. The Pterodactyl egg uses 32 MiB. MCP uploads have their own, lower limit, `mcp_max_length`.

With `file` storage, large pastes cost disk, not memory. An upload goes straight to a temp file in the data folder as it arrives, through a 32 KiB buffer, and is renamed to its paste file once it is complete, so a paste is never seen half written. When the upload fails (too large, too slow, client gone, disk or quota full) the temp file is deleted. Temp files left by a crash are deleted at startup, and by the janitor once nobody has written to them for an hour. Reads work the same way: `/raw` streams the file, `/documents` reads only the first 1 MiB of it, and the MCP tools read it a line at a time. Use `file` storage for large logs.

The other backends (`redis`, `memcached`, `mongodb`, `postgres`, `s3`) hand whole pastes to the server, which holds them in memory. For them `max_length` is capped at startup at 8 MiB, or lower when one client's share of `memory_budget` is too small for a paste that size, and the server logs the value it uses. Older pastes larger than that may get 503 when read.

**memory_budget.** The budget is split in two halves that do not touch each other: one for reads, one for uploads. So slow uploads cannot block paste views, and many reads cannot block uploads. Each request books an estimate of its peak memory in its half before it holds that memory, and gives it back when done:

- With `file` storage, an upload over 1 MiB books its 32 KiB copy buffer, whatever size it announces. `/documents` and `/raw` book 256 KiB each until the answer is sent: a client that reads slowly keeps that much in buffers and kernel socket buffers. An MCP `read_paste` or `search_paste` call books 2 MiB.
- With the other backends, an upload over 1 MiB books nothing up front and then 1 MiB at a time as its body arrives, twice what has arrived so far, whether or not it announced its size. When the next part does not fit, the upload stops there. Reads book twice the paste size, plus the amounts above.
- Every request to `/mcp`, small or large, is held in memory by the MCP library, whatever the storage. Decoding its JSON takes about 6 times its size, so it books 6 times what has arrived as it arrives, in steps of 64 KiB to 1 MiB.
- A read of a paste under 64 KiB books nothing.

One client (an IPv4 address or an IPv6 /64) may hold at most a quarter of each half, and no single request may book more than that. A request that does not fit is refused at once with 503 and `Retry-After: 10` (an MCP tool returns "server busy"). The server needs about `memory_budget` plus 64 MiB for itself, and sets the Go soft memory limit to that sum, so garbage left by large requests is freed early; set `GOMEMLIMIT` to choose another limit. Open connections also cost memory outside the Go heap: the kernel socket buffers of clients that read slowly. So give the server more RAM than that sum.

In a container with a memory limit (cgroup v2 `memory.max`, or cgroup v1 `memory.limit_in_bytes`), the server uses at most 40 percent of that limit as `memory_budget`, and the soft memory limit is at most 70 percent of it. The rest is left for socket buffers and the kernel. In a 300 MiB container that is a 120 MiB budget and a 184 MiB soft limit; a 1 GiB container keeps the default 256 MiB budget. It logs the values it uses at startup.

**min_free_disk.** Only for `file` storage. Before each upload on `/documents`, `/log` and the MCP `upload_log` tool, the server checks the free space on the disk of the data folder, and refuses the upload with 507 (a tool error over MCP) when less than `min_free_disk` would be left after it. The free space is read at most every 5 seconds, and uploads in between are taken off that reading. The default keeps 1 GiB free. `-1` turns the check off.

**upload_quota.** Bytes one client may store per hour, counted on `/documents`, `/log` and the MCP `upload_log` tool, so one client cannot fill the disk. A client is an IPv4 address or an IPv6 /64. Over the quota uploads get 429 with `Retry-After` (an MCP tool error) until the hour that started with its first upload is over. Up to 100000 clients are tracked; past that the one seen longest ago is forgotten.

**max_concurrent_uploads and max_uploads_per_ip.** Bodies over 1 MiB, or of unknown size, need a free slot, on `/documents`, `/log` and `/mcp` alike. `max_concurrent_uploads` caps the slots in total, `max_uploads_per_ip` caps them per client (the real IP, after `trusted_proxies`; an IPv6 client counts as its whole /64, since it can use any address in it). When either limit is reached the server answers 503 with `Retry-After: 10`. Smaller uploads never need a slot.

**upload_timeout.** Uploads over 1 MiB, or of unknown size, on `POST /documents`, `POST` and `PUT /log` and `POST /mcp` get this long to arrive and be answered. Smaller uploads and every other request keep the 60 second read and write timeouts. A large upload must also deliver at least 64 KiB in every 10 seconds, and once it has run for a minute it must have averaged at least 64 KiB per second since it started, or it is dropped with 408. So clients that trickle data hold the upload slots for a minute at most, not for the whole timeout. The cost: on a link slower than about 512 kbit/s, an upload has to finish within its first minute, so only a few MiB get through. Upload large files from a faster link, or split them.

**Key guessing.** Pastes are public to anyone with the link, so the link is the only secret. Two things make guessing one hard. Keys are 16 characters long by default. And a client that asks for more than `not_found_limit` keys that do not exist within `not_found_window` seconds is banned from lookups for `not_found_ban` seconds: `/documents/<key>` and `/raw/<key>` answer 429 with `Retry-After`, and the MCP `read_paste` and `search_paste` tools return an error, even for keys that exist. Uploads, the web page and everything else keep working. Lookups that find a paste do not reset the count. The client is the real IP, after `trusted_proxies`, with an IPv6 /64 counted as one client. Set `trusted_proxies` up behind a proxy, or all clients share one count. The access log shows each key as `#` and the first 8 hex digits of its SHA-256 (for example `/raw/#3fa2b1c0`), so the log does not list working links.

**max_inflight_per_client and mcp_max_inflight_per_client.** Each request in progress holds a connection, its buffers and, for a client that reads slowly, kernel socket buffers that the memory budget does not see. So one client (the real IP, after `trusted_proxies`; an IPv6 /64 counts as one) may have at most `max_inflight_per_client` requests in progress at the same time, on every route, the web page files included. A web page loads well under 16 files at once, so a browser never comes near it. Requests to `/mcp` have their own, lower limit, `mcp_max_inflight_per_client`, as each one holds its whole body in memory. Over a limit the server answers 429 with `Retry-After: 2` at once, before it reads the body. Behind a proxy, set `trusted_proxies`, or every client shares the one count of the proxy.

**mcp_enable.** With `false`, `/mcp` answers 404 and the web page hides the AI / MCP button.

**mcp_max_length.** The JSON that carries a log through `upload_log` is held in memory while it is decoded, which takes about 6 times the request size, so MCP uploads have their own, small limit. The default is 4 MiB, and the value used is never more than `max_length`. `0` means the default. A request is booked at 6 times its size, so the value is also capped so that 6 times it, plus a quarter for JSON escaping, fits in one client's share of the upload half of `memory_budget`: about 4.3 MiB with the default 256 MiB budget, 2 MiB in a 300 MiB container. The server logs a lower value at startup, and `/api/info` shows the value in use. A larger text gets a tool error that tells the assistant to upload the file with `curl --data-binary @file <server>/log`. A request to `/mcp` may be up to twice `mcp_max_length` plus 1 MiB, to leave room for JSON escaping, and at most a sixth of that share (about 5.3 MiB with the default budget, 2.5 MiB in a 300 MiB container). A bigger one is refused with 413 before it is read, with the same hint. Text with many ANSI colour codes grows in JSON, so it may hit that limit below `mcp_max_length`: upload such logs with `curl`.

**rate_limiting.** Counts requests per client (an IPv6 /64 counts as one) and answers 429 over the limit. It is on by default, with 500 requests per 15 seconds; set `enable: false` (or `RATE_LIMITING_ENABLE=false`) to turn it off. A limit or window of 0 means the default. A negative one also falls back to the default, with a warning.

**metrics_addr.** Prometheus metrics are only served on this separate listener, at `/metrics`. `/metrics` on the main port answers 404. Besides the Go runtime metrics there are two counters, `hastebin_paste_created` and `hastebin_paste_read`. Keep the address private, for example `127.0.0.1:9100`.

## Behind a reverse proxy or tunnel

By default the server ignores `CF-Connecting-IP`, `X-Real-IP` and `X-Forwarded-For`, so a client cannot fake its address in the logs or in the rate limiter. Behind a proxy that means every request appears to come from the proxy, and all clients share one rate limit.

To fix that, list the proxy's own address in `trusted_proxies`. Only requests whose TCP peer is in that list can set the client IP, and only through one header:

- `real_ip_header` empty: `X-Forwarded-For`, read right to left, skipping trusted proxies.
- `real_ip_header` set (for example `CF-Connecting-IP` or `X-Real-IP`): only that header, which must hold a single IP.

With cloudflared, set:

```sh
TRUSTED_PROXIES=172.18.0.1
REAL_IP_HEADER=CF-Connecting-IP
```

`172.18.0.1` stands for the address cloudflared connects from. Trust exactly that one address, and make sure it cannot change or be shared:

- cloudflared in a container: give it a fixed IP on a user defined Docker network (`--ip` or `ipv4_address` in compose) and list that IP. Do not list the gateway.
- cloudflared on the host: it reaches the server through the Docker gateway (often `172.18.0.1` in Pterodactyl), so the gateway is what you trust. Anything else that comes in through the gateway is trusted too: clients from the internet if the port is published, other hosts, and other containers that connect through the host's published port. So bind the port to loopback (`-p 127.0.0.1:7777:7777`, or a `127.0.0.1` allocation in Pterodactyl) and point cloudflared at `127.0.0.1:<port>`. Then only processes on the host can come through the gateway.

To find the address, start without `TRUSTED_PROXIES`: each request line in the log shows the address it came from. When a request carries `real_ip_header` (or `CF-Connecting-IP`) but its peer is not in `trusted_proxies`, the header is ignored and the server logs a warning, at most once a minute, with the peer address. If that peer is your proxy, the list is wrong.

Behind nginx on the same host, `proxy_set_header X-Real-IP $remote_addr;` with `REAL_IP_HEADER=X-Real-IP` and the address nginx connects from in `TRUSTED_PROXIES` does the same job.

Set `BASE_URL` to the public address as well, so the links from `/log` and MCP are right whatever Host header the proxy forwards.

The server sets `Content-Security-Policy`, `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer` and `X-Robots-Tag: noindex, nofollow` on every response, so the proxy does not need to add them.

## Accounts

GitHub login is optional and off until `GITHUB_CLIENT_ID` is set. Anonymous use works the same with login on or off. A logged in user gets a dashboard of their own pastes, private pastes, editing under the same link, API tokens and MCP as themselves.

### Setup

1. On GitHub, open Settings > Developer settings > OAuth Apps > New OAuth App.
2. Set the homepage URL to your `BASE_URL`, and the authorization callback URL to `BASE_URL` followed by `/auth/github/callback`, for example `https://paste.example.com/auth/github/callback`.
3. Generate a client secret, then set `GITHUB_CLIENT_ID` and `GITHUB_CLIENT_SECRET`. In the Pterodactyl egg only panel admins can see the secret.
4. Set `BASE_URL` to the public https address. With login on, the server does not start without it. Plain http is accepted only on `localhost` or `127.0.0.1`, for tests; the cookies then lose `Secure` and the `__Host-` prefix.
5. To be an admin, put your numeric GitHub id in `ADMIN_GITHUB_IDS` (`https://api.github.com/users/<login>` shows it as `id`).

The app asks GitHub for no scopes. The server needs outbound https to `github.com` and `api.github.com`. Accounts do not work with `memcached` storage, which can evict pastes at any time: the server refuses to start.

A GitHub account younger than `REGISTER_MIN_ACCOUNT_AGE` (7 days) cannot register; admins are exempt. An account is tied to the numeric GitHub id, so a renamed or reused login name never gets someone else's account.

The server keeps accounts in the SQLite file `hastebin.db` in the data folder (or `ACCOUNTS_DB`), with mode 0600. It is created when login is first turned on. If you turn login off later, the file is still loaded: private pastes stay private and existing sessions keep working, but nobody new can log in.

What is stored per account: the GitHub id, login name and avatar URL, the account times (joined, last login, last seen), the default visibility for new pastes, and for each login the SHA-256 of its cookie and a CSRF token. API tokens are stored as a SHA-256 hash and their first characters. The server does not store the GitHub access token (it reads who logged in once, then drops it), your e-mail address or any IP address.

### Dashboard

The dashboard is at `/dashboard`. The account menu (your avatar in the top bar) leads to it from every page.

- The My pastes tab lists your pastes, 25 per page, with search on title, key and language, filters for visibility, language and tags, and sorting. You can delete one paste or select many. A deleted paste is gone from the list at once; its content is removed from the store right after.
- A paste opens in its own page: open, download, copy the link, duplicate or delete it; change its title, language, visibility, expiry and tags; see its size, views, versions and edit history.
- The Tokens tab creates and revokes API tokens.
- The Account tab shows your usage and limits, sets whether new pastes are shared by link or private, logs you out everywhere (and can revoke every token at the same time), exports your data and deletes the account. To delete it you type your login.

A paste saved on the web page while you are logged in, or uploaded with one of your API tokens, is yours. It never expires, even when `EXPIRATION` is set, unless you give it an expiry date. It counts against your own limits (`REGISTERED_MAX_LENGTH`, `REGISTERED_UPLOAD_QUOTA`, `USER_STORAGE_LIMIT`, `USER_MAX_PASTES`) instead of the anonymous ones. Pastes from before you logged in stay anonymous and cannot be claimed.

The dashboard API under `/api/v1` needs a login session: every change also needs the `X-CSRF-Token` header from `GET /api/v1/session` and a JSON body. API tokens do not work there; they are for uploads, reads and [MCP](#with-an-api-token). Each user may make 120 reads and 30 changes a minute, create 10 tokens an hour and try to delete the account 3 times a day.

### Private pastes

A paste is shared by link (anyone with the link can open it) or private. A private paste opens only for its owner: with the login cookie on the web page, or with one of their tokens on `/raw`, `/documents` and MCP. Everyone else, other users and MCP without the owner's token included, gets 404, the same answer as for a key that does not exist. `HEAD` and `Range` requests follow the same rule.

You choose the visibility when you save (`X-Paste-Visibility` with curl, `visibility` over MCP), set a default on the Account tab, and change it later in the dashboard.

A paste with an expiry date answers 404 from that moment for everyone, its owner included, and is deleted within a minute.

Views are counted only for reads by someone other than the owner. The dashboard shows the count and when the paste was last opened, never who opened it.

### Editing rules

You can change the text of your paste under the same link for `EDIT_WINDOW` seconds after you save it (10 minutes by default), as long as nobody else has opened it. The first read by someone else on `/raw`, `/documents` or MCP ends that window, so nobody but you ever sees two different texts under one link. A browser that opens the paste page reads `/documents`, so it ends the window too. Link previews in chat apps fetch only the page itself (`/<key>`), which does not. Your own reads never do. While the window is open, the paste page shows you an Edit link with the time left.

After the window, or once someone has opened the paste, a change is saved as a new version with a new link. The new version keeps the title, language, visibility, expiry and tags, and the dashboard links the versions together. You can delete the previous version in the same step. Duplicate makes a plain copy under a new link.

Each change under the same link keeps the older text. That history is for the owner only, in the dashboard; no paste route or MCP tool serves it. A paste keeps at most `EDIT_MAX_REVISIONS` older texts (20); past that, changes go to a new version. Readers see that a paste was changed: "edited at" on the page, `X-Paste-Edited` on `/raw` and `/documents`.

### API tokens and curl

Create a token on the Tokens tab of the dashboard. It looks like `hbp_` and 49 letters and digits, and it is shown once. Send it in the `Authorization` header, never in the URL:

```sh
curl --data-binary @build.log \
  -H "Authorization: Bearer $HASTEBIN_TOKEN" \
  -H "X-Paste-Title: build log" \
  -H "X-Paste-Tags: build,armbian" \
  https://paste.example.com/log
```

The same works on `POST /documents`. These optional headers describe the paste:

| Header | Value |
|---|---|
| `X-Paste-Title` | Up to 200 characters |
| `X-Paste-Tags` | Up to 10 tags, comma separated. A tag is 1 to 32 lowercase letters, digits, `.`, `_` or `-`, starting with a letter or digit. Upper case is lowered, duplicates dropped |
| `X-Paste-Visibility` | `link` or `private`. Without it, your default from the Account tab |
| `X-Paste-Expires` | An RFC 3339 time in the future, such as `2030-01-02T15:04:05Z` |
| `X-Paste-Language` | The language shown in the dashboard. Without it, the extension in `?ext=` (`/log?ext=py`) |

They count only on uploads with a token or a login; anonymous uploads ignore them. A value the server does not accept answers 400 and nothing is stored.

Read your own private paste with the same header:

```sh
curl -H "Authorization: Bearer $HASTEBIN_TOKEN" https://paste.example.com/raw/<key>
```

A token that is malformed, unknown or revoked answers 401 with `WWW-Authenticate: Bearer realm="hastebin", error="invalid_token"`; the request never falls back to anonymous. A token revoked while an upload is still arriving stops it too: the content is deleted and the answer is 401. Over your limits an upload answers 413 (largest paste or storage full), 403 (too many pastes) or 429 (hourly quota). On the web page, a paste is yours only when the page sends the CSRF token along, which it does by itself when you are logged in; a plain cookie on `POST /documents` still makes an anonymous paste.

To download a paste as a file, add `?download=1` to its `/raw` link: `/raw/<key>.log?download=1` saves `<key>.log`.

For tests against a fake GitHub, `GITHUB_OAUTH_BASE` and `GITHUB_API_BASE` (`github_oauth_base`, `github_api_base`) replace `https://github.com` and `https://api.github.com`. They take https, or http only on `localhost` or `127.0.0.1`. Never set them on a real server.

### Admin

The GitHub ids in `ADMIN_GITHUB_IDS` are admins. They get the admin page at `/admin` (also in the account menu), with five tabs. For everyone else `/admin` and its API answer 404. API tokens never act as an admin.

- Moderation starts from a report: you look a paste up by its key or link, there is no list to browse. Showing its content needs a reason. You can delete a paste, or remove it as abuse: then its key answers 410 from then on (404 if it was private) and its owner, if it has one, gets a strike.
- Users lists the accounts with their usage. You can disable an account (its sessions and tokens stop working at once, until you enable it again), log it out everywhere and revoke its tokens, or delete it. Admin accounts cannot be disabled, revoked or deleted from here, your own included.
- Abuse shows recent refusals (rate limits, quotas, key guessing, full disk, busy server) and manual bans. A manual ban blocks one IPv4 address or IPv6 /64 on every route but `/health`, for a set time or until lifted. Manual bans are kept in the database and survive a restart.
- Status shows the version, uptime, disk, memory budget, upload slots, account counts and the settings in use, with secrets shown only as `set` or `unset`.
- Audit lists every admin action and every account deletion, newest first. Rows can be added, never changed or removed.

Destructive actions (deleting, showing content, disabling, revoking, banning) need a GitHub login younger than `ADMIN_REAUTH` seconds (12 hours). With an older one the page sends you through GitHub again and back.

### Backup

Back up the accounts database with:

```sh
hastebin --config config.yaml --backup-db /path/to/hastebin-backup.db
```

It writes a consistent copy with SQLite `VACUUM INTO` and is safe while the server runs and takes uploads. The target file must not exist yet; it gets mode 0600. Copying `hastebin.db` while the server runs is not a backup, as the latest changes are still in `hastebin.db-wal`. In Docker or Pterodactyl, run it in the running container: `docker exec <container> hastebin --config config.yaml --backup-db data/hastebin-backup.db` (in the distroless image the binary is `/bin/hastebin` and the config `/app/config.yaml`). The paste contents are the files in the data folder (or your storage backend): back them up as well.

### Privacy

- Pastes are not stored with an IP address, anonymous or not.
- The request log shows the client address, with paste keys hidden (see key guessing). Rate limits, quotas and the recent refusals on the Abuse tab are kept in memory only, per address. Only manual bans, and their rows in the audit log, store an address in the database.
- An admin can see the metadata of any paste found by its key, and its content only after giving a reason, which goes in the audit log.
- Deleting your account removes your sessions, tokens, pastes and their older texts. The audit log keeps one row with your login name and the time.

### Export

The Account tab downloads your data as one JSON file, from `GET /api/v1/account/export`: your account, your tokens (names, prefixes and times, never the token itself) and the details of every paste (key, title, language, visibility, size, views, times, tags, version and links to the previous and next version). The paste texts are not in it; download them with `/raw/<key>?download=1`.

## Storage backends

Network backends connect at startup. If the connection fails, the server exits (memcached only logs the error).

Only `file` storage streams pastes to and from disk. The others hold whole pastes in memory, which caps `max_length` at 8 MiB for them (see `max_length`), so use `file` storage for large logs.

| Backend | Options | Notes |
|---|---|---|
| `file` | `file_path` | One file per paste, named by the MD5 of the key. At startup the folder is created (parents too) and checked for write access; if that fails, the server exits with a `chown` hint. With `expiration` above 0, a janitor deletes old paste files every 10 minutes (see `expiration`) |
| `redis` | `host`, `port`, `username`, `password` | Uses the default database. Expiration through Redis TTLs |
| `memcached` | `host`, `port` | No authentication. Memcached may evict pastes when it runs out of memory |
| `postgres` | `host`, `port`, `username`, `password`, `database` | Creates the `entries` table on startup. Expired pastes are deleted when someone asks for them |
| `mongodb` | `host`, `port`, `username`, `password`, `database` | Uses the `entries` collection with a TTL index on the expiry time |
| `s3` | `host`, `port`, `username` (access key ID), `password` (secret key), `aws_region`, `bucket`, `tls` | Any S3 compatible endpoint, with path style URLs. Creates the bucket on startup if it does not exist. No expiration: use a bucket lifecycle rule instead |

Example for PostgreSQL:

```yaml
storage:
  type: "postgres"
  host: "db"
  port: 5432
  username: "hastebin"
  password: "change-me"
  database: "hastebin"
```

## Development

```sh
go build -o hastebin ./cmd/main.go
go test ./...
```

The storage tests for Redis, Memcached, MongoDB, PostgreSQL and S3 (MinIO) start containers with testcontainers, so `go test ./...` needs a running Docker. Without Docker, run the rest:

```sh
go test ./config/... ./handler/... ./internal/server/... ./internal/mcpserver/... ./internal/keygenerator/...
go test -run 'TestFileStorage|TestPostgresDSN' ./internal/storage/
```

Images:

```sh
docker build -t ansi-hastebin .                                        # default distroless image
docker build --target pterodactyl -t ansi-hastebin:pterodactyl .       # Alpine image for Wings
```

The default image is `gcr.io/distroless/static:nonroot` running as uid 65532. The `pterodactyl` stage is Alpine with the `container` user (uid 988) that Wings expects, and an entrypoint that runs the egg's startup command.

GitHub Actions builds and tests every push to `main` and every pull request with Go 1.26. A push to `main`, or a pushed release tag `vX.Y.Z`, also publishes both images to `ghcr.io/superkali/ansi-hastebin` for amd64 and arm64. A manual run publishes only from `main`. The actions are pinned to commit SHAs.

| Pushed | Default image | Pterodactyl image |
|---|---|---|
| `main` | `latest` | `pterodactyl`, `pterodactyl-<short sha>` |
| tag `v1.2.3` | `v1.2.3` | `pterodactyl-v1.2.3` |

### Releases

To cut a release, tag a commit on `main` and push the tag:

```sh
git tag v1.2.3
git push origin v1.2.3
```

The tag must look like `vX.Y.Z`. GitHub Actions then publishes `ghcr.io/superkali/ansi-hastebin:v1.2.3` and `ghcr.io/superkali/ansi-hastebin:pterodactyl-v1.2.3`. Release images are never rebuilt, so a server on one keeps the same image until you move it.

Then point the egg at the new release. In `pterodactyl/egg-hastebin-ansi.json`, change the first `docker_images` entry to the new version, in its label (`release v1.1.0`) and in its image (`ghcr.io/superkali/ansi-hastebin:pterodactyl-v1.1.0`). Keep it first, as Pterodactyl uses the first entry as the default, keep `latest (follows main)` second, and update `exported_at`. Commit that, then import the egg again in the panel (Admin > Nests > the egg > Import, or update it in place). Existing servers keep their image until you choose the new one in their Startup tab.

## Credits and license

This is a fork of [armbian/hastebin-ansi](https://github.com/armbian/hastebin-ansi), which is based on [seejohnrun/haste-server](https://github.com/seejohnrun/haste-server) by John Crepezzi.

MIT license, as the original haste-server:

```
Copyright (c) 2011-2012 John Crepezzi

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction, including without limitation the rights to
use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies
of the Software, and to permit persons to whom the Software is furnished to do
so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```
