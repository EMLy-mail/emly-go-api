# WebSocket server per statistiche real-time (API → Dashboard)

Design document — 2026-09-04

Spec as received from the dashboard team, implemented server-side in this
repo as `internal/statshub` (in-process event bus) + `GET /v2/stats/stream`
(`internal/handlers/stats_stream.route.go`). Implementation notes / decisions
made while implementing:

- §8 (multi-instance): resolved to option 3 (single-instance, in-process
  event bus) - this API has neither Postgres LISTEN/NOTIFY nor Redis, and the
  expected fleet size does not justify adding either. See the package doc on
  `internal/statshub`.
- `product` (added to the REST `stats/*` endpoints after this spec was
  written) is exposed as an optional `subscribe` param on `stats:summary` and
  `stats:events`, defaulting to `"emly"` exactly like the REST endpoints, so
  the two stay consistent.
- `stats:clients` carries no server-side `online`/`window_minutes` filter
  over WS: the channel always snapshots every known client (the fleet is a
  few hundred rows, and the dashboard already derives "online" from
  `last_seen_at` client-side). `window_minutes` in `subscribe.params` is
  accepted and echoed but only actually applied to `stats:summary`'s
  `connected_clients` count, not to which clients `stats:clients` sends.
- Path chosen: `/v2/stats/stream` (the spec's first-choice, §3).
- Auth: `X-Admin-Key` validated before the WS upgrade, with a
  `?admin_key=`/`?dashboard_key=` query fallback for the rare proxy that
  strips custom headers on the `Upgrade` request (§4). `X-Dashboard-Key`
  needs no extra handling here beyond that fallback - it already bypasses the
  global rate limiter (`internal/middleware/ratelimit.ban.go`), which this
  route sits behind like every other `/v2` route.

---

## 0. Contesto per chi implementa

Questo documento va implementato lato **API** (il servizio che oggi espone
`/v1/api/*` e `/v2/*` consumato dalla dashboard Next.js). La dashboard è un
consumer server-side: le chiamate REST attuali partono da un modulo
`server-only` (`lib/api.ts`) e passano `X-API-Key` / `X-Admin-Key` /
`X-Dashboard-Key` come header. Il nuovo canale WebSocket deve restare
coerente con questo modello di auth: **niente token nuovi lato browser**, la
connessione WS è server-to-server (processo Next.js ↔ API), esattamente come
le fetch REST oggi.

Endpoint REST di riferimento che il WS deve rimpiazzare/affiancare (tutti
sotto `/v2`, header `X-Admin-Key`, opzionale `X-Dashboard-Key`):

- `GET /v2/stats/summary?window_minutes=` → `StatsSummary`
- `GET /v2/stats/clients?page=&page_size=&online=&window_minutes=` → `PaginatedStatsClients`
- `GET /v2/stats/clients/{id}` → `StatsClientDetail`
- `GET /v2/stats/events?bucket=&event_type=&from=&to=` → `StatsEventsResponse`

Tipi TypeScript attuali (da mantenere identici anche nei payload WS, per non
dover toccare il parsing lato dashboard):

```ts
interface UpdaterClient {
  id: number;
  hostname: string;
  ad_domain: string;
  updater_version?: string | null;
  contact?: string | null;
  last_ip?: string | null;
  first_seen_at: string;
  last_seen_at: string;
  hwid?: string | null;
}

interface UpdaterEvent {
  id: number;
  client_id: number;
  event_type: "manifest_check" | "download";
  version?: string | null;
  ip_address?: string | null;
  created_at: string;
}

interface StatsSummary {
  total_clients: number;
  connected_clients: number;
  window_minutes: number;
  events_last_24h: { event_type: string; count: number }[];
  clients_by_version: { updater_version: string | null; count: number }[];
}

interface StatsEventsResponse {
  bucket: "day" | "hour";
  from: string;
  to: string;
  data: { bucket: string; event_type: string; count: number }[];
}
```

## 1. Obiettivo

Oggi ogni caricamento della pagina statistiche fa 3 chiamate REST
(`summary`, `clients` — con paginazione lato server ripetuta finché non
finiscono le pagine —, `events`). Vogliamo:

1. Evitare il polling/il re-fetch ripetuto per avere dati "freschi".
2. Spingere aggiornamenti alla dashboard **quando cambia qualcosa** (nuovo
   evento `manifest_check`/`download`, client che entra/esce dalla finestra
   "online") invece di ri-interrogare l'API a intervalli.
3. Mantenere gli endpoint REST esistenti come fallback/bootstrap — il WS è
   un canale di *aggiornamento*, non l'unico modo di leggere i dati.

## 2. Architettura

```
[API process]                         [Next.js dashboard process]
 stats WS server  <---- 1 conn. persistente (server-to-server) ----  singleton WS client
   |                                                                    |
   | push su eventi + tick periodico                                   | fan-out
   v                                                                    v
 (DB / event bus)                                          browser (via SSE o WS proprio,
                                                             fuori scope di questo doc)
```

- Next.js mantiene **una sola connessione** verso l'API (non una per
  richiesta/utente): è un processo server long-lived, quindi può tenere un
  client WS in un modulo singleton e ridistribuire i dati ai browser
  connessi con SSE (o un secondo WS lato dashboard — spec separata, non
  necessaria per questo task).
