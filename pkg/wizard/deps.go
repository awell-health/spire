// Package wizard implements the wizard lifecycle, review process, and workshop
// orchestration.
//
// The wizard package owns wizard and apprentice lifecycle management, worktree
// setup, prompt building, validation, commit and push handoff, review rounds,
// arbiter escalation, terminal review actions, and workshop runtime orchestration.
//
// cmd/spire provides thin command adapters that parse CLI arguments and wire
// dependencies via the Deps struct. Formula execution and ComputeWaves remain
// in pkg/executor; the workshop imports that logic rather than recreating it.
package wizard

import (
	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/executor"
	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// Type aliases re-exported for callers.
type (
	Bead           = store.Bead
	FormulaV2      = formula.FormulaV2
	RevisionPolicy = formula.RevisionPolicy
	Review         = executor.Review
	ReviewIssue    = executor.ReviewIssue
	SplitTask      = executor.SplitTask
	SubtaskState   = executor.SubtaskState
	TowerConfig    = config.TowerConfig
)

// WizardRunConfig holds the parsed arguments for a wizard-run invocation.
type WizardRunConfig struct {
	BeadID         string
	WizardName     string
	ReviewFix      bool
	ApprenticeMode bool
}

// WizardReviewConfig holds the parsed arguments for a wizard-review invocation.
type WizardReviewConfig struct {
	BeadID       string
	ReviewerName string
	VerdictOnly  bool
	WorktreeDir  string
}

// Deps bundles all external dependencies the wizard package needs. Injected by
// the caller (cmd/spire) so the wizard package has no dependency on cmd/spire
// internals.
type Deps struct {
	// Store operations
	GetBead          func(id string) (Bead, error)
	ListBeads        func(filter beads.IssueFilter) ([]Bead, error)
	GetChildren      func(parentID string) ([]Bead, error)
	GetComments      func(id string) ([]*beads.Comment, error)
	AddComment       func(id, text string) error
	CloseBead        func(id string) error
	UpdateBead       func(id string, updates map[string]interface{}) error
	AddLabel         func(id, label string) error
	RemoveLabel      func(id, label string) error
	CreateReviewBead func(parentID, sageName string, round int) (string, error)
	CloseReviewBead  func(reviewID, verdict, summary string) error
	GetReviewBeads   func(parentID string) ([]Bead, error)

	// Agent registry
	RegistryAdd    func(entry agent.Entry) error
	RegistryRemove func(name string) error
	RegistryUpdate func(name string, f func(*agent.Entry)) error

	// Spawner
	Spawner agent.Backend

	// Resolution
	ResolveRepo func(beadID string) (repoPath, repoURL, baseBranch string, err error)

	// Dolt/infrastructure
	RequireDolt   func() error
	DoltGlobalDir func() string

	// Claim
	ClaimBead func(beadID string) error

	// Focus / bead data
	CaptureFocus func(beadID string) (string, error)
	GetBeadJSON  func(beadID string) (string, error)

	// Config
	ActiveTowerConfig func() (*TowerConfig, error)
	ConfigDir         func() (string, error)
	ResolveBeadsDir   func() string

	// Formula
	LoadFormulaByName   func(name string) (*FormulaV2, error)
	ResolveBeadBuildCmd func(b Bead) string

	// Terminal actions (from pkg/executor via cmd/spire bridge)
	TerminalMerge        func(beadID, branch, baseBranch, repoPath, buildCmd string, log func(string, ...interface{})) error
	TerminalSplit        func(beadID, reviewerName string, tasks []SplitTask, log func(string, ...interface{})) error
	TerminalDiscard      func(beadID string, log func(string, ...interface{})) error
	EscalateHumanFailure func(beadID, agentName, failureType, message string)

	// Compute waves (from pkg/executor)
	ComputeWaves func(epicID string) ([][]string, error)

	// Inbox
	ReadInboxFile func(agentName string) ([]byte, error)

	// External helpers not in a package
	ReviewBeadVerdict func(b Bead) string
	GetPhase          func(b Bead) string

	// Messaging
	SendMessage func(to, message, ref, as string) error
}
