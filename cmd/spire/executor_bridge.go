// executor_bridge.go provides backward-compatible wrappers delegating to pkg/executor.
// cmd/spire callers continue to use unexported names; the real logic lives in
// the executor package.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/executor"
	formulaPkg "github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/metrics"
	"github.com/awell-health/spire/pkg/repoconfig"
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
	deps := buildExecutorDeps(spawner)
	return executor.NewGraph(beadID, agentName, graph, deps)
}

// computeWaves delegates to pkg/executor.ComputeWaves.
func computeWaves(epicID string) ([][]string, error) {
	deps := buildExecutorDeps(ResolveBackend(""))
	return executor.ComputeWaves(epicID, deps)
}

// --- Terminal step wrappers ---

func terminalMerge(beadID, branch, baseBranch, repoPath, buildCmd string, log func(string, ...interface{})) error {
	deps := buildExecutorDeps(ResolveBackend(""))
	return executor.TerminalMerge(beadID, branch, baseBranch, repoPath, buildCmd, deps, log)
}

func terminalSplit(beadID, reviewerName string, splitTasks []SplitTask, log func(string, ...interface{})) error {
	deps := buildExecutorDeps(ResolveBackend(""))
	return executor.TerminalSplit(beadID, reviewerName, splitTasks, deps, log)
}

func terminalDiscard(beadID string, log func(string, ...interface{})) error {
	deps := buildExecutorDeps(ResolveBackend(""))
	return executor.TerminalDiscard(beadID, deps, log)
}

// --- Escalation wrappers ---

func wizardMessageArchmage(from, beadID, message string) {
	deps := buildExecutorDeps(ResolveBackend(""))
	executor.MessageArchmage(from, beadID, message, deps)
}

func escalateHumanFailure(beadID, agentName, failureType, message string) {
	deps := buildExecutorDeps(ResolveBackend(""))
	executor.EscalateHumanFailure(beadID, agentName, failureType, message, deps)
}

// --- Helper: archmageIdentity ---

func archmageIdentity() (name, email string) {
	deps := buildExecutorDeps(ResolveBackend(""))
	return executor.ArchmageIdentity(deps)
}

// --- Deps wiring ---

func buildExecutorDeps(spawner AgentBackend) *executor.Deps {
	return &executor.Deps{
		// Graph state persistence — Dolt-backed in cluster, file-backed locally.
		GraphStateStore: executor.ResolveGraphStateStore(configDir),

		// Store operations
		GetBead:          storeGetBead,
		GetChildren:      storeGetChildren,
		GetComments:      storeGetComments,
		AddComment:       storeAddComment,
		CreateBead:       storeCreateBead,
		CloseBead:        storeCloseBead,
		UpdateBead:       storeUpdateBead,
		AddLabel:         storeAddLabel,
		RemoveLabel:      storeRemoveLabel,
		AddDep:           storeAddDep,
		AddDepTyped:      storeAddDepTyped,
		GetDepsWithMeta:       storeGetDepsWithMeta,
		GetDependentsWithMeta: storeGetDependentsWithMeta,
		GetBlockedIssues:      storeGetBlockedIssues,
		GetReviewBeads:   storeGetReviewBeads,

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
		CloseStepBead:    storeCloseStepBead,

		// Agent registry
		RegistryAdd:    func(entry agent.Entry) error { return wizardRegistryAdd(entry) },
		RegistryRemove: func(name string) error { return wizardRegistryRemove(name) },
		RegisterSelf: func(name, beadID, phase string, opts ...func(*agent.Entry)) func() {
			return agent.RegisterSelf(name, beadID, phase, opts...)
		},

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
			// Best-effort: load from the bead's repo. Returns nil if unavailable.
			cfg, err := repoconfig.Load(".")
			if err != nil {
				return nil
			}
			return cfg
		},

		// Spawner
		Spawner: spawner,

		// Agent run recording
		RecordAgentRun: metrics.Record,
		AgentResultDir: func(agentName string) string {
			return filepath.Join(doltGlobalDir(), "wizards", agentName)
		},

		// Claude runner
		ClaudeRunner: func(args []string, dir string) ([]byte, error) {
			cmd := exec.Command("claude", args...)
			cmd.Dir = dir
			cmd.Env = os.Environ()
			cmd.Stderr = os.Stderr
			return cmd.Output()
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

		// Label / type helpers
		HasLabel:       hasLabel,
		ContainsLabel:  containsLabel,
		ParseIssueType: parseIssueType,
	}
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

	// All formulas are v3 step-graph formulas.
	spawner := ResolveBackend("")

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
