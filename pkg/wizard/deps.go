// Package wizard implements the wizard lifecycle: autonomous implementation
// (apprentice run), code review (sage review), and wizard epic
// orchestration. All external dependencies are injected via the Deps struct
// so this package has no dependency on cmd/spire internals.
package wizard

import (
	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/bundlestore"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/executor"
	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// Re-exported type aliases so callers inside this package (and bridge code)
// can refer to them without importing the underlying packages.
type (
	Bead           = store.Bead
	BoardBead      = store.BoardBead
	CreateOpts     = store.CreateOpts
	RevisionPolicy = formula.RevisionPolicy
	Backend        = agent.Backend
	SpawnConfig    = agent.SpawnConfig
	Handle         = agent.Handle
	Entry          = agent.Entry
	SpawnRole      = agent.SpawnRole
	TowerConfig    = config.TowerConfig
)

// Role constants re-exported for convenience.
const (
	RoleApprentice = agent.RoleApprentice
	RoleSage       = agent.RoleSage
)

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

// InboxMessage is a single message in the inbox file.
type InboxMessage struct {
	ID        string `json:"id"`
	From      string `json:"from"`
	Ref       string `json:"ref,omitempty"`
	Text      string `json:"text"`
	Priority  int    `json:"priority"`
	CreatedAt string `json:"created_at"`
}

// InboxFile is the structure of the inbox.json file.
type InboxFile struct {
	Agent     string         `json:"agent"`
	UpdatedAt string         `json:"updated_at"`
	Messages  []InboxMessage `json:"messages"`
}

// Deps bundles all external dependencies the wizard package needs.
// Injected by the caller (cmd/spire) so this package has no dependency
// on cmd/spire internals.
type Deps struct {
	// Store operations
	GetBead         func(id string) (Bead, error)
	ListBeads       func(filter beads.IssueFilter) ([]Bead, error)
	GetChildren     func(parentID string) ([]Bead, error)
	GetComments     func(id string) ([]*beads.Comment, error)
	AddComment      func(id, text string) error
	CreateBead      func(opts CreateOpts) (string, error)
	CloseBead       func(id string) error
	UpdateBead      func(id string, updates map[string]interface{}) error
	AddLabel        func(id, label string) error
	RemoveLabel     func(id, label string) error

	// Review bead operations
	GetReviewBeads    func(parentID string) ([]Bead, error)
	CreateReviewBead  func(parentID, sageName string, round int) (string, error)
	CloseReviewBead   func(reviewID, verdict, summary string, errorCount, warningCount, round int, findings []store.ReviewFinding) error

	// Label / type helpers
	HasLabel          func(b Bead, prefix string) string
	ContainsLabel     func(b Bead, label string) bool
	ReviewRoundNumber func(b Bead) int
	ReviewBeadVerdict func(b Bead) string

	// Phase helpers
	GetPhase func(b Bead) string

	// Agent registry. RegistryAdd is intentionally absent — backend.Spawn is
	// the sole creator of registry entries (see pkg/agent/README.md "Registry
	// lifecycle"). Wizard / handoff code only Removes (cleanup on spawn
	// failure) and Updates (stamp Phase / PID after spawn).
	RegistryRemove func(name string) error
	RegistryUpdate func(name string, f func(*Entry)) error

	// Agent spawner
	ResolveBackend func(name string) Backend

	// ClusterChildDispatcher, when non-nil AND the active tower's mode
	// is cluster-native, replaces the direct backend.Spawn call at the
	// review-fix re-entry site (ReviewHandleRequestChanges) with a
	// WorkloadIntent emit through the .1-introduced cluster intent
	// plane. Operator routing materializes the apprentice pod with
	// Role=apprentice / Phase=review-fix per intent.Allowed. Local-
	// native deployments leave this nil and the existing
	// ResolveBackend("").Spawn(...) path is preserved unchanged.
	//
	// The seam type lives in pkg/executor (see
	// pkg/executor/cluster_dispatch.go) so executor and wizard share
	// one named contract; this field is a re-export of that interface
	// rather than a parallel definition.
	ClusterChildDispatcher executor.ClusterChildDispatcher

	// Resolution
	ResolveRepo   func(beadID string) (repoPath, repoURL, baseBranch string, err error)
	ResolveBranch func(beadID string, repoPath string) string

	// Config
	ConfigDir         func() (string, error)
	ActiveTowerConfig func() (*TowerConfig, error)
	DoltGlobalDir     func() string
	RequireDolt       func() error
	ResolveBeadsDir   func() string
	LoadConfig        func() (*config.SpireConfig, error)

	// Dolt queries (for wizardResolveRepo)
	RawDoltQuery  func(query string) (string, error)
	ParseDoltRows func(out string, columns []string) []map[string]string
	SQLEscape     func(s string) string
	ResolveDatabase func(cfg *config.SpireConfig) (string, bool)

	// Executor terminal steps
	TerminalMerge   func(beadID, branch, baseBranch, repoPath, buildCmd string, log func(string, ...interface{})) error
	TerminalSplit   func(beadID, reviewerName string, splitTasks []SplitTask, log func(string, ...interface{})) error
	TerminalDiscard func(beadID string, log func(string, ...interface{})) error
	EscalateHumanFailure func(beadID, agentName, failureType, message string)
	ComputeWaves         func(epicID string) ([][]string, error)

	// Focus / bead JSON
	CaptureFocus func(beadID string) (string, error)
	GetBeadJSON  func(beadID string) (string, error)

	// Inbox
	ReadInboxFile func(agentName string) ([]byte, error)

	// CLI commands (thin delegation — these stay in cmd/spire)
	CmdClaim func(args []string) error
	CmdSend  func(args []string) error

	// NewBundleStore constructs a BundleStore for apprentice bundle delivery.
	// Only called when the configured apprentice transport is "bundle".
	NewBundleStore func() (bundlestore.BundleStore, error)
}

// SplitTask represents a follow-on task created when an arbiter decides to split.
type SplitTask struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}
