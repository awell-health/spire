package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// gitFetch fetches all remote branches and prunes stale tracking refs.
func gitFetch(dir string) error {
	return gitCmd(dir, "fetch", "origin", "--prune")
}

// branchExists returns true if the given branch exists on the remote.
func branchExists(dir, branch string) bool {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--verify", "origin/"+branch)
	return cmd.Run() == nil
}

// getHeadCommit returns the commit SHA at the tip of the given remote branch.
func getHeadCommit(dir, branch string) (string, error) {
	return gitOutput(dir, "rev-parse", "origin/"+branch)
}

// gitDiff returns the three-dot diff between base and branch (on the remote).
func gitDiff(dir, base, branch string) (string, error) {
	return gitOutput(dir, "diff", "origin/"+base+"...origin/"+branch)
}

// gitDiffStats returns file count, lines added, and lines removed.
func gitDiffStats(dir, base, branch string) (filesChanged, linesAdded, linesRemoved int, err error) {
	out, err := gitOutput(dir, "diff", "--numstat", "origin/"+base+"...origin/"+branch)
	if err != nil {
		return 0, 0, 0, err
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}
		filesChanged++
		if a, e := strconv.Atoi(parts[0]); e == nil {
			linesAdded += a
		}
		if r, e := strconv.Atoi(parts[1]); e == nil {
			linesRemoved += r
		}
	}
	return filesChanged, linesAdded, linesRemoved, nil
}

// gitCheckout checks out a branch and resets to its remote tracking state.
func gitCheckout(dir, branch string) error {
	if err := gitCmd(dir, "checkout", branch); err != nil {
		// Branch may not exist locally yet — create from remote.
		if err2 := gitCmd(dir, "checkout", "-B", branch, "origin/"+branch); err2 != nil {
			return fmt.Errorf("checkout %s: %w", branch, err2)
		}
	}
	return gitCmd(dir, "reset", "--hard", "origin/"+branch)
}

// gitCheckoutBase checks out the base branch and pulls latest.
func gitCheckoutBase(dir, base string) error {
	if err := gitCmd(dir, "checkout", base); err != nil {
		return err
	}
	return gitCmd(dir, "reset", "--hard", "origin/"+base)
}

// gitMerge performs a no-ff merge of the given remote branch into the current branch.
func gitMerge(dir, branch, message string) error {
	return gitCmd(dir, "merge", "origin/"+branch, "--no-ff", "-m", message)
}

// gitRevertMerge aborts a merge or resets to before the last merge commit.
func gitRevertMerge(dir string) error {
	// Try merge --abort first (works if merge is in progress).
	if err := gitCmd(dir, "merge", "--abort"); err == nil {
		return nil
	}
	// Otherwise reset to before the merge commit.
	return gitCmd(dir, "reset", "--hard", "HEAD~1")
}

// gitPush pushes the given branch to origin.
func gitPush(dir, branch string) error {
	return gitCmd(dir, "push", "origin", branch)
}

// gitClone clones a repo with depth=1.
func gitClone(url, branch, dir string) error {
	cmd := exec.Command("git", "clone", "--depth=1", "--branch", branch, url, dir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone: %w\n%s", err, stderr.String())
	}
	return nil
}

// gitCmd runs a git command in the given directory and returns any error.
func gitCmd(dir string, args ...string) error {
	full := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return nil
}

// gitOutput runs a git command and returns its trimmed stdout.
func gitOutput(dir string, args ...string) (string, error) {
	full := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}
