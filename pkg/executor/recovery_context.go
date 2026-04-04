package executor

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
)

// RecoveryContext is the assembled context for a recovery bead's collect_context
// phase. It bundles the diagnosis, ranked actions, and prior learnings (both
// per-bead and cross-bead) so the decide step has a complete picture.
type RecoveryContext struct {
	SourceBeadID       string                  `json:"source_bead_id"`
	FailureClass       string                  `json:"failure_class"`
	FailureSig         string                  `json:"failure_sig"`
	Diagnosis          recovery.Diagnosis      `json:"diagnosis"`
	RankedActions      []recovery.RecoveryAction `json:"ranked_actions"`
	PerBeadLearnings   []store.RecoveryLearning `json:"per_bead_learnings"`
	CrossBeadLearnings []store.RecoveryLearning `json:"cross_bead_learnings"`
}

// ToMarkdown renders the recovery context as markdown suitable for Claude prompt
// injection. The decide step reads this as a bead comment.
func (rc *RecoveryContext) ToMarkdown() string {
	var sb strings.Builder

	// ## Failure
	sb.WriteString("## Failure\n\n")
	sb.WriteString(fmt.Sprintf("- **Failure class:** %s\n", rc.FailureClass))
	if rc.FailureSig != "" {
		sb.WriteString(fmt.Sprintf("- **Failure signature:** %s\n", rc.FailureSig))
	}
	sb.WriteString(fmt.Sprintf("- **Source bead:** %s\n", rc.SourceBeadID))
	sb.WriteString(fmt.Sprintf("- **Attempt count:** %d\n", rc.Diagnosis.AttemptCount))
	if rc.Diagnosis.LastAttemptResult != "" {
		sb.WriteString(fmt.Sprintf("- **Last attempt result:** %s\n", rc.Diagnosis.LastAttemptResult))
	}
	if rc.Diagnosis.StepContext != nil {
		sb.WriteString(fmt.Sprintf("- **Failed step:** %s", rc.Diagnosis.StepContext.StepName))
		if rc.Diagnosis.StepContext.Action != "" {
			sb.WriteString(fmt.Sprintf(" (action=%s)", rc.Diagnosis.StepContext.Action))
		}
		sb.WriteString("\n")
	}
	sb.WriteString("\n")

	// ## Diagnosis
	sb.WriteString("## Diagnosis\n\n")
	if rc.Diagnosis.Git != nil {
		sb.WriteString(fmt.Sprintf("- **Branch:** %s (exists=%t)\n", rc.Diagnosis.Git.BranchName, rc.Diagnosis.Git.BranchExists))
		if rc.Diagnosis.Git.WorktreeExists {
			sb.WriteString(fmt.Sprintf("- **Worktree:** exists (dirty=%t)\n", rc.Diagnosis.Git.WorktreeDirty))
		}
	}
	if rc.Diagnosis.WizardRunning {
		sb.WriteString(fmt.Sprintf("- **Wizard:** %s (running)\n", rc.Diagnosis.WizardName))
	} else if rc.Diagnosis.WizardName != "" {
		sb.WriteString(fmt.Sprintf("- **Wizard:** %s (not running)\n", rc.Diagnosis.WizardName))
	}
	if rc.Diagnosis.Phase != "" {
		sb.WriteString(fmt.Sprintf("- **Phase:** %s\n", rc.Diagnosis.Phase))
	}
	if len(rc.Diagnosis.AlertBeads) > 0 {
		sb.WriteString("- **Alerts:**")
		for _, a := range rc.Diagnosis.AlertBeads {
			sb.WriteString(fmt.Sprintf(" %s(%s)", a.ID, a.Label))
		}
		sb.WriteString("\n")
	}
	sb.WriteString("\n")

	// ## Ranked Actions
	sb.WriteString("## Ranked Actions\n\n")
	if len(rc.RankedActions) == 0 {
		sb.WriteString("*<none available>*\n")
	} else {
		for i, a := range rc.RankedActions {
			sb.WriteString(fmt.Sprintf("%d. **%s** — %s", i+1, a.Name, a.Description))
			if a.Warning != "" {
				sb.WriteString(fmt.Sprintf(" ⚠ %s", a.Warning))
			}
			if a.Destructive {
				sb.WriteString(" [destructive]")
			}
			sb.WriteString("\n")
		}
	}
	sb.WriteString("\n")

	// ## Prior Learnings (this bead)
	sb.WriteString("## Prior Learnings (this bead)\n\n")
	if len(rc.PerBeadLearnings) == 0 {
		sb.WriteString("*<none recorded>*\n")
	} else {
		for _, l := range rc.PerBeadLearnings {
			sb.WriteString(fmt.Sprintf("- **%s** (%s) resolved %s", l.ResolutionKind, l.VerificationStatus, l.ResolvedAt))
			if l.LearningSummary != "" {
				sb.WriteString(fmt.Sprintf("\n  > %s", l.LearningSummary))
			}
			sb.WriteString("\n")
		}
	}
	sb.WriteString("\n")

	// ## Similar Incidents (cross-bead)
	sb.WriteString("## Similar Incidents (cross-bead)\n\n")
	if len(rc.CrossBeadLearnings) == 0 {
		sb.WriteString("*<none recorded>*\n")
	} else {
		sb.WriteString("*lower confidence — different bead context*\n\n")
		for _, l := range rc.CrossBeadLearnings {
			sb.WriteString(fmt.Sprintf("- **%s** (source: %s, %s) resolved %s", l.ResolutionKind, l.SourceBead, l.VerificationStatus, l.ResolvedAt))
			if l.LearningSummary != "" {
				sb.WriteString(fmt.Sprintf("\n  > %s", l.LearningSummary))
			}
			sb.WriteString("\n")
		}
	}

	return sb.String()
}

// ToJSON serializes the recovery context for in-process storage.
func (rc *RecoveryContext) ToJSON() ([]byte, error) {
	return json.Marshal(rc)
}
