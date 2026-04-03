// Package recovery provides diagnosis and recovery action proposals for
// interrupted parent beads. It does not own interruption signaling (that's
// executor) or display (that's board). It delegates action execution to
// existing Spire commands. The steward imports this package for automated
// recovery decisions.
package recovery

import "time"

// FailureClass categorizes the interruption reason from an interrupted:* label.
type FailureClass string

const (
	FailEmptyImplement FailureClass = "empty-implement"
	FailMerge          FailureClass = "merge-failure"
	FailBuild          FailureClass = "build-failure"
	FailReviewFix      FailureClass = "review-fix"
	FailRepoResolution FailureClass = "repo-resolution"
	FailArbiter        FailureClass = "arbiter"
	FailStepFailure    FailureClass = "step-failure" // v3 graph step failure
	FailUnknown        FailureClass = "unknown"
)

// StepContext captures the v3 graph step that failed, if available.
type StepContext struct {
	StepName  string `json:"step_name"`
	Action    string `json:"action,omitempty"`
	Flow      string `json:"flow,omitempty"`
	Workspace string `json:"workspace,omitempty"`
}

// RecoveryRef identifies an open recovery bead linked to an interrupted parent.
type RecoveryRef struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// Diagnosis is the complete diagnostic report for an interrupted parent bead.
type Diagnosis struct {
	BeadID            string           `json:"bead_id"`
	Title             string           `json:"title"`
	Status            string           `json:"status"`
	FailureMode       FailureClass     `json:"failure_mode"`
	InterruptLabel    string           `json:"interrupt_label"`              // raw interrupted:* label
	Phase             string           `json:"phase,omitempty"`              // current phase:* label value
	AttemptCount      int              `json:"attempt_count"`                // total attempts on this bead
	LastAttemptResult string           `json:"last_attempt_result,omitempty"` // result label from most recent closed attempt
	StepContext       *StepContext     `json:"step_context,omitempty"`       // v3: which graph step failed
	Runtime           *RuntimeState    `json:"runtime,omitempty"`            // executor state if available
	Git               *GitState        `json:"git,omitempty"`                // branch/worktree existence
	AlertBeads        []AlertInfo      `json:"alert_beads,omitempty"`        // related alert bead IDs + labels
	RecoveryBead      *RecoveryRef     `json:"recovery_bead,omitempty"`      // open recovery-for dependent if present
	WizardRunning     bool             `json:"wizard_running"`
	WizardName        string           `json:"wizard_name,omitempty"`
	Actions           []RecoveryAction `json:"actions"`
}

// RecoveryAction is a proposed recovery action with its metadata.
type RecoveryAction struct {
	Name        string `json:"name"`        // "resummon", "reset-to-<phase>", "reset-hard", "close"
	Description string `json:"description"`
	Destructive bool   `json:"destructive"`
	Warning     string `json:"warning,omitempty"` // e.g. "3 prior attempts with same failure"
	Equivalent  string `json:"equivalent"`        // equivalent spire CLI command for display
}

// RuntimeState captures executor state.json if available.
type RuntimeState struct {
	Phase         string            `json:"phase"`
	Wave          int               `json:"wave"`
	WorktreeDir   string            `json:"worktree_dir,omitempty"`
	StagingBranch string            `json:"staging_branch,omitempty"`
	AttemptBeadID string            `json:"attempt_bead_id,omitempty"`
	StepBeadIDs   map[string]string `json:"step_bead_ids,omitempty"`
}

// GitState captures branch/worktree existence for the bead's feature branch.
type GitState struct {
	BranchExists   bool   `json:"branch_exists"`
	WorktreeExists bool   `json:"worktree_exists"`
	WorktreeDirty  bool   `json:"worktree_dirty"`
	BranchName     string `json:"branch_name"`
}

// AlertInfo describes a related alert bead.
type AlertInfo struct {
	ID    string `json:"id"`
	Label string `json:"label"` // alert:* label
}

// ResolutionKind classifies how a recovery was resolved.
type ResolutionKind string

const (
	ResolutionResetHard ResolutionKind = "reset-hard"
	ResolutionRebase    ResolutionKind = "rebase"
	ResolutionResummon  ResolutionKind = "resummon"
	ResolutionManual    ResolutionKind = "manual"
	ResolutionUnknown   ResolutionKind = "unknown"
)

// VerificationStatus classifies the health of the source bead after recovery.
type VerificationStatus string

const (
	VerifyHealthy  VerificationStatus = "healthy"
	VerifyDegraded VerificationStatus = "degraded"
	VerifyUnknown  VerificationStatus = "unknown"
)

// RecoveryLearning is the durable projection written at document/finish time.
// It captures what failed, what was tried, and what fixed it so future
// recoveries can reuse the learning.
type RecoveryLearning struct {
	ResolutionKind     ResolutionKind
	VerificationStatus VerificationStatus
	LearningKey        string    // short slug for dedup lookups, e.g. "implement-merge-conflict"
	Reusable           bool      // true if this learning applies to future similar failures
	ResolvedAt         time.Time
	Narrative          string    // human-readable: what failed, what was tried, what fixed it
}

// RecoveryDeps abstracts store operations needed by the verify/document/finish
// recovery lifecycle. Satisfied by executor-side adapters wrapping executor.Deps.
type RecoveryDeps interface {
	GetBead(id string) (DepBead, error)
	GetDependentsWithMeta(id string) ([]DepDependent, error)
	UpdateBead(id string, meta map[string]interface{}) error
	AddComment(id, text string) error
	CloseBead(id string) error
}

// VerifyResult is the post-recovery verification check.
type VerifyResult struct {
	Clean           bool               `json:"clean"`
	Healthy         bool               `json:"healthy"`
	Status          VerificationStatus `json:"status"`
	Reason          string             `json:"reason,omitempty"`
	Checks          []string           `json:"checks,omitempty"`
	InterruptLabels []string           `json:"interrupt_labels,omitempty"` // any remaining interrupted:* labels
	NeedsHuman      bool               `json:"needs_human"`
	AlertsOpen      int                `json:"alerts_open"`
}

// Exit codes for --auto mode (steward signals).
const (
	ExitSuccess          = 0
	ExitDiagnosisError   = 1
	ExitAllDestructive   = 2 // all proposed actions are destructive — steward should escalate
	ExitWizardRunning    = 3 // wizard still running — wait and retry
)
