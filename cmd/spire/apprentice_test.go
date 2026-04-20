package main

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
)

// apprenticeTestHarness captures everything the submit command writes so
// tests can assert on the full effect set. We stub the store and the
// bundlestore so tests don't need a live dolt server and don't touch
// XDG_DATA_HOME.
type apprenticeTestHarness struct {
	mu       sync.Mutex
	bead     Bead
	metadata map[string]string
	comments []string
	store    *bundlestore.LocalStore
	storeDir string
	// storeErrOnPut forces the first Put to return ErrDuplicate so the
	// idempotency branch can be exercised without a real prior handle.
	storeErrOnPut error
}

// newApprenticeHarness wires test doubles around the apprentice command. The
// returned cleanup func restores the original globals so subsequent tests
// don't see leaked stubs.
func newApprenticeHarness(t *testing.T, bead Bead) (*apprenticeTestHarness, func()) {
	t.Helper()

	storeDir := t.TempDir()
	ls, err := bundlestore.NewLocalStore(bundlestore.Config{LocalRoot: storeDir})
	if err != nil {
		t.Fatalf("bundlestore: %v", err)
	}

	h := &apprenticeTestHarness{
		bead:     bead,
		metadata: map[string]string{},
		store:    ls,
		storeDir: storeDir,
	}

	origGetBead := apprenticeGetBeadFunc
	origSetMeta := apprenticeSetBeadMetadataFunc
	origAddComment := apprenticeAddCommentFunc
	origNewStore := apprenticeNewBundleStoreFunc
	origNow := apprenticeNowFunc

	apprenticeGetBeadFunc = func(id string) (Bead, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if id != h.bead.ID {
			return Bead{}, fmt.Errorf("unexpected bead id %q", id)
		}
		return h.bead, nil
	}
	apprenticeSetBeadMetadataFunc = func(id, key, value string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.metadata[key] = value
		return nil
	}
	apprenticeAddCommentFunc = func(id, text string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.comments = append(h.comments, text)
		return nil
	}
	apprenticeNewBundleStoreFunc = func() (bundlestore.BundleStore, error) {
		return h.store, nil
	}
	apprenticeNowFunc = func() time.Time {
		// Deterministic clock; individual tests may override apprenticeNowFunc
		// after construction to simulate time advancing between submits.
		return time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	}

	cleanup := func() {
		apprenticeGetBeadFunc = origGetBead
		apprenticeSetBeadMetadataFunc = origSetMeta
		apprenticeAddCommentFunc = origAddComment
		apprenticeNewBundleStoreFunc = origNewStore
		apprenticeNowFunc = origNow
	}
	return h, cleanup
}

// setupGitRepo creates a temporary git repo with an initial commit on main,
// then checks out a feature branch. Chdirs into the repo; returns a cleanup
// func that chdirs back and removes the dir.
func setupGitRepo(t *testing.T) (dir string, cleanup func()) {
	t.Helper()
	dir = t.TempDir()

	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	runGit("init", "--initial-branch=main")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test User")
	runGit("config", "commit.gpgsign", "false")

	// Initial commit on main so base..HEAD ranges can be computed.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit("add", ".")
	runGit("commit", "-m", "chore: initial commit")

	// Work on a feature branch so main is a clean base.
	runGit("checkout", "-b", "feat/test")

	return dir, func() {
		os.Chdir(orig)
	}
}

// addCommit writes a file and creates a commit with the given subject line.
func addCommit(t *testing.T, dir, filename, subject string) string {
	t.Helper()
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, []byte(subject), 0o644); err != nil {
		t.Fatalf("write %s: %v", filename, err)
	}
	runGit := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	runGit("add", filename)
	runGit("commit", "-m", subject)

	shaCmd := exec.Command("git", "rev-parse", "HEAD")
	shaCmd.Dir = dir
	out, err := shaCmd.Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// clearApprenticeEnv unsets every SPIRE_* env var the submit command reads so
