// executor_bridge.go provides backward-compatible wrappers delegating to pkg/executor.
// cmd/spire callers continue to use unexported names; the real logic lives in
// the executor package.
package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/awell-health/spire/pkg/bundlestore"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/executor"
	formulaPkg "github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/metrics"
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/awell-health/spire/pkg/steward/intent"
	"github.com/awell-health/spire/pkg/store"
	"github.com/spf13/cobra"
)

var executeCmd = &cobra.Command{
	Use:    "execute <bead-id> [flags]",
	Short:  "Internal: run formula executor",
	Hidden: true,
	Args:   cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var fullArgs []string
		fullArgs = append(fullArgs, args...)
		if v, _ := cmd.Flags().GetString("name"); v != "" {
			fullArgs = append(fullArgs, "--name", v)
		}
		if v, _ := cmd.Flags().GetString("formula"); v != "" {
			fullArgs = append(fullArgs, "--formula", v)
		}
		return cmdExecute(fullArgs)
	},
}

func init() {
	executeCmd.Flags().String("name", "", "Agent name override")
	executeCmd.Flags().String("formula", "", "Formula name override")
}

// --- Type aliases so existing cmd/spire code compiles unchanged ---

type formulaExecutor = executor.Executor
type executorState = executor.State

// SplitTask is re-exported from pkg/executor.
type SplitTask = executor.SplitTask

// --- Function aliases ---

// newGraphExecutor creates a v3 graph executor for a bead, wiring up all dependencies.
func newGraphExecutor(beadID, agentName string, graph *formulaPkg.FormulaStepGraph, spawner AgentBackend) (*formulaExecutor, error) {
	deps, err := buildExecutorDepsForBead(beadID, spawner)
	if err != nil {
		return nil, err
	}
	return executor.NewGraph(beadID, agentName, graph, deps)
}

// computeWaves delegates to pkg/executor.ComputeWaves.
func computeWaves(epicID string) ([][]string, error) {
	deps, err := buildExecutorDepsForBead(epicID, resolveBackendForBead(epicID))
	if err != nil {
		return nil, err
	}
	return executor.ComputeWaves(epicID, deps)
}

// --- Terminal step wrappers ---

func terminalMerge(beadID, branch, baseBranch, repoPath, buildCmd string, logFn func(string, ...interface{})) error {
	deps, err := buildExecutorDepsForBead(beadID, resolveBackendForBead(beadID))
	if err != nil {
		return err
	}
	return executor.TerminalMerge(beadID, branch, baseBranch, repoPath, buildCmd, deps, logFn)
}

func terminalSplit(beadID, reviewerName string, splitTasks []SplitTask, logFn func(string, ...interface{})) error {
	deps, err := buildExecutorDepsForBead(beadID, resolveBackendForBead(beadID))
	if err != nil {
		return err
	}
	return executor.TerminalSplit(beadID, reviewerName, splitTasks, deps, logFn)
}

func terminalDiscard(beadID string, logFn func(string, ...interface{})) error {
	deps, err := buildExecutorDepsForBead(beadID, resolveBackendForBead(beadID))
	if err != nil {
		return err
	}
	return executor.TerminalDiscard(beadID, deps, logFn)
}

// --- Escalation wrappers ---

func wizardMessageArchmage(from, beadID, message string) {
	// Fire-and-forget escalation path. If the bead's prefix is unbound
	// the error is logged and the message is dropped — we refuse to
	// proceed with an unresolved repo. The upstream guards (summon
	// pre-flight, graph_interpreter) should prevent this from ever being
	// reached in practice; this is belt-and-suspenders.
	deps, err := buildExecutorDepsForBead(beadID, resolveBackendForBead(beadID))
	if err != nil {
		log.Printf("[ERROR] wizardMessageArchmage: dropping message for bead %s: %v", beadID, err)
		return
	}
	executor.MessageArchmage(from, beadID, message, deps)
}

