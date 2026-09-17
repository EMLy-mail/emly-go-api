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

// TestConnectOnNilHubDoesNotPanic guards the fix for a live nil-deref trap:
// several doc comments (routes/v2/client.go, routes/v2/v2.go, routes.go)
// claim presence being nil is fine because "presencehub.Hub's own methods
// tolerate that" - Connect must actually live up to that, not just Online.
func TestConnectOnNilHubDoesNotPanic(t *testing.T) {
	var h *Hub
	tok, supersede := h.Connect(1)
	if tok != (Token{}) {
		t.Fatalf("Connect on a nil *Hub returned a non-zero Token: %+v", tok)
	}
	if supersede != nil {
		t.Fatal("Connect on a nil *Hub returned a non-nil supersede channel")
	}
	// A nil channel in a select simply never fires - confirm it doesn't
	// panic or immediately report ready.
	select {
	case <-supersede:
		t.Fatal("nil supersede channel fired")
	default:
	}
}

// TestDisconnectOnNilHubDoesNotPanic guards the other half of the same fix.
func TestDisconnectOnNilHubDoesNotPanic(t *testing.T) {
	var h *Hub
	h.Disconnect(Token{})
	h.Disconnect(Token{clientID: 1, gen: 1})
}

// TestDisconnectWithZeroTokenDoesNotPanic checks a zero-value Token (what a
// nil Hub's Connect returns) is a safe no-op even against a real Hub.
func TestDisconnectWithZeroTokenDoesNotPanic(t *testing.T) {
	h := New(testGrace)
	h.Disconnect(Token{})
	if h.Online(1) {
		t.Fatal("a zero-value Token Disconnect must not mark anything online")
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