- L'API quindi deve gestire un numero **piccolo** di connessioni WS in
  ingresso (una per ambiente/istanza dashboard, quindi tipicamente 1, al più
  qualche unità), non una per utente finale. Questo semplifica molto rate
  limiting e scaling.

## 3. Endpoint e trasporto

- Path proposto: `wss://<API_BASE_URL>/v2/stats/stream`
  (coerente col prefisso `/v2` già usato per `stats/*`; nome alternativo
  `/v2/ws/stats` se preferite un prefisso `ws` dedicato — indifferente,
  basta confermarlo).
- Upgrade HTTP → WebSocket standard (RFC 6455). Nessun sub-protocol
  obbligatorio.
- TLS obbligatorio in produzione (`wss://`), coerente col resto dell'API.

## 4. Autenticazione

Stesso schema delle route `stats/*` REST, applicato in fase di **upgrade
HTTP** (la richiesta di handshake WS è comunque una richiesta HTTP con
header custom leggibili server-side — qui il client è Next.js, non un
browser, quindi non ci sono restrizioni CORS/header sul WS handshake):

- Header richiesti: `X-Admin-Key: <ADMIN_KEY>`
- Header opzionale: `X-Dashboard-Key: <DASHBOARD_KEY>` se presente
- Validare **prima di completare l'upgrade** (rispondere 401/403 e non
  fare upgrade se le chiavi non sono valide — evitare di accettare la
  connessione e poi chiuderla, per non dare informazioni gratuite e per
  permettere al client di distinguere subito un errore di auth da un errore
  di rete).
- Se il vostro reverse proxy/load balancer strips gli header custom sulle
  richieste di `Upgrade`, va verificato: in tal caso fallback ammesso è
  passare le chiavi come query string (`?admin_key=...&dashboard_key=...`)
  ma **solo se necessario**, dato che finiscono nei log di accesso — da
  evitare se gli header arrivano puliti.

## 5. Protocollo messaggi

Tutti i messaggi sono JSON testuali (frame `text`), con una envelope comune:

```json
{
  "type": "snapshot | update | error | ping | pong | subscribe | unsubscribe | subscribed",
  "channel": "stats:summary | stats:clients | stats:events | null",
  "ts": "RFC3339 timestamp",
  "data": { }
}
```

### 5.1 Client → Server (Next.js → API)

