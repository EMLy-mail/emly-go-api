package presencehub

import (
	"testing"
	"time"
)

const testGrace = 30 * time.Millisecond

func TestConnectMarksOnline(t *testing.T) {
	h := New(testGrace)
	if h.Online(1) {
		t.Fatal("Online(1) = true before any Connect")
	}
	tok, _ := h.Connect(1)
	if !h.Online(1) {
		t.Fatal("Online(1) = false right after Connect")
	}
	h.Disconnect(tok)
}

func TestDisconnectStaysOnlineDuringGrace(t *testing.T) {
	h := New(testGrace)
	tok, _ := h.Connect(1)
	h.Disconnect(tok)
	if !h.Online(1) {
		t.Fatal("Online(1) = false immediately after Disconnect, want true during grace period")
	}
}

func TestDisconnectGoesOfflineAfterGraceExpires(t *testing.T) {
	h := New(testGrace)
	tok, _ := h.Connect(1)
	h.Disconnect(tok)
	time.Sleep(testGrace * 3)
	if h.Online(1) {
		t.Fatal("Online(1) = true after grace period expired, want false")
	}
}

func TestReconnectDuringGraceCancelsIt(t *testing.T) {
	h := New(testGrace)
	tok1, _ := h.Connect(1)
	h.Disconnect(tok1)

	tok2, _ := h.Connect(1)
	time.Sleep(testGrace * 3)
	if !h.Online(1) {
		t.Fatal("Online(1) = false after reconnecting during grace, want true")
	}
	h.Disconnect(tok2)
}

func TestNewConnectionSupersedesOld(t *testing.T) {
	h := New(testGrace)
	_, supersede1 := h.Connect(1)

	select {
	case <-supersede1:
		t.Fatal("supersede channel closed before a second Connect")
	default:
	}

	h.Connect(1)

	select {
	case <-supersede1:
		// expected: the first connection is told to close.
	case <-time.After(time.Second):
		t.Fatal("supersede channel never closed after a second Connect for the same clientID")
	}
}

func TestStaleDisconnectIsNoop(t *testing.T) {
	h := New(testGrace)
	tok1, _ := h.Connect(1)
	h.Connect(1) // supersedes tok1

	h.Disconnect(tok1) // stale: must not touch the new connection's entry
	if !h.Online(1) {
		t.Fatal("Online(1) = false after a stale Disconnect from a superseded connection")
	}
}

func TestOnlineOnNilHub(t *testing.T) {
	var h *Hub
	if h.Online(1) {
		t.Fatal("Online(1) on a nil *Hub = true, want false")
	}
}

func TestDifferentClientsAreIndependent(t *testing.T) {
	h := New(testGrace)
	tok1, _ := h.Connect(1)
	h.Connect(2)

	if !h.Online(1) || !h.Online(2) {
		t.Fatal("both clients should be online")
	}
	h.Disconnect(tok1)
	time.Sleep(testGrace * 3)
	if h.Online(1) {
		t.Fatal("client 1 should be offline after its own grace period")
	}
	if !h.Online(2) {
		t.Fatal("client 2 must stay online, unaffected by client 1's disconnect")
	}
}
