# Copilot instructions for emly-go-api

Go REST API for the "EMLy" bug-reporting system. Module name is `emly-api-go`
(see `go.mod`); imports are `emly-api-go/internal/...`. Go 1.26.

## Commands

```bash
# Development (hot-reload via air; builds to ./tmp/emly-api.exe)
air

# Build production binary
go build -o ./build/emly-api.exe .

# Run directly
go run .

# One-off: migrate report files from DB blobs to S3 (requires USE_S3_API_FILE_STORAGE=true)
go run . --migrate-files

# Tests
go test ./...
go test ./internal/... -run TestName -v
```

Test coverage of note: the updater self-update contract (routing in
`internal/routes/v2`, manifest wire format + input sanitizing in
`internal/handlers`), the remote-config document (validation, override
matching, canonical form in `internal/remoteconfig`; auth gating + a
nil-DB-safe subset of handlers in `internal/routes/v2` and
`internal/handlers`), the stats event bus (`internal/statshub`), the stats WS
stream (routing + admin-key gating in `internal/routes/v2`; auth gating, the
query-string key fallback, and a full ping/pong handshake over a real
listener in `internal/handlers`), the presence hub (connect/disconnect/
grace-period/supersede semantics, nil-Hub safety, in `internal/presencehub`),
the client ws protocol v2 wire format (envelope framing, ULID
generation/ordering, command argument validation, in `internal/clientproto`),
its in-memory hub (session attach/detach and supersede, command
issue/ack/result lifecycle including nil-Hub safety and the lazy
timeout/prune sweep, the event ring, concurrent notify fan-out, in
`internal/clienthub`), the `GET /v2/client/ws` handshake (identity
conversion, hello/identity/error framing, the v1/v2 branch on
`identity.protocol`, welcome/capability negotiation, command/event dispatch
and rate limiting, and a full identity->presence-online round trip over a
real listener, in `internal/clientws`), its admin routes (issue/get command,
list events, notify, in `internal/clientws`), and the daily log files
(naming, day rotation, retention in `internal/logfile`).

## Architecture

Stack:
- **Router**: `go-chi/chi/v5`
- **Database**: MySQL via `jmoiron/sqlx`
- **Object storage**: two independent S3-compatible buckets via
  `aws-sdk-go-v2` — an API-file bucket (bug report attachments, gated by
  `USE_S3_API_FILE_STORAGE`) and an updates bucket (release installers,
  gated by `USE_S3_UPDATES_STORAGE`). Each has its own `S3BucketConfig`
  (access key, secret, bucket, region, endpoint) and can live on a different
  S3-compatible host/service/provider (Cloudflare R2, MinIO, AWS S3, etc.) —
  `AccountID` is a Cloudflare R2 convenience for deriving the endpoint when
  `Endpoint` is left blank.
- **Observability**: OpenTelemetry (traces + metrics + logs) exported via
  OTLP/HTTP — optional, gated by `OTEL_ENABLED`.
- **Auth**: header API key (`X-API-Key`), admin key (`X-Admin-Key`), session
  tokens for the dashboard, and a rate-limit bypass key (`X-Dashboard-Key`).

### Startup sequence (`main.go`)

1. `godotenv.Load()` then `config.Load()` (singleton via `sync.Once`).
2. If `LOG_FILE_ENABLED`, open today's file via `internal/logfile` and tee the
   console handler's output into it (a failure falls back to console-only
   with a warn line). If `OTEL_ENABLED`, set up OTel. In both modes bridge
   the std `log` package into `slog`, so `log.Fatalf` reaches OTLP and the
   log file.
3. Connect to MySQL, run `schema.Migrate`.
4. For each of `USE_S3_API_FILE_STORAGE` / `USE_S3_UPDATES_STORAGE` that is
   enabled, build + ping that bucket's S3 connector independently (an
   unreachable bucket logs an error and leaves that connector `nil` rather
   than crashing startup).
5. Handle `--migrate-files` CLI flag.
6. Build chi router, apply global middleware, call `routes.RegisterAll`.

### Global middleware order (`main.go`)

RequestID → RealIP → **AccessLog** → Recoverer → Timeout(30s) → **Timing** →
[otelhttp, if enabled] → **RateLimiter**

