package git

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// BranchDiagnostics captures ahead/behind status and recent commit info
// for a branch relative to the repository's default branch.
type BranchDiagnostics struct {
	AheadOfMain    int    // commits on branch not on main
	BehindMain     int    // commits on main not on branch
	MainRef        string // resolved default branch name ("main" or "master")
	BranchRef      string // the branch that was diagnosed
	LastCommitHash string // HEAD SHA of the branch
	LastCommitMsg  string // subject line of the HEAD commit
	Diverged       bool   // true when both AheadOfMain > 0 and BehindMain > 0
}

// WorktreeDiagnostics captures the state of a worktree identified by a
// caller-provided identifier (typically matched against branch names or paths).
type WorktreeDiagnostics struct {
	Exists          bool     // whether a matching worktree was found
	Path            string   // absolute path to the worktree
	IsDirty         bool     // true if working tree has uncommitted changes
	UntrackedFiles  []string // untracked file paths (from git status --porcelain)
	ConflictedFiles []string // files with unresolved merge conflicts (git diff --diff-filter=U)
	Branch          string   // branch checked out in the worktree
	HeadHash        string   // HEAD SHA of the worktree
}

// DiagnoseBranch computes ahead/behind counts for branch relative to the
// repository's default branch (tries "main" then "master"). It runs
// git rev-list --left-right --count to determine divergence.
func DiagnoseBranch(repoPath string, branch string) (*BranchDiagnostics, error) {
	mainRef, err := resolveDefaultBranch(repoPath)
	if err != nil {
		return nil, fmt.Errorf("resolve default branch: %w", err)
	}

	// Verify the target branch exists.
	if err := gitVerifyRef(repoPath, branch); err != nil {
		return nil, fmt.Errorf("branch %q not found: %w", branch, err)
	}

	diag := &BranchDiagnostics{
		MainRef:   mainRef,
		BranchRef: branch,
	}

	// rev-list --left-right --count main...branch
	// Output: "<left>\t<right>\n"
	// left = commits in main not in branch (BehindMain)
	// right = commits in branch not in main (AheadOfMain)
	out, err := gitCmd(repoPath, "rev-list", "--left-right", "--count", mainRef+"..."+branch).Output()
	if err != nil {
		return nil, fmt.Errorf("git rev-list --left-right --count: %w", err)
	}
	parts := strings.Fields(strings.TrimSpace(string(out)))
	if len(parts) == 2 {
		fmt.Sscanf(parts[0], "%d", &diag.BehindMain)
		fmt.Sscanf(parts[1], "%d", &diag.AheadOfMain)
	}
	diag.Diverged = diag.AheadOfMain > 0 && diag.BehindMain > 0

	// Last commit hash and message.
	hashOut, err := gitCmd(repoPath, "rev-parse", branch).Output()
	if err == nil {
		diag.LastCommitHash = strings.TrimSpace(string(hashOut))
	}
	msgOut, err := gitCmd(repoPath, "log", "-1", "--format=%s", branch).Output()
	if err == nil {
		diag.LastCommitMsg = strings.TrimSpace(string(msgOut))
	}

	return diag, nil
}

// DiagnoseWorktree checks if a worktree matching the given identifier exists
// by scanning git worktree list output for paths or branches containing the
// identifier. If found, it inspects dirty state, untracked files, branch,
// and HEAD.
func DiagnoseWorktree(repoPath string, beadID string) (*WorktreeDiagnostics, error) {
	diag := &WorktreeDiagnostics{}

	// Parse git worktree list output to find a matching worktree.
	out, err := gitCmd(repoPath, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return nil, fmt.Errorf("git worktree list: %w", err)
	}

	wtPath, wtBranch := findWorktreeByID(string(out), beadID)
	if wtPath == "" {
		return diag, nil // Exists=false
	}

	diag.Exists = true
	diag.Path = wtPath
	diag.Branch = wtBranch

	// HEAD hash.
	headOut, err := exec.Command("git", "-C", wtPath, "rev-parse", "HEAD").Output()
	if err == nil {
		diag.HeadHash = strings.TrimSpace(string(headOut))
	}

	// Dirty state and untracked files via git status --porcelain.
	statusOut, _ := exec.Command("git", "-C", wtPath, "status", "--porcelain").Output()
	statusStr := strings.TrimSpace(string(statusOut))
	if statusStr != "" {
		diag.IsDirty = true
		for _, line := range strings.Split(statusStr, "\n") {
			if len(line) >= 3 && line[0] == '?' && line[1] == '?' {
				diag.UntrackedFiles = append(diag.UntrackedFiles, strings.TrimSpace(line[3:]))
			}
		}
	}

	// Unresolved conflict list — filter U, via git diff --name-only --diff-filter=U.
	// Empty when no conflict is in progress. Errors are informational.
	if conflictOut, err := exec.Command("git", "-C", wtPath, "diff", "--name-only", "--diff-filter=U").Output(); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(conflictOut)), "\n") {
			if line != "" {
				diag.ConflictedFiles = append(diag.ConflictedFiles, line)
			}
		}
	}

	return diag, nil
}