// tests start from a known state.
func clearApprenticeEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"SPIRE_BEAD_ID", "SPIRE_ATTEMPT_ID", "SPIRE_APPRENTICE_IDX"} {
		t.Setenv(k, "")
	}
}

// --- Happy path ----------------------------------------------------------

func TestApprenticeSubmit_HappyPath(t *testing.T) {
	dir, teardown := setupGitRepo(t)
	defer teardown()

	sha1 := addCommit(t, dir, "a.txt", "feat(spi-abc): add a")
	sha2 := addCommit(t, dir, "b.txt", "feat(spi-abc): add b")

	bead := Bead{ID: "spi-abc", Title: "happy path", Labels: []string{"base-branch:main"}}
	h, cleanup := newApprenticeHarness(t, bead)
	defer cleanup()

	clearApprenticeEnv(t)
	t.Setenv("SPIRE_BEAD_ID", "spi-abc")
	t.Setenv("SPIRE_ATTEMPT_ID", "spi-abc-att1")
	t.Setenv("SPIRE_APPRENTICE_IDX", "0")

	if err := cmdApprenticeSubmit("", "", false); err != nil {
		t.Fatalf("submit: %v", err)
	}

	metaKey := "apprentice_signal_apprentice-spi-abc-0"
	raw, ok := h.metadata[metaKey]
	if !ok {
		t.Fatalf("metadata key %q not set; got keys %v", metaKey, keysOf(h.metadata))
	}
	var payload signalPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("unmarshal signal: %v\nraw: %s", err, raw)
	}
	if payload.Kind != "bundle" {
		t.Errorf("kind = %q, want bundle", payload.Kind)
	}
	if payload.Role != "apprentice-spi-abc-0" {
		t.Errorf("role = %q, want apprentice-spi-abc-0", payload.Role)
	}
	if payload.BundleKey == "" {
		t.Error("bundle_key empty")
	}
	if len(payload.Commits) != 2 || payload.Commits[0] != sha1 || payload.Commits[1] != sha2 {
		t.Errorf("commits = %v, want [%s %s]", payload.Commits, sha1, sha2)
	}
	if payload.SubmittedAt == "" {
		t.Error("submitted_at empty")
	}

	// Bundle must be retrievable via Get and pass `git bundle verify`.
	rc, err := h.store.Get(context.Background(), bundlestore.BundleHandle{
		BeadID: "spi-abc",
		Key:    payload.BundleKey,
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	bundleBytes, _ := io.ReadAll(rc)
	rc.Close()

	bundleFile := filepath.Join(t.TempDir(), "roundtrip.bundle")
	if err := os.WriteFile(bundleFile, bundleBytes, 0o644); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	cmd := exec.Command("git", "bundle", "verify", bundleFile)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git bundle verify failed: %v\n%s", err, out)
	}

	// Comment should mention the role.
	if len(h.comments) == 0 {
		t.Fatal("no comments recorded")
	}
	if !strings.Contains(h.comments[0], "apprentice-spi-abc-0") {
		t.Errorf("comment %q missing role", h.comments[0])
	}
	if !strings.Contains(h.comments[0], "bundle") {
		t.Errorf("comment %q missing 'bundle'", h.comments[0])
	}
}

// --- --no-changes --------------------------------------------------------

func TestApprenticeSubmit_NoChanges(t *testing.T) {
	dir, teardown := setupGitRepo(t)
	defer teardown()
	_ = dir // no extra commits on the feature branch

	bead := Bead{ID: "spi-noop", Title: "no-op", Labels: []string{"base-branch:main"}}
	h, cleanup := newApprenticeHarness(t, bead)
	defer cleanup()

	clearApprenticeEnv(t)
	t.Setenv("SPIRE_BEAD_ID", "spi-noop")
	t.Setenv("SPIRE_ATTEMPT_ID", "spi-noop-att1")

	if err := cmdApprenticeSubmit("", "", true); err != nil {
		t.Fatalf("submit --no-changes: %v", err)
	}

	raw, ok := h.metadata["apprentice_signal_apprentice-spi-noop-0"]
	if !ok {
		t.Fatalf("missing metadata key; got %v", keysOf(h.metadata))
	}
	var payload signalPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Kind != "no-op" {
		t.Errorf("kind = %q, want no-op", payload.Kind)
	}
	if payload.BundleKey != "" {
		t.Errorf("bundle_key = %q, want empty", payload.BundleKey)
	}
	if len(payload.Commits) != 0 {
		t.Errorf("commits = %v, want empty", payload.Commits)
	}

	// No bundle should be in the store.
	handles, err := h.store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(handles) != 0 {
		t.Errorf("expected empty store, got %v", handles)
	}

	if len(h.comments) == 0 || !strings.Contains(h.comments[0], "no-changes") {
		t.Errorf("comment missing no-changes mention: %v", h.comments)
	}
}

