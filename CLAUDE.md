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

# One-off: migrate report files from DB blobs to S3/R2 (requires USE_S3_COMPATIBLE_STORAGE=true)
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
- **Object storage**: Cloudflare R2 (S3-compatible) via `aws-sdk-go-v2` — optional, gated by `USE_S3_COMPATIBLE_STORAGE`
- **Observability**: OpenTelemetry (traces + metrics + logs) exported via OTLP/HTTP — optional, gated by `OTEL_ENABLED`
- **Auth**: header API key (`X-API-Key`), admin key (`X-Admin-Key`), session tokens for the dashboard, and a rate-limit bypass key (`X-Dashboard-Key`)

### Startup sequence (`main.go`)

1. `godotenv.Load()` then `config.Load()` (singleton via `sync.Once`).
2. If `OTEL_ENABLED`, set up OTel and bridge the std `log` package into `slog` → OTLP.
3. Connect to MySQL, run `schema.Migrate`.
4. If `USE_S3_COMPATIBLE_STORAGE`, build + ping the R2 connector.
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
- `internal/storage/` — `S3Connector` wrapping R2 (upload/download/list/delete/rename, folder helpers) and `migrateFiles.go`.
- `internal/telemetry/` — OTel provider setup (trace/metric/log exporters, W3C propagators).
- `internal/timing/` — Per-request timing checkpoints carried on the context.
- `internal/models/` — Structs with `db:` and `json:` tags. Sensitive fields use `json:"-"`.

### Handler conventions

- Handlers are factory functions: `func CreateBugReport(db *sqlx.DB, dbName string, s3conn *storage.S3Connector) http.HandlerFunc { return func(w, r) { ... } }`. Dependencies (db, dbName, s3conn) are injected at construction.
- All responses are JSON via `jsonOK` / `jsonCreated` / `jsonError`.
- Use the request context (`r.Context()`) for DB calls (`SelectContext`, `GetContext`) and `slog.*Context` logging so traces/spans propagate.
- File uploads use `r.ParseMultipartForm(32 << 20)`; close file streams explicitly.
- ZIP downloads: in-memory `archive/zip` with template-rendered report text via `internal/handlers/templates/report.txt.tmpl`.
- Update releases validate against `validChannels` (`stable`/`beta`/`archived`) and `validSeverity` (`none`/`security`/`bugfix`/`feature`).

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
- Storage: `USE_S3_COMPATIBLE_STORAGE`, `CF_ACCOUNT_ID`, `CF_R2_ACCESS_KEY_ID`, `CF_R2_SECRET_ACCESS_KEY`, `CF_R2_BUCKET_NAME`, `CF_R2_REGION`, `CF_R2_ENDPOINT`
- Telemetry: `OTEL_ENABLED`, `OTEL_ENDPOINT`
- Updates: `UPDATES_ENABLED`, `API_BASE_URL` (builds manifest download links), `UPDATES_S3_PREFIX`

### Adding new environment variables

When you add a var to `internal/config/config.go`, update both of these in the same commit:
1. **`.env.example`** — add it with a sensible default/placeholder and a comment.
2. **`docker-compose.yml`** — add it under `services.api.environment` using `${VAR_NAME:-default}` syntax.
