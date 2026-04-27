package store

import (
	"errors"
	"testing"
)

// TestParseGrepShas verifies the multiline parser tolerates blank lines,
// invalid hex tokens, and short SHAs while collecting valid hex SHAs.
func TestParseGrepShas(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"whitespace_only", "   \n\n  ", nil},
		{"single_line", "abc1234 feat(spi-x): foo\n", []string{"abc1234"}},
		{
			"multi_line",
			"abc1234 feat(spi-x): foo\ndef5678 fix(spi-x): bar\n",
			[]string{"abc1234", "def5678"},
		},
		{
			"strips_garbage",
			"abc1234 ok\nNOTHEX bad\n9999999 also-ok\n",
			[]string{"abc1234", "9999999"},
		},
		{
			"too_short_filtered",
			"abc fast\nabcdef ok\n",
			[]string{"abcdef"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseGrepShas(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i, sha := range got {
				if sha != tt.want[i] {
					t.Errorf("got[%d] = %q, want %q", i, sha, tt.want[i])
				}
			}
		})
	}
}

// TestRunCloseSweep_PrefixUnbound exercises the silent no-op path when
// no repo is registered for a bead's prefix. The lookup function returns
// "" → the sweep skips git invocation entirely.
func TestRunCloseSweep_PrefixUnbound(t *testing.T) {
	origLookup := repoPathLookupFunc
	repoPathLookupFunc = func(string) string { return "" }
	t.Cleanup(func() { repoPathLookupFunc = origLookup })

	gitCalled := false
	origCmd := closeSweepCommand
	closeSweepCommand = func(repoPath, beadID string) (string, error) {
		gitCalled = true
		return "", nil
	}
	t.Cleanup(func() { closeSweepCommand = origCmd })

	runCloseSweep("spi-x", "spi")

	if gitCalled {
		t.Error("git should not be invoked when prefix is unbound")
	}
}

// TestRunCloseSweep_GitFailureSwallowed: when `git log --grep` errors
// (no repo, malformed args, network FS unavailable), the sweep logs
// and returns silently — never abort.
func TestRunCloseSweep_GitFailureSwallowed(t *testing.T) {
	origLookup := repoPathLookupFunc
	repoPathLookupFunc = func(string) string { return "/some/path" }
	t.Cleanup(func() { repoPathLookupFunc = origLookup })

	origCheck := ensureRepoOK
	ensureRepoOK = func(string) error { return nil }
	t.Cleanup(func() { ensureRepoOK = origCheck })

	origCmd := closeSweepCommand
	closeSweepCommand = func(string, string) (string, error) {
		return "", errors.New("simulated git failure")
	}
	t.Cleanup(func() { closeSweepCommand = origCmd })

	// Should not panic / not raise.
	runCloseSweep("spi-x", "spi")
}

// TestRunCloseSweep_NotAGitRepoSkipped: when the registered repo path
// isn't actually a git work tree (mis-registered tower path), the sweep
// stops before the git log invocation rather than letting git print
// "fatal: not a git repository" into the daemon log on every close.
func TestRunCloseSweep_NotAGitRepoSkipped(t *testing.T) {
	origLookup := repoPathLookupFunc
	repoPathLookupFunc = func(string) string { return "/not-a-repo" }
	t.Cleanup(func() { repoPathLookupFunc = origLookup })

	origCheck := ensureRepoOK
	ensureRepoOK = func(string) error { return errors.New("not a git work tree") }
	t.Cleanup(func() { ensureRepoOK = origCheck })

	gitCalled := false
	origCmd := closeSweepCommand
	closeSweepCommand = func(string, string) (string, error) {
		gitCalled = true
		return "", nil
	}
	t.Cleanup(func() { closeSweepCommand = origCmd })

	runCloseSweep("spi-x", "spi")

	if gitCalled {
		t.Error("git log should not be invoked when ensureRepoOK fails")
	}
}

// TestFirePostCloseSweepIfTransitioned_SkipsReClose: when prior status
// was already "closed", the guard prevents the sweep from firing on
// repeat updates / replays / admin re-closes.
func TestFirePostCloseSweepIfTransitioned_SkipsReClose(t *testing.T) {
	origLookup := repoPathLookupFunc
	repoPathLookupFunc = func(string) string {
		t.Error("repoPathLookupFunc must not be invoked on prior=closed")
		return ""
	}
	t.Cleanup(func() { repoPathLookupFunc = origLookup })

	firePostCloseSweepIfTransitioned("spi-x", "closed")
	WaitCloseSweeps()
}

// TestFirePostCloseSweepIfTransitioned_FiresOnRealTransition: when prior
// status was anything other than "closed", the sweep does fire (and we
// see git get invoked).
func TestFirePostCloseSweepIfTransitioned_FiresOnRealTransition(t *testing.T) {
	origLookup := repoPathLookupFunc
	repoPathLookupFunc = func(string) string { return "/repo" }
	t.Cleanup(func() { repoPathLookupFunc = origLookup })

	origCheck := ensureRepoOK
	ensureRepoOK = func(string) error { return nil }
	t.Cleanup(func() { ensureRepoOK = origCheck })

	called := false
	origCmd := closeSweepCommand
	closeSweepCommand = func(string, string) (string, error) {
		called = true
		return "", nil // no commits found, but the call happened
	}
	t.Cleanup(func() { closeSweepCommand = origCmd })

	firePostCloseSweepIfTransitioned("spi-x", "in_progress")
	WaitCloseSweeps()

	if !called {
		t.Error("expected git invocation on prior=in_progress → closed transition")
	}
}

// TestRunCloseSweep_AppendsNovelShas: end-to-end happy path through
// runCloseSweep — prefix bound, git returns SHAs, the appender seam
// records each. We swap closeSweepAppendCommit directly so the test
// doesn't have to go through the dispatched-store metadata read path.
func TestRunCloseSweep_AppendsNovelShas(t *testing.T) {
	origLookup := repoPathLookupFunc
	repoPathLookupFunc = func(string) string { return "/repo" }
	t.Cleanup(func() { repoPathLookupFunc = origLookup })

	origCheck := ensureRepoOK
	ensureRepoOK = func(string) error { return nil }
	t.Cleanup(func() { ensureRepoOK = origCheck })

	origCmd := closeSweepCommand
	closeSweepCommand = func(string, string) (string, error) {
		return "abc1234 feat(spi-x): land\ndef5678 fix(spi-x): patch\n", nil
	}
	t.Cleanup(func() { closeSweepCommand = origCmd })

	type appendCall struct {
		ID    string
		Key   string
		Value string
	}
	var appended []appendCall
	origAppend := closeSweepAppendCommit
	closeSweepAppendCommit = func(id, key, value string) error {
		appended = append(appended, appendCall{ID: id, Key: key, Value: value})
		return nil
	}
	t.Cleanup(func() { closeSweepAppendCommit = origAppend })

	runCloseSweep("spi-x", "spi")

	if len(appended) != 2 {
		t.Fatalf("expected 2 metadata appends (one per SHA), got %d: %+v", len(appended), appended)
	}
	if appended[0].Value != "abc1234" || appended[1].Value != "def5678" {
		t.Errorf("appends out of order: %+v", appended)
	}
	for i, c := range appended {
		if c.ID != "spi-x" {
			t.Errorf("append[%d].ID = %q, want spi-x", i, c.ID)
		}
		if c.Key != "commits" {
			t.Errorf("append[%d].Key = %q, want commits", i, c.Key)
		}
	}
}
