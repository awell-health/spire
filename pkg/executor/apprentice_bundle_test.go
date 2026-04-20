package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/awell-health/spire/pkg/bundlestore"
	spgit "github.com/awell-health/spire/pkg/git"
)

// fakeBundleStore is a minimal in-memory BundleStore for testing. It captures
// Get/Delete calls so tests can assert ordering without a real filesystem.
type fakeBundleStore struct {
	bundles    map[string][]byte
	getErr     error
	deleteErr  error
	getCalls   int32
	delCalls   int32
	lastDelKey string
}

func newFakeBundleStore() *fakeBundleStore {
	return &fakeBundleStore{bundles: make(map[string][]byte)}
}

func (s *fakeBundleStore) Put(_ context.Context, _ bundlestore.PutRequest, _ io.Reader) (bundlestore.BundleHandle, error) {
	return bundlestore.BundleHandle{}, errors.New("Put not implemented in fake")
}

func (s *fakeBundleStore) Get(_ context.Context, h bundlestore.BundleHandle) (io.ReadCloser, error) {
	atomic.AddInt32(&s.getCalls, 1)
	if s.getErr != nil {
		return nil, s.getErr
	}
	b, ok := s.bundles[h.Key]
	if !ok {
		return nil, bundlestore.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (s *fakeBundleStore) Delete(_ context.Context, h bundlestore.BundleHandle) error {
	atomic.AddInt32(&s.delCalls, 1)
	s.lastDelKey = h.Key
	if s.deleteErr != nil {
		return s.deleteErr
	}
	delete(s.bundles, h.Key)
	return nil
}

func (s *fakeBundleStore) List(_ context.Context) ([]bundlestore.BundleHandle, error) {
	var out []bundlestore.BundleHandle
	for k := range s.bundles {
		out = append(out, bundlestore.BundleHandle{Key: k})
	}
	return out, nil
}

func (s *fakeBundleStore) Stat(_ context.Context, h bundlestore.BundleHandle) (bundlestore.BundleInfo, error) {
	b, ok := s.bundles[h.Key]
	if !ok {
		return bundlestore.BundleInfo{}, bundlestore.ErrNotFound
	}
	return bundlestore.BundleInfo{Size: int64(len(b))}, nil
}

// TestApplyApprenticeBundle_NilStore verifies the nil-BundleStore guard.
// Callers that haven't wired a BundleStore must not silently no-op — they
// need an explicit error so misconfiguration is caught at dispatch time.
func TestApplyApprenticeBundle_NilStore(t *testing.T) {
	deps := &Deps{}
	e := NewForTest("spi-test", "wizard-test", nil, deps)

	stagingWt := &spgit.StagingWorktree{}
	_, err := e.applyApprenticeBundle("spi-test", 0, stagingWt)
	if err == nil {
		t.Fatal("expected error when BundleStore is nil")
	}
	if !strings.Contains(err.Error(), "no BundleStore configured") {
		t.Errorf("err = %q, want to mention 'no BundleStore configured'", err)
	}
}

// TestApplyApprenticeBundle_NilStaging verifies the nil-staging guard.
func TestApplyApprenticeBundle_NilStaging(t *testing.T) {
	deps := &Deps{BundleStore: newFakeBundleStore()}
	e := NewForTest("spi-test", "wizard-test", nil, deps)

	_, err := e.applyApprenticeBundle("spi-test", 0, nil)
	if err == nil {
		t.Fatal("expected error when staging worktree is nil")
	}
	if !strings.Contains(err.Error(), "no staging worktree") {
		t.Errorf("err = %q, want to mention 'no staging worktree'", err)
	}
}

// TestApplyApprenticeBundle_MissingSignal verifies that a bead with no
// signal returns an error — every spawn the wizard tracks as complete is
// expected to have produced exactly one signal.
func TestApplyApprenticeBundle_MissingSignal(t *testing.T) {
	deps := &Deps{
		BundleStore: newFakeBundleStore(),
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Metadata: map[string]string{}}, nil
		},
	}
	e := NewForTest("spi-test", "wizard-test", nil, deps)

	stagingWt := &spgit.StagingWorktree{}
	_, err := e.applyApprenticeBundle("spi-test", 0, stagingWt)
	if err == nil {
		t.Fatal("expected error when signal is missing")
	}
	if !strings.Contains(err.Error(), "no apprentice signal") {
		t.Errorf("err = %q, want to mention 'no apprentice signal'", err)
	}
}

