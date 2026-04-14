package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// =============================================================================
// DiagnoseBranch tests
// =============================================================================

func TestDiagnoseBranch(t *testing.T) {
	tests := []struct {
		name        string
		setup       func(t *testing.T, dir string)
		branch      string
		wantAhead   int
		wantBehind  int
		wantDiverge bool
		wantErr     bool
	}{
		{
			name: "branch at same point as main",
			setup: func(t *testing.T, dir string) {
				run(t, dir, "git", "branch", "feat/same")
			},
			branch:      "feat/same",
			wantAhead:   0,
			wantBehind:  0,
			wantDiverge: false,
		},
		{
			name: "branch ahead of main",
			setup: func(t *testing.T, dir string) {
				run(t, dir, "git", "branch", "feat/ahead")
				run(t, dir, "git", "checkout", "feat/ahead")
				writeFile(t, filepath.Join(dir, "ahead.txt"), "ahead\n")
				run(t, dir, "git", "add", "-A")
				run(t, dir, "git", "commit", "-m", "ahead commit 1")
				writeFile(t, filepath.Join(dir, "ahead2.txt"), "ahead2\n")
				run(t, dir, "git", "add", "-A")
				run(t, dir, "git", "commit", "-m", "ahead commit 2")
				run(t, dir, "git", "checkout", "main")
			},
			branch:      "feat/ahead",
			wantAhead:   2,
			wantBehind:  0,
			wantDiverge: false,
		},
		{
			name: "branch behind main",
			setup: func(t *testing.T, dir string) {
				run(t, dir, "git", "branch", "feat/behind")
				// Add commits to main after creating the branch.
				writeFile(t, filepath.Join(dir, "main-new.txt"), "main\n")
				run(t, dir, "git", "add", "-A")
				run(t, dir, "git", "commit", "-m", "main commit")
			},
			branch:      "feat/behind",
			wantAhead:   0,
			wantBehind:  1,
			wantDiverge: false,
		},
		{
			name: "diverged branch",
			setup: func(t *testing.T, dir string) {
				run(t, dir, "git", "branch", "feat/diverged")
				// Commit on main.
				writeFile(t, filepath.Join(dir, "main-div.txt"), "main\n")
				run(t, dir, "git", "add", "-A")
				run(t, dir, "git", "commit", "-m", "main diverge")
				// Commit on branch.
				run(t, dir, "git", "checkout", "feat/diverged")
				writeFile(t, filepath.Join(dir, "branch-div.txt"), "branch\n")
				run(t, dir, "git", "add", "-A")
				run(t, dir, "git", "commit", "-m", "branch diverge")
				run(t, dir, "git", "checkout", "main")
			},
			branch:      "feat/diverged",
			wantAhead:   1,
			wantBehind:  1,
			wantDiverge: true,
		},
		{
			name:    "nonexistent branch",
			setup:   func(t *testing.T, dir string) {},
			branch:  "feat/does-not-exist",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := initTestRepo(t)
			tt.setup(t, dir)

			diag, err := DiagnoseBranch(dir, tt.branch)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("DiagnoseBranch: %v", err)
			}

			if diag.AheadOfMain != tt.wantAhead {
				t.Errorf("AheadOfMain = %d, want %d", diag.AheadOfMain, tt.wantAhead)
			}
			if diag.BehindMain != tt.wantBehind {
				t.Errorf("BehindMain = %d, want %d", diag.BehindMain, tt.wantBehind)
			}
			if diag.Diverged != tt.wantDiverge {
				t.Errorf("Diverged = %v, want %v", diag.Diverged, tt.wantDiverge)
			}
			if diag.MainRef != "main" {
				t.Errorf("MainRef = %q, want %q", diag.MainRef, "main")
			}
			if diag.BranchRef != tt.branch {
				t.Errorf("BranchRef = %q, want %q", diag.BranchRef, tt.branch)
			}
			if diag.LastCommitHash == "" {
				t.Error("LastCommitHash should not be empty")
			}
			if diag.LastCommitMsg == "" {
				t.Error("LastCommitMsg should not be empty")
			}
		})
	}
}

func TestDiagnoseBranch_MasterFallback(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "git", "init")
	run(t, dir, "git", "config", "user.name", "Test")
	run(t, dir, "git", "config", "user.email", "test@test.com")
	writeFile(t, filepath.Join(dir, "README.md"), "# Test\n")
	run(t, dir, "git", "add", "-A")
	run(t, dir, "git", "commit", "-m", "initial")
	// Rename main to master.
	run(t, dir, "git", "branch", "-M", "master")
	run(t, dir, "git", "branch", "feat/test")

	diag, err := DiagnoseBranch(dir, "feat/test")
	if err != nil {
		t.Fatalf("DiagnoseBranch with master: %v", err)
	}
	if diag.MainRef != "master" {
		t.Errorf("MainRef = %q, want %q", diag.MainRef, "master")
	}
}

// =============================================================================
// DiagnoseWorktree tests
// =============================================================================

