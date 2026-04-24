package git

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrMergeRace is returned when the base branch advances during the
// rebase→verify→merge window and all retry attempts are exhausted.
// Callers can check for this with errors.Is to distinguish a retryable
// race from a terminal failure (e.g. rebase conflict).
var ErrMergeRace = errors.New("merge race: main advanced during landing")

// defaultMaxMergeAttempts is the baseline number of rebase→verify→ff-only
// cycles MergeToMain will attempt before returning ErrMergeRace. Overridable
// via SPIRE_MAX_MERGE_ATTEMPTS so smoke tests with known contention can tune
// the budget without a rebuild.
const defaultMaxMergeAttempts = 3

// maxMergeAttempts is the effective merge-attempt budget, resolved once at
// package load and consumed by MergeToMain. Tests read this value back to
// assert the actual attempt count.
var maxMergeAttempts = resolveMaxMergeAttempts()

// MaxMergeAttempts returns the effective merge-attempt budget, honoring the
// SPIRE_MAX_MERGE_ATTEMPTS env override. Exposed so wizard startup can log the
// value alongside other recovery-budget config.
func MaxMergeAttempts() int {
	return maxMergeAttempts
}

// resolveMaxMergeAttempts reads SPIRE_MAX_MERGE_ATTEMPTS and falls back to
// defaultMaxMergeAttempts on missing/invalid values. Invalid values (non-int
// or ≤0) are treated as missing — we never produce a zero budget that would
// skip the retry loop entirely.
func resolveMaxMergeAttempts() int {
	if v := os.Getenv("SPIRE_MAX_MERGE_ATTEMPTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxMergeAttempts
}

// hasRebaseConflicts reports whether git status (porcelain format) indicates
// unresolved merge/rebase conflicts. UU = both modified, AA = both added.
func hasRebaseConflicts(status string) bool {
	return strings.Contains(status, "UU ") || strings.Contains(status, "AA ")
}

// deriveBeadIDFromPath extracts the bead-ID token from a path like
// `<repo>/.worktrees/<beadID>` or `<repo>/.worktrees/<beadID>-feature`. The
// bead ID is the last path segment up to the first '-' separator. Returns ""
// when dir has no "<base>-<id>" shape (defensive — unknown input should not
// touch sibling paths).
func deriveBeadIDFromPath(dir string) string {
	base := filepath.Base(dir)
	if base == "" || base == "." || base == "/" {
		return ""
	}
	// Bead IDs are "<prefix>-<slug>" — the first dash separates prefix from
	// slug, and the slug itself has no dash. A suffix like "-feature" adds a
	// second dash. Split on '-' and keep the first two tokens joined.
	parts := strings.Split(base, "-")
	if len(parts) < 2 {
		return ""
	}
	return parts[0] + "-" + parts[1]
}

// isSameBeadWorktree reports whether path names a worktree belonging to the
// given bead ID. Match rules: the final path component must be exactly beadID
// or start with "beadID-" (a suffix like "-feature"). Guards against the
// common collision where one bead ID is a prefix of another
// (e.g. spi-0fek6l vs. spi-0fek6l2) by anchoring on the trailing character.
func isSameBeadWorktree(path, beadID string) bool {
	base := filepath.Base(path)
	if base == "" || beadID == "" {
		return false
	}
	if base == beadID {
		return true
	}
	return strings.HasPrefix(base, beadID+"-")
}

// cleanupStaleSiblingWorktrees force-removes any `.worktrees/<beadID>*` paths
// that are NOT targetDir, and whose bead-ID prefix matches targetDir's bead ID.
// This is the "stale sibling" case seen in the spi-0fek6l scenario: a prior
// wizard left `.worktrees/<beadID>-feature` checked out on `feat/<beadID>` and
// the next attempt to create `.worktrees/<beadID>` on the same branch fails
// with "'feat/<beadID>' is already used by worktree at ...".
//
// The cleanup is a best-effort, idempotent pass: each failed removal is logged
// but does not fail the caller. A concurrent wizard re-creating a sibling mid-
// pass is tolerated — the next constructor invocation will re-scan and clean
// again. Unknown target paths (no bead-ID prefix inferrable) are skipped.
func cleanupStaleSiblingWorktrees(rc *RepoContext, targetDir string, log func(string, ...interface{})) {
	beadID := deriveBeadIDFromPath(targetDir)
	if beadID == "" {
		return
	}
	parent := filepath.Dir(targetDir)
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	targetBase := filepath.Base(targetDir)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if entry.Name() == targetBase {
			continue
		}
		if !isSameBeadWorktree(entry.Name(), beadID) {
			continue
		}
		sibling := filepath.Join(parent, entry.Name())
		if log != nil {
			log("removing stale sibling worktree %s for bead %s", sibling, beadID)
		}
		if err := rc.ForceRemoveWorktree(sibling); err != nil {
			if log != nil {
				log("warning: force-remove sibling %s: %s", sibling, err)
			}
			// Fall through and try os.RemoveAll so the path is gone even if
			// git's administrative record was already cleaned up.
		}
		if err := os.RemoveAll(sibling); err != nil && log != nil {
			log("warning: remove sibling dir %s: %s", sibling, err)
		}
	}
	// Prune git's internal worktree registry so any records for the removed
	// siblings don't linger and block a fresh `git worktree add`.
	_ = rc.PruneWorktrees()
}

