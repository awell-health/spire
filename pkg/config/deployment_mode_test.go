package config

import "testing"

func TestDeploymentModeDefault(t *testing.T) {
	if got := Default(); got != DeploymentModeLocalNative {
		t.Fatalf("Default() = %q, want %q", got, DeploymentModeLocalNative)
	}
}

func TestDeploymentModeValidate_Valid(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want DeploymentMode
	}{
		{name: "local-native", in: "local-native", want: DeploymentModeLocalNative},
		{name: "cluster-native", in: "cluster-native", want: DeploymentModeClusterNative},
		{name: "attached-reserved", in: "attached-reserved", want: DeploymentModeAttachedReserved},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Validate(tc.in)
			if err != nil {
				t.Fatalf("Validate(%q) returned error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("Validate(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDeploymentModeValidate_Invalid(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{name: "empty", in: ""},
		{name: "unknown word", in: "hybrid"},
		{name: "casing mismatch", in: "Local-Native"},
		{name: "underscore form", in: "local_native"},
		{name: "trailing space", in: "local-native "},
		{name: "leading space", in: " local-native"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Validate(tc.in)
			if err == nil {
				t.Fatalf("Validate(%q) = %q, want error", tc.in, got)
			}
			if got != "" {
				t.Fatalf("Validate(%q) returned mode %q on error path; want zero value", tc.in, got)
			}
		})
	}
}

func TestDeploymentModeRoundTrip(t *testing.T) {
	modes := []DeploymentMode{
		DeploymentModeLocalNative,
		DeploymentModeClusterNative,
		DeploymentModeAttachedReserved,
	}
	for _, m := range modes {
		t.Run(string(m), func(t *testing.T) {
			s := m.String()
			parsed, err := Validate(s)
			if err != nil {
				t.Fatalf("Validate(%q) after String() returned error: %v", s, err)
			}
			if parsed != m {
				t.Fatalf("round-trip: got %q, want %q", parsed, m)
			}
			if s != string(m) {
				t.Fatalf("String() = %q, want %q", s, string(m))
			}
		})
	}
}

func TestDeploymentModeConstants(t *testing.T) {
	if string(DeploymentModeLocalNative) != "local-native" {
		t.Fatalf("DeploymentModeLocalNative = %q, want %q", DeploymentModeLocalNative, "local-native")
	}
	if string(DeploymentModeClusterNative) != "cluster-native" {
		t.Fatalf("DeploymentModeClusterNative = %q, want %q", DeploymentModeClusterNative, "cluster-native")
	}
	if string(DeploymentModeAttachedReserved) != "attached-reserved" {
		t.Fatalf("DeploymentModeAttachedReserved = %q, want %q", DeploymentModeAttachedReserved, "attached-reserved")
	}
}
