package recovery

import (
	"fmt"
	"strings"
)

// classifyInterruptLabel parses an interrupted:* label into a FailureClass.
func classifyInterruptLabel(label string) FailureClass {
	if !strings.HasPrefix(label, "interrupted:") {
		return FailUnknown
	}
	reason := strings.TrimPrefix(label, "interrupted:")
	switch reason {
	case "empty-implement":
		return FailEmptyImplement
	case "merge-failure":
		return FailMerge
	case "build-failure":
		return FailBuild
	case "review-fix", "review-fix-merge-conflict":
		return FailReviewFix
	case "repo-resolution":
		return FailRepoResolution
	case "arbiter-failure", "arbiter":
		return FailArbiter
	case "step-failure":
		return FailStepFailure
	case "cache-refresh-failure":
		return FailureClassCacheRefresh
	default:
		return FailUnknown
	}
}

// buildActions returns ranked recovery actions for a failure class,
// taking into account attempt count, git state, and bead context.
func buildActions(fc FailureClass, beadID string, attemptCount int, git *GitState) []RecoveryAction {
	var actions []RecoveryAction

	branchAvailable := git != nil && git.BranchExists

	switch fc {
	case FailEmptyImplement:
		if branchAvailable {
			actions = append(actions, resummonAction(beadID))
		}
		actions = append(actions,
			resetToAction(beadID, "design"),
			closeAction(beadID),
		)

	case FailMerge:
		if branchAvailable {
			actions = append(actions, resummonAction(beadID))
			actions = append(actions, resetToAction(beadID, "review"))
		}
		actions = append(actions, resetHardAction(beadID))

	case FailBuild:
		if branchAvailable {
			actions = append(actions, resummonAction(beadID))
			actions = append(actions, resetToAction(beadID, "implement"))
		}
		actions = append(actions, resetHardAction(beadID))

	case FailReviewFix:
		if branchAvailable {
			actions = append(actions, resummonAction(beadID))
			actions = append(actions, resetToAction(beadID, "implement"))
		}
		actions = append(actions, resetHardAction(beadID))

	case FailRepoResolution:
		actions = append(actions,
			RecoveryAction{
				Name:        "manual-fix",
				Description: "Manual fix needed: resolve repo/branch issues before retrying",
				Destructive: false,
				Equivalent:  "(manual intervention required)",
			},
			resetHardAction(beadID),
		)

	case FailArbiter:
		actions = append(actions,
			RecoveryAction{
				Name:        "manual-review",
				Description: "Manual review needed: arbiter could not resolve review disagreement",
				Destructive: false,
				Equivalent:  "(manual review required)",
			},
		)
		if branchAvailable {
			actions = append(actions, resetToAction(beadID, "review"))
		}
		actions = append(actions, closeAction(beadID))

	case FailStepFailure:
		// v3 graph step failure — diagnosis.StepContext has node details.
		if branchAvailable {
			actions = append(actions, resummonAction(beadID))
		}
		actions = append(actions, resetHardAction(beadID))

	default: // FailUnknown
		if branchAvailable {
			actions = append(actions, resummonAction(beadID))
			actions = append(actions, resetToAction(beadID, "implement"))
		}
		actions = append(actions, resetHardAction(beadID))
	}

	// Annotate resummon with warning if too many prior attempts.
	if attemptCount > 2 {
		for i := range actions {
			if actions[i].Name == "resummon" {
				actions[i].Warning = fmt.Sprintf("%d prior attempts — retry unlikely to succeed without changes", attemptCount)
			}
		}
	}

	return actions
}

func resummonAction(beadID string) RecoveryAction {
	return RecoveryAction{
		Name:        "resummon",
		Description: "Clear interrupted state and re-summon wizard",
		Destructive: false,
		Equivalent:  fmt.Sprintf("spire resummon %s", beadID),
	}
}

func resetToAction(beadID, phase string) RecoveryAction {
	return RecoveryAction{
		Name:        "reset-to-" + phase,
		Description: fmt.Sprintf("Reset to %s phase and re-summon", phase),
		Destructive: false,
		Equivalent:  fmt.Sprintf("spire reset %s --to %s", beadID, phase),
	}
}

func resetHardAction(beadID string) RecoveryAction {
	return RecoveryAction{
		Name:        "reset-hard",
		Description: "Hard reset: delete worktree, branches, and all state, then re-summon from scratch",
		Destructive: true,
		Equivalent:  fmt.Sprintf("spire reset %s --hard", beadID),
	}
}

func closeAction(beadID string) RecoveryAction {
	return RecoveryAction{
		Name:        "close",
		Description: "Close the bead (abandon work)",
		Destructive: true,
		Equivalent:  fmt.Sprintf("spire close %s", beadID),
	}
}
