package main

import "testing"

func TestNormalizeVersion(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", ""},
		{"v0.36.0", "v0.36.0"},
		{"0.36.0", "v0.36.0"},
		{"v1.0.0", "v1.0.0"},
		{"1.0.0", "v1.0.0"},
	}
	for _, tt := range tests {
		got := normalizeVersion(tt.input)
		if got != tt.want {
			t.Errorf("normalizeVersion(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestIsValidSemver(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"", false},
		{"dev", false},
		{"v0.36.0", true},
		{"0.36.0", true},
		{"v1.0.0-rc1", true},
		{"not-a-version", false},
		{"v", false},
		{"vx.y.z", false},
	}
	for _, tt := range tests {
		got := isValidSemver(tt.input)
		if got != tt.want {
			t.Errorf("isValidSemver(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestDecideVersionAction(t *testing.T) {
	tests := []struct {
		name           string
		binaryVersion  string // override the global `version` var
		storedVersion  string
		wantSkip       bool
		wantWrite      bool
		wantWarn       bool
	}{
		{
			name:          "dev binary, no stored",
			binaryVersion: "dev",
			storedVersion: "",
			wantSkip:      false,
			wantWrite:     false,
			wantWarn:      false,
		},
		{
			name:          "dev binary, stored version exists",
			binaryVersion: "dev",
			storedVersion: "v0.36.0",
			wantSkip:      false,
			wantWrite:     false,
			wantWarn:      false,
		},
		{
			name:          "empty binary",
			binaryVersion: "",
			storedVersion: "v0.36.0",
			wantSkip:      false,
			wantWrite:     false,
			wantWarn:      false,
		},
		{
			name:          "first run, no stored version",
			binaryVersion: "v0.36.0",
			storedVersion: "",
			wantSkip:      false,
			wantWrite:     true,
			wantWarn:      false,
		},
		{
			name:          "first run, stored is invalid",
			binaryVersion: "v0.36.0",
			storedVersion: "garbage",
			wantSkip:      false,
			wantWrite:     true,
			wantWarn:      false,
		},
		{
			name:          "versions match exactly",
			binaryVersion: "v0.36.0",
			storedVersion: "v0.36.0",
			wantSkip:      true,
			wantWrite:     false,
			wantWarn:      false,
		},
		{
			name:          "versions match, stored without v prefix",
			binaryVersion: "v0.36.0",
			storedVersion: "0.36.0",
			wantSkip:      true,
			wantWrite:     false,
			wantWarn:      false,
		},
		{
			name:          "binary newer than stored (upgrade)",
			binaryVersion: "v0.37.0",
			storedVersion: "v0.36.0",
			wantSkip:      false,
			wantWrite:     true,
			wantWarn:      false,
		},
		{
			name:          "binary older than stored (downgrade)",
			binaryVersion: "v0.35.0",
			storedVersion: "v0.36.0",
			wantSkip:      true,
			wantWrite:     false,
			wantWarn:      true,
		},
		{
			name:          "major version upgrade",
			binaryVersion: "v1.0.0",
			storedVersion: "v0.99.0",
			wantSkip:      false,
			wantWrite:     true,
			wantWarn:      false,
		},
		{
			name:          "patch upgrade",
			binaryVersion: "v0.36.1",
			storedVersion: "v0.36.0",
			wantSkip:      false,
			wantWrite:     true,
			wantWarn:      false,
		},
		{
			name:          "binary without v prefix",
			binaryVersion: "0.36.0",
			storedVersion: "v0.36.0",
			wantSkip:      true,
			wantWrite:     false,
			wantWarn:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Override the package-level version var for this test.
			orig := version
			version = tt.binaryVersion
			defer func() { version = orig }()

			action := decideVersionAction(tt.storedVersion)

			if action.skipMigrations != tt.wantSkip {
				t.Errorf("skipMigrations = %v, want %v", action.skipMigrations, tt.wantSkip)
			}
			if action.writeVersion != tt.wantWrite {
				t.Errorf("writeVersion = %v, want %v", action.writeVersion, tt.wantWrite)
			}
			if action.warn != tt.wantWarn {
				t.Errorf("warn = %v, want %v", action.warn, tt.wantWarn)
			}
		})
	}
}
