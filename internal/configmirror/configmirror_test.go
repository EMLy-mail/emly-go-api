package configmirror

import "testing"

// recordingNotifier mirrors internal/configapi's own test double
// (recordingNotifier in notify_test.go) for the same ConfigNotifier shape.
type recordingNotifier struct{ revisions []int64 }

func (r *recordingNotifier) NotifyConfigPublished(rev int64) { r.revisions = append(r.revisions, rev) }

// TestNotifyIfNewToleratesNil pins that a mirror with no clienthub.Hub
// wired in (a build that never constructs one) must not panic.
func TestNotifyIfNewToleratesNil(t *testing.T) {
	notifyIfNew(nil, true, 5) // must not panic
}

// TestNotifyIfNewForwardsOnlyForNewRevision is the unit-level guard for I5:
// storeReplicatedRevision's isNew=false no-op path (a resync of a revision
// this mirror already replicated, e.g. after a restart) must not
// re-announce config.published to the site's own connected clients, while
// isNew=true (a genuinely new revision just landed) must.
func TestNotifyIfNewForwardsOnlyForNewRevision(t *testing.T) {
	r := &recordingNotifier{}

	notifyIfNew(r, false, 44)
	if len(r.revisions) != 0 {
		t.Fatalf("no-op resync notified: %v", r.revisions)
	}

	notifyIfNew(r, true, 44)
	if len(r.revisions) != 1 || r.revisions[0] != 44 {
		t.Fatalf("new revision not notified: %v", r.revisions)
	}

	// A second isNew=true call (a later, different revision) must add,
	// not replace - the notifier sees every new revision, not just the
	// last one.
	notifyIfNew(r, true, 45)
	if len(r.revisions) != 2 || r.revisions[1] != 45 {
		t.Fatalf("second new revision not notified: %v", r.revisions)
	}
}
