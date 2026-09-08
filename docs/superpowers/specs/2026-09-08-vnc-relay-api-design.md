# Relay VNC (dashboard → API → EMLy Updater → TightVNC)

Design document — 2026-09-08

Controparte lato API di
`emly-updater/docs/superpowers/specs/2026-09-08-vnc-relay-design.md`. Questo
documento è la fonte di verità del **protocollo** (endpoint, token, stati,
codici di risposta); lo spec dell'updater descrive come il client lo consuma.
Una modifica al protocollo tocca entrambi.

---

## 0. Il problema

Le macchine con EMLy stanno dietro NAT e firewall del cliente. TightVNC gira
in locale sulla macchina. L'API sta in cloud e **non può aprire una
connessione TCP verso la macchina**: qualunque soluzione basata su un proxy
che fa `net.Dial` verso il VNC (websockify, `go-vncproxy`, il repeater
UltraVNC) presuppone il contrario, e funziona solo dentro la LAN del
cliente — cioè su un site mirror, non sull'istanza cloud.

L'unica direzione praticabile è quella che l'Updater usa già per tutto il
resto: **è la macchina che chiama l'API**. L'API quindi non è un proxy VNC,
è un **rendezvous**: accoppia due WebSocket e copia i byte fra loro. Non
parla mai RFB, non lo interpreta e non lo può interpretare.

```
  macchina cliente (NAT)                API (cloud)              operatore
  ┌────────────────────┐               ┌──────────┐             ┌─────────┐
  │ TightVNC           │               │          │             │ noVNC   │
  │   127.0.0.1:5900   │               │ vncrelay │             │ (browser)│
  │        ▲ TCP       │               │ rendez-  │             └────┬────┘
  │        │           │               │  vous    │                  │
  │ EMLy Updater ──────┼── wss out ───►│ /v2/vnc/ │◄──── wss ────────┘
  │  (poll 30s)        │               │  agent   │  /v2/vnc/session/{token}
  └────────────────────┘               └──────────┘
```

Con noVNC come viewer il browser parla WebSocket nativamente e trasporta RFB
in frame binari: **non serve nessun proxy VNC in questo repo**. Il bridge
WS↔TCP esiste solo lato Updater, dove il TCP è locale.

## 1. Decisioni prese

- **Poll a 30s, non connessione persistente.** L'Updater interroga
  `GET /v2/vnc/pending` su un timer breve. Alternativa scartata: una WS di
  controllo permanente per macchina — latenza 1-2s invece di ≤30s, ma
  introduce N connessioni permanenti sulla flotta (FD, goroutine, backoff,
  keepalive attraverso i firewall dei clienti) e un ciclo di vita di
  connessione che il servizio Windows oggi non ha. Per un tool di assistenza
  interno l'attesa vale la semplicità. Il protocollo qui sotto è compatibile
  con un upgrade successivo: si aggiunge un canale di risveglio, gli stati
  della sessione non cambiano.
- **Il viewer è noVNC nel browser.** Quindi il token della sessione viaggia
  nel **path**, non in un header: il browser non può impostare header custom
  su un handshake WebSocket. Per lo stesso motivo *non* si riusa
  `X-Admin-Key` con il fallback `?admin_key=` dello stats stream — una chiave
  permanente in query string finisce nei log di accesso, nella history e nel
  referrer. Il viewer usa un **ticket monouso a vita breve**.
- **Rendezvous in-process, single-instance**, come `internal/statshub`:
  agente e viewer devono atterrare sullo stesso processo API. Con più
  repliche dietro un load balancer serve routing sticky sull'id di sessione,
  o un secondo hop fra repliche. Fuori scope, documentato nel package doc.
- **Il documento remoto è il kill switch**, coerente con la convenzione
  dell'updater: nessun flag locale sulla macchina abilita il VNC se il
  documento dice di no.

## 2. Endpoint

Tutti sotto `/v2/vnc/`, gruppo nuovo in `internal/routes/v2/vnc.go`.

| Metodo | Path | Auth | Chi chiama |
|---|---|---|---|
| `POST` | `/v2/vnc/sessions` | `X-Admin-Key` | dashboard |
| `GET` | `/v2/vnc/sessions/{id}` | `X-Admin-Key` | dashboard (polling stato) |
| `DELETE` | `/v2/vnc/sessions/{id}` | `X-Admin-Key` | dashboard (annulla/chiude) |
| `GET` | `/v2/vnc/sessions` | `X-Admin-Key` | dashboard (storico/audit) |
| `GET` | `/v2/vnc/pending` | `X-Api-Key` + `X-EMLy-HWID` | Updater |
| `POST` | `/v2/vnc/sessions/{id}/deny` | `X-Api-Key` + agent token | Updater |
| `GET` (WS) | `/v2/vnc/agent` | `X-Api-Key` + agent token | Updater |
| `GET` (WS) | `/v2/vnc/session/{token}` | ticket nel path | noVNC |

