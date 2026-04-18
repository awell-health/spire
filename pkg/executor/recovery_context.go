package executor

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
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
	WizardLogTail      string                   `json:"wizard_log_tail,omitempty"`
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

	// ## Wizard Log (last 100 lines)
	if rc.WizardLogTail != "" {
		sb.WriteString("\n## Wizard Log (last 100 lines)\n\n")
		sb.WriteString("```\n")
		sb.WriteString(rc.WizardLogTail)
		if !strings.HasSuffix(rc.WizardLogTail, "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString("```\n")
	}

	return sb.String()
}

// ToJSON serializes the recovery context for in-process storage.
func (rc *RecoveryContext) ToJSON() ([]byte, error) {
	return json.Marshal(rc)
}

// FullRecoveryContext is the comprehensive recovery context assembled by
// BuildRecoveryContext. It combines git diagnostics, attempt history, bead
// state, and human comments into a single structure for the recovery decide
// step. This is the git-aware successor to RecoveryContext above.
type FullRecoveryContext struct {
	RecoveryBead     store.Bead                `json:"recovery_bead"`
	TargetBead       store.Bead                `json:"target_bead"`
	GitState         *git.BranchDiagnostics    `json:"git_state,omitempty"`
	WorktreeState    *git.WorktreeDiagnostics  `json:"worktree_state,omitempty"`
	StepOutput       string                    `json:"step_output,omitempty"`
	AttemptHistory   []store.RecoveryAttempt    `json:"attempt_history,omitempty"`
	HumanComments    []string                  `json:"human_comments,omitempty"`
	FailedStep       string                    `json:"failed_step,omitempty"`
	WizardPhase      string                    `json:"wizard_phase,omitempty"`
	FailureReason    string                    `json:"failure_reason,omitempty"`
	TotalAttempts    int                       `json:"total_attempts"`
	RepeatedFailures map[string]int            `json:"repeated_failures,omitempty"`
}

// BuildRecoveryContext assembles a full recovery context by loading the
// recovery bead, its target bead, git diagnostics, attempt history, and
// human comments. Errors from individual steps (git diagnostics, comments,
// etc.) are handled gracefully — the context is returned with whatever
// information could be collected.
func BuildRecoveryContext(db *sql.DB, repoPath string, recoveryBeadID string) (*FullRecoveryContext, error) {
	// 1. Load recovery bead.
	recoveryBead, err := store.GetBead(recoveryBeadID)
	if err != nil {
		return nil, fmt.Errorf("get recovery bead %s: %w", recoveryBeadID, err)
	}

	// 2. Find target bead ID from recovery bead's source_bead metadata or parent.
	targetBeadID := recoveryBead.Meta(recovery.KeySourceBead)
	if targetBeadID == "" {
		targetBeadID = recoveryBead.Parent
	}
	if targetBeadID == "" {
		return nil, fmt.Errorf("recovery bead %s has no source_bead metadata or parent", recoveryBeadID)
	}

	// 3. Load target bead.
	targetBead, err := store.GetBead(targetBeadID)
	if err != nil {
		return nil, fmt.Errorf("get target bead %s: %w", targetBeadID, err)
	}

	ctx := &FullRecoveryContext{
		RecoveryBead:     recoveryBead,
		TargetBead:       targetBead,
		RepeatedFailures: make(map[string]int),
	}

	// 4. Parse FailedStep from recovery bead metadata or target bead labels.
	ctx.FailedStep = recoveryBead.Meta(recovery.KeySourceStep)
	if ctx.FailedStep == "" {
		ctx.FailedStep = parseFailedStepFromLabels(targetBead.Labels)
	}

	// 4b. Read SourceFlow from recovery bead metadata as the wizard-compatible phase.
	ctx.WizardPhase = recoveryBead.Meta(recovery.KeySourceFlow)

	// 5. Parse failure reason from recovery bead metadata.
	ctx.FailureReason = recoveryBead.Meta(recovery.KeyFailureClass)
	if ctx.FailureReason == "" {
		ctx.FailureReason = recoveryBead.Meta(recovery.KeyFailureSignature)
	}

	// 6. Resolve branch name for git diagnostics.
	branch := resolveBranchFromBead(targetBead)

	// 7. Call git.DiagnoseBranch — may fail if branch doesn't exist.
	if branch != "" && repoPath != "" {
		branchDiag, diagErr := git.DiagnoseBranch(repoPath, branch)
		if diagErr == nil {
			ctx.GitState = branchDiag
		}
		// Failure is informational, not fatal.
	}

	// 8. Call git.DiagnoseWorktree — may fail if worktree doesn't exist.
	if repoPath != "" {
		wtDiag, wtErr := git.DiagnoseWorktree(repoPath, targetBeadID)
		if wtErr == nil {
			ctx.WorktreeState = wtDiag
		}
	}

	// 9. Collect step output if we know the failed step and have a worktree.
	if ctx.FailedStep != "" && ctx.WorktreeState != nil && ctx.WorktreeState.Exists {
		stepOut, _ := git.CollectStepOutput(ctx.WorktreeState.Path, ctx.FailedStep)
		ctx.StepOutput = stepOut
	}

	// 10. Load attempt history from recovery_attempts table.
	if db != nil {
		attempts, attErr := store.ListRecoveryAttempts(db, recoveryBeadID)
		if attErr == nil {
			ctx.AttemptHistory = attempts
			ctx.TotalAttempts = len(attempts)
		}
	}

	// 11. Load comments on recovery bead and extract human-authored ones.
	comments, commentErr := store.GetComments(recoveryBeadID)
	if commentErr == nil {
		ctx.HumanComments = extractHumanComments(comments)
	}

	// 12. Build RepeatedFailures map from attempt history.
	for _, a := range ctx.AttemptHistory {
		if a.Outcome == "failure" {
			ctx.RepeatedFailures[a.Action]++
		}
	}

	return ctx, nil
}

// SummarizeContext returns a human-readable summary of a FullRecoveryContext
// suitable for inclusion in agent prompts.
func SummarizeContext(ctx *FullRecoveryContext) string {
	var sb strings.Builder

	sb.WriteString("# Recovery Context\n\n")

	// Target bead info.
	sb.WriteString(fmt.Sprintf("**Target bead:** %s — %s (status: %s)\n", ctx.TargetBead.ID, ctx.TargetBead.Title, ctx.TargetBead.Status))
	sb.WriteString(fmt.Sprintf("**Recovery bead:** %s\n", ctx.RecoveryBead.ID))

	if ctx.FailedStep != "" {
		sb.WriteString(fmt.Sprintf("**Failed step:** %s\n", ctx.FailedStep))
	}
	if ctx.FailureReason != "" {
		sb.WriteString(fmt.Sprintf("**Failure reason:** %s\n", ctx.FailureReason))
	}
	sb.WriteString(fmt.Sprintf("**Total attempts:** %d\n\n", ctx.TotalAttempts))

	// Git state.
	if ctx.GitState != nil {
		sb.WriteString("## Git Branch State\n\n")
		sb.WriteString(fmt.Sprintf("- Branch: %s\n", ctx.GitState.BranchRef))
		sb.WriteString(fmt.Sprintf("- Ahead of %s: %d commits\n", ctx.GitState.MainRef, ctx.GitState.AheadOfMain))
		sb.WriteString(fmt.Sprintf("- Behind %s: %d commits\n", ctx.GitState.MainRef, ctx.GitState.BehindMain))
		if ctx.GitState.Diverged {
			sb.WriteString("- **DIVERGED** from main\n")
		}
		sb.WriteString(fmt.Sprintf("- Last commit: %s %s\n\n", ctx.GitState.LastCommitHash[:minInt(8, len(ctx.GitState.LastCommitHash))], ctx.GitState.LastCommitMsg))
	} else {
		sb.WriteString("## Git Branch State\n\n*Branch diagnostics unavailable*\n\n")
	}

	// Worktree state.
	if ctx.WorktreeState != nil && ctx.WorktreeState.Exists {
		sb.WriteString("## Worktree State\n\n")
		sb.WriteString(fmt.Sprintf("- Path: %s\n", ctx.WorktreeState.Path))
		sb.WriteString(fmt.Sprintf("- Branch: %s\n", ctx.WorktreeState.Branch))
		if ctx.WorktreeState.IsDirty {
			sb.WriteString("- **Dirty** (uncommitted changes)\n")
		} else {
			sb.WriteString("- Clean\n")
		}
		if len(ctx.WorktreeState.UntrackedFiles) > 0 {
			sb.WriteString(fmt.Sprintf("- Untracked files: %d\n", len(ctx.WorktreeState.UntrackedFiles)))
		}
		sb.WriteString("\n")
	} else {
		sb.WriteString("## Worktree State\n\n*No worktree found for target bead*\n\n")
	}

	// Step output.
	if ctx.StepOutput != "" {
		sb.WriteString("## Failed Step Output\n\n```\n")
		sb.WriteString(ctx.StepOutput)
		if !strings.HasSuffix(ctx.StepOutput, "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString("```\n\n")
	}

	// Attempt history.
	if len(ctx.AttemptHistory) > 0 {
		sb.WriteString("## Attempt History\n\n")
		for _, a := range ctx.AttemptHistory {
			sb.WriteString(fmt.Sprintf("- #%d: action=%s outcome=%s", a.AttemptNumber, a.Action, a.Outcome))
			if a.Error != "" {
				sb.WriteString(fmt.Sprintf(" error=%q", a.Error))
			}
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	// Repeated failures.
	if len(ctx.RepeatedFailures) > 0 {
		sb.WriteString("## Repeated Failures\n\n")
		for action, count := range ctx.RepeatedFailures {
			sb.WriteString(fmt.Sprintf("- %s: %d failures\n", action, count))
		}
		sb.WriteString("\n")
	}

	// Human comments.
	if len(ctx.HumanComments) > 0 {
		sb.WriteString("## Human Guidance\n\n")
		for _, c := range ctx.HumanComments {
			sb.WriteString(fmt.Sprintf("> %s\n\n", c))
		}
	}

	return sb.String()
}

// parseFailedStepFromLabels scans labels for "interrupted:<step>" and returns
// the step name. Returns "" if no interrupted label is found.
func parseFailedStepFromLabels(labels []string) string {
	for _, l := range labels {
		if strings.HasPrefix(l, "interrupted:") {
			return strings.TrimPrefix(l, "interrupted:")
		}
	}
	return ""
}

// resolveBranchFromBead extracts the branch name from a bead's feat-branch:
// label, falling back to "feat/<beadID>".
func resolveBranchFromBead(b store.Bead) string {
	branch := store.HasLabel(b, "feat-branch:")
	if branch != "" {
		return branch
	}
	return "feat/" + b.ID
}

// extractHumanComments filters bead comments to return only human-authored
// comment bodies. Agent-generated comments are identified by their author
// containing known agent role prefixes.
func extractHumanComments(comments []*beads.Comment) []string {
	var result []string
	for _, c := range comments {
		if c != nil && !isAgentAuthor(c.Author) && c.Text != "" {
			result = append(result, c.Text)
		}
	}
	return result
}

// isAgentAuthor returns true if the author string looks like an agent name
// rather than a human user. This is a denylist — every new agent role needs to
// be added here. Flipping to a positive human-allowlist is a follow-up.
// "spire" is the literal author for system-posted comments (failure reports,
// retry-scheduling notices) — treat these as agent-authored so they don't
// leak into parseHumanGuidance.
func isAgentAuthor(author string) bool {
	if author == "spire" {
		return true
	}
	agentPrefixes := []string{"wizard-", "apprentice-", "sage-", "steward-", "cleric-"}
	for _, p := range agentPrefixes {
		if strings.HasPrefix(author, p) {
			return true
		}
	}
	return false
}

// minInt returns the smaller of a and b.
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