// TestApplyApprenticeBundle_NoOpSignal verifies that a no-op signal short-
// circuits with NoOp=true and Applied=false. The caller MUST skip merge.
func TestApplyApprenticeBundle_NoOpSignal(t *testing.T) {
	role := bundlestore.ApprenticeRole("spi-test", 0)
	deps := &Deps{
		BundleStore: newFakeBundleStore(),
		GetBead: func(id string) (Bead, error) {
			return Bead{
				ID: id,
				Metadata: map[string]string{
					bundlestore.SignalMetadataKey(role): `{"kind":"no-op","role":"` + role + `","submitted_at":"t"}`,
				},
			}, nil
		},
	}
	e := NewForTest("spi-test", "wizard-test", nil, deps)

	stagingWt := &spgit.StagingWorktree{}
	out, err := e.applyApprenticeBundle("spi-test", 0, stagingWt)
	if err != nil {
		t.Fatalf("err = %v, want nil for no-op", err)
	}
	if !out.NoOp {
		t.Errorf("NoOp = false, want true")
	}
	if out.Applied {
		t.Errorf("Applied = true, want false on no-op")
	}
	if out.Branch != "" {
		t.Errorf("Branch = %q, want empty", out.Branch)
	}
	if out.Handle.Key != "" {
		t.Errorf("Handle.Key = %q, want empty for no-op", out.Handle.Key)
	}
}

// TestApplyApprenticeBundle_UnexpectedKind verifies that an unknown signal
// kind (e.g. typo, future protocol revision the wizard doesn't speak) is a
// hard error rather than silently skipped.
func TestApplyApprenticeBundle_UnexpectedKind(t *testing.T) {
	role := bundlestore.ApprenticeRole("spi-test", 0)
	deps := &Deps{
		BundleStore: newFakeBundleStore(),
		GetBead: func(id string) (Bead, error) {
			return Bead{
				ID: id,
				Metadata: map[string]string{
					bundlestore.SignalMetadataKey(role): `{"kind":"weird","role":"` + role + `","submitted_at":"t"}`,
				},
			}, nil
		},
	}
	e := NewForTest("spi-test", "wizard-test", nil, deps)

	stagingWt := &spgit.StagingWorktree{}
	_, err := e.applyApprenticeBundle("spi-test", 0, stagingWt)
	if err == nil {
		t.Fatal("expected error for unexpected signal kind")
	}
	if !strings.Contains(err.Error(), "unexpected signal kind") {
		t.Errorf("err = %q, want to mention 'unexpected signal kind'", err)
	}
}

// TestApplyApprenticeBundle_EmptyBundleKey verifies that a bundle-kind
// signal with an empty bundle_key is rejected. This is a producer bug
// (apprentice should have written either no-op or a real key) — silently
// proceeding would Get a zero-key handle and produce confusing errors.
func TestApplyApprenticeBundle_EmptyBundleKey(t *testing.T) {
	role := bundlestore.ApprenticeRole("spi-test", 0)
	deps := &Deps{
		BundleStore: newFakeBundleStore(),
		GetBead: func(id string) (Bead, error) {
			return Bead{
				ID: id,
				Metadata: map[string]string{
					bundlestore.SignalMetadataKey(role): `{"kind":"bundle","role":"` + role + `","bundle_key":"","submitted_at":"t"}`,
				},
			}, nil
		},
	}
	e := NewForTest("spi-test", "wizard-test", nil, deps)

	stagingWt := &spgit.StagingWorktree{}
	_, err := e.applyApprenticeBundle("spi-test", 0, stagingWt)
	if err == nil {
		t.Fatal("expected error for empty bundle key")
	}
	if !strings.Contains(err.Error(), "empty bundle key") {
		t.Errorf("err = %q, want to mention 'empty bundle key'", err)
	}
}

