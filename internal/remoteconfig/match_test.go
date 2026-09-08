package remoteconfig

import "testing"

func boolPtr(b bool) *bool { return &b }

func TestMatch_All(t *testing.T) {
	sel := Selector{All: boolPtr(true)}
	if !Match(sel, Host{}) {
		t.Fatal("all:true must match an empty host")
	}
}

func TestMatch_HWIDCaseInsensitive(t *testing.T) {
	sel := Selector{HWIDs: []string{"ABC-123"}}
	if !Match(sel, Host{HWID: "abc-123"}) {
		t.Fatal("expected case-insensitive hwid match")
	}
	if Match(sel, Host{HWID: "other"}) {
		t.Fatal("expected no match for a different hwid")
	}
}

func TestMatch_ANDAcrossKeys(t *testing.T) {
	sel := Selector{DCs: []string{"DC-RM2"}, Subnets: []string{"192.168.100.0/24"}}
	// DC matches, subnet doesn't -> selector must not match (AND).
	if Match(sel, Host{DC: "DC-RM2", IPs: []string{"10.0.0.5"}}) {
		t.Fatal("expected no match: subnet key present but no IP inside it")
	}
	if !Match(sel, Host{DC: "DC-RM2", IPs: []string{"192.168.100.42"}}) {
		t.Fatal("expected a match: both keys satisfied")
	}
}

func TestMatch_DCIgnoresDNSSuffix(t *testing.T) {
	sel := Selector{DCs: []string{"DC-RM2"}}
	if !Match(sel, Host{DC: "dc-rm2.tregcc.local"}) {
		t.Fatal("expected DNS-suffix-insensitive DC match")
	}
}

func TestMatch_EmptySelectorNeverMatches(t *testing.T) {
	if Match(Selector{}, Host{HWID: "x", Hostname: "y"}) {
		t.Fatal("an empty selector (no keys at all) must never match")
	}
}

func TestEffective_ExceptExemptsFromMatch(t *testing.T) {
	docJSON := `{
		"schemaVersion":1,"servers":{"a":"https://a.example.com"},"defaultServer":"a",
		"overrides":[{
			"id":"fleet-freeze",
			"match":{"all":true},
			"except":{"hwids":["pilot-1"]},
			"patch":{"control":{"updater":{"enabled":false}}}
		}]
	}`
	doc, problems := Parse([]byte(docJSON))
	if len(problems) != 0 {
		t.Fatalf("invalid doc: %+v", problems)
	}

	eff, ids := Effective(doc, Host{HWID: "some-other-host"})
	if len(ids) != 1 || ids[0] != "fleet-freeze" {
		t.Fatalf("expected fleet-freeze to apply, got ids=%v", ids)
	}
	if eff.Control == nil || eff.Control.Updater.Enabled {
		t.Fatalf("expected control.updater.enabled=false for a non-exempted host")
	}

	effPilot, idsPilot := Effective(doc, Host{HWID: "pilot-1"})
	if len(idsPilot) != 0 {
		t.Fatalf("expected no overrides to apply to the exempted pilot host, got ids=%v", idsPilot)
	}
	_ = effPilot
}

func TestEffective_ExpiredOverrideSkipped(t *testing.T) {
	docJSON := `{
		"schemaVersion":1,"servers":{"a":"https://a.example.com"},"defaultServer":"a",
		"overrides":[{
			"id":"expired",
			"match":{"all":true},
			"until":"2020-01-01T00:00:00Z",
			"patch":{"updater":{"pollIntervalMinutes":5}}
		}]
	}`
	doc, problems := Parse([]byte(docJSON))
	if len(problems) != 0 {
		t.Fatalf("invalid doc: %+v", problems)
	}
	_, ids := Effective(doc, Host{})
	if len(ids) != 0 {
		t.Fatalf("expected the expired override to be skipped, got ids=%v", ids)
	}
}

func TestEffective_OverridesAppliedInOrder(t *testing.T) {
	docJSON := `{
		"schemaVersion":1,"servers":{"a":"https://a.example.com"},"defaultServer":"a",
		"overrides":[
			{"id":"first","match":{"all":true},"patch":{"updater":{"pollIntervalMinutes":5}}},
			{"id":"second","match":{"all":true},"patch":{"updater":{"pollIntervalMinutes":10}}}
		]
	}`
	doc, problems := Parse([]byte(docJSON))
	if len(problems) != 0 {
		t.Fatalf("invalid doc: %+v", problems)
	}
	eff, ids := Effective(doc, Host{})
	if len(ids) != 2 || ids[0] != "first" || ids[1] != "second" {
		t.Fatalf("expected both overrides in order, got %v", ids)
	}
	if eff.Updater == nil || eff.Updater.PollIntervalMinutes == nil || *eff.Updater.PollIntervalMinutes != 10 {
		t.Fatalf("expected the later override to win, got %+v", eff.Updater)
	}
}
