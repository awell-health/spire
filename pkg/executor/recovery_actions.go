package executor

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	spgit "github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/store"
)

// RecoveryAction describes a git-aware recovery operation that can be executed
// against a bead. Actions may or may not require a worktree — git-aware actions
// operate inside a worktree while coordination actions (escalate, resummon) only
// need the database.
//
// This is distinct from the legacy recovery action vocabulary in
// pkg/recovery (which uses RecoveryActionKind). This registry supports
// worktree-capable, attempt-tracked recovery operations.
type RecoveryAction struct {
	Name             string
	Description      string
	RequiresWorktree bool
	MaxRetries       int
	Fn               func(ctx *RecoveryActionCtx) error
}

// RecoveryActionCtx provides the execution context for a recovery action.
// Worktree is non-nil only when RequiresWorktree is true.
type RecoveryActionCtx struct {
	DB             *sql.DB
	RepoPath       string
	Worktree       *spgit.WorktreeContext // nil if !RequiresWorktree
	RecoveryBeadID string
	TargetBeadID   string
	Params         map[string]string
	Log            func(string)
}

var (
	recoveryActionRegistryMu sync.RWMutex
	recoveryActionRegistry   = map[string]RecoveryAction{}
)

func init() {
	// Register the built-in git-aware recovery actions.
	for _, a := range []RecoveryAction{
		actionRebaseOntoMain(),
		actionCherryPick(),
		actionResolveConflicts(),
		actionTargetedFix(),
		actionRebuild(),
		actionResummon(),
		actionResetToStep(),
		actionEscalate(),
	} {
		recoveryActionRegistry[a.Name] = a
	}
}

// RegisterAction adds a recovery action to the registry. If an action with the
// same name already exists, it is replaced.
func RegisterAction(action RecoveryAction) {
	recoveryActionRegistryMu.Lock()
	defer recoveryActionRegistryMu.Unlock()
	recoveryActionRegistry[action.Name] = action
}

// GetAction looks up a recovery action by name.
func GetAction(name string) (*RecoveryAction, bool) {
	recoveryActionRegistryMu.RLock()
	defer recoveryActionRegistryMu.RUnlock()
	a, ok := recoveryActionRegistry[name]
	if !ok {
		return nil, false
	}
	return &a, true
}

// ListActions returns all registered recovery actions.
func ListActions() []RecoveryAction {
	recoveryActionRegistryMu.RLock()
	defer recoveryActionRegistryMu.RUnlock()
	out := make([]RecoveryAction, 0, len(recoveryActionRegistry))
	for _, a := range recoveryActionRegistry {
		out = append(out, a)
	}
	return out
}