// TestApplyApprenticeBundle_MalformedSignal verifies that a bead with
// invalid JSON in its signal value bubbles the parse error up. SignalForRole
// returns ok=true with an error so we know to fail rather than fall into
// "missing signal" semantics.
func TestApplyApprenticeBundle_MalformedSignal(t *testing.T) {
	role := bundlestore.ApprenticeRole("spi-test", 0)
	deps := &Deps{
		BundleStore: newFakeBundleStore(),
		GetBead: func(id string) (Bead, error) {
			return Bead{
				ID: id,
				Metadata: map[string]string{
					bundlestore.SignalMetadataKey(role): "{not valid json",
				},
			}, nil
		},
	}
	e := NewForTest("spi-test", "wizard-test", nil, deps)

	stagingWt := &spgit.StagingWorktree{}
	_, err := e.applyApprenticeBundle("spi-test", 0, stagingWt)
	if err == nil {
		t.Fatal("expected error for malformed signal")
	}
	if !strings.Contains(err.Error(), "parse apprentice signal") {
		t.Errorf("err = %q, want to mention 'parse apprentice signal'", err)
	}
}

// TestApplyApprenticeBundle_GetBeadError verifies that a GetBead failure
// is wrapped and propagated.
func TestApplyApprenticeBundle_GetBeadError(t *testing.T) {
	deps := &Deps{
		BundleStore: newFakeBundleStore(),
		GetBead: func(id string) (Bead, error) {
			return Bead{}, errors.New("dolt unreachable")
		},
	}
	e := NewForTest("spi-test", "wizard-test", nil, deps)

	stagingWt := &spgit.StagingWorktree{}
	_, err := e.applyApprenticeBundle("spi-test", 0, stagingWt)
	if err == nil {
		t.Fatal("expected error when GetBead fails")
	}
	if !strings.Contains(err.Error(), "get bead") {
		t.Errorf("err = %q, want to mention 'get bead'", err)
	}
}

// TestApplyApprenticeBundle_Success exercises the happy path end-to-end:
// real git bundle, real local-store, real worktree. The bundle is applied
// to a fresh branch, Handle is returned with the correct Key, and Delete
// is NOT called (caller's responsibility post-merge — this is the spec the
// review feedback enforced).
func TestApplyApprenticeBundle_Success(t *testing.T) {
	repoDir, baseSHA := initBundleTestRepo(t)
	bundlePath, _ := buildTestBundle(t, repoDir, baseSHA)

	store := newFakeBundleStore()
	bundleBytes, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}
	const bundleKey = "spi-test/spi-att-0.bundle"
	store.bundles[bundleKey] = bundleBytes

	role := bundlestore.ApprenticeRole("spi-test", 0)
	signalJSON := fmt.Sprintf(
		`{"kind":"bundle","role":%q,"bundle_key":%q,"commits":["sha1"],"submitted_at":"t"}`,
		role, bundleKey,
	)

	deps := &Deps{
		BundleStore: store,
		GetBead: func(id string) (Bead, error) {
			return Bead{
				ID: id,
				Metadata: map[string]string{
					bundlestore.SignalMetadataKey(role): signalJSON,
				},
			}, nil
		},
		ResolveBranch: func(beadID string) string { return "feat/" + beadID },
	}
	e := NewForTest("spi-test", "wizard-test", nil, deps)

	// Use the repo dir as the staging worktree's Dir — ApplyBundle calls
	// `git -C <Dir> fetch` against it.
	stagingWt := &spgit.StagingWorktree{
		WorktreeContext: spgit.WorktreeContext{Dir: repoDir, RepoPath: repoDir},
	}

	out, err := e.applyApprenticeBundle("spi-test", 0, stagingWt)
	if err != nil {
		t.Fatalf("applyApprenticeBundle: %v", err)
	}
	if !out.Applied {
		t.Errorf("Applied = false, want true")
	}
	if out.NoOp {
		t.Errorf("NoOp = true, want false")
	}
	if out.Branch != "feat/spi-test" {
		t.Errorf("Branch = %q, want feat/spi-test", out.Branch)
	}
	if out.Handle.Key != bundleKey {
		t.Errorf("Handle.Key = %q, want %q", out.Handle.Key, bundleKey)
	}
	if out.Handle.BeadID != "spi-test" {
		t.Errorf("Handle.BeadID = %q, want spi-test", out.Handle.BeadID)
	}

	// CRITICAL: Delete must NOT have been called. The whole point of
	// returning the handle is that the caller deletes only after a
	// successful merge. Deleting here would leave the wizard with no way
	// to retry on conflict.
	if got := atomic.LoadInt32(&store.delCalls); got != 0 {
		t.Errorf("BundleStore.Delete was called %d times during apply — must wait for caller post-merge", got)
	}

	// Verify the branch ref now exists locally pointing at the bundle's
	// HEAD. This proves ApplyBundle actually ran end-to-end.
	out2, err := exec.Command("git", "-C", repoDir, "rev-parse", "feat/spi-test").Output()
	if err != nil {
		t.Fatalf("rev-parse feat/spi-test: %v", err)
	}
	if strings.TrimSpace(string(out2)) == "" {
		t.Error("feat/spi-test has no SHA after ApplyBundle")
	}
}

