package handlers

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
