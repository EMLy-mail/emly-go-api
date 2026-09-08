package remoteconfig

import "testing"

func TestUnknownFieldPaths_TopLevel(t *testing.T) {
	paths, err := UnknownFieldPaths([]byte(`{"schemaVersion":1,"servers":{},"defaultServer":"a","bogus":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !containsFold(paths, "/bogus") {
		t.Fatalf("expected /bogus in %v", paths)
	}
}

func TestUnknownFieldPaths_Nested(t *testing.T) {
	paths, err := UnknownFieldPaths([]byte(`{
		"schemaVersion":1,"servers":{},"defaultServer":"a",
		"updater": {"pollIntervalMinutes": 5, "certficate": {"enabled": true}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if !containsFold(paths, "/updater/certficate") {
		t.Fatalf("expected /updater/certficate in %v", paths)
	}
}

func TestUnknownFieldPaths_NoFalsePositivesOnDynamicKeys(t *testing.T) {
	paths, err := UnknownFieldPaths([]byte(`{
		"schemaVersion":1,
		"servers":{"srv-a":"https://a.example.com","srv-b":"https://b.example.com"},
		"defaultServer":"srv-a",
		"dcLookupMap": {"DC-1": {"internalSubnets":["10.0.0.0/24"],"baseServer":"srv-a","backupServer":[],"enabled":true}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 {
		t.Fatalf("expected no unknown fields for dynamic map keys, got %v", paths)
	}
}

func TestUnknownFieldPaths_InsideOverridePatchIsNotChecked(t *testing.T) {
	// patch content is arbitrary partial-document JSON, validated separately
	// (allowed top-level keys only) - it must not trip the generic unknown-
	// field walker meant for the schema itself.
	paths, err := UnknownFieldPaths([]byte(`{
		"schemaVersion":1,"servers":{"a":"https://a.example.com"},"defaultServer":"a",
		"overrides":[{"id":"o1","match":{"all":true},"patch":{"updater":{"pollIntervalMinutes":5}}}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 {
		t.Fatalf("expected no unknown fields, got %v", paths)
	}
}
