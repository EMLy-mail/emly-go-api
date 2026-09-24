package clienthub

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"emly-api-go/internal/clientproto"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type capture struct {
	mu     sync.Mutex
	frames []clientproto.Envelope
}

func (c *capture) send(_ context.Context, frame []byte) error {
	var env clientproto.Envelope
	if err := json.Unmarshal(frame, &env); err != nil {
		return err
	}
	c.mu.Lock()
	c.frames = append(c.frames, env)
	c.mu.Unlock()
	return nil
}

func (c *capture) last(t *testing.T) clientproto.Envelope {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.frames) == 0 {
		t.Fatal("nothing sent")
	}
	return c.frames[len(c.frames)-1]
}

var allCaps = clientproto.ServerCapabilities

func newHub() (*Hub, *fakeClock) {
	clk := &fakeClock{t: time.Date(2026, 9, 23, 8, 0, 0, 0, time.UTC)}
	return New(clk.now), clk
}

func TestHubIssueSendsCommand(t *testing.T) {
	h, _ := newHub()
	c := &capture{}
	h.Attach(7, clientproto.ProtocolV2, allCaps, c.send)

	rec, err := h.Issue(context.Background(), 7, clientproto.CmdMachineInfo, nil, 10*time.Minute, "admin")
	if err != nil {
		t.Fatal(err)
	}
	env := c.last(t)
	if env.Type != clientproto.TypeCommand || env.ID != rec.ID || rec.Status != StatusSent {
		t.Fatalf("env=%+v rec=%+v", env, rec)
	}
	var cmd clientproto.Command
	_ = json.Unmarshal(env.Data, &cmd)
	if cmd.Name != clientproto.CmdMachineInfo || cmd.ExpiresAt != "2026-09-23T08:10:00Z" || cmd.IssuedBy != "admin" {
		t.Fatalf("cmd = %+v", cmd)
	}
}

