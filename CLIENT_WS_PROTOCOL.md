# Protocollo messaggi `GET /v2/client/ws` (v2)

Formato dei messaggi scambiati fra l'API e l'EMLy Updater sulla connessione
WebSocket persistente `GET /v2/client/ws`.

- **Stato**: v2 implementata sia lato API (branch `feat/client-ws-v2`):
  envelope, negoziazione `hello`/`identity`/`welcome`, comandi/`ack`/`result`,
  eventi/`notify`, limiti e le route admin (§13), in `internal/clientproto`
  (formato, senza HTTP né stato), `internal/clienthub` (stato in memoria delle
  sessioni v2, dei comandi e degli eventi) e `internal/clientws` (handler
  WebSocket + route admin); sia lato Updater (`emly-updater`, stesso branch
  `feat/client-ws-v2`), in `internal/wsclient` (negoziazione, dispatch
  comandi/eventi/notify) e `internal/service/client*.go` (esecuzione dei
  comandi, catalogo eventi, gestione dei notify) - si veda la checklist §15
  per lo stato comando per comando.
- **Fonte di verità**: questo file è il riferimento normativo del *formato sul
  filo* per `/v2/client/ws`. Il comportamento di ciascun lato (quando si
  connette, come esegue un comando, cosa mostra la dashboard) resta nei
  rispettivi design doc. Una modifica al formato tocca entrambi i repo:
  `emly-go-api` (questo file, `internal/clientws`) ed `emly-updater`
  (`internal/wsclient`, `internal/service/clientws.go`), esattamente come
  `proto/updateripc.proto` fra updater ed EMLy.
- **Documenti collegati**:
  - `docs/superpowers/specs/2026-09-17-client-presence-ws-api-design.md` —
    il protocollo v1, di cui la v2 è un'estensione compatibile;
  - branch `docs/agent-realtime-channel-design`, spec
    `2026-09-07-agent-realtime-channel-design.md` — il modello dei comandi
    (macchina a stati, scaglionamento, autorizzazione) da cui questo formato
    riprende le tre regole di confine (§2).

---

## Indice

