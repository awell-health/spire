package main

import (
	"strings"
	"testing"

	"github.com/steveyegge/beads"
)

func TestIsInterruptedBead(t *testing.T) {
	t.Run("interrupted label present", func(t *testing.T) {
		labels := []string{"needs-human", "interrupted:merge-failure", "phase:merge"}
		if !isInterruptedBead(labels) {
			t.Error("expected true for interrupted:merge-failure label")
		}
	})

	t.Run("no interrupted label", func(t *testing.T) {
		labels := []string{"needs-human", "phase:implement"}
		if isInterruptedBead(labels) {
			t.Error("expected false without interrupted:* label")
		}
	})

	t.Run("empty labels", func(t *testing.T) {
		if isInterruptedBead(nil) {
			t.Error("expected false for nil labels")
		}
	})
}

func TestFormatRecoverySection(t *testing.T) {
	t.Run("shows first open recovery-for dependent", func(t *testing.T) {
		deps := []*beads.IssueWithDependencyMetadata{
			{
				Issue:          beads.Issue{ID: "spi-alert", Status: beads.StatusOpen, Labels: []string{"alert:merge-failure"}},
				DependencyType: "caused-by",
			},
			{
				Issue:          beads.Issue{ID: "spi-rec1", Title: "recovery: merge-failure", Status: beads.StatusOpen},
				DependencyType: "recovery-for",
			},
			{
				Issue:          beads.Issue{ID: "spi-rec2", Title: "recovery: second", Status: beads.StatusOpen},
				DependencyType: "recovery-for",
			},
		}
		section := formatRecoverySection(deps)
		if section == "" {
			t.Fatal("expected non-empty section")
		}
		if !strings.Contains(section, "spi-rec1") {
			t.Errorf("expected spi-rec1 in output, got: %s", section)
		}
		if strings.Contains(section, "spi-rec2") {
			t.Errorf("should only show first open recovery bead, but found spi-rec2 in: %s", section)
		}
		if !strings.Contains(section, "Recovery work") {
			t.Errorf("expected 'Recovery work' header in output: %s", section)
		}
	})

	t.Run("skips closed recovery beads", func(t *testing.T) {
		deps := []*beads.IssueWithDependencyMetadata{
			{
				Issue:          beads.Issue{ID: "spi-rec-closed", Title: "old recovery", Status: beads.StatusClosed},
				DependencyType: "recovery-for",
			},
		}
		section := formatRecoverySection(deps)
		if section != "" {
			t.Errorf("expected empty section for closed recovery, got: %s", section)
		}
	})

	t.Run("returns empty for non-recovery deps", func(t *testing.T) {
		deps := []*beads.IssueWithDependencyMetadata{
			{
				Issue:          beads.Issue{ID: "spi-alert", Status: beads.StatusOpen, Labels: []string{"alert:build-failure"}},
				DependencyType: "caused-by",
			},
		}
		section := formatRecoverySection(deps)
		if section != "" {
			t.Errorf("expected empty section for non-recovery deps, got: %s", section)
		}
	})

	t.Run("returns empty for nil deps", func(t *testing.T) {
		section := formatRecoverySection(nil)
		if section != "" {
			t.Errorf("expected empty section for nil deps, got: %s", section)
		}
	})

	t.Run("skips closed and returns first open", func(t *testing.T) {
		deps := []*beads.IssueWithDependencyMetadata{
			{
				Issue:          beads.Issue{ID: "spi-rec-old", Title: "old", Status: beads.StatusClosed},
				DependencyType: "recovery-for",
			},
			{
				Issue:          beads.Issue{ID: "spi-rec-new", Title: "new recovery", Status: beads.StatusOpen},
				DependencyType: "recovery-for",
			},
		}
		section := formatRecoverySection(deps)
		if !strings.Contains(section, "spi-rec-new") {
			t.Errorf("expected spi-rec-new in output, got: %s", section)
		}
		if strings.Contains(section, "spi-rec-old") {
			t.Errorf("should not show closed recovery bead, got: %s", section)
		}
	})
}
