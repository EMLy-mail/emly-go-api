# Remote configuration: server side of `/v2/config`

Design document — 2026-09-04

Companion to `emly-updater/docs/superpowers/specs/2026-09-04-remote-config-design.md`
(the client spec). That document owns the JSON schema, the validation rules
and the client behaviour; this one owns how the API stores, validates,
versions and serves the document, and how operators change it. Where the two
disagree, the client spec wins on the wire format and this one on storage
and administration.

## 1. Problem

The EMLy Updater and EMLy itself will fetch one policy document from
`GET /v2/config` on the same hosts that already serve the update manifest.
The API has nothing that could back it: no table, no route, no notion of a
document with a monotonic revision, no validation of its content, and no way
for an operator to publish a change without a hand-edited SQL statement.

The document is a fleet-wide control surface. A wrong publish reaches every
machine within one poll interval, and there is no "unpublish" that reaches
them back (the client keeps its last-known-good copy). The server therefore
has to make a wrong publish hard, a rollback easy, and every change
attributable.

## 2. Goals

- Serve the current published document with a strong `ETag`, honouring
  `If-None-Match`, on the same auth (`X-API-Key`) as the updater manifest.
- Assign `revision` and `generatedAt` server-side, so monotonicity is a
  property the server guarantees, not one the operator has to remember.
- Validate a document with the **same rules the client applies** before it
  can be published; a document the fleet would reject never leaves the
  server.
- Keep every revision ever published. Rollback is a new revision with old
  content, never a lower number.
- Let an operator preview the *effective* policy for a given host before
  publishing, and see which revision each client last received.
- Work unchanged on internal site mirrors, which must serve the same bytes
  as the cloud.

## 3. Non-goals

- A dashboard UI. The admin routes are designed for one, but the UI lives
  elsewhere.
- Server-side tailoring of the document per client (keying off
  `X-EMLy-HWID`). The client evaluates `overrides` itself so a cached
  document keeps matching after the machine moves; the server serves one
  document to everyone. The headers are recorded (§8) but not used to
  branch.
- Signing the document. §11 reserves the field; the signing key and the
  rollout of verification are a separate piece of work.
- Telemetry beyond "which revision did each client last get". Heartbeats,
  remote commands and log upload are out of scope here as they are in the
  client spec.

## 4. Storage model

### 4.1 One document, many revisions

The unit of storage is the **whole document**, stored as the canonical JSON
bytes the API will serve, one row per revision. It is *not* normalised into
`servers` / `sites` / `overrides` tables:

- The client contract is all-or-nothing on the whole document; storing it
  whole makes "what exactly did revision 42 say" a single `SELECT`, and makes
  the `ETag` a property of the row rather than something recomputed from a
  join that might not be byte-stable.
- The validation rules (§6) are expressed on the document. One validator,
  one input shape, on both sides of the wire.
- A structured editor can be built on top later, reading and writing the
  JSON; a normalised schema would have to be kept in lock-step with every
  additive change to the client schema, which the client spec explicitly
  allows without a `schemaVersion` bump.

### 4.2 Table

Migration `10_remote_config.sql`, conditional on `table_not_exists`:

```sql
CREATE TABLE IF NOT EXISTS `remote_config_revisions` (
    `revision`       INT UNSIGNED    NOT NULL PRIMARY KEY,
    `schema_version` INT UNSIGNED    NOT NULL,
    `status`         ENUM('draft', 'published', 'superseded') NOT NULL DEFAULT 'draft',
    `document`       MEDIUMTEXT      NOT NULL,   -- canonical JSON, served byte for byte
    `etag`           CHAR(64)        NOT NULL,   -- hex SHA-256 of `document`
    `notes`          TEXT            NULL,       -- operator's change note
    `created_by`     VARCHAR(255)    NULL,       -- dashboard user, when known
    `based_on`       INT UNSIGNED    NULL,       -- revision this was cloned from (rollback)
    `generated_at`   TIMESTAMP       NOT NULL,   -- == document.generatedAt
    `published_at`   TIMESTAMP       NULL,
    `created_at`     TIMESTAMP       NOT NULL DEFAULT CURRENT_TIMESTAMP,
    INDEX `idx_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
