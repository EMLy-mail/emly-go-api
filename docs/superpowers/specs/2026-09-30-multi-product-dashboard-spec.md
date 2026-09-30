# API multi-prodotto e prodotti per utente — spec per la dashboard

Spec per il team della dashboard — 2026-09-30

Controparte lato API: `internal/productreg`, `internal/products`,
`internal/updates`, `internal/session/scope.go` in `emly-go-api`; route in
`ROUTES.md` §1 (*Scope prodotti*), §5.2, §5.3, §5.7, §5.8, §5.11. Controparte
Agent (ex EMLy Updater): `2026-09-30-agent-multi-product-design.md`.

---

## 1. Cosa cambia lato API

1. **EMLy non è più l'unico prodotto.** L'API ha un registro di prodotti
   (`/v2/products`). Ogni prodotto ha le sue release, i suoi slot
   stable/beta/critical e il suo manifest sotto `/v2/updates/{product}/...`.
2. **Ogni utente ha i suoi prodotti.** Un utente vede e gestisce solo i
   prodotti che gli sono stati assegnati, **anche se è admin**. Fa eccezione
   solo l'**owner**, che vede sempre tutti i prodotti. La regola la
   applica l'API, a patto che la dashboard le mandi `X-Session-Token` (§3).
3. **Le macchine seguono i prodotti.** Un utente vede solo le macchine che
   hanno installato almeno uno dei suoi prodotti.

Le route storiche di EMLy (`/v2/updates/releases`, `/v2/updates/manifest`, …)
funzionano ancora e restano alias di `emly`, ma la dashboard dovrebbe passare
alla forma con prodotto per **tutti** i prodotti, EMLy compreso: un solo
codice, parametrizzato dallo slug.

## 2. Al deploy

- La migration assegna `emly` a **tutti gli utenti esistenti**, quindi il
  giorno del rilascio nessuno perde niente di quello che vedeva.
- Gli utenti creati **dopo** non hanno prodotti e non vedono niente finché un
  admin non glieli assegna (§6). Va detto nella UI di creazione utente.
- **Macchine senza inventario.** Una macchina è visibile a un utente solo se ha
  riportato almeno un prodotto installato. La migration ricava `emly` da
  `emly_version` per tutte le macchine che l'hanno già riportata, e gli Agent
  attuali continuano a mandare `X-EMLy-AppVersion`. Quindi una macchina senza
  EMLy (o su cui EMLy non è mai stato rilevato) sparisce dalla lista di tutti
  gli utenti con sessione **tranne gli owner**, che vedono ogni macchina, come
  chi chiama con la sola admin key.
  Aspettatevi qualche macchina in meno nei conteggi rispetto a prima.

## 3. `X-Session-Token` diventa obbligatorio per lo scope

Finora il token veniva mandato solo per attribuire le scritture (chi ha creato
un ban, chi ha pubblicato una config). Ora sulle route *scoped* decide cosa si
vede:

| Richiesta | Cosa vede |
|-----------|-----------|
| admin key, **nessun** token | tutto (script, integrazioni) |
| admin key + token valido di un `owner` | tutto |
| admin key + token valido di un altro utente | solo i prodotti assegnati all'utente |
| admin key + token scaduto/sconosciuto/utente disabilitato | **niente** |

**La dashboard deve mandare `X-Session-Token` su ogni chiamata server-side
fatta per conto di un utente**, altrimenti ogni utente vede tutto. Route
scoped: gestione release (`/v2/updates/{product}/releases…` e gli alias
storici), `/v2/products/*`, `/v2/stats/*`. Per lo stream WebSocket
`/v2/stats/stream` il token va in `X-Session-Token` o, se il proxy lo toglie,
in `?session_token=`, come già succede per `?admin_key=`.

Un token scaduto non fa più "vedere tutto": produce liste vuote e `403`.
Conviene gestire il `403` "product not assigned to this user" rimandando al
login se `/v2/api/admin/auth/validate` risponde `401`.

## 4. Selettore prodotto

`GET /v2/api/admin/auth/validate` ora restituisce anche i prodotti assegnati
(per un owner, tutti i prodotti esistenti):

```json
{
  "success": true,
  "user": {
    "id": "…", "username": "mrossi", "displayname": "Mario Rossi",
    "role": "admin", "enabled": true,
    "products": ["emly", "foo"]
  }
}
```

È lo stesso elenco con cui l'API filtra, quindi è la fonte giusta per il
selettore prodotto. Per il nome leggibile di ciascuno usare
`GET /v2/products` (con il token, restituisce già solo quelli assegnati).

- `products: []` → schermata vuota con un messaggio tipo "Nessun prodotto
  assegnato, contatta un amministratore".
- Un solo prodotto → il selettore si può nascondere.
- Il prodotto scelto va nel path di ogni chiamata release e nel `?product=`
  delle stats.

## 5. Pagine release

Tutto come oggi, con lo slug nel path:

| Prima | Ora |
|-------|-----|
| `GET /v2/updates/releases` | `GET /v2/updates/{product}/releases` |
| `POST /v2/updates/releases` | `POST /v2/updates/{product}/releases` |
| `PUT/PATCH/DELETE /v2/updates/releases/{version}` | `…/{product}/releases/{version}` |
| `PATCH /v2/updates/releases/{version}/channel` | `…/{product}/releases/{version}/channel` |

