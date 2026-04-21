// wizard_bridge.go wires pkg/wizard callbacks and provides thin CLI adapters
// for the apprentice/sage spawn entry points. The public-facing role verbs
// (apprentice submit, sage accept/reject, wizard claim/seal) live in their
// respective role files; this bridge only forwards the internal subprocess
// invocations to pkg/wizard.
package main

import (
	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/bundlestore"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/wizard"
)

// --- Type aliases for backward compatibility ---
// These let existing cmd/spire code (executor_bridge, steward, etc.) continue
// to use the unexported names.

type Review = wizard.Review
type ReviewIssue = wizard.ReviewIssue
// --- Function aliases for backward compatibility ---
// Other cmd/spire files (executor_bridge.go, formula_bridge.go) call these.

func wizardResolveRepo(beadID string) (repoPath, repoURL, baseBranch string, err error) {
	return wizard.ResolveRepo(beadID, buildWizardDeps())
}

func resolveBranchForBead(beadID, repoPath string) string {
	return wizard.ResolveBranchForBead(beadID, repoPath)
}

func wizardCaptureFocus(beadID string) (string, error) {
	return wizard.CaptureWizardFocus(beadID)
}


func wizardCleanup(worktreeDir, repoPath string) {
	wizard.WizardCleanup(worktreeDir, repoPath)
}

func wizardCollectReviewHistory(beadID, wizardName string) string {
	return wizard.WizardCollectReviewHistory(beadID, wizardName, buildWizardDeps())
}

func wizardCollectFeedback(beadID, wizardName string) string {
	return wizard.WizardCollectFeedback(beadID, wizardName, buildWizardDeps())
}

// Review functions used by executor_bridge.go
func reviewHandleApproval(beadID, reviewerName, branch, baseBranch, repoPath string, log func(string, ...interface{})) error {
	return wizard.ReviewHandleApproval(beadID, reviewerName, branch, baseBranch, repoPath, buildWizardDeps(), log)
}

func reviewEscalateToArbiter(beadID, reviewerName string, lastReview *Review, policy RevisionPolicy, log func(string, ...interface{})) error {
	return wizard.ReviewEscalateToArbiter(beadID, reviewerName, lastReview, policy, buildWizardDeps(), log)
}

func reviewMerge(beadID, beadTitle, branch, baseBranch, repoPath string, log func(string, ...interface{})) error {
	return wizard.ReviewMerge(beadID, beadTitle, branch, baseBranch, repoPath, buildWizardDeps(), log)
}

// --- CLI command entry points ---

func cmdWizardRun(args []string) error {
	return wizard.CmdWizardRun(args, buildWizardDeps())
}

func cmdWizardReview(args []string) error {
	return wizard.CmdWizardReview(args, buildWizardDeps())
}

// --- Deps wiring ---

func buildWizardDeps() *wizard.Deps {
	return &wizard.Deps{
		// Store operations
		GetBead:     storeGetBead,
		ListBeads:   storeListBeads,
		GetChildren: storeGetChildren,
		GetComments: storeGetComments,
		AddComment:  storeAddComment,
		CreateBead:  storeCreateBead,
		CloseBead:   storeCloseBead,
		UpdateBead:  storeUpdateBead,
		AddLabel:    storeAddLabel,
		RemoveLabel: storeRemoveLabel,

		// Review bead operations
		GetReviewBeads:   storeGetReviewBeads,
		CreateReviewBead: storeCreateReviewBead,
		CloseReviewBead:  storeCloseReviewBead,

		// Label / type helpers
		HasLabel:          hasLabel,
		ContainsLabel:     containsLabel,
		ReviewRoundNumber: reviewRoundNumber,
		ReviewBeadVerdict: reviewBeadVerdict,

		// Phase helpers
		GetPhase: getPhase,

		// Agent registry
		RegistryAdd:    func(entry agent.Entry) error { return wizardRegistryAdd(entry) },
		RegistryRemove: func(name string) error { return wizardRegistryRemove(name) },
		RegistryUpdate: func(name string, f func(*agent.Entry)) error { return wizardRegistryUpdate(name, f) },
		RegisterSelf: func(name, beadID, phase string, opts ...func(*agent.Entry)) func() {
			return agent.RegisterSelf(name, beadID, phase, opts...)
		},

		// Agent spawner
		ResolveBackend: func(name string) wizard.Backend { return ResolveBackend(name) },

		// Resolution — the bridge functions wizardResolveRepo and resolveBranchForBead
		// are the canonical call sites; they construct deps internally.
		ResolveRepo: func(beadID string) (string, string, string, error) {
			return wizardResolveRepo(beadID)
		},
		ResolveBranch: func(beadID string, repoPath string) string {
			return resolveBranchForBead(beadID, repoPath)
		},

		// Config
		ConfigDir:         configDir,
		ActiveTowerConfig: activeTowerConfig,
		DoltGlobalDir:     doltGlobalDir,
		RequireDolt:       requireDolt,
		ResolveBeadsDir:   resolveBeadsDir,
		LoadConfig: func() (*config.SpireConfig, error) {
			return loadConfig()
		},

		// Dolt queries (for ResolveRepo)
		RawDoltQuery:    rawDoltQuery,
		ParseDoltRows:   parseDoltRows,
		SQLEscape:       sqlEscape,
		ResolveDatabase: func(cfg *config.SpireConfig) (string, bool) {
			return resolveDatabase(cfg)
		},

		// Executor terminal steps
		TerminalMerge: terminalMerge,
		TerminalSplit: func(beadID, reviewerName string, splitTasks []wizard.SplitTask, log func(string, ...interface{})) error {
			// Convert wizard.SplitTask -> executor.SplitTask
			execTasks := make([]SplitTask, len(splitTasks))
			for i, t := range splitTasks {
				execTasks[i] = SplitTask{Title: t.Title, Description: t.Description}
			}
			return terminalSplit(beadID, reviewerName, execTasks, log)
		},
		TerminalDiscard:      terminalDiscard,
		EscalateHumanFailure: escalateHumanFailure,
		ComputeWaves:         computeWaves,

		// Focus / bead JSON
		CaptureFocus: wizard.CaptureWizardFocus,
		GetBeadJSON: func(beadID string) (string, error) {
			return bd("show", beadID, "--json")
		},

		// Inbox
		ReadInboxFile: readInboxFile,

		// CLI commands
		CmdClaim: cmdClaim,
		CmdSend: func(args []string) error {
			return cmdSend(args)
		},

		// Bundle delivery
		NewBundleStore: func() (bundlestore.BundleStore, error) {
			return defaultNewBundleStore()
		},
	}
}

