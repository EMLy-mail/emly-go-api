# Coda download installer — spec per la Dashboard

Spec per il team della dashboard — 2026-09-30

Controparte lato API: `internal/downloadqueue` in `emly-go-api`, route
documentate in `ROUTES.md` §5.10. Controparte Updater:
`2026-09-30-download-queue-updater-spec.md`.

---

## 1. Cosa fa la coda

L'API limita quanti installer (EMLy **e** Updater, un unico pool) possono
essere scaricati **contemporaneamente**. Default da `.env`: attiva, 50 slot,
`Retry-After` 60s. Quando è piena, un nuovo download riceve `429` con
`Retry-After` e l'Updater riprova più tardi: non c'è una fila d'attesa vera,
solo slot occupati/liberi.

Punti da mostrare chiaramente in UI:

- **Stato solo in RAM.** Le modifiche fatte dalla dashboard valgono fino al
  prossimo riavvio dell'API, poi si torna ai valori di `.env` (esposti in
  `defaults`). I contatori `*_total` ripartono da zero a ogni riavvio.
- **Per istanza.** Con più repliche dell'API ognuna ha la propria coda; la
  dashboard vede solo quella dell'istanza che risponde.
- **Da disattivata** la coda traccia comunque i download in corso (`slots`),
  ma non rifiuta nessuno.

## 2. Autenticazione

Tutte le route richiedono **entrambi** gli header, dal server della dashboard:

```
X-Admin-Key: <ADMIN_KEY>
X-Dashboard-Key: <DASHBOARD_KEY>
```

Manca uno dei due (o `DASHBOARD_KEY` non è configurata sull'API) → `401`.
Le chiavi non devono mai arrivare al browser: chiamate solo lato server.
Il `X-Session-Token` dell'utente, se inoltrato, viene usato per attribuire
l'azione nel log dell'API (opzionale ma consigliato).

## 3. Endpoint

Base: `/v2/download-queue`

| Metodo | Path | Uso in UI |
|--------|------|-----------|
| `GET` | `/v2/download-queue` | Stato completo (polling) |
| `PATCH` | `/v2/download-queue` | Attiva/spegni, espandi/riduci slot, cambia Retry-After |
| `POST` | `/v2/download-queue/reset` | "Ripristina valori .env" |
| `DELETE` | `/v2/download-queue/slots/{id}` | Interrompi un download |
| `DELETE` | `/v2/download-queue/slots` | Interrompi **tutti** i download (svuota) |

### 3.1 `GET /v2/download-queue`

```json
{
  "enabled": true,
  "capacity": 50,
  "retry_after_seconds": 60,
  "active": 2,
  "available": 48,
  "acquired_total": 1234,
  "rejected_total": 17,
  "evicted_total": 0,
  "defaults": { "enabled": true, "capacity": 50, "retry_after_seconds": 60 },
  "slots": [
    {
      "id": 1233,
      "product": "emly",
      "version": "1.7.0",
      "ip": "203.0.113.7",
      "hostname": "pc-reception",
      "hwid": "ABC123",
      "started_at": "2026-09-30T08:12:44Z"
    }
  ]
}
```

| Campo | Note |
|-------|------|
| `enabled`, `capacity`, `retry_after_seconds` | Impostazioni correnti |
| `active` | Download in corso adesso |
| `available` | `capacity - active`, minimo 0. Può essere 0 con `active > capacity` se la capacità è stata ridotta |
| `acquired_total` | Download che hanno preso uno slot dall'avvio |
| `rejected_total` | Download rifiutati con `429` dall'avvio |
| `evicted_total` | Download interrotti da un admin dall'avvio |
| `defaults` | Valori di `.env`; se diversi da quelli correnti, mostrare un badge "modificato" |
| `slots[].product` | `"emly"` o `"updater"` |
| `slots[].ip/hostname/hwid` | Possono mancare (omessi se vuoti) |
| `slots` | Ordinati dal più vecchio; sempre un array (mai `null`) |

Polling consigliato: ogni **5 s** sulla pagina della coda, niente polling
altrove. Le route della dashboard sono esenti dal rate limit con il
`X-Dashboard-Key`.

### 3.2 `PATCH /v2/download-queue`

Corpo: solo i campi da cambiare.

```json
{ "enabled": false }
{ "capacity": 80 }
{ "retry_after_seconds": 120 }
{ "enabled": true, "capacity": 80, "retry_after_seconds": 120 }
```

| Campo | Vincoli |
|-------|---------|
| `enabled` | booleano |
| `capacity` | intero 1–10000 |
| `retry_after_seconds` | intero 1–86400 |

Risposta `200` con lo stesso oggetto di §3.1 (usarlo per aggiornare la UI
senza un GET in più). `400` con `{"error": "..."}` per body vuoto, JSON non
valido o valori fuori range: mostrare il messaggio accanto al campo.

Ridurre la capacità sotto `active` **non** interrompe nessuno; i nuovi download
sono rifiutati finché `active` non scende. Mostrarlo nel dialog di conferma se
`capacity < active`.

"Espandi" in UI = `PATCH` con `capacity` corrente + N (p.es. pulsanti +10/+50).
Il calcolo è lato dashboard sul valore appena letto: due admin che espandono
insieme possono sovrascriversi, accettabile per questo uso.

### 3.3 `POST /v2/download-queue/reset`

Nessun corpo. Riporta `enabled`/`capacity`/`retry_after_seconds` a `defaults`.
Non tocca i download in corso né i contatori. Risposta come §3.1.

### 3.4 `DELETE /v2/download-queue/slots/{id}`

Interrompe il download e libera subito lo slot. Risposta `{"evicted": 1}`;
`404` se lo slot non esiste più (download già finito nel frattempo: aggiornare
la lista, non mostrarlo come errore grave). `400` se l'id non è numerico.

L'utente dell'Updater riceve un file troncato, che scarta e riscarica più
tardi. Richiede conferma in UI.

### 3.5 `DELETE /v2/download-queue/slots`

Interrompe **tutti** i download in corso. Risposta `{"evicted": N}`. Azione
distruttiva: conferma esplicita ("Interrompi N download in corso?").

## 4. UI proposta

1. **Card di stato**: badge Attiva/Disattivata, barra `active / capacity`
   (rossa quando `available == 0`), `Retry-After` corrente, contatori
   accettati/rifiutati/interrotti, nota "dall'ultimo riavvio".
2. **Controlli**: toggle attiva/disattiva; input capacità con pulsanti
   +10/+50/−10; input Retry-After (secondi); pulsante "Ripristina .env"
   (abilitato solo se correnti ≠ `defaults`).
3. **Tabella download in corso**: prodotto, versione, hostname, IP, HWID,
   iniziato (relativo, "da 12 s"), pulsante "Interrompi". Sopra, "Interrompi
   tutti" (disabilitato con 0 slot).
4. **Avviso fisso**: "Le modifiche non sopravvivono al riavvio dell'API".

## 5. Errori

| Status | Significato | UI |
|--------|-------------|----|
| `401` | Chiave admin o dashboard mancante/errata | Errore di configurazione del server dashboard |
| `400` | Validazione | Messaggio accanto al campo |
| `404` | Slot già liberato | Aggiorna la lista silenziosamente |
| `503` | Coda non configurata sull'istanza | "Funzione non disponibile su questa istanza" |
