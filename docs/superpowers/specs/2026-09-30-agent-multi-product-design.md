# API multi-prodotto — spec per l'Agent (ex EMLy Updater)

Spec per il team dell'Agent — 2026-09-30

Controparte lato API: `internal/updates`, `internal/productreg`,
`internal/updaterclient/products.go` in `emly-go-api`; route in `ROUTES.md`
§5.3 e §5.7 (*Prodotti installati*), protocollo WS in
`CLIENT_WS_PROTOCOL.md` (§4, `installed_products`). Controparte dashboard:
`2026-09-30-multi-product-dashboard-spec.md`.

---

## 0. In breve

**Nessuna modifica è obbligatoria.** Un Agent già in campo continua a
funzionare esattamente come oggi:

- il manifest e i download di EMLy restano su `/v2/updates/manifest` e
  `/v2/updates/releases/{version}/download`, con lo stesso formato e gli stessi
  link;
- il self-update dell'Agent (`/v2/updates/manifest/updater`,
  `/v2/updates/download/updater/{version}`) **non cambia**: non è un prodotto;
- `X-EMLy-AppVersion` continua a essere letto come versione di EMLy.

Le modifiche qui sotto servono a due cose nuove: **aggiornare prodotti diversi
da EMLy** (§2) e **dire all'API cosa è installato** (§1), da cui dipende quali
macchine vede ciascun utente della dashboard.

## 1. Inventario dei prodotti installati (consigliato, priorità alta)

La dashboard ora mostra a ogni utente solo le macchine che hanno installato
almeno uno dei **suoi** prodotti. L'API lo sa da un inventario che l'Agent
manda.

### 1.1 Via HTTP

Nuovo header, su **ogni** richiesta che già porta gli `X-EMLy-*` (manifest,
download, self-update, `/v2/config`):

```
X-EMLy-InstalledProducts: emly=3.5.0,foo=1.2.0
```

- coppie `slug=versione` separate da virgola, spazi ammessi;
- `slug`: `^[a-z0-9][a-z0-9-]{0,19}$` — lo stesso slug usato nel path
  `/v2/updates/{slug}/…`; versione: max 20 caratteri; voci non valide
  scartate dal server, al massimo 32;
- è l'inventario **completo**: un prodotto che c'era e non compare più viene
  considerato **disinstallato** e tolto;
- header **presente ma vuoto** = "non c'è nessun prodotto installato";
- header **assente** = "non lo so" → il server non tocca niente. Omettetelo
  quando il rilevamento fallisce, invece di mandarlo vuoto: un invio vuoto per
  errore farebbe sparire la macchina dalla dashboard.

**Non** confondere con `X-EMLy-Product`, che resta lo SKU del firmware.

### 1.2 Via WebSocket

Stesso contenuto nel messaggio `identity` di `GET /v2/client/ws`, come oggetto:

```json
"installed_products": { "emly": "3.5.0", "foo": "1.2.0" }
```

Stesse regole: campo assente = non riportato, `{}` = niente installato.
Funziona sia con il protocollo v1 sia con il v2. Va aggiunto in
`internal/wsclient` insieme all'header, che è il canale che l'API considera
"gemello" (una delle due vie senza l'altra lascia buchi nella dashboard).

### 1.3 EMLy

Continuate a mandare `X-EMLy-AppVersion` / `emly_version` come oggi. Se
l'inventario non cita `emly`, il server ricava la riga `emly` da quel valore.
Se lo cita, vince l'inventario. Conviene includere `emly` nell'inventario
comunque, con lo stesso valore (`GUI_SEMVER`), e ometterlo quando EMLy non è
installato (mai `0.0.0`, stessa regola di `installed_version`).

### 1.4 Dopo un aggiornamento

Il nuovo valore arriva con la richiesta successiva, oppure con un `identity`
alla riconnessione WS. Non serve altro. Se volete che la dashboard lo veda
subito dopo un update, riconnettete il WS o fate un giro di manifest.

## 2. Aggiornare altri prodotti (quando servirà)

Il formato del manifest è **identico** a quello di EMLy (stable/beta,
`sha256Checksums`, note, `isCritical`, `minRequiredVersion`):

```
GET /v2/updates/{slug}/manifest
```

- i link `stableDownload`/`betaDownload` puntano a
  `/v2/updates/{slug}/releases/{version}/download`: seguiteli così come sono,
  come fate per EMLy, senza costruirli a mano;
- il download è pubblico e passa per la stessa coda download di EMLy: stessa
  gestione di `429` + `Retry-After`;
- gli eventi `manifest_check`/`download` vengono registrati con `product` =
  slug, quindi le stats per prodotto funzionano senza altro lavoro.

**Differenza importante rispetto al self-update:** qui `404` vuol dire
davvero "questo prodotto non esiste o è stato disabilitato", non "il mirror non
implementa l'endpoint". Trattatelo come "nessun aggiornamento disponibile per
questo prodotto": niente errore rumoroso a ogni ciclo, magari un log una tantum
e un backoff lungo.

`/v2/updates/emly/manifest` esiste ed è equivalente a `/v2/updates/manifest`.
Per EMLy potete restare sul percorso storico: vale anche per i mirror di sito
non ancora aggiornati, che le route con slug non le hanno.

### 2.1 Mirror di sito

Un mirror con una versione dell'API precedente a questa risponde `404` a
`/v2/updates/{slug}/manifest`. Per prodotti diversi da EMLy, se il
`baseServer` del sito dà `404`, valutate di provare il `defaultServer` prima di
concludere che il prodotto non esiste. Da decidere con chi gestisce i mirror.

## 3. Cosa resta aperto

Questi punti non sono coperti dall'API di oggi e vanno decisi insieme:

1. **Quali prodotti gestisce l'Agent su una macchina.** Oggi l'Agent sa cosa
   fare solo per EMLy. Per un nuovo prodotto servono: come rilevarlo e leggerne
   la versione, dove sta l'installer, come installarlo in silenzio. Proposta:
   una sezione `products` nel documento di remote config (`/v2/config`), per
   prodotto: slug, canale, rilevamento, argomenti dell'installer. Richiede una
   modifica a `internal/remoteconfig` lato API. Non è in questa release.
2. **Comandi WS.** `emly.manifest.check` resta specifico di EMLy. Per gli
   altri prodotti servirebbe un comando generico, per esempio
   `product.manifest.check {product: "foo"}`, e un campo `target` libero
   (lo slug) in `ManifestCheck`. È un cambio di protocollo da fare con
   capability negotiation, come gli altri comandi v2.
3. **Eventi `update.*`.** Stesso discorso: oggi hanno `target: emly|updater`;
   per un prodotto qualunque andrebbe ammesso lo slug.

## 4. Checklist

- [ ] `X-EMLy-InstalledProducts` su ogni richiesta HTTP (omesso se il rilevamento fallisce)
- [ ] `installed_products` nell'`identity` WS
- [ ] `emly` incluso nell'inventario quando installato
- [ ] (quando si aggiunge un prodotto) manifest/download via `/v2/updates/{slug}/…`, `404` = non disponibile
- [ ] (quando si aggiunge un prodotto) gestione `429` come per EMLy
- [ ] decidere i punti aperti del §3
