package config

import "testing"

// TestNormalizeWhitelistMode memastikan parsing WHITELIST_MODE: hanya "open"
// (case-insensitive, boleh berspasi) yang membuka mode open; nilai kosong/aneh
// jatuh ke "strict" (default aman). Tak butuh infra — murni fungsi.
func TestNormalizeWhitelistMode(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"open", "open"},
		{"OPEN", "open"},
		{"  Open  ", "open"},
		{"strict", "strict"},
		{"STRICT", "strict"},
		{"", "strict"},
		{"   ", "strict"},
		{"loose", "strict"}, // tak dikenal → default aman
		{"true", "strict"},  // salah ketik → default aman
		{"disabled", "strict"},
	}
	for _, c := range cases {
		if got := normalizeWhitelistMode(c.in); got != c.want {
			t.Errorf("normalizeWhitelistMode(%q) = %q, ingin %q", c.in, got, c.want)
		}
	}
}
