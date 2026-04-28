// Package runtime defines the canonical worker-runtime contract types that
// flow between pkg/executor (orchestrator) and pkg/agent (spawner). It is a
// types-only package with no behavior.
//
// The package exists solely to break the import cycle that would otherwise
// form once SpawnConfig embeds executor-owned runtime types: pkg/executor
// already imports pkg/agent for Backend/SpawnConfig, so pkg/agent cannot
// import pkg/executor for these types. A neutral third package breaks the
// cycle. pkg/executor re-exports the types via aliases in
// pkg/executor/runtime_contract.go so executor call sites keep their
// ergonomic references.
//
// The contract itself is specified in
// docs/design/spi-xplwy-runtime-contract.md §1.
package runtime

// SpawnRole describes what kind of worker to run.
//
// Moved here from pkg/agent so that RunContext can reference it without
// forcing pkg/runtime → pkg/agent (which would re-open the cycle
// pkg/agent → pkg/runtime embeds into SpawnConfig). pkg/agent keeps a
// type alias for back-compat so existing agent.SpawnRole / agent.RoleX
// call sites continue to work unchanged.
type SpawnRole string

const (
	// RoleApprentice is a per-subtask implementer.
	RoleApprentice SpawnRole = "apprentice"
	// RoleSage is a per-review agent.
	RoleSage SpawnRole = "sage"
	// RoleWizard is the executor — handles full workflow for all workload types.
	RoleWizard SpawnRole = "wizard"
	// RoleExecutor is a formula-driven executor.
	RoleExecutor SpawnRole = "executor"
)

// RepoIdentity is the canonical identity a worker resolves before any
// role code runs. It is derived from pkg/config active-tower state plus
// pkg/store repo registration — never from ambient CWD, pod env ad hoc,
// or CR-only fields. RepoIdentity is immutable for the life of a
// worker run.
type RepoIdentity struct {
	// TowerName is the dolt database name (== tower identity).
	TowerName string
	// TowerID is the stable tower id from config.ResolveActiveTower.
	TowerID string
	// Prefix is the bead prefix for this repo (e.g. "spi").
	Prefix string
	// RepoURL is the origin URL from the shared repo registration.
	RepoURL string
	// BaseBranch is the default base branch from registration (e.g. "main").
	BaseBranch string
}

// WorkspaceKind describes how a workspace is materialized.
type WorkspaceKind string

const (
	WorkspaceKindRepo             WorkspaceKind = "repo"
	WorkspaceKindOwnedWorktree    WorkspaceKind = "owned_worktree"
	WorkspaceKindBorrowedWorktree WorkspaceKind = "borrowed_worktree"
	WorkspaceKindStaging          WorkspaceKind = "staging"
)

// WorkspaceOrigin describes how the workspace substrate was produced.
type WorkspaceOrigin string

const (
	// WorkspaceOriginLocalBind means the workspace reuses an existing
	// local repo checkout (bind mount / direct path).
	WorkspaceOriginLocalBind WorkspaceOrigin = "local-bind"
	// WorkspaceOriginOriginClone means the substrate was produced by a
	// fresh clone from origin (e.g. k8s repo-bootstrap init container).
	WorkspaceOriginOriginClone WorkspaceOrigin = "origin-clone"
	// WorkspaceOriginGuildCache is reserved for phase 2 (spi-sn7o3) and
	// is not materialized by the runtime in this phase.
	WorkspaceOriginGuildCache WorkspaceOrigin = "guild-cache"
)