// TestDeleteApprenticeBundle_NilStore verifies the helper is a no-op when
// no BundleStore is wired.
func TestDeleteApprenticeBundle_NilStore(t *testing.T) {
	deps := &Deps{}
	e := NewForTest("spi-test", "wizard-test", nil, deps)
	// Should not panic.
	e.deleteApprenticeBundle("spi-test", bundlestore.BundleHandle{Key: "k"})
}

// TestDeleteApprenticeBundle_EmptyKey verifies the helper is a no-op for
// empty-Key handles (the no-op signal path).
func TestDeleteApprenticeBundle_EmptyKey(t *testing.T) {
	store := newFakeBundleStore()
	deps := &Deps{BundleStore: store}
	e := NewForTest("spi-test", "wizard-test", nil, deps)
	e.deleteApprenticeBundle("spi-test", bundlestore.BundleHandle{Key: ""})
	if got := atomic.LoadInt32(&store.delCalls); got != 0 {
		t.Errorf("Delete was called for empty-Key handle")
	}
}

// TestDeleteApprenticeBundle_Success verifies the helper forwards the
// handle to BundleStore.Delete on the happy path.
func TestDeleteApprenticeBundle_Success(t *testing.T) {
	store := newFakeBundleStore()
	store.bundles["k"] = []byte("x")
	deps := &Deps{BundleStore: store}
	e := NewForTest("spi-test", "wizard-test", nil, deps)
	e.deleteApprenticeBundle("spi-test", bundlestore.BundleHandle{BeadID: "spi-test", Key: "k"})
	if got := atomic.LoadInt32(&store.delCalls); got != 1 {
		t.Errorf("Delete called %d times, want 1", got)
	}
	if store.lastDelKey != "k" {
		t.Errorf("lastDelKey = %q, want k", store.lastDelKey)
	}
}

// TestDeleteApprenticeBundle_DeleteErrorSwallowed verifies that a Delete
// failure is logged but not surfaced — the bundle janitor is the
// correctness net, not this code path.
func TestDeleteApprenticeBundle_DeleteErrorSwallowed(t *testing.T) {
	store := newFakeBundleStore()
	store.deleteErr = errors.New("transient s3 error")
	deps := &Deps{BundleStore: store}
	e := NewForTest("spi-test", "wizard-test", nil, deps)
	// Should not panic and should not return — function signature is void.
	e.deleteApprenticeBundle("spi-test", bundlestore.BundleHandle{Key: "k"})
}

// initBundleTestRepo creates a real git repo with one commit on main and
// returns (dir, baseSHA). Used as the substrate for end-to-end bundle apply.
func initBundleTestRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init", "-q")
	runGit("config", "user.name", "Test")
	runGit("config", "user.email", "t@t.com")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# init\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit("add", "-A")
	runGit("commit", "-q", "-m", "initial")
	runGit("branch", "-M", "main")
	sha := runGit("rev-parse", "HEAD")
	return dir, sha
}

// buildTestBundle creates a feature commit on top of baseSHA in repoDir,
// builds a git bundle covering that commit, and returns (bundle path, head SHA).
// The repo is left at baseSHA so the bundle can be applied as a fresh branch.
func buildTestBundle(t *testing.T, repoDir, baseSHA string) (string, string) {
	t.Helper()
	runGit := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("checkout", "-q", "-b", "build-tmp")
	if err := os.WriteFile(filepath.Join(repoDir, "feature.txt"), []byte("feat\n"), 0o644); err != nil {
		t.Fatalf("write feature: %v", err)
	}
	runGit("add", "-A")
	runGit("commit", "-q", "-m", "feat")
	headSHA := runGit("rev-parse", "HEAD")

	bundlePath := filepath.Join(t.TempDir(), "feat.bundle")
	runGit("bundle", "create", bundlePath, baseSHA+"..HEAD")

	// Reset the repo back to baseSHA on main so the bundle can be applied
	// as a fresh feat/ branch in the same dir.
	runGit("checkout", "-q", "main")
	runGit("branch", "-D", "build-tmp")
	return bundlePath, headSHA
}