// CleanupStaleSiblingWorktrees is the exported form of the internal helper so
// the recovery mechanical (pkg/executor) can invoke it when the classifier
// dispatches the stale-worktree repair. targetDir is the prospective new
// staging-worktree path; siblings for the same bead are force-removed.
//
// Deprecated: merge-recovery code paths should call
// CleanupStaleSiblingWorktreesSafe instead. This variant force-removes sibling
// worktrees unconditionally and can destroy uncommitted work or an in-progress
// rebase. It is kept only for callers that know the worktrees they're scrubbing
// are fully disposable (e.g. explicit teardown, not automated recovery).
func CleanupStaleSiblingWorktrees(repoPath, targetDir string, log func(string, ...interface{})) {
	rc := &RepoContext{Dir: repoPath, Log: log}
	cleanupStaleSiblingWorktrees(rc, targetDir, log)
}

// SiblingCleanupFate records what CleanupStaleSiblingWorktreesSafe decided to
// do with a given sibling worktree. Exposed for callers (recovery mechanicals)
// that want to log per-sibling outcomes.
type SiblingCleanupFate struct {
	Path   string // absolute path to the sibling before cleanup
	Action string // "removed", "renamed", "skipped-live", "error"
	// NewPath is set when Action == "renamed" — the quarantine path.
	NewPath string
	// Reason is a short human-readable description of why the sibling took
	// this fate (which gate tripped, or the removal error).
	Reason string
}

// CleanupStaleSiblingWorktreesSafe scans `.worktrees/<beadID>*` siblings of
// targetDir and applies a four-gate safety check before force-removing any
// sibling. The goal is to clear the branch-in-use lock that blocks
// `git worktree add` WITHOUT destroying in-flight work from a parallel wizard
// or an interrupted rebase.
//
// Gates (all must pass to force-remove):
//
//   - Gate A: `git status --porcelain` non-empty → uncommitted work present.
//   - Gate B: any of `.git/rebase-merge`, `.git/rebase-apply`,
//     `.git/MERGE_HEAD`, `.git/CHERRY_PICK_HEAD` exists → in-progress
//     rebase/merge/cherry-pick.
//   - Gate C: `rev-parse --abbrev-ref HEAD` doesn't match `feat/<beadID>` or
//     `staging/<beadID>` → branch mismatch, unsafe to touch.
//   - Gate D: `time.Since(stat.ModTime()) < 5*time.Minute` → sibling is
//     "fresh" (another wizard may be actively using it).
//
// All gates pass → force-remove as the unsafe variant did.
// Any gate fails → rename to `.worktrees/.abandoned-<unix-ts>-<base>` via
// os.Rename. Never destroy silently.
//
// Returns the list of fates so callers (log consumers, recovery audit) can
// record what happened per sibling.
func CleanupStaleSiblingWorktreesSafe(repoPath, targetDir string, log func(string, ...interface{})) []SiblingCleanupFate {
	rc := &RepoContext{Dir: repoPath, Log: log}
	return cleanupStaleSiblingWorktreesSafe(rc, targetDir, nil, log)
}

