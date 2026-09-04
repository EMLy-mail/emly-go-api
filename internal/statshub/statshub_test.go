package statshub

import (
	"context"
	"testing"
	"time"
)

const testTimeout = 2 * time.Second

// TestSubscribePublish checks the basic fan-out: a subscriber receives an
// event Publish sends after it subscribed.
func TestSubscribePublish(t *testing.T) {
	h := New()
	ch, cancel := h.Subscribe()
	defer cancel()

	h.Publish(Event{Kind: EventKindTick})

	select {
	case ev := <-ch:
		if ev.Kind != EventKindTick {
			t.Fatalf("got Kind %q, want %q", ev.Kind, EventKindTick)
		}
	case <-time.After(testTimeout):
		t.Fatal("subscriber never received the published event")
	}
}

// TestActiveReflectsSubscriberCount checks that Active() - the check
// recordUpdaterEvent uses to skip its extra client-row fetch when nobody is
// listening - tracks subscribe/unsubscribe accurately.
func TestActiveReflectsSubscriberCount(t *testing.T) {
	h := New()
	if h.Active() {
		t.Fatal("Active() = true before any Subscribe")
	}

	_, cancel1 := h.Subscribe()
	if !h.Active() {
		t.Fatal("Active() = false with one subscriber")
	}

	_, cancel2 := h.Subscribe()
	cancel1()
	if !h.Active() {
		t.Fatal("Active() = false with one subscriber still registered")
	}

	cancel2()
	if h.Active() {
		t.Fatal("Active() = true after every subscriber unsubscribed")
	}
}

// TestPublishDoesNotBlockOnAFullSubscriber pins down the design doc §7
// contract: this is a best-effort refresh channel, so a subscriber that
// isn't draining its buffer must never make Publish (called synchronously
// from the updater-event ingest path) block or fail for anyone else.
func TestPublishDoesNotBlockOnAFullSubscriber(t *testing.T) {
	h := New()
	_, cancel := h.Subscribe() // never drained
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < subscriberBuffer+10; i++ {
			h.Publish(Event{Kind: EventKindTick})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("Publish blocked on a full, undrained subscriber channel")
	}
}

// TestSubscribeIsolation checks that one subscriber unsubscribing doesn't
// affect another still-registered one, and that a cancelled subscriber's
// channel receives nothing further.
func TestSubscribeIsolation(t *testing.T) {
	h := New()
	chA, cancelA := h.Subscribe()
	chB, cancelB := h.Subscribe()
	defer cancelB()

	cancelA()
	h.Publish(Event{Kind: EventKindTick})

	select {
	case <-chB:
	case <-time.After(testTimeout):
		t.Fatal("remaining subscriber never received the published event")
	}

	select {
	case ev, ok := <-chA:
		if ok {
			t.Fatalf("unsubscribed channel received an event: %+v", ev)
		}
	case <-time.After(50 * time.Millisecond):
		// Expected: nothing arrives for the unsubscribed channel.
	}
}

// TestRunTicksUntilCancelled checks Hub.Run's periodic EventKindTick (design
// doc §6.2) fires on the given interval and stops once its context is done.
func TestRunTicksUntilCancelled(t *testing.T) {
	h := New()
	ch, cancelSub := h.Subscribe()
	defer cancelSub()

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		h.Run(ctx, 20*time.Millisecond)
		close(runDone)
	}()

	select {
	case ev := <-ch:
		if ev.Kind != EventKindTick {
			t.Fatalf("got Kind %q, want %q", ev.Kind, EventKindTick)
		}
	case <-time.After(testTimeout):
		t.Fatal("Run never published a tick")
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(testTimeout):
		t.Fatal("Run did not return after its context was cancelled")
	}
}
