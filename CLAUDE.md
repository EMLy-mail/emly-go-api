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

# Tests (note: the repo currently has no _test.go files)
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
2. If `OTEL_ENABLED`, set up OTel and bridge the std `log` package into `slog` → OTLP.
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
- `GET /health` → `handlers.Health`
- `POST /api/bug-reports` → legacy alias for v1 bug-report creation
- `r.Mount("/v1", v1.NewRouter(...))` and `r.Mount("/v2", v2.NewRouter(...))`

Each version's `NewRouter` (in `internal/routes/v1/v1.go`, `v2/v2.go`) re-applies the custom `RateLimiter`, sets `X-Server`/`X-Powered-By` headers, exposes `GET /health`, and mounts route groups defined in sibling files (`bug_reports.go`, `admin.go`, and for v2 `updates.go`).

**v1** (`/v1/api/...`):
- `bug-reports`: API-key-only group (`POST /`, `GET /count`) and API-key + admin-key group (full CRUD, `{id}/status`, `{id}/files`, `{id}/download`, etc.).
- `admin/auth`: session login/validate/logout (`/login` is rate-limited; `/validate` + `/logout` require a session token).
- `admin/users`: admin-key-protected user CRUD + password reset.

**v2** (`/v2/...`): everything in v1 plus `updates/` — public update manifest + release download, and admin-key-protected release management (`update_releases` table).

### Rate limiting — two layers

1. **Custom `RateLimiter`** (`ratelimit.ban.go`), applied globally and per-version-router. Two tiers keyed by IP: *unauthenticated* (no `X-API-Key`/`X-Admin-Key`, `RL_UNAUTH_*` env) and *authenticated* (`RL_AUTH_*` env). Tracks request counts per window and **bans** an IP (in-memory `sync.Map`) after `MaxFails` window-violations for `BanDur`. Private/loopback IPs and requests bearing a valid `X-Dashboard-Key` bypass it entirely. A goroutine prunes stale visitor + ban entries every 10 min.
2. **`httprate.LimitByIP(30, time.Minute)`** applied per route group inside v1/v2.

### Package layout

- `internal/config/` — Singleton `Config` loaded from env vars (`Load()`/`once`). Panics if `DATABASE_NAME` (validated against `^[a-zA-Z0-9_]+$` to prevent SQL injection — it is interpolated into queries) or `DB_DSN` is missing. `API_KEY`/`ADMIN_KEY` accept a comma-separated list but only the first non-empty value is used.
- `internal/database/` — MySQL pool setup with configurable limits.
- `internal/database/schema/` — Conditional migrator (see below).
- `internal/handlers/` — Factory functions returning `http.HandlerFunc`, named `<resource>.route.go`. Response helpers (`jsonOK`, `jsonCreated`, `jsonError`) in `response.go`.
- `internal/middleware/` — Auth (`apikey.go`, `adminKey.go`) and rate limiting. Auth middleware load allowed keys into a map at construction for O(1) lookup; they take a `*sqlx.DB` arg that is currently unused (keys come from config).
- `internal/storage/` — `S3Connector` wrapping an S3-compatible bucket (upload/download/list/delete/rename, folder helpers) and `migrateFiles.go`. `NewS3Connector` is provider-agnostic; `main.go` constructs one instance per bucket (API files, updates) from their respective `config.S3BucketConfig`.
- `internal/telemetry/` — OTel provider setup (trace/metric/log exporters, W3C propagators).
- `internal/timing/` — Per-request timing checkpoints carried on the context.
- `internal/models/` — Structs with `db:` and `json:` tags. Sensitive fields use `json:"-"`.

### Handler conventions

- Handlers are factory functions: `func CreateBugReport(db *sqlx.DB, dbName string, s3conn *storage.S3Connector) http.HandlerFunc { return func(w, r) { ... } }`. Dependencies (db, dbName, s3conn) are injected at construction.
- All responses are JSON via `jsonOK` / `jsonCreated` / `jsonError`.
- Use the request context (`r.Context()`) for DB calls (`SelectContext`, `GetContext`) and `slog.*Context` logging so traces/spans propagate.
- File uploads use `r.ParseMultipartForm(32 << 20)`; close file streams explicitly.
- ZIP downloads: in-memory `archive/zip` with template-rendered report text via `internal/handlers/templates/report.txt.tmpl`.
- Update releases have independent `is_stable`/`is_beta` boolean flags (a release can be both at once — setting either to `true` clears that flag from whichever other release previously held it) and validate `severity_type` against `validSeverity` (`none`/`security`/`bugfix`/`feature`).

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
- Auth extras: `DASHBOARD_KEY` (rate-limit bypass)
- Rate limiting: `RL_UNAUTH_*` and `RL_AUTH_*` (`MAX_REQS`, `WINDOW`, `MAX_FAILS`, `BAN_DUR`)
- Storage — API file bucket: `USE_S3_API_FILE_STORAGE`, `S3_API_FILE_ACCESS_KEY_ID`, `S3_API_FILE_SECRET_ACCESS_KEY`, `S3_API_FILE_BUCKET`, `S3_API_FILE_REGION`, `S3_API_FILE_ENDPOINT`, `S3_API_FILE_ACCOUNT_ID` (optional, R2 endpoint shortcut)
- Storage — updates bucket: `USE_S3_UPDATES_STORAGE`, `S3_UPDATES_ACCESS_KEY_ID`, `S3_UPDATES_SECRET_ACCESS_KEY`, `S3_UPDATES_BUCKET`, `S3_UPDATES_REGION`, `S3_UPDATES_ENDPOINT`, `S3_UPDATES_ACCOUNT_ID` (optional, R2 endpoint shortcut). The two buckets are fully independent and may sit on different S3-compatible providers.
- Telemetry: `OTEL_ENABLED`, `OTEL_ENDPOINT`
- Updates: `UPDATES_ENABLED`, `S3_UPDATES_PREFIX` (path prefix inside the updates bucket; manifest download links are built from the request's `Host`/`X-Forwarded-*` headers, not an env var)

### Adding new environment variables

When you add a var to `internal/config/config.go`, update both of these in the same commit:
1. **`.env.example`** — add it with a sensible default/placeholder and a comment.
2. **`docker-compose.yml`** — add it under `services.api.environment` using `${VAR_NAME:-default}` syntax.