// CleanupStaleSiblingWorktreesSafeWithExtraRoots is the generalized form of
// CleanupStaleSiblingWorktreesSafe that also scans additional roots for
// same-bead worktrees. It is used by TerminalMerge to find stale sage or
// wizard worktrees living under $TMPDIR/spire-review/<name>/<bead> and
// $TMPDIR/spire-wizard/<name>/<bead> that would otherwise cause a branch
// collision when the merge staging worktree is created.
//
// extraScanRoots is a list of directories to scan in addition to the parent
// of targetDir. Each root is expanded via filepath.Glob with the pattern
// "<root>/*" — each matched subdirectory is itself scanned for
// `<beadID>*` entries and gated identically to the in-parent siblings.
// Example: passing "/tmp/spire-review" matches
// "/tmp/spire-review/wizard-spi-xxx-review" as a subdir, then scans that
// subdir for "spi-xxx*" children.
//
// The cwgiy9 in-wizard recovery design means no parallel cleric pod exists
// per bead; the only worktrees under these temp roots belong to the sage
// for this bead (or a dead sage from a prior run). Future cleric-pod
// reintroduction would need to widen this contract carefully — the extra
// roots would need per-bead scoping so one cleric's worktree can't be
// wiped by another bead's merge.
func CleanupStaleSiblingWorktreesSafeWithExtraRoots(repoPath, targetDir string, extraScanRoots []string, log func(string, ...interface{})) []SiblingCleanupFate {
	rc := &RepoContext{Dir: repoPath, Log: log}
	return cleanupStaleSiblingWorktreesSafe(rc, targetDir, extraScanRoots, log)
}

// cleanupStaleSiblingWorktreesSafe is the package-private driver used by the
// exported safe variants and by NewStagingWorktreeAt. See
// CleanupStaleSiblingWorktreesSafe for the full contract.
//
// extraScanRoots (optional): additional directories whose immediate children
// are scanned for same-bead worktrees. For each root "R", each subdirectory
// "R/<anything>" is treated as a containing directory and scanned for
// "<beadID>*" children. Each matched child is gated and force-removed /
// quarantined exactly like in-parent siblings.
func cleanupStaleSiblingWorktreesSafe(rc *RepoContext, targetDir string, extraScanRoots []string, log func(string, ...interface{})) []SiblingCleanupFate {
	beadID := deriveBeadIDFromPath(targetDir)
	if beadID == "" {
		return nil
	}
	parent := filepath.Dir(targetDir)
	targetBase := filepath.Base(targetDir)

	var fates []SiblingCleanupFate

	// Scan the immediate parent directory of targetDir.
	fates = append(fates, scanDirForSiblings(rc, parent, targetBase, beadID, log)...)

	// Scan each extra root's children (one directory deep), then within each
	// child directory look for `<beadID>*`. Matches only — a root with no
	// matching children is a no-op.
	for _, root := range extraScanRoots {
		// Glob each immediate child of the root. For "/tmp/spire-review",
		// children like "/tmp/spire-review/wizard-spi-xxx-review" are each
		// scanned for "spi-xxx*" within.
		subdirs, _ := filepath.Glob(filepath.Join(root, "*"))
		for _, sub := range subdirs {
			info, statErr := os.Stat(sub)
			if statErr != nil || !info.IsDir() {
				continue
			}
			fates = append(fates, scanDirForSiblings(rc, sub, "", beadID, log)...)
		}
	}

	// Prune git's internal worktree registry so any records for the removed
	// siblings don't linger and block a fresh `git worktree add`. The
	// quarantined paths still exist on disk; git will re-register them if
	// `git worktree add` ever targets them again.
	_ = rc.PruneWorktrees()
	return fates
}

// scanDirForSiblings scans dir for same-bead sibling worktrees, applying the
// four-gate safety check to each. targetBase (optional) is skipped to avoid
// acting on the worktree we're about to create ourselves. beadID anchors the
// name match — entries whose names don't start with beadID or match it
// exactly are ignored. Returns per-sibling fates.
func scanDirForSiblings(rc *RepoContext, dir, targetBase, beadID string, log func(string, ...interface{})) []SiblingCleanupFate {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var fates []SiblingCleanupFate
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if targetBase != "" && entry.Name() == targetBase {
			continue
		}
		// Skip already-quarantined siblings so repeat invocations don't
		// re-rename them into nested .abandoned-.abandoned-... chains.
		if strings.HasPrefix(entry.Name(), ".abandoned-") {
			continue
		}
		if !isSameBeadWorktree(entry.Name(), beadID) {
			continue
		}
		sibling := filepath.Join(dir, entry.Name())
		fate := evaluateSiblingGates(sibling, beadID)
		if fate.Action == "removed" {
			if log != nil {
				log("sibling %s: all gates passed — removing (bead %s)", sibling, beadID)
			}
			if err := rc.ForceRemoveWorktree(sibling); err != nil {
				if log != nil {
					log("warning: force-remove sibling %s: %s", sibling, err)
				}
				// Fall through and try os.RemoveAll so the path is gone even
				// if git's administrative record was already cleaned up.
			}
			if err := os.RemoveAll(sibling); err != nil && log != nil {
				log("warning: remove sibling dir %s: %s", sibling, err)
			}
			fates = append(fates, fate)
			continue
		}
		// Gate failed — rename instead of destroy. Never silently delete
		// in-flight or dirty work.
		quarantinePath := filepath.Join(dir, fmt.Sprintf(".abandoned-%d-%s", time.Now().Unix(), entry.Name()))
		if log != nil {
			log("sibling %s: %s — quarantining to %s (bead %s)", sibling, fate.Reason, quarantinePath, beadID)
		}
		if err := os.Rename(sibling, quarantinePath); err != nil {
			if log != nil {
				log("warning: rename sibling %s → %s: %s", sibling, quarantinePath, err)
			}
			fate.Action = "error"
			fate.Reason = fmt.Sprintf("rename failed: %s (original reason: %s)", err, fate.Reason)
		} else {
			fate.Action = "renamed"
			fate.NewPath = quarantinePath
		}
		fates = append(fates, fate)
	}
	return fates
}

