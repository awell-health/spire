package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	spireruntime "github.com/awell-health/spire/pkg/runtime"
)

// TestCheckCacheReady_Fresh covers the happy path: a cache root with
// the revision marker present is usable, and checkCacheReady returns
// the marker contents (the cache revision token).
func TestCheckCacheReady_Fresh(t *testing.T) {
	cache := t.TempDir()
	const rev = "deadbeef1234567890"
	writeMarker(t, filepath.Join(cache, CacheRevisionMarkerName), rev+"\n")

	got, err := checkCacheReady(cache)
	if err != nil {
		t.Fatalf("checkCacheReady(fresh) returned %v", err)
	}
	if got != rev {
		t.Errorf("checkCacheReady = %q, want %q", got, rev)
	}
}

// TestCheckCacheReady_Missing covers the "no refresh has completed yet"
// case: the revision marker absent means the cache is not yet safe to
// read. checkCacheReady must wrap ErrCacheUnavailable so callers can
// use errors.Is to detect the typed condition regardless of the
// underlying os error.
func TestCheckCacheReady_Missing(t *testing.T) {
	cache := t.TempDir() // no markers

	_, err := checkCacheReady(cache)
	if err == nil {
		t.Fatalf("expected error when revision marker missing; got nil")
	}
	if !errors.Is(err, ErrCacheUnavailable) {
		t.Errorf("error = %v, want errors.Is(...ErrCacheUnavailable)", err)
	}
}

// TestCheckCacheReady_TmpMarkerIsNotReadySignal covers the invariant
// that the .tmp sibling is NOT a ready signal. The refresh script
// writes the tmp file before the atomic rename; a worker that
// happened to read the cache between those two steps must still see
// "not ready" until the rename lands. If checkCacheReady treated
// CacheRevisionTmpMarkerName as a ready signal, the atomic-rename
// contract would collapse into "possibly partial".
func TestCheckCacheReady_TmpMarkerIsNotReadySignal(t *testing.T) {
	cache := t.TempDir()
	// Only the .tmp sibling exists — the rename has not happened.
	writeMarker(t, filepath.Join(cache, CacheRevisionTmpMarkerName), "in-flight-rev\n")

	_, err := checkCacheReady(cache)
	if err == nil {
		t.Fatalf("expected error when only %s is present; got nil", CacheRevisionTmpMarkerName)
	}
	if !errors.Is(err, ErrCacheUnavailable) {
		t.Errorf("error = %v, want errors.Is(...ErrCacheUnavailable)", err)
	}
}

// TestCheckCacheReady_NoCachePath ensures callers get ErrCacheUnavailable
// when the cache directory itself is missing (e.g. PVC mount failure).
func TestCheckCacheReady_NoCachePath(t *testing.T) {
	_, err := checkCacheReady(filepath.Join(t.TempDir(), "nope"))
	if !errors.Is(err, ErrCacheUnavailable) {
		t.Errorf("error = %v, want errors.Is(...ErrCacheUnavailable)", err)
	}
}