Custom middleware lives in `internal/middleware/`: `AccessLog`
(`accesslog.go`), `Timing` (`timing.go`, records per-request checkpoints into
a `internal/timing.Timer` on the context), and the two-tier `RateLimiter`
(`ratelimit.ban.go`).

### Routing — versioned (`internal/routes/`)

`routes.RegisterAll` mounts versioned sub-routers and a few root/legacy
paths: `GET /` (ping), `GET /health` (`handlers.Health`), `POST
/api/bug-reports` (legacy alias for v1 bug-report creation), and
`r.Mount("/v1", ...)` / `r.Mount("/v2", ...)`.

Each version's `NewRouter` (`internal/routes/v1/v1.go`, `v2/v2.go`)
re-applies the custom `RateLimiter`, sets `X-Server`/`X-Powered-By` headers,
exposes `GET /health`, and mounts route groups defined in sibling files
(`bug_reports.go`, `admin.go`, and for v2 `updates.go`).

**v1** (`/v1/api/...`) — **deprecated, sunset after 2026-10-31**
(`v1.SunsetDate`). `v1.NewRouter` applies `v1.DeprecationWarning` right after
the rate limiter, logging one **warn** line per request (`deprecated API
version used`) with the caller's method, path, IP, hostname, HWID and
User-Agent — deliberately per-request and unsampled so a rare caller doesn't
go unnoticed until the sunset breaks it. The root-level legacy alias `POST
/api/bug-reports` is wrapped with it too. Every v1 route has a v2
equivalent.
- `bug-reports`: API-key-only group (`POST /`, `GET /count`) and API-key +
  admin-key group (full CRUD, `{id}/status`, `{id}/files`, `{id}/download`).
- `admin/auth`: session login/validate/logout (`/login` rate-limited;
  `/validate` + `/logout` require a session token).
- `admin/users`: admin-key-protected user CRUD + password reset.

**v2** (`/v2/...`): everything in v1 plus:
- `updates/`: public update manifest + release download, admin-key-protected
  release management (`update_releases` table). Also carries the EMLy
  Updater's **self-update** surface (`updater.route.go`, `updater_releases`
  table): API-key-protected manifest (`GET /manifest/updater`), public
  installer download (`GET /download/updater/{version}`), admin-key-protected
  release management (`updater/releases`).
- `config/` (`config.route.go`, `remote_config_revisions` table): the
  fleet-wide policy document served to the EMLy Updater and EMLy —
  API-key-protected `GET /config`, admin-key-protected
  `validate`/`preview`/`revisions` (list/get/create/delete)/
  `revisions/{revision}/publish`/`rollback`. See
  `docs/superpowers/specs/2026-09-04-remote-config-api-design.md`.
- `bans/` (`bans.route.go`, `bans` table): admin-key-protected
  `GET`/`POST`/`DELETE {id}` over the permanent block list enforced by
  `middleware.BanList`.
- `stats/` (`stats.route.go`): admin-key-protected
  `summary`/`clients`/`clients/{id}`/`events`, plus their real-time
  counterpart `GET /stats/stream` (`stats_stream.route.go`) — a WebSocket
  upgrade checking `X-Admin-Key` itself (with a `?admin_key=` query fallback)
  before completing the handshake, pushing `stats:summary`/`stats:clients`/
  `stats:events` snapshots on ingestion and a periodic tick. See
  `docs/superpowers/specs/2026-09-04-websocket-stats-stream-design.md`.
- `client/` (`client.go`, `internal/handlers/client_ws.route.go`): an
  API-key-protected `GET /v2/client/ws` the EMLy Updater holds open for the
  service lifetime — a `hello`/`identity` handshake reusing the same
  `clientIdentity`/`upsertUpdaterClient` machinery as manifest/download, then
  a 10s server-ping heartbeat. Presence tracked in-memory
  (`internal/presencehub`), surfaced as `online` on `GET /v2/stats/clients`
  and the `stats:clients` WS channel. See
  `docs/superpowers/specs/2026-09-17-client-presence-ws-api-design.md`.
  Gated client-side by the `clientWs.enabled` remote-config kill switch; the
  API itself does not refuse a connection when that flag is false.

### Rate limiting — two layers