// evaluateSiblingGates runs the four safety gates against a sibling worktree
// path. Returns a SiblingCleanupFate whose Action is either "removed" (all
// gates passed) or "skipped-live" (at least one gate failed). Reason names the
// first gate that failed, so the caller can surface it for debugging.
func evaluateSiblingGates(siblingPath, beadID string) SiblingCleanupFate {
	fate := SiblingCleanupFate{Path: siblingPath}

	// Gate A: uncommitted work (git status --porcelain non-empty).
	if out, err := exec.Command("git", "-C", siblingPath, "status", "--porcelain").Output(); err == nil {
		if strings.TrimSpace(string(out)) != "" {
			fate.Action = "skipped-live"
			fate.Reason = "gate A failed: uncommitted work (git status --porcelain non-empty)"
			return fate
		}
	}
	// A failure to read `git status` is treated as "unknown state" — fall
	// through to subsequent gates rather than declaring it safe to delete.

	// Gate B: in-progress rebase/merge/cherry-pick markers in .git.
	for _, marker := range []string{"rebase-merge", "rebase-apply", "MERGE_HEAD", "CHERRY_PICK_HEAD"} {
		if _, err := os.Stat(filepath.Join(siblingPath, ".git", marker)); err == nil {
			fate.Action = "skipped-live"
			fate.Reason = "gate B failed: in-progress " + marker
			return fate
		}
	}
	// Worktrees record their .git as a file pointing to the main repo's
	// admin dir; the rebase/merge markers land there. Resolve and check that
	// path too.
	if adminDir := resolveWorktreeGitDir(siblingPath); adminDir != "" {
		for _, marker := range []string{"rebase-merge", "rebase-apply", "MERGE_HEAD", "CHERRY_PICK_HEAD"} {
			if _, err := os.Stat(filepath.Join(adminDir, marker)); err == nil {
				fate.Action = "skipped-live"
				fate.Reason = "gate B failed: in-progress " + marker + " (admin dir)"
				return fate
			}
		}
	}

	// Gate C: branch mismatch. Expected "feat/<beadID>" or
	// "staging/<beadID>"; anything else is unsafe to force-remove.
	if out, err := exec.Command("git", "-C", siblingPath, "rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
		branch := strings.TrimSpace(string(out))
		expectedFeat := "feat/" + beadID
		expectedStaging := "staging/" + beadID
		if branch != expectedFeat && branch != expectedStaging {
			fate.Action = "skipped-live"
			fate.Reason = fmt.Sprintf("gate C failed: branch %q not in {%s, %s}", branch, expectedFeat, expectedStaging)
			return fate
		}
	}

	// Gate D: mtime < 5 minutes → treat as live.
	if stat, err := os.Stat(siblingPath); err == nil {
		if time.Since(stat.ModTime()) < 5*time.Minute {
			fate.Action = "skipped-live"
			fate.Reason = fmt.Sprintf("gate D failed: mtime %s (< 5m old)", stat.ModTime().Format(time.RFC3339))
			return fate
		}
	}

	fate.Action = "removed"
	return fate
}

// resolveWorktreeGitDir reads the `.git` file inside a worktree and returns
// the admin directory it points at (e.g.
// `<repo>/.git/worktrees/<sibling>`). Returns "" when the file is absent or
// unparseable — callers treat that as "no secondary admin dir to probe."
func resolveWorktreeGitDir(siblingPath string) string {
	data, err := os.ReadFile(filepath.Join(siblingPath, ".git"))
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(data))
	const prefix = "gitdir:"
	if !strings.HasPrefix(line, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(line, prefix))
}

