package apprentice

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/bundlestore"
	"github.com/awell-health/spire/pkg/store"
)

// submitFixture bundles a temp git repo, a real LocalStore, and in-memory
// implementations of the bead store callbacks. Tests wire Options against
// these and assert on the captured state.
type submitFixture struct {
	t         *testing.T
	repoDir   string
	bead      store.Bead
	metadata  map[string]string
	comments  []string
	mu        sync.Mutex
	bstore    *bundlestore.LocalStore
	storeDir  string
	now       time.Time
	getBeadFn func(id string) (store.Bead, error)
}

func newSubmitFixture(t *testing.T, beadID, baseBranch string) *submitFixture {
	t.Helper()
	repoDir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, string(out))
		}
	}
	runGit("init", "--initial-branch=main")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test User")
	runGit("config", "commit.gpgsign", "false")

	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# test\n"), 0o644); err != nil {
		t.Fatalf("seed README: %v", err)
	}
	runGit("add", ".")
	runGit("commit", "-m", "chore: initial")
	runGit("checkout", "-b", "feat/"+beadID)

	storeDir := t.TempDir()
	ls, err := bundlestore.NewLocalStore(bundlestore.Config{LocalRoot: storeDir})
	if err != nil {
		t.Fatalf("bundlestore: %v", err)
	}

	f := &submitFixture{
		t:        t,
		repoDir:  repoDir,
		bead:     store.Bead{ID: beadID, Labels: []string{"base-branch:" + baseBranch}},
		metadata: map[string]string{},
		bstore:   ls,
		storeDir: storeDir,
		now:      time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC),
	}
	f.getBeadFn = func(id string) (store.Bead, error) {
		if id != f.bead.ID {
			return store.Bead{}, fmt.Errorf("unexpected bead id %q", id)
		}
		return f.bead, nil
	}
	return f
}