Differenze da mostrare nella UI:

- stable/beta/critical sono **per prodotto**: promuovere una release di `foo`
  non tocca EMLy. Un'eventuale scritta "questa sostituirà la stable attuale"
  deve riferirsi al prodotto selezionato;
- la stessa versione può esistere in due prodotti;
- ogni release ha il campo `product`;
- l'URL del manifest da mostrare/copiare è `/v2/updates/{product}/manifest`
  (per EMLy resta valido anche `/v2/updates/manifest`).

Errori nuovi: `404` "product not found" (slug inesistente), `403` "product not
assigned to this user".

## 6. Pagina "Prodotti" (nuova)

CRUD su `/v2/products` (admin key + token):

| Azione | Chiamata | Note |
|--------|----------|------|
| Elenco | `GET /v2/products` | solo quelli assegnati |
| Crea | `POST /v2/products` `{slug, name, s3_prefix?, enabled?}` | chi lo crea se lo ritrova assegnato |
| Modifica | `PATCH /v2/products/{slug}` `{name?, s3_prefix?, enabled?}` | |
| Elimina | `DELETE /v2/products/{slug}` | `409` se ha release o se è `emly` |

Regole di validazione da riflettere nel form:

- `slug`: `^[a-z0-9][a-z0-9-]{0,19}$`, **non modificabile** dopo la
  creazione (va detto nel form). Riservati: `manifest`, `releases`,
  `download`, `updater`, `all`, `products` (`400`). Già esistente: `409`.
- `name`: obbligatorio, max 100 caratteri.
- `s3_prefix`: facoltativo, vuoto = default (`<S3_UPDATES_PREFIX>/<slug>`).
  Avvertimento da mostrare in modifica: **cambiarlo non sposta i file già
  caricati**, i download delle release esistenti andrebbero in `404`.
- `enabled: false` = il prodotto sparisce da manifest e download pubblici
  (`404`), ma le release si possono ancora gestire. Utile per preparare un
  prodotto prima del lancio. Un toggle con questa spiegazione.

## 7. Gestione utenti: assegnazione prodotti

| Chiamata | Note |
|----------|------|
| `GET /v2/api/admin/users/{id}/products` | `{"user_id", "products": [...]}` |
| `PUT /v2/api/admin/users/{id}/products` `{"products": ["emly","foo"]}` | sostituisce **tutto** l'elenco; `[]` toglie tutto |

- Solo un utente con ruolo `admin` o `owner` può cambiare le assegnazioni
  (`403` per `user`): il controllo si nasconde per gli altri
  (`canAssignProducts` in `lib/roles.ts`).
- Un admin può assegnare anche prodotti che lui stesso non vede (per un owner
  il problema non si pone: `GET /v2/products` gli restituisce già tutto). L'elenco
  delle opzioni va quindi preso da una chiamata **senza** token o, meglio,
  mostrando i soli prodotti che l'admin conosce: da decidere lato UX.
  `GET /v2/products` con token restituisce solo quelli dell'admin.
- `400` "unknown products: …" se uno slug non esiste.
- Dopo il `PUT` l'utente vede i nuovi prodotti alla richiesta successiva, senza
  bisogno di rifare login. Il suo selettore però si aggiorna solo quando la
  dashboard richiama `validate`.

## 8. Statistiche e macchine

- `?product=` su `/v2/stats/summary`, `/v2/stats/events` e sul `subscribe`
  dello stream accetta ora qualunque slug del registro, più `updater` e
  `all`. Default ancora `emly`.
- Per un utente con token, `all` vuol dire "tutti i **miei** prodotti più
  `updater`" (il self-update dell'Agent, che non appartiene a nessun
  prodotto). Un prodotto non assegnato → `403` (REST) o `error` sul
  `subscribe` (WS). Se l'utente non ha `emly`, lo stream parte già su `all`.
- `/v2/stats/clients`, `/clients/{id}`, `DELETE /clients/{id}` e i conteggi
  client del summary (`total_clients`, `connected_clients`,
  `clients_by_version`, `clients_by_config_revision`) contano solo le macchine
  visibili (§2). Una macchina fuori scope è `404`.
- `GET /v2/stats/clients/{id}` restituisce anche `products`:

  ```json
  "products": [
    { "product": "emly", "version": "3.5.0", "updated_at": "2026-09-30T08:12:00Z" },
    { "product": "foo",  "version": "1.2.0", "updated_at": "2026-09-28T16:40:00Z" }
  ]
  ```

  `updated_at` = da quando la macchina è a quella versione. Da mostrare nella
  scheda macchina accanto a `emly_version` (che resta, per compatibilità).

## 9. Checklist

- [ ] `X-Session-Token` su ogni chiamata scoped (e `?session_token=` sullo stream)
- [ ] selettore prodotto da `validate.user.products`
- [ ] pagine release parametrizzate da `{product}`
- [ ] pagina Prodotti (CRUD, avvisi su slug e `s3_prefix`)
- [ ] assegnazione prodotti nella gestione utenti (admin e owner)
- [ ] messaggio per utenti senza prodotti; nota "nessun prodotto" alla creazione utente
- [ ] filtro prodotto nelle stats dinamico; `products` nella scheda macchina
- [ ] gestione di `403` / `404` nuovi
