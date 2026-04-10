package executor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	spgit "github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/repoconfig"
)

// reviewDocsForStaleness checks documentation files modified on the staging branch
// for stale language and fixes them.
func (e *Executor) reviewDocsForStaleness(repoPath, branch, baseBranch string, docPatterns []string, model string) error {
	wc := &spgit.WorktreeContext{Dir: repoPath}
	changedFiles, err := wc.DiffNameOnly(baseBranch)
	if err != nil {
		return fmt.Errorf("diff --name-only: %w", err)
	}

	// If no doc patterns configured, skip doc review entirely.
	if len(docPatterns) == 0 {
		e.log("no doc_patterns configured — skipping doc review")
		return nil
	}

	// Filter for documentation files matching configured patterns.
	var docFiles []string
	for _, f := range changedFiles {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if matchesDocPatterns(f, docPatterns) {
			docFiles = append(docFiles, f)
		}
	}

	if len(docFiles) == 0 {
		e.log("no documentation files changed — skipping doc review")
		return nil
	}

	e.log("reviewing %d documentation file(s) for stale language: %s", len(docFiles), strings.Join(docFiles, ", "))

	prompt := fmt.Sprintf(`You are reviewing documentation files after code branches have been merged into a staging branch. Parallel workers wrote these docs against pre-merge code. Some docs may say "planned", "TODO", "not yet implemented", "will be added", "coming soon", or similar language for features that NOW EXIST in the merged code.

Your job:
1. Read each documentation file listed below.
2. For each file, check if it contains stale language — phrases like "planned", "TODO", "not yet implemented", "will be", "coming soon", "future work", "not yet supported" — that refers to functionality that is NOW present in the codebase.
3. To determine what is actually implemented, look at the actual source code files (not just docs).
4. If you find stale language, fix it to reflect the current state of the code. Change "will be implemented" to "is implemented", remove "TODO" items that are done, etc.
5. If no fixes are needed, do nothing — do NOT make unnecessary changes.
6. If you made any changes, stage them with git add and commit with the message: docs: fix stale documentation after merge

Documentation files to review:
%s

IMPORTANT: Only fix genuinely stale language where the described feature now exists in code. Do NOT remove TODOs for things that are actually still pending. Be conservative — when in doubt, leave it alone.`, strings.Join(docFiles, "\n"))

	resolvedModel := repoconfig.ResolveModel(model, e.repoModel())

	cmd := exec.Command("claude",
		"--dangerously-skip-permissions",
		"-p", prompt,
		"--model", resolvedModel,
		"--output-format", "text",
	)
	cmd.Dir = repoPath
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("claude doc review: %w", err)
	}

	e.log("documentation review complete")
	return nil
}

// matchesDocPatterns returns true if path matches any of the given glob patterns.
func matchesDocPatterns(path string, patterns []string) bool {
	for _, p := range patterns {
		if matched, _ := filepath.Match(p, path); matched {
			return true
		}
		// Also match against the base filename for bare patterns like "README.md".
		if !strings.Contains(p, "/") {
			if matched, _ := filepath.Match(p, filepath.Base(path)); matched {
				return true
			}
		}
	}
	return false
}
