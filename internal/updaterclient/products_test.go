package updaterclient

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestInstalledProductsFromHeader(t *testing.T) {
	cases := []struct {
		name   string
		values []string // nil: header absent
		want   map[string]string
	}{
		{"absent is not reported", nil, nil},
		{"empty is an empty inventory", []string{""}, map[string]string{}},
		{"pairs", []string{"emly=3.5.0, foo=1.2.0"}, map[string]string{"emly": "3.5.0", "foo": "1.2.0"}},
		{"repeated header lines merge", []string{"emly=3.5.0", "foo=1.2.0"}, map[string]string{"emly": "3.5.0", "foo": "1.2.0"}},
		{"malformed entries dropped", []string{"emly=3.5.0,Bad Slug=1,noequals,bar=,=1.0"}, map[string]string{"emly": "3.5.0"}},
		{"version truncated to column width", []string{"foo=" + strings.Repeat("9", 30)}, map[string]string{"foo": strings.Repeat("9", 20)}},
	}
	for _, c := range cases {
		h := http.Header{}
		for _, v := range c.values {
			h.Add(InstalledProductsHeader, v)
		}
		got := installedProductsFromHeader(h)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %#v, want %#v", c.name, got, c.want)
		}
		if (got == nil) != (c.want == nil) {
			t.Errorf("%s: nil-ness differs: got nil=%v, want nil=%v", c.name, got == nil, c.want == nil)
		}
	}
}

func TestSanitizeInstalledProductsCapsCount(t *testing.T) {
	in := map[string]string{}
	for i := 0; i < maxInstalledProducts+10; i++ {
		in["p"+strings.Repeat("x", i%15)+string(rune('a'+i%26))] = "1.0"
	}
	if got := sanitizeInstalledProducts(in); len(got) > maxInstalledProducts {
		t.Errorf("kept %d products, cap is %d", len(got), maxInstalledProducts)
	}
}

// The header and the WS identity must land on the same inventory: the
// two-constructor rule this package exists to enforce.
func TestInstalledProductsSameFromBothConstructors(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v2/updates/manifest", nil)
	r.Header.Set("X-EMLy-HWID", "hwid-1")
	r.Header.Set(InstalledProductsHeader, "emly=3.5.0,foo=1.2.0,Bad=1")
	fromHeader := IdentityFromRequest(r).InstalledProducts

	ws := httptest.NewRequest(http.MethodGet, "/v2/client/ws", nil)
	fromWS := IdentityFromWSPayload(ws, WSIdentityPayload{
		HWID:              "hwid-1",
		InstalledProducts: map[string]string{"emly": "3.5.0", "foo": "1.2.0", "Bad": "1"},
	}).InstalledProducts

	want := map[string]string{"emly": "3.5.0", "foo": "1.2.0"}
	if !reflect.DeepEqual(fromHeader, want) || !reflect.DeepEqual(fromWS, want) {
		t.Errorf("header %#v, ws %#v, want %#v", fromHeader, fromWS, want)
	}

	// Absent on both paths means "not reported".
	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	if IdentityFromRequest(r2).InstalledProducts != nil {
		t.Error("absent header must stay nil")
	}
	if IdentityFromWSPayload(ws, WSIdentityPayload{HWID: "x"}).InstalledProducts != nil {
		t.Error("absent WS field must stay nil")
	}
}
