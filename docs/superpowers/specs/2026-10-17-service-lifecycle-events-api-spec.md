# Eventi di ciclo di vita del servizio — spec per l'API

Spec per il team API — 2026-10-17

Nuovi eventi del canale client (`GET /v2/client/ws`, protocollo v2):
**`service.stopped`**, **`machine.power`**, e il nuovo campo **`last_stop`** di
`service.started`. Riferimento normativo: `CLIENT_WS_PROTOCOL.md` §8.6, §8.7,
§8.8. Controparte Agent: branch `feat/service-stopped-event` di `emly-updater`
(`internal/service/stopevents.go`, `internal/wsclient/protocol.go`).

---

## 1. Perché

Oggi il server sa quando un Agent **riparte** (`service.started`), ma non
quando e perché si è **fermato**. Una macchina che sparisce dalla presenza
può essere stata spenta, aggiornata, fermata a mano, messa in sospensione o
andata in crash, e la dashboard non distingue i casi. Con questa modifica:

- l'Agent dice perché si ferma, mentre si ferma (`service.stopped`);
- se quel messaggio non arriva (allo spegnimento la rete può essere già giù),
  lo ripete al riavvio successivo dentro `service.started.last_stop`;
- un crash, una mancanza di corrente o un kill forzato diventano visibili:
  `last_stop.reason: "unexpected"`;
- sospensione e ripresa hanno un evento loro (`machine.power`), perché non
  sono un arresto: il processo resta vivo.

## 2. Cosa deve fare l'API

### 2.1 Obbligatorio

1. **Aggiungere `service.stopped` e `machine.power` a
   `clientproto.ServerCapabilities`.** È il punto che conta: `welcome.
   accepted_capabilities` è l'intersezione fra la lista del client e
   `ServerCapabilities`, e l'Agent **non manda** un evento che il server non
   ha accettato. Senza questa riga i due eventi non arrivano mai.
2. **Aggiungere a `clientproto` nomi e tipi identici a quelli dell'Agent**
   (§3): `EvtServiceStopped`, `EvtMachinePower`, `ServiceStopped`,
   `LastStop`, `MachinePower`, il campo `ServiceStarted.LastStop`, le
   costanti di `reason`/`category`/`state` e `StopCategory`. Nomi diversi
   tra i due repo sono uno split di protocollo.
3. **Registrare gli eventi come tutti gli altri** nell'anello per-client di
   `internal/clienthub`: già succede da solo per ogni `name` dichiarato e
   accettato. Nessun effetto collaterale è richiesto.
4. **Tollerare valori sconosciuti.** Un `reason` o `state` che il server non
   conosce va registrato così com'è, non rifiutato: l'elenco potrà crescere.

### 2.2 Già fatto nel branch `feat/service-stopped-event` di `emly-go-api`

- `internal/clientproto/protocol.go`: costanti, tipi, `StopCategory`, i due
  eventi in `ServerCapabilities`.
- `internal/clientws/events.go`: `service.stopped` viene loggato a Info
  (`reason`, `category`, `command_id`); `service.started` con `last_stop`
  logga il motivo dell'arresto precedente. Nessuna scrittura su DB.
- `internal/clientproto/protocol_test.go`: `TestServerAcceptsTheLifecycleEvents`,
  `TestStopCategory`.
- `CLIENT_WS_PROTOCOL.md`: §1, §4 (esempio di `capabilities`), §8 (tabella),
  §8.6 (`last_stop`), §8.7, §8.8, §15.

Va rivisto e, se va bene, unito: il resto di questa spec è ciò che il branch
**non** fa.

### 2.3 Da decidere lato API (proposte)

Ogni proposta è facoltativa e indipendente dalle altre.

**A. Persistere l'ultimo arresto sulla macchina** (consigliata). L'anello
eventi è in memoria, tiene 50 voci e viene ripulito dopo 24h: una macchina
spenta venerdì sera ha perso il suo `service.stopped` lunedì mattina, e lo
stesso vale dopo un riavvio dell'API. Per mostrare in dashboard "Ultimo
arresto: spegnimento, venerdì 18:02" serve una colonna. Proposta: migration
su `updater_clients` con

