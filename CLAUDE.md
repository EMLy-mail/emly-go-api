# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
# Development (hot-reload via air)
air

# Build production binary
go build -o ./build/emly-api.exe .

# Run directly
go run .

# Run tests
go test ./...

# Run a single test
go test ./internal/... -run TestName -v
```

## Architecture

This is a Go REST API for a bug reporting system ("EMLy"). It uses:
- **Router**: `go-chi/chi/v5`
- **Database**: MySQL via `jmoiron/sqlx`
- **Auth**: Header-based API key (`X-API-Key`) and admin key (`X-Admin-Key`)
- **Rate limiting**: `go-chi/httprate` (global: 100/min, route groups: 30/min by IP)

### Request flow

```
main.go → chi router → global middleware → route group middleware → handler
```

Global middleware order: RequestID → RealIP → Logger → Recoverer → Timeout(30s) → RateLimitByIP

### Route groups

- **Public**: `GET /` (ping), `GET /v1/health`
- **API key only**: `POST /v1/api/bug-reports/`, `GET /v1/api/bug-reports/count`
- **API key + admin key**: All other `/v1/api/bug-reports/*` endpoints

### Package layout

- `internal/config/` — Loads config from env vars (via godotenv). Key vars: `PORT`, `DB_DSN`, `DATABASE_NAME`, `API_KEY`, `ADMIN_KEY`.
- `internal/database/` — MySQL connection pool setup with configurable limits.
- `internal/database/schema/` — Conditional migration system: `init.sql` bootstraps tables, `migrations/tasks.json` defines conditional tasks (e.g. `column_not_exists`), `migrations/*.sql` are the individual migration files.
- `internal/handlers/` — Factory functions returning `http.HandlerFunc`. The `*sqlx.DB` is passed in at construction. Response helpers (`jsonOK`, `jsonCreated`, `jsonError`) live in `response.go`.
- `internal/middleware/` — API key and admin key auth middleware; each loads allowed keys into a map at startup for O(1) lookup.
- `internal/models/` — Structs with `db:` and `json:` tags. Sensitive fields use `json:"-"`.

### Handler conventions

- Each handler file is named `<resource>.route.go`.
- Handlers are factory functions: `func CreateBugReport(db *sqlx.DB) http.HandlerFunc { return func(w http.ResponseWriter, r *http.Request) { ... } }`.
- All responses are JSON. Use `jsonOK`, `jsonCreated`, or `jsonError` from `response.go`.
- File uploads use `r.ParseMultipartForm(32 << 20)`. File streams must be explicitly closed.
- ZIP downloads: in-memory `archive/zip` with template-rendered report text via `internal/handlers/templates/report.txt.tmpl`.

### Database migrations

The migrator in `internal/database/schema/migrator.go` runs on startup:
1. Executes `init.sql` to ensure base tables exist.
2. Reads `migrations/tasks.json` for conditional tasks.
3. Checks each task's condition (e.g. `column_not_exists`) against the DB before running its SQL file.

Supported condition types: `column_not_exists`, `column_exists`, `index_not_exists`, `index_exists`, `table_not_exists`, `table_exists`.

## Environment

Copy `.env.example` to `.env`. Required vars: `DB_DSN`, `DATABASE_NAME`, `API_KEY`, `ADMIN_KEY`.

The `DB_DSN` must include `parseTime=true&loc=UTC`, e.g.:
```
DB_DSN=root:secret@tcp(127.0.0.1:3306)/emly?parseTime=true&loc=UTC
```