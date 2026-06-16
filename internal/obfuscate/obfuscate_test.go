package obfuscate

import (
	"reflect"
	"strings"
	"testing"
)

func TestShiftFromDateString(t *testing.T) {
	cases := []struct {
		date string
		want int
	}{
		{"2026-06-16", 16}, // even month -> positive
		{"2026-07-09", -9}, // odd month  -> negative
		{"2026-02-28", 28}, // even month -> positive
		{"2026-01-01", -1}, // odd month  -> negative
	}
	for _, c := range cases {
		got, err := ShiftFromDateString(c.date)
		if err != nil {
			t.Fatalf("ShiftFromDateString(%q) unexpected error: %v", c.date, err)
		}
		if got != c.want {
			t.Errorf("ShiftFromDateString(%q) = %d, want %d", c.date, got, c.want)
		}
	}

	if _, err := ShiftFromDateString("not-a-date"); err == nil {
		t.Error("ShiftFromDateString(\"not-a-date\") expected error, got nil")
	}
}

func TestObfuscatedNameDeterministic(t *testing.T) {
	// Same field id + same shift -> same name, every time.
	for _, id := range []string{"maintenance_mode", "feature_flags", "max_upload_mb"} {
		for _, shift := range []int{16, -9, 1, -28} {
			a := ObfuscatedName(id, shift)
			b := ObfuscatedName(id, shift)
			if a != b {
				t.Errorf("ObfuscatedName(%q, %d) not deterministic: %q != %q", id, shift, a, b)
			}
			if a == "" {
				t.Errorf("ObfuscatedName(%q, %d) returned empty string", id, shift)
			}
		}
	}
}

func TestObfuscatedNameChangesWithShift(t *testing.T) {
	const id = "feature_flags"
	// Different dates, including an odd month (negative shift), should generally
	// produce different names.
	shifts := []int{16, -9, 1, -1, 28, -28}
	seen := map[string]int{}
	for _, s := range shifts {
		seen[ObfuscatedName(id, s)]++
	}
	if len(seen) < len(shifts)-1 {
		t.Errorf("expected mostly-distinct names across shifts, got %d distinct from %d shifts: %v",
			len(seen), len(shifts), seen)
	}

	// A positive vs negative shift of the same magnitude must differ.
	if ObfuscatedName(id, 16) == ObfuscatedName(id, -16) {
		t.Error("expected different names for +16 and -16 shift")
	}
}

func TestBuildHidesFieldIDs(t *testing.T) {
	values := map[string]any{
		"maintenance_mode": true,
		"feature_flags":    map[string]any{"new_dashboard": false},
	}
	schema := []Field{
		{ID: "maintenance_mode"},
		{ID: "feature_flags", Children: []Field{{ID: "new_dashboard"}}},
	}
	out := Build(schema, values, 16)

	// No raw field id may leak as a key, at any level.
	if _, ok := out["maintenance_mode"]; ok {
		t.Error("raw field id leaked into built output")
	}
	flagsName := ObfuscatedName("feature_flags", 16)
	nested, ok := out[flagsName].(map[string]any)
	if !ok {
		t.Fatalf("nested object missing under obfuscated name %q", flagsName)
	}
	if _, ok := nested["new_dashboard"]; ok {
		t.Error("raw nested field id leaked into built output")
	}
}

func TestBuildParseRoundTrip(t *testing.T) {
	values := map[string]any{
		"maintenance_mode":      false,
		"min_supported_version": "1.4.0",
		"telemetry_enabled":     true,
		"feature_flags": map[string]any{
			"new_dashboard": true,
			"dark_mode_v2":  false,
			"beta_uploads":  false,
		},
		"limits": map[string]any{
			"max_upload_mb":       25,
			"max_reports_per_day": 50,
		},
	}

	for _, shift := range []int{16, -9} {
		built := Build(ConfigSchema, values, shift)
		parsed, err := Parse(ConfigSchema, built, shift)
		if err != nil {
			t.Fatalf("Parse error at shift %d: %v", shift, err)
		}
		if !reflect.DeepEqual(parsed, values) {
			t.Errorf("round-trip mismatch at shift %d:\n got: %#v\nwant: %#v", shift, parsed, values)
		}
	}
}

func TestParseMissingFieldError(t *testing.T) {
	values := map[string]any{
		"maintenance_mode":      false,
		"min_supported_version": "1.4.0",
		"telemetry_enabled":     true,
		"feature_flags": map[string]any{
			"new_dashboard": true,
			"dark_mode_v2":  false,
			"beta_uploads":  false,
		},
		"limits": map[string]any{
			"max_upload_mb":       25,
			"max_reports_per_day": 50,
		},
	}

	// Parsing a build with the WRONG shift must fail loudly (names won't match).
	built := Build(ConfigSchema, values, 16)
	if _, err := Parse(ConfigSchema, built, -9); err == nil {
		t.Error("expected error when parsing with a mismatched shift, got nil")
	}

	// Removing one obfuscated key must yield a clear, named error.
	name := ObfuscatedName("telemetry_enabled", 16)
	delete(built, name)
	_, err := Parse(ConfigSchema, built, 16)
	if err == nil {
		t.Fatal("expected error for missing field, got nil")
	}
	if !strings.Contains(err.Error(), "telemetry_enabled") {
		t.Errorf("error should name the missing field id, got: %v", err)
	}
}
