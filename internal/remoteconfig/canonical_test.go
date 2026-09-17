package remoteconfig

import (
	"strings"
	"testing"
)

func TestCanonical_KeyOrderIndependence(t *testing.T) {
	a := `{"schemaVersion":1,"revision":5,"generatedAt":"2026-09-04T10:00:00Z","servers":{"a":"https://a.example.com","b":"https://b.example.com"},"defaultServer":"a"}`
	b := `{"servers":{"b":"https://b.example.com","a":"https://a.example.com"},"defaultServer":"a","generatedAt":"2026-09-04T10:00:00Z","revision":5,"schemaVersion":1}`

	docA, problemsA := Parse([]byte(a))
	if len(problemsA) != 0 {
		t.Fatalf("doc A invalid: %+v", problemsA)
	}
	docB, problemsB := Parse([]byte(b))
	if len(problemsB) != 0 {
		t.Fatalf("doc B invalid: %+v", problemsB)
	}

	bytesA, etagA := Canonical(docA)
	bytesB, etagB := Canonical(docB)

	if string(bytesA) != string(bytesB) {
		t.Fatalf("canonical bytes differ:\nA: %s\nB: %s", bytesA, bytesB)
	}
	if etagA != etagB {
		t.Fatalf("etags differ: %s vs %s", etagA, etagB)
	}
}

func TestCanonical_Idempotent(t *testing.T) {
	doc, problems := Parse([]byte(minimalValidDocJSON()))
	if len(problems) != 0 {
		t.Fatalf("invalid: %+v", problems)
	}
	b1, e1 := Canonical(doc)
	b2, e2 := Canonical(doc)
	if string(b1) != string(b2) || e1 != e2 {
		t.Fatalf("Canonical is not idempotent")
	}
}

// TestCanonical_OmitsNilClientWS pins down the fix for the site-mirror
// replication break: ClientWS was added to Document after Canonical/ETag
// already existed in the wild, tagged `json:"clientWs,omitempty"` precisely
// so a document that never sets it canonicalizes exactly as it would have
// before the field existed. minimalValidDocJSON predates ClientWS (no
// "clientWs" key at all), so it stands in for an old document here; the
// regression this guards against is a future optional field losing its
// `omitempty` and silently changing the ETag of every document that never
// touches it - which is exactly what broke internal/configmirror's ETag
// comparison on every site mirror the first time either side of a mirror
// pair upgraded, regardless of upgrade order.
func TestCanonical_OmitsNilClientWS(t *testing.T) {
	doc, problems := Parse([]byte(minimalValidDocJSON()))
	if len(problems) != 0 {
		t.Fatalf("invalid: %+v", problems)
	}
	if doc.ClientWS != nil {
		t.Fatalf("expected ClientWS to be nil when absent from the source document, got %+v", doc.ClientWS)
	}

	b, _ := Canonical(doc)
	if strings.Contains(string(b), "clientWs") {
		t.Fatalf("canonical bytes contain \"clientWs\" for a document that never set it: %s", b)
	}
}

func TestCanonical_NoTrailingNewlineOrIndent(t *testing.T) {
	doc, _ := Parse([]byte(minimalValidDocJSON()))
	b, _ := Canonical(doc)
	if len(b) == 0 {
		t.Fatal("empty canonical output")
	}
	if b[len(b)-1] == '\n' {
		t.Fatal("canonical output has a trailing newline")
	}
	for _, c := range b {
		if c == '\n' {
			t.Fatal("canonical output is indented (contains a newline)")
		}
	}
}