func (f *submitFixture) addCommit(filename, subject string) string {
	f.t.Helper()
	path := filepath.Join(f.repoDir, filename)
	if err := os.WriteFile(path, []byte(subject), 0o644); err != nil {
		f.t.Fatalf("write %s: %v", filename, err)
	}
	runGit := func(args ...string) {
		f.t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = f.repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			f.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, string(out))
		}
	}
	runGit("add", filename)
	runGit("commit", "-m", subject)
	out, err := exec.Command("git", "-C", f.repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		f.t.Fatalf("rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// options returns a baseline Options wired to this fixture. Tests mutate the
// returned copy before passing it to Submit.
func (f *submitFixture) options(beadID string, idx int) Options {
	return Options{
		BeadID:        beadID,
		AttemptID:     beadID + "-att1",
		ApprenticeIdx: idx,
		WorktreeDir:   f.repoDir,
		Store:         f.bstore,
		GetBead:       f.getBeadFn,
		SetMetadata: func(id, key, value string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.metadata[key] = value
			return nil
		},
		AddComment: func(id, text string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.comments = append(f.comments, text)
			return nil
		},
		Now: func() time.Time { return f.now },
	}
}

// TestSubmit_HappyPath exercises the full bundle-produce path end-to-end
// with a real git repo and a real LocalStore: commits are enumerated from
// base..HEAD, bundled, uploaded, and the signal metadata + comment land.
func TestSubmit_HappyPath(t *testing.T) {
	f := newSubmitFixture(t, "spi-hp", "main")
	sha1 := f.addCommit("a.txt", "feat(spi-hp): first")
	sha2 := f.addCommit("b.txt", "feat(spi-hp): second")

	if err := Submit(context.Background(), f.options("spi-hp", 0)); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	metaKey := "apprentice_signal_apprentice-spi-hp-0"
	raw, ok := f.metadata[metaKey]
	if !ok {
		t.Fatalf("missing signal metadata %q; got %v", metaKey, keysOf(f.metadata))
	}
	var payload SignalPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("unmarshal signal: %v\nraw: %s", err, raw)
	}
	if payload.Kind != bundlestore.SignalKindBundle {
		t.Errorf("kind = %q, want %q", payload.Kind, bundlestore.SignalKindBundle)
	}
	if payload.Role != "apprentice-spi-hp-0" {
		t.Errorf("role = %q, want apprentice-spi-hp-0", payload.Role)
	}
	if payload.BundleKey == "" {
		t.Error("bundle_key empty")
	}
	if len(payload.Commits) != 2 || payload.Commits[0] != sha1 || payload.Commits[1] != sha2 {
		t.Errorf("commits = %v, want [%s %s]", payload.Commits, sha1, sha2)
	}
	if payload.SubmittedAt != f.now.Format(time.RFC3339) {
		t.Errorf("submitted_at = %q, want %q", payload.SubmittedAt, f.now.Format(time.RFC3339))
	}

	rc, err := f.bstore.Get(context.Background(), bundlestore.BundleHandle{BeadID: "spi-hp", Key: payload.BundleKey})
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	bundleBytes, _ := io.ReadAll(rc)
	rc.Close()
	bundleFile := filepath.Join(t.TempDir(), "rt.bundle")
	if err := os.WriteFile(bundleFile, bundleBytes, 0o644); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	verify := exec.Command("git", "-C", f.repoDir, "bundle", "verify", bundleFile)
	if out, err := verify.CombinedOutput(); err != nil {
		t.Fatalf("git bundle verify: %v\n%s", err, out)
	}

	if len(f.comments) != 1 || !strings.Contains(f.comments[0], "apprentice-spi-hp-0") {
		t.Errorf("comments = %v; want one comment naming the role", f.comments)
	}
}

// TestSubmit_NoChanges writes only the no-op signal and uploads no bundle.
func TestSubmit_NoChanges(t *testing.T) {
	f := newSubmitFixture(t, "spi-noop", "main")

	opts := f.options("spi-noop", 0)
	opts.NoChanges = true
	if err := Submit(context.Background(), opts); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	raw, ok := f.metadata["apprentice_signal_apprentice-spi-noop-0"]
	if !ok {
		t.Fatalf("metadata missing; got %v", keysOf(f.metadata))
	}
	var payload SignalPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Kind != bundlestore.SignalKindNoOp {
		t.Errorf("kind = %q, want %q", payload.Kind, bundlestore.SignalKindNoOp)
	}
	if payload.BundleKey != "" {
		t.Errorf("bundle_key = %q, want empty", payload.BundleKey)
	}
	if len(payload.Commits) != 0 {
		t.Errorf("commits = %v, want empty", payload.Commits)
	}

	handles, err := f.bstore.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(handles) != 0 {
		t.Errorf("store should be empty, got %v", handles)
	}
	if len(f.comments) == 0 || !strings.Contains(f.comments[0], "no-changes") {
		t.Errorf("comment missing no-changes mention: %v", f.comments)
	}
}

// TestSubmit_BaseBranchFromLabel uses the bead's base-branch: label when
// Options.BaseBranch is empty.
func TestSubmit_BaseBranchFromLabel(t *testing.T) {
	// Build a repo whose default branch is "develop" (not "main"), then
	// decorate the bead with base-branch:develop so Submit must resolve it
	// from the label to produce a non-empty commit range.
	repoDir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, string(out))
		}
	}
	runGit("init", "--initial-branch=develop")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test User")
	runGit("config", "commit.gpgsign", "false")
	os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("# test\n"), 0o644)
	runGit("add", ".")
	runGit("commit", "-m", "chore: initial")
	runGit("checkout", "-b", "feat/spi-lbl")

	storeDir := t.TempDir()
	ls, _ := bundlestore.NewLocalStore(bundlestore.Config{LocalRoot: storeDir})

	metadata := map[string]string{}
	var comments []string
	bead := store.Bead{ID: "spi-lbl", Labels: []string{"base-branch:develop"}}

	// Add a valid commit.
	os.WriteFile(filepath.Join(repoDir, "x.txt"), []byte("x"), 0o644)
	runGit("add", "x.txt")
	runGit("commit", "-m", "feat(spi-lbl): add x")

	opts := Options{
		BeadID:        "spi-lbl",
		AttemptID:     "spi-lbl-att1",
		ApprenticeIdx: 0,
		// BaseBranch intentionally left empty; label lookup must set it.
		WorktreeDir: repoDir,
		Store:       ls,
		GetBead:     func(id string) (store.Bead, error) { return bead, nil },
		SetMetadata: func(id, key, value string) error {
			metadata[key] = value
			return nil
		},
		AddComment: func(id, text string) error {
			comments = append(comments, text)
			return nil
		},
		Now: func() time.Time { return time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC) },
	}
	if err := Submit(context.Background(), opts); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if _, ok := metadata["apprentice_signal_apprentice-spi-lbl-0"]; !ok {
		t.Fatalf("signal missing; got %v", keysOf(metadata))
	}
}