// RunRecoveryAction looks up an action by name, provisions a worktree if
// required, executes the action, and records the attempt outcome via
// store.RecordRecoveryAttempt / store.UpdateAttemptOutcome.
//
// This is the git-aware counterpart to ExecuteRecoveryAction (recovery_phase.go)
// which dispatches legacy recovery action kinds. This function supports
// worktree provisioning and per-attempt tracking.
func RunRecoveryAction(ctx *RecoveryActionCtx, actionName string) error {
	action, ok := GetAction(actionName)
	if !ok {
		return fmt.Errorf("unknown recovery action: %s", actionName)
	}

	// Serialize params for attempt recording.
	paramsJSON, _ := json.Marshal(ctx.Params)

	// Count existing attempts for this action to derive attempt number.
	attemptNum := 1
	if ctx.DB != nil {
		count, err := store.CountAttemptsByAction(ctx.DB, ctx.RecoveryBeadID, actionName)
		if err == nil {
			attemptNum = count + 1
		}
	}

	// Check max retries.
	if action.MaxRetries > 0 && attemptNum > action.MaxRetries {
		return fmt.Errorf("action %s exceeded max retries (%d)", actionName, action.MaxRetries)
	}

	// Pre-generate attempt ID so UpdateAttemptOutcome can reference it later.
	// RecordRecoveryAttempt takes by value, so an internally generated ID
	// would be lost to the caller.
	attemptID := generateRecoveryAttemptID()
	attempt := store.RecoveryAttempt{
		ID:             attemptID,
		RecoveryBeadID: ctx.RecoveryBeadID,
		TargetBeadID:   ctx.TargetBeadID,
		Action:         actionName,
		Params:         string(paramsJSON),
		Outcome:        "in_progress",
		AttemptNumber:  attemptNum,
	}
	if ctx.DB != nil {
		if err := store.RecordRecoveryAttempt(ctx.DB, attempt); err != nil {
			ctx.Log(fmt.Sprintf("warning: failed to record attempt: %v", err))
		}
	}

	// Provision worktree if the action requires one and none was provided.
	var cleanupFn func()
	if action.RequiresWorktree && ctx.Worktree == nil {
		wc, cleanup, err := ProvisionRecoveryWorktree(ctx.RepoPath, ctx.TargetBeadID)
		if err != nil {
			if ctx.DB != nil {
				_ = store.UpdateAttemptOutcome(ctx.DB, attempt.ID, "failure", fmt.Sprintf("provision worktree: %v", err))
			}
			return fmt.Errorf("provision recovery worktree: %w", err)
		}
		ctx.Worktree = wc
		cleanupFn = cleanup
	}
	if cleanupFn != nil {
		defer cleanupFn()
	}

	// Execute the action.
	ctx.Log(fmt.Sprintf("executing recovery action: %s (attempt %d)", actionName, attemptNum))
	err := action.Fn(ctx)

	// Record outcome.
	if ctx.DB != nil {
		outcome := "success"
		errText := ""
		if err != nil {
			outcome = "failure"
			errText = err.Error()
		}
		_ = store.UpdateAttemptOutcome(ctx.DB, attempt.ID, outcome, errText)
	}

	return err
}

// ProvisionRecoveryWorktree creates a worktree for recovery operations using
// pkg/git APIs. The worktree is placed at <repoPath>/.worktrees/<beadID>-recovery
// on a branch named recovery/<beadID>, based on the target bead's feature branch
// (not main). This ensures recovery actions operate on the bead's actual work.
// Returns a cleanup function that removes the worktree when called.
func ProvisionRecoveryWorktree(repoPath string, beadID string) (*spgit.WorktreeContext, func(), error) {
	dir := filepath.Join(repoPath, ".worktrees", beadID+"-recovery")
	branch := "recovery/" + beadID

	// Resolve the target bead's feature branch so the recovery worktree
	// contains the bead's work. Fall back to feat/<beadID> if no label.
	startPoint := "feat/" + beadID
	if b, err := store.GetBead(beadID); err == nil {
		if fb := store.HasLabel(b, "feat-branch:"); fb != "" {
			startPoint = fb
		}
	}

	rc := &spgit.RepoContext{Dir: repoPath, BaseBranch: "main"}
	wc, err := rc.CreateWorktreeNewBranch(dir, branch, startPoint)
	if err != nil {
		return nil, nil, fmt.Errorf("create recovery worktree at %s from %s: %w", dir, startPoint, err)
	}

	cleanup := func() {
		wc.Cleanup()
	}
	return wc, cleanup, nil
}

// ---------------------------------------------------------------------------
// Built-in recovery actions
// ---------------------------------------------------------------------------

