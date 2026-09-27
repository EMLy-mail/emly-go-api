package models

import (
	"encoding/json"
	"testing"
)

func TestNullJSONScan(t *testing.T) {
	var j NullJSON
	if err := j.Scan(nil); err != nil || j != nil {
		t.Fatalf("Scan(nil) = %q, %v; want nil, nil", j, err)
	}

	buf := []byte(`{"a":1}`)
	if err := j.Scan(buf); err != nil {
		t.Fatal(err)
	}
	buf[2] = 'X' // the driver reuses its buffer; the scanned value must not follow
	if string(j) != `{"a":1}` {
		t.Fatalf("Scan([]byte) aliased the driver buffer: %q", j)
	}

	if err := j.Scan(`[1]`); err != nil || string(j) != `[1]` {
		t.Fatalf("Scan(string) = %q, %v", j, err)
	}
	if err := j.Scan(42); err == nil {
		t.Fatal("Scan(int) should fail")
	}
}

func TestNullJSONMarshal(t *testing.T) {
	got, err := json.Marshal(BugReport{SystemInfo: NullJSON(`{"os":"win"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(got, &m)
	if string(m["system_info"]) != `{"os":"win"}` {
		t.Fatalf("system_info = %s", m["system_info"])
	}

	got, _ = json.Marshal(BugReport{})
	m = nil
	_ = json.Unmarshal(got, &m)
	if _, ok := m["system_info"]; ok {
		t.Fatalf("NULL system_info should be omitted: %s", got)
	}
}

func TestNullJSONValue(t *testing.T) {
	if v, _ := NullJSON(nil).Value(); v != nil {
		t.Fatalf("empty Value() = %v, want nil", v)
	}
	if v, _ := NullJSON(`{}`).Value(); string(v.([]byte)) != `{}` {
		t.Fatalf("Value() = %v", v)
	}
}