1. [Casi d'uso coperti](#1-casi-duso-coperti)
2. [Principi](#2-principi)
3. [Envelope](#3-envelope)
4. [Negoziazione di versione e capability](#4-negoziazione-di-versione-e-capability)
5. [Famiglie di messaggi](#5-famiglie-di-messaggi)
6. [Oggetti condivisi](#6-oggetti-condivisi)
7. [Catalogo comandi (server → client)](#7-catalogo-comandi-server--client)
8. [Catalogo eventi (client → server)](#8-catalogo-eventi-client--server)
9. [Catalogo notify (server → client)](#9-catalogo-notify-server--client)
10. [Codici di errore](#10-codici-di-errore)
11. [Limiti, timeout, chiusura](#11-limiti-timeout-chiusura)
12. [Sicurezza](#12-sicurezza)
13. [Superficie REST (admin)](#13-superficie-rest-admin)
14. [Esempi di sequenza completi](#14-esempi-di-sequenza-completi)
15. [Checklist di implementazione](#15-checklist-di-implementazione)

---

## 1. Casi d'uso coperti

| Caso d'uso | Messaggio | Direzione | Sorgente lato updater |
|---|---|---|---|
| Riavvio servizio | `command` `service.restart` → `ack`, poi `event` `service.started` | S→C, C→S | `svc` manager, `state.json` |
| Riavvio PC | `command` `machine.reboot` → `ack`, poi `event` `service.started` (`reason: "boot"`) | S→C, C→S | `internal/power` (`InitiateSystemShutdownEx`; conto alla rovescia nativo di Windows, nessun avviso dell'updater) |
| Lista aggiornamenti app (winget) | `command` `apps.list_upgradable` → `ack` + `result` | S→C, C→S | `internal/winget` (branch `feat/winget-upgradable`) |
| Info cambio sessione | `event` `session.changed` | C→S | `watchSessions` (branch `feat/session-change-watcher`) |
| EMLy manifest check | `command` `emly.manifest.check` → `result`; esito spontaneo in `event` `update.available` | S→C, C→S | `resolveTarget` |
| EMLy Updater manifest check | `command` `updater.manifest.check` → `result` | S→C, C→S | `ResolveUpdater` + `selfupdate.Decide` |
| Update disponibile (EMLy e Updater) | `event` `update.available` (la macchina lo ha visto) e `notify` `release.published` (il server lo annuncia) | C→S, S→C | `Cycle` / `selfUpdate` |
| Update applicato | `event` `update.started`, `update.applied`, `update.failed` | C→S | `Updater.install`, `selfupdate` (esito al riavvio) |
| Machine info | `command` `machine.info` → `result`; `event` `machine.info` subito dopo l'handshake e quando cambia | S→C, C→S | `internal/machineinfo` |

La tabella è il perimetro della v2. Un caso nuovo si aggiunge come nuovo
`name` dentro una famiglia esistente (§5), non come nuovo `type`.

## 2. Principi

Ripresi dalla spec agent-channel e validi per ogni messaggio di questo
documento.

1. **Il canale non porta mai autorità.** Un `notify` è un suggerimento: il
   client riscarica comunque da REST (`/v2/config`, manifest) e riapplica la
   propria validazione. Un canale compromesso può provocare qualche GET in più,
   non una configurazione cattiva o un setup non firmato.
2. **Nessun comando trasporta codice, percorsi, URL o testo libero.** I comandi
   sono verbi chiusi che il client sa già eseguire, con argomenti enumerati o
   numerici con limiti. Non esiste "esegui questo" né "scarica da qui".
3. **Il poll REST non si tocca.** Canale giù = sistema di oggi. Nessun caso
   d'uso della tabella §1 deve *dipendere* dal canale per funzionare: gli
   eventi riducono la latenza, il poll resta la verità (ad esempio
   `session.changed` anticipa quello che il prossimo poll porterebbe negli
   header `X-EMLy-LoggedUser*`).
4. **Il canale sveglia, non esegue (R-LOOP).** Qualunque comando che tocchi il
   ciclo di update (`emly.manifest.check`, `release.published`, …) non chiama
   `Cycle` in parallelo al `RunLoop`: o lo sveglia (trigger sulla `select`
   esistente), o si ferma prima di `Downloads.Ensure` (check a secco). Mai due
   cicli concorrenti su `state.json`.
5. **Tolleranza sul filo, rigore sull'esecuzione.** `type` sconosciuto →
   ignorato; campo sconosciuto → ignorato; `name` di comando sconosciuto →
   **rifiutato con `ack`** (§5.2), così l'operatore sa che non è stato eseguito.
   È volutamente l'opposto del documento di config, che è all-or-nothing.
6. **Stessa semantica "assente ≠ vuoto" degli header `X-EMLy-*`.** Un campo
   omesso significa "non riportato" e il server conserva il valore che ha; una
   stringa vuota esplicita significa "vuoto". Il client omette (`omitempty`)
   quello che non conosce.

## 3. Envelope

Un solo formato, JSON, un messaggio per frame di testo:

```jsonc
{
  "type": "command",                               // obbligatorio
  "id": "01J8ZQ6T3M6X9K2V7B4N1C5D8E",              // obbligatorio in v2, tranne ping/pong
  "reply_to": "01J8ZQ6S…",                         // solo ack/result
  "ts": "2026-09-23T08:15:02.123Z",                // orologio del mittente, informativo
  "data": { }                                      // payload, omesso se vuoto
}
```

| Campo | Tipo | Obbl. | Note |
|---|---|---|---|
| `type` | string | sì | Famiglia del messaggio (§5). Unico campo esistente in v1 insieme a `data`: i byte di un messaggio v1 sono un envelope v2 valido. |
| `id` | string | v2 sì | Identificativo univoco generato dal mittente, **ULID** (26 caratteri, ordinabile per tempo). Serve a correlare `ack`/`result` e a deduplicare. Assente su `ping`/`pong` (puro keepalive) e sui messaggi v1. |
| `reply_to` | string | per `ack`/`result` | L'`id` del `command` a cui risponde. |
| `ts` | string | no | RFC 3339 UTC con millisecondi. **Solo informativo**: gli orologi dei PC derivano (cfr. `AGENTS.md` dell'updater, "The local clock is only trusted for `until` and staleness"). Il server registra sempre anche la propria ora di ricezione e ordina per quella. |
| `data` | object | no | Payload specifico del tipo. Omesso quando vuoto. |

Regole:

- Nomi di campo in **`snake_case`**, come il payload `identity` v1.
- Enum e nomi (`name`, `topic`, `code`) in **minuscolo, con `.` come
  separatore di namespace e `_` dentro una parola**: `apps.list_upgradable`,
  `update.available`.
- Versioni come stringhe semver senza `v` (`"3.4.1"`), come `GUI_SEMVER`.
- Tempi come RFC 3339 UTC. Durate in secondi interi con suffisso nel nome
  (`delay_seconds`, `duration_ms` quando serve la precisione).
- Nessun campo `null`: un valore sconosciuto si omette (§2.6).

## 4. Negoziazione di versione e capability

La flotta non si aggiorna tutta insieme: il server deve parlare con un updater
v1 e uno v2 sulla stessa rotta, **sempre**. La negoziazione avviene
nell'handshake esistente, senza un round-trip in più.

```
GET /v2/client/ws     X-Api-Key, User-Agent, X-EMLy-HWID, X-EMLy-Hostname
server ──► hello      { "protocol": 2, "server_version": "…", "server_time": "…" }
client ──► identity   { …campi v1…, "protocol": 2, "capabilities": [ … ] }
server ──► welcome    { "protocol": 2, "accepted_capabilities": [ … ], "limits": { … } }
```

- **`hello.data`** (nuovo, opzionale): un updater v1 lo riceve e lo ignora,
  perché oggi `handshake` guarda solo `msg.Type`.
- **`identity.data.protocol`**: assente = `1`. Il server tratta un client v1
  esattamente come oggi e **non gli manda mai** `command`/`notify`/`welcome`.
- **`identity.data.capabilities`**: la lista dei `name` di comando, evento e
  notify che questa build implementa. Il server non invia un `command` il cui
  `name` non è in lista: risponde subito all'operatore con
  `unsupported_command`, senza sprecare un invio. È così che un updater senza
  il modulo winget (o con una build che non ha ancora `machine.reboot`) si
  dichiara, invece di rifiutare a runtime.
- **`welcome`** (nuovo, S→C, solo a client v2): chiude l'handshake e dice al
  client cosa il server userà davvero. `accepted_capabilities` è
  l'intersezione fra quanto dichiarato dal client e quanto il server supporta
  **e** la policy consente per quella macchina (§12.3). `limits` porta i valori
  di §11 che il client deve rispettare (dimensione massima, tetto di eventi).
- Il server supporta **la versione corrente e la precedente**. Una v3 futura
  alzerà `protocol` e si negozierà allo stesso modo; il minimo della
  negoziazione è `min(server, client)`.

Esempio di `identity` v2:

```json
{
  "type": "identity",
  "id": "01J8ZQ5Y0A4F8R7S2T9W3X6Y1Z",
  "data": {
    "hwid": "4C4C4544-0052-4810-8033-B4C04F4E3732",
    "hostname": "PC-MI-0042",
    "ad_domain": "corp.example.it",
    "logged_user": "CORP\\m.rossi",
    "logged_user_state": "active-console",
    "serial": "5R3HJK2",
    "product": "Latitude 5540",
    "os_version": "Windows 11 24H2 Professional (Build 26100.4652)",
    "emly_version": "3.4.1",
    "protocol": 2,
    "capabilities": [
      "service.restart", "machine.reboot", "apps.list_upgradable",
      "emly.manifest.check", "updater.manifest.check", "machine.info",
      "session.changed", "update.available", "update.started",
      "update.applied", "update.failed", "service.started",
      "release.published", "config.published"
    ]
  }
}
```

## 5. Famiglie di messaggi

Sette `type` in tutto. Le prime cinque esistono già in v1.

| `type` | Direzione | v | Scopo |
|---|---|---|---|
| `hello` | S→C | 1 | Apertura handshake. In v2 porta `protocol`. |
| `identity` | C→S | 1 | Identità della macchina. In v2 porta `protocol`, `capabilities`. |
| `ping` / `pong` | S→C / C→S | 1 | Keepalive applicativo, 10s/20s. Invariati. |
| `error` | S→C | 1 | Errore di protocollo, seguito da chiusura. `{code, message}`. |
| `welcome` | S→C | 2 | Fine handshake v2 (§4). |
| `command` | S→C | 2 | Richiesta di un'azione chiusa (§7). |
| `ack` | C→S | 2 | Comando preso in carico o rifiutato. |
| `result` | C→S | 2 | Esito finale di un comando. |
| `event` | C→S | 2 | Notizia spontanea dalla macchina (§8). |
| `notify` | S→C | 2 | Suggerimento dal server: "c'è qualcosa di nuovo, vai a vedere" (§9). |

Il discriminante dentro `command`/`event` è `data.name`, dentro `notify` è
`data.topic`. Si usa una famiglia con un nome dentro, invece di un `type` per
ogni caso d'uso, per una ragione precisa: un `type` sconosciuto viene
ignorato in silenzio (§2.5), mentre un comando sconosciuto deve essere
**rifiutato esplicitamente**. Tenere i comandi sotto un solo `type` rende
questa differenza un'unica riga di codice per lato.

### 5.1 `command` (S→C)

```json
{
  "type": "command",
  "id": "01J8ZQ6T3M6X9K2V7B4N1C5D8E",
  "ts": "2026-09-23T08:15:02.123Z",
  "data": {
    "name": "apps.list_upgradable",
    "args": {},
    "expires_at": "2026-09-23T08:25:02Z",
    "issued_by": "admin:f.fois"
  }
}
```

| Campo | Tipo | Obbl. | Note |
|---|---|---|---|
| `name` | string | sì | Verbo del catalogo §7. |
| `args` | object | no | Argomenti del verbo. Campo sconosciuto → `invalid_args` (qui la tolleranza non vale: un argomento ignorato è un comando eseguito diversamente da come l'operatore l'ha chiesto). |
| `expires_at` | string | sì | Oltre questo istante il client rifiuta con `expired`. Il client lo confronta col proprio orologio **solo** se la differenza fra `ts` del server e la propria ora è sotto 5 minuti; altrimenti si fida dell'ordine di arrivo (il server non invia comunque comandi scaduti). |
| `issued_by` | string | no | Chi ha lanciato il comando, per il log locale e l'Event Log. Solo informativo. |

### 5.2 `ack` (C→S)

Mandato **sempre**, entro 5s dalla ricezione, prima di iniziare l'esecuzione.

```json
{ "type": "ack", "id": "01J8ZQ6T9…", "reply_to": "01J8ZQ6T3M6X9K2V7B4N1C5D8E",
  "data": { "accepted": true } }
```

```json
{ "type": "ack", "id": "01J8ZQ6T9…", "reply_to": "01J8ZQ6T3M6X9K2V7B4N1C5D8E",
  "data": { "accepted": false, "error": { "code": "disabled_by_policy",
            "message": "machine.reboot is not enabled for this host" } } }
```

| Campo | Tipo | Obbl. | Note |
|---|---|---|---|
| `accepted` | bool | sì | `false` chiude il comando: non arriverà nessun `result`. |
| `error` | object | se `accepted=false` | `{code, message}`, codici in §10. |
| `duplicate` | bool | no | `true` se l'`id` era già stato visto (§5.5): il client non riesegue, ri-conferma. |

### 5.3 `result` (C→S)

```json
{
  "type": "result",
  "id": "01J8ZQ6V0…",
  "reply_to": "01J8ZQ6T3M6X9K2V7B4N1C5D8E",
  "data": {
    "status": "ok",
    "duration_ms": 8421,
    "payload": { }
  }
}
```

| Campo | Tipo | Obbl. | Note |
|---|---|---|---|
| `status` | string | sì | `ok` \| `error`. |
| `duration_ms` | int | no | Tempo di esecuzione lato client. |
| `payload` | object | se `ok` | Specifico del verbo (§7). |
| `error` | object | se `error` | `{code, message}`, codici in §10. |
| `truncated` | bool | no | Il payload è stato tagliato per stare sotto il limite di §11. |

Comandi che per natura non possono mandare un `result` (`service.restart`,
`machine.reboot`: il processo muore prima) si chiudono con un `event`
`service.started` che ne cita l'`id` in `completed_commands` (§8.6). Lato
server lo stato finale di quei comandi è "si è riconnesso entro X" / "non si è
riconnesso", non un fallimento di consegna.

### 5.4 `event` (C→S) e `notify` (S→C)

```json
{ "type": "event", "id": "01J8ZQ7…", "ts": "…",
  "data": { "name": "session.changed", "payload": { } } }
```

```json
{ "type": "notify", "id": "01J8ZQ8…", "ts": "…",
  "data": { "topic": "release.published", "payload": { } } }
```

Nessuna risposta è prevista per `event` e `notify`: sono fire-and-forget. Un
`name`/`topic` sconosciuto viene ignorato e loggato a Debug.

### 5.5 Idempotenza e riconsegna

Una connessione instabile può far riconsegnare lo stesso comando. Il client
tiene in memoria un anello degli ultimi **64** `id` di comando con il loro
esito; se ne rivede uno risponde `ack` con `duplicate: true` e, se il comando
era concluso, rimanda lo stesso `result` senza rieseguire. Per
`service.restart` e `machine.reboot` l'`id` viene anche scritto in
`state.json` **prima** di agire, perché deve sopravvivere al riavvio (§8.6).

## 6. Oggetti condivisi

Tipi riusati da più messaggi. I nomi (`UserSession`, `ManifestCheck`, …) sono
solo etichette di questo documento.

### 6.1 `UserSession`

```json
{ "user": "CORP\\m.rossi", "state": "disconnected",
  "disconnected_at": "2026-09-23T07:58:10Z" }
```

| Campo | Note |
|---|---|
| `user` | `DOMINIO\utente`, o `utente` su workgroup. Omesso = nessuno loggato. |
| `state` | `active-console` \| `active-rdp` \| `disconnected`. Stesso contratto dell'header `X-EMLy-LoggedUserState` (`machineinfo.SessionState`). Omesso esattamente quando `user` lo è. |
| `disconnected_at` | Solo per `disconnected`, e solo se Windows lo riporta. |

### 6.2 `ServerRef`

Da quale server è arrivata una risposta, così la dashboard distingue un
mirror di sito dall'API pubblica.

```json
{ "url": "http://emly.sede-mi.local/v2/updates/manifest", "role": "primary" }
```

`role`: `primary` (il `baseServer` del sito), `backup` (uno dei
`backupServer`), `default` (`defaultServer`, nessun sito corrisposto).

### 6.3 `ManifestCheck`

Esito di un controllo del manifest, per EMLy o per l'Updater. Usato dal
`result` dei due comandi `*.manifest.check` e da `event` `update.available`.

```json
{
  "target": "emly",
  "installed_version": "3.4.1",
  "channel": "stable",
  "available_version": "3.5.0",
  "update_available": true,
  "critical": false,
  "min_required_version": "3.0.0",
  "decision": "install_next_cycle",
  "source": { "url": "http://emly.sede-mi.local/v2/updates/manifest", "role": "primary" },
  "checked_at": "2026-09-23T08:15:04Z"
}
```

| Campo | Tipo | Note |
|---|---|---|
| `target` | string | `emly` \| `updater`. |
| `installed_version` | string | Per `emly` è `GUI_SEMVER`; **omesso** se EMLy non è installato (mai il sentinel `0.0.0`). Per `updater` è `version.Version`. |
| `channel` | string | `stable` \| `beta`. Solo `emly`. |
| `available_version` | string | Versione offerta dal manifest per quel canale. |
| `update_available` | bool | `available_version` > `installed_version` (semver). |
| `critical` | bool | `isCritical` del manifest, o `installed < min_required_version`. Solo `emly`. |
| `min_required_version` | string | Solo `emly`. |
| `decision` | string | Cosa farà la macchina, vedi sotto. |
| `pending` | object | Presente se c'è un update scaricato non ancora applicato: `{version, forced, downloaded_at}` (da `state.json`). |
| `source` | `ServerRef` | Server che ha risposto. Omesso se nessuno ha risposto. |
| `checked_at` | string | Ora locale della macchina. |
| `error` | object | `{code, message}` se il check è fallito (`sources_unreachable`, `manifest_invalid`, `not_found`). In quel caso i campi `available_*` sono omessi. |

Valori di `decision`:

| Valore | Significato |
|---|---|
| `up_to_date` | Niente da fare. |
| `install_next_cycle` | Verrà scaricato e installato al prossimo `Cycle`. |
| `waiting_for_emly_exit` | Update non critico, EMLy è aperto: si attende la chiusura. |
| `forced` | Critico: al prossimo ciclo EMLy verrà chiuso (con avviso WTS se abilitato). |
| `paused` | `control.updater.enabled = false` nel documento remoto. |
| `cooldown` | Solo `updater`: attesa dei 10 minuti fra due lanci del setup. |
| `gave_up` | Solo `updater`: raggiunto `selfupdate.MaxAttempts` per questa versione. |
| `refused_signature` | Solo `updater`: il setup non supera SHA256 o Authenticode. |
| `disabled` | Solo `updater`: `[selfUpdate] enabled = false`. |

### 6.4 `UpgradablePackage`

Una riga di `internal/winget.Package`, rinominata in `snake_case`.

```json
{ "name": "Mozilla Firefox", "id": "Mozilla.Firefox", "installed_version": "129.0",
  "available_version": "130.0.1", "source": "winget" }
```

### 6.5 `MachineInfo`

Payload di `result` di `machine.info` e di `event` `machine.info`. Contiene i
campi di `identity` (stessi nomi) più quello che non viaggia negli header.

```json
{
  "hwid": "4C4C4544-…", "hostname": "PC-MI-0042", "ad_domain": "corp.example.it",
  "serial": "5R3HJK2", "product": "Latitude 5540",
  "os_version": "Windows 11 24H2 Professional (Build 26100.4652)",
  "emly_version": "3.4.1", "updater_version": "1.9.0",
  "logged_user": { "user": "CORP\\m.rossi", "state": "active-console" },
  "boot_time": "2026-09-23T06:02:11Z",
  "uptime_seconds": 7851,
  "network": {
    "interfaces": [ { "name": "Ethernet", "mac": "A4:BB:6D:12:34:56",
                      "ipv4": ["172.16.96.42"], "ipv6": [] } ]
  },
  "site": { "dc": "DC-MI1", "matched_site": "DC-MI1", "server": { "url": "…", "role": "primary" } },
  "config": { "revision": 43, "fetched_at": "2026-09-23T08:00:00Z", "source": "remote" },
  "hardware": { "cpu": "Intel(R) Core(TM) i5-1345U", "cores": 10, "memory_mb": 16384,
                "system_drive": { "total_mb": 488000, "free_mb": 201334 } },
  "emly": { "installed": true, "running": true, "install_dir": "C:\\3gIT\\EMLy",
            "channel": "stable", "language": "it" },
  "pending_update": { "version": "3.5.0", "forced": false, "downloaded_at": "…" },
  "self_update": { "version": "1.9.1", "attempts": 1, "gave_up": false }
}
```

| Gruppo | Note |
|---|---|
| campi identity | Stessa semantica di `identity` v1 (§2.6). |
| `updater_version` | Presente anche se viaggia nello User-Agent: qui è la versione *dopo* un eventuale self-update avvenuto a connessione aperta, cosa che lo User-Agent dell'upgrade non può riflettere. |
| `site` | Esito di `beginCycle`: DC trovato, sito corrisposto (omesso se nessuno), server scelto. È l'evento 700 in forma strutturata. |
| `config` | Revisione del documento remoto in uso; `source`: `remote` \| `cache` \| `legacy`. |
| `hardware` | Raccolto una volta all'avvio, come `machineinfo.Collect()`, tranne `system_drive.free_mb` (letto al momento). |
| `emly`, `pending_update`, `self_update` | Da `config.ini` di EMLy e da `state.json`. `install_dir` è un percorso, ma in **uscita** dalla macchina: la regola 2 vieta percorsi nei comandi, non nei report. |

Qualsiasi sezione che il client non riesce a raccogliere si omette; non fa
fallire il messaggio.

## 7. Catalogo comandi (server → client)

Ogni comando: `ack` entro 5s; `result` entro il timeout indicato (misurato dal
server dall'`ack`), altrimenti il server lo segna `timeout`. Un solo comando
per `name` alla volta: un secondo `apps.list_upgradable` mentre il primo gira
riceve `ack` con `busy`.

| `name` | `args` | Esito | Timeout `result` | Rischio |
|---|---|---|---|---|
| `machine.info` | `sections?` | `result` `MachineInfo` | 30s | lettura |
| `emly.manifest.check` | — | `result` `ManifestCheck` | 60s | lettura |
| `updater.manifest.check` | — | `result` `ManifestCheck` | 60s | lettura |
| `apps.list_upgradable` | — | `result` `{packages}` | 180s | lettura |
| `service.restart` | — | `event` `service.started` | 120s | **distruttivo** |
| `machine.reboot` | `delay_seconds`, `when_user_active` | `event` `service.started` | `delay_seconds` + 15 min | **distruttivo** |

"Distruttivo" significa: autorizzazione rafforzata lato server (admin key +
conferma in dashboard, come `service_restart` nella spec agent-channel) **e**
vincolo lato client descritto in §12.

### 7.1 `machine.info`

```jsonc
// args
{ "sections": ["network", "hardware"] }   // opzionale; assente = tutto
```

`sections` ammessi: `identity`, `logged_user`, `network`, `site`, `config`,
`hardware`, `emly`, `update`. Valore sconosciuto → `invalid_args`.

`result.payload` = `MachineInfo` (§6.5), ridotto alle sezioni richieste.

### 7.2 `emly.manifest.check`

Check **a secco**: esegue la stessa risoluzione di `resolveTarget` (catena di
server del ciclo corrente, stessi header `X-EMLy-*`) e si ferma alla giuntura
prima di `Downloads.Ensure`. Non scarica, non installa, non tocca
`state.json`, non sveglia `RunLoop`. Proprio perché è di sola lettura e non
tocca `state.json`, l'updater lo esegue anche mentre un `Cycle` è già in
corso, invece di rispondere `busy`: `busy` (§10) è riservato allo stesso
comando già in esecuzione (un secondo `emly.manifest.check` mentre il primo
gira), non a un `Cycle` concorrente — non c'è stato condiviso da
serializzare fra i due.

Per chi vuole che la macchina *aggiorni adesso* il verbo giusto è un
`update_now` futuro, che sveglia il loop (spec agent-channel §7.3) — non questo.

`result.payload` = `ManifestCheck` (§6.3) con `target: "emly"`.

### 7.3 `updater.manifest.check`

Come §7.2 sul manifest dell'Updater (`/v2/updates/manifest/updater`, URL
derivato con `config.UpdaterManifestURL`). Valuta `selfupdate.Decide` per
riempire `decision`, ma **non** lancia il setup e non incrementa `attempts`.

`result.payload` = `ManifestCheck` con `target: "updater"`; `channel`,
`critical` e `min_required_version` omessi.

### 7.4 `apps.list_upgradable`

Esegue `winget.ListUpgradable` (Microsoft.WinGet.Client via PowerShell). Solo
lettura: niente viene mai aggiornato.

```json
{
  "status": "ok",
  "duration_ms": 8421,
  "payload": {
    "packages": [
      { "name": "Mozilla Firefox", "id": "Mozilla.Firefox",
        "installed_version": "129.0", "available_version": "130.0.1", "source": "winget" }
    ],
    "collected_at": "2026-09-23T08:15:10Z"
  }
}
```

Errori specifici: `winget_module_missing` (`winget.ErrModuleNotInstalled`),
`powershell_not_found`, `timeout`. Lista vuota = nessun aggiornamento, non un
errore. Oltre il limite di §11 il client taglia la lista e mette
`truncated: true`.

### 7.5 `service.restart`

```jsonc
// args: nessuno
```

Sequenza lato client:

1. `ack accepted:true`;
2. scrive l'`id` del comando in `state.json` (`pendingCommands`);
3. chiude il WebSocket con **1001** ("service restarting");
4. si riavvia tramite un processo figlio staccato che fa `stop` + `start`
   (stesso vincolo di `selfupdate.Launch`: mai attendere un processo che deve
   fermare il servizio stesso);
5. alla ripartenza, dopo l'handshake, manda `event` `service.started` con
   `reason: "command"` e l'`id` in `completed_commands`.

Rifiuti possibili: `busy` se un'installazione (EMLy o self-update) è in corso
— riavviare a metà setup lascerebbe un'installazione a metà — oppure se un
`service.restart`/`machine.reboot` è già stato accettato e non si è ancora
concluso: finché è pending, il client rifiuta ogni altro comando distruttivo
con `busy` e non avvia nessuna installazione (né EMLy né self-update), così
un riavvio in corso non viene interrotto a metà da un secondo comando o da
un ciclo che parte nel frattempo. Lo stato "pending" scade da solo se il
riavvio/servizio non torna entro un margine oltre il tempo atteso (per non
restare bloccati per sempre su un `shutdown /a` o un processo di restart
morto silenziosamente): la macchina torna quindi a rifiutare `busy` solo
finché serve, non oltre.

### 7.6 `machine.reboot`

```json
{ "delay_seconds": 300, "when_user_active": "warn" }
```

| Arg | Tipo | Default | Limiti | Note |
|---|---|---|---|---|
| `delay_seconds` | int | `300` | `0`–`3600` | Attesa prima del riavvio. |
| `when_user_active` | string | `warn` | `warn` \| `skip` | `warn`: pianifica lo spegnimento con `InitiateSystemShutdownEx` e lascia che sia Windows stesso a mostrare il proprio conto alla rovescia (nessun dialogo dell'updater, e quindi nessuna localizzazione lato client), poi riavvia. `skip`: se c'è un utente `active-console` o `active-rdp` rifiuta con `user_active` e non fa nulla. |

Con `delay_seconds < 60` e un utente attivo, `warn` alza comunque l'attesa a
60s: nessuno deve perdere lavoro senza almeno un minuto di preavviso. Non
c'è alcun avviso WTS costruito dall'updater (regola 2 resta rispettata
perché non c'è testo libero da mostrare): è il conto alla rovescia nativo di
Windows a farsi carico dell'avviso, e le applicazioni aperte vengono chiuse
senza salvare allo scadere del tempo, esattamente come qualunque altro
riavvio pianificato di Windows.

Sequenza: `ack`, `id` in `state.json`, `InitiateSystemShutdownEx` con reason
code "planned / application maintenance" (il conto alla rovescia lo mostra
Windows). Alla ripartenza del servizio, `event` `service.started` con
`reason: "boot"` e l'`id` in `completed_commands`. Un `ack` seguito da
nessun `service.started` entro il timeout è un riavvio non avvenuto (o una
macchina che non è tornata): il server lo segna `timeout`, non `failed`.

Rifiuti possibili: `busy` (installazione in corso, oppure un
`service.restart`/`machine.reboot` già pending — vedi §7.5), `user_active`
(con `skip`), `disabled_by_policy` (§12.3).

## 8. Catalogo eventi (client → server)

| `name` | Quando | Payload |
|---|---|---|
| `machine.info` | subito dopo `welcome`, poi solo se cambia `site`, `config.revision`, `network` o una versione | `MachineInfo` (§6.5) |
| `session.changed` | fine di ogni raffica di notifiche di sessione (dopo `sessionSettle`) | §8.1 |
| `update.available` | quando un `Cycle` trova una versione nuova, **una volta per versione** | `ManifestCheck` |
| `update.started` | subito prima di lanciare il setup (EMLy) o `selfupdate.Launch` | §8.3 |
| `update.applied` | installazione verificata (`VerifyInstalled` / versione al riavvio) | §8.4 |
| `update.failed` | tentativo fallito | §8.5 |
| `service.started` | dopo ogni handshake riuscito che segue un avvio del processo | §8.6 |

### 8.1 `session.changed`

Integra il `TODO(presence WS)` di `watchSessions`
(`internal/service/sessionwatch.go`, branch `feat/session-change-watcher`).

```json
{
  "type": "event",
  "id": "01J8ZQ9…",
  "ts": "2026-09-23T08:20:31.004Z",
  "data": {
    "name": "session.changed",
    "payload": {
      "events": ["remote-disconnect", "remote-connect", "unlock"],
      "session_id": 2,
      "session_user": "CORP\\m.rossi",
      "logged_user": { "user": "CORP\\m.rossi", "state": "active-rdp" },
      "changed": true,
      "at": "2026-09-23T08:20:29.450Z"
    }
  }
}
```

| Campo | Note |
|---|---|
| `events` | I `machineinfo.SessionChangeKind` della raffica, in ordine d'arrivo: `console-connect`, `console-disconnect`, `remote-connect`, `remote-disconnect`, `logon`, `logoff`, `lock`, `unlock`, `remote-control`, `create`, `terminate`. **Diventano contratto sul filo con questo documento**: il commento in `sessionchange.go` che li dichiara "not yet a wire contract" va aggiornato. |
| `session_id` | Sessione WTS dell'ultima notifica. |
| `session_user` | Account di quella sessione; omesso se non ne ha (schermata di login) o se è già chiusa. |
| `logged_user` | `UserSession` (§6.1): la risposta di `machineinfo.LoggedUser()` *dopo* la raffica, cioè ciò che gli header `X-EMLy-LoggedUser*` riporterebbero ora. |
| `changed` | `logged_user` diverso da quello dell'evento precedente. |
| `at` | Ora di ricezione dell'ultima notifica. |

Lato server: se `changed` è `true`, `internal/clientws.handleEvent` chiama
`updaterclient.UpdateLoggedUser`, che aggiorna `logged_user`,
`logged_user_state`, `logged_user_disconnected_at` della riga
`updater_clients` con la stessa semantica COALESCE/NULLIF di `upsert` (§2.6),
**senza toccare `last_seen_at`**: un evento di sessione dice chi c'è alla
macchina, non che la macchina ha fatto poll. Con `changed: false` (es. `lock`/`unlock` dello
stesso utente) l'evento è solo audit/telemetria. Questo supera la regola v1
"i campi mutevoli restano quelli dell'ultimo `identity`" (design presenza
§3.2): in v2 una connessione aperta **può** aggiornare l'utente loggato, ma
solo tramite questo evento.

Il client non manda l'evento se il canale è spento o non connesso: il
prossimo poll porta lo stesso valore negli header (regola 3). A differenza
degli altri eventi (`machine.info`, `update.*`, `service.started`), che
restano in un buffer in memoria da massimo 32 voci fino alla prossima
`welcome`, `session.changed` **non** viene messo in coda in quel buffer -
viene scartato: per quando il canale torna su è comunque superato da quello
che il prossimo poll porterebbe, quindi non vale lo spazio nel buffer che un
evento genuinamente ancora valido (un aggiornamento in corso, per esempio)
merita di più.

### 8.2 `update.available`

`payload` = `ManifestCheck` (§6.3). Emesso dal `Cycle` (per `emly`) e da
`selfUpdate` (per `updater`) la **prima volta** che una data
`available_version` supera `installed_version`, non a ogni ciclo: una
macchina che aspetta la chiusura di EMLy per tre giorni non manda 144 eventi
uguali. Il gate è in memoria; dopo un riavvio del servizio viene rimandato una
volta, il che è voluto (dice al server che la macchina è ancora indietro).

Non va confuso con `notify` `release.published` (§9.1): quello è il server
che dice "è uscita una release", questo è la macchina che dice "l'ho vista e
questa è la mia decisione".

### 8.3 `update.started`

```json
{ "target": "emly", "from_version": "3.4.1", "to_version": "3.5.0",
  "forced": false, "attempt": 1, "trigger": "cycle" }
```

`trigger`: `cycle` (poll normale), `resume` (pending ripreso da `state.json`),
`notify` (ciclo svegliato da `release.published`), `command` (futuro
`update_now`, con `command_id`). `from_version` omesso su prima installazione.

Per `target: "updater"` è l'ultimo messaggio che la vecchia build riesce a
mandare: subito dopo `selfupdate.Launch` il setup ferma il servizio.

### 8.4 `update.applied`

```json
{ "target": "emly", "from_version": "3.4.1", "to_version": "3.5.0",
  "forced": false, "attempt": 1, "duration_ms": 48210, "reinstalled": false }
```

| Campo | Note |
|---|---|
| `reinstalled` | Solo `emly`: `true` se il primo setup non ha lasciato `GUI_SEMVER` giusto ed è servito `installer.Uninstall` + secondo tentativo. |
| `duration_ms` | Solo `emly`. Per `updater` si omette: la durata attraversa un riavvio e non è misurabile in modo affidabile. |

Per `target: "updater"` l'evento lo manda la **nuova** build, dopo il
riavvio, quando `selfupdate.Reconcile` conferma che `version.Version` è
quella tentata — cioè nello stesso momento dell'evento 801 dell'Event Log.
Corrispondenze con l'Event Log: `update.applied` emly = 200, updater = 801.

### 8.5 `update.failed`

```json
{ "target": "emly", "from_version": "3.4.1", "to_version": "3.5.0",
  "attempt": 1, "will_retry": true,
  "error": { "code": "version_mismatch", "message": "config.ini reports 3.4.1 after setup" } }
```

Codici tipici: `download_failed`, `checksum_mismatch`, `signature_invalid`,
`setup_exit_code`, `version_mismatch`, `emly_kill_failed`, `gave_up`.
`will_retry` dice se la macchina ritenterà da sola al prossimo ciclo (pending
in `state.json` / tentativi self-update rimasti). Event Log: 201 (emly), 802
(updater).

### 8.6 `service.started`

```json
{
  "reason": "boot",
  "boot_time": "2026-09-23T08:31:02Z",
  "started_at": "2026-09-23T08:31:40Z",
  "updater_version": "1.9.0",
  "previous_version": "1.8.2",
  "completed_commands": ["01J8ZQ6T3M6X9K2V7B4N1C5D8E"]
}
```

| Campo | Note |
|---|---|
| `reason` | `boot` (servizio partito entro pochi minuti dal boot), `command` (riavvio da `service.restart`), `self_update` (la build è cambiata rispetto a `previous_version`), `install` (prima installazione), `unknown`. Best-effort: serve alla dashboard, non a decidere nulla. |
| `previous_version` | Solo se diversa da `updater_version`. |
| `completed_commands` | `id` di `service.restart`/`machine.reboot` letti da `state.json` e poi rimossi. Il server chiude quei comandi come `done`. |

Mandato **una sola volta per vita del processo**, alla prima connessione
riuscita, non a ogni riconnessione: una rete instabile non deve sembrare un
servizio che si riavvia.

## 9. Catalogo notify (server → client)

Solo suggerimenti (regola 1). Il client non si fida del contenuto: lo usa per
decidere *quando* rifare un GET REST, mai *cosa* installare.

### 9.1 `release.published`

```json
{ "topic": "release.published",
  "payload": { "target": "emly", "channel": "stable", "version": "3.5.0",
               "jitter_seconds": 600 } }
```

Il client, se `target`/`channel` lo riguardano, **aspetta un tempo casuale in
`[0, jitter_seconds]`** e poi sveglia `RunLoop` (R-LOOP): il ciclo normale
rilegge il manifest, verifica SHA256 (e Authenticode per `updater`) e decide
come sempre. `jitter_seconds` è obbligatorio e il client applica comunque un
minimo di 60s: 400 macchine che scaricano lo stesso setup nello stesso
secondo sono un DoS del mirror di sito. Il server può mandarlo per singola
macchina, per sito o a tutta la flotta; lo scaglionamento fine resta
responsabilità del server (spec agent-channel §7.2).

Un `release.published` per una versione non più nuova di quella installata
viene ignorato.

### 9.2 `config.published`

```json
{ "topic": "config.published", "payload": { "revision": 44, "jitter_seconds": 120 } }
```

Se `revision` è maggiore di quella in uso, dopo il jitter il client rifà
`GET /v2/config` e lo valida all-or-nothing come sempre. È anche il modo in
cui `clientWs.enabled: false` arriva in secondi invece che al prossimo poll
(oggi `clientws.go` rilegge il ciclo ogni 15s: con questo notify può farlo
subito). Come `release.published`, `jitter_seconds` ha un minimo lato client,
più basso di quello di §9.1 (il documento è già in cache e solo stantio, non
mancante del tutto): **30s**.

Un notify che forza il refresh (`revision` più recente) marca comunque il
prossimo poll come "fetch obbligatorio" anche quando il risveglio anticipato
sotto è scartato dalla regola successiva - solo quel risveglio è filtrato,
non l'effetto del notify sul poll che segue.

Sui due topic insieme vale inoltre un limite lato client: **al massimo un
risveglio anticipato di `RunLoop` ogni 10 minuti**. Un `release.published` o
un `config.published` che arriva mentre la finestra è ancora aperta non
sveglia `RunLoop` una seconda volta - il poll ordinario resta comunque
attivo e recupera al giro successivo - così una raffica di notify (più siti,
un publisher che ritenta) non genera una raffica di cicli anticipati.

## 10. Codici di errore

Usati in `error.code` di `error`, `ack`, `result` e degli eventi `update.*`.

| Codice | Dove | Significato |
|---|---|---|
| `unidentified` | `error` | (v1) identity mancante o senza hwid/hostname. |
| `protocol_error` | `error` | Envelope non valido, JSON rotto, `id` mancante su un messaggio v2 che lo richiede. |
| `message_too_large` | `error` | Oltre `limits.max_message_bytes`. |
| `rate_limited` | `error` | Oltre `limits.max_events_per_minute`. |
| `unsupported_command` | `ack` | `name` sconosciuto a questa build. |
| `invalid_args` | `ack` | Argomento mancante, fuori limite, o sconosciuto. |
| `expired` | `ack` | Ricevuto dopo `expires_at`. |
| `busy` | `ack` | Stesso comando (stesso `name`) già in esecuzione, o un'installazione (EMLy/self-update) in corso per `service.restart`/`machine.reboot`. **Non** per un `Cycle` concorrente: `emly.manifest.check`/`updater.manifest.check` sono a secco e girano comunque (§7.2, §7.3). |
| `disabled_by_policy` | `ack` | Il documento remoto non abilita questo comando per questa macchina (§12.3). |
| `insecure_transport` | `ack` | Comando distruttivo ricevuto su `ws://` (§12.2). |
| `user_active` | `ack` | `machine.reboot` con `when_user_active: "skip"` e un utente attivo. |
| `winget_module_missing` | `result` | Modulo Microsoft.WinGet.Client assente. |
| `powershell_not_found` | `result` | `powershell.exe` non nel PATH. |
| `sources_unreachable` | `result`, `ManifestCheck.error` | Nessun server della catena ha risposto (evento 101). |
| `not_found` | `ManifestCheck.error` | 404: il server non pubblica quel manifest (mirror non aggiornato). |
| `manifest_invalid` | `ManifestCheck.error` | JSON del manifest non valido. |
| `timeout` | `result` | Operazione interna scaduta (es. PowerShell). |
| `internal` | ovunque | Errore imprevisto; il dettaglio è in `message` e nel log locale. |

`message` è testo per i log e la dashboard, in inglese, mai interpretato da
codice. Chi reagisce a un errore legge solo `code`.

## 11. Limiti, timeout, chiusura

Valori di default, comunicati dal server in `welcome.data.limits`:

```json
{ "max_message_bytes": 65536, "max_events_per_minute": 60, "ack_timeout_seconds": 5 }
```

- **Dimensione**: 64 KiB per messaggio, in entrambe le direzioni. Lato
  server questo è `websocket.Conn.SetReadLimit` (`internal/clientws`): la
  libreria (`coder/websocket`) chiude direttamente con 1009
  (`StatusMessageTooBig`) appena un singolo messaggio in arrivo lo supera,
  **senza** mandare prima un frame `error` — a quel punto la libreria ha già
  deciso di chiudere il socket, non c'è più occasione di scriverci sopra.
  `message_too_large` in tabella (§10) resta quindi un codice del
  catalogo per il lato client (un client che decide da sé di segnalarlo)
  piuttosto che qualcosa che questa implementazione emette come frame prima
  del close. Il client, per i `result` che possono crescere
  (`apps.list_upgradable`, `machine.info`), taglia e mette
  `truncated: true` invece di superarlo.
- **Frequenza**: 60 eventi/minuto per connessione (i `pong`, `ack` e `result`
  non contano). Oltre: `error` `rate_limited` + close 1008. Una raffica di
  `session.changed` è già coalescita da `sessionSettle`, quindi il limite non
  la tocca in uso normale.
- **Heartbeat**: invariato dalla v1, ping ogni 10s dal server, connessione
  morta dopo 20s senza nulla; il client considera morto il server dopo 30s.
- **Handshake**: `identity` entro 10s da `hello` (v1). In v2 il client
  aspetta `welcome` fino a 10s dopo `identity`; se non arriva continua in
  modalità v1 (server vecchio che non conosce `welcome`) e non esegue comandi.

Codici di chiusura:

| Codice | Chi | Significato | Reazione del client |
|---|---|---|---|
| 1000 | entrambi | Chiusura normale (stop servizio, `clientWs.enabled=false`) | nessuna / backoff normale |
| 1001 | client | Servizio in riavvio o PC in riavvio (§7.5, §7.6) | — |
| 1008 | server | Policy: `unidentified`, `rate_limited`, "superseded by newer connection" | backoff normale |
| 1009 | server | `message_too_large` | backoff normale; è un bug del client, va loggato a Warn |
| 1011 | server | Errore interno | backoff normale |
| 1012 | server | Riavvio del server | riconnessione con jitter pieno |

## 12. Sicurezza

### 12.1 Autenticazione della connessione

Invariata dalla v1: `X-Api-Key` sull'upgrade, chiave condivisa dalla flotta,
più `X-EMLy-HWID`/`X-EMLy-Hostname` per il ban list. È sufficiente per i
comandi di **lettura** e per gli eventi: il danno massimo di chi possiede la
chiave è falsificare l'inventario di una macchina, cosa che può già fare con
gli header del manifest.

### 12.2 Comandi distruttivi e trasporto

**Non è sufficiente per `service.restart` e `machine.reboot`.** Le sorgenti
interne sono spesso HTTP in chiaro (i mirror di sito, cfr. `AGENTS.md`
"The internal source is plain HTTP"), quindi la presenza WS verso un mirror è
`ws://`: chiunque sia in grado di fare man-in-the-middle su quella rete, o di
rispondere al posto del mirror, potrebbe mandare `machine.reboot` a ogni
macchina del sito. Per questo:

- il client accetta un comando distruttivo **solo** su una connessione
  `wss://` il cui certificato valida normalmente; su `ws://` risponde `ack`
  con `insecure_transport` e non fa nulla;
- in alternativa, se servirà comandare macchine raggiungibili solo via mirror
  HTTP, i comandi distruttivi andranno **firmati** dall'API (Ed25519, chiave
  pubblica compilata nell'updater come oggi il certificato 3gIT, firma su
  `id` + `name` + `args` + `expires_at` canonicalizzati). È un'estensione del
  formato (`data.sig`) da progettare prima di abilitare quei comandi fuori da
  `wss://`, non un dettaglio da aggiungere dopo.

Questo vale anche per ogni comando futuro che modifichi lo stato della
macchina (`update_now`, `pause`): la regola è "lettura ovunque, scrittura solo
su canale autenticato".

### 12.3 Policy per macchina

Oltre all'autorizzazione lato server (chi può lanciare cosa, spec
agent-channel §4.5), il client applica un'allowlist dal documento remoto:

```jsonc
"clientWs": {
  "enabled": true,
  "commands": ["machine.info", "emly.manifest.check",
               "updater.manifest.check", "apps.list_upgradable"]
}
```

- Default, assente o legacy: solo i comandi di **lettura**. I due distruttivi
  vanno abilitati esplicitamente, e si possono pilotare su pochi host con un
  override `match: {hostnames: [...]}`, come `clientWs.enabled` oggi.
- `"commands": []` (lista **presente** ma vuota) è diverso da `commands`
  assente: significa esplicitamente "nessun comando", non "default del
  client" (i comandi di sola lettura). Sul wire, `Commands` è un puntatore a
  slice (`*[]string`, `internal/remoteconfig.ClientWS`) proprio per poter
  distinguere i due casi — una `[]string` con `omitempty` marshalla sia nil
  che `[]string{}` come "assente", che avrebbe reso impossibile spegnere
  tutti i comandi mantenendo `clientWs.enabled: true` per la sola
  telemetria/`notify`.
- **In questa implementazione il server non filtra `accepted_capabilities`
  con `clientWs.commands`.** `welcome.accepted_capabilities` è la sola
  intersezione fra le capability dichiarate dal client (`identity.capabilities`)
  e quelle che il server conosce (`clientproto.ServerCapabilities`) —
  `clientproto.Intersect`, in `internal/clientws.ClientWS`. Il motivo è che, a
  questo punto della connessione, il server conosce la macchina solo da
  `hwid`/`hostname` (l'`identity`) e non può valutare gli override
  `match: {dcs: […]}`/`match: {subnets: […]}` della policy da lì: quel
  matching di sito è lavoro dell'Updater (`beginCycle`), non dell'API.
  L'allowlist è quindi applicata **solo dal client**: un comando il cui `name`
  non è in `clientWs.commands` viene rifiutato con `ack` `disabled_by_policy`
  (§10), anche se il server lo aveva proposto in `accepted_capabilities` e
  anche se l'ha effettivamente inviato — regola 1 (il canale non porta
  autorità) vale anche al contrario, il client non si fida di ciò che il
  server propone.
- Aggiungere `commands` alla sezione `clientWs` segue la procedura di
  `AGENTS.md` dell'updater per un campo nuovo del documento: `document.go`,
  `parse.go`, `legacy.go` e le fixture condivise in `testdata/remoteconfig/`
  **in entrambi i repo**.
- **Rischio di rollout**: un mirror di sito rimasto su una build precedente
  a `clientWs.commands` ri-canonicalizza il documento con il vecchio tipo
  `ToggleOnly` (solo `{enabled}`) invece dell'attuale `ClientWS`
  (`{enabled, commands}`) — il campo `commands` viene silenziosamente
  perso alla ri-canonicalizzazione, l'ETag calcolato da quel mirror non
  corrisponde più a quello upstream, e `internal/configmirror` rifiuta il
  documento per intero ("hash mismatch"), non solo la sezione `clientWs`:
  quel sito smette di sincronizzare **tutta** la configurazione, non solo i
  comandi. Regola operativa: aggiornare ogni mirror di sito **prima** di
  pubblicare un documento che imposta `clientWs.commands` (assente o solo
  `enabled` continua a canonicalizzare com'è sempre stato, vedi
  `TestCanonical_ClientWSWithoutCommandsUnchanged`, quindi non è a rischio).
  Il seguito naturale è far hashare al mirror i byte grezzi ricevuti invece
  di ri-canonicalizzarli lui stesso (coerente con "inserisce il documento
  upstream verbatim" che già fa oggi per il contenuto, non ancora per la
  verifica dell'ETag) — non fatto in questo giro.

### 12.4 Isolamento

Una connessione rappresenta un solo `updater_clients.id`, deciso dal server
dall'`identity`. Nessun campo di un messaggio sceglie un destinatario o legge
lo stato di un'altra macchina. Gli `event` aggiornano solo la riga del client
della connessione su cui arrivano.

## 13. Superficie REST (admin)

Il canale WebSocket non è l'unico modo di toccare il protocollo: quattro
route `X-Admin-Key` (`internal/clientws/admin.route.go`, montate da
`RegisterV2` sotto `/v2/client`) sono quello che la dashboard chiama per
lanciare un comando su una macchina connessa, leggerne l'esito, vedere i suoi
ultimi eventi o mandare un `notify` senza passare da `POST /v2/config`. Sono
un front-end HTTP allo stato in memoria di `internal/clienthub` (§2, "in
memoria, a istanza singola, perso a un riavvio" — vale anche qui, non solo
per le sessioni WS: uno storico di comandi/eventi non sopravvive a un
riavvio dell'API e un comando lanciato su una macchina connessa a un replica
non è visibile dall'altra). Il dettaglio dei corpi/codici è anche in
[`ROUTES.md`](ROUTES.md) §5.9; questa tabella è la vista rapida.

| Metodo | Path | Corpo | Risposta | Codici |
|---|---|---|---|---|
| `POST` | `/v2/client/{client_id}/commands` | `{"name", "args"?, "ttl_seconds"?, "issued_by"?}` | `CommandRecord` (sotto) | `202` inviato; `400` `client_id`, JSON, `args` o `ttl_seconds` non validi; `409` macchina non connessa; `422` `name` sconosciuto, o non nella lista `capabilities` che questa connessione ha dichiarato (v1 compreso: sempre `422`, mai `409`, così l'admin vede il motivo) |
| `GET` | `/v2/client/commands/{command_id}` | — | `CommandRecord` | `200`; `404` `command_id` sconosciuto |
| `GET` | `/v2/client/{client_id}/events` | — | `{"events": [EventRecord, …]}` | `200` (lista vuota se non ci sono eventi, mai `404`) |
| `POST` | `/v2/client/notify` | `{"topic", "payload", "client_ids"?}` (§9) | `{"sent": N}` | `200`; `400` `topic`/`payload` non validi (stessa validazione di §9) |

`ttl_seconds` (default `600`, massimo `86400`) è quanto l'admin è disposto ad
aspettare prima che il comando scada (`expires_at` di §5.1); non è il timeout
di `ack`/`result` di §7, che resta quello del catalogo. `client_ids` assente
su `notify` significa "tutte le sessioni v2 che dichiarano quel `topic`",
come `Hub.Notify` (§9 nota sotto).

`CommandRecord` (`internal/clienthub.CommandRecord`, la stessa forma per
`POST .../commands` e `GET .../commands/{id}`):

```json
{
  "id": "01J8ZQ6T3M6X9K2V7B4N1C5D8E",
  "client_id": 42,
  "name": "apps.list_upgradable",
  "issued_by": "admin:f.fois",
  "issued_at": "2026-09-23T08:15:02Z",
  "expires_at": "2026-09-23T08:25:02Z",
  "status": "sent"
}
```

`args`, `issued_by`, `acked_at`, `finished_at`, `error` e `result` sono tutti
`omitempty` sul tipo Go: appaiono solo una volta impostati (`args` se il
comando ne prevede, `acked_at` dopo l'`ack`, e così via) e sono **assenti**,
non `null`, fino a quel momento — l'esempio sopra è la forma reale di un
comando appena accettato (`202`, `status: "sent"`), non un placeholder con
tutti i campi elencati a `null`.

`status`: `sent` → `acked` → `done` | `failed` | `rejected` | `timeout`
(§5.2/§5.3, applicato lato server con lo stesso significato). `rejected` è
un `ack` con `accepted: false`; `timeout` è applicato pigramente alla
lettura (`Hub.Command`/`Hub.Prune`), non da un timer in background — un
record letto tra la scadenza e il prossimo `Prune` la riflette comunque.
Un comando `POST` risposto `202` (`sent`) è sempre presente da subito in un
successivo `GET /v2/client/commands/{id}`: è registrato nel hub **prima**
dell'invio sul socket, apposta perché un `ack` che corre più veloce della
risposta HTTP non trovi il record assente.

`config.published` è annunciato in modo **asincrono**: le route di
`POST /v2/config/revisions`, `.../publish` e `.../rollback`
(`internal/configapi`) chiamano `Hub.NotifyConfigPublished` come loro
`ConfigNotifier`, che fa il fan-out in una goroutine propria — la risposta
HTTP di quelle route non aspetta un solo socket client. `Hub.Notify` (usato
anche da questa `POST /v2/client/notify`) manda a ogni destinatario in
goroutine separate, il tutto delimitato da un singolo timeout di invio
(10s) e non da (timeout) × (numero di destinatari): un socket bloccato non
fa aspettare gli altri né il chiamante HTTP. `POST /v2/client/notify` resta
comunque sincrono con il proprio fan-out (a differenza di
`config.published`) e la sua risposta riporta quanti destinatari sono stati
effettivamente raggiunti (`sent`).

Ogni evento ricevuto (`type: "event"`, §5.4) viene registrato nell'anello
per-client di `internal/clienthub` (ultimi 50, `GET .../events`) **prima**
che i suoi effetti (aggiornare `logged_user*`, chiudere un comando via
`service.started`) vengano applicati — così un evento non interpretabile
resta comunque visibile nella cronologia. Comandi ed eventi sono pruned da
un ticker ogni 10 minuti: i comandi conclusi (`done`/`failed`/`rejected`/
`timeout`) più vecchi di 24h vengono rimossi, così come gli eventi di un
client senza nulla di più recente di 24h.

Cosa viene effettivamente conservato del `payload` di un evento è limitato
su due assi indipendenti, perché chiunque abbia la fleet API key può aprire
una sessione per hwid e mandare eventi con qualsiasi `name` — senza limiti
sarebbero fino a 50 × 64 KiB di payload grezzo per `client_id`, tenuti fino
a 24h:

- il `payload` è conservato solo se il `name` dell'evento è fra le
  `capabilities` che quella sessione ha dichiarato in `identity` ed è
  finito in `welcome.accepted_capabilities` — un `name` non dichiarato è
  registrato comunque (per la cronologia), ma senza `payload`, come un
  `name` sconosciuto;
- anche un `payload` accettato è scartato oltre 8 KiB, e l'`EventRecord`
  riporta `truncated: true` per distinguere "payload troppo grande" da
  "`name` non dichiarato" (che non è mai `truncated`).

`internal/clienthub` limita anche quanti `client_id` distinti l'anello degli
eventi può tenere contemporaneamente (5000): oltre quella soglia, registrare
un evento per un `client_id` mai visto prima fa sfrattare il `client_id` meno
di recente attivo (quello il cui evento più recente è il più vecchio fra
tutti) — un limite su quante *identità* si possono accumulare, non solo su
quanti byte per identità, perché anche un evento senza `payload` costa una
voce nella mappa.

## 14. Esempi di sequenza completi

### 14.1 Handshake v2 e snapshot iniziale

```
S→C {"type":"hello","data":{"protocol":2,"server_version":"2.14.0","server_time":"2026-09-23T08:00:00Z"}}
C→S {"type":"identity","id":"01J…A","data":{"hwid":"…","hostname":"PC-MI-0042", …,"protocol":2,"capabilities":[…]}}
S→C {"type":"welcome","id":"01J…B","data":{"protocol":2,"accepted_capabilities":[…],"limits":{"max_message_bytes":65536,"max_events_per_minute":60,"ack_timeout_seconds":5}}}
C→S {"type":"event","id":"01J…C","data":{"name":"service.started","payload":{"reason":"boot", …}}}
C→S {"type":"event","id":"01J…D","data":{"name":"machine.info","payload":{ …MachineInfo… }}}
S→C {"type":"ping"}
C→S {"type":"pong"}
```

### 14.2 Lista aggiornamenti winget

```
S→C {"type":"command","id":"01J…X","data":{"name":"apps.list_upgradable","args":{},"expires_at":"…"}}
C→S {"type":"ack","id":"01J…Y","reply_to":"01J…X","data":{"accepted":true}}
      … ~8s di PowerShell …
C→S {"type":"result","id":"01J…Z","reply_to":"01J…X","data":{"status":"ok","duration_ms":8421,"payload":{"packages":[…],"collected_at":"…"}}}
```

### 14.3 Riavvio PC

```
S→C {"type":"command","id":"01J…R","data":{"name":"machine.reboot","args":{"delay_seconds":300,"when_user_active":"warn"},"expires_at":"…"}}
C→S {"type":"ack","id":"01J…S","reply_to":"01J…R","data":{"accepted":true}}
      (client: id in state.json, InitiateSystemShutdownEx con 300s di preavviso
       (conto alla rovescia nativo di Windows, nessun avviso dell'updater), reboot)
      … la macchina riparte, il servizio si riconnette …
S→C {"type":"hello","data":{"protocol":2, …}}
C→S {"type":"identity", …}
S→C {"type":"welcome", …}
C→S {"type":"event","id":"01J…T","data":{"name":"service.started","payload":{"reason":"boot","completed_commands":["01J…R"], …}}}
```

### 14.4 Nuova release EMLy annunciata e applicata

```
S→C {"type":"notify","id":"01J…N","data":{"topic":"release.published","payload":{"target":"emly","channel":"stable","version":"3.5.0","jitter_seconds":600}}}
      (client: attesa casuale 0–600s, poi sveglia RunLoop)
C→S {"type":"event","data":{"name":"update.available","payload":{"target":"emly","installed_version":"3.4.1","available_version":"3.5.0","decision":"install_next_cycle", …}}}
C→S {"type":"event","data":{"name":"update.started","payload":{"target":"emly","from_version":"3.4.1","to_version":"3.5.0","attempt":1,"trigger":"notify"}}}
C→S {"type":"event","data":{"name":"update.applied","payload":{"target":"emly","from_version":"3.4.1","to_version":"3.5.0","duration_ms":48210,"reinstalled":false}}}
```

### 14.5 Cambio sessione (riconnessione RDP)

```
      (SCM: remote-disconnect, remote-connect, unlock — coalescite in 1.5s)
C→S {"type":"event","data":{"name":"session.changed","payload":{"events":["remote-disconnect","remote-connect","unlock"],"session_id":2,"logged_user":{"user":"CORP\\m.rossi","state":"active-rdp"},"changed":true, …}}}
```

## 15. Checklist di implementazione

**API (`emly-go-api`)**

- [x] `internal/clientws`: envelope con `id`/`reply_to`/`ts`; `hello.data`,
      `welcome`; ramo v1/v2 per connessione in base a `identity.protocol`.
- [x] Parsing di `event` (`session.changed` → aggiornamento `logged_user*`
      senza toccare `last_seen_at`; gli altri → registrati nell'anello di
      `internal/clienthub` e loggati, `service.started` chiude i comandi
      `ResultViaEvent` via `completed_commands`).
- [x] Registro comandi in memoria per connessione (`id` → stato, timeout di
      `ack`/`result`) — `internal/clienthub.Hub` (`Issue`/`HandleAck`/
      `HandleResult`/`applyTimeout`).
- [ ] Persistenza dei comandi/eventi come da spec agent-channel §8: in questa
      implementazione restano solo in memoria (`internal/clienthub`, pruned
      dopo 24h) — nessuna tabella, nessuno storico che sopravviva a un
      riavvio dell'API (§13).
- [x] Limiti §11 (dimensione messaggio, eventi/minuto) e i codici di
      chiusura 1000/1008/1009/1011.
- [ ] Codice di chiusura 1012 (riavvio lato server): lo shutdown di
      `main.go` non chiude esplicitamente le connessioni `/v2/client/ws` con
      quel codice, si affida al drop della connessione.
- [x] `clientWs.commands` in `internal/remoteconfig` + fixture condivise
      (`testdata/remoteconfig/invalid/clientws-unknown-command*`,
      `override-clientws-unknown-command*`).
- [x] `ROUTES.md` §5.9 e `DOCS.md` aggiornati (questo giro).

**Updater (`emly-updater`)**

- [x] `internal/wsclient`: dispatch per `type` + `name`, anello di 64 `id`
      (`commandRing`, `internal/service/clientcmd.go`), `ack` entro 5s,
      `welcome` opzionale con fallback v1.
- [x] `watchSessions`: sostituito il `TODO(presence WS)` con `session.changed`
      (`internal/service/sessionwatch.go`).
- [x] `internal/winget` dietro `apps.list_upgradable` (merge di
      `feat/winget-upgradable`).
- [x] `emly.manifest.check` / `updater.manifest.check` come check a secco: di
      sola lettura, non tocca `state.json` né la preferenza di server, quindi
      **può** girare anche mentre un `Cycle` è già in corso invece di
      rispondere `busy` (§7.2) - `busy` resta per lo stesso comando già in
      esecuzione, non per un `Cycle` concorrente
      (`resolveTargetWith`/`resolveUpdaterManifestWith` con
      `notePreferred=false`, `internal/service/clientcmd.go`).
- [x] `release.published` / `config.published` come trigger con jitter sulla
      `select` di `RunLoop` (`internal/service/clientnotify.go`, canale
      `u.wake` a uno slot).
- [x] `state.json`: `pendingCommands` per `service.restart`/`machine.reboot`
      (`internal/state/state.go`), scritto prima di agire e ripulito da
      `service.started`.
- [x] Vincolo `wss://` per i comandi distruttivi (§12.2) —
      `commandSession.Secure()` in `admitCommand`, `internal/service/clientcmd.go`.
- [x] `AGENTS.md`: convenzione "i `SessionChangeKind` sono contratto sul filo",
      nuovi event ID per comandi ricevuti/eseguiti/rifiutati (924/925/926).