1. **Custom `RateLimiter`** (`ratelimit.ban.go`), applied globally and
   per-version-router. Two tiers keyed by IP: *unauthenticated* (no
   `X-API-Key`/`X-Admin-Key`, `RL_UNAUTH_*` env) and *authenticated*
   (`RL_AUTH_*` env). Tracks request counts per window and **bans** an IP
   (in-memory `sync.Map`) after `MaxFails` window-violations for `BanDur`.
   Private/loopback IPs and requests bearing a valid `X-Dashboard-Key`
   bypass it entirely. A goroutine prunes stale visitor + ban entries every
   10 min.
2. **`apimw.RouteLimitByIP(30, time.Minute)`** (`ratelimit.route.go`)
   applied per route group inside v1/v2, and to the WebSocket stack in
   `main.go`. It is `httprate.LimitByIP` plus the same `X-Dashboard-Key`
   exemption layer 1 has, so the two agree on who is exempt. **Never mount
   `httprate.LimitByIP` directly** — a route group that does becomes a
   second, invisible ceiling the dashboard hits while the global limiter
   waves it through (the dashboard renders server-side, so its whole staff
   shares one source address). With `DASHBOARD_KEY` unset the exemption is
   off.

### Package layout

- `internal/config/` — Singleton `Config` loaded from env vars
  (`Load()`/`once`). Panics if `DATABASE_NAME` (validated against
  `^[a-zA-Z0-9_]+$` to prevent SQL injection — it is interpolated into
  queries) or `DB_DSN` is missing. `API_KEY`/`ADMIN_KEY` accept a
  comma-separated list but only the first non-empty value is used.
- `internal/database/` — MySQL pool setup with configurable limits.
- `internal/database/schema/` — Conditional migrator (see below).
- `internal/handlers/` — Factory functions returning `http.HandlerFunc`,
  named `<resource>.route.go`. Response helpers (`jsonOK`, `jsonCreated`,
  `jsonError`) in `response.go`.
- `internal/middleware/` — Auth (`apikey.go`, `adminKey.go`) and rate
  limiting. Auth middleware load allowed keys into a map at construction for
  O(1) lookup; they take a `*sqlx.DB` arg that is currently unused (keys
  come from config).
- `internal/storage/` — `S3Connector` wrapping an S3-compatible bucket
  (upload/download/list/delete/rename, folder helpers) and
  `migrateFiles.go`. `NewS3Connector` is provider-agnostic; `main.go`
  constructs one instance per bucket (API files, updates) from their
  respective `config.S3BucketConfig`.
- `internal/logfile/` — HTTP- and DB-free `io.Writer` over one log file per
  day, `emly-api-log-YYYY-MM-DD-HH-mm-ss.log` (`-`, not `:`, since `:` is
  illegal in Windows file names). Rotates on the first write of a new
  local-time day (a restart also opens a new file), and prunes files older
  than `LOG_RETENTION_DAYS` whenever it opens one, touching only names it
  generates. `main.go` puts it behind `io.MultiWriter(console, file)` —
  console first, so a full disk cannot silence the console.
- `internal/telemetry/` — OTel provider setup (trace/metric/log exporters,
  W3C propagators).
- `internal/timing/` — Per-request timing checkpoints carried on the
  context.
- `internal/models/` — Structs with `db:` and `json:` tags. Sensitive
  fields use `json:"-"`.
- `internal/remoteconfig/` — HTTP- and DB-free: the `/v2/config` document
  type, `Parse` (validation, same rules the EMLy Updater client applies),
  `Match`/`Effective`/`ResolveSite` (override evaluation), `Canonical`
  (deterministic serialization + `ETag`). No dependency on
  `internal/handlers` or `internal/models`; `internal/handlers/config.route.go`
  and `internal/configmirror` are its only callers.
- `internal/configmirror/` — Background loop for a site mirror
  (`CONFIG_UPSTREAM_URL` set): polls upstream `/v2/config`, validates with
  `remoteconfig.Parse`, stores the bytes as received under the upstream's
  own revision/etag. No-op when `CONFIG_UPSTREAM_URL` is empty.
- `internal/statshub/` — HTTP- and DB-free like `internal/remoteconfig`: an
  in-process `Hub` (pub/sub, `Subscribe`/`Publish`/`Active`) fanning
  updater-event and periodic-tick notifications out to every open `GET
  /v2/stats/stream` connection. **Single-instance only by design** (no
  Postgres LISTEN/NOTIFY or Redis in this stack).
