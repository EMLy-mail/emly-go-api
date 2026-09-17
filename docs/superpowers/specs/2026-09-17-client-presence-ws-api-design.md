# Presenza client via WebSocket (EMLy Updater ↔ API)

Design document — 2026-09-17

Controparte lato Updater in
`emly-updater/docs/superpowers/specs/2026-09-17-client-presence-ws-design.md`.
Questo documento è la fonte di verità del **protocollo** (endpoint, envelope,
handshake, heartbeat); lo spec dell'updater descrive come il client lo
consuma. Una modifica al protocollo tocca entrambi.

---

## 0. Il problema

Oggi "online" è dedotto lato dashboard da `updater_clients.last_seen_at`
(scritto a ogni `manifest_check`, poll HTTP ogni tot minuti): un client
appena spento resta "online" fino a quando la finestra non scade, e uno
appena acceso non compare finché non fa il primo check. È una stima, non uno
stato.

Il design VNC (`2026-09-08-vnc-relay-api-design.md` §1) aveva scartato
esplicitamente una WebSocket di controllo permanente per il relay VNC, per
il costo che introduce su tutta la flotta (~350-400 macchine): N connessioni
aperte per sempre, con backoff/riconnessione/keepalive attraverso i firewall
dei clienti, un ciclo di vita che il servizio Windows non aveva. Per quel
caso (aprire un tunnel su richiesta) un poll a 30s bastava.

Qui il costo si paga apposta, per due motivi che il poll non copre:

1. **presenza vera**, non stimata — online/offline nel momento in cui
   succede, non con un ritardo pari alla finestra di stima;
2. **canale verso il client per usi futuri** — un modo di raggiungere una
   macchina specifica senza aspettare il suo prossimo poll (es. un trigger
   VNC, invece del poll dedicato che quel design usa oggi). Questo spec non
   implementa nessun comando: apre solo l'envelope in modo che aggiungerne
   uno domani non rompa i client già connessi.

## 1. Decisioni prese

- **Connessione persistente, una per macchina, sempre attiva.** Diversamente
  dal VNC, qui la latenza è il punto: una presenza "quasi in tempo reale"
  dedotta da un poll da 30s non è quello che serve. Il costo (~350-400
  connessioni concorrenti) è accettato.
- **Stato di presenza solo in-memory**, come `internal/statshub`: nessuna
  nuova colonna su `updater_clients`, nessuna tabella. Un riavvio del
  processo API azzera lo stato di presenza (tutti "offline" finché i client
  non si riconnettono, il che avviene entro pochi secondi per il backoff
  lato updater — vedi lo spec gemello). Coerente con
  `internal/statshub`, anch'esso single-instance e volatile.
- **Auth su header, come il resto del self-update.** `X-Api-Key` sulla
  richiesta di upgrade, stesso schema di `GET /v2/updates/manifest/updater`.
  Nessun nuovo meccanismo di autenticazione.
- **L'identità del client viaggia nel primo messaggio JSON, non negli
  header**, a differenza di manifest/download. Motivo: l'upgrade WebSocket
  in `coder/websocket` (lato client, lo stesso schema di `wsclient.Dial`) può
  già impostare header custom, quindi la scelta non è tecnica — è che il
  payload JSON iniziale è il posto naturale per aggiungere/rimuovere campi
  senza far crescere ulteriormente il set di header X-EMLy-* già numeroso.
  Le uniche informazioni che restano fuori (User-Agent per `updater_version`
  e `contact`, IP dalla connessione) sono quelle che l'API già legge dalla
  connessione stessa, non da un header EMLy dedicato.
- **Kill switch nel documento remoto**, stessa convenzione di `vnc.enabled`:
  aggiornare l'updater non accende nulla finché un sito non pubblica una
  revisione con `clientWs.enabled: true`.

## 2. Endpoint

Nuovo gruppo `internal/routes/v2/client.go`.

| Metodo | Path | Auth | Chi chiama |
|---|---|---|---|
| `GET` (WS) | `/v2/client/ws` | `X-Api-Key` | EMLy Updater |

Un solo endpoint. Non c'è REST di supporto: l'unico stato che la connessione
produce (online/offline, ultima identity) vive nel presence hub e viene letto
da `GET /v2/stats/clients` e da `GET /v2/stats/stream` (channel
`stats:clients`), non da una route dedicata.

## 3. Protocollo

