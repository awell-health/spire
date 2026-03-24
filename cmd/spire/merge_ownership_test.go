package main

import "testing"

func TestIsStatusRegression(t *testing.T) {
	tests := []struct {
		from, to string
		want     bool
	}{
		// Regressions (lost work)
		{"closed", "open", true},
		{"closed", "in_progress", true},
		{"closed", "blocked", true},
		{"closed", "deferred", true},
		{"in_progress", "open", true},

		// Not regressions (legitimate workflow)
		{"open", "in_progress", false},
		{"open", "closed", false},
		{"open", "blocked", false},
		{"open", "deferred", false},
		{"in_progress", "closed", false},
		{"in_progress", "blocked", false},
		{"in_progress", "deferred", false},
		{"blocked", "open", false},
		{"blocked", "in_progress", false},
		{"blocked", "closed", false},
		{"deferred", "open", false},
		{"deferred", "in_progress", false},
		{"deferred", "closed", false},

		// Same status (no change)
		{"open", "open", false},
		{"closed", "closed", false},
		{"in_progress", "in_progress", false},
	}

	for _, tt := range tests {
		got := isStatusRegression(tt.from, tt.to)
		if got != tt.want {
			t.Errorf("isStatusRegression(%q, %q) = %v, want %v", tt.from, tt.to, got, tt.want)
		}
	}
}

func TestIsClusterField(t *testing.T) {
	// Cluster-owned fields
	for _, f := range []string{"status", "owner", "assignee", "closed_at", "closed_by_session"} {
		if !isClusterField(f) {
			t.Errorf("expected %q to be cluster-owned", f)
		}
	}

	// User-owned fields
	for _, f := range []string{"title", "description", "priority", "issue_type", "design", "notes"} {
		if isClusterField(f) {
			t.Errorf("expected %q to be user-owned, got cluster", f)
		}
	}
}

func TestCoalesce(t *testing.T) {
	if got := coalesce("", "", "c"); got != "c" {
		t.Errorf("coalesce('','','c') = %q, want 'c'", got)
	}
	if got := coalesce("a", "b"); got != "a" {
		t.Errorf("coalesce('a','b') = %q, want 'a'", got)
	}
	if got := coalesce("", ""); got != "" {
		t.Errorf("coalesce('','') = %q, want ''", got)
	}
}

func TestExtractCountValue(t *testing.T) {
	// Typical dolt tabular output for COUNT(*)
	output := `+---+
| c |
+---+
| 3 |
+---+`
	if got := extractCountValue(output); got != 3 {
		t.Errorf("extractCountValue = %d, want 3", got)
	}

	// Zero
	output2 := `+---+
| c |
+---+
| 0 |
+---+`
	if got := extractCountValue(output2); got != 0 {
		t.Errorf("extractCountValue = %d, want 0", got)
	}
}

func TestGetCurrentCommitHash_Live(t *testing.T) {
	if !doltIsReachable() {
		t.Skip("dolt server not reachable")
	}
	h := getCurrentCommitHash("beads_spi")
	if h == "" {
		t.Error("expected non-empty commit hash")
	}
	if len(h) < 20 {
		t.Errorf("commit hash too short: %q", h)
	}
}