### 2.1 `POST /v2/vnc/sessions`

```jsonc
// richiesta
{ "hwid": "…", "target": "127.0.0.1:5900" }   // target opzionale
// oppure { "client_id": 42 }
```

```jsonc
// 201
{
  "session_id": 118,
  "status": "pending",
  "hwid": "…",
  "hostname": "PC-MI-0042",
  "viewer_path": "/v2/vnc/session/8Kx…",   // ticket monouso, già dentro il path
  "expires_at": "2026-09-08T10:35:00Z"
}
```

Errori: `404` se l'hwid non è in `updater_clients`, `409` se quella macchina
ha già una sessione `pending`/`claimed`/`active`, `429` se il cap globale
(`VNC_MAX_SESSIONS`) è pieno, `503` se `VNC_RELAY_ENABLED=false`.

### 2.2 `GET /v2/vnc/pending` — il poll dell'agente

Questo è l'endpoint che ogni macchina della flotta chiama ogni 30 secondi.
Deve essere il più economico possibile: una `SELECT` su
`(hwid, status)` indicizzata, nessun join, nessuna scrittura quando non c'è
niente da fare.

- **`204 No Content`** — nessuna sessione in attesa. È il 99,99% delle
  risposte. Stessa convenzione di `GET /v2/config`: mai `404` per "niente da
  darti".
- **`200`** — c'è una sessione. La riga passa a `claimed` e viene emesso
  l'agent token (monouso, mostrato solo qui).
  ```jsonc
  {
    "session_id": 118,
    "agent_token": "qP2…",
    "target": "127.0.0.1:5900",
    "requested_by": "lyz",
    "consent": "required",
    "expires_at": "2026-09-08T10:35:00Z"
  }
  ```
- **`404`** — **significa "questo server non implementa il relay"**, non "non
  ho sessioni". Un mirror non ancora aggiornato risponde così: l'agente
  smette di pollare quel server e lo logga una volta sola, esattamente come
  fa oggi con il manifest updater. È il motivo per cui il caso "niente da
  fare" è `204` e non `404`.
- `401` chiave sbagliata, `503` relay disabilitato.

`X-EMLy-HWID` è l'header che l'Updater manda già a ogni manifest check, e
`updater_clients.hwid` ha già la sua unique key (migration 9). Nessuna nuova
nozione di identità macchina.

### 2.3 Le due WebSocket

**`GET /v2/vnc/agent`** — headers `X-Api-Key` e `X-VNC-Agent-Token`. L'API
verifica il token contro una sessione in stato `claimed`, poi fa l'upgrade e
registra il lato agente nel rendezvous.

**`GET /v2/vnc/session/{token}`** — nessun header (è un browser). Il ticket
nel path è consumato al primo upgrade riuscito: un secondo tentativo con lo
stesso ticket riceve `401`. `AcceptOptions.Subprotocols` deve includere
`"binary"`: noVNC invia `Sec-WebSocket-Protocol: binary` e chiude la
connessione se il server non glielo conferma.

Chi arriva per primo **aspetta** l'altro fino a `VNC_RENDEZVOUS_TIMEOUT`
(default 60s), poi chiude con `1011`. Il caso normale è che l'agente arrivi
per primo: la dashboard aspetta lo stato `claimed` prima di aprire noVNC, così
l'operatore vede "in attesa della macchina…" invece di uno schermo nero.

Appaiate le due connessioni, il relay è:

```go
agent  := websocket.NetConn(ctx, agentWS,  websocket.MessageBinary)
viewer := websocket.NetConn(ctx, viewerWS, websocket.MessageBinary)
go io.Copy(agent, viewer)
io.Copy(viewer, agent)
```

L'API conta i byte nei due sensi e li scrive in `vnc_sessions` alla chiusura.
Nessun frame viene ispezionato: `MessageBinary` in entrata, `MessageBinary` in
uscita, e nient'altro.

## 3. Macchina a stati

```
pending ──(GET /pending)──► claimed ──(entrambe le WS)──► active ──► closed
   │                           │                                        ▲
   │                           └──(POST /deny)──► denied                │
   └──(TTL, DELETE)──► expired / closed ──────────────────────────────►─┘
```

- `pending` → `claimed`: unica scrittura del poll, con `UPDATE … WHERE
  status='pending'` così due poll concorrenti non possono reclamare la stessa
  riga due volte.
- `claimed` → `denied`: l'utente alla macchina ha rifiutato il consenso. La
  dashboard lo mostra come tale, non come timeout: è un'informazione diversa.
- `active` → `closed`: una delle due parti chiude, o si supera
  `VNC_MAX_SESSION_DURATION`.