func TestDiagnoseWorktree(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(t *testing.T, dir string) string // returns beadID to query
		wantExist bool
		wantDirty bool
		wantErr   bool
	}{
		{
			name: "worktree exists and is clean",
			setup: func(t *testing.T, dir string) string {
				rc := &RepoContext{Dir: dir, BaseBranch: "main"}
				wtDir := filepath.Join(t.TempDir(), "wt-spi-abc12")
				rc.CreateBranch("feat/spi-abc12")
				_, err := rc.CreateWorktree(wtDir, "feat/spi-abc12")
				if err != nil {
					t.Fatalf("CreateWorktree: %v", err)
				}
				return "spi-abc12"
			},
			wantExist: true,
			wantDirty: false,
		},
		{
			name: "worktree exists and is dirty",
			setup: func(t *testing.T, dir string) string {
				rc := &RepoContext{Dir: dir, BaseBranch: "main"}
				wtDir := filepath.Join(t.TempDir(), "wt-spi-dirty1")
				rc.CreateBranch("feat/spi-dirty1")
				wc, err := rc.CreateWorktree(wtDir, "feat/spi-dirty1")
				if err != nil {
					t.Fatalf("CreateWorktree: %v", err)
				}
				// Make dirty by creating an untracked file.
				writeFile(t, filepath.Join(wc.Dir, "dirty.txt"), "dirty\n")
				return "spi-dirty1"
			},
			wantExist: true,
			wantDirty: true,
		},
		{
			name: "no matching worktree",
			setup: func(t *testing.T, dir string) string {
				return "spi-nonexistent"
			},
			wantExist: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := initTestRepo(t)
			beadID := tt.setup(t, dir)

			diag, err := DiagnoseWorktree(dir, beadID)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("DiagnoseWorktree: %v", err)
			}

			if diag.Exists != tt.wantExist {
				t.Errorf("Exists = %v, want %v", diag.Exists, tt.wantExist)
			}
			if !tt.wantExist {
				return
			}
			if diag.Path == "" {
				t.Error("Path should not be empty when worktree exists")
			}
			if diag.IsDirty != tt.wantDirty {
				t.Errorf("IsDirty = %v, want %v", diag.IsDirty, tt.wantDirty)
			}
			if diag.Branch == "" {
				t.Error("Branch should not be empty when worktree exists")
			}
			if diag.HeadHash == "" {
				t.Error("HeadHash should not be empty when worktree exists")
			}
		})
	}
}

func TestDiagnoseWorktree_UntrackedFiles(t *testing.T) {
	dir := initTestRepo(t)
	rc := &RepoContext{Dir: dir, BaseBranch: "main"}

	wtDir := filepath.Join(t.TempDir(), "wt-spi-untrk")
	rc.CreateBranch("feat/spi-untrk")
	wc, err := rc.CreateWorktree(wtDir, "feat/spi-untrk")
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}

	// Create untracked files.
	writeFile(t, filepath.Join(wc.Dir, "untracked1.txt"), "a\n")
	writeFile(t, filepath.Join(wc.Dir, "untracked2.txt"), "b\n")

	diag, err := DiagnoseWorktree(dir, "spi-untrk")
	if err != nil {
		t.Fatalf("DiagnoseWorktree: %v", err)
	}
	if !diag.IsDirty {
		t.Error("expected dirty with untracked files")
	}
	if len(diag.UntrackedFiles) != 2 {
		t.Errorf("UntrackedFiles count = %d, want 2; got: %v", len(diag.UntrackedFiles), diag.UntrackedFiles)
	}
}

// =============================================================================
// CollectStepOutput tests
// =============================================================================

func TestCollectStepOutput(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(t *testing.T, dir string)
		step       string
		wantOutput string
		wantEmpty  bool
	}{
		{
			name: "finds log in .spire directory",
			setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, ".spire", "verify-build.log"), "BUILD OK\n")
			},
			step:       "verify-build",
			wantOutput: "BUILD OK\n",
		},
		{
			name: "finds log in .beads directory",
			setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, ".beads", "test-output.log"), "TESTS PASSED\n")
			},
			step:       "test-output",
			wantOutput: "TESTS PASSED\n",
		},
		{
			name: "prefers .spire over .beads",
			setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, ".spire", "build.log"), "from spire\n")
				writeFile(t, filepath.Join(dir, ".beads", "build.log"), "from beads\n")
			},
			step:       "build",
			wantOutput: "from spire\n",
		},
		{
			name: "finds -output.log variant",
			setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, ".spire", "test-output.log"), "test output\n")
			},
			step:       "test",
			wantOutput: "test output\n",
		},
		{
			name:      "no output file found",
			setup:     func(t *testing.T, dir string) {},
			step:      "nonexistent",
			wantEmpty: true,
		},
		{
			name: "truncates to 4KB",
			setup: func(t *testing.T, dir string) {
				// Create content larger than 4KB.
				big := strings.Repeat("x", 5000)
				writeFile(t, filepath.Join(dir, ".spire", "big.log"), big)
			},
			step:       "big",
			wantOutput: strings.Repeat("x", 4096),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			tt.setup(t, dir)

			output, err := CollectStepOutput(dir, tt.step)
			if err != nil {
				t.Fatalf("CollectStepOutput: %v", err)
			}
			if tt.wantEmpty {
				if output != "" {
					t.Errorf("expected empty output, got %q", output)
				}
				return
			}
			if output != tt.wantOutput {
				t.Errorf("output = %q, want %q", output, tt.wantOutput)
			}
		})
	}
}

