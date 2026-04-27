package store

import (
	"strings"
	"testing"
)

// fakeLookupRunner is a hermetic stand-in for the realLookupRunner.
// Tests construct one with the exact strings/state they want git to
// "return" so the LookupBeadCommits paths can be exercised without a
// real repo.
type fakeLookupRunner struct {
	logGrepOut    string
	logGrepErr    error
	reachable     map[string]bool // sha → "git cat-file -e <sha>^{commit}" result
	subjectByShas map[string]string

	// Captured args for spy assertions.
	gotGrepBeadIDs []string
	gotCatFileShas []string
}

func (f *fakeLookupRunner) LogGrep(repoPath, beadID string) (string, error) {
	f.gotGrepBeadIDs = append(f.gotGrepBeadIDs, beadID)
	return f.logGrepOut, f.logGrepErr
}

func (f *fakeLookupRunner) CatFileExists(repoPath, sha string) bool {
	f.gotCatFileShas = append(f.gotCatFileShas, sha)
	if f.reachable == nil {
		return false
	}
	return f.reachable[sha]
}

func (f *fakeLookupRunner) LogSubject(repoPath, sha string) string {
	if f.subjectByShas == nil {
		return ""
	}
	return f.subjectByShas[sha]
}

// installFakeMetadata installs a deterministic stand-in for the
// GetBeadMetadata function used by LookupBeadCommits. We swap the
// dedicated lookup seam (getBeadMetadataForLookup) so this stays
// independent of instance_meta.go's own seam.
func installFakeMetadata(t *testing.T, m map[string]string) {
	t.Helper()
	orig := getBeadMetadataForLookup
	getBeadMetadataForLookup = func(string) (map[string]string, error) { return m, nil }
	t.Cleanup(func() { getBeadMetadataForLookup = orig })
}

func installFakeRunner(t *testing.T, r *fakeLookupRunner) {
	t.Helper()
	orig := lookupRunnerVar
	lookupRunnerVar = r
	t.Cleanup(func() { lookupRunnerVar = orig })
}

// TestLookupBeadCommits_OnlyMetadata: bead has SHAs in metadata.commits[]
// and grep returns nothing. Returned rows tag Source="metadata", and
// Reachable is sourced from the runner.
func TestLookupBeadCommits_OnlyMetadata(t *testing.T) {
	installFakeMetadata(t, map[string]string{
		"commits": `["abc1234", "def5678"]`,
	})
	r := &fakeLookupRunner{
		reachable:     map[string]bool{"abc1234": true, "def5678": true},
		subjectByShas: map[string]string{"abc1234": "feat(spi-x): foo", "def5678": "fix(spi-x): bar"},
	}
	installFakeRunner(t, r)

	got, err := LookupBeadCommits("spi-x", "/repo")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d refs, want 2", len(got))
	}
	if got[0].SHA != "abc1234" || got[0].Source != "metadata" || !got[0].Reachable {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[0].Subject != "feat(spi-x): foo" {
		t.Errorf("got[0].Subject = %q", got[0].Subject)
	}
	if got[1].SHA != "def5678" || got[1].Source != "metadata" {
		t.Errorf("got[1] = %+v", got[1])
	}
}

// TestLookupBeadCommits_OnlyGrep: bead's metadata.commits[] is empty
// (or absent). Grep returns the only commits. Source="grep" on every row.
func TestLookupBeadCommits_OnlyGrep(t *testing.T) {
	installFakeMetadata(t, nil)
	r := &fakeLookupRunner{
		logGrepOut: "abc1234 feat(spi-x): foo\ndef5678 fix(spi-x): bar\n",
		reachable:  map[string]bool{"abc1234": true, "def5678": true},
	}
	installFakeRunner(t, r)

	got, err := LookupBeadCommits("spi-x", "/repo")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d refs, want 2", len(got))
	}
	for _, c := range got {
		if c.Source != "grep" {
			t.Errorf("expected Source=grep, got %q (sha=%s)", c.Source, c.SHA)
		}
	}
	if got[0].Subject != "feat(spi-x): foo" {
		t.Errorf("got[0].Subject = %q", got[0].Subject)
	}
}

// TestLookupBeadCommits_BothSources: SHA in BOTH metadata and grep
// dedupes — metadata wins. SHAs only in grep are appended after.
func TestLookupBeadCommits_BothSources(t *testing.T) {
	installFakeMetadata(t, map[string]string{
		"commits": `["abc1234"]`,
	})
	r := &fakeLookupRunner{
		logGrepOut: "abc1234 feat(spi-x): foo\nzzzzzzz unrelated\n9999999 fix(spi-x): squash-merged on main\n",
		reachable:  map[string]bool{"abc1234": true, "9999999": true, "zzzzzzz": false},
		subjectByShas: map[string]string{
			"abc1234": "feat(spi-x): foo",
		},
	}
	installFakeRunner(t, r)

	got, err := LookupBeadCommits("spi-x", "/repo")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 2 {
		// abc1234 (metadata) + 9999999 (grep). zzzzzzz is invalid hex
		// → filtered.
		t.Fatalf("got %d refs, want 2 (zzzzzzz should be filtered as non-hex): %+v", len(got), got)
	}
	if got[0].SHA != "abc1234" || got[0].Source != "metadata" {
		t.Errorf("got[0]: SHA=%q Source=%q (want abc1234 / metadata)", got[0].SHA, got[0].Source)
	}
	if got[1].SHA != "9999999" || got[1].Source != "grep" {
		t.Errorf("got[1]: SHA=%q Source=%q (want 9999999 / grep)", got[1].SHA, got[1].Source)
	}
}