**`subscribe`** — inviato subito dopo la connessione, e ogni volta che
cambiano i parametri (es. l'utente cambia bucket/evento nel filtro UI):

```json
{
  "type": "subscribe",
  "channels": ["stats:summary", "stats:clients", "stats:events"],
  "params": {
    "window_minutes": 15,
    "events": { "bucket": "day", "event_type": null }
  }
}
```

- `window_minutes` si applica sia a `stats:summary` che a `stats:clients`
  (stessa semantica del parametro REST `window_minutes`/`online`).
- `events.bucket` / `events.event_type` si applicano solo a `stats:events`
  (stessa semantica di `GET /v2/stats/events`; `from`/`to` opzionali, se
  omessi l'API decide una finestra di default come già fa oggi via REST).
- Il server risponde con un `subscribed` di conferma e poi con uno
  `snapshot` immediato per ciascun canale richiesto (vedi 5.2), così il
  client ha subito lo stato corrente senza dover fare una fetch REST
  separata al bootstrap.

**`unsubscribe`**: `{"type":"unsubscribe","channels":["stats:events"]}`

**`ping`**: `{"type":"ping"}` — vedi §7 heartbeat.

### 5.2 Server → Client (API → Next.js)

**`subscribed`** (ack): `{"type":"subscribed","channels":[...]}`

**`snapshot`** — stato completo di un canale, inviato alla subscribe e utile
anche come resync periodico:

```json
{ "type": "snapshot", "channel": "stats:summary", "ts": "...", "data": { /* StatsSummary */ } }
```

```json
{ "type": "snapshot", "channel": "stats:clients", "ts": "...", "data": { "clients": [ /* UpdaterClient[] */ ] } }
```

```json
{ "type": "snapshot", "channel": "stats:events", "ts": "...", "data": { /* StatsEventsResponse */ } }
```

**`update`** — delta o refresh, inviato quando cambia qualcosa (vedi §6):

- `stats:summary` → sempre payload completo (`StatsSummary`, è già
  piccolo/aggregato):
  `{"type":"update","channel":"stats:summary","ts":"...","data":{...}}`
- `stats:clients` → **delta**, non tutta la lista (la lista può arrivare a
  qualche centinaio di righe — il codice dashboard attuale la pagina tutta
  perché non ha filtri server-side, non serve reinviarla intera ad ogni
  update):
  ```json
  {
    "type": "update",
    "channel": "stats:clients",
    "ts": "...",
    "data": {
      "upserted": [ /* UpdaterClient[] — nuovi o modificati (es. last_seen_at, online status) */ ],
      "removed_ids": [ /* number[] — rimossi, se mai applicabile */ ]
    }
  }
  ```
- `stats:events` → payload completo coerente coi parametri sottoscritti
  (bucket/event_type), stessa forma di `StatsEventsResponse`.

**`error`**:
```json
{ "type": "error", "ts": "...", "data": { "code": "invalid_params | unauthorized | internal", "message": "..." } }
```

**`pong`**: `{"type":"pong"}`

## 6. Cosa scatena un invio

1. **Ingest di un nuovo `UpdaterEvent`** (`manifest_check` o `download`):
   - `update` su `stats:events` per ogni sottoscrizione il cui
     `event_type` combacia (o è `null`/"tutti") e il cui bucket copre il
     timestamp dell'evento.
   - `update` su `stats:summary` (cambia `events_last_24h`, e
     eventualmente `connected_clients`/`clients_by_version` se il client è
     nuovo o cambia versione).
   - `update` su `stats:clients` con `upserted: [client]` (il client che ha
     generato l'evento: `last_seen_at`, eventualmente `updater_version`,
     `last_ip` aggiornati).
2. **Tick periodico** (default **30s**, configurabile) indipendente dagli
   eventi: ricalcola `connected_clients` in `stats:summary` e lo stato
   "online" dei client in `stats:clients`, perché lo stato online dipende
   dal tempo trascorso (`last_seen_at` vs `window_minutes`) e può cambiare
   *senza* nessun nuovo evento (un client smette di essere "online" col
   solo passare del tempo). Senza questo tick i dati "online" andrebbero
   silenziosamente stantii tra un evento e l'altro.
3. Nessun altro trigger per ora: niente push su modifiche non legate a
   stats (fuori scope).

## 7. Heartbeat e ciclo di vita della connessione

- Ping/pong applicativo ogni **30s** in entrambe le direzioni (oltre
  all'eventuale ping/pong nativo del protocollo WS, se la vostra libreria lo
  gestisce già a livello di frame di controllo, quello applicativo può
  essere ridondante ma rende più semplice il debug lato Next.js).
- Timeout: se il server non riceve nulla (nemmeno un pong) dal client per
  **90s**, chiude la connessione.
- Next.js implementerà riconnessione con backoff esponenziale (1s → 30s
  max) e, alla riconnessione, rimanda `subscribe` con gli ultimi parametri
  noti — non serve stato lato server tra una connessione e l'altra.
- Il server **non** deve bufferizzare update per un client disconnesso: se
  la connessione cade, alla riconnessione basta il nuovo `snapshot`
  post-subscribe. Niente garanzie di delivery "at least once" richieste in
  questa v1 — è un canale di refresh, la fonte di verità resta il DB via
  REST.

## 8. Scalabilità / multi-istanza API

Da confermare col team API: se l'API gira su **più istanze** dietro un load
balancer, un evento ingerito dall'istanza A deve poter generare un `update`
anche per un client WS connesso all'istanza B. Opzioni, in ordine di
preferenza (dipende da cosa avete già in infra):

1. Se avete già Postgres: `LISTEN/NOTIFY` sul commit dell'insert di
   `UpdaterEvent`.
2. Se avete Redis: pub/sub su un canale `stats-events`.
3. Se l'API gira **a istanza singola** (probabile in questo contesto, dato
   il volume: "poche centinaia" di client), niente di tutto questo serve:
   basta un event bus in-process (channel/observer pattern) tra il layer
   che scrive gli eventi e gli handler WS.

**Questo punto va confermato dal team API prima di implementare** — se
l'architettura è single-instance, saltate direttamente all'opzione 3 e
risparmiate complessità.

## 9. Compatibilità e rollout

- Gli endpoint REST `stats/*` **restano invariati e disponibili** — sono il
  fallback se il WS non è raggiungibile e il modo in cui Next.js fa il
  primo bootstrap se preferisce non aspettare lo `snapshot` WS.
- Nessuna modifica ai tipi/contratti REST esistenti.
- Versionamento: il protocollo WS vive sotto `/v2`, quindi eventuali
  breaking change ai messaggi vanno su un nuovo path (es. `/v3/...`) o su un
  campo `"protocol_version"` nell'envelope, da decidere in base a quanto
  vi aspettate che cambi.

## 10. Checklist implementativa (lato API)

- [x] Endpoint upgrade WS su `/v2/stats/stream`, auth via `X-Admin-Key` (+
      `X-Dashboard-Key` opzionale) validata **prima** dell'upgrade.
- [x] Parsing/validazione di `subscribe` (`channels`, `params`), risposta
      `subscribed` + `snapshot` immediato per canale.
- [x] Handler `update` su ingest evento (§6.1) con delta corretto per
      `stats:clients`.
- [x] Tick periodico configurabile per `connected_clients`/online status
      (§6.2), default 30s.
- [x] Ping/pong applicativo + timeout disconnessione (§7).
- [x] Messaggi `error` con `code` esplicito per: auth fallita, parametri di
      subscribe non validi, canale sconosciuto.
- [ ] Log/metriche: numero di connessioni WS attive, numero di `update`
      inviati per canale, latenza tra ingest evento e push. (basic slog
      logging only for now - no counters/histograms wired into OTel yet.)
- [x] Decisione su §8 (multi-istanza) in base all'infra reale: single
      instance, in-process bus (see `internal/statshub` package doc).

## 11. Domande aperte per il team API

1. Linguaggio/framework dell'API e libreria WS che intendete usare (per
   capire se leggere header custom in fase di upgrade è banale — lo è nella
   stragrande maggioranza dei framework, ma va confermato).
   → Go + chi; header di upgrade leggibili come una richiesta HTTP normale,
   nessuna complicazione. Libreria WS: `github.com/coder/websocket`.
2. L'API gira a istanza singola o dietro load balancer con più repliche?
   (determina se serve §8).
   → Istanza singola nel contesto attuale; risolto con opzione 3.
3. Avete già un meccanismo di notifica su insert (trigger DB, outbox,
   ecc.) da riusare, o va aggiunto da zero nel path di ingest degli eventi
   updater?
   → Aggiunto da zero: `internal/statshub.Hub`, pubblicato da
   `recordUpdaterEvent` (internal/handlers/updates.route.go).
4. Preferite il path `/v2/stats/stream` o un prefisso dedicato tipo
   `/v2/ws/stats`?
   → `/v2/stats/stream`, come da prima opzione della spec.