// StagingWorktree manages a temporary git worktree for staging operations.
// It is the single point responsible for git worktree create/remove and
// main-worktree branch switching, ensuring the main worktree stays on its
// base branch throughout all staging work.
//
// It embeds WorktreeContext for all git operations, ensuring worktree/main-repo
// boundary is enforced through a single abstraction.
type StagingWorktree struct {
	WorktreeContext        // embedded — all git ops go through this
	tmpDir          string // temp directory parent (cleaned up on Close)
}

// NewStagingWorktree creates a new temporary git worktree checking out branch.
// baseBranch is the branch this was forked from (e.g. "main") — stored in the
// embedded WorktreeContext so methods like HasNewCommits work correctly.
// nameHint is included in the temp directory name for debugging (e.g. "spire-staging").
// userName and userEmail configure the git identity in the worktree.
// The caller must call Close() when done.
func NewStagingWorktree(repoPath, branch, baseBranch, nameHint, userName, userEmail string, log func(string, ...interface{})) (*StagingWorktree, error) {
	tmpDir, err := os.MkdirTemp("", nameHint+"-")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	dir := filepath.Join(tmpDir, "wt")
	rc := &RepoContext{Dir: repoPath, BaseBranch: baseBranch, Log: log}
	wcPtr, wtErr := rc.CreateWorktree(dir, branch)
	if wtErr != nil {
		os.RemoveAll(tmpDir)
		return nil, fmt.Errorf("worktree add %s: %w", branch, wtErr)
	}
	wc := *wcPtr

	wc.ConfigureUser(userName, userEmail)

	return &StagingWorktree{
		WorktreeContext: wc,
		tmpDir:          tmpDir,
	}, nil
}

// NewStagingWorktreeAt creates a staging worktree at a specific directory path.
// Unlike NewStagingWorktree (which creates a temp dir), this places the worktree
// at dir, making it discoverable by other processes that know the path.
// userName and userEmail configure the git identity in the worktree.
//
// Before creating the new worktree it force-removes any sibling worktrees for
// the same bead that might still be checked out on `branch` from a prior
// wizard run (e.g. `.worktrees/<beadID>-feature`). Without this, git refuses to
// check out the target path with "'<branch>' is already used by worktree at ..."
// and the step escalates to archmage. Sibling cleanup is scoped strictly to
// the same bead prefix so unrelated beads' worktrees are never touched.
//
// The caller must call Close() when done. Close removes the git worktree and
// the directory itself.
func NewStagingWorktreeAt(repoPath, dir, branch, baseBranch, userName, userEmail string, log func(string, ...interface{})) (*StagingWorktree, error) {
	rc := &RepoContext{Dir: repoPath, BaseBranch: baseBranch, Log: log}

	// Clean up stale sibling worktrees for this bead (not the target path).
	// The branch we're about to check out may already be in use by a sibling
	// path left over from a prior wizard — gate-checked cleanup renames
	// dirty/in-flight siblings to a `.abandoned-*` quarantine path so we
	// never silently destroy concurrent work, then force-removes the rest so
	// `git worktree add` doesn't collide.
	cleanupStaleSiblingWorktreesSafe(rc, dir, nil, log)

	// Clean up stale worktree at this path
	if _, err := os.Stat(dir); err == nil {
		rc.ForceRemoveWorktree(dir)
		os.RemoveAll(dir)
	}

	if err := os.MkdirAll(filepath.Dir(dir), 0755); err != nil {
		return nil, fmt.Errorf("create parent dir: %w", err)
	}

	wcPtr, wtErr := rc.CreateWorktree(dir, branch)
	if wtErr != nil {
		return nil, fmt.Errorf("worktree add %s at %s: %w", branch, dir, wtErr)
	}
	wc := *wcPtr

	wc.ConfigureUser(userName, userEmail)

	return &StagingWorktree{
		WorktreeContext: wc,
		// tmpDir is empty — no temp dir to clean up. Close() handles
		// git worktree removal; the dir itself is removed by git.
	}, nil
}

