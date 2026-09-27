package configapi

import (
	"net/http/httptest"
	"testing"

	"emly-api-go/internal/updaterclient"
)

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

// Same guarantee as updaterclient's TestRecordEventSkipsTestTraffic, for the
// config fetch path: a nil DB would panic on the upsert.
func TestTrackConfigFetchSkipsTestTraffic(t *testing.T) {
	r := httptest.NewRequest("GET", "/v2/config", nil)
	r.Header.Set("X-EMLy-HWID", "K6-0001")
	r.Header.Set("X-EMLy-Hostname", "K6-PC-0001")
	r.Header.Set(updaterclient.TestingHeader, "1")

	trackConfigFetch(r, nil, 7)
}