func escalateHumanFailure(beadID, agentName, failureType, message string) {
	// Fire-and-forget escalation path. See wizardMessageArchmage.
	deps, err := buildExecutorDepsForBead(beadID, resolveBackendForBead(beadID))
	if err != nil {
		log.Printf("[ERROR] escalateHumanFailure: dropping escalation for bead %s: %v", beadID, err)
		return
	}
	executor.EscalateHumanFailure(beadID, agentName, failureType, message, deps)
}

// --- Helper: archmageIdentity ---

func archmageIdentity() (name, email string) {
	// No bead context — fall back to cwd-based resolution. archmageIdentity
	// only reads git config, so the spawner choice here does not affect
	// behavior; we pass a process backend to avoid the cwd assertion.
	// buildExecutorDepsForBead cannot error when beadID is empty.
	deps, _ := buildExecutorDepsForBead("", ResolveBackend(""))
	return executor.ArchmageIdentity(deps)
}

// --- Deps wiring ---

// buildExecutorDeps is the legacy, cwd-based wiring entry point. Prefer
// buildExecutorDepsForBead so config reads honor the bead's registered
// repo path rather than ambient CWD. See spi-vrzhf.
//
// Always returns a non-nil *executor.Deps with a nil error because the
// empty beadID bypasses the ResolveRepo check. Tests and legacy callers
// ignore the second return value.
func buildExecutorDeps(spawner AgentBackend) *executor.Deps {
	deps, _ := buildExecutorDepsForBead("", spawner)
	return deps
}

