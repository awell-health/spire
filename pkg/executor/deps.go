// Package executor implements the formula execution engine.
//
// The Executor drives a bead through its formula's phase pipeline: design
// validation, planning, implementation (direct or wave dispatch), review,
// and merge. It relies on injected dependencies (Deps) to avoid importing
// cmd/spire-specific code.
package executor

import (
	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// Type aliases re-exported for callers that need them.
type (
	Bead            = store.Bead
	BoardBead       = store.BoardBead
	CreateOpts      = store.CreateOpts
	FormulaV2       = formula.FormulaV2
	PhaseConfig     = formula.PhaseConfig
	RevisionPolicy  = formula.RevisionPolicy
	Backend         = agent.Backend
	SpawnConfig     = agent.SpawnConfig
	TowerConfig     = config.TowerConfig
)

// SubtaskState tracks the status of a subtask during wave execution.
type SubtaskState struct {
	Status string `json:"status"` // "open", "in_progress", "closed", "done"
	Branch string `json:"branch"`
	Agent  string `json:"agent,omitempty"`
}

// SplitTask represents a follow-on task created when an arbiter decides to split a bead.
type SplitTask struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

// Review is the structured output from a code review.
type Review struct {
	Verdict string        `json:"verdict"` // "approve", "request_changes"
	Summary string        `json:"summary"`
	Issues  []ReviewIssue `json:"issues,omitempty"`
}

// ReviewIssue represents a single issue found during review.
type ReviewIssue struct {
	File     string `json:"file"`
	Line     int    `json:"line,omitempty"`
	Severity string `json:"severity"` // "error", "warning"
	Message  string `json:"message"`
}

// Deps bundles all external dependencies the executor needs. Injected by the
// caller (cmd/spire) so the executor package has no dependency on cmd/spire
// internals.
type Deps struct {
	// Store operations
	GetBead          func(id string) (Bead, error)
	GetChildren      func(parentID string) ([]Bead, error)
	GetComments      func(id string) ([]*beads.Comment, error)
	AddComment       func(id, text string) error
	CreateBead       func(opts CreateOpts) (string, error)
	CloseBead        func(id string) error
	UpdateBead       func(id string, updates map[string]interface{}) error
	AddLabel         func(id, label string) error
	RemoveLabel      func(id, label string) error
	AddDep           func(issueID, dependsOnID string) error
	GetDepsWithMeta  func(id string) ([]*beads.IssueWithDependencyMetadata, error)
	GetBlockedIssues func(filter beads.WorkFilter) ([]BoardBead, error)
	GetReviewBeads   func(parentID string) ([]Bead, error)

	// Attempt operations
	CreateAttemptBead func(parentID, agentName, model, branch string) (string, error)
	CloseAttemptBead  func(attemptID, result string) error
	GetActiveAttempt  func(parentID string) (*Bead, error)

	// Step bead operations
	CreateStepBead   func(parentID, stepName string) (string, error)
	ActivateStepBead func(stepID string) error
	CloseStepBead    func(stepID string) error

	// Agent registry
	RegistryAdd    func(entry agent.Entry) error
	RegistryRemove func(name string) error

	// Resolution
	ResolveRepo func(beadID string) (repoPath, repoURL, baseBranch string, err error)
	GetPhase    func(b Bead) string

	// Tower / identity
	ActiveTowerConfig func() (*TowerConfig, error)
	ArchmageGitEnv    func(tower *TowerConfig) []string

	// Config
	ConfigDir      func() (string, error)
	ResolveFormula func(b Bead) (*FormulaV2, error)

	// Spawner
	Spawner Backend

	// Claude runner
	ClaudeRunner func(args []string, dir string) ([]byte, error)

	// Focus context
	CaptureFocus func(beadID string) (string, error)

	// Review DAG callbacks
	ReviewHandleApproval    func(beadID, reviewerName, branch, baseBranch, repoPath string, log func(string, ...interface{})) error
	ReviewEscalateToArbiter func(beadID, reviewerName string, lastReview *Review, policy RevisionPolicy, log func(string, ...interface{})) error
	ReviewBeadVerdict       func(b Bead) string

	// Molecule steps
	CloseMoleculeStep func(beadID, stepName string)

	// Bead predicates
	IsAttemptBead    func(b Bead) bool
	IsStepBead       func(b Bead) bool
	IsReviewRoundBead func(b Bead) bool

	// Label / type helpers
	HasLabel       func(b Bead, prefix string) string
	ContainsLabel  func(b Bead, label string) bool
	ParseIssueType func(s string) beads.IssueType
	ResolveBeadBuildCmd func(b Bead) string
}
