package updaterclient

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The whole X-EMLy-* set the EMLy Updater sends must land on Identity.
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
	r.Header.Set("X-EMLy-OSVersion", "Windows 11 24H2 Professional (Build 26100.4652)")
	// The EMLy app's own version. The name matters: the updater's version is
	// the one in the User-Agent, and the two must not be read from the same
	// place - a fleet on one current updater is still spread across EMLy
	// releases.
	r.Header.Set("X-EMLy-AppVersion", "3.4.1")

	got := IdentityFromRequest(r)
	want := Identity{
		HWID:                     "36CC511A-F0DE-EA11-8106-842AFDCE34D0",
		Hostname:                 "PC-01",
		ADDomain:                 "contoso.local",
		LoggedUser:               `CONTOSO\mario.rossi`,
		LoggedUserState:          "disconnected",
		LoggedUserDisconnectedAt: time.Date(2026, 9, 12, 18, 4, 31, 0, time.UTC),
		Serial:                   "CND0342SLW",
		Product:                  "1F3N0EA#ABZ",
		OSVersion:                "Windows 11 24H2 Professional (Build 26100.4652)",
		EMLyVersion:              "3.4.1",
		UAVersion:                "1.5.6",
		Contact:                  "f.fois@3git.eu",
		IP:                       "10.0.0.5",
	}
	if got != want {
		t.Fatalf("IdentityFromRequest =\n  %+v\nwant\n  %+v", got, want)
	}
}

// A client too old to send the new headers must still be identified and
// tracked - the new values simply come through empty, which
// upsertUpdaterClient turns into "leave the stored value alone".
func TestClientIdentityWithoutNewHeaders(t *testing.T) {
	r := httptest.NewRequest("GET", "/v2/updates/manifest", nil)
	r.RemoteAddr = "10.0.0.5:51234"
	r.Header.Set("X-EMLy-Hostname", "PC-01")

	got := IdentityFromRequest(r)
	if !got.Identified() {
		t.Error("a request carrying only a hostname must still be identified")
	}
	if got.LoggedUser != "" || got.LoggedUserState != "" || !got.LoggedUserDisconnectedAt.IsZero() ||
		got.Serial != "" || got.Product != "" || got.OSVersion != "" || got.EMLyVersion != "" {
		t.Errorf("absent headers produced values: %+v", got)
	}
	if got.NobodyLoggedOn {
		t.Error("a request with no updater User-Agent must not claim nobody is logged on")
	}
}

// From 1.6.2 on the updater sends the user on every request that has one, so
// a request without it means nobody is logged on and the stored user must be
// cleared - otherwise a machine signed out for days keeps showing its last
// user. Older updaters never send the state, so their silence stays "unknown".
func TestClientIdentityNobodyLoggedOn(t *testing.T) {
	cases := []struct {
		name, ua, user string
		want           bool
	}{
		{"current updater, no user", "EMLy-Updater/1.6.2 (f.fois@3git.eu)", "", true},
		{"newer updater, no user", "EMLy-Updater/1.10.0 (f.fois@3git.eu)", "", true},
		{"dev build of the first version", "EMLy-Updater/1.6.2-dev (f.fois@3git.eu)", "", true},
		{"current updater with a user", "EMLy-Updater/1.6.2 (f.fois@3git.eu)", `CONTOSO\bera2`, false},
		{"updater before session state", "EMLy-Updater/1.6.1 (f.fois@3git.eu)", "", false},
		{"not the updater", "Mozilla/5.0", "", false},
		{"unparseable version", "EMLy-Updater/dev (f.fois@3git.eu)", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/v2/config", nil)
			r.Header.Set("X-EMLy-HWID", "HW-1")
			r.Header.Set("User-Agent", c.ua)
			if c.user != "" {
				r.Header.Set("X-EMLy-LoggedUser", c.user)
				r.Header.Set("X-EMLy-LoggedUserState", "active-console")
			}
			if got := IdentityFromRequest(r).NobodyLoggedOn; got != c.want {
				t.Errorf("NobodyLoggedOn = %v, want %v", got, c.want)
			}
		})
	}
}

func TestCompareDottedVersions(t *testing.T) {
	cases := []struct {
		a, b string
		cmp  int
		ok   bool
	}{
		{"1.6.2", "1.6.2", 0, true},
		{"1.6.10", "1.6.2", 1, true},
		{"1.5.9", "1.6.2", -1, true},
		{"2.0", "1.6.2", 1, true},
		{"1.6.2.4", "1.6.2", 0, true},
		{"1.6.2+build.7", "1.6.2", 0, true},
		{"v1.6.2", "1.6.2", 0, false},
		{"", "1.6.2", 0, false},
	}
	for _, c := range cases {
		cmp, ok := compareDottedVersions(c.a, c.b)
		if cmp != c.cmp || ok != c.ok {
			t.Errorf("compareDottedVersions(%q, %q) = (%d, %v), want (%d, %v)", c.a, c.b, cmp, ok, c.cmp, c.ok)
		}
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
	if IdentityFromRequest(r).Identified() {
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
	r.Header.Set("X-EMLy-OSVersion", strings.Repeat("W", 300))

	got := IdentityFromRequest(r)
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
	if n := len([]rune(got.OSVersion)); n != 128 {
		t.Errorf("OSVersion kept %d runes, want 128", n)
	}
}

func TestLoggedUserFromWS(t *testing.T) {
	got := LoggedUserFromWS("1.9.0", `CORP\m.rossi`, "disconnected", "2026-09-23T07:58:10Z")
	if got.LoggedUser != `CORP\m.rossi` || got.LoggedUserState != "disconnected" ||
		!got.LoggedUserDisconnectedAt.Equal(time.Date(2026, 9, 23, 7, 58, 10, 0, time.UTC)) || got.NobodyLoggedOn {
		t.Fatalf("got %+v", got)
	}
	// A v2 updater (>= loggedUserSessionMinUpdaterVersion) reporting no
	// user is an answer: nobody is logged on.
	if nobody := LoggedUserFromWS("1.9.0", "", "", ""); !nobody.NobodyLoggedOn {
		t.Fatalf("empty user from 1.9.0 must mean nobody logged on: %+v", nobody)
	}
	// An unknown state is dropped exactly like the header path drops it.
	if odd := LoggedUserFromWS("1.9.0", "u", "sleeping", ""); odd.LoggedUserState != "" {
		t.Fatalf("unknown state kept: %+v", odd)
	}
}