// TestSubmit_BadCommitMessage rejects commits that do not reference the bead.
func TestSubmit_BadCommitMessage(t *testing.T) {
	f := newSubmitFixture(t, "spi-bad", "main")
	f.addCommit("a.txt", "feat(spi-bad): good")
	offender := f.addCommit("b.txt", "untyped no bead id")
	offender2 := f.addCommit("c.txt", "chore: no parens")

	err := Submit(context.Background(), f.options("spi-bad", 0))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, offender) {
		t.Errorf("error missing offender sha %q:\n%s", offender, msg)
	}
	if !strings.Contains(msg, offender2) {
		t.Errorf("error missing second offender sha %q:\n%s", offender2, msg)
	}
	if _, ok := f.metadata["apprentice_signal_apprentice-spi-bad-0"]; ok {
		t.Error("signal metadata should NOT be written on bad-commit rejection")
	}
}

// TestSubmit_DirtyWorktree refuses to bundle when the worktree is dirty.
func TestSubmit_DirtyWorktree(t *testing.T) {
	f := newSubmitFixture(t, "spi-dirty", "main")
	f.addCommit("a.txt", "feat(spi-dirty): committed")
	os.WriteFile(filepath.Join(f.repoDir, "leftover.txt"), []byte("scratch"), 0o644)

	err := Submit(context.Background(), f.options("spi-dirty", 0))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "leftover.txt") {
		t.Errorf("error missing dirty file:\n%s", err.Error())
	}
}

// TestSubmit_NoCommitsAndNotNoChanges errors when there are no commits in
// the range and NoChanges is false.
func TestSubmit_NoCommitsAndNotNoChanges(t *testing.T) {
	f := newSubmitFixture(t, "spi-empty", "main")
	err := Submit(context.Background(), f.options("spi-empty", 0))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "NoChanges") {
		t.Errorf("error should hint at NoChanges flag:\n%s", err.Error())
	}
}

// TestSubmit_MissingBeadID errors cleanly when opts.BeadID is empty.
func TestSubmit_MissingBeadID(t *testing.T) {
	f := newSubmitFixture(t, "spi-any", "main")
	opts := f.options("spi-any", 0)
	opts.BeadID = ""
	err := Submit(context.Background(), opts)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "bead") {
		t.Errorf("error should mention bead id: %s", err.Error())
	}
}

// TestSubmit_MissingStore errors when NoChanges=false and Store is nil.
func TestSubmit_MissingStore(t *testing.T) {
	f := newSubmitFixture(t, "spi-nostore", "main")
	f.addCommit("a.txt", "feat(spi-nostore): x")
	opts := f.options("spi-nostore", 0)
	opts.Store = nil
	err := Submit(context.Background(), opts)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "bundle store required") {
		t.Errorf("error should mention missing store: %s", err.Error())
	}
}

