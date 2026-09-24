# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

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

# Tests live next to the code they test, one _test.go per package - that is
# what gives them access to the unexported identifiers most of them exercise.
# Coverage: the updater self-update contract (routing in internal/routes/v2,
# manifest wire format + input sanitizing in internal/updates), the
# remote-config document (validation, override matching, canonical form in
# internal/remoteconfig; auth gating + a nil-DB-safe subset of handlers in
# internal/routes/v2), the client identity conversion from both wire formats
# (internal/updaterclient), the payload memoizer (internal/ttlcache), the
# stats event bus (internal/statshub), the stats WS stream (routing +
# admin-key gating in internal/routes/v2; auth gating, the query-string key
# fallback, a full ping/pong handshake over a real listener, and the
# coalescing state that keeps it off the database on every event, in
# internal/stats), the presence hub (connect/disconnect/grace-period/
# supersede semantics, nil-Hub safety, in internal/presencehub), the client ws
# protocol v2 wire format (envelope framing, ULID generation/ordering, command
# argument validation in internal/clientproto), its in-memory hub (session
# attach/detach and generation-guarded supersede, command issue/ack/result
# lifecycle including nil-Hub safety and the lazy timeout/prune sweep, the
# event ring, concurrent fan-out in internal/clienthub), the GET /v2/client/ws
# handshake (v1 hello/identity/error framing, the v1/v2 branch on
# identity.protocol, welcome/capability negotiation, command/event dispatch
# and the rate limiter, and a full identity->presence-online round trip over a
# real listener, in internal/clientws), the client ws admin routes (issue/get
# command, list events, notify, in internal/clientws), the daily log files
# (naming, day rotation, retention in internal/logfile) and the raw-event
# retention loop's disabled path (internal/eventprune)
go test ./...
go test ./internal/... -run TestName -v
```

Module name is `emly-api-go` (see `go.mod`); imports are `emly-api-go/internal/...`. Go 1.26.

## Architecture

Go REST API for the "EMLy" bug-reporting system. Stack:
- **Router**: `go-chi/chi/v5`
- **Database**: MySQL via `jmoiron/sqlx`
- **Object storage**: two independent S3-compatible buckets via `aws-sdk-go-v2` — an API-file bucket (bug report attachments, gated by `USE_S3_API_FILE_STORAGE`) and an updates bucket (release installers, gated by `USE_S3_UPDATES_STORAGE`). Each has its own `S3BucketConfig` (access key, secret, bucket, region, endpoint) and can live on a different S3-compatible host/service/provider (Cloudflare R2, MinIO, AWS S3, etc.) — an `AccountID` field is a Cloudflare R2 convenience for deriving the endpoint when `Endpoint` is left blank.
- **Observability**: OpenTelemetry (traces + metrics + logs) exported via OTLP/HTTP — optional, gated by `OTEL_ENABLED`
- **Auth**: header API key (`X-API-Key`), admin key (`X-Admin-Key`), session tokens for the dashboard, and a rate-limit bypass key (`X-Dashboard-Key`)

### Startup sequence (`main.go`)

1. `godotenv.Load()` then `config.Load()` (singleton via `sync.Once`).
2. If `LOG_FILE_ENABLED`, open today's file via `internal/logfile` and tee the console handler's output into it (a failure falls back to console-only with a warn line). If `OTEL_ENABLED`, set up OTel. In both modes bridge the std `log` package into `slog`, so `log.Fatalf` reaches OTLP and the log file.
3. Connect to MySQL, run `schema.Migrate`.
4. For each of `USE_S3_API_FILE_STORAGE` / `USE_S3_UPDATES_STORAGE` that is enabled, build + ping that bucket's S3 connector independently (an unreachable bucket logs an error and leaves that connector `nil` rather than crashing startup).
5. Handle `--migrate-files` CLI flag.
6. Build chi router, apply global middleware, call `routes.RegisterAll`.

### Global middleware order (`main.go`)

RequestID → RealIP → **AccessLog** → Recoverer → Timeout(30s) → **Timing** → [otelhttp, if enabled] → **RateLimiter**

The custom middleware live in `internal/middleware/`: `AccessLog` (`accesslog.go`), `Timing` (`timing.go`, records per-request checkpoints into a `internal/timing.Timer` on the context), and the two-tier `RateLimiter` (`ratelimit.ban.go`).

### Routing — versioned (`internal/routes/`)

`routes.RegisterAll` mounts versioned sub-routers and a few root/legacy paths:
- `GET /` → ping (`emly-api-go`)
- `GET /health` → `health.Health`
- `POST /api/bug-reports` → legacy alias for v1 bug-report creation
- `r.Mount("/v1", v1.NewRouter(...))` and `r.Mount("/v2", v2.NewRouter(...))`

Each version's `NewRouter` (in `internal/routes/v1/v1.go`, `v2/v2.go`) re-applies the custom `RateLimiter`, sets `X-Server`/`X-Powered-By` headers and exposes `GET /health`.

**Each feature package registers its own v2 routes.** `v2.NewRouter` is a mount table and nothing else: it calls `updates.RegisterV2`, `stats.RegisterV2`, `configapi.RegisterV2`, `bans.RegisterV2`, `clientws.RegisterV2`, `admin.RegisterV2`, `bugreports.RegisterV2`, each defined in that feature's own `routes.go` next to the handlers it mounts. A new endpoint is therefore one file's worth of change - handler and route in the same package - not a handler here and a registration in a router package over there. **v1 is the deliberate exception**: its route files stay in `internal/routes/v1/`, calling the same exported handler constructors. It is frozen and sunsets 2026-10-31, so it is not worth giving a home inside the feature packages only to delete it weeks later.

**v1** (`/v1/api/...`) — **deprecated, sunset after 2026-10-31** (`v1.SunsetDate`). `v1.NewRouter` applies `v1.DeprecationWarning` right after the rate limiter, logging one **warn** line per request (`deprecated API version used`) with the caller's method, path, IP, hostname, HWID and User-Agent. The root-level legacy alias `POST /api/bug-reports` is wrapped with it too, since it is a v1 route mounted outside the v1 router. The line is per-request and not sampled on purpose: a sampled warning would hide the rare caller nobody remembers deploying until the sunset breaks it, and the noise ends when `/v1` does. Every v1 route has a v2 equivalent.

- `bug-reports`: API-key-only group (`POST /`, `GET /count`) and API-key + admin-key group (full CRUD, `{id}/status`, `{id}/files`, `{id}/download`, etc.).
- `admin/auth`: session login/validate/logout (`/login` is rate-limited; `/validate` + `/logout` require a session token).
- `admin/users`: admin-key-protected user CRUD + password reset.

**v2** (`/v2/...`): everything in v1 plus `updates/` — public update manifest + release download, and admin-key-protected release management (`update_releases` table). The same `updates/` group also carries the EMLy Updater's **self-update** surface (`updater.route.go`, `updater_releases` table): an API-key-protected manifest (`GET /manifest/updater`), a public installer download (`GET /download/updater/{version}`), and admin-key-protected release management (`updater/releases`). v2 also carries `config/` (`config.route.go`, `remote_config_revisions` table) — the fleet-wide policy document served to the EMLy Updater and EMLy: an API-key-protected `GET /config`, and admin-key-protected `validate`/`preview`/`revisions` (list/get/create/delete)/`revisions/{revision}/publish`/`rollback`. See `docs/superpowers/specs/2026-09-04-remote-config-api-design.md`. v2 also carries `bans/` (`bans.route.go`, `bans` table): admin-key-protected `GET`/`POST`/`DELETE {id}` over the permanent block list enforced by `middleware.BanList`. v2 also carries `stats/` (`stats.route.go`): admin-key-protected `summary`/`clients`/`clients/{id}`/`events`, plus their real-time counterpart `GET /stats/stream` (`stats_stream.route.go`) — a WebSocket upgrade that checks `X-Admin-Key` itself (with a `?admin_key=` query fallback) before completing the handshake, then pushes `stats:summary`/`stats:clients`/`stats:events` snapshots and updates as `updater_events` are ingested and on a periodic tick. See `docs/superpowers/specs/2026-09-04-websocket-stats-stream-design.md`. v2 also carries `client/` (`internal/clientws`): an API-key-protected `GET /v2/client/ws` the EMLy Updater holds open for the lifetime of the service — a `hello`/`identity` handshake reusing the same `updaterclient.Identity`/`updaterclient.Upsert` machinery as manifest/download, then a 10s server-ping heartbeat. Presence is tracked in-memory (`internal/presencehub`) and surfaced as `online` on `GET /v2/stats/clients` and the `stats:clients` WS channel. See `docs/superpowers/specs/2026-09-17-client-presence-ws-api-design.md`. Gated client-side by the `clientWs.enabled` kill switch in the remote-config document; the API does not itself refuse a connection when that flag is false.

### Rate limiting — two layers

1. **Custom `RateLimiter`** (`ratelimit.ban.go`), applied globally and per-version-router. Two tiers keyed by IP: *unauthenticated* (no `X-API-Key`/`X-Admin-Key`, `RL_UNAUTH_*` env) and *authenticated* (`RL_AUTH_*` env). Tracks request counts per window and **bans** an IP (in-memory `sync.Map`) after `MaxFails` window-violations for `BanDur`. Private/loopback IPs and requests bearing a valid `X-Dashboard-Key` bypass it entirely. A goroutine prunes stale visitor + ban entries every 10 min.
2. **`apimw.RouteLimitByIP(30, time.Minute)`** (`ratelimit.route.go`) applied per route group inside v1/v2, and to the WebSocket stack in `main.go`. It is `httprate.LimitByIP` plus the same `X-Dashboard-Key` exemption layer 1 has, so the two agree on who is exempt. **Never mount `httprate.LimitByIP` directly** — a route group that does becomes a second, invisible ceiling the dashboard hits while the global limiter waves it through. The exemption is there because the dashboard renders every page server-side: its whole staff shares one source address, so a per-IP budget sized for one caller gets split among all of them. Holding the key is equivalent to holding the admin key in blast radius and it never leaves the dashboard's server. With `DASHBOARD_KEY` unset the exemption is simply off.

### Package layout

- `internal/config/` — Singleton `Config` loaded from env vars (`Load()`/`once`). Panics if `DATABASE_NAME` (validated against `^[a-zA-Z0-9_]+$` to prevent SQL injection — it is interpolated into queries) or `DB_DSN` is missing. `API_KEY`/`ADMIN_KEY` accept a comma-separated list but only the first non-empty value is used.
- `internal/database/` — MySQL pool setup with configurable limits.
- `internal/database/schema/` — Conditional migrator (see below).
**Feature packages.** One package per feature, each owning its handlers (`<resource>.route.go`, factory functions returning `http.HandlerFunc`), its v2 route registration (`routes.go`, `RegisterV2`) and its own tests. There is no shared `handlers` package: a change to one feature touches one directory.
- `internal/bugreports/` — `/bug-reports` CRUD, multipart upload, the ZIP download and its `templates/report.txt.tmpl`.
- `internal/admin/` — `admin/auth` (login/validate/logout, password hashing) and `admin/users` (user CRUD + password reset).
- `internal/updates/` — EMLy release manifest/download/management **and** the EMLy Updater's own self-update surface (`updater.route.go`), which share the updates bucket, `streamInstaller` (`download.go`) and the release-flag rules.
- `internal/configapi/` — the `/v2/config` HTTP surface: document serving plus revision validate/preview/create/publish/rollback/delete. Distinct from `internal/remoteconfig`, which is the HTTP-free document itself.
- `internal/stats/` — `/v2/stats/*` REST **and** its real-time counterpart `/v2/stats/stream`, which share the fetchers (`fetchStatsSummary`, `fetchStatsEvents`, `decorateOnline`) so a polled figure and a pushed one can never disagree.
- `internal/clientws/` — `GET /v2/client/ws`, the connection the updater holds open for the lifetime of the service.
- `internal/bans/` — the `bans` table's admin surface, enforced by `middleware.BanList`.
- `internal/health/` — `GET /health`, plus the `ConfigMirrorReporter` interface a site mirror's state is reported through.

**Shared leaf packages** — imported by features, importing none of them. A helper that two features need belongs in one of these, never in a sibling feature:
- `internal/response/` — `OK`, `Created`, `Error`: the JSON response shape, one decision in one place.
- `internal/dbvalue/` — `NullString`/`NullTime` (empty and zero become SQL NULL), `Deref`, `Truncate` (rune-counted, so a column sized in characters is never overrun).
- `internal/ttlcache/` — the generic payload memoizer behind the polled stats endpoints.
- `internal/session/` — `Header` and `Username`: who the dashboard user behind a session token is. Best-effort attribution, so a request with only an admin key is attributed to nobody rather than rejected. `admin` issues these tokens; `bans` and `configapi` only read them.
- `internal/updaterclient/` — see below; the EMLy Updater's identity and telemetry.
- `internal/middleware/` — Auth (`apikey.go`, `adminKey.go`) and rate limiting. Auth middleware load allowed keys into a map at construction for O(1) lookup; they take a `*sqlx.DB` arg that is currently unused (keys come from config).
- `internal/storage/` — `S3Connector` wrapping an S3-compatible bucket (upload/download/list/delete/rename, folder helpers) and `migrateFiles.go`. `NewS3Connector` is provider-agnostic; `main.go` constructs one instance per bucket (API files, updates) from their respective `config.S3BucketConfig`.
- `internal/logfile/` — HTTP- and DB-free `io.Writer` over one log file per day, `emly-api-log-YYYY-MM-DD-HH-mm-ss.log` named after the moment it was opened (`-`, not `:`, because `:` is illegal in Windows file names). Rotates on the first write of a new local-time day, and a restart opens a new file too; prunes files older than `LOG_RETENTION_DAYS` whenever it opens one, touching only names it generates. `main.go` is its only caller and puts it behind `io.MultiWriter(console, file)` — console first, so a full disk cannot silence the console. The Docker entrypoint `exec`s the binary instead of `tee`-ing into `/logs/app.log`.
- `internal/telemetry/` — OTel provider setup (trace/metric/log exporters, W3C propagators).
- `internal/timing/` — Per-request timing checkpoints carried on the context.
- `internal/models/` — Structs with `db:` and `json:` tags. Sensitive fields use `json:"-"`.
- `internal/remoteconfig/` — HTTP- and DB-free: the `/v2/config` document type, `Parse` (validation, same rules the EMLy Updater client applies), `Match`/`Effective`/`ResolveSite` (override evaluation), `Canonical` (deterministic serialization + `ETag`). No dependency on any feature package or on `internal/models`; `internal/configapi` and `internal/configmirror` are its only callers.
- `internal/configmirror/` — Background loop for a site mirror (`CONFIG_UPSTREAM_URL` set): polls upstream `/v2/config`, validates with `remoteconfig.Parse`, and stores the bytes as received under the upstream's own revision/etag. A no-op `State` (nothing started) when `CONFIG_UPSTREAM_URL` is empty.
- `internal/statshub/` — HTTP- and DB-free like `internal/remoteconfig`: an in-process `Hub` (pub/sub, `Subscribe`/`Publish`/`Active`) that fans updater-event and periodic-tick notifications out to every open `GET /v2/stats/stream` connection. Single-instance only by design (no Postgres LISTEN/NOTIFY or Redis in this stack) — see the package doc. `internal/updaterclient` (`RecordEvent`) and `internal/stats` (`stats_stream.route.go`) are its only callers; `main.go` constructs the one `Hub` and runs its tick loop.
- `internal/eventprune/` — HTTP-free background loop (like `internal/configmirror`) that keeps `updater_events` bounded: every hour it deletes raw rows older than `EVENTS_RETENTION_DAYS`, by primary key in batches of 5000 (no index starts with `created_at`, so a `WHERE created_at < ?` batch loop would full-scan per batch). Safe to delete because no aggregate reads raw rows any more — `/v2/stats/summary` and `/v2/stats/events` are served from the `updater_event_hourly` rollup, which is never pruned. `main.go` is its only caller.
- `internal/presencehub/` — HTTP- and DB-free like `internal/statshub`: an in-process `Hub` tracking which EMLy Updater clients currently hold an open `GET /v2/client/ws` connection, with a short grace period on disconnect so a brief drop/reconnect never flickers "offline". Single-instance only, same limit as `statshub`. `internal/clientws` (`ClientWS`) and `internal/stats` (the `online` decoration on `GET /v2/stats/clients` / the `stats:clients` WS channel) are its only callers; `main.go` constructs the one `Hub`.
- `internal/clientproto/` — HTTP- and DB-free: the wire format of protocol v2 of `GET /v2/client/ws` (`CLIENT_WS_PROTOCOL.md`) — the `Envelope`, `Command`/`Ack`/`Result`/`Event`/`Notify` payload types, the command/event/notify name and error-code constants, `ValidateArgs`, `NewID` (ULID). No dependency on any feature package; `internal/clientws` and `internal/clienthub` are its only callers. `id.go` is copied verbatim into `emly-updater/internal/wsclient/id.go` (package name aside) so both ends mint identically-shaped IDs — the two repos share no Go module, so nothing enforces that automatically.
- `internal/clienthub/` — HTTP- and DB-free like `internal/presencehub`: an in-process `Hub` holding protocol v2's session/command/event state — which `updater_clients.id` has a v2 connection open and what it declared it can do, the commands issued to machines and their outcome (`Issue`/`HandleAck`/`HandleResult`/`CompleteByRestart`), and a 50-entry ring of each machine's recent events. Nil-receiver safe throughout, same as `presencehub`. Single-instance only, same limit as `statshub`/`presencehub` — a command issued on one API replica only reaches a machine connected to that replica, and nothing survives a restart; a ticker in `main.go` prunes finished commands/events older than 24h every 10 minutes. `internal/clientws` is its only caller; `main.go` constructs the one `Hub` and passes it through `internal/routes` as `configapi.ConfigNotifier` too (`NotifyConfigPublished`).

### Handler conventions

- **Permanent bans are a separate mechanism from the rate limiter's** — `internal/middleware/ban.go` (`BanList`) enforces the operator-set `bans` table: one row per identifier (`ip` / `hwid` / `hostname`), no expiry, removed by hand. The rate limiter's `banned` map stays what it was: automatic, time-boxed, in-memory. The list is cached in memory and refreshed both on a 30s ticker (so a second API instance picks up a ban made through the first) and immediately after an admin write (`Reload`), and a refresh failure **keeps the previous snapshot** — enforcement must not swing open *or* shut because the DB hiccuped. It runs **ahead of the rate limiter** in `main.go`: rejecting a banned client is an RLock plus three map lookups, cheaper than the limiter's locked per-IP bookkeeping. **A valid `X-Admin-Key` is exempt** — otherwise banning the office IP locks out the dashboard *and* the route that removes the ban; anyone holding that key can delete any ban anyway. Hostnames are stored and matched lower-cased, IPs are canonicalised through `net.ParseIP` (so two spellings of one address cannot become two rows that both miss), and a HWID is kept verbatim because it is matched byte-for-byte against the header.
- **The EMLy Updater's identity is built in exactly two places, and a new field must be added to both** — both now live side by side in `internal/updaterclient`, which is why that package exists: `IdentityFromRequest` builds an `Identity` from the `X-EMLy-*` headers — `Hostname`, `HWID`, `ADDomain`, `LoggedUser`, `LoggedUserState`, `LoggedUserDisconnectedAt`, `Serial`, `Product`, `OSVersion`, `AppVersion` — plus the User-Agent version/contact and the peer IP, and both telemetry paths (`updaterclient.RecordEvent`, `configapi.trackConfigFetch`) go through it into `updaterclient.Upsert`. `IdentityFromWSPayload` is the second, in the same file: the same fields, sourced from the `GET /v2/client/ws` "identity" JSON message instead of headers, feeding the same `Upsert`. Adding a header/field means adding it to *both* constructors, not just one call site — a field that only lands on one path fills in for updaters that use that path and silently stays NULL for the other. A request carrying neither HWID nor hostname is served but not tracked. **A header the client does not send never clears the stored value** (the `COALESCE(NULLIF(?, ''), col)` pairs in the upsert): the updater omits a header it has no value for, so "absent" means "unknown", and an updater too old to send `X-EMLy-LoggedUser` must not wipe what earlier sightings established. **Except for the logged-on user from updater 1.6.2+** (`reportsNobodyLoggedOn`, keyed off the User-Agent version): that build always sends user and state together and omits both only when nobody is logged on, so its silence is an answer and clears `logged_user`, `logged_user_state` and `logged_user_disconnected_at` — keeping them showed users who had signed out days ago as still logged on. `logged_user` is therefore a snapshot to be read against `last_seen_at`, not a history. The one exception is `logged_user_disconnected_at`, which follows `logged_user_state` rather than its own header: any request carrying a state rewrites it, to NULL for a non-disconnected session, so a session that reconnected cannot keep a stale disconnection time. `parseLoggedUserSession` only accepts the three states the updater defines (`active-console`, `active-rdp`, `disconnected`) and drops the timestamp for the other two.
- Handlers are factory functions: `func CreateBugReport(db *sqlx.DB, dbName string, s3conn *storage.S3Connector) http.HandlerFunc { return func(w, r) { ... } }`. Dependencies (db, dbName, s3conn) are injected at construction.
- All responses are JSON via `response.OK` / `response.Created` / `response.Error` (`internal/response`).
- Use the request context (`r.Context()`) for DB calls (`SelectContext`, `GetContext`) and `slog.*Context` logging so traces/spans propagate.
- File uploads use `r.ParseMultipartForm(32 << 20)`; close file streams explicitly.
- ZIP downloads: in-memory `archive/zip` with template-rendered report text via `internal/bugreports/templates/report.txt.tmpl`.
- Installer downloads stream through `streamInstaller` (`download.go`), never a bare `io.Copy`: once the 200 and `Content-Length` are on the wire nothing can be turned into an HTTP error, so a short copy is reported as a **warn** log line (`installer download did not complete`) carrying the reason (`server timeout` / `client disconnected` / `copy failed`), bytes sent vs expected, average throughput and the client's hostname/HWID/IP. `server timeout` means the global `chiMiddleware.Timeout(30s)` cut a transfer in half: the deadline cancels the S3 read, chi's deferred `WriteHeader(504)` is discarded by net/http as a *superfluous response.WriteHeader call*, the access log records a 504, and the client got a 200 with a truncated file. A slow link is enough to hit it.
- net/http's own soft errors (superfluous `WriteHeader`, TLS handshake failures, malformed request lines) go to `srv.ErrorLog` in `main.go`, wired to a `logBridge` at **warn** level. They used to surface as INFO, indistinguishable from ordinary request lines. Anything else reaching the std `log` package still bridges at info.
- The updater self-update manifest answers 200 in every non-error case: an empty catalogue serializes as `{"version": ""}`, which the client treats as a silent no-op. **Never return 404 there** — the client reads 404 as "this mirror does not implement the endpoint yet" and stops without retrying, which is what lets not-yet-upgraded internal site mirrors coexist. At most one `updater_releases` row holds `is_current`; clearing it everywhere is the kill-switch.
- The same reasoning extends to `GET /v2/config`: **never return 404** for "nothing published yet" — answer **204** instead. A client that already treats any `4xx` as an outage would log one on every machine, every cycle, until the first publish; `204` means "reachable, nothing to give you, keep what you have" and logs nothing.
- `GET /v2/client/ws`'s envelope `type` field is deliberately open-ended. v1 is `hello`/`identity`/`ping`/`pong`/`error`; protocol v2 (`identity.protocol >= 2`) adds `welcome`, `command`, `ack`, `result`, `event`, `notify` (`internal/clientproto`, `CLIENT_WS_PROTOCOL.md` is the normative wire format — mirrored by hand in `emly-updater/internal/wsclient`, no shared Go module between the two repos). An unrecognized `type` on either side is still logged and ignored rather than closing the connection, exactly as before — that is what let v2 itself roll out without breaking an already-deployed v1 updater, and is what lets a *future* type do the same. Tolerance stops at the command level: an unsupported/unknown command `name` is explicitly **refused** with `ack` (`unsupported_command`), not silently ignored — the operator has to know it did not run. `internal/clienthub` holds the v2 session/command/event state in memory; see its package doc and `CLIENT_WS_PROTOCOL.md` §2.5 for why the two levels of tolerance differ.
- **Fleet aggregates read `updater_event_hourly`, never raw `updater_events`.** The rollup (migration 20) holds one counter per `(hour, product, event_type)` and is incremented **inside the same transaction as the event insert** (`updaterclient.insertEvent`), so the number a dashboard reads is as live as the event itself — that is what lets the read side stop scanning half a million rows without giving up real time. Two consequences to preserve: the bucket is derived from the inserted row's own `created_at` (not a second `NOW()`) via the single `updaterclient.HourBucketExpr` the read side in `internal/stats` also uses, so every event lands where a `GROUP BY` over the raw rows would put it and migration 20's backfill statement stays usable as a repair (deletes do not follow: pruning and `DeleteStatsClient` leave the counters alone on purpose, so the rollup keeps the fleet's aggregate history); and a new `event_type` or `product` must be added to the rollup's ENUM too, or its rows are counted nowhere. The 24h summary window and the `/events` `from`/`to` filter are therefore hour-aligned.
- `GET /v2/stats/summary` and `GET /v2/stats/events` memoize their payloads behind a `ttlcache.Cache` (`internal/ttlcache`), sized by `STATS_CACHE_TTL`. Dashboards poll them continuously while they report aggregates over a day and a month, so the queries run once per TTL rather than once per request; concurrent callers on a cold key collapse into one build. `/events` keys on every param with the timestamps quantized to a TTL-wide step — its default window ends at `time.Now()`, so without that no two requests would ever share a key. Anything added to the summary belongs in `fetchStatsSummary`, behind the cache — not in the handler.
- **`GET /v2/stats/stream` coalesces instead of caching** (`wsCoalesceWindow`, 1s). A hub event only marks which channels went stale (`noteHubEvent`); a ticker does the queries for the dirty ones (`flush`). Recomputing per event meant every ingested row re-ran the summary and the 30-day rollup once per open connection — cost growing as fleet size × dashboards connected. Draining the hub channel is the event loop's first duty (`statshub.Publish` drops rather than blocks on a full buffer), which is exactly why the queries live in the ticker branch and marking stays DB-free.
- Update releases have independent `is_stable`/`is_beta` boolean flags (a release can be both at once — setting either to `true` clears that flag from whichever other release previously held it) and validate `severity_type` against `validSeverity` (`none`/`security`/`bugfix`/`feature`).
- Remote-config revisions are append-only: `remote_config_revisions.document` never changes after insert. Publishing supersedes the previous `published` row in the same transaction; rolling back clones an old revision's content into a **new**, higher-numbered revision rather than republishing the old one (a lower number would be ignored by every client). `POST /v2/config/revisions`, `/revisions/{revision}/publish`, `/rollback` and `DELETE /revisions/{revision}` all answer `405` on a site mirror (`CONFIG_UPSTREAM_URL` set) — a mirror only replicates, it never accepts writes.

### Database migrations

`internal/database/schema/migrator.go` runs on startup:
1. Executes `init.sql` to ensure base tables exist.
2. Reads `migrations/tasks.json` for conditional tasks.
3. For each task, checks its condition against the live DB before running the corresponding `migrations/*.sql`.

Supported condition types: `column_not_exists`, `column_exists`, `index_not_exists`, `index_exists`, `table_not_exists`, `table_exists`.

## Environment

Copy `.env.example` to `.env`. Required: `DB_DSN`, `DATABASE_NAME`, `API_KEY`, `ADMIN_KEY`. `DB_DSN` must include `parseTime=true&loc=UTC`:
```
DB_DSN=root:secret@tcp(127.0.0.1:3306)/emly?parseTime=true&loc=UTC
```

Other notable vars (see `.env.example` for full list + defaults):
- DB pool: `DB_MAX_OPEN_CONNS`, `DB_MAX_IDLE_CONNS`, `DB_CONN_MAX_LIFETIME`
- Logging: `LOG_LEVEL` (`debug`/`info`/`warn`/`error`, default `info`) — sets the `slog` handler level for both the plain and OTel-forwarded log paths. `LOG_FILE_ENABLED` (default `true`), `LOG_DIR` (default `logs`; `/logs` in `docker-compose.yml`), `LOG_RETENTION_DAYS` (default `30`, `0` keeps everything) — the daily log files
- Auth extras: `DASHBOARD_KEY` (bypasses both rate-limit layers)
- Rate limiting: `RL_UNAUTH_*` and `RL_AUTH_*` (`MAX_REQS`, `WINDOW`, `MAX_FAILS`, `BAN_DUR`)
- Storage — API file bucket: `USE_S3_API_FILE_STORAGE`, `S3_API_FILE_ACCESS_KEY_ID`, `S3_API_FILE_SECRET_ACCESS_KEY`, `S3_API_FILE_BUCKET`, `S3_API_FILE_REGION`, `S3_API_FILE_ENDPOINT`, `S3_API_FILE_ACCOUNT_ID` (optional, R2 endpoint shortcut)
- Storage — updates bucket: `USE_S3_UPDATES_STORAGE`, `S3_UPDATES_ACCESS_KEY_ID`, `S3_UPDATES_SECRET_ACCESS_KEY`, `S3_UPDATES_BUCKET`, `S3_UPDATES_REGION`, `S3_UPDATES_ENDPOINT`, `S3_UPDATES_ACCOUNT_ID` (optional, R2 endpoint shortcut). The two buckets are fully independent and may sit on different S3-compatible providers.
- Telemetry: `OTEL_ENABLED`, `OTEL_ENDPOINT`
- Updates: `UPDATES_ENABLED`, `S3_UPDATES_PREFIX` (path prefix inside the updates bucket; manifest download links are built from the request's `Host`/`X-Forwarded-*` headers, not an env var), `S3_UPDATER_PREFIX` (default `updater`; separate prefix in the same updates bucket for the EMLy Updater's own installers)
- Remote config: `CONFIG_UPSTREAM_URL` (empty on the cloud/primary instance; set on a site mirror to replicate `/v2/config` from upstream instead of accepting writes), `CONFIG_UPSTREAM_INTERVAL` (default `5m`), `CONFIG_UPSTREAM_API_KEY` (defaults to this instance's own `API_KEY`)
- Real-time stats: `STATS_STREAM_TICK_INTERVAL` (default `30s`) — how often `GET /v2/stats/stream` pushes a resync tick to connected dashboards independent of any new `updater_events` row
- Fleet stats: `STATS_CACHE_TTL` (default `30s`) — how long `GET /v2/stats/summary` and `GET /v2/stats/events` may serve a memoized payload; `0` disables the cache. `EVENTS_RETENTION_DAYS` (default `30`, `0` keeps everything) — how many days of raw `updater_events` rows `internal/eventprune` keeps; only the per-client history on `GET /v2/stats/clients/{id}` reads them, so this bounds detail, not the charts

### Adding new environment variables

When you add a var to `internal/config/config.go`, update both of these in the same commit:
1. **`.env.example`** — add it with a sensible default/placeholder and a comment.
2. **`docker-compose.yml`** — add it under `services.api.environment` using `${VAR_NAME:-default}` syntax.

## Documentation upkeep

Two documents at the repo root describe the API to humans and must be kept
current — they are not optional follow-up work, they belong in the same commit
as the change.

### `DOCS.md` — after any major architectural change

`DOCS.md` is written in **Italian** for developers coming from Node.js
(Express/Fastify) or PHP (Laravel/Slim) who have never written Go. Every
concept is introduced by analogy to those stacks: `r.Use(...)` is explained as
`app.use(...)`, `sqlx` against Eloquent/Knex, `struct` against a TypeScript
interface, and so on. Keep that voice — it is the only document in the repo a
non-Go developer can actually read.

Update it whenever a change would make a section of it wrong or incomplete:
a new package under `internal/`, a new middleware or a change to the global
middleware order, a new auth mechanism, a change in how handlers are
constructed or wired, a new external dependency (another S3 bucket, another
backing service), a change to the migration mechanism, or a new route group.
A bug fix inside an existing handler does not require a `DOCS.md` change.

When you touch it, follow what is already there: explain the *why* alongside
the *what*, and prefer the Node/PHP analogy over Go jargon.

### `ROUTES.md` — whenever routes change

`ROUTES.md` is the endpoint-by-endpoint reference (Italian, same audience):
every route with its auth requirement, query/body parameters and behaviour.
Adding, removing or renaming a route, changing its auth gating, its accepted
parameters or its status codes means updating the matching table and prose
there in the same commit.