// WorkspaceHandle is the contract piece a backend must satisfy before
// the worker's main container/process starts. The executor produces the
// handle; the backend materializes it; the wizard consumes it by Path.
//
// Borrowed=true means same-owner continuation (e.g. a review-fix loop
// reusing the implement workspace). The caller owns cleanup, and the
// worker must not mutate the workspace outside its declared ownership
// surface. Cross-owner runs MUST set Borrowed=false.
type WorkspaceHandle struct {
	// Name is the formula workspace name.
	Name string
	// Kind is the workspace kind (repo, owned_worktree, borrowed_worktree, staging).
	Kind WorkspaceKind
	// Branch is the resolved branch name (may be empty for kind=repo).
	Branch string
	// BaseBranch is the base branch from which Branch was derived.
	BaseBranch string
	// Path is the absolute path the worker will see. This is the single
	// way the worker finds its workspace.
	Path string
	// Origin records how the substrate was produced.
	Origin WorkspaceOrigin
	// Borrowed is true iff the caller owns cleanup — i.e. same-owner
	// continuation. Cross-owner runs MUST be Borrowed=false.
	Borrowed bool
}

// HandoffMode is the selected delivery protocol for a role transition.
//
// HandoffBorrowed is NOT a delivery protocol — it is the statement that
// no delivery is needed because workspace ownership did not change. The
// canonical implement → sage-review → review-fix chain is the
// same-owner borrowed path.
//
// HandoffTransitional is explicit compatibility debt for the legacy push
// transport. It is quarantined in chunk 5a of the runtime-contract
// migration and removed in a later phase once metrics show zero use.
type HandoffMode string

const (
	// HandoffNone marks terminal/no-op transitions where no delivery happens.
	HandoffNone HandoffMode = "none"
	// HandoffBorrowed marks same-owner continuation (no delivery needed).
	HandoffBorrowed HandoffMode = "borrowed"
	// HandoffBundle is the canonical cross-owner delivery protocol.
	HandoffBundle HandoffMode = "bundle"
	// HandoffTransitional is the quarantined legacy push transport.
	HandoffTransitional HandoffMode = "transitional"
)

// RunContext is the identity set that every worker run must propagate
// through logs, traces, and metrics. It is assembled by the executor at
// role dispatch and passed to backends via SpawnConfig.
//
// Metric cardinality is controlled: BeadID, AttemptID, and RunID stay
// OFF high-frequency metric labels and live in logs/traces only. The
// lower-cardinality fields (tower, prefix, role, backend, formula step,
// workspace kind/name/origin, handoff mode) are safe to use as labels.
type RunContext struct {
	// TowerName is the dolt database name for the run's tower.
	TowerName string
	// Prefix is the bead prefix for the run's repo.
	Prefix string
	// BeadID is the bead this run is working on (high-cardinality — logs only).
	BeadID string
	// AttemptID is the attempt bead ID (high-cardinality — logs only).
	AttemptID string
	// RunID correlates child runs to a parent invocation (high-cardinality — logs only).
	RunID string
	// AgentName is the logical agent name for the run (e.g.
	// "wizard-spi-abc", "apprentice-spi-abc-w1-0"). It is the fifth
	// identity segment in the log artifact path schema (see design
	// spi-7wzwk2) and rides on every artifact written by this run so the
	// exporter (spi-k1cnof) and gateway (spi-j3r694) can join an artifact
	// back to a specific worker without parsing pod names. High-
	// cardinality — logs/artifacts only, never on metric labels.
	AgentName string
	// Role is the worker role. Reuses SpawnRole from pkg/agent (via the
	// shared canonical definition here).
	Role SpawnRole
	// FormulaStep is the current step name (e.g. "implement", "sage-review").
	FormulaStep string
	// Backend identifies the execution environment: "process", "docker",
	// "k8s", or "operator-k8s".
	Backend string
	// WorkspaceKind is the declared workspace kind for the run.
	WorkspaceKind WorkspaceKind
	// WorkspaceName is the formula workspace name.
	WorkspaceName string
	// WorkspaceOrigin records how the workspace substrate was produced.
	WorkspaceOrigin WorkspaceOrigin
	// HandoffMode is the delivery protocol selected by the executor for
	// this role transition.
	HandoffMode HandoffMode
}