// --- Bad commit message --------------------------------------------------

func TestApprenticeSubmit_BadCommitMessage(t *testing.T) {
	dir, teardown := setupGitRepo(t)
	defer teardown()

	// First commit has correct prefix; second does not; third also wrong.
	addCommit(t, dir, "a.txt", "feat(spi-bad): ok commit")
	sha2 := addCommit(t, dir, "b.txt", "untyped message no bead id")
	sha3 := addCommit(t, dir, "c.txt", "chore: missing parens")

	bead := Bead{ID: "spi-bad", Title: "bad", Labels: []string{"base-branch:main"}}
	_, cleanup := newApprenticeHarness(t, bead)
	defer cleanup()

	clearApprenticeEnv(t)
	t.Setenv("SPIRE_BEAD_ID", "spi-bad")
	t.Setenv("SPIRE_ATTEMPT_ID", "spi-bad-att1")

	err := cmdApprenticeSubmit("", "", false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, sha2) {
		t.Errorf("error missing sha2 %q:\n%s", sha2, msg)
	}
	if !strings.Contains(msg, sha3) {
		t.Errorf("error missing sha3 %q:\n%s", sha3, msg)
	}
}

// --- Dirty worktree ------------------------------------------------------

func TestApprenticeSubmit_DirtyWorktree(t *testing.T) {
	dir, teardown := setupGitRepo(t)
	defer teardown()

	addCommit(t, dir, "a.txt", "feat(spi-dirty): committed")

	// Leave an untracked file behind.
	untracked := filepath.Join(dir, "leftover.txt")
	if err := os.WriteFile(untracked, []byte("scratch"), 0o644); err != nil {
		t.Fatalf("write untracked: %v", err)
	}

	bead := Bead{ID: "spi-dirty", Title: "dirty", Labels: []string{"base-branch:main"}}
	_, cleanup := newApprenticeHarness(t, bead)
	defer cleanup()

	clearApprenticeEnv(t)
	t.Setenv("SPIRE_BEAD_ID", "spi-dirty")
	t.Setenv("SPIRE_ATTEMPT_ID", "spi-dirty-att1")

	err := cmdApprenticeSubmit("", "", false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "leftover.txt") {
		t.Errorf("error missing dirty file path:\n%s", err.Error())
	}
}

// --- Non-zero apprentice idx --------------------------------------------

func TestApprenticeSubmit_NonZeroIdx(t *testing.T) {
	dir, teardown := setupGitRepo(t)
	defer teardown()
	_ = dir

	bead := Bead{ID: "spi-fanout", Title: "fan-out", Labels: []string{"base-branch:main"}}
	h, cleanup := newApprenticeHarness(t, bead)
	defer cleanup()

	clearApprenticeEnv(t)
	t.Setenv("SPIRE_BEAD_ID", "spi-fanout")
	t.Setenv("SPIRE_ATTEMPT_ID", "spi-fanout-att1")
	t.Setenv("SPIRE_APPRENTICE_IDX", "3")

	if err := cmdApprenticeSubmit("", "", true); err != nil {
		t.Fatalf("submit: %v", err)
	}

	wantKey := "apprentice_signal_apprentice-spi-fanout-3"
	raw, ok := h.metadata[wantKey]
	if !ok {
		t.Fatalf("metadata key %q missing; got %v", wantKey, keysOf(h.metadata))
	}
	var payload signalPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Role != "apprentice-spi-fanout-3" {
		t.Errorf("role = %q, want apprentice-spi-fanout-3", payload.Role)
	}
}