// buildExecutorDepsForBead resolves executor.Deps values that read
// spire.yaml from the bead's registered repo path. Pass beadID="" when
// no bead context is available (e.g. archmage-identity only reads git
// config and is insensitive to backend/repo config).
//
// When beadID's prefix is unbound, wizardResolveRepo returns an error.
// That error is logged loudly and propagated up via the returned error.
// Callers must not silently substitute a CWD fallback — the empty
// repoPath flows downstream only to make it observable to the executor
// guard in pkg/executor/graph_interpreter.go, which fails closed.
func buildExecutorDepsForBead(beadID string, spawner AgentBackend) (*executor.Deps, error) {
	repoPath := ""
	var resolveErr error
	if beadID != "" {
		rp, _, _, err := wizardResolveRepo(beadID)
		if err != nil {
			log.Printf("[ERROR] executor_bridge: ResolveRepo failed for bead %s: %v (run `spire repo list` to see prefix bindings; `spire repo bind <prefix> <path>` to register a local path)", beadID, err)
			resolveErr = fmt.Errorf("executor_bridge: resolve repo for bead %s: %w", beadID, err)
		} else {
			repoPath = rp
		}
	}
	// Graph state persistence — Dolt-backed in cluster, file-backed
	// locally. When we have a bead ID in scope, the prefix is derived
	// from it (via store.PrefixFromID) so a multi-prefix tower doesn't
	// silently fall back to local-only mode (which, on cluster mode,
	// loses writes — see spi-pwdhs5 Bug C). When beadID is empty
	// (legacy archmage-identity callers), the empty-prefix form is
	// used, which may fall back to local-only mode; those call sites
	// don't persist graph state anyway. Steward-style tower-global
	// sweeps must use resolveGlobalGraphStateStore instead.
	var gsStore executor.GraphStateStore
	if beadID != "" {
		gsStore = resolveGraphStateStoreForBeadOrLocal(beadID)
	} else {
		gsStore = resolveGraphStateStoreOrLocal("")
	}
	return &executor.Deps{
		GraphStateStore: gsStore,

		MaxApprentices: resolveMaxApprenticesForRepo(repoPath),

		// Store operations
		GetBead:               storeGetBead,
		GetChildren:           storeGetChildren,
		GetComments:           storeGetComments,
		AddComment:            storeAddComment,
		CreateBead:            storeCreateBead,
		CloseBead:             storeCloseBeadLifecycle,
		UpdateBead:            storeUpdateBead,
		AddLabel:              storeAddLabel,
		RemoveLabel:           storeRemoveLabel,
		AddDep:                storeAddDep,
		AddDepTyped:           storeAddDepTyped,
		GetDepsWithMeta:       storeGetDepsWithMeta,
		GetDependentsWithMeta: storeGetDependentsWithMeta,
		GetBlockedIssues:      storeGetBlockedIssues,
		GetReviewBeads:        storeGetReviewBeads,
		ListBeads:             storeListBeads,

		// Attempt operations
		CreateAttemptBead:      storeCreateAttemptBead,
		CloseAttemptBead:       storeCloseAttemptBead,
		GetActiveAttempt:       storeGetActiveAttempt,
		StampAttemptInstance:   store.StampAttemptInstance,
		IsOwnedByInstance:      store.IsOwnedByInstance,
		GetAttemptInstance:     store.GetAttemptInstance,
		UpdateAttemptHeartbeat: store.UpdateAttemptHeartbeat,

		// Step bead operations
		CreateStepBead:   storeCreateStepBead,
		ActivateStepBead: storeActivateStepBead,
		ReopenStepBead:   storeReopenStepBead,
		CloseStepBead:    storeCloseStepBead,
		HookStepBead:     storeHookStepBead,
		UnhookStepBead:   storeUnhookStepBead,

		// Agent registry. RegistryAdd is intentionally absent — backend.Spawn
		// is the sole creator (see pkg/agent/README.md "Registry lifecycle").
		RegistryRemove: func(name string) error { return wizardRegistryRemove(name) },

		// Resolution
		ResolveRepo:   wizardResolveRepo,
		ResolveBranch: executorResolveBranch,
		GetPhase:      getPhase,

		// Tower / identity
		ActiveTowerConfig: activeTowerConfig,
		ArchmageGitEnv:    archmageGitEnv,

		// Config
		ConfigDir: configDir,
		RepoConfig: func() *repoconfig.RepoConfig {
			// Best-effort: load from the bead's registered repo path.
			// Falls back to cwd when beadID is empty or the prefix is
			// not registered locally. Loading via repoPath — not "." —
			// is what keeps operator-managed wizards, whose cwd may be
			// above the clone, on the canonical spire.yaml.
			dir := repoPath
			if dir == "" {
				dir = "."
			}
			cfg, err := repoconfig.Load(dir)
			if err != nil {
				return nil
			}
			return cfg
		},

		// Spawner
		Spawner: spawner,

		// ClusterChildDispatcher is wired only when the active tower's
		// effective deployment mode is cluster-native. The dispatcher
		// adapts intent.NewDoltPublisher (the .1-introduced canonical
		// transport) into the executor's ClusterChildDispatcher seam,
		// adding a fresh DispatchSeq via store.NextDispatchSeq and an
		// intent.Validate gate before publish. Local-native deployments
		// see nil here and the executor's useClusterChildDispatch()
		// helper falls through to Spawner.Spawn unchanged. The mode
		// decision is made once here at construction so action handlers
		// never re-inspect tower config themselves.
		ClusterChildDispatcher: buildExecutorClusterChildDispatcher(),

		// BundleStore — same resolver used by `spire apprentice submit` so
		// producer and consumer agree on the backend. Nil when construction
		// fails; dispatch sites fall back to legacy merge behavior.
		BundleStore: buildBundleStore(),

		// Agent run recording
		RecordAgentRun: metrics.Record,
		AgentResultDir: func(agentName string) string {
			return filepath.Join(doltGlobalDir(), "wizards", agentName)
		},

		// Claude runner
		ClaudeRunner: func(args []string, dir string, logOut io.Writer) ([]byte, error) {
			var stdout bytes.Buffer
			cmd := exec.Command("claude", args...)
			cmd.Dir = dir
			cmd.Env = os.Environ()
			cmd.Stdout = io.MultiWriter(&stdout, logOut)
			cmd.Stderr = io.MultiWriter(os.Stderr, logOut)
			err := cmd.Run()
			return stdout.Bytes(), err
		},

		// Focus context
		CaptureFocus: wizardCaptureFocus,

		// Review DAG callbacks
		ReviewHandleApproval:    reviewHandleApproval,
		ReviewEscalateToArbiter: bridgeReviewEscalateToArbiter,
		ReviewBeadVerdict:       reviewBeadVerdict,

		// Bead predicates
		IsAttemptBead:     isAttemptBead,
		IsStepBead:        isStepBead,
		IsReviewRoundBead: isReviewRoundBead,

		// Hard reset (destructive: kills wizard, deletes worktree/branches/state/beads)
		HardResetBead: hardResetBeadCore,

		// Metadata
		SetBeadMetadata: store.SetBeadMetadataMap,

		// Recovery outcome writer — pkg/recovery.WriteOutcome is the sole
		// writer of the RecoveryOutcome shape (bead metadata + recovery_learnings).
		// Exposed as a dep so tests can inject a failing writer to exercise
		// the cleric's fail-closed path on outcome persistence.
		WriteRecoveryOutcome: recovery.WriteOutcome,

		// Label / type helpers
		HasLabel:       hasLabel,
		ContainsLabel:  containsLabel,
		ParseIssueType: parseIssueType,
	}, resolveErr
}

