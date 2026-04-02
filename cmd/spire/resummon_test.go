package main

import (
	"strings"
	"testing"
)

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