Envelope minimo, stesso stile di `stats_stream.route.go`:

```jsonc
{ "type": "...", "data": { ... } }
```

`data` è omesso quando il messaggio non porta payload (`hello`, `ping`,
`pong`).

### 3.1 Handshake

```
GET /v2/client/ws          X-Api-Key
    │
    ├─ chiave mancante/errata ──► 401, nessun upgrade
    │
    └─ upgrade ok
         server ──► { "type": "hello" }
         client ──► { "type": "identity", "data": { ...clientIdentity... } }
              │
              ├─ non identificato (né hwid né hostname) entro 10s ──► server
              │    manda { "type":"error", "data":{"code":"unidentified"} } e
              │    chiude (close code 1008, policy violation)
              │
              └─ identificato ──► server fa upsert (§4), registra il client
                   nel presence hub, entra in modalità heartbeat
```

`data` di `identity` porta lo stesso set di campi che oggi viaggia negli
header `X-EMLy-*` di un manifest check:

```jsonc
{
  "hwid": "…",
  "hostname": "PC-MI-0042",
  "ad_domain": "…",
  "logged_user": "DOMAIN\\utente",
  "logged_user_state": "active-console",
  "logged_user_disconnected_at": "2026-09-17T08:12:00Z",
  "serial": "…",
  "product": "…",
  "os_version": "Windows 11 24H2 Professional (Build 26100.4652)",
  "emly_version": "3.4.1"
}
```

Ogni campo è opzionale allo stesso modo del corrispondente header oggi:
assente significa "non riportato", non "vuoto" — stessa semantica
COALESCE/NULLIF di `upsertUpdaterClient` (§4). `updater_version` e `contact`
non sono nel JSON: si leggono dallo User-Agent della richiesta di upgrade
(`EMLy-Updater/1.6.3 (contatto)`), esattamente come `parseUpdaterUserAgent`
fa oggi per manifest e download. L'IP viene dalla connessione
(`clientIPFromRequest`), non dal payload.

### 3.2 Heartbeat

Dopo l'identity accettata: **ping ogni 10s dal server**, il client deve
rispondere pong entro **20s** (due intervalli) o la connessione è considerata
morta e chiusa lato server.

```jsonc
server ──► { "type": "ping" }
client ──► { "type": "pong" }
```

Un `pong` non aggiorna `last_seen_at` né riesegue l'upsert — è puro
keepalive. I campi mutevoli (utente loggato, stato sessione) restano quelli
dell'ultimo `identity` ricevuto finché la connessione resta aperta; per
vederli aggiornati serve una nuova connessione (riconnessione) o il poll
manifest HTTP esistente, che continua a fare il proprio upsert in modo
indipendente (§6). Il `pong` che il client manda in risposta al ping
applicativo non è distinguibile a livello di frame WS da un
`websocket.Conn.Ping`/`Pong` nativo: si usa il livello applicativo
(envelope JSON) esattamente come `stats_stream.route.go` fa già per la
dashboard, non i control frame RFC 6455, così lo stesso meccanismo di
timeout/lettura bloccante (`ws.Read` con deadline) copre entrambi i lati
senza codice a parte.

### 3.3 Tipi sconosciuti

Un `type` che nessuno dei due lati riconosce viene loggato e scartato, non
chiude la connessione — stessa scelta di
`handleClientMessage`/`default` in `stats_stream.route.go`. È quello che
rende sicuro aggiungere un `type` nuovo (es. `command` in futuro) senza
dover coordinare un rollout sincrono fra API e i ~350-400 updater in campo:
un updater vecchio che riceve un `command` lo ignora invece di disconnettersi.

## 4. Identità e upsert

Il primo messaggio `identity` viene convertito nello stesso tipo
`clientIdentity` (`internal/handlers/updates.route.go`) e passato alla stessa
`upsertUpdaterClient` che manifest/download già usano — nessuna logica di
merge duplicata. `identity.HWID`/`identity.Hostname` sostituiscono
rispettivamente `r.Header.Get("X-EMLy-HWID")`/`r.Header.Get("X-EMLy-Hostname")`
nella costruzione di `clientIdentity`; il resto (`UAVersion`, `Contact`, `IP`)
viene da `clientIdentityFromRequest(r)` sulla richiesta di upgrade originale,
prima ancora del primo messaggio.

