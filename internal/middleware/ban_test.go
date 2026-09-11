package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"emly-api-go/internal/models"
)

// newTestBanList builds a BanList with a pinned snapshot and no database, so
// the matcher can be exercised without one.
func newTestBanList(adminKey string, bans ...models.Ban) *BanList {
	b := &BanList{
		ips:       map[string]models.Ban{},
		hwids:     map[string]models.Ban{},
		hostnames: map[string]models.Ban{},
		adminKey:  adminKey,
	}
	for _, ban := range bans {
		switch ban.BanType {
		case models.BanTypeIP:
			b.ips[ban.Value] = ban
		case models.BanTypeHWID:
			b.hwids[ban.Value] = ban
		case models.BanTypeHostname:
			b.hostnames[ban.Value] = ban
		}
	}
	return b
}

func serve(b *BanList, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	b.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot) // stands in for "reached the route"
	})).ServeHTTP(rec, r)
	return rec
}

func TestBanMatchesEachIdentifier(t *testing.T) {
	b := newTestBanList("admin-key",
		models.Ban{ID: 1, BanType: models.BanTypeIP, Value: "203.0.113.9"},
		models.Ban{ID: 2, BanType: models.BanTypeHWID, Value: "36CC511A-F0DE-EA11-8106-842AFDCE34D0"},
		models.Ban{ID: 3, BanType: models.BanTypeHostname, Value: "pc-01"},
	)

	cases := []struct {
		name    string
		prepare func(*http.Request)
		blocked bool
	}{
		{"banned ip", func(r *http.Request) { r.Header.Set("X-Real-IP", "203.0.113.9") }, true},
		{"clean ip", func(r *http.Request) { r.Header.Set("X-Real-IP", "203.0.113.10") }, false},
		{"banned hwid", func(r *http.Request) {
			r.Header.Set("X-EMLy-HWID", "36CC511A-F0DE-EA11-8106-842AFDCE34D0")
		}, true},
		{"banned hostname", func(r *http.Request) { r.Header.Set("X-EMLy-Hostname", "pc-01") }, true},
		// Windows is case-insensitive about hostnames and the fleet reports
		// them however the machine feels like; a ban that only caught one
		// casing would be trivially evaded by a rename that changes nothing.
		{"banned hostname, different case", func(r *http.Request) {
			r.Header.Set("X-EMLy-Hostname", "PC-01")
		}, true},
		{"clean hostname", func(r *http.Request) { r.Header.Set("X-EMLy-Hostname", "pc-02") }, false},
		{"nothing identifying", func(r *http.Request) {}, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/v2/config", nil)
			r.RemoteAddr = "198.51.100.1:1234"
			c.prepare(r)

			rec := serve(b, r)
			if c.blocked && rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rec.Code)
			}
			if !c.blocked && rec.Code != http.StatusTeapot {
				t.Errorf("status = %d, want the request to pass through", rec.Code)
			}
		})
	}
}

// Banning the office IP must not lock the dashboard - and the route that
// removes the ban - out from behind that same address.
func TestBanExemptsAdminKey(t *testing.T) {
	b := newTestBanList("admin-key", models.Ban{ID: 1, BanType: models.BanTypeIP, Value: "203.0.113.9"})

	r := httptest.NewRequest("GET", "/v2/bans", nil)
	r.Header.Set("X-Real-IP", "203.0.113.9")
	r.Header.Set("X-Admin-Key", "admin-key")

	if rec := serve(b, r); rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want the admin key to bypass the ban", rec.Code)
	}
}

// A wrong admin key is not a bypass.
func TestBanIgnoresWrongAdminKey(t *testing.T) {
	b := newTestBanList("admin-key", models.Ban{ID: 1, BanType: models.BanTypeIP, Value: "203.0.113.9"})

	r := httptest.NewRequest("GET", "/v2/bans", nil)
	r.Header.Set("X-Real-IP", "203.0.113.9")
	r.Header.Set("X-Admin-Key", "not-the-key")

	if rec := serve(b, r); rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// An empty list must let everything through, including the state before the
// first successful load - a database hiccup at boot must not close the API.
func TestBanEmptyListPassesEverything(t *testing.T) {
	b := newTestBanList("admin-key")
	r := httptest.NewRequest("GET", "/v2/config", nil)
	r.Header.Set("X-Real-IP", "203.0.113.9")
	r.Header.Set("X-EMLy-HWID", "whatever")

	if rec := serve(b, r); rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want pass-through on an empty list", rec.Code)
	}
}

func TestClientIPPrefersProxyHeaders(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(*http.Request)
		want    string
	}{
		{"remote addr", func(r *http.Request) { r.RemoteAddr = "203.0.113.9:5555" }, "203.0.113.9"},
		{"x-real-ip wins", func(r *http.Request) {
			r.RemoteAddr = "10.0.0.1:5555"
			r.Header.Set("X-Real-IP", "203.0.113.9")
		}, "203.0.113.9"},
		// A chain names the original client first; taking the last would
		// ban the proxy and let the real one through.
		{"x-forwarded-for takes the first hop", func(r *http.Request) {
			r.RemoteAddr = "10.0.0.1:5555"
			r.Header.Set("X-Forwarded-For", "203.0.113.9, 70.41.3.18")
		}, "203.0.113.9"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			c.prepare(r)
			if got := clientIP(r); got != c.want {
				t.Errorf("clientIP = %q, want %q", got, c.want)
			}
		})
	}
}
