package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/runtime"
)

// Phase-2 cluster repo-cache contract (spi-sn7o3).
//
// Cluster wizard pods mount a read-only guild-owned repo cache at
// CacheMountPath and derive a writable per-pod workspace at
// WorkspaceMountPath before the main container starts. The helpers below
// are the interface the operator-managed init container invokes to
// perform that derivation. Repo identity (prefix, tower, base branch) is
// established by the canonical runtime-contract vocabulary from
// spi-xplwy — this contract composes with it and does not redefine it.
const (
	// CacheMountPath is the read-only mount point where the guild
	// repo cache is surfaced inside a wizard pod.
	CacheMountPath = "/spire/cache"

	// WorkspaceMountPath is the writable mount point where the
	// per-pod execution workspace is materialized from the cache.
	WorkspaceMountPath = "/spire/workspace"
)

// Canonical intra-PVC layout for the guild repo cache. These names are
// the single source of truth consumed by both sides of the phase-2
// contract (spi-sn7o3, spi-yzmq0):
//
//   - The operator's refresh Job receives them as SPIRE_CACHE_* env
//     vars (see cache_reconciler.go) and its shell script references
//     only those env vars — no intra-PVC layout string literals live
//     in shell.
//   - The worker-side helpers in this file consume the constants
//     directly.
//
// Layout, relative to the cache PVC's mount point (producer mounts at
// /cache, consumer at CacheMountPath — mount points are per-container,
// not part of this contract):
//
//   - <mount>/<CacheMirrorSubdir>/            bare git mirror the
//                                             refresh Job maintains;
//                                             workers clone from here.
//   - <mount>/<CacheRevisionMarkerName>       atomic-rename-published
//                                             file whose contents are
//                                             the current cache revision
//                                             (git commit SHA). Its
//                                             presence is the sole
//                                             "cache is ready" signal;
//                                             absence means not yet
//                                             refreshed.
//   - <mount>/<CacheRevisionTmpMarkerName>    intermediate write target
//                                             the refresh Job uses
//                                             before the atomic rename.
//                                             Never read by workers.
//
// There is no separate in-flight / refreshing sentinel: the atomic
// rename of tmp→final means a worker inspecting the layout at any
// moment sees either the old revision, the new revision, or NotFound
// — never a partial state. "Refreshing" visibility is surfaced via
// CacheStatus.Phase on the operator side; workers do not observe it
// through a marker file.
const (
	CacheMirrorSubdir          = "mirror"
	CacheRevisionMarkerName    = ".spire-cache-revision"
	CacheRevisionTmpMarkerName = ".spire-cache-revision.tmp"
)

// ErrCacheUnavailable is returned when the guild repo cache at cachePath
// is not safe to read — the revision marker is absent, so no refresh
// has completed yet. Callers should fail the init container with this
// error; the operator will retry the pod once the reconciler publishes
// the marker.
var ErrCacheUnavailable = errors.New("agent: guild repo cache is unavailable (no refresh has completed)")

// MaterializeWorkspaceFromCache derives a writable working tree at
// workspacePath from the read-only guild repo cache at cachePath. The
// prefix argument is the canonical repo prefix (matching
// runtime.RepoIdentity.Prefix) — callers MUST supply it from the
// executor/runtime-contract surface rather than deriving it locally.
//
// Materialization strategy: a plain local clone `git clone --no-hardlinks <cache>/<CacheMirrorSubdir> <workspace>` is used instead of `git worktree add` because a worktree add would require writing to the cache's `.git/worktrees` registry (not possible on a read-only mount, and a shared-state hazard across pods).
func MaterializeWorkspaceFromCache(ctx context.Context, cachePath, workspacePath, prefix string) error {
	run := runtime.RunContextFromEnv()
	if run.Prefix == "" {
		run.Prefix = prefix
	}

	start := time.Now()
	logPhase(run, StartupPhaseCacheReady, "checking cache readiness at %s", cachePath)

	revision, err := checkCacheReady(cachePath)
	if err != nil {
		emitBootstrapResult(run, "failure", time.Since(start), err)
		return err
	}
	logCacheFreshness(run, revision, "fresh")

	mirrorPath := filepath.Join(cachePath, CacheMirrorSubdir)
	logPhase(run, StartupPhaseWorkspaceDerive, "cloning cache mirror %s → workspace %s (prefix=%s)", mirrorPath, workspacePath, prefix)
	if err := os.MkdirAll(filepath.Dir(workspacePath), 0o755); err != nil {
		emitBootstrapResult(run, "failure", time.Since(start), err)
		return fmt.Errorf("mkdir workspace parent: %w", err)
	}

	// Skip the clone when an existing workspace is already present — the
	// init container is idempotent so pod restarts do not redo the clone.
	if _, err := os.Stat(filepath.Join(workspacePath, ".git")); err == nil {
		logPhase(run, StartupPhaseWorkspaceDerive, "workspace %s already materialized; skipping clone", workspacePath)
		emitBootstrapResult(run, "success", time.Since(start), nil)
		return nil
	}

	cmd := exec.CommandContext(ctx, "git", "clone", "--no-hardlinks", mirrorPath, workspacePath)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		emitBootstrapResult(run, "failure", time.Since(start), err)
		return fmt.Errorf("git clone from cache mirror %s to %s: %w", mirrorPath, workspacePath, err)
	}

	emitBootstrapResult(run, "success", time.Since(start), nil)
	return nil
}