// actionRebaseOntoMain fetches origin/main and rebases the worktree branch
// onto it. Aborts and returns an error with conflicted file list on conflict.
func actionRebaseOntoMain() RecoveryAction {
	return RecoveryAction{
		Name:             "rebase-onto-main",
		Description:      "Fetch origin/main and rebase the worktree branch onto it",
		RequiresWorktree: true,
		MaxRetries:       3,
		Fn: func(ctx *RecoveryActionCtx) error {
			wc := ctx.Worktree

			// Fetch origin main into the worktree's shared refs.
			wc.EnsureRemoteRef("origin", "main")

			// Attempt rebase.
			if err := wc.RunCommand("git rebase origin/main"); err != nil {
				// Collect conflicted files before aborting.
				files, _ := wc.ConflictedFiles()
				_ = wc.RunCommand("git rebase --abort")
				if len(files) > 0 {
					return fmt.Errorf("rebase conflict in files: %s", strings.Join(files, ", "))
				}
				return fmt.Errorf("rebase onto origin/main failed: %w", err)
			}
			ctx.Log("rebase onto origin/main succeeded")
			return nil
		},
	}
}

// actionCherryPick cherry-picks the commit specified in Params["commit"].
// Aborts on conflict.
func actionCherryPick() RecoveryAction {
	return RecoveryAction{
		Name:             "cherry-pick",
		Description:      "Cherry-pick a specific commit into the worktree",
		RequiresWorktree: true,
		MaxRetries:       3,
		Fn: func(ctx *RecoveryActionCtx) error {
			commit := ctx.Params["commit"]
			if commit == "" {
				return fmt.Errorf("cherry-pick: missing 'commit' parameter")
			}
			if !validCommitSHA.MatchString(commit) {
				return fmt.Errorf("cherry-pick: invalid commit hash %q (must be 7-40 hex characters)", commit)
			}

			wc := ctx.Worktree
			if err := wc.RunCommand(fmt.Sprintf("git cherry-pick %s", commit)); err != nil {
				files, _ := wc.ConflictedFiles()
				_ = wc.RunCommand("git cherry-pick --abort")
				if len(files) > 0 {
					return fmt.Errorf("cherry-pick conflict in files: %s", strings.Join(files, ", "))
				}
				return fmt.Errorf("cherry-pick %s failed: %w", commit, err)
			}
			ctx.Log(fmt.Sprintf("cherry-pick %s succeeded", commit))
			return nil
		},
	}
}

// actionResolveConflicts attempts conflict resolution for each conflicted file.
// Defaults to --theirs; uses --ours if Params["strategy"] is "ours". Commits
// the result.
func actionResolveConflicts() RecoveryAction {
	return RecoveryAction{
		Name:             "resolve-conflicts",
		Description:      "Resolve merge conflicts using --theirs (or --ours) and commit",
		RequiresWorktree: true,
		MaxRetries:       2,
		Fn: func(ctx *RecoveryActionCtx) error {
			wc := ctx.Worktree
			strategy := "--theirs"
			if ctx.Params["strategy"] == "ours" {
				strategy = "--ours"
			}

			files, err := wc.ConflictedFiles()
			if err != nil {
				return fmt.Errorf("list conflicted files: %w", err)
			}
			if len(files) == 0 {
				ctx.Log("no conflicted files found")
				return nil
			}

			for _, f := range files {
				// Use exec.Command with args slice to avoid shell injection
				// from filenames containing spaces, quotes, or semicolons.
				cmd := exec.Command("git", "checkout", strategy, "--", f)
				cmd.Dir = wc.Dir
				if out, err := cmd.CombinedOutput(); err != nil {
					return fmt.Errorf("resolve conflict in %s with %s: %w\n%s", f, strategy, err, out)
				}
			}

			// Stage and commit the resolution.
			if err := wc.RunCommand("git add -A"); err != nil {
				return fmt.Errorf("stage resolved files: %w", err)
			}
			if _, err := wc.Commit(fmt.Sprintf("resolve conflicts using %s strategy", strategy)); err != nil {
				return fmt.Errorf("commit conflict resolution: %w", err)
			}
			ctx.Log(fmt.Sprintf("resolved %d conflicted files with %s", len(files), strategy))
			return nil
		},
	}
}