// TestMaterializeWorkspaceFromCache_Succeeds sets up a cache fixture
// that mirrors the real producer output (a mirror/ subdirectory
// containing a fake git repo, plus a revision marker at the cache
// root) and runs MaterializeWorkspaceFromCache end-to-end. The
// resulting workspace must be a valid git clone, contain the
// committed file, and be writable. Exercises the atomic clone →
// writable working tree transition, which is the whole point of the
// phase-2 contract.
func TestMaterializeWorkspaceFromCache_Succeeds(t *testing.T) {
	requireGit(t)

	cache := makeCacheFixture(t, "README.md", "hello from cache\n", "cache-rev-a1b2c3")

	workspace := filepath.Join(t.TempDir(), "ws")
	setRunContextEnv(t)

	logBuf := captureAgentLog(t)

	err := MaterializeWorkspaceFromCache(context.Background(), cache, workspace, "spi")
	if err != nil {
		t.Fatalf("MaterializeWorkspaceFromCache: %v\nlog:\n%s", err, logBuf.String())
	}

	// Cloned README.md must exist with the expected content.
	readmePath := filepath.Join(workspace, "README.md")
	content, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("read cloned README: %v", err)
	}
	if string(content) != "hello from cache\n" {
		t.Errorf("cloned README content = %q, want %q", string(content), "hello from cache\n")
	}

	// The workspace must be writable. The whole point of the clone (vs a
	// worktree add, which would touch the cache's .git/worktrees) is
	// that a wizard pod can mutate its working tree. Create a new file,
	// git add, and commit — if any of that fails, the workspace is not
	// usable as an execution substrate.
	if err := os.WriteFile(filepath.Join(workspace, "new.txt"), []byte("ok\n"), 0o644); err != nil {
		t.Fatalf("write to workspace: %v", err)
	}
	if out, err := runGit(workspace, "config", "user.email", "test@example.com"); err != nil {
		t.Fatalf("git config email: %v\n%s", err, out)
	}
	if out, err := runGit(workspace, "config", "user.name", "test"); err != nil {
		t.Fatalf("git config name: %v\n%s", err, out)
	}
	if out, err := runGit(workspace, "add", "new.txt"); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	if out, err := runGit(workspace, "commit", "-m", "test"); err != nil {
		t.Fatalf("git commit failed — workspace is not writable: %v\n%s", err, out)
	}

	// Observability: the canonical runtime identity labels (tower,
	// prefix) must appear on bootstrap log lines per spi-xplwy §1.4.
	// MaterializeWorkspaceFromCache is the entry point for phase-2
	// cache→workspace bootstrap; its log fields are the audit trail.
	logs := logBuf.String()
	assertLogContains(t, logs, "source="+BootstrapSourceGuildCache)
	assertLogContains(t, logs, " "+spireruntime.LogFieldTower+"=test-tower")
	assertLogContains(t, logs, " "+spireruntime.LogFieldPrefix+"=spi")
	// workspace id: the canonical "workspace_name" label propagates via
	// the SPIRE_WORKSPACE_NAME env. Test uses "wizard".
	assertLogContains(t, logs, " "+spireruntime.LogFieldWorkspaceName+"=wizard")
	assertLogContains(t, logs, "phase="+StartupPhaseCacheReady)
	assertLogContains(t, logs, "phase="+StartupPhaseWorkspaceDerive)
	// Success metrics with the canonical metric label surface.
	assertLogContains(t, logs, "metric="+MetricBootstrapDuration)
	assertLogContains(t, logs, "metric="+MetricBootstrapSuccess)
	assertLogContains(t, logs, "result=success")
	// The cache revision (from the revision marker) must land on the
	// canonical high-cardinality label for trace/log correlation.
	assertLogContains(t, logs, LabelCacheRevision+"=cache-rev-a1b2c3")
}

// TestMaterializeWorkspaceFromCache_StaleCacheReturnsSentinel asserts
// the typed error contract — a missing revision marker returns
// ErrCacheUnavailable rather than a generic clone failure. Callers
// (the init container entrypoint) rely on the sentinel to decide
// whether to fail the pod vs. let it try again.
func TestMaterializeWorkspaceFromCache_StaleCacheReturnsSentinel(t *testing.T) {
	requireGit(t)
	cache := t.TempDir()
	// Mirror subdir exists but the revision marker never landed.
	// That's the exact fingerprint of a crashed refresh: clone finished,
	// atomic rename did not.
	makeMirror(t, cache, "README.md", "x")
	workspace := filepath.Join(t.TempDir(), "ws")
	setRunContextEnv(t)

	err := MaterializeWorkspaceFromCache(context.Background(), cache, workspace, "spi")
	if !errors.Is(err, ErrCacheUnavailable) {
		t.Errorf("stale cache: err = %v, want errors.Is(...ErrCacheUnavailable)", err)
	}
	// Workspace must not have been created — the init container would
	// otherwise pick up a half-built workspace on retry.
	if _, statErr := os.Stat(filepath.Join(workspace, ".git")); statErr == nil {
		t.Errorf("workspace was cloned despite stale cache")
	}
}

