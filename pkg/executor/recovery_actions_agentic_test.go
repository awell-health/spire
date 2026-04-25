package executor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/agent"
	spgit "github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
)

// ---------------------------------------------------------------------------
// testPackagesFor
// ---------------------------------------------------------------------------

func TestTestPackagesFor(t *testing.T) {
	tests := []struct {
		name  string
		files []string
		want  []string
	}{
		{
			name:  "empty input",
			files: nil,
			want:  nil,
		},
		{
			name:  "only non-test files",
			files: []string{"pkg/foo/foo.go", "cmd/spire/main.go"},
			want:  nil,
		},
		{
			name:  "single test file",
			files: []string{"pkg/foo/foo_test.go"},
			want:  []string{"./pkg/foo/..."},
		},
		{
			name:  "multiple test files in same package dedupe",
			files: []string{"pkg/foo/a_test.go", "pkg/foo/b_test.go"},
			want:  []string{"./pkg/foo/..."},
		},
		{
			name:  "multiple test files across packages",
			files: []string{"pkg/foo/a_test.go", "pkg/bar/b_test.go"},
			want:  []string{"./pkg/foo/...", "./pkg/bar/..."},
		},
		{
			name:  "mix of test and non-test",
			files: []string{"pkg/foo/foo.go", "pkg/foo/foo_test.go", "README.md"},
			want:  []string{"./pkg/foo/..."},
		},
		{
			name:  "test file at repo root",
			files: []string{"root_test.go"},
			want:  []string{"././..."},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := testPackagesFor(tt.files)
			if len(got) != len(tt.want) {
				t.Fatalf("testPackagesFor = %v, want %v", got, tt.want)
			}
			// Compare as sets — order reflects input order but we only care
			// about the unique set.
			gotSet := map[string]bool{}
			for _, g := range got {
				gotSet[g] = true
			}
			for _, w := range tt.want {
				if !gotSet[w] {
					t.Errorf("testPackagesFor missing %q; got %v", w, got)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// containsConflictMarker
// ---------------------------------------------------------------------------

func TestContainsConflictMarker(t *testing.T) {
	tests := []struct {
		name string
		data string
		want bool
	}{
		{"clean", "line1\nline2\n", false},
		{"leading-marker", "<<<<<<< HEAD\nfoo\n=======\nbar\n>>>>>>> branch\n", true},
		{"embedded-marker", "prefix\n<<<<<<< HEAD\nfoo\n=======\nbar\n>>>>>>> branch\nsuffix\n", true},
		// The `=======` marker must be on its own line (per the sentinel defn).
		{"equals-inside-text", "some ======= text inline\n", false},
		{"separator-line-only", "a\n=======\nb\n", true},
		// Without the trailing space the `<<<<<<<` substring shouldn't match.
		{"lt-less-than-seven", "<<<<<< too few\n", false},
		{"empty", "", false},
		{"close-marker-trailing-space", "hello >>>>>>> foo\n", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := containsConflictMarker([]byte(tt.data))
			if got != tt.want {
				t.Errorf("containsConflictMarker(%q) = %v, want %v", tt.data, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// indentBlock
// ---------------------------------------------------------------------------

func TestIndentBlock(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		prefix string
		want   string
	}{
		{"empty string", "", "> ", "> \n"},
		{"single line", "hello", "> ", "> hello\n"},
		{"multi-line", "line1\nline2", "> ", "> line1\n> line2\n"},
		{"trims trailing newlines", "hello\n\n", "> ", "> hello\n"},
		{"non-empty prefix", "a\nb", "  ", "  a\n  b\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := indentBlock(tt.input, tt.prefix)
			if got != tt.want {
				t.Errorf("indentBlock(%q, %q) = %q, want %q", tt.input, tt.prefix, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// renderConflictPrompt
// ---------------------------------------------------------------------------

// TestRenderConflictPrompt_FullBundle verifies the prompt renders all the
// parts the apprentice relies on: worktree path, operation, both sides'
// commits + beads, each conflict file with contents + history.
func TestRenderConflictPrompt_FullBundle(t *testing.T) {
	ctx := &RecoveryActionCtx{
		Worktree: &spgit.WorktreeContext{Dir: "/tmp/wt-spi-abc12"},
	}
	headBead := &store.Bead{ID: "spi-head", Title: "Head-side work", Status: "in_progress", Description: "head bead desc"}
	incomingBead := &store.Bead{ID: "spi-in", Title: "Incoming-side work", Status: "closed", Description: "incoming bead desc"}
	bundle := conflictBundle{
		State: spgit.ConflictState{InProgressOp: "rebase", HeadSHA: "aaa111", IncomingSHA: "bbb222"},
		HeadSide: &conflictSideContext{
			Label: "HEAD", Operation: "rebase",
			Commit: &spgit.CommitMetadata{SHA: "aaa111", Subject: "feat(spi-head): head change", Author: "A <a@x>", Date: "2026-01-02T00:00:00Z", Body: "head body"},
			BeadID: "spi-head", Bead: headBead,
		},
		IncomingSide: &conflictSideContext{
			Label: "incoming (rebase)", Operation: "rebase",
			Commit: &spgit.CommitMetadata{SHA: "bbb222", Subject: "feat(spi-in): incoming change", Author: "B <b@x>", Date: "2026-01-03T00:00:00Z"},
			BeadID: "spi-in", Bead: incomingBead,
		},
		Files: []conflictFileContext{
			{Path: "pkg/foo/foo.go", Content: "line1\n<<<<<<< HEAD\nleft\n=======\nright\n>>>>>>> branch\nline2\n", Log: "commit abc\nAuthorDate: 2026\n    first change"},
			{Path: "pkg/foo/bar.go", Content: "ok\n"},
		},
	}

	out := renderConflictPrompt(ctx, bundle)

	mustContain := []string{
		"conflict-resolution apprentice",
		"/tmp/wt-spi-abc12",
		"## In-progress operation\nrebase",
		"## HEAD side",
		"aaa111",
		"feat(spi-head): head change",
		"spi-head",
		"Head-side work",
		"head bead desc",
		"head body",
		"## Incoming side",
		"bbb222",
		"feat(spi-in): incoming change",
		"Incoming-side work",
		"incoming bead desc",
		"## Conflicted files",
		"### pkg/foo/foo.go",
		"<<<<<<< HEAD",
		"=======",
		">>>>>>> branch",
		"### pkg/foo/bar.go",
		"AuthorDate: 2026",
		"Resolve the conflicts now.",
	}
	for _, s := range mustContain {
		if !strings.Contains(out, s) {
			t.Errorf("prompt missing %q; full prompt:\n%s", s, out)
		}
	}
}

// TestRenderConflictPrompt_EmptyBundle — nil sides and no files still produces
// a prompt that names the worktree and the "none" signals.
func TestRenderConflictPrompt_EmptyBundle(t *testing.T) {
	ctx := &RecoveryActionCtx{
		Worktree: &spgit.WorktreeContext{Dir: "/tmp/wt-empty"},
	}
	out := renderConflictPrompt(ctx, conflictBundle{})
	for _, s := range []string{
		"/tmp/wt-empty",
		"## In-progress operation\nnone detected",
		"*commit metadata unavailable*",
		"(none — if you see this, the conflict may already be resolved)",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("empty-bundle prompt missing %q; full prompt:\n%s", s, out)
		}
	}
}

// ---------------------------------------------------------------------------
// buildConflictBundle — uses a real git repo (worktree) so ShowCommit / FileLog
// resolve actual data, and GetBeadFn is mocked.
// ---------------------------------------------------------------------------

func TestBuildConflictBundle_WithMockedBeadStore(t *testing.T) {
	dir := initAgenticTestRepo(t)

	// Two divergent commits that touch the same file — create a real paused
	// rebase so DetectConflictState picks up op=rebase and HEAD/incoming SHAs.
	path := filepath.Join(dir, "shared.go")
	writeAgenticFile(t, path, "package shared\n\nfunc F() int { return 0 }\n")
	mustRun(t, dir, "git", "add", "-A")
	mustRun(t, dir, "git", "commit", "-m", "base")

	mustRun(t, dir, "git", "checkout", "-b", "feat/branch-side")
	writeAgenticFile(t, path, "package shared\n\nfunc F() int { return 1 }\n")
	mustRun(t, dir, "git", "add", "-A")
	mustRun(t, dir, "git", "commit", "-m", "feat(spi-inc01): branch change")
	branchSHA := strings.TrimSpace(mustRun(t, dir, "git", "rev-parse", "HEAD"))

	mustRun(t, dir, "git", "checkout", "main")
	writeAgenticFile(t, path, "package shared\n\nfunc F() int { return 2 }\n")
	mustRun(t, dir, "git", "add", "-A")
	mustRun(t, dir, "git", "commit", "-m", "feat(spi-head1): main change")
	mainSHA := strings.TrimSpace(mustRun(t, dir, "git", "rev-parse", "HEAD"))

	// Trigger rebase pause.
	mustRun(t, dir, "git", "checkout", "feat/branch-side")
	allowFail(t, dir, "git", "rebase", "main")

	wc := &spgit.WorktreeContext{Dir: dir, RepoPath: dir, Branch: "feat/branch-side", BaseBranch: "main"}

	var beadLookups []string
	ctx := &RecoveryActionCtx{
		Worktree:     wc,
		TargetBeadID: "spi-target",
		GetBeadFn: func(id string) (store.Bead, error) {
			beadLookups = append(beadLookups, id)
			switch id {
			case "spi-head1":
				return store.Bead{ID: "spi-head1", Title: "Main-side change", Status: "in_progress", Description: "main desc"}, nil
			case "spi-inc01":
				return store.Bead{ID: "spi-inc01", Title: "Branch-side change", Status: "open", Description: "branch desc"}, nil
			}
			return store.Bead{}, fmt.Errorf("not found: %s", id)
		},
	}

	files, err := wc.ConflictedFiles()
	if err != nil {
		t.Fatalf("ConflictedFiles: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("expected at least one conflicted file")
	}

	bundle := buildConflictBundle(ctx, wc, files)

	// State.
	if bundle.State.InProgressOp != "rebase" {
		t.Errorf("InProgressOp = %q, want rebase", bundle.State.InProgressOp)
	}
	if bundle.State.HeadSHA == "" || bundle.State.IncomingSHA == "" {
		t.Errorf("State SHAs empty: %+v", bundle.State)
	}

	// Head/Incoming side context populated with bead lookups.
	if bundle.HeadSide == nil || bundle.IncomingSide == nil {
		t.Fatal("expected both HeadSide and IncomingSide populated")
	}
	// In a rebase, HEAD is the mainSHA, incoming is the branchSHA.
	if bundle.HeadSide.Commit == nil || bundle.HeadSide.Commit.SHA != mainSHA {
		t.Errorf("HeadSide SHA = %v, want %q", bundle.HeadSide.Commit, mainSHA)
	}
	if bundle.IncomingSide.Commit == nil || bundle.IncomingSide.Commit.SHA != branchSHA {
		t.Errorf("IncomingSide SHA = %v, want %q", bundle.IncomingSide.Commit, branchSHA)
	}

	// Bead lookups hit both IDs via the mock.
	if bundle.HeadSide.BeadID != "spi-head1" || bundle.HeadSide.Bead == nil {
		t.Errorf("HeadSide bead not resolved: id=%q bead=%v", bundle.HeadSide.BeadID, bundle.HeadSide.Bead)
	}
	if bundle.IncomingSide.BeadID != "spi-inc01" || bundle.IncomingSide.Bead == nil {
		t.Errorf("IncomingSide bead not resolved: id=%q bead=%v", bundle.IncomingSide.BeadID, bundle.IncomingSide.Bead)
	}
	if bundle.IncomingSide.Bead != nil && bundle.IncomingSide.Bead.Title != "Branch-side change" {
		t.Errorf("IncomingSide.Bead.Title = %q, want 'Branch-side change'", bundle.IncomingSide.Bead.Title)
	}

	// GetBeadFn was called at least for each side's extracted bead.
	seen := map[string]bool{}
	for _, id := range beadLookups {
		seen[id] = true
	}
	if !seen["spi-head1"] || !seen["spi-inc01"] {
		t.Errorf("GetBeadFn lookups = %v, want both spi-head1 and spi-inc01", beadLookups)
	}

	// File content includes the conflict markers.
	if len(bundle.Files) == 0 {
		t.Fatal("no files in bundle")
	}
	found := false
	for _, fc := range bundle.Files {
		if strings.Contains(fc.Content, "<<<<<<<") {
			found = true
		}
	}
	if !found {
		t.Error("expected at least one file content to include conflict markers")
	}
}

// TestBuildConflictBundle_BeadLookupMiss verifies that when GetBeadFn returns
// an error, the bundle still populates Commit metadata on the side — only
// Bead is left nil.
func TestBuildConflictBundle_BeadLookupMiss(t *testing.T) {
	dir := initAgenticTestRepo(t)
	path := filepath.Join(dir, "x.txt")
	writeAgenticFile(t, path, "base\n")
	mustRun(t, dir, "git", "add", "-A")
	mustRun(t, dir, "git", "commit", "-m", "base")

	mustRun(t, dir, "git", "checkout", "-b", "feat/side")
	writeAgenticFile(t, path, "branch\n")
	mustRun(t, dir, "git", "add", "-A")
	mustRun(t, dir, "git", "commit", "-m", "feat(spi-nope1): change")

	mustRun(t, dir, "git", "checkout", "main")
	writeAgenticFile(t, path, "main\n")
	mustRun(t, dir, "git", "add", "-A")
	mustRun(t, dir, "git", "commit", "-m", "feat(spi-miss1): change")
	allowFail(t, dir, "git", "merge", "feat/side")

	wc := &spgit.WorktreeContext{Dir: dir, RepoPath: dir, Branch: "main", BaseBranch: "main"}
	ctx := &RecoveryActionCtx{
		Worktree: wc,
		GetBeadFn: func(id string) (store.Bead, error) {
			return store.Bead{}, fmt.Errorf("not found")
		},
	}

	files, _ := wc.ConflictedFiles()
	bundle := buildConflictBundle(ctx, wc, files)

	if bundle.HeadSide == nil || bundle.HeadSide.Commit == nil {
		t.Fatal("expected HeadSide commit populated even when bead lookup fails")
	}
	if bundle.HeadSide.Bead != nil {
		t.Errorf("expected nil HeadSide.Bead on lookup miss, got %+v", bundle.HeadSide.Bead)
	}
	if bundle.IncomingSide == nil || bundle.IncomingSide.Commit == nil {
		t.Fatal("expected IncomingSide commit populated even when bead lookup fails")
	}
	if bundle.IncomingSide.Bead != nil {
		t.Errorf("expected nil IncomingSide.Bead on lookup miss, got %+v", bundle.IncomingSide.Bead)
	}
}

// ---------------------------------------------------------------------------
// SpawnRepairWorker — validates the top-level orchestration via DispatchFn
// + GetBeadFn hooks. The function is the canonical RepairModeWorker
// entrypoint; these tests exercise the conflict-resolution worker shape.
// ---------------------------------------------------------------------------

// fakeHandle implements agent.Handle for tests.
type fakeHandle struct {
	name   string
	wait   error
	waitFn func() error
}

func (h *fakeHandle) Wait() error {
	if h.waitFn != nil {
		return h.waitFn()
	}
	return h.wait
}
func (h *fakeHandle) Signal(os.Signal) error { return nil }
func (h *fakeHandle) Alive() bool            { return false }
func (h *fakeHandle) Name() string           { return h.name }
func (h *fakeHandle) Identifier() string     { return h.name }

// fakeSpawner implements a non-nil agent.Backend for the guard in
// dispatchConflictApprentice. Spawn is unused because DispatchFn overrides.
type fakeSpawner struct{}

func (fakeSpawner) Spawn(cfg agent.SpawnConfig) (agent.Handle, error) {
	return nil, errors.New("not used")
}
func (fakeSpawner) List() ([]agent.Info, error)        { return nil, nil }
func (fakeSpawner) Logs(string) (io.ReadCloser, error) { return nil, os.ErrNotExist }
func (fakeSpawner) Kill(string) error                  { return nil }

// testBuildRuntimeContract is a minimal BuildRuntimeContract stub that
// mirrors the shape (*Executor).withRuntimeContract produces — Identity
// (TowerName, Prefix, RepoURL, BaseBranch), Workspace, Run (Backend,
// FormulaStep, WorkspaceKind/Name/Origin, HandoffMode). Tests that
// bypass the real Executor use it so the substrate-contract fields that
// k8s validation enforces are present in the captured cfg. The stub
// intentionally uses a fixed RepoURL so the k8s-contract assertion in
// TestSpawnRepairWorker_BuildsCanonicalRuntimeContract can pin the
// value.
func testBuildRuntimeContract(cfg agent.SpawnConfig, step, workspaceName string, ws WorkspaceHandle, mode HandoffMode) (agent.SpawnConfig, error) {
	prefix := store.PrefixFromID(cfg.BeadID)
	if prefix == "" {
		prefix = "spi"
	}
	if ws.Name == "" {
		ws.Name = workspaceName
	}
	if ws.BaseBranch == "" {
		ws.BaseBranch = "main"
	}
	cfg.Identity = RepoIdentity{
		TowerName:  "test-tower",
		TowerID:    "test-tower",
		Prefix:     prefix,
		RepoURL:    "https://example.com/test.git",
		BaseBranch: ws.BaseBranch,
	}
	cfg.Workspace = &ws
	cfg.Run = RunContext{
		TowerName:       "test-tower",
		Prefix:          prefix,
		BeadID:          cfg.BeadID,
		Role:            cfg.Role,
		FormulaStep:     step,
		Backend:         "k8s",
		WorkspaceKind:   ws.Kind,
		WorkspaceName:   ws.Name,
		WorkspaceOrigin: ws.Origin,
		HandoffMode:     mode,
	}
	return cfg, nil
}

// TestSpawnRepairWorker_ResolveConflictsNoConflictsErrors verifies that
// resolve-conflicts with zero conflict markers on disk is a
// decide/execute vocabulary mismatch — the pre-spi-6wiz9 blanket "no
// conflicts → success" short-circuit is gone.
func TestSpawnRepairWorker_ResolveConflictsNoConflictsErrors(t *testing.T) {
	dir := initAgenticTestRepo(t)
	wc := &spgit.WorktreeContext{Dir: dir, RepoPath: dir, Branch: "main", BaseBranch: "main"}

	ctx := &RecoveryActionCtx{
		Worktree: wc,
		Log:      func(string) {},
	}

	_, err := SpawnRepairWorker(ctx, recovery.RepairPlan{Action: "resolve-conflicts"}, WorkspaceHandle{Path: dir})
	if err == nil {
		t.Fatal("expected error when resolve-conflicts runs against a clean worktree")
	}
	if !strings.Contains(err.Error(), "no conflicted files") {
		t.Errorf("error = %q, want to mention 'no conflicted files'", err)
	}
}

// TestSpawnRepairWorker_EmptyActionErrors verifies that an empty
// plan.Action errors rather than silently succeeding. Pre-spi-6wiz9 the
// worker path returned success when there were no conflicts, regardless
// of action — that short-circuit is gone; decide must set a canonical
// action for every worker plan.
func TestSpawnRepairWorker_EmptyActionErrors(t *testing.T) {
	dir := initAgenticTestRepo(t)
	wc := &spgit.WorktreeContext{Dir: dir, RepoPath: dir, Branch: "main", BaseBranch: "main"}

	ctx := &RecoveryActionCtx{
		Worktree: wc,
		Log:      func(string) {},
	}
	_, err := SpawnRepairWorker(ctx, recovery.RepairPlan{}, WorkspaceHandle{Path: dir})
	if err == nil {
		t.Fatal("expected error on empty plan.Action")
	}
	if !strings.Contains(err.Error(), "plan.Action is empty") {
		t.Errorf("error = %q, want to mention 'plan.Action is empty'", err)
	}
}

// TestSpawnRepairWorker_UnknownActionErrors verifies that an action
// outside the closed worker-role set fails loudly. Decide/execute
// vocabulary mismatches must surface rather than dispatch into an
// ambiguous prompt.
func TestSpawnRepairWorker_UnknownActionErrors(t *testing.T) {
	dir := initAgenticTestRepo(t)
	wc := &spgit.WorktreeContext{Dir: dir, RepoPath: dir, Branch: "main", BaseBranch: "main"}

	ctx := &RecoveryActionCtx{
		Worktree: wc,
		Log:      func(string) {},
	}
	_, err := SpawnRepairWorker(ctx, recovery.RepairPlan{Action: "invent-action"}, WorkspaceHandle{Path: dir})
	if err == nil {
		t.Fatal("expected error on unknown plan.Action")
	}
	if !strings.Contains(err.Error(), "unsupported action") {
		t.Errorf("error = %q, want to mention 'unsupported action'", err)
	}
}

// TestSpawnRepairWorker_NilWorkspace returns an error when neither
// ctx.Worktree nor the passed WorkspaceHandle carries a path.
func TestSpawnRepairWorker_NilWorkspace(t *testing.T) {
	ctx := &RecoveryActionCtx{Log: func(string) {}}
	_, err := SpawnRepairWorker(ctx, recovery.RepairPlan{}, WorkspaceHandle{})
	if err == nil {
		t.Fatal("expected error with no workspace")
	}
	if !strings.Contains(err.Error(), "no workspace") {
		t.Errorf("error = %q, want to mention 'no workspace'", err)
	}
}

// TestSpawnRepairWorker_DispatchSucceedsAndGatesPass sets up a real
// paused rebase, injects a DispatchFn that simulates the apprentice by
// writing a merged file + running `git rebase --continue`, and asserts the
// top-level flow returns nil (all gates pass).
func TestSpawnRepairWorker_DispatchSucceedsAndGatesPass(t *testing.T) {
	dir := initAgenticTestRepo(t)

	// Trivial go.mod so `go build ./...` succeeds — the gate will run it.
	writeAgenticFile(t, filepath.Join(dir, "go.mod"), "module agentic_test\n\ngo 1.20\n")
	writeAgenticFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc main() {}\n")
	mustRun(t, dir, "git", "add", "-A")
	mustRun(t, dir, "git", "commit", "-m", "scaffold")

	// Conflict scaffold.
	path := filepath.Join(dir, "shared.txt")
	writeAgenticFile(t, path, "base\n")
	mustRun(t, dir, "git", "add", "-A")
	mustRun(t, dir, "git", "commit", "-m", "base data")

	mustRun(t, dir, "git", "checkout", "-b", "feat/side")
	writeAgenticFile(t, path, "branch\n")
	mustRun(t, dir, "git", "add", "-A")
	mustRun(t, dir, "git", "commit", "-m", "feat(spi-brn01): branch")

	mustRun(t, dir, "git", "checkout", "main")
	writeAgenticFile(t, path, "main\n")
	mustRun(t, dir, "git", "add", "-A")
	mustRun(t, dir, "git", "commit", "-m", "feat(spi-mn001): main")

	mustRun(t, dir, "git", "checkout", "feat/side")
	allowFail(t, dir, "git", "rebase", "main")

	// Sanity: rebase paused with a conflict.
	wc := &spgit.WorktreeContext{Dir: dir, RepoPath: dir, Branch: "feat/side", BaseBranch: "main"}
	if st := wc.DetectConflictState(); st.InProgressOp != "rebase" {
		t.Fatalf("expected paused rebase, got %+v", st)
	}

	// DispatchFn simulates the apprentice: write a merged file and continue
	// the rebase so gates see clean markers + ff-able HEAD.
	var dispatched int
	dispatch := func(cfg agent.SpawnConfig) (agent.Handle, error) {
		dispatched++
		// Write merged content combining both sides, staging it.
		if err := os.WriteFile(path, []byte("main+branch\n"), 0644); err != nil {
			return nil, err
		}
		if out, err := runCmd(dir, "git", "add", "-A"); err != nil {
			return nil, fmt.Errorf("git add: %v\n%s", err, out)
		}
		// GIT_EDITOR=true skips the commit-message editor during rebase-continue.
		cmd := makeCmd(dir, "git", "-c", "core.editor=true", "rebase", "--continue")
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("rebase --continue: %v\n%s", err, out)
		}
		return &fakeHandle{name: cfg.Name}, nil
	}

	ctx := &RecoveryActionCtx{
		Worktree:             wc,
		TargetBeadID:         "spi-target",
		Spawner:              fakeSpawner{},
		DispatchFn:           dispatch,
		BuildRuntimeContract: testBuildRuntimeContract,
		Log:                  func(string) {},
	}

	if _, err := SpawnRepairWorker(ctx, recovery.RepairPlan{Action: "resolve-conflicts"}, WorkspaceHandle{Path: dir}); err != nil {
		t.Fatalf("SpawnRepairWorker: %v", err)
	}
	if dispatched != 1 {
		t.Errorf("dispatched = %d, want 1", dispatched)
	}
	// State should no longer report rebase-in-progress.
	if st := wc.DetectConflictState(); st.InProgressOp != "" {
		t.Errorf("expected no in-progress op after resolve, got %+v", st)
	}
}

// TestSpawnRepairWorker_GateFailsWhenMarkersRemain verifies the validation
// gate catches an apprentice that claimed success but left conflict markers.
func TestSpawnRepairWorker_GateFailsWhenMarkersRemain(t *testing.T) {
	dir := initAgenticTestRepo(t)

	path := filepath.Join(dir, "data.txt")
	writeAgenticFile(t, path, "base\n")
	mustRun(t, dir, "git", "add", "-A")
	mustRun(t, dir, "git", "commit", "-m", "base")

	mustRun(t, dir, "git", "checkout", "-b", "feat/dup")
	writeAgenticFile(t, path, "branch\n")
	mustRun(t, dir, "git", "add", "-A")
	mustRun(t, dir, "git", "commit", "-m", "feat(spi-brn02): branch")

	mustRun(t, dir, "git", "checkout", "main")
	writeAgenticFile(t, path, "main\n")
	mustRun(t, dir, "git", "add", "-A")
	mustRun(t, dir, "git", "commit", "-m", "feat(spi-mn002): main")

	mustRun(t, dir, "git", "checkout", "feat/dup")
	allowFail(t, dir, "git", "rebase", "main")

	wc := &spgit.WorktreeContext{Dir: dir, RepoPath: dir, Branch: "feat/dup", BaseBranch: "main"}

	// Dispatch leaves markers on disk.
	dispatch := func(cfg agent.SpawnConfig) (agent.Handle, error) {
		return &fakeHandle{name: cfg.Name}, nil
	}
	ctx := &RecoveryActionCtx{
		Worktree:             wc,
		TargetBeadID:         "spi-target",
		Spawner:              fakeSpawner{},
		DispatchFn:           dispatch,
		BuildRuntimeContract: testBuildRuntimeContract,
		Log:                  func(string) {},
	}

	_, err := SpawnRepairWorker(ctx, recovery.RepairPlan{Action: "resolve-conflicts"}, WorkspaceHandle{Path: dir})
	if err == nil {
		t.Fatal("expected gate to fail on unresolved markers")
	}
	if !strings.Contains(err.Error(), "conflict markers remain") {
		t.Errorf("error = %q, want to mention 'conflict markers remain'", err)
	}
}

// TestSpawnRepairWorker_DispatchSpawnError surfaces the error.
func TestSpawnRepairWorker_DispatchSpawnError(t *testing.T) {
	dir := initAgenticTestRepo(t)

	path := filepath.Join(dir, "d.txt")
	writeAgenticFile(t, path, "base\n")
	mustRun(t, dir, "git", "add", "-A")
	mustRun(t, dir, "git", "commit", "-m", "base")

	mustRun(t, dir, "git", "checkout", "-b", "feat/err-side")
	writeAgenticFile(t, path, "branch\n")
	mustRun(t, dir, "git", "add", "-A")
	mustRun(t, dir, "git", "commit", "-m", "feat(spi-err01): branch")

	mustRun(t, dir, "git", "checkout", "main")
	writeAgenticFile(t, path, "main\n")
	mustRun(t, dir, "git", "add", "-A")
	mustRun(t, dir, "git", "commit", "-m", "feat(spi-err02): main")

	mustRun(t, dir, "git", "checkout", "feat/err-side")
	allowFail(t, dir, "git", "rebase", "main")

	wc := &spgit.WorktreeContext{Dir: dir, RepoPath: dir, Branch: "feat/err-side", BaseBranch: "main"}

	dispatch := func(cfg agent.SpawnConfig) (agent.Handle, error) {
		return nil, errors.New("boom: spawner broken")
	}
	ctx := &RecoveryActionCtx{
		Worktree:             wc,
		TargetBeadID:         "spi-target",
		Spawner:              fakeSpawner{},
		DispatchFn:           dispatch,
		BuildRuntimeContract: testBuildRuntimeContract,
		Log:                  func(string) {},
	}

	_, err := SpawnRepairWorker(ctx, recovery.RepairPlan{Action: "resolve-conflicts"}, WorkspaceHandle{Path: dir})
	if err == nil {
		t.Fatal("expected error when dispatch fails to spawn")
	}
	if !strings.Contains(err.Error(), "spawn") {
		t.Errorf("error = %q, want to mention 'spawn'", err)
	}
}

// TestDispatchConflictApprentice_NoSpawner returns an error without panicking.
func TestDispatchConflictApprentice_NoSpawner(t *testing.T) {
	ctx := &RecoveryActionCtx{
		Worktree: &spgit.WorktreeContext{Dir: "/tmp/x"},
		Log:      func(string) {},
	}
	_, err := dispatchConflictApprentice(ctx, conflictBundle{}, WorkspaceHandle{Path: "/tmp/x"})
	if err == nil {
		t.Fatal("expected error with no Spawner")
	}
	if !strings.Contains(err.Error(), "Spawner") {
		t.Errorf("error = %q, want to mention Spawner", err)
	}
}

// TestDispatchConflictApprentice_NoWorktree returns an error.
func TestDispatchConflictApprentice_NoWorktree(t *testing.T) {
	ctx := &RecoveryActionCtx{
		Spawner: fakeSpawner{},
		Log:     func(string) {},
	}
	_, err := dispatchConflictApprentice(ctx, conflictBundle{}, WorkspaceHandle{})
	if err == nil {
		t.Fatal("expected error with no worktree")
	}
	if !strings.Contains(err.Error(), "worktree") {
		t.Errorf("error = %q, want to mention worktree", err)
	}
}

// TestDispatchConflictApprentice_WaitErrorNonFatal verifies that a non-nil
// wait error from the apprentice is logged but does NOT cause dispatch to
// return an error — the validation gates are the authoritative check.
func TestDispatchConflictApprentice_WaitErrorNonFatal(t *testing.T) {
	dir := t.TempDir()
	wc := &spgit.WorktreeContext{Dir: dir}
	ctx := &RecoveryActionCtx{
		Worktree:     wc,
		TargetBeadID: "spi-target",
		Spawner:      fakeSpawner{},
		DispatchFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
			return &fakeHandle{name: cfg.Name, wait: errors.New("non-zero exit")}, nil
		},
		BuildRuntimeContract: testBuildRuntimeContract,
		Log:                  func(string) {},
	}

	_, err := dispatchConflictApprentice(ctx, conflictBundle{}, WorkspaceHandle{Path: dir})
	if err != nil {
		t.Errorf("dispatchConflictApprentice returned error on non-nil Wait() — expected nil: %v", err)
	}
}

// TestDispatchConflictApprentice_UsesCustomAgentNamespace verifies the spawn
// name uses ctx.AgentNamespace when set.
func TestDispatchConflictApprentice_UsesCustomAgentNamespace(t *testing.T) {
	dir := t.TempDir()
	wc := &spgit.WorktreeContext{Dir: dir}
	var captured agent.SpawnConfig
	ctx := &RecoveryActionCtx{
		Worktree:       wc,
		TargetBeadID:   "spi-target",
		AgentNamespace: "custom-ns",
		Spawner:        fakeSpawner{},
		DispatchFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
			captured = cfg
			return &fakeHandle{name: cfg.Name}, nil
		},
		BuildRuntimeContract: testBuildRuntimeContract,
		Log:                  func(string) {},
	}

	ws := WorkspaceHandle{
		Name:       "recovery",
		Kind:       WorkspaceKindOwnedWorktree,
		Path:       dir,
		Branch:     "recovery/spi-target",
		BaseBranch: "main",
		Origin:     WorkspaceOriginLocalBind,
	}
	_, err := dispatchConflictApprentice(ctx, conflictBundle{}, ws)
	if err != nil {
		t.Fatalf("dispatchConflictApprentice: %v", err)
	}
	if !strings.HasPrefix(captured.Name, "custom-ns-resolve-conflicts-spi-target-") {
		t.Errorf("spawn name = %q, want prefix custom-ns-resolve-conflicts-spi-target-", captured.Name)
	}
	if captured.Role != agent.RoleApprentice {
		t.Errorf("Role = %q, want apprentice", captured.Role)
	}
	// Ensure the worktree dir is passed via --worktree-dir.
	found := false
	for i, a := range captured.ExtraArgs {
		if a == "--worktree-dir" && i+1 < len(captured.ExtraArgs) && captured.ExtraArgs[i+1] == dir {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ExtraArgs missing --worktree-dir %q; got %v", dir, captured.ExtraArgs)
	}
	if captured.CustomPrompt == "" {
		t.Error("CustomPrompt should be non-empty")
	}
	if captured.Workspace == nil {
		t.Fatal("expected conflict apprentice spawn to include workspace handle")
	}
	if captured.Workspace.Path != dir {
		t.Errorf("workspace path = %q, want %q", captured.Workspace.Path, dir)
	}
	if captured.Run.FormulaStep != "resolve-conflicts" {
		t.Errorf("run formula step = %q, want %q", captured.Run.FormulaStep, "resolve-conflicts")
	}
	if captured.Identity.Prefix != "spi" {
		t.Errorf("identity prefix = %q, want %q", captured.Identity.Prefix, "spi")
	}
}

// TestSpawnRepairWorker_NonConflictActionDispatches verifies the
// spi-6wiz9 regression: a worker plan whose Action is not
// resolve-conflicts (e.g. resummon) actually reaches the apprentice
// spawn instead of silently returning success. Pre-fix, a plan with
// zero conflicted files short-circuited to Output="no conflicts to
// resolve" regardless of Action.
func TestSpawnRepairWorker_NonConflictActionDispatches(t *testing.T) {
	dir := initAgenticTestRepo(t)
	wc := &spgit.WorktreeContext{Dir: dir, RepoPath: dir, Branch: "main", BaseBranch: "main"}

	var dispatched int
	var captured agent.SpawnConfig
	ctx := &RecoveryActionCtx{
		Worktree:     wc,
		TargetBeadID: "spi-target",
		Spawner:      fakeSpawner{},
		DispatchFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
			dispatched++
			captured = cfg
			return &fakeHandle{name: cfg.Name}, nil
		},
		BuildRuntimeContract: testBuildRuntimeContract,
		Log:                  func(string) {},
	}

	plan := recovery.RepairPlan{
		Action: "resummon",
		Reason: "Test/build failure suggests transient",
		Params: map[string]string{"hint": "retry"},
	}
	ws := WorkspaceHandle{
		Name:       "recovery",
		Kind:       WorkspaceKindBorrowedWorktree,
		Path:       dir,
		Branch:     "feat/spi-target",
		BaseBranch: "main",
		Origin:     WorkspaceOriginLocalBind,
	}

	result, err := SpawnRepairWorker(ctx, plan, ws)
	if err != nil {
		t.Fatalf("SpawnRepairWorker(resummon): %v", err)
	}
	if dispatched != 1 {
		t.Fatalf("dispatched = %d, want 1 — the apprentice must be spawned for non-conflict actions", dispatched)
	}
	if result.WorkerAttemptID == "" {
		t.Error("WorkerAttemptID empty — expected a spawn name even on success")
	}
	if !strings.Contains(result.Output, "resummon") {
		t.Errorf("Output = %q, want to mention 'resummon' (got from non-conflict worker path)", result.Output)
	}

	// Prompt must carry the plan context the apprentice needs.
	if captured.CustomPrompt == "" {
		t.Fatal("captured CustomPrompt empty — apprentice would receive no context")
	}
	mustMention := []string{"resummon", "spi-target", "Test/build failure suggests transient", "hint: retry"}
	for _, s := range mustMention {
		if !strings.Contains(captured.CustomPrompt, s) {
			t.Errorf("prompt missing %q; full prompt:\n%s", s, captured.CustomPrompt)
		}
	}

	// FormulaStep must carry the action so metrics/logs distinguish
	// worker roles. Backend must come from the builder, not be
	// hard-coded "process" (the spi-6wiz9 bug).
	if captured.Run.FormulaStep != "resummon" {
		t.Errorf("Run.FormulaStep = %q, want %q", captured.Run.FormulaStep, "resummon")
	}
	if captured.Run.Backend == "process" {
		// The stub sets Backend="k8s"; if it reads "process" here,
		// someone reintroduced the hand-built SpawnConfig.
		t.Errorf("Run.Backend = %q — suggests hand-built process-only config is back", captured.Run.Backend)
	}
}

// TestSpawnRepairWorker_ResummonDispatchesWithSkipClaimFlag pins the
// fix for spi-77bk9s: the repair-worker SpawnConfig must carry the
// `--apprentice` skip-claim flag so the spawned subprocess takes the
// apprentice path in CmdWizardRun (pkg/wizard/wizard.go:458) instead
// of re-entering the wizard claim flow and colliding with the target
// bead's existing active attempt. The earlier in-process self-collision
// gate that rejected resummon was masking this missing flag and made
// resummon unreachable from live recovery loops (reproduced on
// spi-mupwy4 / spi-1daurp).
func TestSpawnRepairWorker_ResummonDispatchesWithSkipClaimFlag(t *testing.T) {
	dir := initAgenticTestRepo(t)
	wc := &spgit.WorktreeContext{Dir: dir, RepoPath: dir, Branch: "main", BaseBranch: "main"}

	var dispatched int
	var captured agent.SpawnConfig
	ctx := &RecoveryActionCtx{
		Worktree:     wc,
		TargetBeadID: "spi-target",
		Spawner:      fakeSpawner{},
		DispatchFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
			dispatched++
			captured = cfg
			return &fakeHandle{name: cfg.Name}, nil
		},
		BuildRuntimeContract: testBuildRuntimeContract,
		Log:                  func(string) {},
	}
	plan := recovery.RepairPlan{Action: "resummon", Reason: "wizard interrupted on close"}
	ws := WorkspaceHandle{
		Name:       "recovery",
		Kind:       WorkspaceKindBorrowedWorktree,
		Path:       dir,
		Branch:     "feat/spi-target",
		BaseBranch: "main",
		Origin:     WorkspaceOriginLocalBind,
	}

	if _, err := SpawnRepairWorker(ctx, plan, ws); err != nil {
		t.Fatalf("SpawnRepairWorker(resummon): %v", err)
	}
	if dispatched != 1 {
		t.Fatalf("dispatched = %d, want 1 — resummon must reach the apprentice spawn", dispatched)
	}

	// Apprentice role: the subprocess must take the apprentice code
	// path, not be registered as a fresh wizard.
	if captured.Role != agent.RoleApprentice {
		t.Errorf("Role = %q, want %q", captured.Role, agent.RoleApprentice)
	}
	// --apprentice flag: the canonical skip-claim signal — without it,
	// CmdWizardRun re-enters the claim flow and self-collides with the
	// target bead's active attempt.
	if !containsArg(captured.ExtraArgs, "--apprentice") {
		t.Errorf("ExtraArgs missing --apprentice (skip-claim): %v", captured.ExtraArgs)
	}
	// --worktree-dir: borrowed worktree, not a fresh provision.
	if !containsArg(captured.ExtraArgs, "--worktree-dir") {
		t.Errorf("ExtraArgs missing --worktree-dir: %v", captured.ExtraArgs)
	}
	if !containsArg(captured.ExtraArgs, dir) {
		t.Errorf("ExtraArgs missing borrowed worktree path %q: %v", dir, captured.ExtraArgs)
	}
	if !containsArg(captured.ExtraArgs, "--no-review") {
		t.Errorf("ExtraArgs missing --no-review: %v", captured.ExtraArgs)
	}
	// Target bead: the repair worker acts on the parent bead, not the
	// recovery bead.
	if captured.BeadID != "spi-target" {
		t.Errorf("BeadID = %q, want %q", captured.BeadID, "spi-target")
	}
	// Borrowed-handoff runtime contract: workspace must be the borrowed
	// worktree, not a fresh provision.
	if captured.Run.WorkspaceKind != WorkspaceKindBorrowedWorktree {
		t.Errorf("Run.WorkspaceKind = %q, want %q",
			captured.Run.WorkspaceKind, WorkspaceKindBorrowedWorktree)
	}
	if captured.Run.HandoffMode != HandoffBorrowed {
		t.Errorf("Run.HandoffMode = %q, want %q",
			captured.Run.HandoffMode, HandoffBorrowed)
	}
}

// TestSpawnRepairWorker_BuildsCanonicalRuntimeContract asserts the
// k8s-oriented invariant from spi-6wiz9: every worker spawn — conflict
// resolver or generic repair role — must carry the canonical
// Identity/Workspace/RunContext fields that pkg/agent/backend_k8s.go
// buildSubstratePod enforces (RepoURL, BaseBranch, Prefix, non-nil
// Workspace, non-empty FormulaStep/HandoffMode). Before the fix,
// SpawnRepairWorker hand-built Identity without RepoURL and Run.Backend
// was hard-coded "process", so k8s dispatches would fail with
// ErrIdentityRequired before the worker started.
func TestSpawnRepairWorker_BuildsCanonicalRuntimeContract(t *testing.T) {
	dir := initAgenticTestRepo(t)
	wc := &spgit.WorktreeContext{Dir: dir, RepoPath: dir, Branch: "feat/spi-target", BaseBranch: "main"}

	var captured agent.SpawnConfig
	ctx := &RecoveryActionCtx{
		Worktree:     wc,
		TargetBeadID: "spi-target",
		BaseBranch:   "main",
		Spawner:      fakeSpawner{},
		DispatchFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
			captured = cfg
			return &fakeHandle{name: cfg.Name}, nil
		},
		BuildRuntimeContract: testBuildRuntimeContract,
		Log:                  func(string) {},
	}

	plan := recovery.RepairPlan{Action: "targeted-fix", Reason: "assertion failed"}
	ws := WorkspaceHandle{
		Name:       "recovery",
		Kind:       WorkspaceKindBorrowedWorktree,
		Path:       dir,
		Branch:     "feat/spi-target",
		BaseBranch: "main",
		Origin:     WorkspaceOriginLocalBind,
	}
	if _, err := SpawnRepairWorker(ctx, plan, ws); err != nil {
		t.Fatalf("SpawnRepairWorker(targeted-fix): %v", err)
	}

	// Exact preconditions from pkg/agent/backend_k8s.go:buildSubstratePod.
	if captured.Workspace == nil {
		t.Error("cfg.Workspace is nil — buildSubstratePod would reject with ErrWorkspaceRequired")
	}
	if captured.Identity.RepoURL == "" {
		t.Error("Identity.RepoURL empty — buildSubstratePod would reject with ErrIdentityRequired (the spi-6wiz9 bug)")
	}
	if captured.Identity.BaseBranch == "" {
		t.Error("Identity.BaseBranch empty — buildSubstratePod would reject with ErrIdentityRequired")
	}
	if captured.Identity.Prefix == "" {
		t.Error("Identity.Prefix empty — buildSubstratePod would reject with ErrIdentityRequired")
	}

	// Run context must be stamped — these fields drive log / metric
	// labels and the pod env contract.
	if captured.Run.FormulaStep != "targeted-fix" {
		t.Errorf("Run.FormulaStep = %q, want %q", captured.Run.FormulaStep, "targeted-fix")
	}
	if captured.Run.HandoffMode != HandoffBorrowed {
		t.Errorf("Run.HandoffMode = %q, want %q", captured.Run.HandoffMode, HandoffBorrowed)
	}
	if captured.Run.Backend == "" {
		t.Error("Run.Backend empty — must be set by the canonical builder, never hard-coded")
	}
	if captured.Run.Backend == "process" {
		// The builder selects the backend from substrate config. If the
		// stub (which sets backend="k8s") is bypassed and we see the
		// legacy hard-coded "process", the hand-built config is back.
		t.Errorf("Run.Backend = %q — hand-built process-only config is back (spi-6wiz9 regression)", captured.Run.Backend)
	}
	if captured.Run.WorkspaceKind != WorkspaceKindBorrowedWorktree {
		t.Errorf("Run.WorkspaceKind = %q, want %q", captured.Run.WorkspaceKind, WorkspaceKindBorrowedWorktree)
	}
}

// TestSpawnRepairWorker_RequiresRuntimeContractBuilder verifies that the
// worker path refuses to dispatch when ctx.BuildRuntimeContract is not
// wired. This prevents a silent regression where a future caller
// bypasses buildRecoveryActionCtx and hand-builds a SpawnConfig without
// the canonical runtime contract — the exact shape of the spi-6wiz9
// bug.
func TestSpawnRepairWorker_RequiresRuntimeContractBuilder(t *testing.T) {
	dir := initAgenticTestRepo(t)
	wc := &spgit.WorktreeContext{Dir: dir, RepoPath: dir, Branch: "main", BaseBranch: "main"}

	ctx := &RecoveryActionCtx{
		Worktree:     wc,
		TargetBeadID: "spi-target",
		Spawner:      fakeSpawner{},
		DispatchFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
			return &fakeHandle{name: cfg.Name}, nil
		},
		Log: func(string) {},
		// BuildRuntimeContract intentionally nil.
	}

	_, err := SpawnRepairWorker(ctx, recovery.RepairPlan{Action: "resummon"}, WorkspaceHandle{Path: dir})
	if err == nil {
		t.Fatal("expected error when BuildRuntimeContract is not wired")
	}
	if !strings.Contains(err.Error(), "BuildRuntimeContract") {
		t.Errorf("error = %q, want to mention BuildRuntimeContract", err)
	}
}

// ---------------------------------------------------------------------------
// Helpers — local to the agentic tests.
// ---------------------------------------------------------------------------

func initAgenticTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustRun(t, dir, "git", "init")
	mustRun(t, dir, "git", "config", "user.name", "Test")
	mustRun(t, dir, "git", "config", "user.email", "test@test.com")
	writeAgenticFile(t, filepath.Join(dir, "README.md"), "# Test\n")
	mustRun(t, dir, "git", "add", "-A")
	mustRun(t, dir, "git", "commit", "-m", "initial commit")
	mustRun(t, dir, "git", "branch", "-M", "main")
	return dir
}

func writeAgenticFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustRun(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	out, err := runCmd(dir, name, args...)
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return out
}

func allowFail(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	_, _ = runCmd(dir, name, args...)
}

// runCmd runs a command in dir and returns combined output + error.
func runCmd(dir, name string, args ...string) (string, error) {
	cmd := makeCmd(dir, name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func makeCmd(dir, name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	return cmd
}
