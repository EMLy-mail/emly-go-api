# ROUTES.md — Mappa completa delle route

Elenco di ogni endpoint esposto da **emly-api-go**, con autenticazione richiesta,
parametri e comportamento. Generato dal codice in `internal/routes/` e nei
package per feature sotto `internal/` (`bugreports`, `admin`, `updates`,
`products`, `configapi`, `stats`, `clientws`, `bans`, `downloadqueue`, `health`), ognuno dei quali registra
le proprie route v2 nel suo `routes.go`.

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
   - [Updates per prodotto](#53-updates-per-prodotto--v2updates)
   - [Self-update dell'Updater](#54-self-update-dellupdater--v2updates)
   - [Remote config](#55-remote-config--v2config)
   - [Ban permanenti](#56-ban-permanenti--v2bans)
   - [Statistiche](#57-statistiche--v2stats)
   - [Stream WebSocket](#58-stream-websocket--v2statsstream)
   - [Presenza client](#59-presenza-client--v2clientws)
   - [Coda download](#510-coda-download--v2download-queue)
   - [Prodotti](#511-prodotti--v2products)
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
| `ADMIN+DASH` | `X-Admin-Key` **e** `X-Dashboard-Key` validi insieme (`AdminKeyAuth` + `DashboardKeyAuth`, 401 se ne manca uno) |
| `ADMIN (WS)` | `X-Admin-Key` controllato dall'handler prima dell'upgrade WebSocket, con fallback `?admin_key=` |

Tutti i gruppi applicano anche `apimw.RouteLimitByIP(30, time.Minute)`, oltre
al rate limiter custom descritto sotto. Entrambi esentano chi presenta un
`X-Dashboard-Key` valido. Tutte le risposte sono JSON tramite
`response.OK` / `response.Created` / `response.Error` (`internal/response`),
tranne i download binari.

**Scope prodotti.** Le route marcate *scoped* nelle sezioni sotto
(gestione release, `/v2/products`, `/v2/stats/*`) guardano anche
`X-Session-Token`. Senza token (chiamata con la sola admin key: script,
integrazioni) non c'è restrizione. Con un token, la richiesta vede **solo i
prodotti assegnati a quell'utente** (tabella `user_products`, gestita da
`/v2/api/admin/users/{id}/products`), **admin compresi**. L'unica eccezione è il
ruolo **`owner`**, il più alto: vede sempre tutti i prodotti, senza
assegnazioni. Un token sconosciuto, scaduto o di un utente disabilitato vede
**nessun** prodotto (mai "tutto"). Un prodotto fuori scope risponde `403`
sulla gestione release e sulle stats, `404` su `/v2/products/{slug}` e sulle
macchine (così non si scopre cosa esiste). Le route pubbliche usate dai client
in campo (manifest, download) non sono toccate.

Il corpo di errore è sempre nella forma:

```json
{ "error": "descrizione del problema" }
```

---

## 2. Middleware globali

Catena applicata in `main.go` a ogni richiesta, nell'ordine:

```
RequestID → RealIP → AccessLog → Recoverer → Timeout(30s)* → Timing
          → [otelhttp, se OTEL_ENABLED] → BanList → RateLimiter
```

\* Tranne i due download di installer (`updates.IsInstallerDownload`), la cui
durata massima è `download_timeout_seconds` della
[coda download](#510-coda-download--v2download-queue) (default 10 minuti,
modificabile dalla dashboard): 30 s tagliavano ogni installer su una linea
sotto i ~330 KB/s.

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
| `GET`    | `/users/{id}/products`        | `ADMIN`   |
| `PUT`    | `/users/{id}/products`        | `ADMIN`   |

**`GET /auth/validate`** — in più rispetto alla v1, `user.products` elenca i
prodotti assegnati all'utente (per un `owner`, tutti i prodotti del registro): è lo stesso elenco con cui l'API limita le
sue richieste, quindi la dashboard ci costruisce il selettore prodotto.
(Il campo compare anche su `/v1/.../validate`, che usa lo stesso handler: è
additivo.)

**`GET /users/{id}/products`** → `{"user_id": "...", "products": ["emly"]}`.
`404` se l'utente non esiste.

**`PUT /users/{id}/products` — corpo JSON**

```json
{ "products": ["emly", "foo"] }
```

Sostituisce l'intera assegnazione (`[]` toglie tutto). `400` se `products`
manca o contiene uno slug che non esiste in `products`, `404` se l'utente non
esiste. Con un `X-Session-Token`, solo un utente con ruolo `admin` o `owner`
può chiamarla (`403` per `user`): uno scope che l'utente potesse allargarsi da solo
non servirebbe a niente. Un utente appena creato non ha prodotti e non vede
niente finché non gli se ne assegnano; chi crea un prodotto da
`POST /v2/products` lo riceve automaticamente.

### 5.3 Updates per prodotto — `/v2/updates`

L'API distribuisce più prodotti (registro in [`/v2/products`](#511-prodotti--v2products)).
Ogni prodotto ha le sue release, i suoi slot stable/beta/critical e il suo
manifest, sotto `/v2/updates/{product}/...`. Le route **senza** prodotto sono
quelle storiche e valgono sempre per `emly`: ogni client EMLy in campo le usa,
quindi restano identiche. Sono gli stessi handler con il prodotto fissato.

| Metodo   | Path                                            | Auth    | Cosa fa |
|----------|-------------------------------------------------|---------|---------|
| `GET`    | `/{product}/manifest`                           | `—`     | Manifest del prodotto. Registra un evento `manifest_check` con `product` = slug. `404` se il prodotto non esiste o è disabilitato. |
| `GET`    | `/{product}/releases/{version}/download`        | `—`     | Scarica l'installer. Soggetto alla [coda download](#510-coda-download--v2download-queue): `429` se piena. `404` come sopra. |
| `GET`    | `/{product}/releases`                           | `ADMIN`, scoped | Elenca le release del prodotto. |
| `POST`   | `/{product}/releases`                           | `ADMIN`, scoped | Crea una release caricando l'installer. |
| `PUT`    | `/{product}/releases/{version}`                 | `ADMIN`, scoped | Sostituisce tutti i metadati della release. |
| `PATCH`  | `/{product}/releases/{version}`                 | `ADMIN`, scoped | Aggiorna solo i campi presenti nel corpo. |
| `DELETE` | `/{product}/releases/{version}`                 | `ADMIN`, scoped | Elimina la release e il file su S3. |
| `PATCH`  | `/{product}/releases/{version}/channel`         | `ADMIN`, scoped | Cambia solo i flag `is_stable` / `is_beta`. |
| `GET`    | `/manifest`                                     | `—`     | Alias storico di `/emly/manifest`. |
| `GET`    | `/releases/{version}/download`                  | `—`     | Alias storico di `/emly/releases/{version}/download`. |
| `GET` `POST` `PUT` `PATCH` `DELETE` | `/releases`, `/releases/{version}`, `/releases/{version}/channel` | `ADMIN`, scoped | Alias storici della gestione release di `emly`. |

Sulle route admin un prodotto sconosciuto è `404` (dopo il controllo della
admin key, così chi non ce l'ha riceve `401` per qualunque slug); un prodotto
**disabilitato** resta gestibile, così le release si preparano prima di
renderlo pubblico. Un prodotto non assegnato all'utente della sessione è
`403` (vedi *Scope prodotti* in §1). Gli alias storici servono `emly` anche
se qualcuno lo disabilita: lo facevano incondizionatamente prima dei prodotti.

Gli slug `manifest`, `releases`, `download`, `updater`, `all`, `products` sono
riservati: i primi tre (e `updater`) sono segmenti statici sotto `/v2/updates`,
che chi preferisce a `{product}`, quindi un prodotto con quel nome sarebbe
irraggiungibile.

**File su S3.** `emly` resta dove è sempre stato, sotto `S3_UPDATES_PREFIX`.
Ogni altro prodotto usa `S3_UPDATES_PREFIX/<slug>/`, a meno che il prodotto non
abbia un `s3_prefix` esplicito.

**`GET /manifest` — forma della risposta**

Aggrega le righe di `update_releases` **del prodotto** in un unico documento:

| Campo                    | Contenuto |
|--------------------------|-----------|
| `stable_version` / `stable_download` | versione e URL della release con `is_stable` |
| `beta_version` / `beta_download`     | versione e URL della release con `is_beta` |
| `min_required_version`   | preso dalla release stabile |
| `is_critical` / `critical_version` | attivi se una qualsiasi release ha `is_critical` |
| `sha256_checksums`       | mappa versione → checksum |
| `release_notes`          | mappa versione → nota breve |
| `detailed_release_notes` | mappa versione → `{severity_type, description:{en,it}}`, solo per severità diversa da `none` |

Il link di download di `emly` resta `/v2/updates/releases/{version}/download`
(quello che i client in campo conoscono); per gli altri prodotti è
`/v2/updates/{product}/releases/{version}/download`.
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
release **dello stesso prodotto** che lo deteneva prima, nella stessa
transazione; gli altri prodotti non vengono toccati. Stessa cosa per
`is_critical`. La versione è unica per prodotto (due prodotti possono avere
entrambi una `1.0.0`). Ogni release restituita porta il campo `product`.
`503` se il bucket updates non è configurato.

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
il file non esistono, `503` se S3 non è configurato, `429` con `Retry-After`
se la [coda download](#510-coda-download--v2download-queue) è piena. Un
download rimosso dal suo slot da un admin viene registrato con causa
`evicted from queue slot`.

### 5.4 Self-update dell'Updater — `/v2/updates`

Superficie separata, con la sua tabella `updater_releases` e il suo prefisso S3
(`S3_UPDATER_PREFIX`, default `updater`) nello stesso bucket updates.

| Metodo   | Path                                 | Auth    | Cosa fa |
|----------|--------------------------------------|---------|---------|
| `GET`    | `/manifest/updater`                  | `API`   | Manifest di self-update dell'Updater. |
| `GET`    | `/download/updater/{version}`        | `—`     | Scarica l'installer dell'Updater. Soggetto alla [coda download](#510-coda-download--v2download-queue): `429` se piena. |
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
Condivide con il download EMLy la stessa [coda download](#510-coda-download--v2download-queue):
gli slot sono uno solo pool per entrambi i prodotti.

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
| `GET`  | `/summary`       | `ADMIN`, scoped | Aggregati della flotta, memoizzati. |
| `GET`  | `/clients`       | `ADMIN`, scoped | Elenco paginato dei client noti. |
| `GET`  | `/clients/{id}`  | `ADMIN`, scoped | Un client con i suoi eventi recenti e i prodotti installati. `404` se l'ID non esiste. |
| `DELETE` | `/clients/{id}` | `ADMIN`, scoped | Cancella un client e tutti i suoi eventi. `404` se l'ID non esiste. |
| `GET`  | `/events`        | `ADMIN`, scoped | Serie temporale degli eventi, a bucket. |

**Scope.** Con un `X-Session-Token` (vedi §1): `product` deve essere un
prodotto assegnato (`403` altrimenti); `updater` (il self-update dell'Agent,
che non appartiene a nessun prodotto) e `all` sono sempre ammessi, e `all`
significa "tutti i **miei** prodotti più `updater`". Le macchine visibili —
in `/clients`, `/clients/{id}`, `DELETE`, e negli aggregati client di
`/summary` (`total_clients`, `connected_clients`, `clients_by_version`,
`clients_by_config_revision`) — sono solo quelle che hanno installato almeno
un prodotto assegnato (`updater_client_products`); le altre sono `404`. Una
macchina che non ha mai riportato un prodotto installato non è quindi visibile
a nessun utente con sessione tranne gli `owner`, che non hanno restrizioni,
come la admin key senza sessione.
**`GET /summary` — query string**

| Parametro        | Valori |
|------------------|--------|
| `window_minutes` | finestra per il conteggio dei client connessi |
| `product`        | uno slug del registro prodotti (anche disabilitato), `updater`, `all`; default `emly` |

Risposta: `total_clients`, `connected_clients`, `window_minutes`, `product`,
`events_last_24h`, `clients_by_version`, `clients_by_config_revision`.

`events_last_24h` è sommato dalla tabella di rollup `updater_event_hourly`
(migration 20), non dalle righe grezze di `updater_events`. Di conseguenza la
finestra è allineata all'ora: copre le 24 ore intere precedenti più l'ora
corrente parziale, quindi il totale può comprendere fino a un'ora in più di un
esatto rolling 24h. È un indicatore di volume per la dashboard, e arrotondare
per eccesso è la direzione innocua.

Il payload è memoizzato dietro un `ttlcache.Cache` (`internal/ttlcache`), con chiave
`product|window_minutes|scope` e durata `STATS_CACHE_TTL` (default 30s, `0` disabilita),
e la risposta porta un `Cache-Control: private, max-age=<TTL>`.
Le dashboard fanno polling continuo su aggregati a 24 ore, quindi le query girano
una volta per TTL invece che una volta per richiesta, e più chiamate concorrenti
su una chiave fredda collassano in una sola build. Quello che si aggiunge al
sommario va in `fetchStatsSummary`, dietro la cache, non nell'handler.

**`GET /clients/{id}`** — risposta `{client, events, products}`, dove
`products` è `[{product, version, updated_at}]` da `updater_client_products`
(`updated_at` = da quando la macchina è a quella versione).

**`GET /clients` — query string**

| Parametro        | Default | Note |
|------------------|---------|------|
| `page`           | `1`     | |
| `page_size`      |         | |
| `online`         | `false` | `true` filtra i soli client visti nella finestra |
| `window_minutes` |         | definisce "online" |

Ogni client dell'elenco (e dello snapshot/delta del canale WS `stats:clients`)
porta `products`: l'inventario dei prodotti installati, nella stessa forma del
dettaglio (`[{product, version, updated_at}]`, ordinato per prodotto) — tutti,
non solo quelli assegnati. `[]` se non ne ha nessuno.

**`DELETE /clients/{id}`**

Cancella prima tutte le righe di `updater_events` del client e poi la riga di
`updater_clients`, nella stessa transazione: se una delle due fallisce non
cambia niente, e non resta mai un client con metà storico. La riga del client
è letta con `SELECT ... FOR UPDATE`, così un evento che arriva in quel momento
per la stessa macchina aspetta la fine della cancellazione invece di infilarsi
tra le due `DELETE`.

Risposta `200`: `{"status": "deleted", "client_id": 42, "events_deleted": 118}`.
Errori: `400` ID non numerico, `404` client inesistente, `500` errore del database.

Il rollup `updater_event_hourly` **non** viene toccato: i grafici della flotta
continuano a contare il traffico che quella macchina ha prodotto finché
esisteva, esattamente come fa il pruning periodico (`internal/eventprune`).

Cancellare un client non lo tiene fuori: alla prossima richiesta con i suoi
header `X-EMLy-*` la riga viene ricreata da zero. Serve a togliere dalla
dashboard una macchina dismessa o di test; per bloccarla davvero va bannato
il suo HWID (`/v2/bans`).

**`GET /events` — query string**

| Parametro    | Valori |
|--------------|--------|
| `bucket`     | `day` (default) o `hour`; `400` altrimenti |
| `event_type` | filtro sul tipo di evento |
| `product`    | uno slug del registro prodotti (anche disabilitato), `updater`, `all`; default `emly` |
| `from`       | RFC3339 |
| `to`         | RFC3339 |

Anche questa serie viene dal rollup `updater_event_hourly`: l'ora è il bucket
più fine che l'API espone, quindi **il filtro `from`/`to` ha granularità
oraria** — `from` è arrotondato all'inizio della sua ora, così un intervallo
che inizia a metà ora comprende l'ora intera e la prima colonna di un grafico
giornaliero non risulta tagliata. I campi `from`/`to` nella risposta riportano
comunque la finestra come l'ha chiesta il chiamante.

Come `/summary`, la risposta è memoizzata per `STATS_CACHE_TTL` e porta il
relativo `Cache-Control: private`. La chiave di cache include tutti i
parametri, con gli istanti arrotondati a scatti larghi quanto il TTL: senza
quell'arrotondamento la finestra di default finisce a `time.Now()` e due
richieste non condividerebbero mai una chiave. Due chiamanti le cui finestre
differiscono per meno del TTL condividono quindi un payload.

**Telemetria dei client.** Gli header `X-EMLy-*` (`Hostname`, `HWID`, `ADDomain`,
`LoggedUser`, `LoggedUserState`, `LoggedUserDisconnectedAt`, `Serial`, `Product`,
`OSVersion`, `AppVersion`, `InstalledProducts`) sono letti in un punto solo per la via HTTP,
`clientIdentityFromRequest`, insieme alla versione e al contatto estratti dallo
User-Agent e all'IP del peer. Il messaggio `identity` di `GET /v2/client/ws`
(§5.9) è la seconda via: stessi campi, letti da JSON invece che da header, da
`clientIdentityFromWSPayload`. Aggiungere un campo significa aggiungerlo a
**entrambi** i costruttori, non solo a uno dei due call site. Una richiesta
senza né HWID né hostname viene servita ma
non tracciata. Un header che il client non invia non azzera mai il valore già
memorizzato: l'Updater omette gli header per cui non ha un valore, quindi
"assente" vuol dire "sconosciuto". Per questo `logged_user` è un'istantanea da
leggere insieme a `last_seen_at`, non uno storico. Due eccezioni:
`logged_user_disconnected_at` viene riscritto (anche a `NULL`) ogni volta che
arriva uno stato, così non sopravvive a una sessione che si è ricollegata; e un
Updater 1.6.2 o successivo che non manda `X-EMLy-LoggedUser` sta dicendo che non
c'è nessuno loggato, quindi utente, stato e orario di disconnessione vengono
azzerati.

**Prodotti installati.** `X-EMLy-InstalledProducts: emly=3.5.0,foo=1.2.0`
(e il campo `installed_products` dell'`identity` WS) è l'inventario
**completo** della macchina: i prodotti elencati vengono scritti in
`updater_client_products`, quelli non elencati cancellati (disinstallati).
Header presente ma vuoto = nessun prodotto installato; header **assente** = non
riportato, non cambia nulla. Voci con slug non valido o versione vuota sono
scartate, al massimo 32. `X-EMLy-AppVersion` continua a riempire
`emly_version` e, se l'inventario non cita `emly`, anche la riga `emly`: così
gli Agent non ancora aggiornati restano visibili. `X-EMLy-Product` **non** è il
prodotto software: è lo SKU del firmware.

### 5.8 Stream WebSocket — `/v2/stats/stream`

| Metodo | Path                | Auth         | Cosa fa |
|--------|---------------------|--------------|---------|
| `GET`  | `/v2/stats/stream`  | `ADMIN (WS)` | Upgrade WebSocket che spinge aggiornamenti live alle dashboard. |

L'autenticazione avviene **prima** dell'upgrade: `X-Admin-Key`, oppure
`?admin_key=` come ripiego per i proxy che rimuovono gli header custom sulla
richiesta di Upgrade. Una chiave errata riceve `401` senza che l'upgrade venga
nemmeno tentato, così il client distingue subito "chiave sbagliata" da "problema
di rete".

Lo [scope prodotti](#1-come-leggere-questa-tabella) vale anche qui: il token si
passa in `X-Session-Token` o, con lo stesso ripiego, `?session_token=`, ed è
risolto una volta all'apertura. Tutti i canali sono filtrati come le route REST
corrispondenti; un `subscribe` con un `product` non assegnato riceve un
`error` per quel parametro. Il prodotto di default è `emly`, oppure `all` (cioè
"i miei") se l'utente non ha `emly`.

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

**Gli aggiornamenti sono raggruppati, non uno per evento** (`wsCoalesceWindow`,
1s). Un evento dell'hub non fa query: segna soltanto quali canali sono da
ricalcolare, e un ticker da un secondo ricalcola e spinge quelli sporchi. Una
raffica di eventi produce quindi **un** `update` per canale, non uno per evento:
la latenza percepita resta sotto il secondo, mentre il carico sul database
diventa costante invece di crescere come (eventi al secondo) × (connessioni
aperte). Sul canale `stats:clients` il raggruppamento vale anche per i delta: una
macchina che fa più check dentro la stessa finestra compare una volta sola, nel
suo stato più recente, e un tick che chiede una risincronizzazione completa
prevale sui delta accumulati. Un client non deve quindi contare gli `update`
ricevuti né assumere che a ogni evento ingerito corrisponda un messaggio — è
sempre lo stato corrente, non un flusso di eventi.

### 5.9 Presenza client — `/v2/client/ws`

| Metodo | Path                                 | Auth      | Cosa fa |
|--------|--------------------------------------|-----------|---------|
| `GET`  | `/v2/client/ws`                      | `API`   | Upgrade WebSocket che l'EMLy Updater tiene aperta per tutta la vita del servizio, per la presenza online/offline in tempo reale e, dal protocollo v2, per comandi/eventi/notify. |
| `POST` | `/v2/client/{client_id}/commands`    | `ADMIN` | Invia un comando (§7 di `CLIENT_WS_PROTOCOL.md`) alla macchina connessa con quell'`updater_clients.id`. |
| `GET`  | `/v2/client/commands/{command_id}`   | `ADMIN` | Legge lo stato/esito di un comando già inviato. |
| `GET`  | `/v2/client/{client_id}/events`      | `ADMIN` | Ultimi eventi (`event`, §8) ricevuti da quella macchina — un anello in memoria, non uno storico permanente. |
| `POST` | `/v2/client/notify`                  | `ADMIN` | Manda un `notify` (§9) a una o più macchine connesse, o a tutte quelle che dichiarano quel `topic`. |

L'autenticazione (`X-Api-Key`) avviene tramite lo stesso middleware
`apimw.APIKeyAuth` del manifest self-update dell'Updater, come middleware di
route ordinario **prima** dell'upgrade — non è un controllo fatto a mano
dentro l'handler (a differenza di `/v2/stats/stream`, che è per questo
`ADMIN (WS)` nella tabella e non semplicemente `ADMIN`), quindi la sigla qui è
`API` e basta. A differenza di `/v2/stats/stream` non esiste un fallback in
query string.

**Handshake**

```
server ──► { "type": "hello" }
client ──► { "type": "identity", "data": { hwid, hostname, ad_domain,
             logged_user, logged_user_state, logged_user_disconnected_at,
             serial, product, os_version, emly_version,
             installed_products } }
```

L'identità arriva nel payload invece che negli header `X-EMLy-*`, ma passa
per lo stesso upsert di manifest/download: la riga in `updater_clients` è
la stessa. Due casi diversi entro i 10s:

- il client manda un primo messaggio che non è `identity`, o un `identity`
  senza `hwid`/`hostname`: riceve un `error` e poi la connessione viene
  chiusa dal server;
- il client non manda **nulla** entro i 10s: `coder/websocket` considera
  qualunque errore (compresa la scadenza del context di lettura) motivo
  per chiudere la connessione da solo, quindi non c'è tempo per scrivere un
  `error` — il client vede semplicemente la connessione cadere, senza busta.

**Heartbeat**: il server manda `{"type":"ping"}` ogni 10s; il client deve
rispondere `{"type":"pong"}` entro 20s o la connessione è considerata morta.
Un `type` sconosciuto da uno dei due lati viene ignorato, non chiude la
connessione — è così che un futuro comando si aggiunge senza rompere un
Updater già distribuito.

Il formato completo dei messaggi — incluso il protocollo v2 (comandi,
eventi, notify, implementato lato API in `internal/clientproto` +
`internal/clienthub`) — è in
[`CLIENT_WS_PROTOCOL.md`](CLIENT_WS_PROTOCOL.md).

**Presenza**: tracciata solo in memoria (`internal/presencehub`), con una
finestra di grazia di 15s alla disconnessione prima di segnare il client
offline — assorbe un calo di rete breve o una riconnessione. Una nuova
connessione con lo stesso client sostituisce quella precedente, che viene
chiusa dal server.

Il campo `"online"` che questo canale alimenta compare in
`GET /v2/stats/clients` e nel canale `stats:clients` di
`GET /v2/stats/stream` (§5.7-5.8): è calcolato al momento della risposta dal
presence hub, **non** dallo stesso filtro di `last_seen_at` che
`?online=true` su `/v2/stats/clients` usa per decidere quali righe
restituire — i due possono disaccordare per un client appena disconnesso
ma ancora dentro la finestra di `last_seen_at`.

Attivata lato client dal flag `clientWs.enabled` nel documento di
configurazione remota (§5.5): l'API non rifiuta comunque una connessione se
quel flag è `false`, è l'Updater a non aprirla.

**Rate limit senza eccezione per IP privati/loopback.** `/v2/client/ws` porta
lo stesso `apimw.RouteLimitByIP(30, time.Minute)` di ogni altro gruppo v2, ma
a differenza del `RateLimiter` globale non esiste un'eccezione per IP
privati/loopback qui — solo `X-Dashboard-Key` esenta, e questa rotta non lo
usa. Una raffica di riconnessioni da molte macchine dietro un solo IP
pubblico (un unico gateway NAT, nessun mirror di sito) dopo un riavvio
dell'API può quindi essere limitata a ~30 riconnessioni al minuto. È un limite
noto e accettato finché non si conferma la reale topologia di rete della
flotta — non è stato toccato in questo giro di fix.

**Route admin (comandi, eventi, notify)** — `internal/clientws/admin.route.go`,
montate da `RegisterV2` dietro `AdminKeyAuth` (a differenza di `GET /ws`, qui
la sigla è proprio `ADMIN`, controllo ordinario di route). Sono il modo per
la dashboard di pilotare una macchina connessa senza aprire essa stessa un
client WS.

**`POST /v2/client/{client_id}/commands` — corpo JSON**

```json
{ "name": "apps.list_upgradable", "args": {}, "ttl_seconds": 600, "issued_by": "admin:f.fois" }
```

`args` segue le regole del comando (`CLIENT_WS_PROTOCOL.md` §7); `ttl_seconds`
default `600`, deve stare in `[1, 86400]` (il controllo avviene sul numero di
secondi grezzo, prima di convertirlo in `time.Duration`, altrimenti un valore
enorme può mandare in overflow la conversione e superare il controllo per
puro caso); `issued_by` è opzionale ma se presente deve stare entro 64
caratteri ed essere fatto solo di rune stampabili (nessun carattere di
controllo). Risposta `202` con lo stesso `CommandRecord` che
`GET .../commands/{command_id}` restituisce. `status` è `sent` solo nel caso
comune: il comando è registrato nel hub **prima** dell'invio sul socket
(`CLIENT_WS_PROTOCOL.md` §13), ma nulla impedisce a un `ack` (o, più
raramente, anche a un `result`) di arrivare più veloce della risposta HTTP
stessa — la `202` può quindi già mostrare `acked` o oltre, non è garantito
`sent`. Codici di errore: `400` `client_id`/corpo JSON/`args`/`ttl_seconds`/
`issued_by` non validi; `409` la macchina non ha una connessione v2/v1
aperta in questo momento (nessuna sessione per quel `client_id`); `422` il
`name` non esiste nel catalogo, o esiste ma questa connessione non l'ha
dichiarato fra le sue `capabilities` (updater v1 compreso: sempre `422`, non
`409`, per distinguere "non lo sa fare" da "non è raggiungibile").

**`GET /v2/client/commands/{command_id}`**: `200` col `CommandRecord`, `404`
se l'id non esiste (mai esistito o già rimosso dal prune di 24h, vedi sotto).

**`GET /v2/client/{client_id}/events`**: `200` con `{"events": [...]}`, lista
vuota (mai `404`) se quel client non ha eventi registrati o non esiste. Il
`payload` di un `EventRecord` non è garantito: è conservato solo se il
`name` dell'evento è fra le `capabilities` accettate per quella sessione
(altrimenti l'evento resta comunque in lista, ma senza `payload`) e solo
fino a 8 KiB, oltre i quali viene scartato e `truncated: true` compare
invece (`CLIENT_WS_PROTOCOL.md` §13).

**`POST /v2/client/notify` — corpo JSON**

```json
{ "topic": "release.published",
  "payload": { "target": "emly", "channel": "stable", "version": "3.5.0", "jitter_seconds": 600 },
  "client_ids": [42, 57] }
```

`topic`/`payload` validati con le stesse regole di `CLIENT_WS_PROTOCOL.md`
§9; `client_ids` assente = tutte le sessioni v2 che dichiarano quel `topic`.
Risposta `200` con `{"sent": N}`, il numero di destinatari effettivamente
raggiunti (non il numero di client_ids passati); `400` se `topic`/`payload`
non superano la validazione.

**Stato in memoria, non un audit log.** Sessioni, comandi ed eventi vivono
solo in `internal/clienthub`: nessuna tabella, nessuna persistenza. Un
riavvio dell'API perde tutto (una macchina riconnessa ricostruisce solo la
sua sessione, non lo storico), ed è per-istanza come `presencehub`/
`statshub` — un comando lanciato su una macchina connessa a una replica non
è visibile né eseguibile dall'altra. Un ticker in `main.go` pota ogni 10
minuti: i comandi conclusi (`done`/`failed`/`rejected`/`timeout`) e gli
eventi più vecchi di 24h vengono scartati.

### 5.10 Coda download — `/v2/download-queue`

Limita quanti installer (EMLy **e** Updater, un unico pool) possono essere in
streaming nello stesso momento. Non è una fila d'attesa: un download o prende
subito uno slot libero o viene rifiutato con `429`, e il client riprova più
tardi. Tenere la richiesta aperta ad aspettare occuperebbe proprio la
connessione che il limite vuole proteggere.

Tutto lo stato è **in memoria** (`internal/downloadqueue`): nessuna tabella,
nulla salvato su DB. Le modifiche fatte da queste route valgono fino al
prossimo riavvio, poi si torna ai valori di `.env` (`DOWNLOAD_QUEUE_ENABLED`,
default `true`; `DOWNLOAD_QUEUE_SLOTS`, default `50`;
`DOWNLOAD_QUEUE_RETRY_AFTER`, default `60s`; `DOWNLOAD_QUEUE_TIMEOUT`,
default `10m`). È per-istanza come
`presencehub`: ogni replica conta i propri download.

**Risposta quando la coda è piena** (su `GET /v2/updates/releases/{version}/download`
e `GET /v2/updates/download/updater/{version}`):

```
HTTP/1.1 429 Too Many Requests
Retry-After: 60
Content-Type: application/json
```

```json
{
  "error": "download queue full",
  "message": "No free download slots are available right now. Retry in 60 seconds.",
  "retry_after": 60,
  "capacity": 50,
  "active": 50
}
```

Il limite scatta solo dopo il rate limiter del gruppo: una richiesta già
rifiutata da `RouteLimitByIP` non occupa mai uno slot. Lo slot resta occupato
per tutta la durata dello streaming e si libera quando l'handler ritorna
(completato, interrotto dal client, scaduto il timeout del download o
rimosso da un admin).

**Timeout del download.** Le due route di download sono escluse dal
`Timeout(30s)` globale: la loro durata massima è `download_timeout_seconds`
(`DOWNLOAD_QUEUE_TIMEOUT`, default 10 minuti), modificabile dalla dashboard
come la capacità. Vale anche a coda disattivata. Ogni download fissa la sua
scadenza quando parte (`deadline_at` nello slot): cambiare il valore vale per
i download nuovi, non per quelli in corso. Allo scadere il trasferimento viene
tagliato e contato in `failed_by_reason` come `server timeout`.
A coda **disattivata** i download sono comunque tracciati (compaiono in
`slots`) ma nessuno viene rifiutato.

| Metodo   | Path            | Auth         | Cosa fa |
|----------|-----------------|--------------|---------|
| `GET`    | `/`             | `ADMIN+DASH` | Stato: impostazioni correnti e di default, slot occupati/liberi, contatori, elenco dei download in corso con avanzamento e velocità media per client. |
| `PATCH`  | `/`             | `ADMIN+DASH` | Attiva/disattiva, espande/riduce la capacità, cambia il `Retry-After` e il timeout del download. Solo i campi presenti. |
| `POST`   | `/reset`        | `ADMIN+DASH` | Torna ai valori di `.env`. I download in corso restano. |
| `DELETE` | `/slots`        | `ADMIN+DASH` | Svuota la coda: interrompe **tutti** i download in corso. `{"evicted": N}`. |
| `DELETE` | `/slots/{id}`   | `ADMIN+DASH` | Interrompe un singolo download e libera il suo slot. `404` se lo slot non esiste (già finito). |

Con `DASHBOARD_KEY` non impostata le route rispondono sempre `401`: sono
pensate solo per la dashboard. Se il router è costruito senza coda (test)
rispondono `503`.

**`GET /` — forma della risposta** (identica per `PATCH` e `POST /reset`)

```json
{
  "enabled": true,
  "capacity": 50,
  "retry_after_seconds": 60,
  "download_timeout_seconds": 600,
  "active": 2,
  "available": 48,
  "completed_total": 1180,
  "failed_total": 54,
  "failed_by_reason": {
    "server timeout": 41,
    "client disconnected": 12,
    "error response": 1
  },
  "rejected_total": 17,
  "evicted_total": 0,
  "total_bytes_per_sec": 2536877,
  "defaults": { "enabled": true, "capacity": 50, "retry_after_seconds": 60, "download_timeout_seconds": 600 },
  "slots": [
    {
      "id": 1233,
      "product": "emly",
      "version": "1.7.0",
      "ip": "203.0.113.7",
      "hostname": "pc-reception",
      "hwid": "ABC123",
      "started_at": "2026-09-30T08:12:44Z",
      "deadline_at": "2026-09-30T08:22:44Z",
      "elapsed_seconds": 12.4,
      "bytes_sent": 31457280,
      "bytes_total": 94371840,
      "percent": 33.3,
      "avg_bytes_per_sec": 2536877
    }
  ]
}
```

`available` è `capacity - active`, mai sotto zero. I contatori `*_total`
ripartono da zero a ogni riavvio. `slots` è ordinato dal più vecchio.

Ogni download che ha preso uno slot finisce in **uno solo** di questi
contatori (finché è in corso è contato in `active`):

| Contatore | Quando |
|-----------|--------|
| `completed_total` | L'installer è stato inviato **per intero** (byte inviati = `Content-Length`). |
| `failed_total` | Il download non è andato a buon fine. `failed_by_reason` dice perché. |
| `evicted_total` | Interrotto da un admin da `DELETE /slots...`. Non conta anche come fallito. |
| `rejected_total` | Rifiutato con `429` perché la coda era piena: non ha mai avuto uno slot. |

Chiavi di `failed_by_reason` (sempre un oggetto, vuoto se nessun errore; una
chiave compare solo dal primo caso):

| Chiave | Significato |
|--------|-------------|
| `server timeout` | Il timeout del download (`download_timeout_seconds`) ha tagliato il trasferimento a metà: il client ha ricevuto `200` con un file troncato. Tanti casi = timeout troppo corto per le linee più lente della flotta. |
| `client disconnected` | Il client ha chiuso la connessione prima della fine. |
| `copy failed` | Errore di scrittura/lettura con la richiesta ancora viva. |
| `error response` | Risposta `4xx`/`5xx` prima di iniziare lo streaming (release inesistente, file assente su S3, S3 non raggiungibile). |
| `internal error` | Panic dell'handler durante il download. |

Le chiavi `server timeout` / `client disconnected` / `copy failed` sono le
stesse del campo `reason` nel log **warn** `installer download did not
complete`, calcolate dalla stessa funzione (`downloadqueue.FailureReason`):
una riga di log e il contatore corrispondente non possono divergere.

Per ogni slot, calcolati al momento della richiesta:

| Campo               | Contenuto |
|---------------------|-----------|
| `deadline_at`       | Quando il timeout del download taglierà il trasferimento. |
| `elapsed_seconds`   | Secondi da `started_at` (un decimale). |
| `bytes_sent`        | Byte già scritti verso il client. |
| `bytes_total`       | `Content-Length` dell'installer; assente finché l'handler non l'ha impostato (lookup della release/apertura S3 in corso). |
| `percent`           | `bytes_sent / bytes_total`, un decimale; assente senza `bytes_total`. |
| `avg_bytes_per_sec` | **Velocità media** dall'inizio del download: `bytes_sent / elapsed_seconds`. Non è istantanea, e include il tempo di lookup prima del primo byte. |

`total_bytes_per_sec` è la somma delle velocità medie di tutti gli slot: più o
meno la banda che i download di installer stanno usando in questo momento.

**`PATCH /` — corpo JSON**

```json
{ "enabled": true, "capacity": 80, "retry_after_seconds": 120, "download_timeout_seconds": 900 }
```

| Campo                      | Vincoli |
|----------------------------|---------|
| `enabled`                  | booleano |
| `capacity`                 | intero `1..10000` |
| `retry_after_seconds`      | intero `1..86400` |
| `download_timeout_seconds` | intero `1..86400`; vale per i download che partono dopo |

Almeno un campo è richiesto, altrimenti `400`; un valore fuori range è `400`.
Ridurre la capacità sotto il numero di download in corso non interrompe
nessuno: quelli finiscono, e i nuovi vengono rifiutati finché `active` non
scende sotto la nuova capacità.

**`DELETE /slots/{id}`** annulla il contesto della richiesta di download: la
lettura da S3 si interrompe e il client riceve un file troncato (lo stato
`200` era già partito). Il log `installer download did not complete` riporta
`reason: evicted from queue slot`.

---

### 5.11 Prodotti — `/v2/products`

Il registro dei prodotti distribuiti da [`/v2/updates/{product}`](#53-updates-per-prodotto--v2updates)
(tabella `products`, migration 22, `emly` già presente).

| Metodo   | Path       | Auth            | Cosa fa |
|----------|------------|-----------------|---------|
| `GET`    | `/`        | `ADMIN`, scoped | Elenca i prodotti (con sessione: solo quelli assegnati). |
| `POST`   | `/`        | `ADMIN`, scoped | Crea un prodotto; con sessione lo assegna anche a chi lo crea. |
| `GET`    | `/{slug}`  | `ADMIN`, scoped | Un prodotto. `404` se non esiste o non è assegnato. |
| `PATCH`  | `/{slug}`  | `ADMIN`, scoped | Aggiorna `name`, `s3_prefix`, `enabled`. |
| `DELETE` | `/{slug}`  | `ADMIN`, scoped | Elimina il prodotto. |

**`POST /` — corpo JSON**

```json
{ "slug": "foo", "name": "Foo", "s3_prefix": null, "enabled": true }
```

| Campo       | Note |
|-------------|------|
| `slug`      | obbligatorio, `^[a-z0-9][a-z0-9-]{0,19}$`, non riservato (§5.3); **non modificabile** dopo: è scritto in ogni release, evento e riga di inventario |
| `name`      | obbligatorio, max 100 caratteri |
| `s3_prefix` | opzionale; vuoto/`null` = derivato (`S3_UPDATES_PREFIX/<slug>`, per `emly` `S3_UPDATES_PREFIX`). Niente segmenti vuoti, `.` o `..` |
| `enabled`   | default `true`; `false` = `404` su manifest e download pubblici, gestione release ancora possibile |

Esiti: `201` col prodotto, `400` validazione, `409` slug già esistente.

**`PATCH /{slug}`** — `name`, `s3_prefix` (stringa vuota = torna al default),
`enabled`, tutti opzionali; `400` se non c'è nessun campo. Cambiare
`s3_prefix` **non sposta** i file già caricati: vanno spostati a mano o i
download delle release esistenti diventano `404`.

**`DELETE /{slug}`** — `409` se il prodotto ha ancora release (va disabilitato
o svuotato prima) e sempre `409` per `emly`. Telemetria e inventari restano:
sono storia della flotta. Le assegnazioni in `user_products` spariscono con il
prodotto (foreign key).

Il registro è tenuto in memoria (`internal/productreg`), ricaricato subito
dopo ogni scrittura e ogni minuto, così una seconda istanza dell'API vede un
prodotto creato sulla prima entro un minuto.

## 6. Riepilogo autenticazione

| Header            | Dove viene usato | Fallimento |
|-------------------|------------------|------------|
| `X-API-Key`       | Creazione bug report, manifest dell'Updater, `GET /v2/config`, `GET /v2/client/ws` | `401` |
| `X-Admin-Key`     | Tutte le route admin, releases, config writes, bans, stats, download-queue, comandi/eventi/notify di `/v2/client` | `401` |
| `X-Dashboard-Key` | Bypass di entrambi i rate limiter, globale e per gruppo di route; **richiesto** (insieme a `X-Admin-Key`) da `/v2/download-queue` | nessuno per i limiter (la richiesta prosegue limitata); `401` su `/v2/download-queue` |
| `X-Session-Token` | `auth/validate`, `auth/logout`; sulle route *scoped* (release, `/v2/products`, `/v2/stats/*`, anche `?session_token=` sullo stream) limita ai prodotti assegnati all'utente | `401` o `403`; sulle route scoped `403`/`404` per prodotti e macchine fuori scope |

`API_KEY` e `ADMIN_KEY` accettano una lista separata da virgole, ma viene usato
solo il primo valore non vuoto.

Gli header `X-EMLy-*` non autenticano nulla: alimentano la telemetria e sono
confrontati con la block list.
