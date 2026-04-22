package main

import (
	"strings"
	"testing"
)

func TestIsDebugTower(t *testing.T) {
	cases := []struct {
		name, allow string
		want        bool
	}{
		{"debug-recovery", "", true},
		{"debug-", "", true},
		{"prod", "", false},
		{"test", "test", true},
		{"test", "foo,test,bar", true},
		{"test", " foo , test , bar ", true}, // whitespace-tolerant
		{"test", "foo,bar", false},
		{"", "", false},
		{"", "debug-recovery", false},
	}
	for _, tc := range cases {
		if got := isDebugTower(tc.name, tc.allow); got != tc.want {
			t.Errorf("isDebugTower(%q, %q) = %v, want %v", tc.name, tc.allow, got, tc.want)
		}
	}
}

func TestRequireDebugTower_AllowlistFromEnv(t *testing.T) {
	t.Setenv("SPIRE_TOWER", "my-test-tower")
	t.Setenv("SPIRE_DEBUG_TOWER", "other,my-test-tower")
	if err := requireDebugTower(); err != nil {
		t.Fatalf("expected allowlisted tower to pass, got %v", err)
	}
}

func TestRequireDebugTower_RejectsProdTower(t *testing.T) {
	t.Setenv("SPIRE_TOWER", "prod")
	t.Setenv("SPIRE_DEBUG_TOWER", "")
	err := requireDebugTower()
	if err == nil {
		t.Fatal("expected rejection, got nil")
	}
	// Message must match the design-specified format so operators can grep it.
	if !strings.Contains(err.Error(), `refusing to file debug beads in tower "prod"`) {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRequireDebugTower_AcceptsDebugPrefix(t *testing.T) {
	t.Setenv("SPIRE_TOWER", "debug-recovery")
	t.Setenv("SPIRE_DEBUG_TOWER", "")
	if err := requireDebugTower(); err != nil {
		t.Fatalf("expected debug- prefixed tower to pass, got %v", err)
	}
}