// ResumeStagingWorktree wraps an existing worktree directory in a StagingWorktree.
// The worktree already exists on disk and just needs to be wrapped for method access.
//
// Captures HEAD SHA as StartSHA for session-scoped commit detection. If HEAD
// cannot be read (e.g. worktree is corrupt), StartSHA is left empty and
// callers fall back to BaseBranch..HEAD comparison.
func ResumeStagingWorktree(repoPath, dir, branch, baseBranch string, log func(string, ...interface{})) *StagingWorktree {
	// Capture session baseline if the worktree exists.
	var startSHA string
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output(); err == nil {
		startSHA = strings.TrimSpace(string(out))
	}
	return &StagingWorktree{
		WorktreeContext: WorktreeContext{
			Dir:        dir,
			Branch:     branch,
			BaseBranch: baseBranch,
			RepoPath:   repoPath,
			StartSHA:   startSHA,
			Log:        log,
		},
	}
}

// FetchBranch fetches a specific branch from a remote into this staging worktree.
// Fetch operations live on StagingWorktree (not WorktreeContext) because
// WorktreeContext enforces local-ref-only semantics.
//
// Returns the underlying git error (including stderr) on failure. Callers that
// treat fetch as best-effort (e.g. the push-transport fallback in
// action_dispatch) may still discard it and rely on MergeBranch failing later,
// but surfacing a genuine fetch error (network, auth) here prevents the
// confusing "merge failed" message when the root cause was the fetch.
func (w *StagingWorktree) FetchBranch(remote, branch string) error {
	out, err := exec.Command("git", "-C", w.Dir, "fetch", remote, branch).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git fetch %s %s: %w\n%s", remote, branch, err, string(out))
	}
	return nil
}

// MergeBranch merges childBranch into this staging worktree's branch with
// linear history (no merge commits). Strategy:
//  1. Try ff-only merge — succeeds when staging hasn't diverged.
//  2. If ff-only fails, rebase the child onto staging, then ff-only again.
//
// All branch refs are local — MergeBranch does not fetch or push.
//
// On rebase conflict, resolver is called (if non-nil) to attempt resolution.
func (w *StagingWorktree) MergeBranch(childBranch string, resolver func(dir, branch string) error) error {
	w.logf("  merging %s into %s", childBranch, w.Branch)

	// Use local branch ref directly — no remote fetching.
	branchRef := childBranch

	// Step 1: Try fast-forward-only merge.
	if err := w.MergeFFOnly(branchRef); err == nil {
		return nil
	}
	w.logf("  ff-only failed, rebasing %s onto %s", branchRef, w.Branch)

	// Step 2: Rebase the child tip onto the current staging tip using detached
	// HEADs. Rebasing the branch name directly is fragile: it can fail if the
	// child branch is still checked out in another worktree, and some git
	// setups do not reliably resolve the staging branch name here. Using SHAs
	// keeps the rebase local to this worktree and avoids mutating the child ref.
	stagingTip, err := w.HeadSHA()
	if err != nil {
		return fmt.Errorf("read staging tip before rebase: %w", err)
	}
	childTipOut, err := exec.Command("git", "-C", w.Dir, "rev-parse", branchRef).CombinedOutput()
	if err != nil {
		return fmt.Errorf("resolve child branch %s: %w\n%s", branchRef, err, string(childTipOut))
	}
	childTip := strings.TrimSpace(string(childTipOut))

	rebaseCmd := exec.Command("git", "-C", w.Dir, "rebase", stagingTip, childTip)
	rebaseCmd.Env = os.Environ()
	if out, err := rebaseCmd.CombinedOutput(); err != nil {
		// Check if rebase stopped due to conflicts.
		status := w.StatusPorcelain()
		if hasRebaseConflicts(status) {
			if resolver == nil {
				exec.Command("git", "-C", w.Dir, "rebase", "--abort").Run()
				return fmt.Errorf("rebase conflict in %s: no resolver provided", childBranch)
			}
			if resolveErr := resolveRebaseConflicts(w.Dir, childBranch, resolver, w.logf); resolveErr != nil {
				return resolveErr
			}
		} else {
			exec.Command("git", "-C", w.Dir, "rebase", "--abort").Run()
			return fmt.Errorf("rebase %s onto %s failed: %s\n%s", branchRef, stagingTip, err, string(out))
		}
	}

	// Capture the rebased tip SHA (HEAD is now at the rebased result).
	rebasedSHA, _ := exec.Command("git", "-C", w.Dir, "rev-parse", "HEAD").Output()
	tip := strings.TrimSpace(string(rebasedSHA))

	// Switch back to the staging branch.
	if out, err := exec.Command("git", "-C", w.Dir, "checkout", w.Branch).CombinedOutput(); err != nil {
		return fmt.Errorf("checkout %s after rebase: %w\n%s", w.Branch, err, string(out))
	}

	// Step 3: ff-only merge the rebased commits — should succeed now.
	if err := w.MergeFFOnly(tip); err != nil {
		return fmt.Errorf("ff-only merge failed after rebase: %w", err)
	}
	return nil
}