// TestSubmit_NonZeroIdx verifies the role key respects ApprenticeIdx.
func TestSubmit_NonZeroIdx(t *testing.T) {
	f := newSubmitFixture(t, "spi-fan", "main")
	opts := f.options("spi-fan", 3)
	opts.NoChanges = true
	if err := Submit(context.Background(), opts); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	wantKey := "apprentice_signal_apprentice-spi-fan-3"
	if _, ok := f.metadata[wantKey]; !ok {
		t.Fatalf("missing key %q; got %v", wantKey, keysOf(f.metadata))
	}
}

// TestSubmit_DuplicateRetry verifies the idempotent re-submit: when Put
// reports ErrDuplicate, the prior handle is deleted and the bundle is
// re-uploaded exactly once.
func TestSubmit_DuplicateRetry(t *testing.T) {
	f := newSubmitFixture(t, "spi-idem", "main")
	f.addCommit("a.txt", "feat(spi-idem): one")
	f.addCommit("b.txt", "feat(spi-idem): two")

	// First submit.
	if err := Submit(context.Background(), f.options("spi-idem", 0)); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	handlesAfterFirst, _ := f.bstore.List(context.Background())
	if len(handlesAfterFirst) != 1 {
		t.Fatalf("after first submit, expected 1 bundle, got %d", len(handlesAfterFirst))
	}
	firstBytes, _ := os.ReadFile(filepath.Join(f.storeDir, handlesAfterFirst[0].Key))

	// Advance the clock and re-submit. The second submit exercises the
	// ErrDuplicate → Delete → retry branch inside putBundle.
	f.now = f.now.Add(1 * time.Hour)
	if err := Submit(context.Background(), f.options("spi-idem", 0)); err != nil {
		t.Fatalf("second submit: %v", err)
	}
	handlesAfterSecond, _ := f.bstore.List(context.Background())
	if len(handlesAfterSecond) != 1 {
		t.Fatalf("after second submit, expected 1 bundle, got %d", len(handlesAfterSecond))
	}
	secondBytes, _ := os.ReadFile(filepath.Join(f.storeDir, handlesAfterSecond[0].Key))
	if len(secondBytes) == 0 {
		t.Fatal("second bundle is empty — retry did not re-upload")
	}
	if !bytes.Equal(firstBytes, secondBytes) && len(secondBytes) == 0 {
		t.Error("second bundle unexpectedly empty")
	}
}

// TestSubmit_WorktreeDirScoping proves Submit runs git commands in
// opts.WorktreeDir, not the process CWD. The fixture chdirs the test process
// away from the repo and then calls Submit with WorktreeDir set.
func TestSubmit_WorktreeDirScoping(t *testing.T) {
	f := newSubmitFixture(t, "spi-wtd", "main")
	f.addCommit("a.txt", "feat(spi-wtd): x")

	// Chdir to a scratch dir that is not a git repo. If Submit were running
	// git in CWD, the status/log/bundle commands would fail here.
	scratch := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(scratch); err != nil {
		t.Fatalf("chdir scratch: %v", err)
	}
	defer os.Chdir(orig)

	if err := Submit(context.Background(), f.options("spi-wtd", 0)); err != nil {
		t.Fatalf("Submit with WorktreeDir scope: %v", err)
	}
	if _, ok := f.metadata["apprentice_signal_apprentice-spi-wtd-0"]; !ok {
		t.Fatalf("signal missing; got %v", keysOf(f.metadata))
	}
}

// TestSubmit_HandoffModeOnBundle verifies that the bundle-path signal
// records opts.HandoffMode verbatim — the apprentice emits whatever the
// executor selected; it does not choose. See spi-xplwy chunk 5a.
func TestSubmit_HandoffModeOnBundle(t *testing.T) {
	f := newSubmitFixture(t, "spi-hm", "main")
	f.addCommit("a.txt", "feat(spi-hm): x")

	opts := f.options("spi-hm", 0)
	opts.HandoffMode = "bundle"

	if err := Submit(context.Background(), opts); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	raw, ok := f.metadata["apprentice_signal_apprentice-spi-hm-0"]
	if !ok {
		t.Fatalf("signal missing; got %v", keysOf(f.metadata))
	}
	var payload SignalPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("unmarshal signal: %v", err)
	}
	if payload.HandoffMode != "bundle" {
		t.Errorf("handoff_mode = %q, want \"bundle\"", payload.HandoffMode)
	}
}