- `pending`/`claimed` scadono dopo `VNC_SESSION_TTL` (default 5m — deve
  coprire un ciclo di poll da 30s **più** il tempo che l'utente impiega a
  rispondere al prompt di consenso).

## 4. Schema

`internal/database/schema/migrations/14_vnc_sessions.sql`, con la sua voce
`table_not_exists` in `tasks.json`.

```sql
CREATE TABLE IF NOT EXISTS `vnc_sessions` (
    `id`              BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
    `client_id`       INT UNSIGNED NOT NULL,
    `hwid`            VARCHAR(64) NOT NULL,
    `status`          ENUM('pending','claimed','active','closed','denied','expired','failed')
                          NOT NULL DEFAULT 'pending',
    `viewer_token_hash` CHAR(64) NOT NULL,
    `agent_token_hash`  CHAR(64) NULL,
    `target`          VARCHAR(64) NOT NULL DEFAULT '127.0.0.1:5900',
    `requested_by`    VARCHAR(255) NOT NULL DEFAULT '',
    `requested_ip`    VARCHAR(45) NULL,
    `close_reason`    VARCHAR(255) NULL,
    `bytes_to_viewer` BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `bytes_to_agent`  BIGINT UNSIGNED NOT NULL DEFAULT 0,
    `created_at`      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `expires_at`      TIMESTAMP NOT NULL,
    `claimed_at`      TIMESTAMP NULL,
    `started_at`      TIMESTAMP NULL,
    `ended_at`        TIMESTAMP NULL,
    UNIQUE KEY `uniq_viewer_token` (`viewer_token_hash`),
    INDEX `idx_hwid_status` (`hwid`, `status`),
    INDEX `idx_created` (`created_at`),
    FOREIGN KEY (`client_id`) REFERENCES `updater_clients` (`id`) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
```

I token sono 32 byte da `crypto/rand` in base64url, e in tabella ci va solo
lo **SHA-256**: il valore in chiaro esiste una sola volta, nella risposta che
lo emette. La lookup è per hash.

## 5. Sicurezza

Questo è il punto che conta davvero: il resto è idraulica.

1. **TightVNC deve ascoltare solo su loopback**
   (`HKLM\SOFTWARE\TightVNC\Server`, `LoopbackOnly=1`, `AllowLoopback=1`),
   con self-heal lato Updater nello stile di `internal/assoc`. Con il VNC su
   loopback l'unica via d'ingresso alla macchina è il tunnel autenticato: non
   c'è una porta 5900 esposta su nessuna LAN cliente, e la password VNC non è
   più l'unica cosa fra un attaccante e il desktop.
2. **Ticket monouso a TTL breve**, uno per lato, mai la admin key in URL.
3. **Consenso dell'utente alla macchina** prima di aprire il tunnel, con il
   dialog WTS che l'Updater ha già (`internal/notify/console_user.go`).
   Modalità per sito nel documento remoto: `required` (default) / `notify`
   (avvisa e procede) / `none` (macchine non presidiate: chioschi, sale).
   Su PC di clienti questa è la parte che conta contrattualmente, non
   l'implementazione del tunnel.
4. **Audit completo** in `vnc_sessions`: chi ha chiesto, quale macchina,
   quando, per quanto, quanti byte, come è finita. È interrogabile da
   `GET /v2/vnc/sessions`.
5. **Kill switch e cap**: `vnc.enabled` nel documento remoto (lato macchina),
   `VNC_RELAY_ENABLED` (lato API), `VNC_MAX_SESSIONS` per il tetto di
   sessioni concorrenti, `httprate` sulla creazione sessioni.
6. **`VNC_MAX_SESSION_DURATION`** (default 2h): un tunnel dimenticato aperto
   non resta aperto per sempre.

## 6. Middleware — la trappola nota

Entrambe le route WebSocket vanno registrate nel `mux` di `main.go` con lo
stack ridotto, **non** nello stack di `r`. `chiMiddleware.Timeout` avvolge
ogni risposta in `TimeoutHandler`, il cui `ResponseWriter` non implementa
`http.Hijacker`: è esattamente il bug del commit `a0cb4f0`
(`failed to accept WebSocket connection: http.ResponseWriter does not
implement http.Hijacker`), e riapparirebbe identico qui. Vedi
`main.go:197-221` per il pattern e il commento.

`/v2/vnc/session/` va montato come prefisso (il ticket è nel path), quindi
l'handler estrae il token con `chi.URLParam` e, quando serve, con un
`strings.TrimPrefix` sul path — il `mux` non ha i parametri di chi.

Anche il rate limiting va ricalibrato: `httprate.LimitByIP(30, time.Minute)`
è pensato per richieste brevi, mentre una sessione VNC è *una* richiesta che
dura un'ora. Il limite serve sulla creazione delle sessioni e sul poll, non
sulle due WS.

