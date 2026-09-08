package handlers

import (
	"testing"

	"emly-api-go/internal/models"
)

func TestNormalizeBanValue(t *testing.T) {
	cases := []struct {
		name    string
		banType models.BanType
		in      string
		want    string
		wantErr bool
	}{
		{"ipv4", models.BanTypeIP, "203.0.113.9", "203.0.113.9", false},
		{"ipv4 padded", models.BanTypeIP, "  203.0.113.9 ", "203.0.113.9", false},
		// Two spellings of one address must not become two rows that each
		// miss the traffic, so the value is re-rendered from the parsed IP.
		{"ipv6 canonicalised", models.BanTypeIP, "2001:0DB8:0000::1", "2001:db8::1", false},
		{"not an ip", models.BanTypeIP, "pc-01", "", true},
		{"ip empty", models.BanTypeIP, "   ", "", true},

		{"hostname lower-cased", models.BanTypeHostname, "PC-01", "pc-01", false},
		{"hostname trimmed", models.BanTypeHostname, " pc-01 ", "pc-01", false},

		// A HWID is an opaque firmware string matched byte for byte against
		// the header, so its case is preserved where a hostname's is not.
		{"hwid kept verbatim", models.BanTypeHWID, "36CC511A-F0DE-EA11-8106-842AFDCE34D0", "36CC511A-F0DE-EA11-8106-842AFDCE34D0", false},
		{"hwid empty", models.BanTypeHWID, "", "", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := normalizeBanValue(c.banType, c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("normalizeBanValue(%q) = %q, want an error", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeBanValue(%q): %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("normalizeBanValue(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestNormalizeBanValueRejectsOverlongValue(t *testing.T) {
	long := make([]byte, 256)
	for i := range long {
		long[i] = 'a'
	}
	if _, err := normalizeBanValue(models.BanTypeHWID, string(long)); err == nil {
		t.Fatal("a value past the column width must be rejected, not silently truncated")
	}
}

func TestValidBanType(t *testing.T) {
	for _, ok := range []models.BanType{models.BanTypeIP, models.BanTypeHWID, models.BanTypeHostname} {
		if !models.ValidBanType(ok) {
			t.Errorf("%q must be valid", ok)
		}
	}
	for _, bad := range []models.BanType{"", "IP", "mac", "serial"} {
		if models.ValidBanType(bad) {
			t.Errorf("%q must not be valid", bad)
		}
	}
}