// TestMaterializeWorkspaceFromCache_TmpMarkerOnlyReturnsSentinel
// exercises the crash-between-tmp-and-rename path into
// ErrCacheUnavailable: the refresh script wrote the tmp file but the
// process died before the atomic rename, so the final marker is
// absent. Workers must treat this identically to the "no marker"
// case — the tmp file alone is NOT a ready signal.
func TestMaterializeWorkspaceFromCache_TmpMarkerOnlyReturnsSentinel(t *testing.T) {
	requireGit(t)
	cache := t.TempDir()
	makeMirror(t, cache, "README.md", "x")
	writeMarker(t, filepath.Join(cache, CacheRevisionTmpMarkerName), "mid-write-rev\n")

	workspace := filepath.Join(t.TempDir(), "ws")
	setRunContextEnv(t)

	err := MaterializeWorkspaceFromCache(context.Background(), cache, workspace, "spi")
	if !errors.Is(err, ErrCacheUnavailable) {
		t.Errorf("tmp-only cache: err = %v, want errors.Is(...ErrCacheUnavailable)", err)
	}
}

// TestMaterializeWorkspaceFromCache_Idempotent ensures a repeat
// invocation on an already-materialized workspace is a no-op. Pod
// restarts inside the init container must not redo the clone (slow) nor
// fail because the target exists — the contract doc commits to
// idempotency.
func TestMaterializeWorkspaceFromCache_Idempotent(t *testing.T) {
	requireGit(t)

	cache := makeCacheFixture(t, "README.md", "hello\n", "rev1")
	workspace := filepath.Join(t.TempDir(), "ws")
	setRunContextEnv(t)

	if err := MaterializeWorkspaceFromCache(context.Background(), cache, workspace, "spi"); err != nil {
		t.Fatalf("first materialize: %v", err)
	}

	// Drop a sentinel file inside the workspace; a second materialize
	// that re-cloned would wipe it out.
	sentinel := filepath.Join(workspace, "stays.txt")
	if err := os.WriteFile(sentinel, []byte("keep me"), 0o644); err != nil {
		t.Fatalf("seed sentinel: %v", err)
	}

	if err := MaterializeWorkspaceFromCache(context.Background(), cache, workspace, "spi"); err != nil {
		t.Fatalf("second materialize: %v", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("idempotent materialize clobbered workspace contents: sentinel missing (%v)", err)
	}
}

// TestBindLocalRepo_InvokesBindLocalNotAdd is the load-bearing assertion
// for spi-jetfb: BindLocalRepo MUST NOT shell out to `spire repo add`.
// That would mutate the shared repos table on every pod start. The
// helper is required to call `spire repo bind-local`, the local-only
// bind entrypoint that writes only to per-tower LocalBindings.
//
// We plant a shim `spire` binary on PATH that records its argv and
// succeeds. The test fails if the shim is invoked with "repo add" or
// without "repo bind-local".
func TestBindLocalRepo_InvokesBindLocalNotAdd(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.log")
	installSpireShim(t, argsFile, 0)

	workspace := t.TempDir()
	t.Setenv("SPIRE_REPO_URL", "git@example.com:awell-health/spire.git")
	t.Setenv("SPIRE_REPO_BRANCH", "main")
	setRunContextEnv(t)

	if err := BindLocalRepo(context.Background(), workspace, "spi"); err != nil {
		t.Fatalf("BindLocalRepo: %v", err)
	}

	argv := readShimInvocations(t, argsFile)
	if len(argv) != 1 {
		t.Fatalf("spire shim invoked %d times, want 1", len(argv))
	}
	args := argv[0]

	// MUST call `spire repo bind-local`. The first arg is the subcommand
	// path; the shim records everything after the binary name.
	if len(args) < 2 || args[0] != "repo" || args[1] != "bind-local" {
		t.Fatalf("BindLocalRepo invoked: spire %s; want `spire repo bind-local`", strings.Join(args, " "))
	}
	// MUST NOT call `spire repo add` — that mutates the shared repos
	// table (pkg/store) and would duplicate registration on every pod
	// restart.
	for _, arg := range args {
		if arg == "add" {
			t.Fatalf("BindLocalRepo must NOT invoke `spire repo add`; got: spire %s", strings.Join(args, " "))
		}
	}
	// Flags must carry the prefix, workspace path, repo URL, and branch.
	wantFlagSubstrings := []string{
		"--prefix", "spi",
		"--path", workspace,
		"--repo-url", "git@example.com:awell-health/spire.git",
		"--branch", "main",
	}
	joined := strings.Join(args, " ")
	for _, want := range wantFlagSubstrings {
		if !strings.Contains(joined, want) {
			t.Errorf("BindLocalRepo args missing %q; got: spire %s", want, joined)
		}
	}
}

// TestBindLocalRepo_ObservabilityLabels asserts that BindLocalRepo emits
// the canonical runtime identity fields on its structured log lines.
// spi-xplwy §1.4 requires: tower, prefix, role, backend,
// workspace_kind/name/origin — missing fields render empty, but the
// keys are always present.
func TestBindLocalRepo_ObservabilityLabels(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.log")
	installSpireShim(t, argsFile, 0)

	workspace := t.TempDir()
	t.Setenv("SPIRE_REPO_URL", "git@example.com:awell-health/spire.git")
	t.Setenv("SPIRE_REPO_BRANCH", "main")
	setRunContextEnv(t)

	logBuf := captureAgentLog(t)
	if err := BindLocalRepo(context.Background(), workspace, "spi"); err != nil {
		t.Fatalf("BindLocalRepo: %v", err)
	}

	logs := logBuf.String()
	// The local-bind phase marker is the canonical signal for the bind step.
	assertLogContains(t, logs, "phase="+StartupPhaseLocalBindBootstrap)
	// Bootstrap source must be the guild-cache value; `origin-clone` etc.
	// live in their own code paths.
	assertLogContains(t, logs, "source="+BootstrapSourceGuildCache)
	// Canonical identity labels from spi-xplwy — these are what
	// dashboards and alert rules grep for.
	assertLogContains(t, logs, " "+spireruntime.LogFieldTower+"=test-tower")
	assertLogContains(t, logs, " "+spireruntime.LogFieldPrefix+"=spi")
	assertLogContains(t, logs, " "+spireruntime.LogFieldRole+"=wizard")
	assertLogContains(t, logs, " "+spireruntime.LogFieldBackend+"=operator-k8s")
	assertLogContains(t, logs, " "+spireruntime.LogFieldWorkspaceKind+"=owned_worktree")
	assertLogContains(t, logs, " "+spireruntime.LogFieldWorkspaceName+"=wizard")
	// Metric emissions carry the low-cardinality label set (no bead_id).
	assertLogContains(t, logs, "metric="+MetricBootstrapDuration)
	assertLogContains(t, logs, "result=success")
}

// TestBindLocalRepo_FailureEmitsFailureMetric asserts that a failing
// bind-local invocation surfaces as a metric=... result=failure line so
// dashboards can distinguish success from failure without parsing the
// error message.
func TestBindLocalRepo_FailureEmitsFailureMetric(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.log")
	installSpireShim(t, argsFile, 1) // exit non-zero

	workspace := t.TempDir()
	t.Setenv("SPIRE_REPO_URL", "git@example.com:awell-health/spire.git")
	t.Setenv("SPIRE_REPO_BRANCH", "main")
	setRunContextEnv(t)

	logBuf := captureAgentLog(t)
	if err := BindLocalRepo(context.Background(), workspace, "spi"); err == nil {
		t.Fatalf("BindLocalRepo unexpectedly succeeded with failing shim")
	}
	logs := logBuf.String()
	assertLogContains(t, logs, "result=failure")
	assertLogContains(t, logs, "metric="+MetricBootstrapSuccess)
}

// TestBindLocalRepo_MissingRequiredEnv asserts input validation: the
// helper demands SPIRE_REPO_URL / SPIRE_REPO_BRANCH / prefix /
// workspacePath up-front rather than silently calling the shim with
// empty flags.
func TestBindLocalRepo_MissingRequiredEnv(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.log")
	installSpireShim(t, argsFile, 0)
	workspace := t.TempDir()
	// SPIRE_REPO_URL intentionally unset.
	t.Setenv("SPIRE_REPO_URL", "")
	t.Setenv("SPIRE_REPO_BRANCH", "main")
	setRunContextEnv(t)

	err := BindLocalRepo(context.Background(), workspace, "spi")
	if err == nil {
		t.Fatalf("want error when SPIRE_REPO_URL unset; got nil")
	}
	if !strings.Contains(err.Error(), "SPIRE_REPO_URL") {
		t.Errorf("error = %v, want to mention SPIRE_REPO_URL", err)
	}
	// Shim must NOT have been called — the guard runs before exec.
	if _, err := os.Stat(argsFile); err == nil {
		t.Errorf("shim invoked despite missing env; args file exists")
	}
}

// --- helpers ---

// requireGit skips the test when git is not on PATH (common on CI
// containers with a minimal base). Materialize/bootstrap tests have no
// useful fallback — the contract IS git behavior.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not on PATH (%v); skipping", err)
	}
}