// --- Idempotent re-submit -----------------------------------------------

func TestApprenticeSubmit_IdempotentResubmit(t *testing.T) {
	dir, teardown := setupGitRepo(t)
	defer teardown()

	addCommit(t, dir, "a.txt", "feat(spi-idem): one")
	addCommit(t, dir, "b.txt", "feat(spi-idem): two")

	bead := Bead{ID: "spi-idem", Title: "idem", Labels: []string{"base-branch:main"}}
	h, cleanup := newApprenticeHarness(t, bead)
	defer cleanup()

	clearApprenticeEnv(t)
	t.Setenv("SPIRE_BEAD_ID", "spi-idem")
	t.Setenv("SPIRE_ATTEMPT_ID", "spi-idem-att1")

	// First submit at t=0.
	apprenticeNowFunc = func() time.Time {
		return time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	}
	if err := cmdApprenticeSubmit("", "", false); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	firstPayload := h.metadata["apprentice_signal_apprentice-spi-idem-0"]
	if firstPayload == "" {
		t.Fatal("no first payload")
	}

	handlesAfterFirst, _ := h.store.List(context.Background())
	if len(handlesAfterFirst) != 1 {
		t.Fatalf("expected 1 bundle after first submit, got %d", len(handlesAfterFirst))
	}
	firstBundle, _ := os.ReadFile(filepath.Join(h.storeDir, handlesAfterFirst[0].Key))

	// Second submit at t=+1h — must not leak ErrDuplicate, must refresh
	// the on-disk bundle and the signal JSON.
	apprenticeNowFunc = func() time.Time {
		return time.Date(2026, 1, 1, 13, 0, 0, 0, time.UTC)
	}
	if err := cmdApprenticeSubmit("", "", false); err != nil {
		t.Fatalf("second submit: %v", err)
	}

	handlesAfterSecond, _ := h.store.List(context.Background())
	if len(handlesAfterSecond) != 1 {
		t.Fatalf("expected 1 bundle after second submit, got %d", len(handlesAfterSecond))
	}
	secondBundle, _ := os.ReadFile(filepath.Join(h.storeDir, handlesAfterSecond[0].Key))
	// The bundle was rewritten — file exists, contents are a valid bundle.
	// (Byte-for-byte equality is possible for identical inputs; the point
	// here is that the rename-into-place completed rather than bailing
	// out on ErrDuplicate.)
	if len(secondBundle) == 0 {
		t.Fatal("second bundle is empty — Put did not complete")
	}
	if !bytes.Equal(firstBundle, secondBundle) && len(secondBundle) == 0 {
		t.Error("second bundle unexpectedly empty")
	}

	// submitted_at should have advanced.
	var p1, p2 signalPayload
	_ = json.Unmarshal([]byte(firstPayload), &p1)
	_ = json.Unmarshal([]byte(h.metadata["apprentice_signal_apprentice-spi-idem-0"]), &p2)
	if p1.SubmittedAt == p2.SubmittedAt {
		t.Errorf("submitted_at did not advance (%s == %s)", p1.SubmittedAt, p2.SubmittedAt)
	}
}

// --- Missing bead ID -----------------------------------------------------

func TestApprenticeSubmit_MissingBeadID(t *testing.T) {
	dir, teardown := setupGitRepo(t)
	defer teardown()
	_ = dir

	bead := Bead{ID: "spi-x"}
	_, cleanup := newApprenticeHarness(t, bead)
	defer cleanup()

	clearApprenticeEnv(t)

	err := cmdApprenticeSubmit("", "", false)
	if err == nil {
		t.Fatal("expected error when no bead ID provided")
	}
	if !strings.Contains(err.Error(), "bead") {
		t.Errorf("error %q does not mention bead", err.Error())
	}
}

func keysOf(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
