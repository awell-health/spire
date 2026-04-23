package executor

// principle1_audit_test.go — TestPrinciple1_NoBorrowedForCommitPaths.
//
// Spec (spi-tlj32a / spi-1dk71j Principle 1): any dispatch path that produces
// new commits must not use HandoffBorrowed. The fix step and the worker-mode
// cleric repair were the two known offenders before this bead landed; this
// test pins the rule so a regression that re-introduces a borrowed-handoff
// dispatch on a commit-producing path fails CI loudly.
//
// Mechanism: scan every .go source file in pkg/executor and pkg/wizard and
// look for the literal string `HandoffBorrowed` near a known commit-producing
// dispatch site (review-fix, fix, cleric-worker, repair-worker). Any
// occurrence not annotated with the explicit comment marker
// `// principle1-exception:` fails the test.
//
// The exception marker is intentionally narrow: the only legitimate uses of
// HandoffBorrowed are non-commit-producing reads (sage-review, narrow-check
// recovery-verify) where the runtime contract is "no delivery needed" rather
// than "deliver via bundle." Those uses are NOT in fix-adjacent call sites,
// so they don't trip the test.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// commitProducingTags are the substrings the audit treats as evidence that
// a nearby HandoffBorrowed reference is on a commit-producing dispatch path.
// The full list is intentional — adding a new commit-producing dispatch
// path is rare enough that updating this slice is the right place to gate
// the addition.
var commitProducingTags = []string{
	"review-fix",
	"reviewFix",
	"actionReviewFix",
	"cleric-worker",
	"clericWorker",
	"DispatchClericWorkerApprentice",
	"commitProducingApprentice",
	"dispatchCommitProducingApprentice",
}

// principle1ExceptionMarker is the only documented exception comment that
// silences the audit. Any HandoffBorrowed line on a commit-producing path
// must carry this marker plus a justification.
const principle1ExceptionMarker = "principle1-exception:"

func TestPrinciple1_NoBorrowedForCommitPaths(t *testing.T) {
	// Resolve repo root from this test file so the audit walks the live tree
	// rather than a snapshot.
	roots := []string{
		mustResolvePackageDir(t, "executor"),
		mustResolvePackageDir(t, "wizard"),
	}

	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if info.IsDir() {
				// Skip nested testdata trees if any exist.
				if info.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			// Test files often assert "must NOT equal HandoffBorrowed" or
			// reference both literals to compare modes — those are part of
			// the enforcement, not violations of it. The audit's scope is
			// runtime code, so _test.go files are excluded wholesale.
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			body := string(data)
			lines := strings.Split(body, "\n")
			for i, line := range lines {
				if !strings.Contains(line, "HandoffBorrowed") {
					continue
				}
				// Pure-comment mentions don't introduce a runtime
				// HandoffBorrowed value — they explain the rule. The audit
				// only fires on lines that USE the symbol.
				if isCommentOnly(line) {
					continue
				}
				// Look at a context window of 6 lines before and after the
				// HandoffBorrowed line: a function-scope reference is
				// usually within a handful of lines of the dispatch tag.
				windowStart := i - 6
				if windowStart < 0 {
					windowStart = 0
				}
				windowEnd := i + 6
				if windowEnd >= len(lines) {
					windowEnd = len(lines) - 1
				}
				window := strings.Join(lines[windowStart:windowEnd+1], "\n")

				if !mentionsCommitProducingTag(window) {
					continue
				}
				if strings.Contains(window, principle1ExceptionMarker) {
					continue
				}
				t.Errorf("%s:%d: HandoffBorrowed appears near a commit-producing tag without a `// principle1-exception:` justification.\nLine: %s", path, i+1, strings.TrimSpace(line))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
}

// mentionsCommitProducingTag reports whether the source window contains any
// known commit-producing dispatch tag.
func mentionsCommitProducingTag(window string) bool {
	for _, tag := range commitProducingTags {
		if strings.Contains(window, tag) {
			return true
		}
	}
	return false
}

// isCommentOnly reports whether a source line carries only a Go comment.
// HandoffBorrowed inside a // doc line is allowed to appear next to a
// commit-producing tag because it documents the rule rather than enforcing
// the wrong handoff at runtime.
func isCommentOnly(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "//")
}

// mustResolvePackageDir locates the source directory of a sibling package
// inside pkg/. The audit walks live source rather than reflecting on
// compiled symbols so the rule covers comments, strings, and unreachable
// code paths that the compiler optimizes out.
func mustResolvePackageDir(t *testing.T, pkgName string) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// Tests run from pkg/executor; pkg/<pkgName> is a sibling.
	dir := filepath.Join(filepath.Dir(wd), pkgName)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("locate package dir %s (resolved %q): %v", pkgName, dir, err)
	}
	return dir
}
