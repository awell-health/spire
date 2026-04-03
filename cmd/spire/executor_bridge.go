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
type subtaskState = executor.SubtaskState

// SplitTask is re-exported from pkg/executor.
type SplitTask = executor.SplitTask

// --- Function aliases ---

// newExecutor creates a formula executor for a bead, wiring up all dependencies.
func newExecutor(beadID, agentName string, formula *FormulaV2, spawner AgentBackend) (*formulaExecutor, error) {
	deps := buildExecutorDeps(spawner)
	return executor.New(beadID, agentName, formula, deps)
}

// newGraphExecutor creates a v3 graph executor for a bead, wiring up all dependencies.
func newGraphExecutor(beadID, agentName string, graph *formulaPkg.FormulaStepGraph, spawner AgentBackend) (*formulaExecutor, error) {
	deps := buildExecutorDeps(spawner)
	return executor.NewGraph(beadID, agentName, graph, deps)
}

// loadExecutorState loads executor state from disk.
func loadExecutorState(agentName string) (*executorState, error) {
	return executor.LoadState(agentName, configDir)
}

// executorStatePath returns the state file path for an agent.
func executorStatePath(agentName string) string {
	return executor.StatePath(agentName, configDir)
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

// --- Helper: resolveBeadBuildCmd ---

func resolveBeadBuildCmd(bead Bead) string {
	// V2 formula resolution removed; ResolveBeadBuildCmd returns "" for
	// beads without a v2 formula.  Build commands in v3 are declared via
	// step-graph vars / repo config, not formula phases.
	return executor.ResolveBeadBuildCmd(bead, func(_ Bead) (*FormulaV2, error) {
		return nil, fmt.Errorf("v2 formula resolution removed")
	})
}

// --- Helper: archmageIdentity ---

func archmageIdentity() (name, email string) {
	deps := buildExecutorDeps(ResolveBackend(""))
	return executor.ArchmageIdentity(deps)
}

// --- Deps wiring ---

func buildExecutorDeps(spawner AgentBackend) *executor.Deps {
	return &executor.Deps{
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
		CreateAttemptBead: storeCreateAttemptBead,
		CloseAttemptBead:  storeCloseAttemptBead,
		GetActiveAttempt:  storeGetActiveAttempt,

		// Step bead operations
		CreateStepBead:   storeCreateStepBead,
		ActivateStepBead: storeActivateStepBead,
		CloseStepBead:    storeCloseStepBead,

		// Agent registry
		RegistryAdd:    func(entry agent.Entry) error { return wizardRegistryAdd(entry) },
		RegistryRemove: func(name string) error { return wizardRegistryRemove(name) },

		// Resolution
		ResolveRepo:   wizardResolveRepo,
		ResolveBranch: executorResolveBranch,
		GetPhase:      getPhase,

		// Tower / identity
		ActiveTowerConfig: activeTowerConfig,
		ArchmageGitEnv:    archmageGitEnv,

		// Config
		ConfigDir:      configDir,
		ResolveFormula: func(_ Bead) (*FormulaV2, error) {
			return nil, fmt.Errorf("v2 formula resolution removed")
		},
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

		// Molecule steps
		CloseMoleculeStep: wizardCloseMoleculeStep,

		// Bead predicates
		IsAttemptBead:     isAttemptBead,
		IsStepBead:        isStepBead,
		IsReviewRoundBead: isReviewRoundBead,

		// Label / type helpers
		HasLabel:            hasLabel,
		ContainsLabel:       containsLabel,
		ParseIssueType:      parseIssueType,
		ResolveBeadBuildCmd: resolveBeadBuildCmd,
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
// no existing executor state exists. Extracted from cmdExecute to avoid
// duplicating the claim logic across v2/v3 paths.
func claimBeadIfNeeded(beadID, agentName string) {
	// Check for existing v2 state.
	existingState, _ := loadExecutorState(agentName)
	if existingState != nil {
		return // resuming — don't re-claim
	}
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
	anyFormula, _, err := formulaPkg.ResolveAny(bi)
	if err != nil {
		return fmt.Errorf("resolve formula: %w", err)
	}

	graph := anyFormula.(*formulaPkg.FormulaStepGraph)
	claimBeadIfNeeded(beadID, agentName)
	ex, execErr := newGraphExecutor(beadID, agentName, graph, spawner)
	if execErr != nil {
		return execErr
	}
	return ex.Run()
}
