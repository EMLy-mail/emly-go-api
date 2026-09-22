package handlers

import (
	"testing"
	"time"

	"emly-api-go/internal/models"
	"emly-api-go/internal/statshub"
)

// These exercise the coalescing state of a /v2/stats/stream connection - what
// noteHubEvent records and what takePending hands to flush. That split is the
// whole point of the design (see wsCoalesceWindow): marking must stay
// DB-free, which is also what lets these run with a nil websocket and a nil
// database.

// subscribedConn returns a connection subscribed to the given channels, with
// an events window wide enough that any event timestamped "now" falls inside
// it.
func subscribedConn(channels ...string) *wsConn {
	cn := newWSConn(nil, nil)
	for _, c := range channels {
		cn.sub.channels[c] = true
	}
	return cn
}

func updaterEvent(clientID int, hostname string) statshub.Event {
	return statshub.Event{
		Kind:   statshub.EventKindUpdaterEvent,
		Client: &models.UpdaterClient{ID: clientID, Hostname: hostname},
		EventEntry: &models.UpdaterEvent{
			ClientID:  clientID,
			EventType: "manifest_check",
			Product:   "emly",
			CreatedAt: time.Now().UTC(),
		},
	}
}

func TestNoteHubEventCoalescesABurstIntoOneRefresh(t *testing.T) {
	cn := subscribedConn(channelSummary, channelEvents)

	for i := 0; i < 50; i++ {
		cn.noteHubEvent(updaterEvent(1, "pc-01"))
	}

	p, any := cn.takePending()
	if !any {
		t.Fatal("expected pending work after 50 events")
	}
	if !p.summary || !p.events {
		t.Fatalf("expected summary and events marked stale, got summary=%v events=%v", p.summary, p.events)
	}

	// The point of the exercise: 50 ingested rows owe exactly one recompute
	// per channel, not 50.
	if _, any := cn.takePending(); any {
		t.Fatal("takePending left work behind; a burst must drain in one flush")
	}
}

func TestNoteHubEventKeepsOnlyTheLatestRowPerClient(t *testing.T) {
	cn := subscribedConn(channelClients)

	cn.noteHubEvent(updaterEvent(7, "old-name"))
	cn.noteHubEvent(updaterEvent(7, "new-name"))
	cn.noteHubEvent(updaterEvent(9, "other"))

	p, any := cn.takePending()
	if !any {
		t.Fatal("expected pending client deltas")
	}
	if len(p.clients) != 2 {
		t.Fatalf("expected one entry per distinct client, got %d", len(p.clients))
	}
	if got := p.clients[7].Hostname; got != "new-name" {
		t.Fatalf("expected the latest row for client 7, got hostname %q", got)
	}
}

func TestNoteHubEventMarksNothingForUnsubscribedChannels(t *testing.T) {
	// Subscribed to clients only: an ingested event must not queue a summary
	// or events recompute nobody asked for.
	cn := subscribedConn(channelClients)

	cn.noteHubEvent(updaterEvent(1, "pc-01"))

	p, _ := cn.takePending()
	if p.summary || p.events {
		t.Fatalf("expected no aggregate recompute, got summary=%v events=%v", p.summary, p.events)
	}
	if len(p.clients) != 1 {
		t.Fatalf("expected the client delta to still be queued, got %d", len(p.clients))
	}
}

func TestNoteHubEventIgnoresEventsOutsideTheSubscriptionFilter(t *testing.T) {
	cn := subscribedConn(channelSummary, channelEvents)
	// A window that closed yesterday: the summary still goes stale (it has a
	// window of its own), but the events chart cannot change.
	cn.sub.eventsFrom = time.Now().UTC().AddDate(0, 0, -3)
	cn.sub.eventsTo = time.Now().UTC().AddDate(0, 0, -1)

	cn.noteHubEvent(updaterEvent(1, "pc-01"))

	p, _ := cn.takePending()
	if p.events {
		t.Fatal("expected no events recompute for an event outside the window")
	}
	if !p.summary {
		t.Fatal("expected the summary to still be marked stale")
	}
}

func TestNoteTickAsksForAClientsResync(t *testing.T) {
	cn := subscribedConn(channelSummary, channelClients)

	cn.noteHubEvent(updaterEvent(1, "pc-01"))
	cn.noteHubEvent(statshub.Event{Kind: statshub.EventKindTick})

	p, _ := cn.takePending()
	if !p.clientsSnapshot {
		t.Fatal("expected the tick to request a full clients snapshot")
	}
	if !p.summary {
		t.Fatal("expected the tick to mark the summary stale")
	}
	// flush prefers the snapshot over the deltas; the deltas staying queued
	// alongside it is fine, and is what keeps noteHubEvent branch-free.
	if len(p.clients) != 1 {
		t.Fatalf("expected the client delta to survive alongside the snapshot request, got %d", len(p.clients))
	}
}

func TestTakePendingReportsNothingOwedOnAQuietConnection(t *testing.T) {
	cn := subscribedConn(channelSummary, channelClients, channelEvents)

	if _, any := cn.takePending(); any {
		t.Fatal("a connection that saw no events must owe nothing")
	}
}

// TestNoteHubEventIsSafeUnderConcurrentFlush covers the lock discipline the
// design depends on: the hub loop marks while a flush drains, and a subscribe
// arriving on the read loop rewrites the subscription set underneath both.
// Worth having because takePending deliberately does not hold pendingMu while
// flush queries. Run with -race for it to mean anything.
func TestNoteHubEventIsSafeUnderConcurrentFlush(t *testing.T) {
	cn := subscribedConn(channelSummary, channelClients, channelEvents)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			cn.noteHubEvent(updaterEvent(i%5, "pc"))
		}
	}()

	for i := 0; i < 500; i++ {
		cn.takePending()
		cn.subSnapshot()
	}
	<-done
}

func TestQuantizeCollapsesTimestampsWithinAStep(t *testing.T) {
	base := time.Date(2026, 9, 21, 16, 39, 8, 0, time.UTC)

	a := quantize(base, 30*time.Second)
	b := quantize(base.Add(7*time.Second), 30*time.Second)
	if !a.Equal(b) {
		t.Fatalf("expected timestamps 7s apart to share a 30s step, got %v and %v", a, b)
	}

	c := quantize(base.Add(31*time.Second), 30*time.Second)
	if a.Equal(c) {
		t.Fatal("expected timestamps more than a step apart to land on different keys")
	}

	// A disabled cache must not rewrite the window it was handed.
	if got := quantize(base, 0); !got.Equal(base) {
		t.Fatalf("expected a non-positive step to pass the time through, got %v", got)
	}
}