L'upsert avviene **una volta per connessione**, all'arrivo dell'`identity`.
Non si ripete a ogni `pong` (§3.2): il canale manifest/download resta la via
per aggiornamenti periodici dei campi mutevoli finché questo canale non
cambia dati durante la sua vita.

## 5. Presence hub — `internal/presencehub`

Pacchetto nuovo, HTTP- e DB-free come `internal/statshub`: nessuna query,
nessun `http.ResponseWriter`, solo lo stato di presenza in memoria.

```go
type Hub struct { /* mu + map[int64]*entry */ }

func New() *Hub
func (h *Hub) Connect(clientID int64) (cancelGrace func())
func (h *Hub) Disconnect(clientID int64)
func (h *Hub) Online(clientID int64) bool
func (h *Hub) OnlineIDs() map[int64]bool
```

- `Connect` registra il client come online e restituisce una funzione che
  annulla un eventuale timer di grazia pendente (una riconnessione rapida
  dopo una disconnessione non deve far "sparire" il client per la finestra di
  grazia di una connessione precedente).
- `Disconnect` non toglie subito l'entry: arma un timer da **15-20s**
  (`presenceGraceDuration`). Se nel frattempo `Connect` richiama con lo
  stesso `clientID`, il timer è cancellato e l'entry resta online senza
  interruzioni visibili sul dashboard. Se il timer scade, l'entry è rimossa
  e il client è "offline" per `Online`/`OnlineIDs`.
- Una nuova connessione con lo **stesso `clientID`** di una già registrata
  (due processi Updater sulla stessa macchina, o una riconnessione che arriva
  prima che il server abbia notato la vecchia connessione morta) sostituisce
  quella vecchia: il presence hub tiene un solo "slot" per client, la
  connessione precedente viene chiusa lato server (`websocket.StatusPolicyViolation`,
  "superseded by newer connection").
- Single-instance, stesso limite architetturale di `statshub` (nessun
  coordinamento fra repliche): documentato nel package doc, non risolto qui.

`GET /v2/stats/clients` e il channel `stats:clients` di
`GET /v2/stats/stream` aggiungono un campo `online: bool` calcolato da
`presencehub.Online(client.ID)` al momento della risposta — non è una colonna
di `updater_clients` e non passa dal `SELECT *` che `fetchAllStatsClients`
già fa. `last_seen_at` resta come oggi (scritto dal poll manifest, non da
questo canale) per distinguere "connesso adesso" da "ultima volta visto
attivo su qualcos'altro".

## 6. Manifest poll HTTP — invariato

Il poll `GET /v2/updates/manifest/updater` e gli altri endpoint di
telemetria (`manifest_check`, `download`) restano esattamente come sono:
`recordUpdaterEvent`/`upsertUpdaterClient` continuano a scrivere
`last_seen_at` e i campi mutevoli indipendentemente da questo canale. I due
percorsi non si escludono: un client con la WS di presenza aperta continua a
pollare il manifest sul proprio intervallo esistente.

## 7. Middleware — la stessa trappola del VNC/stats stream

`GET /v2/client/ws` hijacka la connessione per completare l'upgrade
(`coder/websocket.Accept` richiede `http.Hijacker`). Come già documentato per
`/v2/stats/stream` in `main.go` (commento sopra `wsHandler`), lo stack di
`r` non va bene: `chiMiddleware.Timeout` avvolge la risposta in
`http.TimeoutHandler`, il cui `ResponseWriter` non implementa `Hijacker` —
`main.go:197-221` (`failed to accept WebSocket connection: http.ResponseWriter
does not implement http.Hijacker`).