// TestSubmit_HandoffModeOnNoChanges verifies the no-op signal also carries
// the HandoffMode — even a no-op delivery still happened under a chosen
// mode, and the chunk-6 observability sweep relies on the field being
// present everywhere.
func TestSubmit_HandoffModeOnNoChanges(t *testing.T) {
	f := newSubmitFixture(t, "spi-hmn", "main")

	opts := f.options("spi-hmn", 0)
	opts.NoChanges = true
	opts.HandoffMode = "bundle"

	if err := Submit(context.Background(), opts); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	raw, ok := f.metadata["apprentice_signal_apprentice-spi-hmn-0"]
	if !ok {
		t.Fatalf("signal missing; got %v", keysOf(f.metadata))
	}
	var payload SignalPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("unmarshal signal: %v", err)
	}
	if payload.HandoffMode != "bundle" {
		t.Errorf("handoff_mode = %q, want \"bundle\"", payload.HandoffMode)
	}
}

// TestSubmit_HandoffModeEmptyOmitted verifies that when opts.HandoffMode
// is empty (older callers), the field is omitted from the JSON — consumers
// treat empty as "unknown," not as HandoffNone.
func TestSubmit_HandoffModeEmptyOmitted(t *testing.T) {
	f := newSubmitFixture(t, "spi-hme", "main")
	f.addCommit("a.txt", "feat(spi-hme): x")

	opts := f.options("spi-hme", 0)
	// HandoffMode left empty.

	if err := Submit(context.Background(), opts); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	raw := f.metadata["apprentice_signal_apprentice-spi-hme-0"]
	if strings.Contains(raw, "handoff_mode") {
		t.Errorf("raw signal should omit handoff_mode when unset, got: %s", raw)
	}
}

// TestSubmit_AttemptIDFallback verifies the "-local" fallback when AttemptID
// is empty: the bundle still lands under a stable key.
func TestSubmit_AttemptIDFallback(t *testing.T) {
	f := newSubmitFixture(t, "spi-local", "main")
	f.addCommit("a.txt", "feat(spi-local): x")
	opts := f.options("spi-local", 0)
	opts.AttemptID = ""

	if err := Submit(context.Background(), opts); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	handles, _ := f.bstore.List(context.Background())
	if len(handles) != 1 {
		t.Fatalf("expected 1 bundle, got %d", len(handles))
	}
	if !strings.Contains(handles[0].Key, "spi-local-local-0.bundle") {
		t.Errorf("key = %q, want *-local suffix", handles[0].Key)
	}
}

// TestSubmit_GitCommandError surfaces errors from the injected RunGit
// function rather than masking them.
func TestSubmit_GitCommandError(t *testing.T) {
	f := newSubmitFixture(t, "spi-err", "main")
	f.addCommit("a.txt", "feat(spi-err): x")

	opts := f.options("spi-err", 0)
	opts.RunGit = func(args ...string) ([]byte, error) {
		return nil, fmt.Errorf("boom: %v", args)
	}
	err := Submit(context.Background(), opts)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should surface RunGit failure: %s", err.Error())
	}
}

