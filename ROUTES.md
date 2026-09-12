# ROUTES.md — Mappa completa delle route

Elenco di ogni endpoint esposto da **emly-api-go**, con autenticazione richiesta,
parametri e comportamento. Generato dal codice in `internal/routes/` e
`internal/handlers/`.

Per l'architettura generale vedi [CLAUDE.md](CLAUDE.md); per la guida estesa
vedi [DOCS.md](DOCS.md).

---

## Indice

1. [Come leggere questa tabella](#1-come-leggere-questa-tabella)
2. [Middleware globali](#2-middleware-globali)
3. [Route root](#3-route-root)
4. [API v1 — `/v1`](#4-api-v1--v1)
5. [API v2 — `/v2`](#5-api-v2--v2)
   - [Bug report](#51-bug-report--v2apibug-report)
   - [Admin](#52-admin--v2apiadmin)
   - [Updates (client EMLy)](#53-updates-client-emly--v2updates)
   - [Self-update dell'Updater](#54-self-update-dellupdater--v2updates)
   - [Remote config](#55-remote-config--v2config)
   - [Ban permanenti](#56-ban-permanenti--v2bans)
   - [Statistiche](#57-statistiche--v2stats)
   - [Stream WebSocket](#58-stream-websocket--v2statsstream)
6. [Riepilogo autenticazione](#6-riepilogo-autenticazione)

---

## 1. Come leggere questa tabella

La colonna **Auth** usa queste sigle:

| Sigla        | Significato                                                       |
|--------------|-------------------------------------------------------------------|
| `—`          | Pubblica, nessuna credenziale richiesta                           |
| `API`        | Header `X-API-Key` valido (middleware `APIKeyAuth`, 401 altrimenti) |
| `ADMIN`      | Header `X-Admin-Key` valido (middleware `AdminKeyAuth`, 401 altrimenti) |
| `API+ADMIN`  | Entrambi gli header insieme                                       |
| `SESSION`    | Header `X-Session-Token` valido, verificato dall'handler stesso    |
| `ADMIN (WS)` | `X-Admin-Key` controllato dall'handler prima dell'upgrade WebSocket, con fallback `?admin_key=` |

Tutti i gruppi applicano anche `apimw.RouteLimitByIP(30, time.Minute)`, oltre
al rate limiter custom descritto sotto. Entrambi esentano chi presenta un
`X-Dashboard-Key` valido. Tutte le risposte sono JSON tramite
`jsonOK` / `jsonCreated` / `jsonError`, tranne i download binari.

Il corpo di errore è sempre nella forma:

```json
{ "error": "descrizione del problema" }
```

---

## 2. Middleware globali

Catena applicata in `main.go` a ogni richiesta, nell'ordine:

```
RequestID → RealIP → AccessLog → Recoverer → Timeout(30s) → Timing
          → [otelhttp, se OTEL_ENABLED] → BanList → RateLimiter
```

- **BanList** (`internal/middleware/ban.go`) rifiuta con `403` gli identificatori
  presenti nella tabella `bans` (IP, HWID, hostname). Un `X-Admin-Key` valido è
  esente, altrimenti bannare l'IP dell'ufficio bloccherebbe anche la route che
  rimuove il ban.
- **RateLimiter** (`internal/middleware/ratelimit.ban.go`) è a due livelli per IP:
  non autenticato (`RL_UNAUTH_*`) e autenticato (`RL_AUTH_*`). Dopo `MaxFails`
  violazioni banna l'IP in memoria per `BanDur`. IP privati/loopback e richieste
  con `X-Dashboard-Key` valido lo bypassano.
- **RouteLimitByIP** (`internal/middleware/ratelimit.route.go`) è il limite per
  gruppo di route: `httprate.LimitByIP` con la stessa esenzione dashboard. La
  dashboard rende ogni pagina lato server, quindi tutto il suo traffico esce da
  un solo indirizzo per conto di qualunque admin stia navigando: un budget per
  IP pensato per un chiamante solo verrebbe diviso fra tutto lo staff.

I router `/v1` e `/v2` riapplicano il RateLimiter e aggiungono gli header di
risposta `X-Server` e `X-Powered-By`.

---

## 3. Route root

| Metodo | Path                | Auth | Cosa fa |
|--------|---------------------|------|---------|
| `GET`  | `/`                 | `—`  | Ping. Risponde con il testo `emly-api-go`. |
| `GET`  | `/health`           | `—`  | Stato del servizio. `{"status":"ok","db":"ok"}`; `503` con `"db":"error"` se il ping MySQL (timeout 3s) fallisce. |
| `POST` | `/api/bug-reports`  | `API` | Alias legacy della creazione bug report v1. Stesso handler di `POST /v1/api/bug-reports/`. Deprecato insieme a v1: emette lo stesso warning, pur essendo montato fuori dal router v1. |

---

## 4. API v1 — `/v1`

> **Deprecata. Viene disattivata dopo il 31 ottobre 2026.**
>
> Ogni richiesta che arriva su una route v1 produce una riga di log a livello
> **warn** (`deprecated API version used`) con metodo, path, IP, hostname,
> HWID e User-Agent del chiamante, più la data di spegnimento. Serve a trovare
> chi è ancora su v1 finché c'è tempo per spostarlo: gli identificatori sono
> gli stessi su cui è indicizzata `updater_clients`, quindi un hostname o un
> HWID trovato nei log si cerca lì direttamente.
>
> Il log non è campionato di proposito. Un warning campionato lascerebbe
> invisibile proprio il chiamante raro, quello che nessuno si ricorda di aver
> messo in produzione, fino al giorno in cui lo spegnimento lo rompe.
>
> Ogni route v1 ha un equivalente v2. Le uniche differenze sono il prefisso, il
> plurale in `bug-reports` (in v2 è singolare) e il rate limit sul gruppo di
> auth admin. La costante è `v1.SunsetDate` in `internal/routes/v1/v1.go`.

### 4.1 Health

| Metodo | Path          | Auth | Cosa fa |
|--------|---------------|------|---------|
| `GET`  | `/v1/health`  | `—`  | Come `/health`, senza il campo `config_upstream`. |

### 4.2 Bug report — `/v1/api/bug-reports`

| Metodo   | Path                              | Auth        | Cosa fa |
|----------|-----------------------------------|-------------|---------|
| `POST`   | `/`                               | `API`       | Crea un bug report da `multipart/form-data`. |
| `GET`    | `/count`                          | `API`       | Conteggio dei report, opzionalmente filtrato per stato. |
| `GET`    | `/`                               | `API+ADMIN` | Elenco paginato dei report. |
| `GET`    | `/{id}`                           | `API+ADMIN` | Un singolo report con i suoi metadati. |
| `GET`    | `/{id}/status`                    | `API+ADMIN` | Solo lo stato: `{"status":"new"}`. |
| `GET`    | `/{id}/files`                     | `API+ADMIN` | Elenco degli allegati del report. |
| `GET`    | `/{id}/files/{file_id}`           | `API+ADMIN` | Scarica un singolo allegato (da S3 o dal blob in DB). |
| `GET`    | `/{id}/download`                  | `API+ADMIN` | ZIP in memoria con il report renderizzato più tutti gli allegati. |
| `PATCH`  | `/{id}/status`                    | `API+ADMIN` | Cambia lo stato del report. |
| `DELETE` | `/{id}`                           | `API+ADMIN` | Elimina il report e i suoi file. |

**`POST /` — campi del form**

| Campo          | Obbligatorio | Note |
|----------------|--------------|------|
| `name`         | sì           | `400` se vuoto |
| `email`        | sì           | `400` se vuoto |
| `description`  | sì           | `400` se vuoto |
| `hwid`         | no           | |
| `hostname`     | no           | |
| `os_user`      | no           | |
| `system_info`  | no           | JSON; se contiene `InternalIP` viene usato come IP del mittente |
| `screenshot`   | no           | file, default MIME `image/png` |
| `mail_file`    | no           | file, default MIME `message/rfc822` |
| `localstorage` | no           | file, default MIME `application/json` |
| `config`       | no           | file, default MIME `application/json` |

Limite multipart 32 MiB. L'IP del mittente viene preso da `system_info.InternalIP`,
poi da `X-Forwarded-For`, poi da `X-Real-IP`, infine `"unknown"`. Risposta `201`.

**`GET /count` — query string**

| Parametro | Valori ammessi                          |
|-----------|-----------------------------------------|
| `status`  | `new`, `in_review`, `resolved`, `closed` |

Uno stato non valido risponde `400`.

**`GET /` — query string**

| Parametro   | Default | Note |
|-------------|---------|------|
| `page`      | `1`     | |
| `page_size` | vedi handler | |
| `search`    | vuoto   | ricerca testuale |

**`PATCH /{id}/status` — corpo JSON**

```json
{ "status": "in_review" }
```

Valori ammessi `new`, `in_review`, `resolved`, `closed`; `404` se il report non esiste.

### 4.3 Autenticazione dashboard — `/v1/api/admin/auth`

| Metodo | Path        | Auth      | Cosa fa |
|--------|-------------|-----------|---------|
| `POST` | `/login`    | `—`       | Login con username e password, restituisce un token di sessione. |
| `GET`  | `/validate` | `SESSION` | Verifica il token di sessione e restituisce l'utente. |
| `POST` | `/logout`   | `SESSION` | Invalida il token di sessione. |

Solo `/login` è rate-limitato: è l'unico endpoint esposto al brute force.
`/validate` e `/logout` richiedono già un token da 256 bit e vengono chiamati
di frequente dai client autenticati. In v2 il limite copre tutto il gruppo, ma
la dashboard ne è esente (vedi §2).

**`POST /login` — corpo JSON**

```json
{ "username": "admin", "password": "..." }
```

Esiti: `400` campi mancanti, `401` credenziali errate, `403` account disabilitato.
Le password sono verificate in formato PHC.

### 4.4 Gestione utenti — `/v1/api/admin/users`

| Metodo   | Path                    | Auth    | Cosa fa |
|----------|-------------------------|---------|---------|
| `GET`    | `/`                     | `ADMIN` | Elenca tutti gli utenti (senza campi sensibili). |
| `POST`   | `/`                     | `ADMIN` | Crea un utente. `409` se lo username esiste già. |
| `GET`    | `/{id}`                 | `ADMIN` | Un utente per ID. `404` se assente. |
| `PATCH`  | `/{id}`                 | `ADMIN` | Aggiorna `displayname` e/o `enabled`. `400` se non ci sono campi aggiornabili. |
| `POST`   | `/{id}/reset-password`  | `ADMIN` | Imposta una nuova password. |
| `DELETE` | `/{id}`                 | `ADMIN` | Elimina l'utente. |

**`POST /` — corpo JSON**

```json
{ "username": "mario", "displayname": "Mario Rossi", "password": "...", "role": "user" }
```

`role` deve essere `admin` o `user`.

**`POST /{id}/reset-password` — corpo JSON**

```json
{ "password": "nuova-password" }
```

### 4.5 Alias legacy

| Metodo   | Path                             | Auth        | Cosa fa |
|----------|----------------------------------|-------------|---------|
| `DELETE` | `/v1/api/admin/bug-reports/{id}` | `API+ADMIN` | Stesso handler di `DELETE /v1/api/bug-reports/{id}`. |

---

## 5. API v2 — `/v2`

| Metodo | Path         | Auth | Cosa fa |
|--------|--------------|------|---------|
| `GET`  | `/v2/health` | `—`  | Come `/health`, più il campo `config_upstream` quando l'istanza è un site mirror (`CONFIG_UPSTREAM_URL` impostata). |

> Nota sul path: in v1 il gruppo è `bug-reports` (plurale), in v2 è
> `bug-report` (singolare). Non è un refuso di questo documento.

### 5.1 Bug report — `/v2/api/bug-report`

Identico a [4.2](#42-bug-report--v1apibug-reports) per handler, parametri e
autenticazione. Cambia solo il prefisso.

| Metodo   | Path                     | Auth        |
|----------|--------------------------|-------------|
| `POST`   | `/`                      | `API`       |
| `GET`    | `/count`                 | `API`       |
| `GET`    | `/`                      | `API+ADMIN` |
| `GET`    | `/{id}`                  | `API+ADMIN` |
| `GET`    | `/{id}/status`           | `API+ADMIN` |
| `GET`    | `/{id}/files`            | `API+ADMIN` |
| `GET`    | `/{id}/files/{file_id}`  | `API+ADMIN` |
| `GET`    | `/{id}/download`         | `API+ADMIN` |
| `PATCH`  | `/{id}/status`           | `API+ADMIN` |
| `DELETE` | `/{id}`                  | `API+ADMIN` |

### 5.2 Admin — `/v2/api/admin`

Stessi handler della v1. Differenza: in v2 il rate limit è applicato all'intero
gruppo `/admin`, quindi anche `/auth/validate` e `/auth/logout` sono limitati —
per chi non presenta `X-Dashboard-Key`. L'alias legacy `admin/bug-reports` non
esiste in v2.

| Metodo   | Path                          | Auth      |
|----------|-------------------------------|-----------|
| `POST`   | `/auth/login`                 | `—`       |
| `GET`    | `/auth/validate`              | `SESSION` |
| `POST`   | `/auth/logout`                | `SESSION` |
| `GET`    | `/users/`                     | `ADMIN`   |
| `POST`   | `/users/`                     | `ADMIN`   |
| `GET`    | `/users/{id}`                 | `ADMIN`   |
| `PATCH`  | `/users/{id}`                 | `ADMIN`   |
| `POST`   | `/users/{id}/reset-password`  | `ADMIN`   |
| `DELETE` | `/users/{id}`                 | `ADMIN`   |

### 5.3 Updates (client EMLy) — `/v2/updates`

| Metodo   | Path                                 | Auth    | Cosa fa |
|----------|--------------------------------------|---------|---------|
| `GET`    | `/manifest`                          | `—`     | Manifest degli aggiornamenti EMLy. Registra un evento `manifest_check`. |
| `GET`    | `/releases/{version}/download`        | `—`     | Scarica l'installer della release dal bucket updates. |
| `GET`    | `/releases`                          | `ADMIN` | Elenca le release. |
| `POST`   | `/releases`                          | `ADMIN` | Crea una release caricando l'installer. |
| `PUT`    | `/releases/{version}`                | `ADMIN` | Sostituisce tutti i metadati della release. |
| `PATCH`  | `/releases/{version}`                | `ADMIN` | Aggiorna solo i campi presenti nel corpo. |
| `DELETE` | `/releases/{version}`                | `ADMIN` | Elimina la release e il file su S3. |
| `PATCH`  | `/releases/{version}/channel`        | `ADMIN` | Cambia solo i flag `is_stable` / `is_beta`. |

**`GET /manifest` — forma della risposta**

Aggrega tutte le righe di `update_releases` in un unico documento:

| Campo                    | Contenuto |
|--------------------------|-----------|
| `stable_version` / `stable_download` | versione e URL della release con `is_stable` |
| `beta_version` / `beta_download`     | versione e URL della release con `is_beta` |
| `min_required_version`   | preso dalla release stabile |
| `is_critical` / `critical_version` | attivi se una qualsiasi release ha `is_critical` |
| `sha256_checksums`       | mappa versione → checksum |
| `release_notes`          | mappa versione → nota breve |
| `detailed_release_notes` | mappa versione → `{severity_type, description:{en,it}}`, solo per severità diversa da `none` |

Gli URL di download sono costruiti dallo `Host` della richiesta, rispettando
`X-Forwarded-Proto` e `X-Forwarded-Host`, così un mirror interno serve link che
puntano a sé stesso senza configurazione per sito.

**`GET /releases` — query string**

| Parametro | Valori                                  |
|-----------|-----------------------------------------|
| `channel` | `stable`, `beta`, `archived`, oppure assente per tutte |

Un valore diverso risponde `400`.

**`POST /releases` — campi del form (`multipart/form-data`)**

| Campo                  | Obbligatorio | Note |
|------------------------|--------------|------|
| `version`              | sì           | `400` se vuoto |
| `file`                 | sì           | l'installer; lo SHA-256 è calcolato dal server |
| `is_stable`            | no           | `true` o `1` |
| `is_beta`              | no           | `true` o `1` |
| `short_note`           | no           | |
| `severity_type`        | no           | `none` (default), `security`, `bugfix`, `feature` |
| `description_en`       | no           | |
| `description_it`       | no           | |
| `is_critical`          | no           | `true` o `1` |
| `critical_version`     | no           | |
| `min_required_version` | no           | |
| `released_at`          | no           | RFC3339, default adesso |

`is_stable` e `is_beta` sono indipendenti: la stessa release può occupare
entrambi gli slot del manifest. Impostare uno dei due a `true` lo azzera sulla
release che lo deteneva prima, nella stessa transazione. Stessa cosa per
`is_critical`. `503` se il bucket updates non è configurato.

**`PATCH /releases/{version}` — corpo JSON**

Tutti i campi sono opzionali; assente significa "non toccare". `released_at`
deve essere RFC3339. `400` se il corpo non contiene nessun campo aggiornabile.

**`PATCH /releases/{version}/channel` — corpo JSON**

```json
{ "is_stable": true, "is_beta": false }
```

Almeno uno dei due è richiesto, altrimenti `400`.

**`GET /releases/{version}/download`**

Streaming tramite `streamInstaller`, mai un `io.Copy` nudo. Una volta inviati
lo stato `200` e il `Content-Length` nulla può più diventare un errore HTTP,
quindi una copia incompleta viene registrata come **warn**
(`installer download did not complete`) con la causa, i byte inviati rispetto
agli attesi, il throughput medio e l'identità del client. `404` se la release o
il file non esistono, `503` se S3 non è configurato.

### 5.4 Self-update dell'Updater — `/v2/updates`

Superficie separata, con la sua tabella `updater_releases` e il suo prefisso S3
(`S3_UPDATER_PREFIX`, default `updater`) nello stesso bucket updates.

| Metodo   | Path                                 | Auth    | Cosa fa |
|----------|--------------------------------------|---------|---------|
| `GET`    | `/manifest/updater`                  | `API`   | Manifest di self-update dell'Updater. |
| `GET`    | `/download/updater/{version}`        | `—`     | Scarica l'installer dell'Updater. |
| `GET`    | `/updater/releases`                  | `ADMIN` | Elenca le release dell'Updater. |
| `POST`   | `/updater/releases`                  | `ADMIN` | Crea una release caricando l'installer. |
| `PATCH`  | `/updater/releases/{version}`        | `ADMIN` | Aggiorna i metadati della release. |
| `DELETE` | `/updater/releases/{version}`        | `ADMIN` | Elimina la release e il file su S3. |

**`GET /manifest/updater`**

Risponde `200` in ogni caso non di errore. Un catalogo vuoto si serializza come
`{"version": ""}`, che il client tratta come no-op silenzioso. **Qui non va mai
restituito `404`**: il client lo legge come "questo mirror non implementa ancora
l'endpoint" e smette di riprovare, ed è esattamente questo che permette ai
mirror interni non ancora aggiornati di convivere. Un errore DB risponde `500`,
così il client riprova al ciclo successivo con backoff.

Campi della risposta: `version`, `download`, `sha256`, `size`, `published_at`
(RFC3339 UTC) e `release_notes` come mappa `{it, en}` quando ci sono note.

Al massimo una riga di `updater_releases` ha `is_current`; azzerarlo ovunque è
il kill-switch.

**`POST /updater/releases` — campi del form**

| Campo          | Obbligatorio | Note |
|----------------|--------------|------|
| `version`      | sì           | semver senza `v` iniziale, es. `1.5.0`; `400` altrimenti |
| `file`         | sì           | l'installer |
| `is_current`   | no           | `true` o `1`; lo azzera sulla release precedente |
| `notes_it`     | no           | |
| `notes_en`     | no           | |
| `published_at` | no           | RFC3339, default adesso |

**`GET /download/updater/{version}`**

Resta pubblico come il download delle release EMLy: il link del manifest può
legittimamente passare da un mirror o da una CDN che non inoltra l'API key.

### 5.5 Remote config — `/v2/config`

Documento di policy per la flotta, servito all'Updater e a EMLy. Specifica in
`docs/superpowers/specs/2026-09-04-remote-config-api-design.md`.

| Metodo   | Path                             | Auth    | Cosa fa |
|----------|----------------------------------|---------|---------|
| `GET`    | `/`                              | `API`   | Il documento pubblicato corrente. |
| `POST`   | `/validate`                      | `ADMIN` | Valida un documento senza salvarlo. |
| `POST`   | `/preview`                       | `ADMIN` | Calcola la configurazione effettiva per un host fittizio. |
| `GET`    | `/revisions`                     | `ADMIN` | Elenco paginato delle revisioni. |
| `POST`   | `/revisions`                     | `ADMIN` | Crea una revisione, opzionalmente pubblicandola. |
| `GET`    | `/revisions/{revision}`          | `ADMIN` | Una revisione con il suo documento. |
| `DELETE` | `/revisions/{revision}`          | `ADMIN` | Elimina una revisione ancora in bozza. |
| `POST`   | `/revisions/{revision}/publish`  | `ADMIN` | Pubblica una bozza. |
| `POST`   | `/rollback`                      | `ADMIN` | Clona il contenuto di una vecchia revisione in una nuova. |

**`GET /`**

Risponde con il documento canonico, più gli header `ETag`,
`X-Config-Revision` e `Cache-Control: no-cache`. Un `If-None-Match` che
corrisponde riceve `304`. Quando non c'è ancora nulla di pubblicato risponde
**`204`**, mai `404`: un client che tratta ogni `4xx` come un guasto
registrerebbe un errore su ogni macchina a ogni ciclo fino alla prima
pubblicazione, mentre `204` significa "raggiungibile, niente da darti, tieni
quello che hai". Ogni fetch aggiorna la telemetria del client.

**`POST /validate` e `POST /revisions` — corpo JSON**

```json
{ "document": { }, "notes": "opzionale", "publish": false }
```

`validate` applica le stesse regole del client Updater e restituisce l'elenco
dei problemi. `revisions` accetta anche `notes` e `publish`; il documento ha un
tetto di 1 MiB (`413` oltre). I campi `revision` e `generatedAt` presenti nel
documento inviato generano un avviso: sono assegnati dal server.

**`GET /revisions` — query string**

| Parametro   | Valori |
|-------------|--------|
| `page`      | default `1` |
| `page_size` | |
| `status`    | `draft`, `published`, `superseded` |

**`POST /rollback` — corpo JSON**

```json
{ "to": 12, "notes": "opzionale" }
```

Il rollback **non** ripubblica la vecchia revisione: ne clona il contenuto in una
revisione nuova e con numero più alto, perché un numero più basso verrebbe
ignorato da ogni client. `404` se la revisione di origine non esiste o è ancora
una bozza.

**`POST /revisions/{revision}/publish`**

`409` se la revisione è già pubblicata, se è superseded (va usato il rollback) o
se nel frattempo ne è stata pubblicata una più recente. La riga precedente viene
superseded nella stessa transazione.

**`POST /preview` — corpo JSON**

```json
{
  "revision": 12,
  "host": { "hwid": "...", "hostname": "...", "dc": "...", "ips": ["10.0.0.1"], "domain": "...", "now": "2026-09-04T10:00:00Z" }
}
```

Esattamente uno tra `revision` e `document` è richiesto, altrimenti `400`.
`host.now` deve essere RFC3339.

**Mirror di sito.** `POST /revisions`, `POST /revisions/{revision}/publish`,
`POST /rollback` e `DELETE /revisions/{revision}` rispondono **`405`** quando
`CONFIG_UPSTREAM_URL` è impostata: un mirror replica soltanto, non accetta
scritture. Il messaggio di errore indica l'upstream su cui pubblicare.

Le revisioni sono append-only: `remote_config_revisions.document` non cambia mai
dopo l'insert.

### 5.6 Ban permanenti — `/v2/bans`

Block list impostata dall'operatore, applicata dal middleware `BanList`. È un
meccanismo distinto dai ban automatici del rate limiter, che restano in memoria
e a tempo.

| Metodo   | Path      | Auth    | Cosa fa |
|----------|-----------|---------|---------|
| `GET`    | `/`       | `ADMIN` | Elenca i ban attivi. |
| `POST`   | `/`       | `ADMIN` | Crea un ban e ricarica subito lo snapshot in memoria. |
| `DELETE` | `/{id}`   | `ADMIN` | Rimuove un ban e ricarica lo snapshot. `404` se l'ID non esiste. |

**`POST /` — corpo JSON**

```json
{ "ban_type": "hwid", "value": "ABC123", "reason": "abuso" }
```

`ban_type` deve essere `ip`, `hwid` o `hostname`. Il valore è normalizzato prima
del salvataggio: gli hostname in minuscolo, gli IP attraverso `net.ParseIP` (così
due scritture dello stesso indirizzo non diventano due righe che mancano
entrambe il bersaglio), gli HWID verbatim perché sono confrontati byte per byte
con l'header.

La lista è in cache in memoria, aggiornata da un ticker ogni 30 secondi (così una
seconda istanza dell'API vede i ban creati sulla prima) e subito dopo ogni
scrittura admin. Se il refresh fallisce lo snapshot precedente resta valido:
l'applicazione dei ban non deve né aprirsi né chiudersi per un singhiozzo del DB.

### 5.7 Statistiche — `/v2/stats`

| Metodo | Path             | Auth    | Cosa fa |
|--------|------------------|---------|---------|
| `GET`  | `/summary`       | `ADMIN` | Aggregati della flotta, memoizzati. |
| `GET`  | `/clients`       | `ADMIN` | Elenco paginato dei client noti. |
| `GET`  | `/clients/{id}`  | `ADMIN` | Un client con i suoi eventi recenti. `404` se l'ID non esiste. |
| `GET`  | `/events`        | `ADMIN` | Serie temporale degli eventi, a bucket. |

**`GET /summary` — query string**

| Parametro        | Valori |
|------------------|--------|
| `window_minutes` | finestra per il conteggio dei client connessi |
| `product`        | `emly`, `updater`, `all` |

Risposta: `total_clients`, `connected_clients`, `window_minutes`, `product`,
`events_last_24h`, `clients_by_version`, `clients_by_config_revision`.

Il payload è memoizzato dietro un `ttlCache` (`statscache.go`), con chiave
`product|window_minutes` e durata `STATS_CACHE_TTL` (default 30s, `0` disabilita).
Le dashboard fanno polling continuo su aggregati a 24 ore, quindi le query girano
una volta per TTL invece che una volta per richiesta, e più chiamate concorrenti
su una chiave fredda collassano in una sola build. Quello che si aggiunge al
sommario va in `buildStatsSummary`, dietro la cache, non nell'handler.

**`GET /clients` — query string**

| Parametro        | Default | Note |
|------------------|---------|------|
| `page`           | `1`     | |
| `page_size`      |         | |
| `online`         | `false` | `true` filtra i soli client visti nella finestra |
| `window_minutes` |         | definisce "online" |

**`GET /events` — query string**

| Parametro    | Valori |
|--------------|--------|
| `bucket`     | `day` (default) o `hour`; `400` altrimenti |
| `event_type` | filtro sul tipo di evento |
| `product`    | `emly`, `updater`, `all` |
| `from`       | RFC3339 |
| `to`         | RFC3339 |

**Telemetria dei client.** Gli header `X-EMLy-*` (`Hostname`, `HWID`, `ADDomain`,
`LoggedUser`, `Serial`, `Product`, `AppVersion`) sono letti in un solo punto,
`clientIdentityFromRequest`, insieme alla versione e al contatto estratti dallo
User-Agent e all'IP del peer. Aggiungere un header significa aggiungerlo lì, non
nei singoli call site. Una richiesta senza né HWID né hostname viene servita ma
non tracciata. Un header che il client non invia non azzera mai il valore già
memorizzato: l'Updater omette gli header per cui non ha un valore, quindi
"assente" vuol dire "sconosciuto". Per questo `logged_user` è un'istantanea da
leggere insieme a `last_seen_at`, non uno storico.

### 5.8 Stream WebSocket — `/v2/stats/stream`

| Metodo | Path                | Auth         | Cosa fa |
|--------|---------------------|--------------|---------|
| `GET`  | `/v2/stats/stream`  | `ADMIN (WS)` | Upgrade WebSocket che spinge aggiornamenti live alle dashboard. |

L'autenticazione avviene **prima** dell'upgrade: `X-Admin-Key`, oppure
`?admin_key=` come ripiego per i proxy che rimuovono gli header custom sulla
richiesta di Upgrade. Una chiave errata riceve `401` senza che l'upgrade venga
nemmeno tentato, così il client distingue subito "chiave sbagliata" da "problema
di rete".

**Messaggi dal client**

| `type`        | Effetto |
|---------------|---------|
| `subscribe`   | Aggiunge canali e invia subito uno snapshot di ognuno di quelli citati. |
| `unsubscribe` | Rimuove canali. |
| `ping`        | Il server risponde `pong`. |
| `pong`        | Nessuna azione: la lettura ha già azzerato il timer di inattività. |

Un `type` sconosciuto o un JSON non valido ricevono un messaggio `error` con
codice `invalid_params`.

**Canali**

| Canale          | Contenuto |
|-----------------|-----------|
| `stats:summary` | Lo stesso payload di `GET /summary`. |
| `stats:clients` | `{"clients": [...]}` con tutti i client. |
| `stats:events`  | Lo stesso payload di `GET /events`. |

**`subscribe` — forma del messaggio**

```json
{
  "type": "subscribe",
  "channels": ["stats:summary", "stats:events"],
  "params": {
    "window_minutes": 15,
    "product": "updater",
    "events": { "bucket": "hour", "event_type": "manifest_check", "from": "...", "to": "..." }
  }
}
```

I canali si aggiungono, non sostituiscono: `unsubscribe` è l'unico modo per
toglierne uno. Ogni canale citato in un `subscribe` riceve comunque uno snapshot
immediato, anche se la connessione era già iscritta: è così che un client applica
un filtro nuovo senza riconnettersi. I canali sconosciuti generano un `error`
singolo senza far cadere il resto della richiesta.

**Messaggi dal server**: `snapshot`, `update`, `ping`, `pong`, `error`, ognuno
con `type`, `channel`, `ts` e `data`.

Gli aggiornamenti partono quando viene ingerita una riga di `updater_events` che
rientra nel filtro della sottoscrizione, e a ogni tick periodico
(`STATS_STREAM_TICK_INTERVAL`, default 30s) per la risincronizzazione. Il bus è
in-process (`internal/statshub`) e quindi a istanza singola per scelta: in questo
stack non ci sono né Postgres LISTEN/NOTIFY né Redis.

---

## 6. Riepilogo autenticazione

| Header            | Dove viene usato | Fallimento |
|-------------------|------------------|------------|
| `X-API-Key`       | Creazione bug report, manifest dell'Updater, `GET /v2/config` | `401` |
| `X-Admin-Key`     | Tutte le route admin, releases, config writes, bans, stats | `401` |
| `X-Dashboard-Key` | Bypass di entrambi i rate limiter, globale e per gruppo di route | nessuno, la richiesta prosegue limitata |
| `X-Session-Token` | `auth/validate`, `auth/logout` | `401` o `403` |

`API_KEY` e `ADMIN_KEY` accettano una lista separata da virgole, ma viene usato
solo il primo valore non vuoto.

Gli header `X-EMLy-*` non autenticano nulla: alimentano la telemetria e sono
confrontati con la block list.
