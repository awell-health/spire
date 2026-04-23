package git

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrBranchNotFound indicates EnsureWorktreeAt was asked to recreate a worktree
// for a branch that does not exist in the repo. Callers use errors.Is to
// distinguish this from other worktree-add failures so the dispatch policy can
// surface a "run spire reset --hard" message when both path and branch are gone.
var ErrBranchNotFound = errors.New("branch not found")

// EnsureWorktreeAt guarantees a worktree for branch exists at path. Pure
// primitive: no policy, no logging. Returns nil when the directory already
// exists and is a worktree at branch. Otherwise runs git worktree add path
// branch. When branch does not exist, returns a wrapped ErrBranchNotFound so
// the dispatch layer can surface the right recovery message.
//
// repoDir must point at the main repo; the worktree add runs from there so
// git resolves refs against the main worktree's .git.
func EnsureWorktreeAt(repoDir, path, branch string) error {
	if path == "" {
		return fmt.Errorf("ensure worktree at: empty path")
	}
	if branch == "" {
		return fmt.Errorf("ensure worktree at %s: empty branch", path)
	}
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		cur, cErr := currentBranchAt(path)
		if cErr == nil && cur == branch {
			return nil
		}
	}
	rc := &RepoContext{Dir: repoDir}
	if !rc.BranchExists(branch) {
		return fmt.Errorf("ensure worktree at %s: %w: %s", path, ErrBranchNotFound, branch)
	}
	cmd := exec.Command("git", "worktree", "add", path, branch)
	cmd.Dir = repoDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git worktree add %s %s: %w: %s", path, branch, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// currentBranchAt returns the branch name currently checked out at dir.
// Returns empty string when dir is not a worktree or HEAD is detached.
func currentBranchAt(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// WorktreeContext is the single abstraction for all git operations inside a
// worktree. Every git command that runs in a worktree must go through this
// type so that:
//   - Dir is always used as the working directory (no accidental main-repo ops)
//   - BaseBranch is a local ref, not origin/* (worktrees don't always have origin fetched)
//   - Config is scoped with --worktree (no pollution of the main repo's .git/config)
//
// Forbidden operations (these MUST NOT exist on WorktreeContext):
//   - Checkout — worktrees don't switch branches
//   - SetGlobalConfig — use --worktree flag instead
type WorktreeContext struct {
	Dir        string              // absolute path to this worktree
	Branch     string              // branch checked out in this worktree
	BaseBranch string              // the branch this was forked from (e.g. "main")
	RepoPath   string              // the main repo (for worktree management only)
	StartSHA   string              // HEAD SHA captured at session start (for session-scoped commit detection)
	Log        func(string, ...any) // optional structured logger; nil = silent
}

// logf logs a message if a logger is set. Nil-safe.
func (wc *WorktreeContext) logf(format string, args ...any) {
	if wc.Log != nil {
		wc.Log(format, args...)
	}
}

// Commit stages all changes and commits with the given message.
// Returns the commit SHA. If there are no staged changes after git add,
// returns ("", nil).
//
// Before staging, it removes any files matching the patterns in cleanFiles
// (e.g. prompt files that should not be committed). Pass nil to skip cleanup.
func (wc *WorktreeContext) Commit(msg string, cleanFiles ...string) (string, error) {
	// Remove specified files before staging
	for _, f := range cleanFiles {
		os.Remove(filepath.Join(wc.Dir, f))
	}

	// Stage all
	if err := exec.Command("git", "-C", wc.Dir, "add", "-A").Run(); err != nil {
		return "", fmt.Errorf("git add: %w", err)
	}

	// Check if there's anything staged
	if exec.Command("git", "-C", wc.Dir, "diff", "--cached", "--quiet").Run() == nil {
		return "", nil // nothing staged
	}

	// Commit
	commitCmd := exec.Command("git", "-C", wc.Dir, "commit", "-m", msg)
	if out, err := commitCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git commit: %w\n%s", err, string(out))
	}

	sha, err := wc.HeadSHA()
	if err == nil {
		wc.logf("committed %s on branch %s in %s", sha, wc.Branch, wc.Dir)
	}
	return sha, err
}

