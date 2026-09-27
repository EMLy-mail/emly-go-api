package clientws

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jmoiron/sqlx"

	"emly-api-go/internal/updaterclient"
)

// stubDataUpsert replaces upsertClientFn for one test and returns a pointer to
// every identity it was called with.
func stubDataUpsert(t *testing.T, err error) *[]updaterclient.Identity {
	t.Helper()
	var got []updaterclient.Identity
	orig := upsertClientFn
	upsertClientFn = func(_ context.Context, _ *sqlx.DB, id updaterclient.Identity) (int64, error) {
		got = append(got, id)
		return 7, err
	}
	t.Cleanup(func() { upsertClientFn = orig })
	return &got
}

func postClientData(t *testing.T, body string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Post("/v2/clients/data", PostClientData(nil))
	req := httptest.NewRequest(http.MethodPost, "/v2/clients/data", strings.NewReader(body))
	req.RemoteAddr = "10.0.0.5:51234"
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// The body carries every field the X-EMLy-* headers carry, and it must land
// on the same Identity IdentityFromRequest would build from those headers -
// User-Agent version/contact and peer IP included.
func TestPostClientDataUpsertsFullIdentity(t *testing.T) {
	got := stubDataUpsert(t, nil)
	rec := postClientData(t, `{
		"hwid": "36CC511A-F0DE-EA11-8106-842AFDCE34D0",
		"hostname": "PC-01",
		"ad_domain": "contoso.local",
		"logged_user": "CONTOSO\\mario.rossi",
		"logged_user_state": "disconnected",
		"logged_user_disconnected_at": "2026-09-12T18:04:31Z",
		"serial": "CND0342SLW",
		"product": "1F3N0EA#ABZ",
		"os_version": "Windows 11 24H2 Professional (Build 26100.4652)",
		"emly_version": "3.4.1"
	}`, map[string]string{"User-Agent": "EMLy-Updater/1.9.0 (f.fois@3git.eu)"})

	if rec.Code != http.StatusNoContent {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body)
	}
	if len(*got) != 1 {
		t.Fatalf("upsert called %d times, want 1", len(*got))
	}
	want := updaterclient.Identity{
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
		UAVersion:                "1.9.0",
		Contact:                  "f.fois@3git.eu",
		IP:                       "10.0.0.5",
	}
	if (*got)[0] != want {
		t.Fatalf("identity =\n  %+v\nwant\n  %+v", (*got)[0], want)
	}
}

func TestPostClientDataRejects(t *testing.T) {
	cases := []struct {
		name, body string
		want       int
	}{
		{"not JSON", `{`, http.StatusBadRequest},
		{"no hwid nor hostname", `{"ad_domain":"contoso.local"}`, http.StatusBadRequest},
		{"too large", `{"hwid":"` + strings.Repeat("a", clientDataMaxBody) + `"}`, http.StatusRequestEntityTooLarge},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stubDataUpsert(t, nil)
			if rec := postClientData(t, c.body, nil); rec.Code != c.want {
				t.Fatalf("code = %d (%s), want %d", rec.Code, rec.Body, c.want)
			}
			if len(*got) != 0 {
				t.Fatal("a rejected body must not reach the upsert")
			}
		})
	}
}

// Load-test traffic is answered like the real thing but never recorded.
func TestPostClientDataSkipsTestTraffic(t *testing.T) {
	got := stubDataUpsert(t, nil)
	rec := postClientData(t, `{"hwid":"K6-0001"}`, map[string]string{updaterclient.TestingHeader: "1"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code = %d, want 204", rec.Code)
	}
	if len(*got) != 0 {
		t.Fatal("test traffic reached the upsert")
	}
}

func TestPostClientDataUpsertFailure(t *testing.T) {
	stubDataUpsert(t, context.DeadlineExceeded)
	if rec := postClientData(t, `{"hwid":"HW-1"}`, nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500", rec.Code)
	}
}
