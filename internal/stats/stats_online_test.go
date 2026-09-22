package stats

import (
	"testing"

	"emly-api-go/internal/models"
	"emly-api-go/internal/presencehub"
)

func TestDecorateOnline(t *testing.T) {
	presence := presencehub.New(presencehub.DefaultGraceDuration)
	tok, _ := presence.Connect(42)
	defer presence.Disconnect(tok)

	clients := []models.UpdaterClient{{ID: 42}, {ID: 7}}
	decorateOnline(clients, presence)

	if !clients[0].Online {
		t.Fatal("client 42 should be Online=true (connected)")
	}
	if clients[1].Online {
		t.Fatal("client 7 should be Online=false (never connected)")
	}
}

func TestDecorateOnlineNilHub(t *testing.T) {
	clients := []models.UpdaterClient{{ID: 42}}
	decorateOnline(clients, nil)
	if clients[0].Online {
		t.Fatal("a nil presence hub must decorate every client as offline")
	}
}

// TestDecorateOnline_SingleClientSlice pins GetStatsClientDetail's fix: it
// used to never decorate Online at all (always false, even for a client
// ListStatsClients reported online at the same instant - a lying field).
// GetStatsClientDetail now calls decorateOnline on a one-element slice, the
// exact pattern exercised here, for both an online and an offline client -
// a full HTTP-level test would need a live/mocked *sqlx.DB (GetContext for
// the client row, SelectContext for its events), which this package's tests
// don't otherwise set up; decorateOnline is the actual decoration logic and
// is what both handlers now share.
func TestDecorateOnline_SingleClientSlice(t *testing.T) {
	presence := presencehub.New(presencehub.DefaultGraceDuration)
	tok, _ := presence.Connect(42)
	defer presence.Disconnect(tok)

	online := []models.UpdaterClient{{ID: 42}}
	decorateOnline(online, presence)
	if !online[0].Online {
		t.Fatal("client 42 should decorate Online=true (connected) via the one-element-slice path")
	}

	offline := []models.UpdaterClient{{ID: 99}}
	decorateOnline(offline, presence)
	if offline[0].Online {
		t.Fatal("client 99 should decorate Online=false (never connected) via the one-element-slice path")
	}
}
