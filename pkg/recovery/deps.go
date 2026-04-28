package recovery

import (
	"github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/store"
)

// DepBead is the minimal bead projection needed by recovery.
// Callers map from their store.Bead to this type.
//
// Description and Metadata are optional — adapters that want to surface
// resource-scoped recovery context (see FailureClass.IsResourceScoped) must
// populate them from the underlying store.Bead. Legacy wizard-failure paths
// leave them empty; extractResourceContext is nil-safe.
type DepBead struct {
	ID          string
	Title       string
	Status      string
	Description string
	Labels      []string
	Parent      string
	Metadata    map[string]string
}

// DepDependent represents a dependent bead with its dependency type.
type DepDependent struct {
	ID             string
	Title          string
	Status         string
	Labels         []string
	DependencyType string
}

// Deps abstracts all external dependencies so the recovery package can be
// tested without a real store, registry, or filesystem. Modeled after
// pkg/executor.Deps and pkg/wizard.Deps.
//
// Deps is both a strategy bundle and a context pack. Function fields are
// seams that tests fake; plain data fields carry caller-assembled context
// (git diagnostics, repeated failures, etc.) that Decide consumes as-is.
type Deps struct {
	// Store operations
	GetBead     func(id string) (DepBead, error)
	GetChildren func(parentID string) ([]DepBead, error)

	// Dependents (reverse deps) — returns beads that depend on the given ID.
	GetDependentsWithMeta func(id string) ([]DepDependent, error)

	// Dependencies (forward deps) — returns beads the given ID depends on.
	// Optional; only populated by adapters that surface resource-scoped
	// recovery context. extractResourceContext uses this to resolve a
	// wisp's caused-by edge to its pinned-identity bead.
	GetDepsWithMeta func(id string) ([]DepDependent, error)

	// Executor state — returns nil if no state file exists.
	LoadExecutorState func(agentName string) (*RuntimeState, error)

	// Git checks
	CheckBranchExists  func(repoPath, branch string) bool
	CheckWorktreeExists func(dir string) bool
	CheckWorktreeDirty  func(dir string) bool

	// Mutations
	AddComment func(id, text string) error
	CloseBead  func(id string) error

	// Wizard registry
	LookupRegistry func(beadID string) (name string, pid int, alive bool, err error)

	// Repo resolution — returns (repoPath, baseBranch, err).
	ResolveRepo func(beadID string) (string, string, error)

	// Prior recovery lookups — queries closed recovery beads by structured metadata.
	ListRecoveryLearnings func(filter store.RecoveryLookupFilter) ([]store.RecoveryLearning, error)

	// --- Decide seams (spi-nwfn1) ---
	// These seams are consumed by Decide; Diagnose / Verify / Finish do not
	// need them and leave them nil.

	// RecoveryBeadID is the bead the current Decide invocation is running
	// on. AddRecoveryBeadComment / SetRecoveryBeadMeta both implicitly target
	// this bead, so callers do not pass an id at each site.
	RecoveryBeadID string

	// ClaudeRunner invokes the Claude CLI and returns stdout bytes. label
	// is a stable per-call name used by the executor's per-invocation log
	// file. A nil runner causes Decide to fall back to resummon.
	ClaudeRunner func(args []string, label string) ([]byte, error)

	// AddRecoveryBeadComment posts a comment on the current recovery bead.
	// Nil means "no-op" — useful in tests that don't care about comment
	// side effects.
	AddRecoveryBeadComment func(text string) error

	// SetRecoveryBeadMeta writes metadata onto the current recovery bead.
	// Nil means "no-op" — the expected_outcome capture is best-effort.
	SetRecoveryBeadMeta func(meta map[string]string) error

	// Logf is an optional structured log sink used for trace-style lines
	// ("recovery: decide: auto-escalate ..."). Nil means drop.
	Logf func(format string, args ...interface{})

	// LearningStats returns aggregate outcome statistics for a failure
	// class. Typically binds to store.GetLearningStatsAuto. Nil means "no
	// stats available" — the prompt omits the historical-statistics
	// section.
	LearningStats func(failureClass string) (*store.LearningStats, error)

	// PromotionThreshold resolves the effective promotion cutoff for a
	// failure signature (repoconfig precedence chain). Nil disables the
	// promotion path entirely — Decide falls through to the agentic
	// default without attempting a recipe replay.
	PromotionThreshold func(failureSig string) int

	// CaptureDecideResult is retained as a stub field for backward
	// compatibility during the cleric-foundation transition (spi-h2d7yn).
	// The Claude-backed decide path was deleted along with pkg/recovery/decide.go;
	// callers may still set this field but it will never be invoked. Removed
	// in the cleric-runtime feature.
	CaptureDecideResult func(result interface{})

	// CaptureDecideBranch is an optional side-channel hook invoked once
	// per Decide call with the priority-ladder branch that produced the
	// returned RepairPlan: "budget", "guidance", "recipe", "claude", or
	// "fallback". Nil means "don't capture" — the steward path leaves
	// this unset; the foreground debug-dispatch path wires it through
	// to a PhaseEvent.Branch so operators see which rung fired.
	CaptureDecideBranch func(branch string)

	// --- Decide context data (spi-nwfn1) ---
	// Pre-assembled by the caller before invoking Decide. These carry data
	// that was historically read from FullRecoveryContext in the
	// executor-side implementation.

	// BranchDiagnostics is the target feature branch's ahead/behind status
	// versus main. Nil means "unknown" — the git-state heuristic treats
	// that as "no signal" and falls through.
	BranchDiagnostics *git.BranchDiagnostics

	// WorktreeDiagnostics is the target bead's worktree state. Nil means
	// "unknown" (no worktree found or diagnostics failed).
	WorktreeDiagnostics *git.WorktreeDiagnostics

	// ConflictedFiles lists tracked files with unresolved merge conflicts
	// in the target worktree. Non-empty implies a paused
	// rebase/merge/cherry-pick — Decide routes to the agentic conflict
	// resolver instead of rebasing again.
	ConflictedFiles []string

	// HumanComments is the recovery bead's human-authored comments
	// (agent-authored filtered out). Decide scans these for imperative
	// guidance.
	HumanComments []string

	// MaxAttempts caps total recovery attempts before auto-escalation.
	// 0 means "use DefaultMaxRecoveryAttempts" (3).
	MaxAttempts int

	// TriageCount is the number of prior triage attempts on the current
	// recovery bead (max 2). Used by the decide prompt's triage-budget
	// guidance.
	TriageCount int

	// FailureSignature is the stamped failure_signature from the recovery
	// bead's metadata, used as the promotion-lookup key. Empty disables
	// the promoted-recipe check.
	FailureSignature string

	// RankedActions is the mechanically-ranked set of candidate recovery
	// actions produced by collect_context.
	RankedActions []RecoveryAction

	// BeadLearnings are the per-bead reusable learnings for this source
	// bead (same bead recovering from the same failure class before).
	BeadLearnings []store.RecoveryLearning

	// CrossLearnings are cross-bead learnings for the same failure class
	// across the system.
	CrossLearnings []store.RecoveryLearning

	// WizardLogTail is the last ~100 lines of the source wizard's log,
	// embedded in the Claude prompt so decide can distinguish
	// infrastructure failures from code-level failures.
	WizardLogTail string

	// ContextSummary is a pre-rendered markdown summary of the full
	// recovery context (git state, worktree state, attempt history, human
	// comments, repeated failures). Appended to the Claude prompt when
	// non-empty.
	ContextSummary string
}