`/v2/client/ws` va montato nello stesso `mux` accanto a `/v2/stats/stream`,
con uno stack ridotto equivalente (`RequestID → RealIP → Recoverer →
RouteLimitByIP → APIKeyAuth → handler`), **senza** `chiMiddleware.Timeout` né
`AccessLog`/`otelhttp` (una connessione che vive per ore non è "una
richiesta" da loggare come tale). `v2.NewRouter` monta comunque la stessa
route su `r` per restare testabile via `httptest`, come già fa
`stats_stream_routing_test.go` — in produzione il `mux` intercetta il path
prima che raggiunga `r`.

## 8. Configurazione

### 8.1 Env

Nessuna nuova variabile di ambiente: intervalli di ping/grazia sono costanti
di pacchetto (`presenceGraceDuration`, `wsClientPingInterval`), non
configurabili via env — sono dettagli di protocollo condivisi con l'updater,
non tuning locale dell'istanza.

### 8.2 Documento remoto — kill switch

`internal/remoteconfig/types.go`, nuova sezione sullo stesso livello di
`Updater *UpdaterTuning`:

```go
ClientWS *ClientWS `json:"clientWs"`

type ClientWS struct {
	Enabled bool `json:"enabled"`
}
```

Default `enabled: false` quando la sezione è assente (stesso trattamento di
`vnc.enabled` nello spec VNC): un sito che non ha mai pubblicato questa
sezione non fa connettere nessuna macchina. Validazione in
`remoteconfig.Parse`; fixture aggiornate in `testdata/remoteconfig/` — copiate
verbatim nei due repo, stessa regola di `CLAUDE.md` §Documentazione.

## 9. Pacchetti

- **`internal/presencehub/`** — §5, HTTP- e DB-free.
- **`internal/handlers/client_ws.route.go`** — l'upgrade, l'handshake,
  l'heartbeat loop. Struttura a specchio di `stats_stream.route.go`
  (`readLoop`/`pingLoop`, un `context.WithCancel` per connessione), ma un
  solo "canale" implicito invece di sottoscrizioni multiple.
- **`internal/routes/v2/client.go`** — montaggio, `APIKeyAuth` +
  `RouteLimitByIP`.
- **`internal/handlers/stats.route.go`** — `fetchAllStatsClients` prende un
  `*presencehub.Hub` in più e popola `online`.
- **`internal/handlers/stats_stream.route.go`** — `sendSnapshot`/
  `handleTick` per `channelClients` passano per lo stesso hub.
- **`main.go`** — nuova entry nel `mux` accanto a `/v2/stats/stream`,
  costruzione del `presencehub.Hub` accanto a `statsHub`.

## 10. Scala e capacità

~350-400 connessioni concorrenti attese. Ogni connessione: una goroutine
`readLoop` + una `pingLoop` (stesso schema di `stats_stream`), più l'entry nel
presence hub. A questa scala è un budget di goroutine/FD trascurabile per un
processo Go (`stats_stream` già regge lo stesso schema per le connessioni
dashboard, qui il numero è più alto ma l'ordine di grandezza — centinaia, non
migliaia — resta gestibile senza pool o limiti espliciti). Se la flotta
cresce di un ordine di grandezza, il punto da rivedere per primo è la
frequenza di ping (10s × 400 = 40 write/s, banale; × 4000 inizia a contare) e
il fatto che il hub sia single-instance — non prima.

## 11. Testing

- **`internal/presencehub`**: `Connect`/`Disconnect`/grace period (timer
  scade → offline; riconnessione prima della scadenza → resta online;
  connessione doppia sullo stesso `clientID` → la vecchia viene segnalata per
  la chiusura), puro Go, nessuna dipendenza HTTP/DB — come `statshub_test.go`.
- **`internal/handlers/client_ws.route.go`**: auth fallita (401, nessun
  upgrade), identity mancante/non identificata (timeout → chiusura 1008),
  identity valida → upsert avvenuto → hub registra online, ping/pong loop,
  disconnessione → hub segna offline dopo la finestra di grazia. Handshake
  completo su un listener reale, stesso approccio di
  `stats_stream_test.go`.
- **`internal/routes/v2/client_routing_test.go`**: la route è montata,
  risponde 401 senza `X-Api-Key` — stesso schema di
  `stats_stream_routing_test.go`/`updater_routing_test.go`.
- **`internal/remoteconfig`**: `ClientWS` in `Parse`/`Canonical`, default
  assente → `enabled: false`.

## 12. Checklist implementativa

- [ ] `internal/presencehub` + test (grace period, connessione doppia)
- [ ] `internal/handlers/client_ws.route.go` (handshake, heartbeat, upsert)
- [ ] `internal/routes/v2/client.go` + routing test
- [ ] bypass middleware nel `mux` di `main.go`, accanto a `/v2/stats/stream`
- [ ] `fetchAllStatsClients` + `stats_stream.route.go`: campo `online`
- [ ] sezione `clientWs` in `internal/remoteconfig` + fixture condivise
- [ ] `CLAUDE.md`: gruppo `/v2/client`, `internal/presencehub`
- [ ] `ROUTES.md`: nuova riga per `GET /v2/client/ws`
