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
