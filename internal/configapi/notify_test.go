package configapi

import "testing"

type recordingNotifier struct{ revisions []int64 }

func (r *recordingNotifier) NotifyConfigPublished(rev int64) { r.revisions = append(r.revisions, rev) }

func TestNotifyPublishedToleratesNil(t *testing.T) {
	notifyPublished(nil, 5) // must not panic
}

func TestNotifyPublishedForwards(t *testing.T) {
	r := &recordingNotifier{}
	notifyPublished(r, 44)
	if len(r.revisions) != 1 || r.revisions[0] != 44 {
		t.Fatalf("got %v", r.revisions)
	}
}
