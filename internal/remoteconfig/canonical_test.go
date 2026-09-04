package remoteconfig

import (
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