// Push pushes the worktree's branch to the given remote.
func (wc *WorktreeContext) Push(remote string) error {
	pushCmd := exec.Command("git", "-C", wc.Dir, "push", "-u", remote, wc.Branch)
	pushCmd.Env = os.Environ()
	if out, err := pushCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git push %s %s: %w\n%s", remote, wc.Branch, err, string(out))
	}
	return nil
}

// HasNewCommits returns true if there are commits on HEAD that are not on
// BaseBranch. Uses local refs only — no origin/ prefix — because worktrees
// don't always have origin fetched.
func (wc *WorktreeContext) HasNewCommits() (bool, error) {
	logCmd := exec.Command("git", "-C", wc.Dir, "log", wc.BaseBranch+"..HEAD", "--oneline")
	out, err := logCmd.Output()
	if err != nil {
		return false, fmt.Errorf("git log %s..HEAD: %w", wc.BaseBranch, err)
	}
	return len(strings.TrimSpace(string(out))) > 0, nil
}

// HasNewCommitsSinceStart returns true if there are commits on HEAD that were
// not present at session start. When StartSHA is set, it compares StartSHA..HEAD
// (session-scoped detection). When StartSHA is empty, it falls back to
// BaseBranch..HEAD (the original HasNewCommits behavior). The fallback exists
// only for callers that manually construct a WorktreeContext without using the
// constructors (CreateWorktree, CreateWorktreeNewBranch, ResumeWorktreeContext)
// — all constructors now capture StartSHA at creation time.
//
// On any comparison error, returns (false, err) — never assumes commits exist.
func (wc *WorktreeContext) HasNewCommitsSinceStart() (bool, error) {
	base := wc.StartSHA
	if base == "" {
		base = wc.BaseBranch
	}
	if base == "" {
		return false, fmt.Errorf("no StartSHA or BaseBranch set for commit comparison")
	}
	logCmd := exec.Command("git", "-C", wc.Dir, "log", base+"..HEAD", "--oneline")
	out, err := logCmd.Output()
	if err != nil {
		return false, fmt.Errorf("git log %s..HEAD: %w", base, err)
	}
	return len(strings.TrimSpace(string(out))) > 0, nil
}

// Diff returns the diff between the given base ref and HEAD.
// For worktree use, pass wc.BaseBranch (a local ref). If you need the
// three-dot diff (merge-base), use DiffMergeBase instead.
func (wc *WorktreeContext) Diff(base string) (string, error) {
	cmd := exec.Command("git", "-C", wc.Dir, "diff", base+"..HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git diff %s..HEAD: %w", base, err)
	}
	return string(out), nil
}

// DiffMergeBase returns the three-dot diff (from merge-base) between base and HEAD.
func (wc *WorktreeContext) DiffMergeBase(base string) (string, error) {
	cmd := exec.Command("git", "-C", wc.Dir, "diff", base+"...HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git diff %s...HEAD: %w", base, err)
	}
	return string(out), nil
}