// makeCacheFixture returns the path to a cache root laid out the way
// the real refresh Job publishes it:
//
//   - <cache>/<CacheMirrorSubdir>/   a git mirror that `git clone
//                                    --local` can consume (seeded
//                                    with a single committed file).
//   - <cache>/<CacheRevisionMarkerName>  contains the given revision
//                                        token.
//
// Tests that want to simulate partial publisher state (mirror without
// marker, tmp-only, etc.) should compose makeMirror + writeMarker
// manually rather than extending this helper with mode flags.
func makeCacheFixture(t *testing.T, relPath, content, revision string) string {
	t.Helper()
	requireGit(t)
	cache := t.TempDir()
	makeMirror(t, cache, relPath, content)
	writeMarker(t, filepath.Join(cache, CacheRevisionMarkerName), revision+"\n")
	return cache
}

// makeMirror seeds <cache>/<CacheMirrorSubdir> with a freshly
// initialized git repository containing a single commit of
// (relPath → content). The subdirectory matches the refresh Job's
// layout so MaterializeWorkspaceFromCache finds the clone source at
// the right path. This is NOT a bare mirror (the real producer is);
// the phase-2 worker contract only requires that `git clone --local`
// of the path succeeds, which a non-bare repo also satisfies.
func makeMirror(t *testing.T, cache, relPath, content string) {
	t.Helper()
	requireGit(t)
	mirror := filepath.Join(cache, CacheMirrorSubdir)
	if err := os.MkdirAll(mirror, 0o755); err != nil {
		t.Fatalf("mkdir mirror: %v", err)
	}
	if out, err := runGit(mirror, "init", "-q", "-b", "main"); err != nil {
		t.Fatalf("git init mirror: %v\n%s", err, out)
	}
	if out, err := runGit(mirror, "config", "user.email", "fixture@example.com"); err != nil {
		t.Fatalf("git config email: %v\n%s", err, out)
	}
	if out, err := runGit(mirror, "config", "user.name", "fixture"); err != nil {
		t.Fatalf("git config name: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(mirror, relPath), []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
	if out, err := runGit(mirror, "add", relPath); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	if out, err := runGit(mirror, "commit", "-q", "-m", "init"); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
}

func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// Keep the child's env minimal: inherit PATH so `git` resolves,
	// overlay a trivial commiter identity for any commit that doesn't
	// take --author.
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// writeMarker creates (or truncates) a marker file at path with the
// given contents.
func writeMarker(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir marker parent: %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write marker %s: %v", path, err)
	}
}

// setRunContextEnv populates the canonical SPIRE_* runtime env vars so
// RunContextFromEnv() produces a stable identity for log assertions.
// t.Setenv handles restoration automatically.
func setRunContextEnv(t *testing.T) {
	t.Helper()
	t.Setenv(spireruntime.EnvTower, "test-tower")
	t.Setenv(spireruntime.EnvPrefix, "spi")
	t.Setenv(spireruntime.EnvBeadID, "spi-test")
	t.Setenv(spireruntime.EnvAttemptID, "att-1")
	t.Setenv(spireruntime.EnvRunID, "run-1")
	t.Setenv(spireruntime.EnvRole, string(spireruntime.RoleWizard))
	t.Setenv(spireruntime.EnvFormulaStep, "wizard")
	t.Setenv(spireruntime.EnvBackend, "operator-k8s")
	t.Setenv(spireruntime.EnvWorkspaceKind, string(spireruntime.WorkspaceKindOwnedWorktree))
	t.Setenv(spireruntime.EnvWorkspaceName, "wizard")
	t.Setenv(spireruntime.EnvWorkspaceOrigin, string(spireruntime.WorkspaceOriginGuildCache))
	t.Setenv(spireruntime.EnvHandoffMode, string(spireruntime.HandoffNone))
}

// captureAgentLog redirects the standard logger to a buffer for the
// duration of the test and returns that buffer. The default logger is
// restored on cleanup.
func captureAgentLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	origOut := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(origOut)
		log.SetFlags(origFlags)
	})
	return &buf
}

