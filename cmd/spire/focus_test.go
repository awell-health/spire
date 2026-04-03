package main

import (
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/executor"
	"github.com/awell-health/spire/pkg/formula"
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

func TestGetInterruptLabel(t *testing.T) {
	t.Run("extracts label value", func(t *testing.T) {
		labels := []string{"needs-human", "interrupted:step-failure", "phase:implement"}
		got := getInterruptLabel(labels)
		if got != "step-failure" {
			t.Errorf("expected 'step-failure', got %q", got)
		}
	})

	t.Run("returns empty for no interrupted label", func(t *testing.T) {
		labels := []string{"needs-human"}
		got := getInterruptLabel(labels)
		if got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})

	t.Run("returns empty for nil labels", func(t *testing.T) {
		got := getInterruptLabel(nil)
		if got != "" {
			t.Errorf("expected empty, got %q", got)
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
		if !strings.Contains(section, "Recovery Work") {
			t.Errorf("expected 'Recovery Work' header in output: %s", section)
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

func TestTopoSortSteps(t *testing.T) {
	t.Run("linear chain", func(t *testing.T) {
		graph := &formula.FormulaStepGraph{
			Steps: map[string]formula.StepConfig{
				"plan":      {},
				"implement": {Needs: []string{"plan"}},
				"review":    {Needs: []string{"implement"}},
				"merge":     {Needs: []string{"review"}},
			},
		}
		result := topoSortSteps(graph)
		expected := []string{"plan", "implement", "review", "merge"}
		if len(result) != len(expected) {
			t.Fatalf("expected %d steps, got %d: %v", len(expected), len(result), result)
		}
		for i, name := range expected {
			if result[i] != name {
				t.Errorf("position %d: expected %q, got %q", i, name, result[i])
			}
		}
	})

	t.Run("branching graph", func(t *testing.T) {
		graph := &formula.FormulaStepGraph{
			Steps: map[string]formula.StepConfig{
				"plan":      {},
				"implement": {Needs: []string{"plan"}},
				"review":    {Needs: []string{"implement"}},
				"close":     {Needs: []string{"merge"}},
				"merge":     {Needs: []string{"review"}},
				"discard":   {Needs: []string{"review"}},
			},
		}
		result := topoSortSteps(graph)

		// Verify ordering constraints
		indexOf := make(map[string]int)
		for i, name := range result {
			indexOf[name] = i
		}
		if indexOf["plan"] >= indexOf["implement"] {
			t.Error("plan must come before implement")
		}
		if indexOf["implement"] >= indexOf["review"] {
			t.Error("implement must come before review")
		}
		if indexOf["review"] >= indexOf["merge"] {
			t.Error("review must come before merge")
		}
		if indexOf["review"] >= indexOf["discard"] {
			t.Error("review must come before discard")
		}
		if indexOf["merge"] >= indexOf["close"] {
			t.Error("merge must come before close")
		}
	})
}

func TestFindFailedStep(t *testing.T) {
	t.Run("finds failed step", func(t *testing.T) {
		gs := &executor.GraphState{
			Steps: map[string]executor.StepState{
				"plan":      {Status: "completed"},
				"implement": {Status: "failed", Outputs: map[string]string{"error": "exit code 1"}},
				"review":    {Status: "pending"},
			},
		}
		name, ss := findFailedStep(gs)
		if name != "implement" {
			t.Errorf("expected 'implement', got %q", name)
		}
		if ss.Outputs["error"] != "exit code 1" {
			t.Errorf("expected error output, got %v", ss.Outputs)
		}
	})

	t.Run("returns empty when no failure", func(t *testing.T) {
		gs := &executor.GraphState{
			Steps: map[string]executor.StepState{
				"plan":      {Status: "completed"},
				"implement": {Status: "active"},
			},
		}
		name, _ := findFailedStep(gs)
		if name != "" {
			t.Errorf("expected empty, got %q", name)
		}
	})

	t.Run("handles nil steps", func(t *testing.T) {
		gs := &executor.GraphState{}
		name, _ := findFailedStep(gs)
		if name != "" {
			t.Errorf("expected empty, got %q", name)
		}
	})
}

func TestParseDuration(t *testing.T) {
	t.Run("seconds", func(t *testing.T) {
		got := parseDuration("2026-01-01T00:00:00Z", "2026-01-01T00:00:45Z")
		if got != "45s" {
			t.Errorf("expected '45s', got %q", got)
		}
	})

	t.Run("minutes and seconds", func(t *testing.T) {
		got := parseDuration("2026-01-01T00:00:00Z", "2026-01-01T00:02:30Z")
		if got != "2m30s" {
			t.Errorf("expected '2m30s', got %q", got)
		}
	})

	t.Run("hours and minutes", func(t *testing.T) {
		got := parseDuration("2026-01-01T00:00:00Z", "2026-01-01T01:15:00Z")
		if got != "1h15m" {
			t.Errorf("expected '1h15m', got %q", got)
		}
	})

	t.Run("sub-second", func(t *testing.T) {
		got := parseDuration("2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z")
		if got != "<1s" {
			t.Errorf("expected '<1s', got %q", got)
		}
	})

	t.Run("invalid start", func(t *testing.T) {
		got := parseDuration("bad", "2026-01-01T00:00:00Z")
		if got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})

	t.Run("invalid end", func(t *testing.T) {
		got := parseDuration("2026-01-01T00:00:00Z", "bad")
		if got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})
}

func TestFormatStepDetail(t *testing.T) {
	t.Run("completed with duration", func(t *testing.T) {
		ss := executor.StepState{
			Status:      "completed",
			StartedAt:   "2026-01-01T00:00:00Z",
			CompletedAt: "2026-01-01T00:00:05Z",
		}
		got := formatStepDetail(ss)
		if got != "(5s)" {
			t.Errorf("expected '(5s)', got %q", got)
		}
	})

	t.Run("completed multiple times", func(t *testing.T) {
		ss := executor.StepState{
			Status:         "completed",
			StartedAt:      "2026-01-01T00:00:00Z",
			CompletedAt:    "2026-01-01T00:00:10Z",
			CompletedCount: 3,
		}
		got := formatStepDetail(ss)
		if !strings.Contains(got, "(10s)") {
			t.Errorf("expected duration in output, got %q", got)
		}
		if !strings.Contains(got, "x3") {
			t.Errorf("expected 'x3' in output, got %q", got)
		}
	})

	t.Run("pending has no detail", func(t *testing.T) {
		ss := executor.StepState{Status: "pending"}
		got := formatStepDetail(ss)
		if got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})
}

func TestFormatStepCondition(t *testing.T) {
	t.Run("terminal with condition", func(t *testing.T) {
		stepCfg := formula.StepConfig{
			Terminal: true,
			When: &formula.StructuredCondition{
				All: []formula.Predicate{
					{Left: "steps.review.outputs.outcome", Op: "eq", Right: "merge"},
				},
			},
		}
		got := formatStepCondition(stepCfg)
		if !strings.Contains(got, "terminal") {
			t.Errorf("expected 'terminal' in output, got %q", got)
		}
		if !strings.Contains(got, "when:") {
			t.Errorf("expected 'when:' in output, got %q", got)
		}
	})

	t.Run("terminal without condition", func(t *testing.T) {
		stepCfg := formula.StepConfig{Terminal: true}
		got := formatStepCondition(stepCfg)
		if got != "[terminal]" {
			t.Errorf("expected '[terminal]', got %q", got)
		}
	})

	t.Run("conditional non-terminal", func(t *testing.T) {
		stepCfg := formula.StepConfig{
			When: &formula.StructuredCondition{
				All: []formula.Predicate{
					{Left: "steps.review.outputs.outcome", Op: "eq", Right: "merge"},
				},
			},
		}
		got := formatStepCondition(stepCfg)
		if !strings.Contains(got, "when:") {
			t.Errorf("expected 'when:' in output, got %q", got)
		}
		if strings.Contains(got, "terminal") {
			t.Errorf("should not contain 'terminal', got %q", got)
		}
	})

	t.Run("no condition", func(t *testing.T) {
		stepCfg := formula.StepConfig{}
		got := formatStepCondition(stepCfg)
		if got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})
}

func TestConditionSummary(t *testing.T) {
	t.Run("structured when", func(t *testing.T) {
		stepCfg := formula.StepConfig{
			When: &formula.StructuredCondition{
				All: []formula.Predicate{
					{Left: "steps.review.outputs.outcome", Op: "eq", Right: "merge"},
				},
			},
		}
		got := conditionSummary(stepCfg)
		if got != "outcome eq merge" {
			t.Errorf("expected 'outcome eq merge', got %q", got)
		}
	})

	t.Run("string condition", func(t *testing.T) {
		stepCfg := formula.StepConfig{
			Condition: "verdict == approve",
		}
		got := conditionSummary(stepCfg)
		if got != "verdict == approve" {
			t.Errorf("expected 'verdict == approve', got %q", got)
		}
	})
}

func TestShortKey(t *testing.T) {
	t.Run("dotted key", func(t *testing.T) {
		got := shortKey("steps.review.outputs.outcome")
		if got != "outcome" {
			t.Errorf("expected 'outcome', got %q", got)
		}
	})

	t.Run("simple key", func(t *testing.T) {
		got := shortKey("verdict")
		if got != "verdict" {
			t.Errorf("expected 'verdict', got %q", got)
		}
	})
}