// CollectStepOutput looks for build/test output files in .spire/ or .beads/
// subdirectories of the given worktree path. It searches for files matching
// common naming patterns for the given step name. Returns the file content
// truncated to 4KB, or an empty string if no output file is found.
func CollectStepOutput(worktreePath string, step string) (string, error) {
	// Candidate file names for this step.
	candidates := []string{
		step + ".log",
		step + "-output.log",
	}

	// Candidate directories.
	dirs := []string{
		filepath.Join(worktreePath, ".spire"),
		filepath.Join(worktreePath, ".beads"),
	}

	for _, dir := range dirs {
		for _, name := range candidates {
			path := filepath.Join(dir, name)
			data, err := os.ReadFile(path)
			if err != nil {
				continue // file doesn't exist or can't be read
			}
			content := string(data)
			const maxSize = 4096
			if len(content) > maxSize {
				content = content[:maxSize]
			}
			return content, nil
		}
	}

	return "", nil
}

// resolveDefaultBranch returns "main" or "master", whichever exists as a
// valid ref in the given repository.
func resolveDefaultBranch(repoPath string) (string, error) {
	if err := gitVerifyRef(repoPath, "main"); err == nil {
		return "main", nil
	}
	if err := gitVerifyRef(repoPath, "master"); err == nil {
		return "master", nil
	}
	return "", fmt.Errorf("neither 'main' nor 'master' branch found in %s", repoPath)
}

// gitVerifyRef verifies that a ref exists in the given repository.
func gitVerifyRef(repoPath string, ref string) error {
	return gitCmd(repoPath, "rev-parse", "--verify", ref).Run()
}

// gitCmd builds an exec.Cmd for a git command rooted at the given directory.
func gitCmd(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd
}

// findWorktreeByID parses porcelain worktree list output and returns the path
// and branch of the first worktree whose path or branch contains the given
// identifier. Returns ("", "") if no match is found.
//
// Porcelain format (blocks separated by blank lines):
//
//	worktree /path/to/wt
//	HEAD abc123
//	branch refs/heads/feat/spi-abc12
//
//	worktree /path/to/main
//	HEAD def456
//	branch refs/heads/main
func findWorktreeByID(porcelainOutput string, id string) (path string, branch string) {
	blocks := splitWorktreeBlocks(porcelainOutput)
	for _, block := range blocks {
		wtPath := ""
		wtBranch := ""
		for _, line := range block {
			if strings.HasPrefix(line, "worktree ") {
				wtPath = strings.TrimPrefix(line, "worktree ")
			}
			if strings.HasPrefix(line, "branch ") {
				ref := strings.TrimPrefix(line, "branch ")
				// Strip refs/heads/ prefix to get the short branch name.
				wtBranch = strings.TrimPrefix(ref, "refs/heads/")
			}
		}
		if wtPath == "" {
			continue
		}
		// Match against path or branch containing the identifier.
		if strings.Contains(wtPath, id) || strings.Contains(wtBranch, id) {
			return wtPath, wtBranch
		}
	}
	return "", ""
}

// splitWorktreeBlocks splits porcelain worktree list output into blocks,
// where each block is a slice of non-empty lines for one worktree entry.
func splitWorktreeBlocks(output string) [][]string {
	var blocks [][]string
	var current []string
	for _, line := range strings.Split(output, "\n") {
		if line == "" {
			if len(current) > 0 {
				blocks = append(blocks, current)
				current = nil
			}
			continue
		}
		current = append(current, line)
	}
	if len(current) > 0 {
		blocks = append(blocks, current)
	}
	return blocks
}
