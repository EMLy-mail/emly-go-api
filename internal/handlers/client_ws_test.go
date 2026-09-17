package handlers

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientIdentityFromWSPayload(t *testing.T) {
	r := httptest.NewRequest("GET", "/v2/client/ws", nil)
	r.RemoteAddr = "10.0.0.5:51234"
	r.Header.Set("User-Agent", "EMLy-Updater/1.6.3 (f.fois@3git.eu)")

	payload := clientWSIdentityPayload{
		HWID:                     "36CC511A-F0DE-EA11-8106-842AFDCE34D0",
		Hostname:                 "PC-01",
		ADDomain:                 "contoso.local",
		LoggedUser:               `CONTOSO\mario.rossi`,
		LoggedUserState:          "disconnected",
		LoggedUserDisconnectedAt: "2026-09-12T18:04:31Z",
		Serial:                   "CND0342SLW",
		Product:                  "1F3N0EA#ABZ",
		OSVersion:                "Windows 11 24H2 Professional (Build 26100.4652)",
		EMLyVersion:              "3.4.1",
	}

	got := clientIdentityFromWSPayload(r, payload)
	want := clientIdentity{
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
		UAVersion:                "1.6.3",
		Contact:                  "f.fois@3git.eu",
		IP:                       "10.0.0.5",
	}
	if got != want {
		t.Fatalf("clientIdentityFromWSPayload =\n  %+v\nwant\n  %+v", got, want)
	}
}

func TestClientIdentityFromWSPayloadUnidentified(t *testing.T) {
	r := httptest.NewRequest("GET", "/v2/client/ws", nil)
	got := clientIdentityFromWSPayload(r, clientWSIdentityPayload{})
	if got.identified() {
		t.Fatalf("an empty payload must not be identified()")
	}
}