```

`document` is `MEDIUMTEXT`, not `JSON`, on purpose: MySQL's `JSON` type
normalises what it stores (key order, whitespace, number formatting), so the
bytes read back would not be the bytes hashed into `etag`. The API produces
the canonical form itself (§5.3) and the column holds exactly that.

**Invariants**, enforced in transactions, not by the schema:

- At most one row has `status = 'published'`. Publishing sets the previous
  one to `superseded` in the same transaction.
- `revision` is allocated as `MAX(revision) + 1` inside the publish/create
  transaction; a duplicate-key error (two operators at once) is retried
  once, then reported as `409`.
- A row never changes `document` after creation. Fixing a draft means
  creating a new draft; the old one can be deleted (§7.6). A published or
  superseded row is never deleted.

### 4.3 Client tracking

Migration `11_updater_clients_config.sql`, conditional on
`column_not_exists updater_clients.config_revision`:

```sql
ALTER TABLE `updater_clients`
    ADD COLUMN `config_revision`   INT UNSIGNED NULL AFTER `updater_version`,
    ADD COLUMN `config_fetched_at` TIMESTAMP    NULL AFTER `config_revision`,
    ADD INDEX  `idx_config_revision` (`config_revision`);
```

Updated on every `200` and `304` from `GET /v2/config` (§8). This is what
answers "has the fleet picked up revision 42 yet" without any new client
telemetry, at the cost of one `UPDATE` per fetch, which the manifest path
already pays for `manifest_check` events. No new `updater_events` row is
written: the fetch happens every cycle, the event table would double in
size for a fact the client row already carries.

## 5. Public endpoint

### 5.1 `GET /v2/config`

Mounted in `internal/routes/v2/config.go` (new file, same shape as
`updates.go`), API-key protected like `GET /updates/manifest/updater`,
`httprate.LimitByIP(30, time.Minute)`.

Request headers the handler reads: `If-None-Match`, and the identification
set the updater already sends (`X-EMLy-HWID`, `X-EMLy-Hostname`,
`X-EMLy-ADDomain`, `User-Agent`) for §8.

| Case | Status | Body | Headers |
|---|---|---|---|
| Published row exists, `If-None-Match` absent or different | `200` | `document` bytes verbatim | `ETag: "<etag>"`, `Content-Type: application/json`, `Cache-Control: no-cache`, `X-Config-Revision: <n>` |
| Published row exists, `If-None-Match` equals `"<etag>"` (also `W/"<etag>"`, also a comma list containing it) | `304` | none | `ETag`, `Cache-Control: no-cache`, `X-Config-Revision` |
| No published row | `204` | none | `Cache-Control: no-cache` |
| Missing / wrong API key | `401` | `{"error":"unauthorized"}` | (middleware) |
| DB error | `500` | `{"error":"..."}` | |

**`204`, not `404`, when nothing is published.** The client treats `4xx` as
"try the next server, and if they all fail, keep the cache and log an
outage". A `404` from a perfectly healthy API that simply has no document
yet would log that outage on every machine, every cycle, until the first
publish. `204` means "reachable, nothing to give you, keep what you have":
no outage event. It is also the same reasoning that makes the updater
manifest answer `200 {"version": ""}` rather than `404`: a status that
already means something else to the client must not be reused. (The client
spec §6 lists `204` alongside `304` as a non-error.)

`Cache-Control: no-cache` means "revalidate every time", which is exactly
the `If-None-Match` round-trip the client makes. A reverse proxy or CDN in
front of a mirror will do the same, and never serve a stale document past
a publish.

The body is written from the `document` column with `w.Write`, not through
`jsonOK`: re-encoding would risk changing the bytes the `ETag` was computed
over.

### 5.2 `ETag`

Strong, `"` + lowercase hex SHA-256 of the stored bytes + `"`. Stored in the
row at creation, so it is stable across restarts and identical on every
mirror serving the same revision. Comparison is exact on the hex; the
handler strips a `W/` prefix and splits on commas before comparing, since
some proxies weaken or merge validators.

### 5.3 Canonical form

Whatever an operator posts (§7.2) is decoded into the typed document,
validated, completed with the server-assigned `revision` and `generatedAt`,
and re-encoded with `encoding/json` **from the typed struct**: fixed field
order, no indentation, no trailing newline, `null` for unset optionals. The
result is what is stored and served. Two operators posting the same content
with different key order or whitespace get byte-identical documents and the
same `etag`. Unknown fields the operator sent are dropped by this
round-trip; §7.2 reports them in the response so a typo in a field name is
not silently lost.

## 6. Validation: the shared ruleset

`internal/remoteconfig` (new package, no HTTP, no DB) implements:

- `Parse([]byte) (*Document, []Problem)` — structural typing + every rule in
  the client spec §7 and §8: `schemaVersion == 1`, server URL shape,
  `serverRef` integrity, CIDR parsing, `defaultVersion` enabled, `until` /
  `generatedAt` RFC 3339, override `id` uniqueness, `match` / `except`
  shape (`all` alone, no empty lists, no `all` in `except`), `patch` limited
  to `control` / `updater` / `logging` / `defaultServer`, ranges on every
  numeric field, and the dry-run of every override against an all-matching
  synthetic host. `Problem` carries a JSON-pointer-style `path` and a
  `message`, and **all** problems are returned, not the first, so a
  dashboard can show them at once.
- `Match(sel Selector, h Host) bool` and `Effective(doc, h Host) (*Document,
  []string)` — override evaluation (AND across keys, OR within a list,
  `except`, `until`) and JSON Merge Patch, returning the effective document
  and the ids of the overrides that applied. Used by `preview` (§7.5) and by
  the dry-run inside `Parse`.
- `Canonical(*Document) ([]byte, etag string)` — §5.3.

The rules are written in two repos in the same language with no shared
module (the IPC `.proto` is already kept in sync by hand the same way).
What keeps them equal is a **shared conformance fixture set**:
`testdata/remoteconfig/` holds `valid/*.json`, `invalid/*.json` (each with
a sibling `.problems.json` listing the expected paths) and
`effective/*.json` (document + host + expected effective output + expected
override ids). The directory is copied verbatim into both repos and both
validators are tested against every file. A rule added on one side without
its fixture fails no test; a fixture added on one side and not copied
fails the other side's test the moment it is copied. That is as much as two
repos without a shared module can enforce, and it is enough.

The size cap is enforced before parsing: 1 MiB, the same number the client
uses.

## 7. Admin endpoints

All under `/v2/config`, `AdminKeyAuth`, `httprate.LimitByIP(30, time.Minute)`,
JSON in and out via the existing `jsonOK` / `jsonCreated` / `jsonError`
helpers. Snake_case field names in admin payloads, like the release
management routes; the document itself keeps its camelCase because it is
the wire format.

### 7.1 `GET /v2/config/revisions`

Paginated (`page`, `page_size` ≤ 100, newest first), optional
`status=draft|published|superseded`. Returns metadata only:

```json
{
  "page": 1, "page_size": 20, "total": 43,
  "revisions": [
    { "revision": 42, "schema_version": 1, "status": "published",
      "etag": "…", "notes": "freeze CB until the 5th", "created_by": "flavio",
      "based_on": null, "generated_at": "…", "published_at": "…", "created_at": "…",
      "clients_on_revision": 118 }
  ]
}
```

`clients_on_revision` is `COUNT(*) FROM updater_clients WHERE
config_revision = ?`, so the list doubles as the rollout view.

### 7.2 `POST /v2/config/revisions`

Body:

```json
{
  "document": { "...": "a document without revision/generatedAt" },
  "notes": "optional",
  "publish": false
}
```

1. Size cap, then `remoteconfig.Parse`. Any problem → `422` with
   `{"error": "invalid document", "problems": [{"path": "/servers/srv-x", "message": "…"}]}`.
   A `revision` or `generatedAt` present in the posted document is
   **ignored and reported** as a problem of severity `warning` in the
   response, not an error: the server owns both.
2. Unknown fields dropped by the canonical round-trip are reported the same
   way (`"path": "/updater/certficate", "message": "unknown field, dropped"`).
   A dashboard shows them; CI treats warnings as failures if it wants to.
3. In a transaction: allocate `revision`, set `generated_at = NOW()` (UTC,
   second precision), inject both into the document, canonicalise, insert
   as `draft`. If `publish` is `true`, continue as §7.3 in the same
   transaction.
4. `201` with the stored row (metadata + `document`, plus `warnings`).

`created_by` is taken from the dashboard session when the request carries
one (`X-Session-Token`, the existing admin session mechanism), else `null`.
The admin key alone is anonymous; a dashboard that wants attribution sends
both.

### 7.3 `POST /v2/config/revisions/{revision}/publish`

Publishes a draft. Rules:

- The row must be `draft`; `published` → `409 "already published"`,
  `superseded` → `409 "superseded; use rollback to republish its content"`.
- `revision` must be greater than the currently published one. It always is
  for a draft created after the current publish; a draft older than a
  later publish gets `409 "a newer revision has been published since; create
  a new draft"`, because clients would ignore it (client spec §9.3).
- Transaction: `UPDATE … SET status='superseded' WHERE status='published'`,
  then `UPDATE … SET status='published', published_at=NOW() WHERE revision=?
  AND status='draft'`; zero rows affected on the second → `409`.
- `200` with the row.

### 7.4 `POST /v2/config/rollback`

```json
{ "to": 41, "notes": "2.1.3 verified, lifting the freeze" }
```

Clones revision 41's document (any status except `draft`) into a **new**
revision with `based_on = 41`, fresh `revision` and `generatedAt`,
canonicalised again (the content is re-validated on the way, so a rule
tightened since 41 was written surfaces here as `422` rather than as a
fleet-wide rejection), and publishes it in the same transaction. `201` with
the new row.

This is the only rollback. Republishing 41 itself would hand the fleet a
lower number than 42 and every client would ignore it, which is precisely
the protection against a lagging mirror.

### 7.5 `POST /v2/config/preview`

```json
{
  "revision": 42,
  "document": null,
  "host": {
    "hwid": "9A3F1C77-…", "hostname": "RM095", "dc": "DC-RM2",
    "ips": ["172.16.96.41"], "domain": "tregcc.local",
    "now": "2026-09-05T09:00:00Z"
  }
}
```

Exactly one of `revision` (stored) or `document` (inline, validated first,
not stored) is required. Returns the effective document for that host, the
list of override ids that applied, the site matched in `dcLookupMap`
(or `null` → `defaultServer`) and the resolver chain that host would use.
`now` defaults to the server clock and exists so an operator can ask "what
will this host see after the freeze expires".

This is the operator's safety net: "if I publish this, what does the CB
site actually get" is answered before publishing, with the same code that
validates. It is also the fastest way to reproduce a ticket: paste the
host's headers from the access log.

### 7.6 `GET /v2/config/revisions/{revision}` and `DELETE`

`GET` returns the row with its full `document`. `DELETE` is allowed only on
`draft` rows (`409` otherwise) and exists so an abandoned draft does not sit
in the list forever; it does not free the revision number.

### 7.7 `POST /v2/config/validate`

Same body as §7.2 without `notes`/`publish`; runs `Parse` and returns
`200 {"valid": true, "warnings": […]}` or `422` with problems. Nothing is
stored. For CI pipelines and for a dashboard's "check" button.

## 8. Client tracking on fetch

After writing the response of `GET /v2/config` (both `200` and `304`), the
handler calls the existing `upsertUpdaterClient` path with the request's
identification headers and then:

```sql
UPDATE updater_clients SET config_revision = ?, config_fetched_at = NOW() WHERE id = ?
```

Best-effort, logged at warn on failure, never affects the response, exactly
like `recordUpdaterEvent`. A request with neither `X-EMLy-HWID` nor
`X-EMLy-Hostname` is served but not tracked.

`GET /v2/stats/summary` gains `clients_by_config_revision` (`revision`,
`count`), and `GET /v2/stats/clients` / `…/clients/{id}` expose the two new
columns. An operator sees the rollout progress of a revision from the
existing dashboard without a new screen.

## 9. Site mirrors

Internal site mirrors run this same API against their own database. For
releases that is workable because a release is a file plus a row an
operator can publish on each mirror. For the config document it is not:
the whole design rests on every server handing out the **same** document
with the **same** `revision`, or a client that flips between its site mirror
and the cloud would see conflicting policies with the ordering rule
silently picking whichever published later.

A mirror therefore does not accept config writes; it replicates:

- `CONFIG_UPSTREAM_URL` (env, empty on the cloud instance). When set, the
  admin routes of §7 answer `405 {"error": "this instance mirrors <url>;
  publish there"}`, and a background goroutine fetches
  `<upstream>/v2/config` every `CONFIG_UPSTREAM_INTERVAL` (default `5m`)
  with the mirror's own `X-API-Key` (`CONFIG_UPSTREAM_API_KEY`, defaulting
  to this instance's `API_KEY`) and `If-None-Match`.
- On `200`: `remoteconfig.Parse` (a mirror never serves what it could not
  validate), then insert the row **with the upstream's `revision`, `etag`
  and `generated_at`**, status `published`, superseding the previous one.
  The bytes are stored as received; canonicalisation is not re-run, and the
  local `etag` is compared with the upstream's `ETag` header, a mismatch
  being logged as an error and the document rejected. Same bytes, same
  hash, same revision on every host.
- On `304` / `204` / error: nothing changes; the mirror keeps serving its
  last copy. `GET /v2/config` on a mirror that has never reached upstream
  answers `204`, which the client handles.
- `GET /v2/health` reports `config_upstream: { revision, fetched_at,
  last_error }` on a mirror so the sync state is visible.

Both `CONFIG_UPSTREAM_*` variables go into `internal/config/config.go`,
`.env.example` and `docker-compose.yml` in the same commit, per the repo
rule.

## 10. Rate limiting and load

One fetch per client per `refresh.intervalMinutes` (15 by default), a `304`
in the overwhelming majority of cases, answered from one indexed `SELECT`
on a single-row result. Negligible.

`httprate.LimitByIP(30, time.Minute)` counts per source IP, and a whole
site behind one NAT shares an IP when it talks to the cloud. At 4 fetches
per client per hour, 30/min is 450 clients per NAT before the limit trips.
Sites are expected to hit their own mirror (on which clients have distinct
LAN addresses), so this is not an issue today; the number is written here
so it is found before it is.

The custom `RateLimiter` already bypasses private/loopback ranges, so
mirrors are unaffected by it.

## 11. Security considerations

- **Auth.** The public route requires the API key like the updater manifest.
  The admin routes require the admin key; a wrong key is a `401` before any
  parsing. The rollback and publish routes are the highest-impact writes in
  this API: they are logged at `info` with revision, `created_by`, client
  IP and `notes`, via `slog.InfoContext` so they land in the OTel log
  stream when enabled.
- **Validation is a safety feature, not just hygiene.** The server rejecting
  what the fleet would reject means a mistake is a `422` on an operator's
  screen instead of event 902 on 400 machines.
- **No document is ever mutated.** Every state the fleet has been in is a
  row that can be read back and re-published through rollback.
- **Mirrors validate what they replicate** (§9) and compare the hash, so a
  compromised or misconfigured upstream cannot push malformed bytes through
  a mirror, only well-formed policy, which is the same exposure as today's
  manifest.
- **Signature, reserved.** The canonical form (§5.3) is what a future
  signature would be computed over, and `remote_config_revisions` gains a
  nullable `signature` column when that lands. Nothing here has to change
  to add it; nothing here pretends to provide it.

## 12. Failure scenarios

| Scenario | Behaviour |
|---|---|
| No revision published yet | `204` to every client; no outage logged fleet-side. |
| Two operators publish at once | Second `MAX+1` collides → one retry → `409` to the loser, who refreshes and re-creates the draft. |
| Operator posts a document with a broken override | `422` with every problem path; nothing stored. |
| Operator publishes the wrong thing | `POST /rollback {"to": previous}`: new higher revision, fleet converges at its next cycle. |
| DB down | `500`; the client treats it as a server failure, tries the next candidate, keeps its cache. |
| Mirror cannot reach upstream | Serves its last copy, `health` shows `last_error`; no client impact until the copy ages past `staleAfterDays`. |
| Mirror stores a partial body | Rejected by `Parse` or by the hash mismatch; previous row kept. |
| Proxy weakens the `ETag` | `W/` stripped before comparison → still `304`. |
| Document grows past 1 MiB | `413` on post; the client would have rejected it too. |

## 13. Testing

- `internal/remoteconfig`: the conformance fixtures (§6) plus unit tests on
  canonical form (key order independence, idempotence, stable `etag`) and
  on `Match` edge cases the fixtures do not cover.
- `internal/handlers/config_test.go`: `200`/`304`/`204` decision table with
  a fake row, `If-None-Match` variants (`W/`, comma list, wrong quote), body
  served byte-for-byte, response headers.
- `internal/routes/v2/config_routing_test.go`: same style as
  `updater_routing_test.go` with `NewRouter(nil, nil, nil)`: public route is
  API-key gated, admin routes are admin-key gated, `preview` and `validate`
  do not need a DB and answer `422` on garbage with the problem list.
- Publish/rollback transaction logic: table-driven against the SQL through
  `sqlmock` (new test dependency) since the repo has no integration DB in CI.
- Mirror sync loop: `httptest.Server` upstream serving `200` → `304` → `500`
  → new revision, asserting the local state after each step.

## 14. Implementation order

1. `internal/remoteconfig`: types, `Parse`, `Match`, `Effective`,
   `Canonical`, fixtures. Copy the fixture directory to the updater repo.
2. Migrations 10 and 11 + `tasks.json` entries; `models.RemoteConfigRevision`.
3. `GET /v2/config` handler and route; client tracking on fetch; stats
   additions.
4. Admin routes §7 in order: `validate`, `revisions` (list/get/create),
   `publish`, `rollback`, `preview`, `delete`.
5. Mirror replication (§9) and the `CONFIG_UPSTREAM_*` variables.
6. `openapi.yaml` (new `Config` tag, every route, the `Document` schema
   referenced from the client spec), `CLAUDE.md` handler conventions
   (the `204` rule next to the existing "never 404" rule), `DOCS.md`.

Steps 1–3 are enough for the client rollout described in the client spec
§15; until step 4 lands, a revision can be inserted with a one-off script
that uses the same package.

## 15. Open questions

- **Attribution.** `created_by` from the dashboard session is the cheapest
  path; if the dashboard calls the API with the admin key only, every
  revision is anonymous. Decide whether the dashboard must send its session
  token on config writes.
- **Mirror bootstrap.** A brand-new mirror answers `204` until its first
  successful upstream sync. Acceptable, or should deployment seed it?
- **`X-Config-Revision` header.** Convenience for access logs and for
  `curl`; not part of the client contract. Keep or drop.
- **Fixture sync.** Manual copy like the `.proto`, or a CI job in each repo
  that fails when `testdata/remoteconfig/` differs from the other repo's
  main branch.
