// executor_bridge.go provides backward-compatible wrappers delegating to pkg/executor.
// cmd/spire callers continue to use unexported names; the real logic lives in
// the executor package.
package main

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/executor"
)

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
	return executor.ResolveBeadBuildCmd(bead, ResolveFormula)
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
		GetDepsWithMeta:  storeGetDepsWithMeta,
		GetBlockedIssues: storeGetBlockedIssues,
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
		ResolveRepo: wizardResolveRepo,
		GetPhase:    getPhase,

		// Tower / identity
		ActiveTowerConfig: activeTowerConfig,
		ArchmageGitEnv:    archmageGitEnv,

		// Config
		ConfigDir:      configDir,
		ResolveFormula: ResolveFormula,

		// Spawner
		Spawner: spawner,

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

// --- Type compatibility: Review is now in both executor and wizard_review.go ---
// The cmd/spire Review type stays in wizard_review.go; executor.Review is separate.
// The bridge above handles conversion. pkg/executor callers use executor.Review.

// --- Command entry point ---

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

	// Resolve formula
	var formula *FormulaV2
	var err error
	if formulaName != "" {
		formula, err = LoadFormulaByName(formulaName)
		if err != nil {
			return fmt.Errorf("load formula %s: %w", formulaName, err)
		}
	} else {
		bead, berr := storeGetBead(beadID)
		if berr != nil {
			return fmt.Errorf("get bead: %w", berr)
		}
		formula, err = ResolveFormula(bead)
	}
	if err != nil {
		return fmt.Errorf("load formula: %w", err)
	}

	// Skip claim when resuming an existing executor session.
	existingState, stateErr := loadExecutorState(agentName)
	if stateErr != nil {
		return fmt.Errorf("load state: %w", stateErr)
	}
	if existingState == nil {
		// Fresh start: claim bead if not already in progress.
		bead, _ := storeGetBead(beadID)
		if bead.Status != "in_progress" {
			os.Setenv("SPIRE_IDENTITY", agentName)
			if cerr := cmdClaim([]string{beadID}); cerr != nil {
				return fmt.Errorf("claim: %w", cerr)
			}
		}
	}

	spawner := ResolveBackend("")

	ex, execErr := newExecutor(beadID, agentName, formula, spawner)
	if execErr != nil {
		return execErr
	}

	return ex.Run()
}