// TestHubIssueSnapshotDoesNotRaceWithConcurrentAck exercises the window
// between Issue's unlocked send and its final read of rec: the ack for this
// very command can arrive (and mutate the same *CommandRecord under lock)
// before Issue takes its own lock to snapshot it. Run with -race; a bare
// `return h.snapshot(rec), nil` without re-acquiring h.mu after send
// triggers a data race here.
func TestHubIssueSnapshotDoesNotRaceWithConcurrentAck(t *testing.T) {
	h, _ := newHub()
	var wg sync.WaitGroup
	send := func(_ context.Context, frame []byte) error {
		var env clientproto.Envelope
		if err := json.Unmarshal(frame, &env); err != nil {
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.HandleAck(7, env.ID, clientproto.Ack{Accepted: true})
		}()
		return nil
	}
	h.Attach(7, clientproto.ProtocolV2, allCaps, send)

	rec, err := h.Issue(context.Background(), 7, clientproto.CmdMachineInfo, nil, time.Hour, "")
	if err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	if got, ok := h.Command(rec.ID); !ok || got.Status != StatusAcked || got.AckedAt == nil {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
}

func TestHubIssueErrors(t *testing.T) {
	h, _ := newHub()
	ctx := context.Background()
	if _, err := h.Issue(ctx, 1, clientproto.CmdMachineInfo, nil, time.Minute, ""); !errors.Is(err, ErrOffline) {
		t.Errorf("offline: %v", err)
	}
	h.Attach(2, clientproto.ProtocolV1, nil, (&capture{}).send)
	if _, err := h.Issue(ctx, 2, clientproto.CmdMachineInfo, nil, time.Minute, ""); !errors.Is(err, ErrUnsupported) {
		t.Errorf("v1 client: %v", err)
	}
	h.Attach(3, clientproto.ProtocolV2, []string{clientproto.CmdMachineInfo}, (&capture{}).send)
	if _, err := h.Issue(ctx, 3, clientproto.CmdMachineReboot, nil, time.Minute, ""); !errors.Is(err, ErrUnsupported) {
		t.Errorf("capability not declared: %v", err)
	}
	var nilHub *Hub
	if _, err := nilHub.Issue(ctx, 3, clientproto.CmdMachineInfo, nil, time.Minute, ""); !errors.Is(err, ErrOffline) {
		t.Errorf("nil hub: %v", err)
	}
}

func TestHubAckThenResult(t *testing.T) {
	h, clk := newHub()
	h.Attach(7, clientproto.ProtocolV2, allCaps, (&capture{}).send)
	rec, _ := h.Issue(context.Background(), 7, clientproto.CmdAppsListUpgradable, nil, time.Hour, "")

	clk.add(time.Second)
	h.HandleAck(7, rec.ID, clientproto.Ack{Accepted: true})
	if got, _ := h.Command(rec.ID); got.Status != StatusAcked || got.AckedAt == nil {
		t.Fatalf("after ack: %+v", got)
	}
	h.HandleResult(7, rec.ID, clientproto.Result{Status: clientproto.ResultOK, Payload: json.RawMessage(`{"packages":[]}`)})
	got, _ := h.Command(rec.ID)
	if got.Status != StatusDone || got.Result == nil || got.FinishedAt == nil {
		t.Fatalf("after result: %+v", got)
	}
}

func TestHubRejectedAckAndErrorResult(t *testing.T) {
	h, _ := newHub()
	h.Attach(7, clientproto.ProtocolV2, allCaps, (&capture{}).send)
	a, _ := h.Issue(context.Background(), 7, clientproto.CmdMachineReboot, nil, time.Hour, "")
	h.HandleAck(7, a.ID, clientproto.Ack{Accepted: false, Error: &clientproto.ErrorBody{Code: clientproto.ErrUserActive}})
	if got, _ := h.Command(a.ID); got.Status != StatusRejected || got.Error.Code != clientproto.ErrUserActive {
		t.Fatalf("rejected: %+v", got)
	}
	b, _ := h.Issue(context.Background(), 7, clientproto.CmdEMLyManifestCheck, nil, time.Hour, "")
	h.HandleAck(7, b.ID, clientproto.Ack{Accepted: true})
	h.HandleResult(7, b.ID, clientproto.Result{Status: clientproto.ResultError, Error: &clientproto.ErrorBody{Code: clientproto.ErrBusy}})
	if got, _ := h.Command(b.ID); got.Status != StatusFailed || got.Error.Code != clientproto.ErrBusy {
		t.Fatalf("failed: %+v", got)
	}
}

func TestHubIgnoresAckFromAnotherClient(t *testing.T) {
	h, _ := newHub()
	h.Attach(7, clientproto.ProtocolV2, allCaps, (&capture{}).send)
	h.Attach(8, clientproto.ProtocolV2, allCaps, (&capture{}).send)
	rec, _ := h.Issue(context.Background(), 7, clientproto.CmdMachineInfo, nil, time.Hour, "")
	h.HandleAck(8, rec.ID, clientproto.Ack{Accepted: true})
	h.HandleResult(8, rec.ID, clientproto.Result{Status: clientproto.ResultOK})
	if got, _ := h.Command(rec.ID); got.Status != StatusSent {
		t.Fatalf("another client's ack/result changed the command: %+v", got)
	}
}

func TestHubAckTimeout(t *testing.T) {
	h, clk := newHub()
	h.Attach(7, clientproto.ProtocolV2, allCaps, (&capture{}).send)
	rec, _ := h.Issue(context.Background(), 7, clientproto.CmdMachineInfo, nil, time.Hour, "")
	clk.add(ackGrace + time.Second)
	got, _ := h.Command(rec.ID)
	if got.Status != StatusTimeout || got.Error == nil || got.Error.Code != clientproto.ErrTimeout {
		t.Fatalf("got %+v", got)
	}
}

func TestHubResultTimeout(t *testing.T) {
	h, clk := newHub()
	h.Attach(7, clientproto.ProtocolV2, allCaps, (&capture{}).send)
	rec, _ := h.Issue(context.Background(), 7, clientproto.CmdMachineInfo, nil, time.Hour, "")
	h.HandleAck(7, rec.ID, clientproto.Ack{Accepted: true})
	clk.add(29 * time.Second)
	if got, _ := h.Command(rec.ID); got.Status != StatusAcked {
		t.Fatalf("too early: %+v", got)
	}
	clk.add(2 * time.Second)
	if got, _ := h.Command(rec.ID); got.Status != StatusTimeout {
		t.Fatalf("after 31s: %+v", got)
	}
}

func TestHubCompleteByRestart(t *testing.T) {
	h, clk := newHub()
	detach := h.Attach(7, clientproto.ProtocolV2, allCaps, (&capture{}).send)
	rec, _ := h.Issue(context.Background(), 7, clientproto.CmdMachineReboot,
		json.RawMessage(`{"delay_seconds":60}`), time.Hour, "")
	h.HandleAck(7, rec.ID, clientproto.Ack{Accepted: true})
	detach()
	clk.add(3 * time.Minute)
	h.Attach(7, clientproto.ProtocolV2, allCaps, (&capture{}).send)
	h.CompleteByRestart(7, []string{rec.ID, "unknown-id"})
	if got, _ := h.Command(rec.ID); got.Status != StatusDone {
		t.Fatalf("got %+v", got)
	}
}

func TestHubRebootTimesOutWithoutRestart(t *testing.T) {
	h, clk := newHub()
	h.Attach(7, clientproto.ProtocolV2, allCaps, (&capture{}).send)
	rec, _ := h.Issue(context.Background(), 7, clientproto.CmdMachineReboot,
		json.RawMessage(`{"delay_seconds":60}`), time.Hour, "")
	h.HandleAck(7, rec.ID, clientproto.Ack{Accepted: true})
	clk.add(60*time.Second + 15*time.Minute + time.Second)
	if got, _ := h.Command(rec.ID); got.Status != StatusTimeout {
		t.Fatalf("got %+v", got)
	}
}

func TestHubDetachOfSupersededSessionKeepsNewOne(t *testing.T) {
	h, _ := newHub()
	oldDetach := h.Attach(7, clientproto.ProtocolV2, allCaps, (&capture{}).send)
	fresh := &capture{}
	h.Attach(7, clientproto.ProtocolV2, allCaps, fresh.send)
	oldDetach()
	if _, err := h.Issue(context.Background(), 7, clientproto.CmdMachineInfo, nil, time.Minute, ""); err != nil {
		t.Fatalf("new session lost by old detach: %v", err)
	}
	if fresh.last(t).Type != clientproto.TypeCommand {
		t.Fatal("command did not reach the new session")
	}
}

func TestHubEventsRingKeepsLast50(t *testing.T) {
	h, _ := newHub()
	for i := 0; i < 60; i++ {
		h.RecordEvent(EventRecord{ClientID: 7, Name: clientproto.EvtSessionChanged, ID: clientproto.NewID()})
	}
	h.RecordEvent(EventRecord{ClientID: 8, Name: clientproto.EvtMachineInfo})
	if got := len(h.Events(7)); got != eventRingSize {
		t.Fatalf("len = %d", got)
	}
	if got := len(h.Events(8)); got != 1 {
		t.Fatalf("other client len = %d", got)
	}
}

func TestHubNotifyOnlyV2WithCapability(t *testing.T) {
	h, _ := newHub()
	v2 := &capture{}
	v2NoCap := &capture{}
	v1 := &capture{}
	h.Attach(1, clientproto.ProtocolV2, allCaps, v2.send)
	h.Attach(2, clientproto.ProtocolV2, []string{clientproto.CmdMachineInfo}, v2NoCap.send)
	h.Attach(3, clientproto.ProtocolV1, nil, v1.send)
	n := h.Notify(context.Background(), nil, clientproto.TopicConfigPublished,
		clientproto.ConfigPublished{Revision: 44, JitterSeconds: 120})
	if n != 1 || len(v2.frames) != 1 || len(v2NoCap.frames) != 0 || len(v1.frames) != 0 {
		t.Fatalf("sent=%d v2=%d v2NoCap=%d v1=%d", n, len(v2.frames), len(v2NoCap.frames), len(v1.frames))
	}
	if n := h.Notify(context.Background(), []int64{2, 3}, clientproto.TopicConfigPublished, nil); n != 0 {
		t.Fatalf("targeted notify reached %d clients without the capability", n)
	}
}

// TestHubNotifyFansOutConcurrently proves Notify dispatches to its targets
// in parallel rather than one at a time: three sessions each block ~200ms in
// their Sender, so a sequential fan-out would take ~600ms+ but a concurrent
// one bounded by a single sendTimeout should return in a small multiple of
// 200ms, not three of them.
func TestHubNotifyFansOutConcurrently(t *testing.T) {
	h, _ := newHub()
	const delay = 200 * time.Millisecond
	slow := func(_ context.Context, _ []byte) error {
		time.Sleep(delay)
		return nil
	}
	h.Attach(1, clientproto.ProtocolV2, allCaps, slow)
	h.Attach(2, clientproto.ProtocolV2, allCaps, slow)
	h.Attach(3, clientproto.ProtocolV2, allCaps, slow)

	start := time.Now()
	n := h.Notify(context.Background(), nil, clientproto.TopicConfigPublished,
		clientproto.ConfigPublished{Revision: 1, JitterSeconds: 120})
	elapsed := time.Since(start)

	if n != 3 {
		t.Fatalf("sent = %d, want 3", n)
	}
	// Sequential would be >= 3*delay (600ms); concurrent should land close
	// to one delay. Give generous scheduling slack while staying well under
	// what a sequential fan-out would take.
	if elapsed >= 2*delay {
		t.Fatalf("Notify took %v for 3 targets at %v each; fan-out does not look concurrent", elapsed, delay)
	}
}

// TestHubNotifyConfigPublishedReturnsImmediately proves the config.published
// fan-out never makes the caller (a config publish/rollback HTTP handler)
// wait on a client socket: NotifyConfigPublished must return long before the
// blocked Sender is unblocked, and the send must still eventually happen.
func TestHubNotifyConfigPublishedReturnsImmediately(t *testing.T) {
	h, _ := newHub()
	unblock := make(chan struct{})
	sent := make(chan struct{})
	send := func(_ context.Context, _ []byte) error {
		<-unblock
		close(sent)
		return nil
	}
	h.Attach(1, clientproto.ProtocolV2, allCaps, send)

	start := time.Now()
	h.NotifyConfigPublished(7)
	elapsed := time.Since(start)
	if elapsed > 50*time.Millisecond {
		t.Fatalf("NotifyConfigPublished took %v, want near-immediate return", elapsed)
	}

	select {
	case <-sent:
		t.Fatal("send completed before the sender was unblocked")
	default:
	}

	close(unblock)
	select {
	case <-sent:
	case <-time.After(2 * time.Second):
		t.Fatal("send never happened after unblocking")
	}
}

// TestHubNotifyConfigPublishedNilHubIsNoOp guards the nil-receiver path: it
// must not panic and must not spawn anything observable.
func TestHubNotifyConfigPublishedNilHubIsNoOp(t *testing.T) {
	var nilHub *Hub
	nilHub.NotifyConfigPublished(1) // must not panic
}

// TestHubLateAckAfterTimeoutStaysTimeout pins I3: with no intervening read
// (no h.Command call) between the ack deadline passing and the late ack
// arriving, the outcome must still be timeout, not whatever the late ack
// says - HandleAck must apply the lazy timeout itself rather than relying
// on some earlier read to have already materialized it.
func TestHubLateAckAfterTimeoutStaysTimeout(t *testing.T) {
	h, clk := newHub()
	h.Attach(7, clientproto.ProtocolV2, allCaps, (&capture{}).send)
	rec, _ := h.Issue(context.Background(), 7, clientproto.CmdMachineInfo, nil, time.Hour, "")
	clk.add(ackGrace + time.Second)
	h.HandleAck(7, rec.ID, clientproto.Ack{Accepted: true})
	if got, _ := h.Command(rec.ID); got.Status != StatusTimeout {
		t.Fatalf("late ack overrode timeout: %+v", got)
	}
}

// TestHubLateResultAfterTimeoutStaysTimeout is the HandleResult half of I3:
// a result that arrives after the post-ack result timeout, with nothing
// reading the record in between, must not flip an already-due timeout to
// done/failed.
func TestHubLateResultAfterTimeoutStaysTimeout(t *testing.T) {
	h, clk := newHub()
	h.Attach(7, clientproto.ProtocolV2, allCaps, (&capture{}).send)
	rec, _ := h.Issue(context.Background(), 7, clientproto.CmdMachineInfo, nil, time.Hour, "")
	h.HandleAck(7, rec.ID, clientproto.Ack{Accepted: true})
	clk.add(31 * time.Second) // machine.info's result timeout is 30s from the ack
	h.HandleResult(7, rec.ID, clientproto.Result{Status: clientproto.ResultOK})
	if got, _ := h.Command(rec.ID); got.Status != StatusTimeout {
		t.Fatalf("late result overrode timeout: %+v", got)
	}
}

// TestHubIssueSendContextSurvivesCallerCancellation pins M3: cancelling the
// context Issue was called with (an admin HTTP request's context aborting
// or timing out) must not cancel the context handed to the Sender, or
// coder/websocket.Conn.Write would close the whole client connection out
// from under an otherwise-healthy socket.
func TestHubIssueSendContextSurvivesCallerCancellation(t *testing.T) {
	h, _ := newHub()
	ctx, cancel := context.WithCancel(context.Background())
	var sctxDone bool
	done := make(chan struct{})
	send := func(sctx context.Context, _ []byte) error {
		cancel() // cancel the caller's context from inside the send itself
		select {
		case <-sctx.Done():
			sctxDone = true
		case <-time.After(100 * time.Millisecond):
		}
		close(done)
		return nil
	}
	h.Attach(7, clientproto.ProtocolV2, allCaps, send)
	if _, err := h.Issue(ctx, 7, clientproto.CmdMachineInfo, nil, time.Minute, ""); err != nil {
		t.Fatal(err)
	}
	<-done
	if sctxDone {
		t.Fatal("send's context was cancelled by the caller's context cancellation")
	}
}

// TestHubNotifySendContextSurvivesCallerCancellation is the Notify half of
// M3, same reasoning as TestHubIssueSendContextSurvivesCallerCancellation.
func TestHubNotifySendContextSurvivesCallerCancellation(t *testing.T) {
	h, _ := newHub()
	ctx, cancel := context.WithCancel(context.Background())
	var sctxDone bool
	done := make(chan struct{})
	send := func(sctx context.Context, _ []byte) error {
		cancel()
		select {
		case <-sctx.Done():
			sctxDone = true
		case <-time.After(100 * time.Millisecond):
		}
		close(done)
		return nil
	}
	h.Attach(7, clientproto.ProtocolV2, allCaps, send)
	h.Notify(ctx, nil, clientproto.TopicConfigPublished, clientproto.ConfigPublished{Revision: 1})
	<-done
	if sctxDone {
		t.Fatal("send's context was cancelled by the caller's context cancellation")
	}
}

// TestHubRecordEventPayloadOnlyForAcceptedCapability pins I1(b): an event
// whose name the session did not declare among its accepted capabilities is
// recorded name-only - no payload, and not marked Truncated (that field is
// reserved for the size cap, a different rule).
func TestHubRecordEventPayloadOnlyForAcceptedCapability(t *testing.T) {
	h, _ := newHub()
	h.Attach(7, clientproto.ProtocolV2, []string{clientproto.CmdMachineInfo}, (&capture{}).send)

	h.RecordEvent(EventRecord{ClientID: 7, Name: clientproto.EvtServiceStarted, Payload: json.RawMessage(`{"reason":"boot"}`)})
	got := h.Events(7)
	if len(got) != 1 || got[0].Payload != nil || got[0].Truncated {
		t.Fatalf("undeclared capability: got %+v", got)
	}

	h2, _ := newHub()
	h2.Attach(8, clientproto.ProtocolV2, allCaps, (&capture{}).send)
	h2.RecordEvent(EventRecord{ClientID: 8, Name: clientproto.EvtServiceStarted, Payload: json.RawMessage(`{"reason":"boot"}`)})
	got2 := h2.Events(8)
	if len(got2) != 1 || string(got2[0].Payload) != `{"reason":"boot"}` {
		t.Fatalf("declared capability: got %+v", got2)
	}
}

// TestHubRecordEventCapsPayloadSize pins I1(a): a payload over
// eventPayloadCapBytes is dropped, not truncated to it, and the record is
// marked Truncated so a reader can tell "large" apart from "no payload
// sent".
func TestHubRecordEventCapsPayloadSize(t *testing.T) {
	h, _ := newHub()
	h.Attach(7, clientproto.ProtocolV2, allCaps, (&capture{}).send)

	big := make(json.RawMessage, eventPayloadCapBytes+1)
	for i := range big {
		big[i] = 'a'
	}
	h.RecordEvent(EventRecord{ClientID: 7, Name: clientproto.EvtServiceStarted, Payload: big})
	got := h.Events(7)
	if len(got) != 1 || got[0].Payload != nil || !got[0].Truncated {
		t.Fatalf("oversized payload: got payload_len=%d truncated=%v", len(got[0].Payload), got[0].Truncated)
	}

	small := json.RawMessage(`{"reason":"boot"}`)
	h.RecordEvent(EventRecord{ClientID: 7, Name: clientproto.EvtServiceStarted, Payload: small})
	got = h.Events(7)
	last := got[len(got)-1]
	if last.Truncated || string(last.Payload) != string(small) {
		t.Fatalf("payload at the cap: got %+v", last)
	}
}

// TestHubRecordEventCapsTotalClients pins I1(c): once h.events already
// tracks maxEventClients distinct client ids, RecordEvent for a new id
// evicts the least recently active one (the one whose newest event is
// oldest) rather than growing without bound.
func TestHubRecordEventCapsTotalClients(t *testing.T) {
	h, clk := newHub()
	for id := int64(1); id <= maxEventClients; id++ {
		h.RecordEvent(EventRecord{ClientID: id, Name: clientproto.EvtMachineInfo, ReceivedAt: clk.now()})
		clk.add(time.Millisecond)
	}
	if got := len(h.Events(1)); got != 1 {
		t.Fatalf("client 1 events = %d before eviction, want 1", got)
	}

	// One more distinct client id past the cap: client 1 (the oldest -
	// its only event has the earliest ReceivedAt of all) must be evicted.
	h.RecordEvent(EventRecord{ClientID: maxEventClients + 1, Name: clientproto.EvtMachineInfo, ReceivedAt: clk.now()})

	if got := len(h.Events(1)); got != 0 {
		t.Fatalf("client 1 (oldest) survived eviction: %d events", got)
	}
	if got := len(h.Events(maxEventClients + 1)); got != 1 {
		t.Fatalf("newest client missing after eviction: %d events", got)
	}
	// Total tracked clients stays at the cap.
	if got := len(h.events); got != maxEventClients {
		t.Fatalf("len(h.events) = %d, want %d", got, maxEventClients)
	}
}

func TestHubPruneDropsOldFinishedCommands(t *testing.T) {
	h, clk := newHub()
	h.Attach(7, clientproto.ProtocolV2, allCaps, (&capture{}).send)
	rec, _ := h.Issue(context.Background(), 7, clientproto.CmdMachineInfo, nil, time.Minute, "")
	h.HandleAck(7, rec.ID, clientproto.Ack{Accepted: true})
	h.HandleResult(7, rec.ID, clientproto.Result{Status: clientproto.ResultOK})
	clk.add(recordRetention + time.Minute)
	h.Prune()
	if _, ok := h.Command(rec.ID); ok {
		t.Fatal("finished record older than retention survived Prune")
	}
}
