package handlers

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// The whole X-EMLy-* set the EMLy Updater sends must land on clientIdentity.
// Both telemetry paths key off this one reader, so a header dropped here is a
// column that silently never fills in.
func TestClientIdentityFromRequest(t *testing.T) {
	r := httptest.NewRequest("GET", "/v2/config", nil)
	r.RemoteAddr = "10.0.0.5:51234"
	r.Header.Set("User-Agent", "EMLy-Updater/1.5.6 (f.fois@3git.eu)")
	r.Header.Set("X-EMLy-HWID", "36CC511A-F0DE-EA11-8106-842AFDCE34D0")
	r.Header.Set("X-EMLy-Hostname", "PC-01")
	r.Header.Set("X-EMLy-ADDomain", "contoso.local")
	r.Header.Set("X-EMLy-LoggedUser", `CONTOSO\mario.rossi`)
	r.Header.Set("X-EMLy-Serial", "CND0342SLW")
	r.Header.Set("X-EMLy-Product", "1F3N0EA#ABZ")

	got := clientIdentityFromRequest(r)
	want := clientIdentity{
		HWID:       "36CC511A-F0DE-EA11-8106-842AFDCE34D0",
		Hostname:   "PC-01",
		ADDomain:   "contoso.local",
		LoggedUser: `CONTOSO\mario.rossi`,
		Serial:     "CND0342SLW",
		Product:    "1F3N0EA#ABZ",
		UAVersion:  "1.5.6",
		Contact:    "f.fois@3git.eu",
		IP:         "10.0.0.5",
	}
	if got != want {
		t.Fatalf("clientIdentityFromRequest =\n  %+v\nwant\n  %+v", got, want)
	}
}

// A client too old to send the new headers, or one on a machine with nobody
// logged on, must still be identified and tracked - the new values simply
// come through empty, which upsertUpdaterClient turns into "leave the stored
// value alone".
func TestClientIdentityWithoutNewHeaders(t *testing.T) {
	r := httptest.NewRequest("GET", "/v2/updates/manifest", nil)
	r.RemoteAddr = "10.0.0.5:51234"
	r.Header.Set("X-EMLy-Hostname", "PC-01")

	got := clientIdentityFromRequest(r)
	if !got.identified() {
		t.Error("a request carrying only a hostname must still be identified")
	}
	if got.LoggedUser != "" || got.Serial != "" || got.Product != "" {
		t.Errorf("absent headers produced values: %+v", got)
	}
}

// A request carrying neither HWID nor hostname names no machine, so it is
// served but not tracked.
func TestClientIdentityUnidentified(t *testing.T) {
	r := httptest.NewRequest("GET", "/v2/config", nil)
	r.RemoteAddr = "10.0.0.5:51234"
	if clientIdentityFromRequest(r).identified() {
		t.Error("a request with no identifying header must not be identified")
	}
}

// Over-long values are cut to the column width rather than failing the
// upsert, and the cut is on runes so a multi-byte account name never ends in
// half a character.
func TestClientIdentityTruncatesToColumnWidth(t *testing.T) {
	r := httptest.NewRequest("GET", "/v2/config", nil)
	r.Header.Set("X-EMLy-HWID", "HW-1")
	r.Header.Set("X-EMLy-LoggedUser", strings.Repeat("à", 400))
	r.Header.Set("X-EMLy-Serial", strings.Repeat("S", 200))
	r.Header.Set("X-EMLy-Product", strings.Repeat("P", 200))

	got := clientIdentityFromRequest(r)
	if n := len([]rune(got.LoggedUser)); n != 255 {
		t.Errorf("LoggedUser kept %d runes, want 255", n)
	}
	if got.LoggedUser != strings.Repeat("à", 255) {
		t.Error("LoggedUser was cut mid-character")
	}
	if n := len([]rune(got.Serial)); n != 128 {
		t.Errorf("Serial kept %d runes, want 128", n)
	}
	if n := len([]rune(got.Product)); n != 128 {
		t.Errorf("Product kept %d runes, want 128", n)
	}
}
