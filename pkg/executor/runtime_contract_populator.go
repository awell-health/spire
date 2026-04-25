package executor

// runtime_contract_populator.go — exported runtime-contract helpers so
// dispatch sites outside of pkg/executor (steward review routing,
// wizard review handoff, wizard review-fix re-engagement) can populate
// Identity/Workspace/Run on a SpawnConfig without re-implementing the
// rules that (*Executor).withRuntimeContract applies internally. The
// cluster backend rejects any SpawnConfig with empty Identity or
// Workspace (ErrIdentityRequired / ErrWorkspaceRequired), so every
// direct backend.Spawn caller MUST route through this helper.

import (
	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/store"
)

// RuntimeContractInputs bundles the explicit inputs PopulateRuntimeContract
// needs to fill Identity, Workspace, and Run on a SpawnConfig. Call sites
// that don't have an Executor (steward DetectReviewReady, wizard
// WizardReviewHandoff, wizard ReviewHandleRequestChanges) fill this
// struct from their own state and call PopulateRuntimeContract so the
// canonical runtime-contract fields are non-empty before reaching the
// backend.
//
// Every field is caller-sourced; PopulateRuntimeContract has no hidden
// executor state. HandoffMode is required — see the docstring on
// PopulateRuntimeContract for the rationale.
type RuntimeContractInputs struct {
	// TowerName is the active tower (dolt database) name.
	TowerName string

	// RepoURL is the resolved origin URL for the bead's prefix (from
	// the tower's LocalBinding or the shared repos table). When empty,
	// cfg.RepoURL is used as a fallback so existing call sites that
	// threaded RepoURL through keep working.
	RepoURL string

	// RepoPath is the local repo directory on the dispatching instance.
	// Used both as the workspace kind=repo default path and as the
	// source of the base-branch fallback.
	RepoPath string

	// BaseBranch is the default branch for the repo (e.g. "main").
	// When empty, cfg.RepoBranch is used as a fallback.
	BaseBranch string

	// RunStep is the formula step name the spawn belongs to (e.g.
	// "review", "review-fix"). Empty for out-of-formula spawns.
	RunStep string

	// WorkspaceName is the formula workspace name. May be empty; the
	// normalizer will apply kind=repo defaults.
	WorkspaceName string

	// Workspace is the materialized workspace handle. May be nil; the
	// normalizer substitutes a kind=repo handle rooted at RepoPath.
	Workspace *WorkspaceHandle

	// Backend identifies the execution environment: "process",
	// "docker", "k8s", or "operator-k8s". Used to label Run.Backend.
	Backend string

	// RunID is the parent-run correlation ID for agent_runs. Empty is
	// acceptable — out-of-executor call sites (steward, wizard review
	// handoff) have no parent run to point at.
	RunID string

	// HandoffMode is the delivery protocol selected for this role
	// transition. REQUIRED — callers must pass an explicit value.
	// Pass HandoffNone for terminal/no-op spawns, HandoffBorrowed for
	// same-owner continuations (sage review, recovery-verify), and an
	// apprentice delivery mode via ApprenticeDeliveryHandoff(tower)
	// for commit-producing spawns.
	HandoffMode HandoffMode

	// Log is the optional log sink used to emit the
	// HandoffTransitional deprecation line. When nil, the counter
	// still bumps and the SPIRE_FAIL_ON_TRANSITIONAL_HANDOFF gate
	// still fires — only the Warn-level log is skipped.
	Log func(string, ...interface{})
}

// PopulateRuntimeContract fills Identity, Workspace, and Run on cfg
// from the supplied inputs. It is the canonical way to prepare a
// SpawnConfig for backend.Spawn outside of the executor's own dispatch
// path: every call site that hands a SpawnConfig directly to a Backend
// MUST route it through this helper (or (*Executor).withRuntimeContract,
// which wraps it) so the non-optional cluster-backend fields are set.
//
// HandoffMode is never auto-derived; the caller is the single authority
// on delivery semantics for its spawn site. Pass HandoffNone for
// terminal spawns, HandoffBorrowed for same-owner continuations (sage
// review, recovery-verify), and ApprenticeDeliveryHandoff(tower) for
// commit-producing spawns.
//
// As a side effect, when the selected mode is HandoffTransitional this
// function bumps spire_handoff_transitional_total, emits the Warn-level
// deprecation log via inputs.Log (when non-nil), and honors the
// SPIRE_FAIL_ON_TRANSITIONAL_HANDOFF gate. The returned error, when
// non-nil, must be propagated so the caller can fail the spawn.
func PopulateRuntimeContract(cfg agent.SpawnConfig, inputs RuntimeContractInputs) (agent.SpawnConfig, error) {
	prefix := store.PrefixFromID(cfg.BeadID)
	if prefix == "" {
		prefix = cfg.RepoPrefix
	}
	baseBranch := inputs.BaseBranch
	if baseBranch == "" {
		baseBranch = cfg.RepoBranch
	}
	repoURL := inputs.RepoURL
	if repoURL == "" {
		repoURL = cfg.RepoURL
	}

	workspace := normalizeWorkspaceHandle(inputs.Workspace, inputs.WorkspaceName, inputs.RepoPath, baseBranch)

	cfg.Identity = RepoIdentity{
		TowerName:  inputs.TowerName,
		TowerID:    inputs.TowerName,
		Prefix:     prefix,
		RepoURL:    repoURL,
		BaseBranch: baseBranch,
	}
	cfg.Workspace = workspace
	cfg.Run = RunContext{
		TowerName:   inputs.TowerName,
		Prefix:      prefix,
		BeadID:      cfg.BeadID,
		AttemptID:   cfg.AttemptID,
		RunID:       inputs.RunID,
		Role:        cfg.Role,
		FormulaStep: inputs.RunStep,
		Backend:     inputs.Backend,
		HandoffMode: inputs.HandoffMode,
	}
	if workspace != nil {
		cfg.Run.WorkspaceKind = workspace.Kind
		cfg.Run.WorkspaceName = workspace.Name
		cfg.Run.WorkspaceOrigin = workspace.Origin
	}

	if err := recordHandoffSelection(inputs.Log, inputs.HandoffMode, cfg.Run); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// ApprenticeDeliveryHandoff returns the HandoffMode the executor's
// review-fix and cleric-worker dispatches use for the given tower
// config. Exported for call sites outside pkg/executor (notably the
// wizard's review-fix re-engagement path) so they stay in lockstep
// with the executor's apprentice-transport selection.
func ApprenticeDeliveryHandoff(tower *TowerConfig) HandoffMode {
	return apprenticeDeliveryHandoff(tower)
}
