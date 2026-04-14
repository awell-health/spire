package main

import (
	"strings"
	"testing"
)

// resummonGuardAccepts mirrors the guard condition in cmdResummon (lines 41-46).
// Extracted here so we can unit-test the decision logic without requiring a live store.
func resummonGuardAccepts(b Bead) bool {
	isHooked := b.Status == "hooked"
	hasLegacyLabel := containsLabel(b, "needs-human") || hasLabelPrefix(b, "interrupted:")
	return isHooked || hasLegacyLabel
}

// TestResummonGuard_HookedBead verifies that a bead with status=hooked is
// accepted by the resummon guard even without needs-human or interrupted:* labels.
func TestResummonGuard_HookedBead(t *testing.T) {
	b := Bead{ID: "spi-hooked", Status: "hooked", Labels: []string{"phase:implement"}}
	if !resummonGuardAccepts(b) {
		t.Error("expected hooked bead to be accepted by resummon guard")
	}
}

// TestResummonGuard_InterruptedLabel verifies that a bead with an interrupted:*
// label is accepted by the resummon guard.
func TestResummonGuard_InterruptedLabel(t *testing.T) {
	b := Bead{ID: "spi-int", Status: "in_progress", Labels: []string{"interrupted:merge-failure", "needs-human"}}
	if !resummonGuardAccepts(b) {
		t.Error("expected bead with interrupted:merge-failure to be accepted by resummon guard")
	}
}

// TestResummonGuard_OpenNonInterrupted verifies that a normal open bead with
// no hooked status and no interrupted:* labels is rejected.
func TestResummonGuard_OpenNonInterrupted(t *testing.T) {
	b := Bead{ID: "spi-normal", Status: "open", Labels: []string{"phase:implement"}}
	if resummonGuardAccepts(b) {
		t.Error("expected normal open bead to be rejected by resummon guard")
	}
}

// TestHasLabelPrefix verifies the hasLabelPrefix helper used by the resummon guard.
func TestHasLabelPrefix(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		prefix string
		want   bool
	}{
		{"match", []string{"interrupted:merge-failure"}, "interrupted:", true},
		{"no match", []string{"phase:implement"}, "interrupted:", false},
		{"empty labels", []string{}, "interrupted:", false},
		{"prefix only", []string{"interrupted:"}, "interrupted:", true},
		{"multiple labels match", []string{"phase:implement", "interrupted:build-failure"}, "interrupted:", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := Bead{Labels: tt.labels}
			got := hasLabelPrefix(b, tt.prefix)
			if got != tt.want {
				t.Errorf("hasLabelPrefix(%v, %q) = %v, want %v", tt.labels, tt.prefix, got, tt.want)
			}
		})
	}
}

// TestResummon_ClearsDispatchOverride verifies that resummon strips stale
// dispatch:* labels so the executor falls back to formula-default dispatch.
// Regression test for spi-wlxbf.
func TestResummon_ClearsDispatchOverride(t *testing.T) {
	requireStore(t)

	// Create an epic bead with needs-human and dispatch:direct labels.
	id := createTestBead(t, createOpts{
		Title:    "test-resummon-dispatch-cleanup",
		Priority: 1,
		Type:     parseIssueType("epic"),
		Labels:   []string{"needs-human", "dispatch:direct", "interrupted:implement-merge-conflict"},
	})

	// Verify labels are present before resummon.
	b, err := storeGetBead(id)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	if !containsLabel(b, "dispatch:direct") {
		t.Fatal("expected dispatch:direct label before resummon")
	}
	if !containsLabel(b, "needs-human") {
		t.Fatal("expected needs-human label before resummon")
	}

	// Run resummon. It will fail at cmdSummon (no wizard capacity configured)
	// but the label cleanup happens before that call.
	_ = cmdResummon([]string{id})

	// Re-fetch and verify dispatch:direct was stripped.
	b, err = storeGetBead(id)
	if err != nil {
		t.Fatalf("get bead after resummon: %v", err)
	}

	for _, l := range b.Labels {
		if strings.HasPrefix(l, "dispatch:") {
			t.Errorf("dispatch label %q should have been stripped by resummon", l)
		}
		if strings.HasPrefix(l, "interrupted:") {
			t.Errorf("interrupted label %q should have been stripped by resummon", l)
		}
	}
	if containsLabel(b, "needs-human") {
		t.Error("needs-human label should have been stripped by resummon")
	}
}
