package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	spgit "github.com/awell-health/spire/pkg/git"
)

// verifyResolvedRepo is the package-level hook executed from
// resolveGraphBranchState. Replaced by tests that need to bypass the
// filesystem/git checks (e.g. because their fake ResolveRepo returns a
// non-existent path). Production callers should not reassign this; use
// verifyResolvedRepoReal directly if a second check is ever needed.
var verifyResolvedRepo = verifyResolvedRepoReal

// verifyResolvedRepoReal asserts that repoPath is a usable git repo for
// the given prefix. It checks three things before any role hand-off:
//
//  1. The path exists on disk.
//  2. It's a git repository (either has a .git entry or
//     `git -C repoPath rev-parse --git-dir` succeeds).
//  3. Its origin remote URL, if an expected URL is known, matches the
//     prefix's registered remote after normalization (SSH ↔ HTTPS, .git
//     suffix, trailing slash).
//
// When expectedURL is empty (e.g. the repos table didn't have a row for
// the prefix yet), the remote-URL check is skipped — path existence
// and git-ness are still enforced.
//
// See spi-rpuzs6 for why this guard exists: the executor used to accept
// any CWD as "the repo" and commit to it, causing silent cross-repo
// corruption. The pre-flight and bridge guards backstop this, but the
// executor has the final say since it's the one spawning roles.
func verifyResolvedRepoReal(repoPath, prefix, expectedURL string) error {
	if repoPath == "" {
		return fmt.Errorf("repo path is empty")
	}

	info, err := os.Stat(repoPath)
	if err != nil {
		return fmt.Errorf("repo path %q does not exist: %w", repoPath, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("repo path %q is not a directory", repoPath)
	}

	if !isGitRepo(repoPath) {
		return fmt.Errorf("repo path %q is not a git repository", repoPath)
	}

	if expectedURL == "" {
		return nil
	}

	rc := &spgit.RepoContext{Dir: repoPath}
	actual := rc.RemoteURL("origin")
	if actual == "" {
		// Origin not configured yet (bare checkout, fresh worktree) —
		// don't block on it; the path/git check above is sufficient.
		return nil
	}
	if !remoteURLsEquivalent(actual, expectedURL) {
		hint := ""
		if prefix != "" {
			hint = fmt.Sprintf(" (prefix %q)", prefix)
		}
		return fmt.Errorf("repo path %q origin=%q does not match registered remote %q%s — the prefix is bound to a different local checkout", repoPath, actual, expectedURL, hint)
	}
	return nil
}

// isGitRepo reports whether repoPath is a git repository. Accepts both
// standard repos (a .git directory) and git worktrees (a .git file
// pointing at a gitdir). Does not shell out to `git rev-parse` to keep
// the check cheap and deterministic for tests.
func isGitRepo(repoPath string) bool {
	_, err := os.Stat(filepath.Join(repoPath, ".git"))
	return err == nil
}

// remoteURLsEquivalent reports whether two git remote URLs refer to the
// same repo. Normalizes SSH (`git@host:org/repo[.git]`) and HTTPS
// (`https://host/org/repo[.git]`) forms, strips the `.git` suffix, and
// ignores a single trailing slash. Returns true for empty pairs of the
// same form so legacy callers don't false-fail.
func remoteURLsEquivalent(a, b string) bool {
	return normalizeRemoteURL(a) == normalizeRemoteURL(b)
}

// normalizeRemoteURL reduces a git remote URL to a canonical
// host/org/repo shape for comparison. It intentionally does not validate
// — any unknown form is returned with only trimming and .git-suffix
// removal applied. Case-insensitive: protocol prefixes like HTTPS://
// and uppercase host/path segments are all lowercased first so human
// keystroke variance doesn't confuse the comparison.
func normalizeRemoteURL(u string) string {
	s := strings.ToLower(strings.TrimSpace(u))
	if s == "" {
		return ""
	}

	// SSH form: git@github.com:awell-health/spire.git
	if strings.HasPrefix(s, "git@") {
		if colon := strings.Index(s, ":"); colon > 0 {
			host := s[len("git@"):colon]
			path := s[colon+1:]
			s = host + "/" + path
		}
	} else {
		// HTTPS / SSH-URL form
		for _, pre := range []string{"https://", "http://", "ssh://git@", "ssh://", "git://"} {
			if strings.HasPrefix(s, pre) {
				s = strings.TrimPrefix(s, pre)
				break
			}
		}
	}

	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	return s
}

// extractPrefix returns the prefix portion of a bead ID (characters
// before the first "-") or "" if the ID has no separator.
func extractPrefix(beadID string) string {
	if idx := strings.Index(beadID, "-"); idx > 0 {
		return beadID[:idx]
	}
	return ""
}
