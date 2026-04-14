package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/awell-health/spire/pkg/executor"
	"github.com/spf13/cobra"
)

// Test-replaceable vars (follow claim.go pattern).
var approveGetBeadFunc = storeGetBead
var approveGetStepBeadsFunc = storeGetStepBeads
var approveUnhookStepBeadFunc = storeUnhookStepBead
var approveUpdateBeadFunc = storeUpdateBead
var approveAddCommentFunc = storeAddComment
var approveIdentityFunc = func() (string, error) { return detectIdentity("") }
var approveSummonFunc = func(beadID string) error {
	return cmdSummon([]string{"1", "--targets", beadID})
}
var loadGraphStateForApprove = func(wizardName string) (*executor.GraphState, error) {
	return executor.LoadGraphState(wizardName, configDir)
}
var saveGraphStateForApprove = func(wizardName string, gs *executor.GraphState) error {
	return gs.Save(wizardName, configDir)
}

var approveCmd = &cobra.Command{
	Use:   "approve <bead-id> [comment]",
	Short: "Approve a hooked approval step and resume the wizard",
	Long: `Approve a bead that has a hooked approval step (status=hooked on a step bead).
Unhooks the approval step, adds an approval comment, and re-summons the wizard
to advance past the gate.

Unlike 'spire resolve', this is for beads where the agent succeeded and the human
gives the go-ahead — no recovery learning is recorded.`,
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		comment := ""
		if len(args) > 1 {
			comment = args[1]
		}
		return cmdApprove(args[0], comment)
	},
}

func cmdApprove(beadID, comment string) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	bead, err := approveGetBeadFunc(beadID)
	if err != nil {
		return fmt.Errorf("get bead %s: %w", beadID, err)
	}

	if bead.Status == "closed" {
		return fmt.Errorf("bead %s is already closed", beadID)
	}

	// Find hooked step beads under this parent.
	stepBeads, err := approveGetStepBeadsFunc(beadID)
	if err != nil {
		return fmt.Errorf("get step beads for %s: %w", beadID, err)
	}

	// Find the hooked approval gate. Accept human.approve and design-check steps —
	// these are legitimate approval gates. Failed/recovery hooks must not be cleared.
	var approvalStep *Bead
	for i, sb := range stepBeads {
		if sb.Status != "hooked" {
			continue
		}
		if containsLabel(sb, "step:human.approve") || containsLabel(sb, "step:design-check") {
			approvalStep = &stepBeads[i]
			break
		}
	}
	if approvalStep == nil {
		return fmt.Errorf("bead %s has no hooked approval gate (human.approve or design-check)", beadID)
	}

	// Unhook the approval step (sets it back to 'open').
	if err := approveUnhookStepBeadFunc(approvalStep.ID); err != nil {
		return fmt.Errorf("unhook approval step %s: %w", approvalStep.ID, err)
	}

	// Resolve identity and add approval comment.
	identity, _ := approveIdentityFunc()
	if identity == "" {
		identity = "human"
	}
	approvalMsg := fmt.Sprintf("Approved by %s", identity)
	if comment != "" {
		approvalMsg += ": " + comment
	}
	if err := approveAddCommentFunc(beadID, approvalMsg); err != nil {
		fmt.Fprintf(os.Stderr, "  (note: could not add approval comment to %s: %s)\n", beadID, err)
	}

	// Update graph state: reset ONLY the approved step to pending and bump CompletedCount
	// so actionHumanApprove sees CompletedCount > 0 and returns approved.
	// Derive the graph step name from the step bead's label (e.g., "step:human.approve" → "human.approve").
	approvedStepName := ""
	for _, l := range approvalStep.Labels {
		if strings.HasPrefix(l, "step:") {
			approvedStepName = strings.TrimPrefix(l, "step:")
			break
		}
	}
	wizardName := "wizard-" + beadID
	if approvedStepName != "" {
		if gs, gsErr := loadGraphStateForApprove(wizardName); gsErr == nil && gs != nil {
			if ss, ok := gs.Steps[approvedStepName]; ok && ss.Status == "hooked" {
				ss.Status = "pending"
				ss.CompletedCount++
				gs.Steps[approvedStepName] = ss
			}
			gs.ActiveStep = ""
			if saveErr := saveGraphStateForApprove(wizardName, gs); saveErr != nil {
				fmt.Fprintf(os.Stderr, "  (note: could not update graph state: %s)\n", saveErr)
			}
		}
	}

	// Check if any other step beads are still hooked; if not, set parent to in_progress.
	anyHooked := false
	for _, sb := range stepBeads {
		if sb.ID == approvalStep.ID {
			continue
		}
		if sb.Status == "hooked" {
			anyHooked = true
			break
		}
	}
	if !anyHooked {
		if err := approveUpdateBeadFunc(beadID, map[string]interface{}{"status": "in_progress"}); err != nil {
			fmt.Fprintf(os.Stderr, "  (note: could not set %s to in_progress: %s)\n", beadID, err)
		}
	}

	fmt.Printf("%sApproved %s%s\n", bold, beadID, reset)
	if comment != "" {
		fmt.Printf("  comment: %q\n", comment)
	}

	// Re-summon wizard to pick up the approved step.
	fmt.Printf("  %s↑ re-summoning wizard for %s%s\n", cyan, beadID, reset)
	if err := approveSummonFunc(beadID); err != nil {
		return fmt.Errorf("re-summon %s: %w", beadID, err)
	}

	return nil
}
