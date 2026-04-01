package dolt

import "testing"

func TestSemverAtLeast(t *testing.T) {
	tests := []struct {
		version, minimum string
		want             bool
	}{
		{"1.84.0", "1.84.0", true},  // exact match
		{"1.85.0", "1.84.0", true},  // newer minor
		{"2.0.0", "1.84.0", true},   // newer major
		{"1.84.1", "1.84.0", true},  // newer patch
		{"1.83.0", "1.84.0", false}, // older minor
		{"0.99.0", "1.84.0", false}, // older major
		{"1.84.0", "1.84.1", false}, // older patch
	}
	for _, tt := range tests {
		got := semverAtLeast(tt.version, tt.minimum)
		if got != tt.want {
			t.Errorf("semverAtLeast(%q, %q) = %v, want %v", tt.version, tt.minimum, got, tt.want)
		}
	}
}