- `internal/presencehub/` — HTTP- and DB-free like `internal/statshub`: an
  in-process `Hub` tracking which EMLy Updater clients currently hold an
  open `GET /v2/client/ws` connection, with a short grace period on
  disconnect so a brief drop/reconnect never flickers "offline".
  Single-instance only, same limit as `statshub`.
- `internal/clientproto/` — HTTP- and DB-free: the wire format of protocol
  v2 of `GET /v2/client/ws` (`CLIENT_WS_PROTOCOL.md`) — the `Envelope`,
  `Command`/`Ack`/`Result`/`Event`/`Notify` payload types, command/event/
  notify name and error-code constants, `ValidateArgs`, `NewID` (ULID).
  `id.go` is copied verbatim into `emly-updater/internal/wsclient/id.go`
  (package name aside) so both ends mint identically-shaped IDs — the two
  repos share no Go module, so nothing enforces that automatically.
- `internal/clienthub/` — HTTP- and DB-free like `internal/presencehub`: an
  in-process `Hub` holding protocol v2's session/command/event state — which
  client has a v2 connection open and what it declared it can do, the
  commands issued to machines and their outcome, and a 50-entry ring of each
  machine's recent events. Nil-receiver safe throughout, same as
  `presencehub`. Single-instance only, same limit as `statshub`/
  `presencehub`; a ticker in `main.go` prunes finished commands/events older
  than 24h every 10 minutes.

### Handler conventions

- **Permanent bans are a separate mechanism from the rate limiter's** —
  `internal/middleware/ban.go` (`BanList`) enforces the operator-set `bans`
  table: one row per identifier (`ip` / `hwid` / `hostname`), no expiry,
  removed by hand. The rate limiter's `banned` map stays automatic and
  time-boxed. The list is cached in memory and refreshed both on a 30s
  ticker and immediately after an admin write (`Reload`); a refresh failure
  **keeps the previous snapshot**. It runs **ahead of the rate limiter** in
  `main.go` (cheaper: an RLock + three map lookups). **A valid
  `X-Admin-Key` is exempt** (otherwise banning the office IP locks out the
  route that removes the ban). Hostnames are stored/matched lower-cased,
  IPs are canonicalised through `net.ParseIP`, HWIDs are matched
  byte-for-byte.
- **The EMLy Updater's identity is built in exactly two places, and a new
  field must be added to both**: `clientIdentityFromRequest`
  (`updates.route.go`) builds a `clientIdentity` from the `X-EMLy-*`
  headers — `Hostname`, `HWID`, `ADDomain`, `LoggedUser`,
  `LoggedUserState`, `LoggedUserDisconnectedAt`, `Serial`, `Product`,
  `OSVersion`, `AppVersion` — plus User-Agent version/contact and peer IP;
  both telemetry paths (`recordUpdaterEvent`, `trackConfigFetch`) go
  through it into `upsertUpdaterClient`. `clientIdentityFromWSPayload`
  (`client_ws.route.go`) is the second, sourced from the `GET
  /v2/client/ws` "identity" JSON message instead of headers, feeding the
  same `upsertUpdaterClient`. A field added to only one constructor silently
  stays NULL for the other path. A request carrying neither HWID nor
  hostname is served but not tracked. **A header the client does not send
  never clears the stored value** (the `COALESCE(NULLIF(?, ''), col)`
  pairs in the upsert) — "absent" means "unknown". **Except the logged-on
  user from updater 1.6.2+** (`reportsNobodyLoggedOn`, keyed off the
  User-Agent version): that build always sends user and state together and
  omits both only when nobody is logged on, so its silence clears
  `logged_user`, `logged_user_state` and `logged_user_disconnected_at`.
  `logged_user` is a snapshot to be read against `last_seen_at`, not a
  history. `logged_user_disconnected_at` follows `logged_user_state` rather
  than its own header: any request carrying a state rewrites it (to NULL
  for a non-disconnected session). `parseLoggedUserSession` only accepts
  `active-console`/`active-rdp`/`disconnected` and drops the timestamp for
  the other two.