| Colonna | Tipo | Da |
|---|---|---|
| `last_stop_reason` | `VARCHAR(20) NULL` | `service.stopped.reason`, o `last_stop.reason` |
| `last_stop_category` | `VARCHAR(20) NULL` | idem, `category` |
| `last_stop_at` | `DATETIME NULL` | `at` (assente per `unexpected`: `NULL`) |

Scritta da `handleEvent`, **senza toccare `last_seen_at`**, con la stessa
regola di `session.changed` (§8.1 del protocollo). Se arrivano entrambi (un
`service.stopped` dal vivo e poi `last_stop` con `sent: true`), il secondo
riscrive lo stesso valore: è idempotente. Un `last_stop` con `sent: false`
è l'unica fonte di quell'arresto.

**B. Esporre il motivo nella presenza.** Quando la connessione cade subito
dopo un `service.stopped`, `presencehub` potrebbe riportare il motivo
insieme allo stato offline ("offline: spegnimento" invece di "offline").
Serve tenere in memoria l'ultimo `service.stopped` per `client_id` fino alla
disconnessione.

**C. Filtri e conteggi.** Per esempio "macchine con arresti `unexpected`
negli ultimi 7 giorni", per trovare i PC che crashano o vengono spenti dal
pulsante. Richiede A, oppure una tabella storica dedicata se interessa più
dell'ultimo valore.

Per la dashboard serve, come minimo, la descrizione dei valori (§3) per
tradurli in etichette.

## 3. Contratto sul filo

### 3.1 `service.stopped` (C→S)

Mandato mentre il servizio si ferma. Best-effort: inviato dal vivo con un
limite di 3s e **mai messo nel buffer**, perché quel processo non avrà
un'altra connessione.

```json
{ "type": "event", "id": "01JA…", "ts": "2026-10-17T09:12:40.120Z",
  "data": { "name": "service.stopped", "payload": {
    "reason": "self_update", "category": "automatic",
    "at": "2026-10-17T09:12:40Z", "to_version": "1.9.0",
    "uptime_seconds": 86412, "updater_version": "1.8.2" } } }
```

| Campo | Tipo | Note |
|---|---|---|
| `reason` | string | Tabella sotto. Sempre presente. |
| `category` | string | `manual` \| `automatic` \| `machine`. Sempre presente, derivata da `reason`. |
| `at` | string RFC 3339, UTC | Quando l'Agent ha ricevuto l'arresto. |
| `command_id` | string, opzionale | Solo per `command` e `reboot`: l'`id` del `service.restart`/`machine.reboot`. |
| `to_version` | string, opzionale | Solo per `self_update`: la versione che il setup sta installando. |
| `uptime_seconds` | int | Da quanto girava il processo (non la macchina). |
| `updater_version` | string | Versione dell'Agent che si ferma. |

| `reason` | `category` | Quando |
|---|---|---|
| `manual` | `manual` | Arresto senza altra causa nota: services.msc, `sc stop`, `net stop`, `EMLyUpdater.exe stop`. |
| `uninstall` | `manual` | Disinstallazione (`EMLyUpdater.exe uninstall`). |
| `restart` | `manual` | `restart-service` lanciato a mano, o impostazioni salvate dal tray (il servizio si riavvia per rileggerle). |
| `upgrade` | `automatic` | Un setup dell'Agent lanciato dall'esterno (a mano, GPO, SCCM). |
| `self_update` | `automatic` | L'Agent ha lanciato da sé il proprio aggiornamento. |
| `command` | `automatic` | `service.restart` inviato dal server. |
| `shutdown` | `machine` | Spegnimento **o** riavvio di Windows: dal servizio non si distinguono. |
| `reboot` | `machine` | Riavvio chiesto con un `machine.reboot` del server. |

Limiti noti, da tenere presenti nell'interpretazione:

- `manual` è il residuo: vuol dire "nessuna causa nota", non "di sicuro una
  persona". Un setup di una build vecchia dell'Agent ferma il servizio senza
  dire perché, e risulta `manual`.
- Per `command` di solito l'evento dal vivo **non arriva**: per
  `service.restart` l'Agent chiude la connessione (1001) prima di fermarsi.
  Arriva pochi secondi dopo come `last_stop` (`sent: false`). Il comando
  resta chiuso da `completed_commands` come oggi.
- Con l'Avvio rapido di Windows attivo, "Arresta il sistema" iberna invece di
  spegnere: risulta `machine.power` `suspend`/`resume`, non `shutdown`. Il
  riavvio è sempre un arresto vero.

### 3.2 `service.started.last_stop` (C→S, campo nuovo)

Come è finito il processo precedente, letto da `state.json` dell'Agent. Il
motivo viene scritto lì **prima** dell'invio dal vivo, quindi c'è anche
quando l'invio non è passato.

```json
{ "reason": "boot", "started_at": "2026-10-17T07:31:40Z", "updater_version": "1.9.0",
  "last_stop": {
    "reason": "shutdown", "category": "machine", "at": "2026-10-16T18:02:11Z",
    "sent": false, "previous_started_at": "2026-10-16T07:58:40Z",
    "previous_version": "1.9.0" } }
```

| Campo | Note |
|---|---|
| `reason`, `category`, `command_id`, `to_version` | Come §3.1. In più `reason: "unexpected"` con `category: "unexpected"`. |
| `at` | Quando si è fermato. **Assente** per `unexpected`: un crash non lascia l'ora. |
| `sent` | `true` se il `service.stopped` dal vivo era arrivato: in quel caso il server ce l'ha già, e questo è un duplicato. `false`: questa è l'unica notizia di quell'arresto. |
| `previous_started_at` | Avvio del processo che si è fermato. Per `unexpected` è l'unico riferimento temporale. |
| `previous_version` | Versione del processo che si è fermato (diversa da `updater_version` dopo un aggiornamento). |

`last_stop` è **assente** quando l'Agent non ha storia: prima installazione,
o primo avvio dopo l'aggiornamento da una build che non scriveva il record.

`unexpected` vuol dire: il processo precedente ha registrato il proprio
avvio ma nessun arresto pulito. Crash, mancanza di corrente, kill da Task
Manager, schermata blu.

### 3.3 `machine.power` (C→S)

```json
{ "type": "event", "id": "01JA…", "ts": "…",
  "data": { "name": "machine.power", "payload": { "state": "suspend", "at": "2026-10-17T12:30:02Z" } } }
```

| Campo | Note |
|---|---|
| `state` | `suspend` (sospensione o ibernazione) \| `resume`. |
| `at` | Quando l'Agent ha ricevuto la notifica, RFC 3339 UTC. |

`suspend` parte subito (limite 2s). Se non passa resta nel buffer in memoria
dell'Agent e arriva **insieme a** `resume`, nell'ordine giusto, quando il
canale si riconnette dopo il risveglio. Quindi il `ts` di ricezione può
essere molto dopo `at`: per la cronologia vale `at`. Nessun
`service.stopped` né `service.started` accompagna una sospensione.

## 4. Sequenze

Spegnimento con la rete ancora su:

```
C→S event service.stopped {"reason":"shutdown","category":"machine",…}
    (connessione cade)
    … boot …
C→S event service.started {"reason":"boot","last_stop":{"reason":"shutdown","sent":true,…}}
```

Spegnimento senza rete, o crash:

```
    (connessione cade, nessun evento)
    … boot …
C→S event service.started {"reason":"boot","last_stop":{"reason":"shutdown","sent":false,…}}
C→S event service.started {"reason":"boot","last_stop":{"reason":"unexpected","previous_started_at":"…"}}   ← caso crash
```

Auto-aggiornamento dell'Agent:

```
C→S event update.started {"target":"updater","to_version":"1.9.0",…}
C→S event service.stopped {"reason":"self_update","to_version":"1.9.0",…}
    … la nuova build parte …
C→S event service.started {"reason":"self_update","previous_version":"1.8.2","last_stop":{"reason":"self_update","sent":true,…}}
C→S event update.applied {"target":"updater",…}
```