// bridgeReviewEscalateToArbiter adapts the cmd/spire reviewEscalateToArbiter
// (which uses the cmd/spire Review type) to the executor.Review type.
func bridgeReviewEscalateToArbiter(beadID, reviewerName string, lastReview *executor.Review, policy RevisionPolicy, log func(string, ...interface{})) error {
	// Convert executor.Review to cmd/spire Review
	r := &Review{
		Verdict: lastReview.Verdict,
		Summary: lastReview.Summary,
	}
	for _, issue := range lastReview.Issues {
		r.Issues = append(r.Issues, ReviewIssue{
			File:     issue.File,
			Line:     issue.Line,
			Severity: issue.Severity,
			Message:  issue.Message,
		})
	}
	return reviewEscalateToArbiter(beadID, reviewerName, r, policy, log)
}

// --- Type compatibility: Review lives in pkg/wizard and pkg/executor as separate types ---
// cmd/spire aliases wizard.Review via wizard_bridge.go; executor.Review is separate.
// The bridge above handles conversion. pkg/executor callers use executor.Review.

// resolveMaxApprentices returns the cap on concurrent apprentice
// subprocesses using the process's current working directory for
// spire.yaml lookup. Prefer resolveMaxApprenticesForRepo so config reads
// honor the bead's registered repo path rather than ambient CWD — see
// spi-vrzhf. Kept for callers that have no bead in scope.
func resolveMaxApprentices() int {
	return resolveMaxApprenticesForRepo("")
}

// resolveMaxApprenticesForRepo returns the cap on concurrent apprentice
// subprocesses for this wizard, loading spire.yaml from repoDir.
//
// Precedence: SPIRE_MAX_APPRENTICES env > spire.yaml agent.max-apprentices
// > 0 (executor falls back to DefaultMaxApprentices).
//
// The operator sets SPIRE_MAX_APPRENTICES on the wizard pod when
// WizardGuild.spec.maxApprentices is set; locally the env is unset and the
// spire.yaml value wins. Per-step formula overrides are applied later in
// dispatchWaveCore via step.With["max-apprentices"].
//
// When repoDir is empty we fall back to cwd. If SPIRE_REPO_PREFIX is set
// but no spire.yaml is reachable from cwd, a warning fires via the same
// assertion used by ResolveBackend so the silent-operator-fallback
// regression from spi-vrzhf stays detectable.
func resolveMaxApprenticesForRepo(repoDir string) int {
	if raw := os.Getenv("SPIRE_MAX_APPRENTICES"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	dir := repoDir
	explicit := dir != ""
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return 0
		}
	}
	cfg, err := repoconfig.Load(dir)
	if err != nil || cfg == nil {
		if !explicit {
			assertRepoConfigReachable(dir, "resolveMaxApprentices")
		}
		return 0
	}
	if !explicit && cfg.Agent.MaxApprentices == 0 && cfg.Agent.Backend == "" {
		// cwd-based lookup returned an empty config; warn if we appear
		// to be inside an operator-managed pod that landed above the
		// clone.
		assertRepoConfigReachable(dir, "resolveMaxApprentices")
	}
	return cfg.Agent.MaxApprentices
}

