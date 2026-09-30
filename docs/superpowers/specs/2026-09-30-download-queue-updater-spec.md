# Coda download installer — spec per l'EMLy Updater

Spec per il team dell'Updater — 2026-09-30

Controparte lato API: `internal/downloadqueue` in `emly-go-api`, route
documentate in `ROUTES.md` §5.10. Controparte dashboard:
`2026-09-30-download-queue-dashboard-spec.md`.

---

## 1. Cosa cambia lato API

I due download di installer ora hanno un tetto sul numero di download
**contemporanei**, condiviso fra i due prodotti (un unico pool, default 50
slot):

| Endpoint | Prodotto |
|----------|----------|
| `GET /v2/updates/releases/{version}/download` | installer EMLy |
| `GET /v2/updates/download/updater/{version}` | installer Updater (self-update) |

Quando tutti gli slot sono occupati il server **non mette in attesa**: rifiuta
subito con `429`. Nulla cambia per manifest, `/v2/config` e `/v2/client/ws`.

## 2. La risposta 429

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

| Campo | Significato |
|-------|-------------|
| `Retry-After` (header) | Secondi interi da attendere prima di riprovare. Fonte primaria. |
| `error` | Sempre esattamente `"download queue full"` per questo caso. È il discriminante (§4). |
| `message` | Testo leggibile, adatto al log. Non va parsato. |
| `retry_after` | Stesso valore dell'header, per chi legge solo il body. |
| `capacity` / `active` | Informativi (log/diagnostica). Non usarli per decidere nulla. |

Il valore di `Retry-After` è configurabile lato server (default 60s, range
1–86400) e può cambiare a runtime dalla dashboard: **non va assunto costante**.

## 3. Comportamento richiesto

1. **Non è un errore, né un'outage.** Un `429` con `error == "download queue full"`
   è un "riprova più tardi". Loggarlo a livello **info**, non error/warn, e non
   farlo contare nei contatori di fallimento/backoff esponenziale del download.
2. **Rispettare `Retry-After`.** Attendere almeno quel numero di secondi prima
   di ritentare *lo stesso* download. Ordine di priorità della fonte:
   header `Retry-After` → campo `retry_after` del body → 60s di default.
   Ignorare valori non numerici o `<= 0` (usare il fallback).
3. **Aggiungere jitter.** Ritentare a `Retry-After + random(0, 30s)` (o
   proporzionale, p.es. +0–50%). Senza jitter tutte le macchine rifiutate nello
   stesso istante tornerebbero nello stesso istante, riempiendo di nuovo la coda.
4. **Tetto di tentativi per ciclo.** Dopo 5 `429` consecutivi sullo stesso
   download, rinunciare per il ciclo corrente e riprovare al prossimo ciclo di
   aggiornamento normale. Il manifest resta valido: non serve rileggerlo prima
   di ogni retry, ma se il ciclo successivo lo rilegge va bene.
5. **Nessun retry immediato.** Mai ritentare prima di `Retry-After`, nemmeno
   "una volta sola": è proprio il carico che la coda vuole evitare.
6. **Stato persistente non necessario.** Se il servizio viene riavviato durante
   l'attesa, si riparte dal ciclo normale; nessun obbligo di ricordare l'attesa.
7. **Self-update e update EMLy seguono le stesse regole**, perché condividono
   lo stesso pool.

## 4. Distinguere dagli altri 429

L'API può rispondere `429` anche dai rate limiter per IP (`RouteLimitByIP`,
limiter globale). Quelli **non** hanno il body sopra e di solito non hanno
`Retry-After`. Regola:

- `429` + JSON con `error == "download queue full"` → comportamento §3.
- qualunque altro `429` → comportamento già esistente dell'Updater per i 429
  (se non esiste: trattarlo come §3 con fallback 60s).

## 5. Download interrotti da un admin

La dashboard può interrompere un download in corso per liberarne lo slot.
In quel caso il client ha già ricevuto `200` e `Content-Length`, e la
connessione si chiude **prima** della fine del file. Lato Updater va già
gestito come un download troncato qualunque:

- bytes ricevuti `<` `Content-Length`, oppure checksum SHA-256 del manifest non
  corrispondente → scartare il file, **mai** eseguirlo;
- ritentare con il backoff normale (non c'è `Retry-After` in questo caso).

Non è richiesto nulla di nuovo se questa verifica esiste già; va solo
confermato che esiste.

## 6. Esempio (pseudo-Go)

```go
resp, err := client.Do(req)
if err != nil { return retryLater(err) }
defer resp.Body.Close()

if resp.StatusCode == http.StatusTooManyRequests {
    var body struct {
        Error      string `json:"error"`
        RetryAfter int    `json:"retry_after"`
    }
    _ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body)

    wait := 60 * time.Second
    if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
        wait = time.Duration(s) * time.Second
    } else if body.RetryAfter > 0 {
        wait = time.Duration(body.RetryAfter) * time.Second
    }
    wait += time.Duration(rand.Int63n(int64(30 * time.Second)))

    if body.Error == "download queue full" {
        log.Info("download queue full, retrying later", "wait", wait)
        return errQueueFull{wait: wait} // non incrementa il backoff di errore
    }
    return errRateLimited{wait: wait}
}
```

## 7. Compatibilità

- Nessun cambiamento di formato sui `200`: un download che prende uno slot è
  identico a prima.
- Un Updater già in campo che non conosce questo spec riceverà `429` quando la
  coda è piena e lo tratterà come oggi tratta un errore HTTP generico. Va
  verificato che questo non porti a un retry stretto (loop senza attesa): se
  sì, la coda va rilasciata a capacità alta finché la nuova versione non è
  distribuita.
- Test suggeriti: 429 con header, 429 con solo body, 429 senza body (rate
  limiter), valore `Retry-After` non numerico, 5 `429` di fila, download
  troncato dopo `200`.