// resolveRebaseConflicts handles a rebase that has stopped due to merge conflicts.
// It loops: call resolver → rebase --continue → check for new conflicts → repeat
// until the rebase completes or the resolver fails. Called after the initial
// rebase command has returned an error and the caller has confirmed conflicts
// are present (via hasRebaseConflicts).
//
// dir is the worktree directory in a mid-rebase state. branch is the logical
// branch name passed to the resolver for context.
func resolveRebaseConflicts(dir, branch string, resolver func(dir, branch string) error, logf func(string, ...any)) error {
	for {
		if logf != nil {
			logf("  resolving rebase conflicts for %s", branch)
		}
		if resolveErr := resolver(dir, branch); resolveErr != nil {
			exec.Command("git", "-C", dir, "rebase", "--abort").Run()
			return fmt.Errorf("conflict resolution failed during rebase: %w", resolveErr)
		}

		// Resolver succeeded — continue the rebase. May stop again if the
		// next commit in a multi-commit rebase also conflicts.
		contCmd := exec.Command("git", "-C", dir, "rebase", "--continue")
		contCmd.Env = os.Environ()
		contOut, contErr := contCmd.CombinedOutput()
		if contErr == nil {
			return nil // rebase completed
		}

		// Check if rebase --continue stopped on new conflicts (next commit).
		wc := &WorktreeContext{Dir: dir}
		status := wc.StatusPorcelain()
		if hasRebaseConflicts(status) {
			continue // loop to resolve the next batch
		}

		// Non-conflict error (e.g., empty commit after resolution).
		exec.Command("git", "-C", dir, "rebase", "--abort").Run()
		return fmt.Errorf("rebase --continue failed after resolution: %s\n%s", contErr, string(contOut))
	}
}

// RunBuild runs buildStr as a command in the worktree directory.
// buildStr is split on spaces and run directly (no shell).
func (w *StagingWorktree) RunBuild(buildStr string) error {
	out, err := w.RunCommandOutput(buildStr)
	if err != nil {
		w.logf("build failed: %s\n%s", err, out)
		return fmt.Errorf("%s: %w\n%s", buildStr, err, out)
	}
	w.logf("build passed")
	return nil
}

// RunTests runs testStr as a command in the worktree directory.
// testStr is split on spaces and run directly (no shell).
func (w *StagingWorktree) RunTests(testStr string) error {
	out, err := w.RunCommandOutput(testStr)
	if err != nil {
		w.logf("tests failed: %s\n%s", err, out)
		return fmt.Errorf("%s: %w\n%s", testStr, err, out)
	}
	w.logf("tests passed")
	return nil
}