// assertRepoConfigReachable is a thin cmd-side mirror of the pkg/agent
// runtime assertion. When SPIRE_REPO_PREFIX is set and spire.yaml is not
// reachable from cwd, but the canonical clone path /workspace/<prefix>
// has one, we log a warning naming the caller. See spi-vrzhf.
func assertRepoConfigReachable(cwd, caller string) {
	prefix := os.Getenv("SPIRE_REPO_PREFIX")
	if prefix == "" {
		return
	}
	expected := filepath.Join("/workspace", prefix)
	if _, err := os.Stat(filepath.Join(cwd, "spire.yaml")); err == nil {
		return
	}
	if _, err := os.Stat(filepath.Join(expected, "spire.yaml")); err == nil {
		fmt.Fprintf(os.Stderr,
			"[spire] WARN %s: cwd=%q but SPIRE_REPO_PREFIX=%q resolves to %q; spire.yaml not reachable from cwd. Pass an explicit repo path or fix the container WorkingDir.\n",
			caller, cwd, prefix, expected,
		)
	}
}

// buildBundleStore resolves the tower-configured bundle store used by the
// wizard to consume apprentice bundles. Returns nil on any construction
// error — dispatch sites treat nil as "legacy path, no bundle fetch".
// Mirrors defaultNewBundleStore from apprentice.go so producer and consumer
// resolve to the same backend/root.
func buildBundleStore() bundlestore.BundleStore {
	bs, err := defaultNewBundleStore()
	if err != nil {
		return nil
	}
	return bs
}

// buildExecutorClusterChildDispatcher wires the executor's
// cluster-native child-dispatch seam. Returns nil for local-native
// towers, a missing ActiveDB (test/mock paths), or unresolvable tower
// config — the executor's useClusterChildDispatch() helper treats nil
// as "fall through to Spawner.Spawn", so a nil here is safe in
// local-native mode. In cluster-native mode a nil here makes the
// dispatch site fail closed (no Spawner.Spawn fallback) which keeps
// the cluster-native invariant explicit.
//
// The dispatcher itself is the doltPublisherChildDispatcher adapter
// declared below — it adds a fresh DispatchSeq via store.NextDispatchSeq
// and an intent.Validate gate before delegating to the .1-introduced
// intent.IntentPublisher.
func buildExecutorClusterChildDispatcher() executor.ClusterChildDispatcher {
	tower, err := activeTowerConfig()
	if err != nil || tower == nil {
		return nil
	}
	if tower.EffectiveDeploymentMode() != config.DeploymentModeClusterNative {
		return nil
	}
	db, ok := store.ActiveDB()
	if !ok || db == nil {
		log.Printf("[executor] cluster-native dispatcher: ActiveDB unavailable; cluster child dispatch disabled")
		return nil
	}
	if ensureErr := intent.EnsureWorkloadIntentsTable(db); ensureErr != nil {
		log.Printf("[executor] cluster-native dispatcher: ensure workload_intents table: %s", ensureErr)
	}
	return &doltPublisherChildDispatcher{
		publisher: intent.NewDoltPublisher(db),
		nextSeq:   store.NextDispatchSeq,
	}
}

// doltPublisherChildDispatcher adapts intent.IntentPublisher into
// executor.ClusterChildDispatcher. It assigns a fresh DispatchSeq when
// the caller leaves it zero (the executor side never tracks dispatch
// seq itself — that's the dolt outbox's monotonic counter) and runs
// intent.Validate before publish so a malformed (Role, Phase, Runtime)
// triple fails closed at the dispatch site rather than reaching the
// operator's reconcile path.
type doltPublisherChildDispatcher struct {
	publisher intent.IntentPublisher
	nextSeq   func(taskID string) (int, error)
}

