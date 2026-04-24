package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/formula"
)

// TestResolveGraphBranchState_EmptyRepoPathErrors is the layer-3 guard
// for the silent-fallback chain documented in spi-rpuzs6. When the
// resolver hands back an empty repoPath the executor must refuse to
// continue — the old behavior of silently substituting "." leaked all
// subsequent git ops into the process CWD (typically the tower's home
// repo). Regression test.
func TestResolveGraphBranchState_EmptyRepoPathErrors(t *testing.T) {
	deps, _ := testGraphDeps(t)

	// ResolveRepo reports success but returns no path — exactly the
	// shape that bubbled through from a bridge that had swallowed the
	// underlying "no local repo registered" error.
	deps.ResolveRepo = func(beadID string) (string, string, string, error) {
		return "", "", "main", nil
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-empty-repo",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"a": {Action: "test.noop", Terminal: true},
		},
	}

	exec := NewGraphForTest("spd-1jd", "wizard-spd", graph, nil, deps)
	err := exec.resolveGraphBranchState(graph, exec.graphState)
	if err == nil {
		t.Fatal("resolveGraphBranchState with empty repoPath = nil error, want a repo-resolution error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "unresolved") && !strings.Contains(msg, "repo path") {
		t.Errorf("error %q should say the repo path is unresolved", err)
	}
	if !strings.Contains(msg, "spd-1jd") {
		t.Errorf("error %q should name the bead", err)
	}
}

// TestResolveGraphBranchState_ResolveError documents that a direct
// error from the resolver is propagated up verbatim — the bridge no
// longer swallows "no local repo registered" (spi-rpuzs6 layer 2).
func TestResolveGraphBranchState_ResolveError(t *testing.T) {
	deps, _ := testGraphDeps(t)

	deps.ResolveRepo = func(beadID string) (string, string, string, error) {
		return "", "", "", errForTest("no local repo registered for prefix \"spd\"")
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-resolver-error",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"a": {Action: "test.noop", Terminal: true},
		},
	}

	exec := NewGraphForTest("spd-abc", "wizard-spd-abc", graph, nil, deps)
	err := exec.resolveGraphBranchState(graph, exec.graphState)
	if err == nil {
		t.Fatal("resolveGraphBranchState with resolver error = nil, want propagated error")
	}
	if !strings.Contains(err.Error(), "no local repo registered") {
		t.Errorf("error %q should preserve the underlying resolver error", err)
	}
}

// --- remoteURLsEquivalent / normalizeRemoteURL ---

func TestRemoteURLsEquivalent(t *testing.T) {
	cases := []struct {
		name  string
		a, b  string
		equal bool
	}{
		{"identical ssh", "git@github.com:foo/bar.git", "git@github.com:foo/bar.git", true},
		{"ssh vs https", "git@github.com:foo/bar.git", "https://github.com/foo/bar.git", true},
		{"ssh vs https no git suffix", "git@github.com:foo/bar.git", "https://github.com/foo/bar", true},
		{"trailing slash tolerance", "https://github.com/foo/bar/", "git@github.com:foo/bar", true},
		{"case differences", "HTTPS://GitHub.com/Foo/Bar", "git@github.com:foo/bar", true},
		{"different org", "git@github.com:foo/bar.git", "git@github.com:baz/bar.git", false},
		{"different repo", "git@github.com:foo/bar.git", "git@github.com:foo/qux.git", false},
		{"different host", "git@github.com:foo/bar.git", "git@gitlab.com:foo/bar.git", false},
		{"empty pair", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := remoteURLsEquivalent(c.a, c.b); got != c.equal {
				t.Errorf("remoteURLsEquivalent(%q, %q) = %v, want %v", c.a, c.b, got, c.equal)
			}
		})
	}
}

// --- verifyResolvedRepoReal ---

func TestVerifyResolvedRepoReal_PathMissing(t *testing.T) {
	err := verifyResolvedRepoReal("/definitely/does/not/exist", "spd", "")
	if err == nil {
		t.Fatal("expected error for missing path")
	}
}

func TestVerifyResolvedRepoReal_NotAGitRepo(t *testing.T) {
	dir := t.TempDir()
	err := verifyResolvedRepoReal(dir, "spd", "")
	if err == nil {
		t.Fatal("expected error for non-git directory")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("error %q should mention git repository", err)
	}
}

func TestVerifyResolvedRepoReal_GitRepoNoURL(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	// Empty expectedURL → path + git-ness are enough.
	if err := verifyResolvedRepoReal(dir, "spd", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func errForTest(msg string) error { return &testError{msg: msg} }

type testError struct{ msg string }

func (e *testError) Error() string { return e.msg }