// BindLocalRepo performs the local-only bind/bootstrap steps a wizard
// pod needs after its workspace is materialized (beads-dir wiring,
// local config). It MUST NOT call `spire repo add` or mutate shared
// repo registration — repo identity is supplied by the caller via
// prefix, which is resolved upstream from the canonical runtime
// contract (spi-xplwy).
//
// Implementation: shells out to `spire repo bind-local`, the existing
// local-only bind entrypoint (cmd/spire/repo_bind_local.go). That
// command writes only to the tower's LocalBindings and the global
// Instances map; it never touches the shared `repos` dolt table.
func BindLocalRepo(ctx context.Context, workspacePath, prefix string) error {
	run := runtime.RunContextFromEnv()
	if run.Prefix == "" {
		run.Prefix = prefix
	}

	start := time.Now()
	logPhase(run, StartupPhaseLocalBindBootstrap, "binding prefix=%s at %s", prefix, workspacePath)

	repoURL := os.Getenv("SPIRE_REPO_URL")
	branch := os.Getenv("SPIRE_REPO_BRANCH")
	if prefix == "" {
		err := fmt.Errorf("prefix is required")
		emitBootstrapResult(run, "failure", time.Since(start), err)
		return err
	}
	if workspacePath == "" {
		err := fmt.Errorf("workspacePath is required")
		emitBootstrapResult(run, "failure", time.Since(start), err)
		return err
	}
	if repoURL == "" {
		err := fmt.Errorf("SPIRE_REPO_URL env is required for local bind")
		emitBootstrapResult(run, "failure", time.Since(start), err)
		return err
	}
	if branch == "" {
		err := fmt.Errorf("SPIRE_REPO_BRANCH env is required for local bind")
		emitBootstrapResult(run, "failure", time.Since(start), err)
		return err
	}

	cmd := exec.CommandContext(ctx, "spire", "repo", "bind-local",
		"--prefix", prefix,
		"--path", workspacePath,
		"--repo-url", repoURL,
		"--branch", branch,
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		emitBootstrapResult(run, "failure", time.Since(start), err)
		return fmt.Errorf("spire repo bind-local prefix=%s path=%s: %w", prefix, workspacePath, err)
	}

	emitBootstrapResult(run, "success", time.Since(start), nil)
	return nil
}

// checkCacheReady inspects the cache-root revision marker written by
// the reconciler and returns the cache revision token (contents of
// <cachePath>/<CacheRevisionMarkerName>) when the cache is safe to
// read. Returns ErrCacheUnavailable when the marker is missing or the
// cachePath itself does not exist.
//
// The refresh Job publishes the marker via an atomic rename of a
// sibling .tmp file, so workers observing the cache at any moment see
// either a previous revision, the new revision, or NotFound — never a
// truncated read. The .tmp sibling is deliberately NOT treated as a
// ready signal: its presence alone does not mean the refresh completed.
func checkCacheReady(cachePath string) (string, error) {
	if cachePath == "" {
		return "", fmt.Errorf("cachePath is required")
	}
	if _, err := os.Stat(cachePath); err != nil {
		return "", fmt.Errorf("%w: cache path %s: %v", ErrCacheUnavailable, cachePath, err)
	}
	revision, err := os.ReadFile(filepath.Join(cachePath, CacheRevisionMarkerName))
	if err != nil {
		return "", fmt.Errorf("%w: %s marker missing at %s: %v", ErrCacheUnavailable, CacheRevisionMarkerName, cachePath, err)
	}
	return strings.TrimSpace(string(revision)), nil
}

// logPhase emits a structured log line tagged with the canonical
// startup-phase label, bootstrap source, and the canonical runtime
// identity fields (tower/prefix/role/backend/...). This is the
// observability surface for the cache→workspace bootstrap; the init
// container is short-lived so a full Prometheus registration is not
// warranted — the canonical log fields are the audit trail.
func logPhase(run runtime.RunContext, phase, format string, a ...interface{}) {
	msg := fmt.Sprintf(format, a...)
	log.Printf("[cache-bootstrap] phase=%s source=%s %s%s",
		phase, BootstrapSourceGuildCache, msg, runtime.LogFields(run))
}

// logCacheFreshness emits the cache-freshness / revision labels at bind
// time. Revision is high-cardinality for metrics (it rotates per cache
// refresh) so it is logged, not used as a metric label.
func logCacheFreshness(run runtime.RunContext, revision, freshness string) {
	log.Printf("[cache-bootstrap] %s=%s %s=%s source=%s%s",
		LabelCacheRevision, revision,
		LabelCacheFreshness, freshness,
		BootstrapSourceGuildCache,
		runtime.LogFields(run))
}

// emitBootstrapResult records the terminal outcome (duration + result)
// of a bootstrap phase under the MetricBootstrapDuration and
// MetricBootstrapSuccess names. Uses low-cardinality labels only
// (tower, prefix, role, backend, workspace_kind, handoff_mode) per the
// cardinality rules in cache_observability.go and spi-xplwy §1.4. The
// emission is a structured log line rather than a Prometheus register
// call because the init container is too short-lived to scrape.
func emitBootstrapResult(run runtime.RunContext, result string, dur time.Duration, cause error) {
	labels := runtime.MetricLabelsString(run)
	log.Printf("[cache-bootstrap] metric=%s value=%0.3f result=%s source=%s labels=%s%s",
		MetricBootstrapDuration, dur.Seconds(), result, BootstrapSourceGuildCache, labels, runtime.LogFields(run))
	log.Printf("[cache-bootstrap] metric=%s result=%s source=%s labels=%s%s",
		MetricBootstrapSuccess, result, BootstrapSourceGuildCache, labels, runtime.LogFields(run))
	if cause != nil {
		log.Printf("[cache-bootstrap] error=%v source=%s%s", cause, BootstrapSourceGuildCache, runtime.LogFields(run))
	}
}