Sospensione:

```
C→S event machine.power {"state":"suspend",…}        (o niente, se la rete è già giù)
    … sleep, connessione cade, risveglio, riconnessione …
C→S event machine.power {"state":"suspend",…}        (dal buffer, se prima non era passato)
C→S event machine.power {"state":"resume",…}
```

`service.started` **non** si ripete alla riconnessione dopo una sospensione:
resta una volta per processo.

## 5. Compatibilità

| Agent | API | Risultato |
|---|---|---|
| vecchio | nuova | Nulla cambia: un Agent vecchio non dichiara le nuove capability. |
| nuovo | vecchia | L'API vecchia non accetta i due eventi in `welcome`: l'Agent non li manda. `last_stop` arriva comunque dentro `service.started` (accettato), finisce nel payload grezzo registrato nell'anello ed è visibile nella cronologia, solo non interpretato. |
| nuovo | nuova | Tutto come in §3. |

Nessuna migration è necessaria per la parte obbligatoria; serve solo per la
proposta A. Nessun impatto sul documento di configurazione remota: gli eventi
non sono soggetti a `clientWs.commands`, quindi nessuna fixture nuova in
`testdata/remoteconfig/`.

Carico sul collegamento MPLS: trascurabile. Un evento di poche centinaia di
byte per arresto e per sospensione, fuori dal percorso dei download.

## 6. Verifica

Automatica, nel branch: `go test ./internal/clientproto/ ./internal/clientws/`.

Manuale, con un Agent del branch `feat/service-stopped-event` collegato:

1. `EMLyUpdater.exe stop` sulla macchina → in `GET /v2/client/{id}/events`
   un `service.stopped` `manual`; dopo `start`, `service.started` con
   `last_stop.sent: true`.
2. `EMLyUpdater.exe stop --reason=upgrade` → `upgrade` / `automatic`.
3. `service.restart` dalla dashboard → nessun `service.stopped` dal vivo (o
   al massimo uno), poi `service.started` con `reason: "command"` e
   `last_stop.reason: "command"` con lo stesso `command_id`.
4. Riavvio della macchina → `service.stopped` `shutdown` prima che cada,
   `last_stop` al boot.
5. `taskkill /f /im EMLyUpdater.exe` → al riavvio del servizio
   `last_stop.reason: "unexpected"`, senza `at`.
6. Sospensione e risveglio → `machine.power` `suspend` e poi `resume`,
   nessun `service.started`.
7. Con l'API **senza** questa modifica (punto 2.1.1 assente) → gli eventi
   nuovi non compaiono, `last_stop` sì dentro `service.started`.

## 7. Checklist API

- [x] `clientproto`: nomi, tipi, `StopCategory`, `ServerCapabilities`.
- [x] `clientws/events.go`: log a Info di `service.stopped` e di `last_stop`.
- [x] Test di `clientproto`.
- [x] `CLIENT_WS_PROTOCOL.md` §1, §4, §8, §8.6–§8.8, §15.
- [x] Revisione di `feat/service-stopped-event` (merge in sospeso).
- [x] Proposta A: migration 28, `updaterclient.UpdateLastStop`, scritta da
      `service.stopped` e da `service.started.last_stop`; esposta come
      `last_stop_reason`/`last_stop_category`/`last_stop_at` su
      `GET /v2/stats/clients(/{id})` e `stats:clients`.
- [x] Proposta B: `presencehub.Stopping`/`Stopped`, esposta come
      `offline_stop`. La finestra di grazia resta invariata.
- [ ] Proposta C (non implementata: richiede uno storico, A tiene solo
      l'ultimo valore).
- [x] `ROUTES.md` / `DOCS.md`: citare i nuovi eventi dove si descrive
      `GET /v2/client/{id}/events`.
- [ ] Spec per la dashboard (etichette dei motivi, ed eventualmente "ultimo
      arresto" se passa A).