- Handlers are factory functions: `func CreateBugReport(db *sqlx.DB, dbName
  string, s3conn *storage.S3Connector) http.HandlerFunc { return func(w, r)
  { ... } }`. Dependencies are injected at construction.
- All responses are JSON via `jsonOK` / `jsonCreated` / `jsonError`.
- Use the request context (`r.Context()`) for DB calls (`SelectContext`,
  `GetContext`) and `slog.*Context` logging so traces/spans propagate.
- File uploads use `r.ParseMultipartForm(32 << 20)`; close file streams
  explicitly.
- ZIP downloads: in-memory `archive/zip` with template-rendered report text
  via `internal/handlers/templates/report.txt.tmpl`.
- Installer downloads stream through `streamInstaller` (`download.go`),
  never a bare `io.Copy`: once the 200 and `Content-Length` are on the wire
  nothing can be turned into an HTTP error, so a short copy is reported as a
  **warn** log line (`installer download did not complete`) with the reason
  (`server timeout` / `client disconnected` / `copy failed`), bytes
  sent/expected, average throughput, client hostname/HWID/IP. `server
  timeout` means the global `chiMiddleware.Timeout(30s)` cut a transfer in
  half.
- net/http's own soft errors (superfluous `WriteHeader`, TLS handshake
  failures, malformed request lines) go to `srv.ErrorLog` in `main.go`,
  wired to a `logBridge` at **warn** level (used to surface as INFO).
  Anything else reaching the std `log` package still bridges at info.
- The updater self-update manifest answers 200 in every non-error case: an
  empty catalogue serializes as `{"version": ""}`. **Never return 404
  there** — the client reads 404 as "this mirror doesn't implement the
  endpoint" and stops without retrying. At most one `updater_releases` row
  holds `is_current`; clearing it everywhere is the kill-switch.
- Same reasoning for `GET /v2/config`: **never return 404** for "nothing
  published yet" — answer **204** instead.
- `GET /v2/client/ws`'s envelope `type` field is deliberately open-ended. v1
  is `hello`/`identity`/`ping`/`pong`/`error`; protocol v2
  (`identity.protocol >= 2`) adds `welcome`, `command`, `ack`, `result`,
  `event`, `notify` (`internal/clientproto`; `CLIENT_WS_PROTOCOL.md` is the
  normative wire format, mirrored by hand in `emly-updater/internal/wsclient`
  — no shared Go module between the two repos). An unrecognized `type` on
  either side is still logged and ignored rather than closing the
  connection. That tolerance stops at the command level: an
  unsupported/unknown command `name` is explicitly **refused** with `ack`
  (`unsupported_command`), not silently ignored.
- `GET /v2/stats/summary` memoizes its payload behind a `ttlCache`
  (`statscache.go`), keyed by `product|window_minutes`, sized by
  `STATS_CACHE_TTL`. Concurrent callers on a cold key collapse into one
  build. Anything added to that summary belongs in `buildStatsSummary`,
  behind the cache — not in the handler.
- Update releases have independent `is_stable`/`is_beta` boolean flags (a
  release can be both; setting either to `true` clears that flag from
  whichever other release previously held it) and validate
  `severity_type` against `validSeverity`
  (`none`/`security`/`bugfix`/`feature`).
- Remote-config revisions are append-only:
  `remote_config_revisions.document` never changes after insert. Publishing
  supersedes the previous `published` row in the same transaction; rolling
  back clones an old revision's content into a **new**, higher-numbered
  revision rather than republishing the old one. `POST
  /v2/config/revisions`, `/revisions/{revision}/publish`, `/rollback` and
  `DELETE /revisions/{revision}` all answer `405` on a site mirror
  (`CONFIG_UPSTREAM_URL` set).

### Database migrations

`internal/database/schema/migrator.go` runs on startup:
1. Executes `init.sql` to ensure base tables exist.
2. Reads `migrations/tasks.json` for conditional tasks.
3. For each task, checks its condition against the live DB before running
   the corresponding `migrations/*.sql`.

Supported condition types: `column_not_exists`, `column_exists`,
`index_not_exists`, `index_exists`, `table_not_exists`, `table_exists`.

## Environment

