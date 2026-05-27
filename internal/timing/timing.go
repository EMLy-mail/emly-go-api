package timing

import (
	"context"
	"time"
)

type contextKey struct{}

// Checkpoint is a named point in time recorded during request processing.
type Checkpoint struct {
	Name string
	At   time.Time
}

// Timer records the request start time and named checkpoints.
type Timer struct {
	Start       time.Time
	checkpoints []Checkpoint
}

// Mark records a checkpoint with the given name.
func (t *Timer) Mark(name string) {
	t.checkpoints = append(t.checkpoints, Checkpoint{Name: name, At: time.Now()})
}

// Checkpoints returns all recorded checkpoints in order.
func (t *Timer) Checkpoints() []Checkpoint {
	return t.checkpoints
}

// NewContext attaches a new Timer to ctx and returns both.
func NewContext(ctx context.Context) (context.Context, *Timer) {
	t := &Timer{Start: time.Now()}
	return context.WithValue(ctx, contextKey{}, t), t
}

// FromContext retrieves the Timer from ctx, or nil if not present.
func FromContext(ctx context.Context) *Timer {
	t, _ := ctx.Value(contextKey{}).(*Timer)
	return t
}

// Mark records a checkpoint in the Timer stored in ctx, if any.
// It is a no-op when ctx carries no Timer (e.g. in tests).
func Mark(ctx context.Context, name string) {
	if t := FromContext(ctx); t != nil {
		t.Mark(name)
	}
}