## 7. Banda e capacità

Tutto il traffico pixel transita dalla tua API. TightVNC + noVNC in Tight
encoding con JPEG stanno indicativamente sui 0,3-1 Mbps per sessione attiva,
con picchi di qualche Mbps sui redraw a schermo intero. Per una manutenzione
"guarda e clicca" è poco; dieci sessioni simultanee su un'istanza cloud
piccola non lo sono. Da qui `VNC_MAX_SESSIONS` (default 20) e i contatori di
byte in `vnc_sessions`, che dopo un mese dicono quanto costa davvero.

Il costo del poll è l'altro numero da tenere d'occhio: 500 macchine × 1
richiesta ogni 30s = ~17 req/s costanti che rispondono `204`. Trascurabile in
sé, ma è il motivo per cui `GET /v2/vnc/pending` non deve toccare il DB più
di una `SELECT` indicizzata.

## 8. Configurazione

Nuove variabili in `internal/config/config.go` — e, per convenzione del
repo, nello stesso commit anche in `.env.example` e in `docker-compose.yml`
con la sintassi `${VAR:-default}`:

| Var | Default | Cosa fa |
|---|---|---|
| `VNC_RELAY_ENABLED` | `false` | interruttore generale del gruppo `/v2/vnc` |
| `VNC_SESSION_TTL` | `5m` | vita di una sessione `pending`/`claimed` |
| `VNC_RENDEZVOUS_TIMEOUT` | `60s` | quanto un lato aspetta l'altro |
| `VNC_MAX_SESSIONS` | `20` | sessioni `active` simultanee |
| `VNC_MAX_SESSION_DURATION` | `2h` | tetto sulla singola sessione |
| `VNC_POC_PAGE` | `false` | serve la pagina noVNC di test (§10) |

Lato documento remoto (`internal/remoteconfig` qui, `internal/policy`
nell'updater), sezione nuova:

```jsonc
"vnc": {
  "enabled": false,
  "target": "127.0.0.1:5900",
  "pollInterval": "30s",
  "consent": "required",
  "consentTimeout": "60s"
}
```

Aggiungere un campo qui tocca **quattro** posti più le fixture: `document.go`
e `parse.go` e `legacy.go` nell'updater, `internal/remoteconfig` qui, e
`testdata/remoteconfig/` copiata verbatim nei due repo. Una regola aggiunta
su un lato solo è una regola che l'altro lato non ha.

## 9. Pacchetti

- **`internal/vncrelay/`** — HTTP- e DB-free come `statshub` e
  `remoteconfig`: solo il rendezvous (registrazione di un lato, attesa
  dell'altro, appaiamento, conteggio byte) e il tipo sessione. Nessuna query,
  nessun `http.ResponseWriter`.
- **`internal/handlers/vnc.route.go`** — REST + i due upgrade WS.
- **`internal/routes/v2/vnc.go`** — montaggio, gating admin/api key.
- **`main.go`** — le due route WS nel `mux`, il reaper delle sessioni scadute.

## 10. PoC

Implementato in questo repo come primo passo, per misurare latenza e banda
reali attraverso il cloud prima di costruire il resto:

- rendezvous e store sessioni **in memoria** (nessuna migration, nessun
  audit): l'obiettivo è il percorso end-to-end, non la persistenza;
- nessun consenso utente, nessun kill switch nel documento remoto;
- `GET /v2/vnc/poc` serve una pagina noVNC minimale (noVNC da jsdelivr come
  modulo ES), gated da `VNC_POC_PAGE=true`;
- lato Updater, un sottocomando `vnc-agent` in foreground che polla e fa da
  bridge, senza toccare il ciclo del servizio.

Quello che il PoC deve dire prima di procedere: la latenza percepita in
sessione, la banda per sessione, e se 30s di attesa fra click e schermata
sono accettabili per l'assistenza o se serve il canale di risveglio
persistente (§1).

## 11. Checklist implementativa

- [ ] `internal/vncrelay` + test del rendezvous (entrambi gli ordini di
      arrivo, timeout, chiusura di un lato)
- [ ] migration `14_vnc_sessions.sql` + `tasks.json`
- [ ] `internal/handlers/vnc.route.go` (REST + 2 WS)
- [ ] `internal/routes/v2/vnc.go` + routing test come
      `stats_stream_routing_test.go`
- [ ] bypass middleware nel `mux` di `main.go` per le due route WS
- [ ] reaper sessioni scadute + `VNC_MAX_SESSION_DURATION`
- [ ] config + `.env.example` + `docker-compose.yml`
- [ ] sezione `vnc` in `internal/remoteconfig` + fixture condivise
- [ ] `CLAUDE.md`: gruppo `/v2/vnc`, `internal/vncrelay`, nuove env var