func keysOf(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// TestSubmit_StartSHA_ScopesHygieneCheck covers the review-fix-on-epic
// scenario: the branch has pre-existing subtask commits referencing child
// bead IDs (not the epic's ID), then the fix apprentice adds a new commit
// referencing the epic. With StartSHA set to the session baseline, the
// commit-reference check must skip the pre-existing commits and only
// validate what the apprentice itself produced.
func TestSubmit_StartSHA_ScopesHygieneCheck(t *testing.T) {
	f := newSubmitFixture(t, "spi-epic", "main")
	// Pre-existing subtask commits on the epic branch. These reference
	// child bead IDs, NOT the epic bead ID — legitimate for an epic branch.
	f.addCommit("a.txt", "feat(spi-child1): first subtask")
	f.addCommit("b.txt", "feat(spi-child2): second subtask")
	f.addCommit("c.txt", "feat(spi-child3): third subtask")

	// Capture the branch tip here — this is what wc.StartSHA would be at
	// the start of the review-fix apprentice session.
	out, err := exec.Command("git", "-C", f.repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	startSHA := strings.TrimSpace(string(out))

	// Fix apprentice adds its own commit referencing the epic bead.
	fixSHA := f.addCommit("d.txt", "fix(spi-epic): address sage feedback")

	opts := f.options("spi-epic", 0)
	opts.BaseBranch = "main"
	opts.StartSHA = startSHA
	if err := Submit(context.Background(), opts); err != nil {
		t.Fatalf("Submit with StartSHA should succeed despite pre-existing commits: %v", err)
	}

	// Signal should still contain ALL commits in base..HEAD (bundle content
	// is untouched by the scoping change).
	raw := f.metadata["apprentice_signal_apprentice-spi-epic-0"]
	var payload SignalPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(payload.Commits) != 4 {
		t.Errorf("bundle commit list = %d commits, want 4 (3 pre-existing + 1 fix)", len(payload.Commits))
	}
	foundFix := false
	for _, c := range payload.Commits {
		if c == fixSHA {
			foundFix = true
		}
	}
	if !foundFix {
		t.Errorf("fix commit %s missing from payload commits %v", fixSHA, payload.Commits)
	}
}

// TestSubmit_StartSHA_StillRejectsUntaggedOwnCommits verifies the scoping
// doesn't disable hygiene entirely — a new commit made by the apprentice
// that fails to reference the bead is still rejected, while the
// pre-existing commits are left alone.
func TestSubmit_StartSHA_StillRejectsUntaggedOwnCommits(t *testing.T) {
	f := newSubmitFixture(t, "spi-epic2", "main")
	f.addCommit("a.txt", "feat(spi-child1): subtask work")

	out, _ := exec.Command("git", "-C", f.repoDir, "rev-parse", "HEAD").Output()
	startSHA := strings.TrimSpace(string(out))

	// Apprentice makes a commit that fails to reference the epic bead.
	f.addCommit("d.txt", "chore: forgot to tag this one")

	opts := f.options("spi-epic2", 0)
	opts.BaseBranch = "main"
	opts.StartSHA = startSHA
	err := Submit(context.Background(), opts)
	if err == nil {
		t.Fatal("Submit should reject an untagged own-commit, got nil error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "1 commit(s) do not reference spi-epic2") {
		t.Errorf("error should count exactly 1 offender (the own-commit), got: %v", err)
	}
	if !strings.Contains(msg, "forgot to tag") {
		t.Errorf("error should cite the offending commit subject, got: %v", err)
	}
	if strings.Contains(msg, "spi-child1") {
		t.Errorf("error must NOT cite the pre-baseline commit; StartSHA scoping failed: %v", err)
	}
}

// TestSubmit_NoStartSHA_LegacyBehavior verifies the check falls back to
// base..HEAD when StartSHA is empty, preserving CLI-caller behavior.
func TestSubmit_NoStartSHA_LegacyBehavior(t *testing.T) {
	f := newSubmitFixture(t, "spi-leg", "main")
	f.addCommit("a.txt", "feat(spi-other): forgotten commit")

	opts := f.options("spi-leg", 0)
	opts.BaseBranch = "main"
	// StartSHA intentionally empty → legacy full-range check.
	err := Submit(context.Background(), opts)
	if err == nil {
		t.Fatal("Submit should reject in legacy mode when an unrelated commit is in base..HEAD")
	}
	if !strings.Contains(err.Error(), "1 commit(s) do not reference spi-leg") {
		t.Errorf("legacy error shape unexpected: %v", err)
	}
}