// MergeToMain ensures the main worktree is on baseBranch, pulls it, and
// performs a ff-only merge of this staging branch into baseBranch.
// env is used for git operations that need identity (e.g. archmage git env).
//
// If ff-only fails (main has diverged), it rebases the staging branch onto
// baseBranch in a new temporary worktree. After a successful rebase, it
// verifies build (buildStr) and tests (testStr) in that worktree — empty
// strings skip the respective step — then retries the ff-only merge.
// Never force-merges; returns an error if rebase fails.
//
// resolver, when non-nil, is called to resolve merge conflicts during rebase.
// If nil, rebase conflicts are terminal errors (backward-compatible behavior).
// When a resolver is provided and conflicts occur, the function will attempt
// resolution and retry up to maxMergeAttempts times before returning ErrMergeRace.
//
// NOTE: Main-repo operations (checkout, pull, merge, worktree lifecycle) go
// through RepoContext. The rebase operations target a temporary worktree and
// remain as raw exec.Command calls since WorktreeContext doesn't expose rebase.
func (w *StagingWorktree) MergeToMain(baseBranch string, env []string, buildStr, testStr string, resolver func(dir, branch string) error) error {
	rc := &RepoContext{Dir: w.RepoPath, BaseBranch: baseBranch, Log: w.Log}

	// Ensure main worktree is on baseBranch.
	if rc.CurrentBranch() != baseBranch {
		if err := rc.Checkout(baseBranch); err != nil {
			return err
		}
	}

	// Pull baseBranch to be up to date.
	if pullErr := rc.PullFFOnly("origin", baseBranch, env); pullErr != nil {
		w.logf("warning: pull %s: %s", baseBranch, pullErr)
	}

	// Belt-and-suspenders: verify we're still on baseBranch after the pull.
	if rc.CurrentBranch() != baseBranch {
		if err := rc.Checkout(baseBranch); err != nil {
			return err
		}
	}

	w.logf("ff-only merge %s → %s (committer: archmage)", w.Branch, baseBranch)

	// First attempt: fast-forward only merge (common case — no rebase needed).
	if err := rc.MergeFFOnly(w.Branch, env); err == nil {
		return nil // success — done
	} else {
		w.logf("ff-only failed: %s — rebasing staging onto %s", err, baseBranch)
	}

	// ff-only failed — main has diverged. Enter a bounded retry loop:
	// pull main → rebase → verify build/tests → ff-only merge.
	// If main advances again during verification, loop again.
	for attempt := 0; attempt < maxMergeAttempts; attempt++ {
		if attempt > 0 {
			w.logf("merge race detected, retry %d/%d", attempt+1, maxMergeAttempts)
		}

		// Re-pull baseBranch to pick up any advances since last attempt.
		if pullErr := rc.PullFFOnly("origin", baseBranch, env); pullErr != nil {
			w.logf("warning: pull %s (attempt %d): %s", baseBranch, attempt+1, pullErr)
		}

		// Rebase staging onto the (possibly updated) baseBranch in-place.
		w.logf("rebasing %s onto %s in place (attempt %d)", w.Branch, baseBranch, attempt+1)
		rebaseCmd := exec.Command("git", "-C", w.Dir, "rebase", baseBranch)
		rebaseCmd.Env = os.Environ()
		if out, rbErr := rebaseCmd.CombinedOutput(); rbErr != nil {
			// Check if rebase stopped due to conflicts.
			status := w.StatusPorcelain()
			if hasRebaseConflicts(status) {
				if resolver == nil {
					exec.Command("git", "-C", w.Dir, "rebase", "--abort").Run()
					return fmt.Errorf("rebase %s onto %s hit conflicts (no resolver, aborting): %s\n%s", w.Branch, baseBranch, rbErr, string(out))
				}
				if resolveErr := resolveRebaseConflicts(w.Dir, w.Branch, resolver, w.logf); resolveErr != nil {
					w.logf("conflict resolution failed (attempt %d): %s", attempt+1, resolveErr)
					continue // try next attempt
				}
				// Resolution succeeded, fall through to build/test verification.
			} else {
				exec.Command("git", "-C", w.Dir, "rebase", "--abort").Run()
				return fmt.Errorf("rebase %s onto %s failed (aborting, will not force merge): %s\n%s", w.Branch, baseBranch, rbErr, string(out))
			}
		}

		// Re-verify build after rebase.
		if buildStr != "" {
			w.logf("verifying build after rebase (attempt %d)", attempt+1)
			out, buildErr := w.RunCommandOutput(buildStr)
			if buildErr != nil {
				return fmt.Errorf("build failed after rebase (aborting merge): %s\n%s", buildErr, out)
			}
		}

		// Re-verify tests after rebase.
		if testStr != "" {
			w.logf("running tests after rebase (attempt %d)", attempt+1)
			out, testErr := w.RunCommandOutput(testStr)
			if testErr != nil {
				return fmt.Errorf("tests failed after rebase (aborting merge): %s\n%s", testErr, out)
			}
		}

		// Attempt ff-only merge — succeeds unless main advanced again.
		w.logf("retrying ff-only merge after rebase (attempt %d)", attempt+1)
		if err := rc.MergeFFOnly(w.Branch, env); err == nil {
			return nil
		} else {
			w.logf("ff-only failed again (attempt %d): %s", attempt+1, err)
		}
	}

	return fmt.Errorf("ff-only merge failed after %d rebase attempts (will not force merge): %w", maxMergeAttempts, ErrMergeRace)
}

// Close removes the worktree from git and deletes its temp directory.
// It is safe to call multiple times.
func (w *StagingWorktree) Close() error {
	if w.Dir != "" {
		rc := &RepoContext{Dir: w.RepoPath, Log: w.Log}
		rc.ForceRemoveWorktree(w.Dir)
		w.Dir = ""
	}
	if w.tmpDir != "" {
		os.RemoveAll(w.tmpDir)
		w.tmpDir = ""
	}
	return nil
}
