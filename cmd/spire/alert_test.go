package main

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/steveyegge/beads"
)

func TestCmdAlert_LinksRefViaCausedBy(t *testing.T) {
	oldCreate := alertCreateBead
	oldAddDep := alertAddDepTyped
	t.Cleanup(func() {
		alertCreateBead = oldCreate
		alertAddDepTyped = oldAddDep
	})

	var created createOpts
	var depIssueID, depOnID, depType string
	alertCreateBead = func(opts createOpts) (string, error) {
		created = opts
		return "spi-alert-1", nil
	}
	alertAddDepTyped = func(issueID, dependsOnID, t string) error {
		depIssueID = issueID
		depOnID = dependsOnID
		depType = t
		return nil
	}

	if err := cmdAlert([]string{"merge failed", "--ref", "spi-parent", "--type", "merge-failure", "-p", "0"}); err != nil {
		t.Fatalf("cmdAlert returned error: %v", err)
	}

	if created.Title != "merge failed" {
		t.Fatalf("created title = %q, want %q", created.Title, "merge failed")
	}
	if created.Priority != 0 {
		t.Fatalf("created priority = %d, want 0", created.Priority)
	}
	if created.Type != beads.TypeTask {
		t.Fatalf("created type = %q, want %q", created.Type, beads.TypeTask)
	}
	if len(created.Labels) != 1 || created.Labels[0] != "alert:merge-failure" {
		t.Fatalf("created labels = %v, want [alert:merge-failure]", created.Labels)
	}
	if depIssueID != "spi-alert-1" || depOnID != "spi-parent" || depType != "caused-by" {
		t.Fatalf("dep = %s→%s:%s, want spi-alert-1→spi-parent:caused-by", depIssueID, depOnID, depType)
	}
}

func TestCmdAlert_WithoutRefSkipsDependency(t *testing.T) {
	oldCreate := alertCreateBead
	oldAddDep := alertAddDepTyped
	t.Cleanup(func() {
		alertCreateBead = oldCreate
		alertAddDepTyped = oldAddDep
	})

	calledDep := false
	alertCreateBead = func(opts createOpts) (string, error) {
		return "spi-alert-2", nil
	}
	alertAddDepTyped = func(issueID, dependsOnID, t string) error {
		calledDep = true
		return nil
	}

	if err := cmdAlert([]string{"something happened"}); err != nil {
		t.Fatalf("cmdAlert returned error: %v", err)
	}
	if calledDep {
		t.Fatal("expected no dependency to be added when --ref is omitted")
	}
}

func TestAlertCLIArgs_PreservesMessageBeforeFlags(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.Flags().String("ref", "", "")
	cmd.Flags().String("type", "", "")
	cmd.Flags().IntP("priority", "p", 1, "")
	if err := cmd.Flags().Set("ref", "spi-parent"); err != nil {
		t.Fatalf("set ref: %v", err)
	}
	if err := cmd.Flags().Set("type", "merge-failure"); err != nil {
		t.Fatalf("set type: %v", err)
	}
	if err := cmd.Flags().Set("priority", "0"); err != nil {
		t.Fatalf("set priority: %v", err)
	}

	got := alertCLIArgs(cmd, []string{"merge failed"})
	want := []string{"merge failed", "--ref", "spi-parent", "--type", "merge-failure", "-p", "0"}
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %q, want %q (full=%v)", i, got[i], want[i], got)
		}
	}
}
