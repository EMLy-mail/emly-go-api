# EMLy API — Documentazione per sviluppatori Node/PHP

Questa guida spiega l'intera architettura dell'API **emly-api-go** assumendo che tu conosca Node.js (Express/Fastify) o PHP (Laravel/Slim), ma che non abbia mai scritto Go.

---

## Indice

1. [Go per chi viene da Node/PHP](#1-go-per-chi-viene-da-nodephp)
2. [Struttura del progetto](#2-struttura-del-progetto)
3. [Setup e avvio](#3-setup-e-avvio)
4. [go-chi: il router](#4-go-chi-il-router)
5. [Middleware](#5-middleware)
6. [Handler (i controller)](#6-handler-i-controller)
7. [Modelli e database](#7-modelli-e-database)
8. [Sistema di autenticazione](#8-sistema-di-autenticazione)
9. [Migrazioni del database](#9-migrazioni-del-database)
10. [Endpoints API completi](#10-endpoints-api-completi)
11. [Come aggiungere un nuovo endpoint](#11-come-aggiungere-un-nuovo-endpoint)

---

## 1. Go per chi viene da Node/PHP

### Analogie rapide

| Concetto               | Node/Express          | PHP/Laravel         | Go (questo progetto)              |
|------------------------|-----------------------|---------------------|-----------------------------------|
| Entry point            | `index.js`            | `public/index.php`  | `main.go`                         |
| Router                 | Express, Fastify      | Router Laravel      | `go-chi/chi`                      |
| Middleware             | `app.use(...)`        | Middleware Laravel  | `r.Use(...)`                      |
| Controller             | `req, res` function   | Controller class    | `http.HandlerFunc`                |
| ORM / Query builder    | Sequelize, Knex       | Eloquent            | `sqlx` (query raw con struct)     |
| `.env`                 | `dotenv`              | `vlucas/phpdotenv`  | `joho/godotenv`                   |
| Tipi / interfacce      | TypeScript interfaces | PHP types           | `struct`                          |
| `package.json`         | `package.json`        | `composer.json`     | `go.mod`                          |
| Hot reload             | `nodemon`             | N/A                 | `air`                             |

### Differenze chiave da tenere a mente

**Go compila tutto in un singolo binario.** Non c'e' un runtime separato come Node. Il risultato di `go build` e' un `.exe` (o file ELF su Linux) che contiene tutto: il server, i template, le query SQL — tutto embedded.

**Niente `null` implicito.** In Go ogni variabile ha sempre un valore di default (0, "", false, nil). Gli errori si gestiscono con valori di ritorno, non con eccezioni:

```go
// Node
try {
  const result = await db.query(sql)
} catch (err) {
  console.error(err)
}

// Go
result, err := db.QueryContext(ctx, sql)
if err != nil {
    // gestisci l'errore
    return
}
```

**I package sono come i moduli ES.** Il nome del package in cima al file (es. `package handlers`) e' l'equivalente di un namespace. Le funzioni con la prima lettera maiuscola (`CreateBugReport`) sono pubbliche (exported); quelle minuscole (`jsonError`) sono private al package.

**Le funzioni ritornano valori multipli.** In Go e' normale avere `(result, error)` come return type. Non ci sono Promise; il codice e' sincrono (il parallelismo si fa con goroutine, non usate direttamente in questo progetto).

**I `struct` sono i "model".** Non ci sono classi in Go. Uno `struct` con tag `db:"..."` e `json:"..."` funziona sia da schema DB (come un Model Eloquent) che da DTO JSON.

---

## 2. Struttura del progetto

```
emly-api-go/
├── main.go                          # Entry point: boot server, DB, middleware globali
├── go.mod                           # Dipendenze (come package.json)
├── .env                             # Variabili d'ambiente locali
│
└── internal/                        # Codice privato dell'applicazione
    ├── config/
    │   └── config.go                # Carica env vars in una struct Config
    │
    ├── database/
    │   ├── database.go              # Apre la connessione MySQL (pool)
    │   └── schema/
    │       ├── migrator.go          # Sistema di migration condizionali
    │       ├── init.sql             # Schema base (CREATE TABLE IF NOT EXISTS)
    │       └── migrations/
    │           ├── tasks.json       # Definisce le migration con condizioni
    │           ├── 1_bug_reports.sql
    │           └── 2_users.sql
    │
    ├── handlers/                    # I "controller" — una funzione per endpoint
    │   ├── response.go              # Helper: jsonOK, jsonCreated, jsonError
    │   ├── health.route.go          # GET /v1/health
    │   ├── bug_report.route.go      # Tutti gli endpoint /bug-reports
    │   ├── admin_auth.route.go      # Login, validate, logout
    │   ├── admin_users.route.go     # CRUD utenti admin
    │   └── templates/
    │       └── report.txt.tmpl      # Template testo per il file ZIP
    │
    ├── middleware/
    │   ├── apikey.go                # Verifica header X-API-Key
    │   └── adminKey.go              # Verifica header X-Admin-Key
    │
    ├── models/                      # Struct che mappano le tabelle DB e i JSON
    │   ├── bug_report.go
    │   ├── bug_report_file.go
    │   ├── user.go
    │   ├── session.go
    │   └── rate_limit_hwid.go
    │
    └── routes/
        ├── routes.go                # Monta i sub-router sul router root
        └── v1/
            ├── v1.go                # Crea il router /v1 con middleware globale v1
            ├── bug_reports.go       # Registra le rotte /bug-reports
            └── admin.go             # Registra le rotte /admin
```

### Perche' `internal/`?

In Go, tutto cio' che sta dentro `internal/` non puo' essere importato da progetti esterni. E' una convenzione per dire "questo codice e' implementazione privata, non una libreria pubblica". Equivale a non esportare un modulo in Node.

---

## 3. Setup e avvio

### Prerequisiti

- Go 1.21+
- MySQL 8+
- `air` per hot-reload in sviluppo: `go install github.com/air-verse/air@latest`

### Configurazione `.env`

Copia `.env.example` in `.env`:

```env
PORT=8080

# DSN MySQL — DEVE includere parseTime=true&loc=UTC
DB_DSN=root:secret@tcp(127.0.0.1:3306)/emly?parseTime=true&loc=UTC
DATABASE_NAME=emly

# Chiavi di autenticazione
API_KEY=la-tua-api-key
ADMIN_KEY=la-tua-admin-key

# Pool di connessioni (opzionali, hanno default)
DB_MAX_OPEN_CONNS=30
DB_MAX_IDLE_CONNS=5
DB_CONN_MAX_LIFETIME=5
```

### Comandi

```bash
# Sviluppo con hot-reload (come nodemon)
air

# Build binario di produzione
go build -o ./build/emly-api.exe .

# Avvio diretto senza build
go run .

# Test
go test ./...
go test ./internal/... -run NomeTest -v
```

### Cosa succede all'avvio

1. Carica `.env` (se presente)
2. Legge la config dalle env vars
3. Apre il pool di connessioni MySQL
4. Esegue le migrazioni (vedi sezione 9)
5. Crea il router chi e registra tutti i middleware e le rotte
6. Avvia il server HTTP su `PORT`

---

## 4. go-chi: il router

`go-chi/chi` e' l'equivalente di Express.js per Go. E' un router HTTP leggero e componibile.

### Analogia Express → Chi

```javascript
// Express
const app = express()
app.use(morgan('dev'))
app.get('/users/:id', (req, res) => { ... })
```

```go
// Chi
r := chi.NewRouter()
r.Use(middleware.Logger)
r.Get("/users/{id}", func(w http.ResponseWriter, r *http.Request) { ... })
```

### Differenze nella sintassi dei parametri

| Express       | Chi           |
|---------------|---------------|
| `/users/:id`  | `/users/{id}` |
| `req.params.id` | `chi.URLParam(r, "id")` |
| `req.query.page` | `r.URL.Query().Get("page")` |
| `req.body`    | `r.Body` (letto con `json.NewDecoder`) |
| `req.headers` | `r.Header.Get("X-API-Key")` |

### API principali di chi

#### `chi.NewRouter()`
Crea un nuovo router. Equivale a `express()` o `new Slim\App()`.

```go
r := chi.NewRouter()
```

#### `r.Use(middleware)`
Aggiunge un middleware globale al router (o al gruppo corrente). L'ordine conta: vengono eseguiti nell'ordine di registrazione.

```go
r.Use(middleware.Logger)    // loga ogni richiesta
r.Use(middleware.Recoverer) // cattura i panic (come un try/catch globale)
```

#### Metodi HTTP: `r.Get`, `r.Post`, `r.Patch`, `r.Delete`

```go
r.Get("/health", handlers.Health(db))
r.Post("/bug-reports/", handlers.CreateBugReport(db))
r.Patch("/bug-reports/{id}/status", handlers.PatchBugReportStatus(db))
r.Delete("/bug-reports/{id}", handlers.DeleteBugReportByID(db))
```

#### `r.Route(pattern, fn)` — Gruppo con prefisso

Crea un sotto-gruppo con un prefisso comune. Equivale a `express.Router()` o ai route group di Laravel.

```go
// Tutte le rotte dentro la funzione avranno il prefisso /bug-reports
r.Route("/bug-reports", func(r chi.Router) {
    r.Get("/", handlers.GetAllBugReports(db))
    r.Get("/{id}", handlers.GetBugReportByID(db))
})
```

#### `r.Group(fn)` — Gruppo senza prefisso

Crea un gruppo di rotte che condividono middleware, ma senza aggiungere un prefisso al path. Utile per applicare auth diversa a rotte dello stesso livello.

```go
r.Route("/bug-reports", func(r chi.Router) {

    // Gruppo 1: solo API key
    r.Group(func(r chi.Router) {
        r.Use(apimw.APIKeyAuth(db))
        r.Get("/count", handlers.GetReportsCount(db))
        r.Post("/", handlers.CreateBugReport(db))
    })

    // Gruppo 2: API key + Admin key
    r.Group(func(r chi.Router) {
        r.Use(apimw.APIKeyAuth(db))
        r.Use(apimw.AdminKeyAuth(db))
        r.Get("/", handlers.GetAllBugReports(db))
        r.Delete("/{id}", handlers.DeleteBugReportByID(db))
    })
})
```

#### `r.Mount(pattern, handler)` — Monta un sub-router

Incolla un router separato su un prefisso. Permette di dividere le rotte in file diversi mantenendo tutto componibile. Equivale a `app.use('/v1', v1Router)` in Express.

```go
// routes.go
r.Mount("/v1", v1.NewRouter(db))
```

#### `chi.URLParam(r, "name")` — Legge i parametri di percorso

```go
// Rotta: /bug-reports/{id}
id := chi.URLParam(r, "id") // es. "42"
```

### Middleware built-in di chi (`go-chi/chi/v5/middleware`)

Questi sono tutti usati in `main.go`:

| Middleware               | Equivalente Node/Laravel              | Cosa fa                                                   |
|--------------------------|---------------------------------------|-----------------------------------------------------------|
| `middleware.RequestID`   | `express-request-id`                  | Aggiunge un UUID univoco `X-Request-Id` ad ogni richiesta |
| `middleware.RealIP`      | `express-ip` / `TrustProxies`         | Legge l'IP reale da `X-Forwarded-For` o `X-Real-IP`       |
| `middleware.Logger`      | `morgan`                              | Loga metodo, path, status, durata su stdout               |
| `middleware.Recoverer`   | Express error handler / `rescue_from`  | Cattura i panic e ritorna 500 invece di crashare          |
| `middleware.Timeout(30s)`| `connect-timeout`                     | Cancella la richiesta se supera i 30 secondi              |

### Rate limiting (`go-chi/httprate`)

```go
// main.go — globale: max 100 req/min per IP
r.Use(httprate.LimitByIP(100, time.Minute))

// nelle rotte — per gruppo: max 30 req/min per IP
r.Use(httprate.LimitByIP(30, time.Minute))
```

---

## 5. Middleware

In questo progetto i middleware custom si trovano in `internal/middleware/`. Un middleware in Go e' una funzione che prende un `http.Handler` e restituisce un `http.Handler`.

**Concettualmente identico a Express:**

```javascript
// Express
function apiKeyAuth(req, res, next) {
    if (req.headers['x-api-key'] !== process.env.API_KEY) {
        return res.status(401).json({ error: 'unauthorized' })
    }
    next()
}
```

```go
// Go/Chi
func APIKeyAuth(_ *sqlx.DB) func(http.Handler) http.Handler {
    // Questo blocco gira UNA VOLTA sola all'avvio (come costruttore)
    allowed := map[string]struct{}{cfg.APIKey: {}}

    return func(next http.Handler) http.Handler {
        // Questo ritorna il middleware vero e proprio
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            key := r.Header.Get("X-API-Key")
            if _, ok := allowed[key]; !ok {
                w.WriteHeader(http.StatusUnauthorized)
                json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
                return // equivale a NON chiamare next()
            }
            next.ServeHTTP(w, r) // equivale a next()
        })
    }
}
```

### Middleware disponibili

| Middleware       | Header richiesto  | Applicato a                                           |
|------------------|-------------------|-------------------------------------------------------|
| `APIKeyAuth`     | `X-API-Key`       | Tutti gli endpoint `/v1/api/bug-reports/*`            |
| `AdminKeyAuth`   | `X-Admin-Key`     | Endpoint admin bug-reports + `/v1/api/admin/users/*`  |

---

## 6. Handler (i controller)

Gli handler sono in `internal/handlers/`. Ogni file corrisponde a una risorsa (es. `bug_report.route.go`).

### Pattern factory function

Gli handler NON sono funzioni dirette. Sono **factory functions** che ricevono `*sqlx.DB` e restituiscono l'handler vero. Questo permette di iniettare la dipendenza del database senza variabili globali.

```go
// Definizione
func CreateBugReport(db *sqlx.DB) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        // qui hai accesso a `db` tramite closure
    }
}

// Utilizzo nel router
r.Post("/", handlers.CreateBugReport(db))
```

**Analogo Node sarebbe:**

```javascript
function createBugReport(db) {
    return async (req, res) => {
        // uso db
    }
}
app.post('/', createBugReport(db))
```

### Response helpers (`response.go`)

Tre funzioni usate in tutti gli handler per rispondere in JSON:

```go
jsonOK(w, payload)        // HTTP 200 + JSON
jsonCreated(w, payload)   // HTTP 201 + JSON
jsonError(w, status, msg) // HTTP <status> + { "error": "msg" }
```

### Leggere il body JSON

```go
// In Go (come json.parse in Node)
var body struct {
    Username string `json:"username"`
    Password string `json:"password"`
}
if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
    jsonError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
    return
}
```

### Leggere query string

```go
page := r.URL.Query().Get("page")   // ?page=2
status := r.URL.Query().Get("status") // ?status=new
```

### Upload multipart (form-data)

```go
// Legge fino a 32MB in memoria
r.ParseMultipartForm(32 << 20)

name := r.FormValue("name")           // campo testo
file, header, err := r.FormFile("screenshot") // file upload
```

---

## 7. Modelli e database

### I modelli (`internal/models/`)

I modelli sono semplici struct con tag speciali. Non c'e' ORM: le query sono SQL raw.

```go
type BugReport struct {
    ID          uint64          `db:"id"          json:"id"`
    Name        string          `db:"name"        json:"name"`
    Email       string          `db:"email"       json:"email"`
    // json:"-" significa: NON includere mai questo campo nella risposta JSON
    PasswordHash string         `db:"password_hash" json:"-"`
}
```

- `db:"nome_colonna"` — mappa la colonna SQL al campo Go (usato da `sqlx`)
- `json:"nome_campo"` — mappa il campo Go alla chiave JSON (usato da `encoding/json`)
- `json:"-"` — campo nascosto nelle risposte JSON (es. `password_hash`, `data` dei file)
- `json:"campo,omitempty"` — il campo viene omesso dal JSON se e' zero/nil

### sqlx — Query database

`sqlx` e' un wrapper sottile su `database/sql`. Non e' un ORM: si scrive SQL a mano e si mappa il risultato su struct.

```go
// Legge UNA riga in una struct (GetContext)
// Equivale a: db.query('SELECT ... WHERE id = ?', [id]).then(rows => rows[0])
var report models.BugReport
err := db.GetContext(r.Context(), &report,
    "SELECT * FROM bug_reports WHERE id = ?", id)
if errors.Is(err, sql.ErrNoRows) {
    // nessun risultato trovato
}

// Legge PIU' righe in uno slice (SelectContext)
// Equivale a: db.query('SELECT ...').then(rows => rows)
var reports []models.BugReport
err := db.SelectContext(r.Context(), &reports,
    "SELECT * FROM bug_reports ORDER BY created_at DESC")

// Esegue INSERT/UPDATE/DELETE (ExecContext)
result, err := db.ExecContext(r.Context(),
    "INSERT INTO bug_reports (name, email) VALUES (?, ?)", name, email)
reportID, _ := result.LastInsertId()
rowsAffected, _ := result.RowsAffected()
```

**Perche' `r.Context()`?** Il context trasporta la deadline di timeout (30s impostata in `main.go`). Se la richiesta viene cancellata (timeout o client disconnect), la query SQL viene interrotta automaticamente. E' come `AbortController` in Node ma automatico.

### Tipi custom (enum-like)

Go non ha enum. Si usa un `type` su `string` con costanti:

```go
type BugReportStatus string

const (
    BugReportStatusNew      BugReportStatus = "new"
    BugReportStatusInReview BugReportStatus = "in_review"
    BugReportStatusResolved BugReportStatus = "resolved"
    BugReportStatusClosed   BugReportStatus = "closed"
)
```

---

## 8. Sistema di autenticazione

L'API ha **due livelli di autenticazione separati**, entrambi header-based (niente cookie, niente JWT per le rotte API).

### Livello 1: API Key (`X-API-Key`)

Usata da client esterni (es. l'applicazione desktop) per inviare bug report.

```
X-API-Key: la-tua-api-key
```

Il valore e' configurato in `.env` come `API_KEY`. Il middleware `APIKeyAuth` carica la chiave in una mappa all'avvio e fa un lookup O(1) ad ogni richiesta.

### Livello 2: Admin Key (`X-Admin-Key`)

Usata per accedere agli endpoint di sola lettura/scrittura amministrativa.

```
X-Admin-Key: la-tua-admin-key
```

Il valore e' configurato in `.env` come `ADMIN_KEY`.

### Livello 3: Sessione utente (`X-Session-Token`)

Gli endpoint `/v1/api/admin/auth/*` gestiscono login/logout di utenti admin con sessioni su database.

**Flusso login:**

```
POST /v1/api/admin/auth/login
Body: { "username": "...", "password": "..." }

Response: {
  "session_id": "<64-char hex token>",
  "user": { "id": "...", "username": "...", "role": "admin", ... }
}
```

**Flusso validate:**

```
GET /v1/api/admin/auth/validate
Headers: X-Session-Token: <session_id>

Response: { "success": true, "user": { ... } }
```

**Flusso logout:**

```
POST /v1/api/admin/auth/logout
Headers: X-Session-Token: <session_id>
```

**Dettagli implementativi:**
- Password hashed con **argon2id** (formato PHC, compatibile con `@node-rs/argon2`)
- Sessioni salvate in tabella `session` con scadenza a **30 giorni**
- Le sessioni scadute vengono eliminate automaticamente alla prima validazione fallita
- UUID utenti generati con `crypto/rand` (UUID v4)
- Session ID: 32 byte random, encoded come hex (64 caratteri)

---

## 9. Migrazioni del database

Il sistema di migration e' custom e si trova in `internal/database/schema/`. Non usa librerie esterne come `golang-migrate`.

### Come funziona

All'avvio, `schema.Migrate(db, cfg.Database)` esegue questi passi:

1. **Controlla se il database e' vuoto** — Se non ci sono tabelle, esegue `init.sql` che crea tutto lo schema base con `CREATE TABLE IF NOT EXISTS`.

2. **Controlla le tabelle attese** — Se il DB non e' vuoto ma mancano tabelle, riesegue `init.sql`.

3. **Esegue le migration condizionali** — Legge `migrations/tasks.json` e per ogni task valuta le condizioni. Un task viene eseguito solo se almeno una condizione e' vera.

### `tasks.json` — Definizione migration

```json
{
  "tasks": [
    {
      "id": "1_bug_reports",
      "sql_file": "1_bug_reports.sql",
      "description": "Add hostname, os_user columns and their indexes to bug_reports.",
      "conditions": [
        { "type": "column_not_exists", "table": "bug_reports", "column": "hostname" },
        { "type": "column_not_exists", "table": "bug_reports", "column": "os_user" }
      ]
    }
  ]
}
```

### Tipi di condizione supportati

| Tipo                | Esegui la migration se...                          |
|---------------------|----------------------------------------------------|
| `column_not_exists` | la colonna non esiste nella tabella                |
| `column_exists`     | la colonna esiste nella tabella                    |
| `index_not_exists`  | l'indice non esiste nella tabella                  |
| `index_exists`      | l'indice esiste nella tabella                      |
| `table_not_exists`  | la tabella non esiste nel database                 |
| `table_exists`      | la tabella esiste nel database                     |

### Come aggiungere una migration

1. Crea il file SQL in `internal/database/schema/migrations/3_nome.sql`
2. Aggiungi il task in `tasks.json` con le condizioni appropriate

```json
{
  "id": "3_nome",
  "sql_file": "3_nome.sql",
  "description": "Descrizione della migration.",
  "conditions": [
    { "type": "column_not_exists", "table": "bug_reports", "column": "nuova_colonna" }
  ]
}
```

---

## 10. Endpoints API completi

Base URL: `http://localhost:8080`

### Header di autenticazione

| Header            | Richiesto per                                         |
|-------------------|-------------------------------------------------------|
| `X-API-Key`       | Tutti gli endpoint `/v1/api/bug-reports/*`            |
| `X-Admin-Key`     | Endpoint admin bug-reports + `/v1/api/admin/users/*`  |
| `X-Session-Token` | `/v1/api/admin/auth/validate` e `/logout`             |

---

### Pubblici

#### `GET /`
Ping. Ritorna il testo `emly-api-go`.

#### `GET /v1/health`
```json
{ "status": "ok", "db": "ok" }
```

---

### Bug Reports — Solo `X-API-Key`

#### `POST /v1/api/bug-reports/`
Crea un nuovo bug report. Content-Type: `multipart/form-data`.

| Campo         | Tipo   | Obbligatorio | Descrizione                         |
|---------------|--------|:------------:|-------------------------------------|
| `name`        | string | si           | Nome del reporter                   |
| `email`       | string | si           | Email del reporter                  |
| `description` | string | si           | Descrizione del bug                 |
| `hwid`        | string | no           | Hardware ID                         |
| `hostname`    | string | no           | Nome macchina                       |
| `os_user`     | string | no           | Utente OS                           |
| `system_info` | string | no           | JSON serializzato con info sistema  |
| `attachment`  | file   | no           | File allegato generico              |
| `screenshot`  | file   | no           | Screenshot del bug                  |
| `log`         | file   | no           | File di log                         |

**Response 201:**
```json
{ "success": true, "report_id": 42, "message": "Bug report submitted successfully" }
```

#### `GET /v1/api/bug-reports/count`
```json
{ "count": 128 }
```

---

### Bug Reports — `X-API-Key` + `X-Admin-Key`

#### `GET /v1/api/bug-reports/`
Lista paginata con filtri.

| Query param | Default | Descrizione                                              |
|-------------|---------|----------------------------------------------------------|
| `page`      | 1       | Numero pagina                                            |
| `page_size` | 20      | Risultati per pagina (max 100)                          |
| `status`    | -       | Filtra per stato: `new`, `in_review`, `resolved`, `closed` |
| `search`    | -       | Cerca in hostname, os_user, name, email                  |

**Response 200:**
```json
{
  "data": [ /* array di BugReportListItem */ ],
  "total": 128,
  "page": 1,
  "page_size": 20,
  "total_pages": 7
}
```

#### `GET /v1/api/bug-reports/{id}`
Dettaglio singolo report.

#### `GET /v1/api/bug-reports/{id}/status`
```json
{ "status": "new" }
```

#### `PATCH /v1/api/bug-reports/{id}/status`
Body: stringa raw (non JSON) con il nuovo stato.
```
in_review
```

#### `GET /v1/api/bug-reports/{id}/files`
Lista dei file allegati al report (senza dati binari).

#### `GET /v1/api/bug-reports/{id}/files/{file_id}`
Scarica il file specifico. Response con `Content-Type` originale e `Content-Disposition: attachment`.

#### `GET /v1/api/bug-reports/{id}/download`
Scarica un file `.zip` contenente:
- `report.txt` — report formattato dal template
- `screenshot/nome.png`, `log/nome.log`, `attachment/nome.bin` — file allegati organizzati per ruolo

#### `DELETE /v1/api/bug-reports/{id}`
```json
{ "message": "bug report deleted successfully" }
```

---

### Auth Admin — Nessuna chiave richiesta (gestisce le proprie credenziali)

#### `POST /v1/api/admin/auth/login`
```json
// Request
{ "username": "admin", "password": "secret" }

// Response 200
{
  "session_id": "a3f9...<64 chars>",
  "user": { "id": "uuid", "username": "admin", "displayname": "Admin", "role": "admin", "enabled": true }
}
```

#### `GET /v1/api/admin/auth/validate`
Header: `X-Session-Token: <session_id>`

```json
{ "success": true, "user": { ... } }
```

#### `POST /v1/api/admin/auth/logout`
Header: `X-Session-Token: <session_id>`
```json
{ "logged_out": true }
```

---

### Gestione Utenti — Solo `X-Admin-Key`

#### `GET /v1/api/admin/users/`
Lista tutti gli utenti.

#### `POST /v1/api/admin/users/`
```json
// Request
{ "username": "mario", "displayname": "Mario Rossi", "password": "secret", "role": "user" }
// Response 201: oggetto User
```

#### `GET /v1/api/admin/users/{id}`

#### `PATCH /v1/api/admin/users/{id}`
Aggiorna solo i campi forniti. Campi modificabili: `displayname`, `enabled`.
```json
{ "displayname": "Nuovo Nome", "enabled": false }
```

#### `POST /v1/api/admin/users/{id}/reset-password`
```json
{ "password": "nuova-password" }
```

#### `DELETE /v1/api/admin/users/{id}`

---

### Modello `BugReport`

```json
{
  "id": 42,
  "name": "Mario Rossi",
  "email": "mario@example.com",
  "description": "L'app crasha all'avvio",
  "hwid": "ABC123",
  "hostname": "DESKTOP-XYZ",
  "os_user": "mario",
  "submitter_ip": "192.168.1.1",
  "system_info": { "os": "Windows 11", "ram": "16GB" },
  "status": "new",
  "created_at": "2026-03-23T10:00:00Z",
  "updated_at": "2026-03-23T10:00:00Z"
}
```

**Stati possibili:** `new` → `in_review` → `resolved` / `closed`

### Modello `BugReportFile`

```json
{
  "id": 1,
  "report_id": 42,
  "file_role": "screenshot",
  "filename": "crash.png",
  "mime_type": "image/png",
  "file_size": 204800,
  "created_at": "2026-03-23T10:00:00Z"
}
```

**Ruoli file:** `attachment`, `screenshot`, `log`

### Modello `User`

```json
{
  "id": "uuid-v4",
  "username": "mario",
  "displayname": "Mario Rossi",
  "role": "admin",
  "enabled": true,
  "created_at": "2026-03-23T10:00:00Z"
}
```

**Nota:** `password_hash` non viene mai esposto nelle risposte JSON (`json:"-"`).

---

## 11. Come aggiungere un nuovo endpoint

Esempio: aggiungere `GET /v1/api/bug-reports/{id}/summary`.

### Step 1 — Scrivi l'handler in `internal/handlers/bug_report.route.go`

```go
func GetBugReportSummary(db *sqlx.DB) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        id := chi.URLParam(r, "id")
        if id == "" {
            jsonError(w, http.StatusBadRequest, "missing id parameter")
            return
        }

        // esegui la query...
        jsonOK(w, map[string]string{"summary": "..."})
    }
}
```

### Step 2 — Registra la rotta in `internal/routes/v1/bug_reports.go`

Aggiungila nel gruppo con le permission corrette:

```go
r.Group(func(r chi.Router) {
    r.Use(apimw.APIKeyAuth(db))
    r.Use(apimw.AdminKeyAuth(db))
    r.Use(httprate.LimitByIP(30, time.Minute))

    r.Get("/", handlers.GetAllBugReports(db))
    r.Get("/{id}", handlers.GetBugReportByID(db))
    r.Get("/{id}/summary", handlers.GetBugReportSummary(db)) // <-- aggiunto qui
    // ...
})
```

### Step 3 — Build e test

```bash
go build ./...      # verifica che compili
go test ./...       # esegui i test
air                 # hot-reload in sviluppo
```

Non serve nessun file di routing separato, nessun decoratore, nessuna annotation. La rotta e' attiva immediatamente alla ricompilazione.