// RunCommand runs an arbitrary command string in the worktree directory.
// The command is executed via "sh -c" for shell expansion.
func (wc *WorktreeContext) RunCommand(cmdStr string) error {
	cmd := exec.Command("sh", "-c", cmdStr)
	cmd.Dir = wc.Dir
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// RunCommandOutput runs a command in the worktree directory and returns combined output.
// The command is executed via "sh -c" for shell expansion, consistent with RunCommand.
func (wc *WorktreeContext) RunCommandOutput(cmdStr string) (string, error) {
	cmd := exec.Command("sh", "-c", cmdStr)
	cmd.Dir = wc.Dir
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// HeadSHA returns the current HEAD commit SHA.
func (wc *WorktreeContext) HeadSHA() (string, error) {
	out, err := exec.Command("git", "-C", wc.Dir, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// HasUncommittedChanges returns true if the working tree has uncommitted changes.
func (wc *WorktreeContext) HasUncommittedChanges() bool {
	out, _ := exec.Command("git", "-C", wc.Dir, "status", "--porcelain").Output()
	return len(strings.TrimSpace(string(out))) > 0
}

// ConfigureUser sets user.name and user.email in the worktree-scoped config.
// Uses --worktree flag so the setting doesn't pollute the main repo's config.
// Also enables extensions.worktreeConfig on the main repo if needed.
func (wc *WorktreeContext) ConfigureUser(name, email string) {
	// Enable worktree-scoped config on the main repo
	exec.Command("git", "-C", wc.RepoPath, "config", "extensions.worktreeConfig", "true").Run()
	// Set user identity scoped to this worktree only
	exec.Command("git", "-C", wc.Dir, "config", "--worktree", "user.name", name).Run()
	exec.Command("git", "-C", wc.Dir, "config", "--worktree", "user.email", email).Run()
}

// Merge attempts to merge the given ref into the worktree's current branch.
// Uses --no-edit to avoid opening an editor. Returns the combined output and
// any error from the merge command.
func (wc *WorktreeContext) Merge(ref string) (string, error) {
	out, err := exec.Command("git", "-C", wc.Dir, "merge", "--no-edit", ref).CombinedOutput()
	return string(out), err
}

// MergeFFOnly performs a fast-forward-only merge of the given ref.
// Returns an error if the merge cannot be completed as a fast-forward.
func (wc *WorktreeContext) MergeFFOnly(ref string) error {
	out, err := exec.Command("git", "-C", wc.Dir, "merge", "--ff-only", ref).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git merge --ff-only %s: %w\n%s", ref, err, string(out))
	}
	wc.logf("ff-only merge %s into %s", ref, wc.Branch)
	return nil
}

// MergeAbort aborts an in-progress merge. Safe to call even if no merge is active.
func (wc *WorktreeContext) MergeAbort() {
	exec.Command("git", "-C", wc.Dir, "merge", "--abort").Run()
}

// StatusPorcelain returns the machine-readable status output (git status --porcelain).
func (wc *WorktreeContext) StatusPorcelain() string {
	out, _ := exec.Command("git", "-C", wc.Dir, "status", "--porcelain").Output()
	return string(out)
}

// EnsureRemoteRef fetches a ref from a remote so it's available in this worktree.
// Worktrees share refs with the main repo, so the fetch runs against RepoPath.
// This is the ONLY operation that touches the remote — all other methods use local refs.
func (wc *WorktreeContext) EnsureRemoteRef(remote, ref string) {
	exec.Command("git", "-C", wc.RepoPath, "fetch", remote, ref).Run()
}

// ApplyBundle fetches a git bundle at bundlePath and force-updates targetBranch
// to point at the bundle's HEAD ref. Used to materialize an apprentice-produced
// bundle as a local branch inside the wizard's staging worktree before merge.
//
// The bundle's prerequisites (commits referenced but not carried) must already
// be present in the repo — callers must ensure the base branch is up to date.
//
// The "+" prefix force-updates the ref; this makes replays idempotent (a
// re-applied bundle resets the branch rather than failing on non-fast-forward).
func (wc *WorktreeContext) ApplyBundle(bundlePath, targetBranch string) error {
	return wc.applyBundleAtPath(bundlePath, targetBranch)
}

// ApplyBundleFromReader streams bundle bytes from r into a temp file owned by
// this package, then applies it to targetBranch via the same fetch logic as
// ApplyBundle. The temp file lives in os.TempDir() — git fetch <bundle> does
// not require same-FS placement — so this works against linked worktrees
// (where the worktree's .git is a pointer file, not a directory).
//
// The caller owns r's lifecycle (this method does not Close it). The temp
// file is removed before return.
func (wc *WorktreeContext) ApplyBundleFromReader(r io.Reader, targetBranch string) error {
	tmp, err := os.CreateTemp("", "spire-bundle-*.bundle")
	if err != nil {
		return fmt.Errorf("apply bundle from reader: create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return fmt.Errorf("apply bundle from reader: copy to %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("apply bundle from reader: close %s: %w", tmpPath, err)
	}

	if err := wc.applyBundleAtPath(tmpPath, targetBranch); err != nil {
		return fmt.Errorf("apply bundle from reader: %w", err)
	}
	return nil
}

// applyBundleAtPath runs the underlying `git fetch <bundle> +HEAD:<branch>`
// from the worktree dir. Shared by ApplyBundle and ApplyBundleFromReader so
// the fetch refspec/flags don't drift between callers.
func (wc *WorktreeContext) applyBundleAtPath(bundlePath, targetBranch string) error {
	out, err := exec.Command("git", "-C", wc.Dir, "fetch",
		bundlePath, "+HEAD:"+targetBranch).CombinedOutput()
	if err != nil {
		return fmt.Errorf("apply bundle %s -> %s: %w\n%s", bundlePath, targetBranch, err, string(out))
	}
	return nil
}

// ConflictedFiles returns the list of files with unresolved merge conflicts.
func (wc *WorktreeContext) ConflictedFiles() ([]string, error) {
	out, err := exec.Command("git", "-C", wc.Dir, "diff", "--name-only", "--diff-filter=U").Output()
	if err != nil {
		return nil, fmt.Errorf("git diff --diff-filter=U: %w", err)
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

// CommitMerge commits an in-progress merge (after conflict resolution) using
// the default merge message. Equivalent to "git commit --no-edit".
func (wc *WorktreeContext) CommitMerge() error {
	if out, err := exec.Command("git", "-C", wc.Dir, "commit", "--no-edit").CombinedOutput(); err != nil {
		return fmt.Errorf("git commit --no-edit: %w\n%s", err, out)
	}
	return nil
}

// DiffNameOnly returns the list of file paths changed between the given ref and HEAD.
func (wc *WorktreeContext) DiffNameOnly(ref string) ([]string, error) {
	out, err := exec.Command("git", "-C", wc.Dir, "diff", ref, "--name-only").Output()
	if err != nil {
		return nil, fmt.Errorf("git diff %s --name-only: %w", ref, err)
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

// Cleanup removes this worktree from git and deletes its directory.
func (wc *WorktreeContext) Cleanup() {
	if wc.Dir != "" {
		exec.Command("git", "-C", wc.RepoPath, "worktree", "remove", "--force", wc.Dir).Run()
		os.RemoveAll(wc.Dir)
	}
}

// ConflictState describes an in-progress conflicted operation in a worktree.
// InProgressOp is one of "rebase", "merge", "cherry-pick", or "" if no
// operation is paused. HeadSHA is the current HEAD. IncomingSHA is the
// commit being applied (rebase: the cherry being picked onto the new base;
// merge: the other side; cherry-pick: the source). Empty strings are
// returned when the field cannot be resolved.
type ConflictState struct {
	InProgressOp string
	HeadSHA      string
	IncomingSHA  string
}

// DetectConflictState inspects the worktree's .git/ directory for in-progress
// rebase / merge / cherry-pick state and resolves the HEAD and incoming SHAs.
// When the worktree is a linked worktree (shares .git with the main repo),
// it reads from the worktree's private gitdir rather than the main repo .git.
// Returns an empty state (InProgressOp == "") when no operation is paused.
func (wc *WorktreeContext) DetectConflictState() ConflictState {
	state := ConflictState{}
	gitDir := wc.resolveGitDir()
	if gitDir == "" {
		return state
	}

	// Current HEAD.
	if out, err := exec.Command("git", "-C", wc.Dir, "rev-parse", "HEAD").Output(); err == nil {
		state.HeadSHA = strings.TrimSpace(string(out))
	}

	// Detect operation + resolve incoming SHA.
	switch {
	case fileExists(filepath.Join(gitDir, "rebase-merge")) || fileExists(filepath.Join(gitDir, "rebase-apply")):
		state.InProgressOp = "rebase"
		// REBASE_HEAD points to the commit being replayed.
		if sha, ok := wc.readRefFile("REBASE_HEAD"); ok {
			state.IncomingSHA = sha
			break
		}
		// Fallback: rebase-merge/stopped-sha (interactive) or rebase-apply/original-commit.
		if sha, ok := readFileTrim(filepath.Join(gitDir, "rebase-merge", "stopped-sha")); ok {
			state.IncomingSHA = sha
		} else if sha, ok := readFileTrim(filepath.Join(gitDir, "rebase-apply", "original-commit")); ok {
			state.IncomingSHA = sha
		}
	case fileExists(filepath.Join(gitDir, "CHERRY_PICK_HEAD")):
		state.InProgressOp = "cherry-pick"
		if sha, ok := wc.readRefFile("CHERRY_PICK_HEAD"); ok {
			state.IncomingSHA = sha
		}
	case fileExists(filepath.Join(gitDir, "MERGE_HEAD")):
		state.InProgressOp = "merge"
		if sha, ok := wc.readRefFile("MERGE_HEAD"); ok {
			state.IncomingSHA = sha
		}
	}
	return state
}

// readRefFile resolves a pseudo-ref (REBASE_HEAD / MERGE_HEAD / CHERRY_PICK_HEAD)
// via git rev-parse so the caller gets the full SHA regardless of where git
// stores the pointer (packed refs, worktree-private gitdir, etc.).
func (wc *WorktreeContext) readRefFile(ref string) (string, bool) {
	out, err := exec.Command("git", "-C", wc.Dir, "rev-parse", ref).Output()
	if err != nil {
		return "", false
	}
	sha := strings.TrimSpace(string(out))
	if sha == "" {
		return "", false
	}
	return sha, true
}

// resolveGitDir returns the absolute path to this worktree's gitdir
// (the per-worktree directory, not the main repo's .git when this is a
// linked worktree). Returns "" if it can't be resolved.
func (wc *WorktreeContext) resolveGitDir() string {
	out, err := exec.Command("git", "-C", wc.Dir, "rev-parse", "--git-dir").Output()
	if err != nil {
		return ""
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return ""
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(wc.Dir, dir)
	}
	return dir
}

// FileLog returns `git log --all --pretty=fuller -n <limit> -- <path>` output.
// Used to enrich conflict-resolution context with the file's recent history.
func (wc *WorktreeContext) FileLog(path string, limit int) (string, error) {
	if limit <= 0 {
		limit = 20
	}
	out, err := exec.Command("git", "-C", wc.Dir, "log", "--all",
		"--pretty=fuller", fmt.Sprintf("-n%d", limit), "--", path).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git log -- %s: %w", path, err)
	}
	return string(out), nil
}

// CommitMetadata describes a single commit. Empty fields indicate lookup failure.
type CommitMetadata struct {
	SHA     string
	Subject string
	Author  string
	Date    string
	Body    string
}

// ShowCommit returns metadata for the given commit SHA. Nil when the SHA
// cannot be resolved (e.g. root commit, garbage-collected).
func (wc *WorktreeContext) ShowCommit(sha string) (*CommitMetadata, error) {
	if sha == "" {
		return nil, fmt.Errorf("empty sha")
	}
	// Format: SHA%nSubject%nAuthor%nDate%n%nBody — separator chosen because
	// pretty format strings don't offer an unambiguous record separator.
	// We parse by splitting on "\n" — subject is a single line by git convention.
	out, err := exec.Command("git", "-C", wc.Dir, "show", "--no-patch",
		"--format=%H%n%s%n%an <%ae>%n%aI%n%b", sha).Output()
	if err != nil {
		return nil, fmt.Errorf("git show %s: %w", sha, err)
	}
	lines := strings.SplitN(strings.TrimRight(string(out), "\n"), "\n", 5)
	md := &CommitMetadata{}
	if len(lines) > 0 {
		md.SHA = lines[0]
	}
	if len(lines) > 1 {
		md.Subject = lines[1]
	}
	if len(lines) > 2 {
		md.Author = lines[2]
	}
	if len(lines) > 3 {
		md.Date = lines[3]
	}
	if len(lines) > 4 {
		md.Body = lines[4]
	}
	return md, nil
}

// DiffCheck runs `git diff --check` to detect conflict markers in tracked files.
// A non-nil error means markers (or other whitespace issues) are present.
// Returns combined output regardless.
func (wc *WorktreeContext) DiffCheck() (string, error) {
	out, err := exec.Command("git", "-C", wc.Dir, "diff", "--check").CombinedOutput()
	return string(out), err
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func readFileTrim(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return "", false
	}
	return s, true
}
