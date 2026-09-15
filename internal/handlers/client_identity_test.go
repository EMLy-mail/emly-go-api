package handlers

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
	r.Header.Set("X-EMLy-LoggedUserState", "disconnected")
	r.Header.Set("X-EMLy-LoggedUserDisconnectedAt", "2026-09-12T18:04:31Z")
	r.Header.Set("X-EMLy-Serial", "CND0342SLW")
	r.Header.Set("X-EMLy-Product", "1F3N0EA#ABZ")

	got := clientIdentityFromRequest(r)
	want := clientIdentity{
		HWID:                     "36CC511A-F0DE-EA11-8106-842AFDCE34D0",
		Hostname:                 "PC-01",
		ADDomain:                 "contoso.local",
		LoggedUser:               `CONTOSO\mario.rossi`,
		LoggedUserState:          "disconnected",
		LoggedUserDisconnectedAt: time.Date(2026, 9, 12, 18, 4, 31, 0, time.UTC),
		Serial:                   "CND0342SLW",
		Product:                  "1F3N0EA#ABZ",
		UAVersion:                "1.5.6",
		Contact:                  "f.fois@3git.eu",
		IP:                       "10.0.0.5",
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
	if got.LoggedUser != "" || got.LoggedUserState != "" || !got.LoggedUserDisconnectedAt.IsZero() ||
		got.Serial != "" || got.Product != "" {
		t.Errorf("absent headers produced values: %+v", got)
	}
}

// Only the states the updater defines are stored, and the disconnection time
// only ever travels with a disconnected session: anything else would put a
// value in the row the dashboard cannot interpret, or a stale timestamp next
// to a live session.
func TestParseLoggedUserSession(t *testing.T) {
	at := "2026-09-12T20:04:31+02:00"
	wantAt := time.Date(2026, 9, 12, 18, 4, 31, 0, time.UTC)
	cases := []struct {
		name, state, at string
		wantState       string
		wantAt          time.Time
	}{
		{"disconnected with time, normalised to UTC", "disconnected", at, "disconnected", wantAt},
		{"fractional seconds dropped", "disconnected", "2026-09-12T18:04:31.734Z", "disconnected", wantAt},
		{"disconnected without time", "disconnected", "", "disconnected", time.Time{}},
		{"disconnected with garbage time", "disconnected", "yesterday", "disconnected", time.Time{}},
		{"state is case-insensitive", " Active-RDP ", "", "active-rdp", time.Time{}},
		{"time dropped on an active session", "active-console", at, "active-console", time.Time{}},
		{"unknown state ignored", "locked", at, "", time.Time{}},
		{"absent", "", "", "", time.Time{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state, got := parseLoggedUserSession(c.state, c.at)
			if state != c.wantState || !got.Equal(c.wantAt) {
				t.Errorf("parseLoggedUserSession(%q, %q) = (%q, %v), want (%q, %v)",
					c.state, c.at, state, got, c.wantState, c.wantAt)
			}
		})
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