// TestLookupBeadCommits_UnreachableMetadataFallsBack: a wizard-recorded
// SHA is no longer reachable on main (squash-merged). Grep still finds
// the squash commit on main. Both rows return; metadata row has
// Reachable=false, grep row has Reachable=true.
func TestLookupBeadCommits_UnreachableMetadataFallsBack(t *testing.T) {
	installFakeMetadata(t, map[string]string{
		"commits": `["aaaaaaa"]`, // pre-squash apprentice SHA
	})
	r := &fakeLookupRunner{
		logGrepOut: "bbbbbbb chore: squashed main commit (spi-x)\n",
		reachable: map[string]bool{
			"aaaaaaa": false, // unreachable post-squash
			"bbbbbbb": true,  // reachable on main
		},
	}
	installFakeRunner(t, r)

	got, err := LookupBeadCommits("spi-x", "/repo")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d refs, want 2", len(got))
	}
	if got[0].Reachable {
		t.Error("aaaaaaa should be unreachable post-squash")
	}
	if !got[1].Reachable {
		t.Error("bbbbbbb should be reachable (squash on main)")
	}
	if got[1].Source != "grep" {
		t.Errorf("squash fallback row should have Source=grep, got %q", got[1].Source)
	}
}

// TestLookupBeadCommits_IdempotentReruns: running the helper twice
// produces the same output (the helper is read-only — no state on
// disk to drift between runs).
func TestLookupBeadCommits_IdempotentReruns(t *testing.T) {
	installFakeMetadata(t, map[string]string{
		"commits": `["abc1234"]`,
	})
	r := &fakeLookupRunner{
		logGrepOut: "abc1234 feat(spi-x): foo\n",
		reachable:  map[string]bool{"abc1234": true},
	}
	installFakeRunner(t, r)

	first, err := LookupBeadCommits("spi-x", "/repo")
	if err != nil {
		t.Fatalf("first err = %v", err)
	}
	second, err := LookupBeadCommits("spi-x", "/repo")
	if err != nil {
		t.Fatalf("second err = %v", err)
	}
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("first=%d second=%d, want 1 each", len(first), len(second))
	}
	if first[0].SHA != second[0].SHA {
		t.Errorf("non-idempotent: first=%q second=%q", first[0].SHA, second[0].SHA)
	}
}

// TestLookupBeadCommits_EmptyInputs: the helper is defensive — empty
// beadID or empty repoPath both return nil rows without erroring.
func TestLookupBeadCommits_EmptyInputs(t *testing.T) {
	got, err := LookupBeadCommits("", "/repo")
	if err != nil || got != nil {
		t.Errorf("empty beadID: got=%v err=%v, want nil/nil", got, err)
	}
	got, err = LookupBeadCommits("spi-x", "")
	if err != nil || got != nil {
		t.Errorf("empty repoPath: got=%v err=%v, want nil/nil", got, err)
	}
}

// TestLookupBeadCommits_MultiBeadPRs: a single SHA appended to multiple
// beads' metadata is independent — querying bead A returns the SHA
// once; querying bead B (with the same SHA) also returns it once.
// (Read-side dedupe is per-bead — we don't try to share rows across
// beads.)
func TestLookupBeadCommits_MultiBeadPRs(t *testing.T) {
	// Bead A's metadata has the shared SHA.
	installFakeMetadata(t, map[string]string{
		"commits": `["abc1234"]`,
	})
	r := &fakeLookupRunner{
		logGrepOut: "abc1234 feat: shared work for spi-A and spi-B\n",
		reachable:  map[string]bool{"abc1234": true},
	}
	installFakeRunner(t, r)

	got, err := LookupBeadCommits("spi-A", "/repo")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 1 || got[0].SHA != "abc1234" {
		t.Errorf("got = %+v, want one row for abc1234", got)
	}
}

// TestParseGrepShas_FiltersGarbage exercises the parser used by both
// the close-sweep and LookupBeadCommits paths. Mixed-quality input
// should yield only valid hex tokens.
func TestParseGrepShas_FiltersGarbage(t *testing.T) {
	out := strings.Join([]string{
		"abc1234 feat: ok",
		"   ", // whitespace
		"",    // empty
		"NOT_HEX_HERE invalid sha",
		"def5678 second valid",
		"ZZ short hex (filtered)", // non-hex
		"123 too short",            // <4 chars
	}, "\n")

	got := parseGrepShas(out)
	if len(got) != 2 || got[0] != "abc1234" || got[1] != "def5678" {
		t.Errorf("got %v, want [abc1234 def5678]", got)
	}
}

// TestLooksLikeSHA_Boundaries pins the SHA-shape predicate. <4 chars
// fails, >40 chars fails, mixed hex/non-hex fails, common short
// (7-char) and full (40-char) SHAs pass.
func TestLooksLikeSHA_Boundaries(t *testing.T) {
	tests := []struct {
		s    string
		want bool
	}{
		{"abc", false},                                    // too short
		{"abcd", true},                                    // 4 chars min
		{"abcdef0123456789abcdef0123456789abcdef01", true}, // 40-char full SHA
		{"abcdef0123456789abcdef0123456789abcdef012", false}, // >40
		{"deadbeef", true},
		{"DEADBEEF", true},
		{"deadbe-f", false},
		{"", false},
	}
	for _, tt := range tests {
		got := looksLikeSHA(tt.s)
		if got != tt.want {
			t.Errorf("looksLikeSHA(%q) = %v, want %v", tt.s, got, tt.want)
		}
	}
}
