// Package executor implements the formula execution engine.
//
// The Executor drives a bead through its formula's phase pipeline: design
// validation, planning, implementation (direct or wave dispatch), review,
// and merge. It relies on injected dependencies (Deps) to avoid importing
// cmd/spire-specific code.
package executor

import (
	"context"
	"database/sql"
	"io"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/bundlestore"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/metrics"
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// Type aliases re-exported for callers that need them.
type (
	Bead             = store.Bead
	BoardBead        = store.BoardBead
	CreateOpts       = store.CreateOpts
	FormulaStepGraph = formula.FormulaStepGraph
	StepConfig       = formula.StepConfig
	RevisionPolicy   = formula.RevisionPolicy
	Backend          = agent.Backend
	SpawnConfig      = agent.SpawnConfig
	TowerConfig      = config.TowerConfig
	AgentRun         = metrics.AgentRun
)

// AuthPool is the executor-facing seam onto pkg/auth/pool's Selector. It is
// implemented in cmd/spire (where the concrete *pool.Selector is constructed)
// and used by the executor's dispatch sites to reserve a credential slot per
// spawned apprentice/sage/cleric. The interface keeps pkg/executor free of a
// direct pkg/auth/pool import so tests can stub the dispatch path without
// pulling in the cache + flock machinery.
//
// Acquire reserves a slot for the given dispatchID and returns a PoolLease.
// The lease carries the slot's env-var bindings (suitable for SpawnConfig.AuthEnv),
// the slot name (for SpawnConfig.AuthSlot and observability), the pool state
// directory (so the spawned subprocess can write rate-limit events back), and
// a Release closure the caller MUST defer. Release runs Selector.Release plus
// cancels the heartbeat goroutine; calling it twice is safe (the second call
// is a no-op).
//
// On *pool.ErrAllRateLimited (every slot rejected, no fallback available)
// Acquire returns the typed error so the caller can park the dispatch with
// the carried ResetsAt.
type AuthPool interface {
	Acquire(ctx context.Context, dispatchID string) (PoolLease, error)
}

// PoolLease is the per-dispatch reservation of an auth-pool slot. It is
// returned by AuthPool.Acquire and consumed by acquireAuthPoolSlot, which
// stamps the lease onto SpawnConfig before the spawn and defers Release.
type PoolLease struct {
	// SlotName is the name of the picked slot ("default", "personal", etc.).
	// Empty when the AuthPool dep is nil — callers should treat that as "no
	// pool configured, fall through to legacy AuthEnv-from-bead-context".
	SlotName string

	// PoolName is the pool the slot was drawn from ("subscription" or
	// "api-key"). Empty when no pool was acquired.
	PoolName string

	// AuthEnv is the env-var slice ready to be assigned to
	// SpawnConfig.AuthEnv. It contains exactly one entry — either
	// CLAUDE_CODE_OAUTH_TOKEN=<token> for the subscription pool or
	// ANTHROPIC_API_KEY=<key> for the api-key pool.
	AuthEnv []string

	// PoolStateDir is the directory containing the pool's per-slot state
	// JSON files. The spawned subprocess uses this (via
	// SPIRE_AUTH_POOL_STATE_DIR env) to apply rate-limit events scraped
	// from the claude JSONL stream back to the slot's on-disk state.
	PoolStateDir string

	// Release is the cleanup func the caller MUST defer. It cancels the
	// heartbeat goroutine bound to this lease and invokes the underlying
	// Selector.Release. Idempotent — safe to call multiple times. Nil
	// only when AuthPool is itself nil (in which case Acquire returned an
	// empty PoolLease and no cleanup is needed).
	Release func()
}

// IsErrAllRateLimited reports whether err is the pool-exhaustion sentinel
// returned by AuthPool.Acquire. The executor uses this to park a dispatch
// (Hooked=true) until the carried ResetsAt rather than fail it outright.
// Implementations satisfy this by wrapping pool.*ErrAllRateLimited; tests
// can return a value that errors.As'es to *RateLimitedError to exercise
// the parking path without importing pkg/auth/pool.
type RateLimitedError struct {
	// ResetsAt is the soonest moment the pool may regain capacity. Zero
	// when no reset hint was carried by the underlying signal.
	ResetsAt time.Time

	// Wrapped is the original error from the pool layer, kept so callers
	// that want a richer message can chain through Unwrap.
	Wrapped error
}

// Error implements error.
func (e *RateLimitedError) Error() string {
	if e == nil {
		return ""
	}
	if e.Wrapped != nil {
		return e.Wrapped.Error()
	}
	if e.ResetsAt.IsZero() {
		return "auth pool: all slots rate-limited (no reset hint)"
	}
	return "auth pool: all slots rate-limited; soonest reset at " + e.ResetsAt.Format(time.RFC3339)
}

// Unwrap returns the original pool-layer error so errors.Is / errors.As
// work transparently.
func (e *RateLimitedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Wrapped
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
	// Graph state persistence (file-backed local, Dolt-backed cluster)
	GraphStateStore GraphStateStore

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
	AddDepTyped      func(issueID, dependsOnID, depType string) error
	GetDepsWithMeta       func(id string) ([]*beads.IssueWithDependencyMetadata, error)
	GetDependentsWithMeta func(id string) ([]*beads.IssueWithDependencyMetadata, error)
	GetBlockedIssues      func(filter beads.WorkFilter) ([]BoardBead, error)
	GetReviewBeads   func(parentID string) ([]Bead, error)
	ListBeads        func(filter beads.IssueFilter) ([]Bead, error)

	// Attempt operations
	CreateAttemptBead      func(parentID, agentName, model, branch string) (string, error)
	CloseAttemptBead       func(attemptID, result string) error
	GetActiveAttempt       func(parentID string) (*Bead, error)
	StampAttemptInstance   func(attemptID string, m store.InstanceMeta) error
	IsOwnedByInstance      func(attemptID, instanceID string) (bool, error)
	GetAttemptInstance     func(attemptID string) (*store.InstanceMeta, error)
	UpdateAttemptHeartbeat func(attemptID string) error

	// Step bead operations
	CreateStepBead   func(parentID, stepName string) (string, error)
	ActivateStepBead func(stepID string) error
	// ReopenStepBead is the rewind-reconciliation counterpart to
	// ActivateStepBead: it transitions a closed/hooked step bead back to
	// "open" without marking it active. Callers must use this when the graph
	// step is "pending" (e.g. after a reset rewound the graph) so the parent
	// bead surface does not show non-active steps as in_progress (spi-ogo3wv).
	ReopenStepBead func(stepID string) error
	CloseStepBead  func(stepID string) error
	HookStepBead   func(stepID string) error
	UnhookStepBead func(stepID string) error

	// Agent registry. RegistryAdd is intentionally absent — backend.Spawn is
	// the sole creator of registry entries (see pkg/agent/README.md "Registry
	// lifecycle"). RunGraph stamps Phase via registry.Update directly.
	RegistryRemove func(name string) error

	// Resolution
	ResolveRepo   func(beadID string) (repoPath, repoURL, baseBranch string, err error)
	ResolveBranch func(beadID string) string // returns branch name from repoconfig pattern
	GetPhase      func(b Bead) string

	// Tower / identity
	ActiveTowerConfig func() (*TowerConfig, error)
	ArchmageGitEnv    func(tower *TowerConfig) []string

	// Config
	ConfigDir  func() (string, error)
	RepoConfig func() *repoconfig.RepoConfig // nil-safe; returns nil if unavailable

	// Spawner
	Spawner Backend

	// AuthPool, when non-nil, gates each apprentice/sage/cleric spawn on a
	// successful Acquire against the multi-token auth pool. The lease is
	// released after the spawn's Wait returns. Nil means "no pool
	// configured" — every dispatch falls through to the legacy
	// AuthEnv-from-bead-context path. Constructed in cmd/spire (where
	// pkg/auth/pool's Selector lives) and stamped onto Deps before
	// RunGraph is called.
	AuthPool AuthPool

	// ClusterChildDispatcher, when non-nil AND tower mode is
	// cluster-native, replaces direct Spawner.Spawn for executor-driven
	// child work (step/implement/fix dispatch in graph_actions.go and
	// action_dispatch.go, plus the wizard's review-fix re-entry). The
	// dispatcher publishes a WorkloadIntent through the .1-introduced
	// intent plane and the operator materializes the child pod. In
	// local-native mode this stays nil and dispatch falls through to
	// Spawner.Spawn unchanged. See cluster_dispatch.go for the seam
	// contract; the cluster branch fails closed (no Spawner.Spawn
	// fallback) so a missing dispatcher in cluster-native is an explicit
	// configuration error rather than silent local execution.
	ClusterChildDispatcher ClusterChildDispatcher

	// BundleStore is the artifact store the wizard consumes apprentice
	// bundles from. Nil when unavailable (tests or older setups) — dispatch
	// sites must nil-check and fall back to the legacy branch-merge path.
	BundleStore bundlestore.BundleStore

	// MaxApprentices caps the number of concurrent apprentice subprocesses
	// spawned during wave dispatch. 0 means "use built-in default" — the
	// executor resolves to repoconfig.DefaultMaxApprentices (3). Per-step
	// overrides via step.With["max-apprentices"] take precedence.
	MaxApprentices int

	// ClericRetryCap bounds how many consecutive cleric escalation
	// failures a recovery bead may suppress before the bead is closed
	// and labeled `needs-human`. 0 means "use built-in default" — the
	// executor resolves to DefaultClericRetryCap (25). Resolved by the
	// bridge from env SPIRE_CLERIC_RETRY_CAP > tower.ClericRetryCap >
	// 0. spi-1u84ec.
	ClericRetryCap int

	// Agent run recording
	RecordAgentRun func(run AgentRun) (string, error)

	// AgentResultDir returns the directory containing result.json for the named agent.
	// Path: <doltGlobalDir>/wizards/<agentName>
	AgentResultDir func(agentName string) string

	// Claude runner. logOut receives a live tee of the subprocess stdout and
	// stderr; the returned []byte is stdout only (for parsing). Callers
	// typically go through (*Executor).runClaude() which opens a
	// per-invocation log file and fills logOut, so operators can drill from
	// high-level wizard logs down to raw claude output and the tee survives
	// if the wizard dies mid-call.
	ClaudeRunner func(args []string, dir string, logOut io.Writer) ([]byte, error)

	// Focus context
	CaptureFocus func(beadID string) (string, error)

	// Review DAG callbacks
	ReviewHandleApproval    func(beadID, reviewerName, branch, baseBranch, repoPath string, log func(string, ...interface{})) error
	ReviewEscalateToArbiter func(beadID, reviewerName string, lastReview *Review, policy RevisionPolicy, log func(string, ...interface{})) error
	ReviewBeadVerdict       func(b Bead) string

	// Bead predicates
	IsAttemptBead    func(b Bead) bool
	IsStepBead       func(b Bead) bool
	IsReviewRoundBead func(b Bead) bool

	// HardResetBead performs a full destructive reset: kills wizard, deletes
	// worktree, branches, graph state, internal DAG beads, and sets bead to open.
	// Wired from cmd/spire where the git/config/registry machinery lives.
	HardResetBead func(beadID string) error

	// Dolt DB handle for recovery attempt tracking. Nil when no Dolt
	// server is available (local execution). Callers must nil-check.
	DoltDB func() *sql.DB

	// Metadata
	SetBeadMetadata func(id string, meta map[string]string) error

	// WriteRecoveryOutcome persists a RecoveryOutcome through the sole
	// writer in pkg/recovery. Exposed as a dep so tests can inject a
	// failing writer without having to stub the store. Production wires
	// this to recovery.WriteOutcome; the executor's writeRecoveryOutcome
	// helper falls back to that default when unset so legacy tests that
	// don't exercise the learn path don't have to wire it.
	WriteRecoveryOutcome func(ctx context.Context, bead *store.Bead, out recovery.RecoveryOutcome) error

	// Label / type helpers
	HasLabel       func(b Bead, prefix string) string
	ContainsLabel  func(b Bead, label string) bool
	ParseIssueType func(s string) beads.IssueType

	// OnStepCompleted is an optional observer invoked by RunGraph after
	// every step finishes — including error-recorded and hooked
	// outcomes. stepOutputs is a snapshot of the step's outputs; err is
	// non-nil when the step errored (hooked or on_error=record path).
	// Used by the foreground debug-dispatch path to translate each step
	// completion into a recovery.PhaseEvent for the cleric observability
	// surface. Nil means "don't observe" — production paths leave it
	// unset and RunGraph skips the callback.
	OnStepCompleted func(stepName string, stepOutputs map[string]string, err error)
}