Copy `.env.example` to `.env`. Required: `DB_DSN`, `DATABASE_NAME`,
`API_KEY`, `ADMIN_KEY`. `DB_DSN` must include `parseTime=true&loc=UTC`:
```
DB_DSN=root:secret@tcp(127.0.0.1:3306)/emly?parseTime=true&loc=UTC
```

Other notable vars (see `.env.example` for the full list + defaults):
- DB pool: `DB_MAX_OPEN_CONNS`, `DB_MAX_IDLE_CONNS`, `DB_CONN_MAX_LIFETIME`
- Logging: `LOG_LEVEL` (`debug`/`info`/`warn`/`error`, default `info`),
  `LOG_FILE_ENABLED` (default `true`), `LOG_DIR` (default `logs`; `/logs` in
  `docker-compose.yml`), `LOG_RETENTION_DAYS` (default `30`, `0` keeps
  everything)
- Auth extras: `DASHBOARD_KEY` (bypasses both rate-limit layers)
- Rate limiting: `RL_UNAUTH_*` and `RL_AUTH_*` (`MAX_REQS`, `WINDOW`,
  `MAX_FAILS`, `BAN_DUR`)
- Storage — API file bucket: `USE_S3_API_FILE_STORAGE`,
  `S3_API_FILE_ACCESS_KEY_ID`, `S3_API_FILE_SECRET_ACCESS_KEY`,
  `S3_API_FILE_BUCKET`, `S3_API_FILE_REGION`, `S3_API_FILE_ENDPOINT`,
  `S3_API_FILE_ACCOUNT_ID` (optional, R2 endpoint shortcut)
- Storage — updates bucket: `USE_S3_UPDATES_STORAGE`,
  `S3_UPDATES_ACCESS_KEY_ID`, `S3_UPDATES_SECRET_ACCESS_KEY`,
  `S3_UPDATES_BUCKET`, `S3_UPDATES_REGION`, `S3_UPDATES_ENDPOINT`,
  `S3_UPDATES_ACCOUNT_ID` (optional). The two buckets are fully independent
  and may sit on different S3-compatible providers.
- Telemetry: `OTEL_ENABLED`, `OTEL_ENDPOINT`
- Updates: `UPDATES_ENABLED`, `S3_UPDATES_PREFIX` (manifest download links
  are built from the request's `Host`/`X-Forwarded-*` headers, not an env
  var), `S3_UPDATER_PREFIX` (default `updater`)
- Remote config: `CONFIG_UPSTREAM_URL` (empty on primary; set on a site
  mirror), `CONFIG_UPSTREAM_INTERVAL` (default `5m`),
  `CONFIG_UPSTREAM_API_KEY` (defaults to this instance's own `API_KEY`)
- Real-time stats: `STATS_STREAM_TICK_INTERVAL` (default `30s`)
- Fleet stats: `STATS_CACHE_TTL` (default `30s`, `0` disables the cache)

### Adding new environment variables

When you add a var to `internal/config/config.go`, update both of these in
the same commit:
1. **`.env.example`** — add it with a sensible default/placeholder and a
   comment.
2. **`docker-compose.yml`** — add it under `services.api.environment` using
   `${VAR_NAME:-default}` syntax.

## Documentation upkeep

Two documents at the repo root describe the API to humans and must be kept
current — they belong in the same commit as the change, not a follow-up.

### `DOCS.md` — after any major architectural change

Written in **Italian** for developers coming from Node.js
(Express/Fastify) or PHP (Laravel/Slim) who have never written Go — every
concept introduced by analogy to those stacks (`r.Use(...)` ~
`app.use(...)`, `sqlx` ~ Eloquent/Knex, `struct` ~ a TypeScript interface,
etc.). Keep that voice.

Update it whenever a change would make a section wrong or incomplete: a new
package under `internal/`, a new middleware or a change to the global
middleware order, a new auth mechanism, a change in how handlers are
constructed/wired, a new external dependency (another S3 bucket, another
backing service), a change to the migration mechanism, or a new route
group. A bug fix inside an existing handler does not require a `DOCS.md`
change.

### `ROUTES.md` — whenever routes change

The endpoint-by-endpoint reference (Italian, same audience): every route
with its auth requirement, query/body parameters and behaviour. Adding,
removing or renaming a route, changing its auth gating, accepted
parameters or status codes means updating the matching table and prose
there in the same commit.