// Dispatch implements executor.ClusterChildDispatcher.
func (d *doltPublisherChildDispatcher) Dispatch(ctx context.Context, wi intent.WorkloadIntent) error {
	if d == nil || d.publisher == nil {
		return fmt.Errorf("cluster child dispatcher: nil publisher")
	}
	if wi.TaskID == "" {
		return fmt.Errorf("cluster child dispatcher: empty TaskID")
	}
	if wi.DispatchSeq < 1 && d.nextSeq != nil {
		seq, err := d.nextSeq(wi.TaskID)
		if err != nil {
			return fmt.Errorf("cluster child dispatcher: next seq for %s: %w", wi.TaskID, err)
		}
		wi.DispatchSeq = seq
	}
	if err := intent.Validate(wi); err != nil {
		return fmt.Errorf("cluster child dispatcher: validate intent for %s: %w", wi.TaskID, err)
	}
	return d.publisher.Publish(ctx, wi)
}

// executorResolveBranch loads spire.yaml from the bead's repo and resolves
// the branch name. Used by the executor's Deps.ResolveBranch.
func executorResolveBranch(beadID string) string {
	repoPath, _, _, err := wizardResolveRepo(beadID)
	if err != nil {
		return "feat/" + beadID
	}
	return resolveBranchForBead(beadID, repoPath)
}

// --- Command entry point ---

// claimBeadIfNeeded claims the bead if it's not already in progress and
// no existing graph state exists. Extracted from cmdExecute to avoid
// duplicating the claim logic.
func claimBeadIfNeeded(beadID, agentName string) {
	// Check for existing v3 graph state.
	existingGraphState, _ := executor.LoadGraphState(agentName, configDir)
	if existingGraphState != nil {
		return // resuming — don't re-claim
	}
	// Fresh start: claim bead if not already in progress.
	bead, _ := storeGetBead(beadID)
	if bead.Status != "in_progress" {
		os.Setenv("SPIRE_IDENTITY", agentName)
		cmdClaim([]string{beadID}) // best-effort
	}
}

// cmdExecute is the internal entry point for the formula executor.
// Usage: spire execute <bead-id> [--name <name>] [--formula <name>]
func cmdExecute(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: spire execute <bead-id> [--name <name>] [--formula <name>]")
	}

	beadID := args[0]
	agentName := "wizard-" + beadID
	formulaName := ""

	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--name":
			if i+1 < len(args) {
				i++
				agentName = args[i]
			}
		case "--formula":
			if i+1 < len(args) {
				i++
				formulaName = args[i]
			}
		}
	}

	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	// All formulas are v3 step-graph formulas. Resolve the backend from
	// the bead's registered-repo spire.yaml rather than cwd so
	// operator-managed wizard pods (whose cwd is /workspace, above the
	// clone at /workspace/<prefix>) still pick up agent.backend. See
	// spi-vrzhf.
	spawner := resolveBackendForBead(beadID)

	if formulaName != "" {
		graph, gErr := formulaPkg.LoadStepGraphByName(formulaName)
		if gErr != nil {
			return fmt.Errorf("load formula %s: %w", formulaName, gErr)
		}
		claimBeadIfNeeded(beadID, agentName)
		ex, execErr := newGraphExecutor(beadID, agentName, graph, spawner)
		if execErr != nil {
			return execErr
		}
		return ex.Run()
	}

	// No explicit formula: resolve v3 from bead type/labels.
	bead, berr := storeGetBead(beadID)
	if berr != nil {
		return fmt.Errorf("get bead: %w", berr)
	}

	bi := beadToInfo(bead)
	graph, err := formulaPkg.ResolveV3(bi)
	if err != nil {
		return fmt.Errorf("resolve formula: %w", err)
	}

	claimBeadIfNeeded(beadID, agentName)
	ex, execErr := newGraphExecutor(beadID, agentName, graph, spawner)
	if execErr != nil {
		return execErr
	}
	return ex.Run()
}