// =============================================================================
// Internal helper tests
// =============================================================================

func TestFindWorktreeByID(t *testing.T) {
	porcelain := `worktree /home/user/repo
HEAD abc123def
branch refs/heads/main

worktree /tmp/spire/wt-spi-x1y2z
HEAD 999888777
branch refs/heads/feat/spi-x1y2z

worktree /tmp/spire/wt-spi-other
HEAD 111222333
branch refs/heads/feat/spi-other

`
	tests := []struct {
		id         string
		wantPath   string
		wantBranch string
	}{
		{"spi-x1y2z", "/tmp/spire/wt-spi-x1y2z", "feat/spi-x1y2z"},
		{"spi-other", "/tmp/spire/wt-spi-other", "feat/spi-other"},
		{"spi-nope", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			path, branch := findWorktreeByID(porcelain, tt.id)
			if path != tt.wantPath {
				t.Errorf("path = %q, want %q", path, tt.wantPath)
			}
			if branch != tt.wantBranch {
				t.Errorf("branch = %q, want %q", branch, tt.wantBranch)
			}
		})
	}
}

func TestResolveDefaultBranch(t *testing.T) {
	// Test with main branch.
	dir := initTestRepo(t)
	ref, err := resolveDefaultBranch(dir)
	if err != nil {
		t.Fatalf("resolveDefaultBranch: %v", err)
	}
	if ref != "main" {
		t.Errorf("resolveDefaultBranch = %q, want %q", ref, "main")
	}

	// Test with master branch.
	dirMaster := t.TempDir()
	run(t, dirMaster, "git", "init")
	run(t, dirMaster, "git", "config", "user.name", "Test")
	run(t, dirMaster, "git", "config", "user.email", "test@test.com")
	writeFile(t, filepath.Join(dirMaster, "f.txt"), "f\n")
	run(t, dirMaster, "git", "add", "-A")
	run(t, dirMaster, "git", "commit", "-m", "init")
	run(t, dirMaster, "git", "branch", "-M", "master")

	ref, err = resolveDefaultBranch(dirMaster)
	if err != nil {
		t.Fatalf("resolveDefaultBranch (master): %v", err)
	}
	if ref != "master" {
		t.Errorf("resolveDefaultBranch = %q, want %q", ref, "master")
	}

	// Test with neither.
	dirEmpty := t.TempDir()
	run(t, dirEmpty, "git", "init")
	_, err = resolveDefaultBranch(dirEmpty)
	if err == nil {
		t.Error("expected error when neither main nor master exists")
	}
}

func TestSplitWorktreeBlocks(t *testing.T) {
	input := "worktree /a\nHEAD abc\nbranch refs/heads/main\n\nworktree /b\nHEAD def\n\n"
	blocks := splitWorktreeBlocks(input)
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	if len(blocks[0]) != 3 {
		t.Errorf("block 0 has %d lines, want 3", len(blocks[0]))
	}
	if len(blocks[1]) != 2 {
		t.Errorf("block 1 has %d lines, want 2", len(blocks[1]))
	}
}

// Verify that DiagnoseWorktree does not error on an empty repo with no worktrees.
func TestDiagnoseWorktree_NoWorktrees(t *testing.T) {
	dir := initTestRepo(t)
	diag, err := DiagnoseWorktree(dir, "spi-nothing")
	if err != nil {
		t.Fatalf("DiagnoseWorktree: %v", err)
	}
	if diag.Exists {
		t.Error("expected Exists=false for repo with no extra worktrees")
	}
}

// Verify that CollectStepOutput gracefully handles missing directories.
func TestCollectStepOutput_NoDirs(t *testing.T) {
	dir := t.TempDir()
	// Neither .spire/ nor .beads/ exist.
	out, err := CollectStepOutput(dir, "build")
	if err != nil {
		t.Fatalf("CollectStepOutput: %v", err)
	}
	if out != "" {
		t.Errorf("expected empty output, got %q", out)
	}
}

// Verify we don't error on unreadable files — just skip them.
func TestCollectStepOutput_UnreadableFile(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".spire", "build.log")
	writeFile(t, logPath, "content\n")
	// Make unreadable.
	os.Chmod(logPath, 0000)
	t.Cleanup(func() { os.Chmod(logPath, 0644) })

	out, err := CollectStepOutput(dir, "build")
	if err != nil {
		t.Fatalf("CollectStepOutput: %v", err)
	}
	// Should skip the unreadable file and return empty.
	if out != "" {
		t.Errorf("expected empty output for unreadable file, got %q", out)
	}
}