// assertLogContains fails the test if `logs` does not contain the given
// substring. The full log buffer is surfaced so failures are
// debuggable.
func assertLogContains(t *testing.T, logs, want string) {
	t.Helper()
	if !strings.Contains(logs, want) {
		t.Errorf("log missing %q; full log:\n%s", want, logs)
	}
}

// installSpireShim puts a fake `spire` binary on PATH for the duration
// of the test. The shim records its invocation argv into `argsFile`
// (one line per invocation, space-separated) and exits with
// `exitCode`. Used by BindLocalRepo tests to assert subprocess contract
// without requiring the real `spire` binary in the test environment.
//
// Skips on windows where `#!/bin/sh` shims don't execute.
func installSpireShim(t *testing.T, argsFile string, exitCode int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shim-based subprocess mocking not supported on windows")
	}
	shimDir := t.TempDir()
	shimPath := filepath.Join(shimDir, "spire")
	// The shim records every arg to argsFile separated by '\x1f' (unit
	// separator) within an invocation and '\n' between invocations, so a
	// flag value containing spaces survives round-trip.
	script := fmt.Sprintf(`#!/bin/sh
# fake spire for tests: record argv, exit %d.
IFS='%s'
printf '%%s\n' "$*" >> %q
exit %d
`, exitCode, " ", argsFile, exitCode)
	if err := os.WriteFile(shimPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	// Prepend the shim dir to PATH so the `spire` lookup in BindLocalRepo
	// resolves to our shim before any installed binary.
	orig := os.Getenv("PATH")
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+orig)
}

// readShimInvocations parses the shim's arg log into a slice of
// invocations (each invocation is a slice of string args).
func readShimInvocations(t *testing.T, argsFile string) [][]string {
	t.Helper()
	data, err := os.ReadFile(argsFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		t.Fatalf("read shim args: %v", err)
	}
	var out [][]string
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		out = append(out, strings.Fields(line))
	}
	return out
}
