package formula

import "testing"

// TestValidOnError covers the valid set, the empty-string default, and an
// explicit invalid value for the on_error step directive (spi-676a4).
func TestValidOnError(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", true},       // default (park)
		{"park", true},   // explicit park
		{"record", true}, // opt-in to error-recording
		{"bogus", false},
		{"PARK", false},    // case-sensitive
		{"record ", false}, // whitespace not trimmed
	}
	for _, tc := range cases {
		if got := ValidOnError(tc.in); got != tc.want {
			t.Errorf("ValidOnError(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