// actionTargetedFix records that an apprentice dispatch is needed to fix a
// specific issue. The actual dispatch is wired in a later task.
func actionTargetedFix() RecoveryAction {
	return RecoveryAction{
		Name:             "targeted-fix",
		Description:      "Record a targeted fix request for apprentice dispatch",
		RequiresWorktree: false,
		MaxRetries:       3,
		Fn: func(ctx *RecoveryActionCtx) error {
			issue := ctx.Params["issue"]
			if issue == "" {
				return fmt.Errorf("targeted-fix: missing 'issue' parameter")
			}
			ctx.Log(fmt.Sprintf("targeted-fix recorded: %s (awaiting apprentice dispatch)", issue))
			return nil
		},
	}
}

// actionRebuild runs 'go build ./...' in the worktree and captures output.
func actionRebuild() RecoveryAction {
	return RecoveryAction{
		Name:             "rebuild",
		Description:      "Run 'go build ./...' in the worktree and capture output",
		RequiresWorktree: true,
		MaxRetries:       3,
		Fn: func(ctx *RecoveryActionCtx) error {
			wc := ctx.Worktree
			output, err := wc.RunCommandOutput("go build ./...")
			if err != nil {
				ctx.Params["build_output"] = output
				return fmt.Errorf("build failed: %w\n%s", err, output)
			}
			ctx.Log("rebuild succeeded")
			return nil
		},
	}
}

// actionResummon marks the bead for apprentice re-summon. The executor wiring
// will handle the actual re-summon.
func actionResummon() RecoveryAction {
	return RecoveryAction{
		Name:             "resummon",
		Description:      "Mark bead for apprentice re-summon",
		RequiresWorktree: false,
		MaxRetries:       3,
		Fn: func(ctx *RecoveryActionCtx) error {
			ctx.Log(fmt.Sprintf("marking %s for re-summon", ctx.TargetBeadID))
			return nil
		},
	}
}

// actionResetToStep resets execution to the step specified in Params["step"].
// The executor wiring reads this and performs the actual graph reset.
func actionResetToStep() RecoveryAction {
	return RecoveryAction{
		Name:             "reset-to-step",
		Description:      "Reset bead execution to a specific step",
		RequiresWorktree: false,
		MaxRetries:       2,
		Fn: func(ctx *RecoveryActionCtx) error {
			step := ctx.Params["step"]
			if step == "" {
				return fmt.Errorf("reset-to-step: missing 'step' parameter")
			}
			ctx.Log(fmt.Sprintf("marking reset to step: %s", step))
			return nil
		},
	}
}

// validCommitSHA matches a hex SHA (7-40 characters, the common range for
// abbreviated and full SHAs). Used to guard against command injection in
// actions that interpolate commit hashes into shell commands.
var validCommitSHA = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)

// generateRecoveryAttemptID generates a random attempt ID in the same format
// as store.generateAttemptID ("ra-" + 8 hex chars). We generate it here so
// the caller retains the ID after RecordRecoveryAttempt (which takes the
// struct by value and would lose an internally generated ID).
func generateRecoveryAttemptID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "ra-00000000"
	}
	return "ra-" + hex.EncodeToString(b)
}

// actionEscalate sets the bead priority to P0 and adds a 'needs-human' label.
func actionEscalate() RecoveryAction {
	return RecoveryAction{
		Name:             "escalate",
		Description:      "Escalate bead to P0 priority and add needs-human label",
		RequiresWorktree: false,
		MaxRetries:       1,
		Fn: func(ctx *RecoveryActionCtx) error {
			if err := store.UpdateBead(ctx.TargetBeadID, map[string]interface{}{
				"priority": 0,
			}); err != nil {
				return fmt.Errorf("set priority to P0: %w", err)
			}

			if err := store.AddLabel(ctx.TargetBeadID, "needs-human"); err != nil {
				return fmt.Errorf("add needs-human label: %w", err)
			}

			ctx.Log(fmt.Sprintf("escalated %s to P0 with needs-human label", ctx.TargetBeadID))
			return nil
		},
	}
}
