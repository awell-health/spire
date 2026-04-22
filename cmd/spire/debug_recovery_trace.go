package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/awell-health/spire/pkg/recovery"
	"github.com/spf13/cobra"
)

var debugRecoveryTraceJSON bool

func init() {
	debugRecoveryTraceCmd.Flags().BoolVar(&debugRecoveryTraceJSON, "json", false, "emit JSON instead of text")
	debugRecoveryTraceCmd.Args = cobra.ExactArgs(1)
}

type traceView struct {
	BeadID             string                    `json:"bead_id"`
	BeadTitle          string                    `json:"bead_title"`
	BeadStatus         string                    `json:"bead_status"`
	OutcomePresent     bool                      `json:"outcome_present"`
	Outcome            *recovery.RecoveryOutcome `json:"outcome,omitempty"`
	DecideBranch       string                    `json:"decide_branch,omitempty"`
	Metadata           recovery.RecoveryMetadata `json:"metadata"`
	LearningsCount     int                       `json:"learnings_count"`
	LearningsRecentIDs []string                  `json:"learnings_recent_ids,omitempty"`
}

func cmdDebugRecoveryTrace(args []string) error {
	if err := requireDebugTower(); err != nil {
		return err
	}
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}
	if len(args) != 1 {
		return fmt.Errorf("usage: spire debug recovery trace <recovery-bead>")
	}
	id := args[0]
	bead, err := storeGetBead(id)
	if err != nil {
		return fmt.Errorf("load bead %s: %w", id, err)
	}

	view := traceView{
		BeadID:     bead.ID,
		BeadTitle:  bead.Title,
		BeadStatus: bead.Status,
		Metadata:   recovery.RecoveryMetadataFromBead(bead),
	}
	if outcome, ok := recovery.ReadOutcome(bead); ok {
		view.OutcomePresent = true
		view.Outcome = &outcome
		view.DecideBranch = decideBranchLabel(outcome.RepairMode, outcome, view.Metadata)
	}

	sourceID := bead.ID
	if view.Outcome != nil && view.Outcome.SourceBeadID != "" {
		sourceID = view.Outcome.SourceBeadID
	}
	if learnings, err := recovery.GetRecoveryLearnings(sourceID); err == nil {
		view.LearningsCount = len(learnings)
		for i, b := range learnings {
			if i >= 5 {
				break
			}
			view.LearningsRecentIDs = append(view.LearningsRecentIDs, b.ID)
		}
	}

	if debugRecoveryTraceJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(view)
	}
	renderTraceText(view)
	return nil
}

func renderTraceText(v traceView) {
	fmt.Printf("Recovery bead:   %s  [%s]\n", v.BeadID, v.BeadStatus)
	if v.BeadTitle != "" {
		fmt.Printf("Title:           %s\n", v.BeadTitle)
	}
	if !v.OutcomePresent {
		fmt.Println("Outcome:         (none written — recovery not yet finished)")
	} else {
		o := v.Outcome
		fmt.Printf("Source bead:     %s  (failed step: %s)\n", o.SourceBeadID, o.FailedStep)
		fmt.Printf("Failure class:   %s\n", o.FailureClass)
		fmt.Printf("Decide branch:   %s\n", v.DecideBranch)
		fmt.Printf("Repair mode:     %s\n", o.RepairMode)
		fmt.Printf("Repair action:   %s\n", orDash(o.RepairAction))
		if o.RecipeID != "" {
			fmt.Printf("Recipe:          %s (v%d)\n", o.RecipeID, o.RecipeVersion)
		}
		if o.WorkerAttemptID != "" {
			fmt.Printf("Worker attempt:  %s\n", o.WorkerAttemptID)
		}
		fmt.Printf("Verify:          %s / %s\n", o.VerifyKind, o.VerifyVerdict)
		fmt.Printf("Decision:        %s\n", o.Decision)
	}
	if v.Metadata.LearningSummary != "" {
		fmt.Printf("\nLearning summary:\n  %s\n", v.Metadata.LearningSummary)
	}
	if v.LearningsCount > 0 {
		fmt.Printf("\nRelated learnings (%d total, recent first):\n", v.LearningsCount)
		for _, id := range v.LearningsRecentIDs {
			fmt.Printf("  - %s\n", id)
		}
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// decideBranchLabel reconstructs the Decide-step branch name from persisted
// state. The branch is not stored as a field; this mapping mirrors the
// decision tree in pkg/recovery/decide.go:
//   - RepairModeRecipe           → "promoted-recipe"
//   - RepairModeEscalate         → "budget-guard" | "human-guidance" | "escalate"
//     (using RecoveryMetadata.ResolutionKind as a hint)
//   - RepairModeMechanical/Worker with RepairAction="resummon" → "fallback"
//   - RepairModeMechanical/Worker otherwise → "claude"
//   - RepairModeNoop             → "noop"
func decideBranchLabel(mode recovery.RepairMode, o recovery.RecoveryOutcome, m recovery.RecoveryMetadata) string {
	switch mode {
	case recovery.RepairModeRecipe:
		return "promoted-recipe"
	case recovery.RepairModeEscalate:
		if m.ResolutionKind == "budget-guard" {
			return "budget-guard"
		}
		if m.ResolutionKind == "human-guidance" {
			return "human-guidance"
		}
		return "escalate"
	case recovery.RepairModeMechanical, recovery.RepairModeWorker:
		if o.RepairAction == "resummon" {
			return "fallback"
		}
		return "claude"
	case recovery.RepairModeNoop:
		return "noop"
	default:
		return "unknown"
	}
}
