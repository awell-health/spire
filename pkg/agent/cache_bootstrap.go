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

// Serialization markers the cache reconciler (spi-myzn5) writes into the
// guild cache root to signal readiness to workers. The reconciler owns
// the write side; this package only reads.
//
//   - CacheReadyMarker is touched last after a successful refresh; its
//     presence means the cache tree is complete and safe to bind. Its
//     contents, when present, are the cache revision/generation token
//     and are surfaced on LabelCacheRevision.
//   - CacheRefreshingMarker is present while a refresh is in flight.
//     Readers must treat the cache as unsafe until the marker is gone.
const (
	CacheReadyMarker      = "CACHE_READY"
	CacheRefreshingMarker = "CACHE_REFRESHING"
)

// ErrCacheUnavailable is returned when the guild repo cache at cachePath
// is not safe to read — either the ready marker is absent (no refresh
// has completed yet) or the refreshing marker is present (a reconciler
// run is in flight). Callers should fail the init container with this
// error; the operator will retry the pod once the reconciler republishes
// the ready marker.
var ErrCacheUnavailable = errors.New("agent: guild repo cache is unavailable (stale or mid-update)")

// MaterializeWorkspaceFromCache derives a writable working tree at
// workspacePath from the read-only guild repo cache at cachePath. The
// prefix argument is the canonical repo prefix (matching
// runtime.RepoIdentity.Prefix) — callers MUST supply it from the
// executor/runtime-contract surface rather than deriving it locally.
//
// Materialization strategy: a plain local clone `git clone --no-hardlinks <cache> <workspace>` is used instead of `git worktree add` because a worktree add would require writing to the cache's `.git/worktrees` registry (not possible on a read-only mount, and a shared-state hazard across pods).
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

	logPhase(run, StartupPhaseWorkspaceDerive, "cloning cache %s → workspace %s (prefix=%s)", cachePath, workspacePath, prefix)
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

	cmd := exec.CommandContext(ctx, "git", "clone", "--no-hardlinks", cachePath, workspacePath)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		emitBootstrapResult(run, "failure", time.Since(start), err)
		return fmt.Errorf("git clone from cache %s to %s: %w", cachePath, workspacePath, err)
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

// checkCacheReady inspects the cache-root markers written by the
// reconciler and returns the cache revision token (contents of
// CacheReadyMarker) when the cache is safe to read. Returns
// ErrCacheUnavailable when the ready marker is missing or the
// refreshing marker is present.
func checkCacheReady(cachePath string) (string, error) {
	if cachePath == "" {
		return "", fmt.Errorf("cachePath is required")
	}
	if _, err := os.Stat(cachePath); err != nil {
		return "", fmt.Errorf("%w: cache path %s: %v", ErrCacheUnavailable, cachePath, err)
	}
	if _, err := os.Stat(filepath.Join(cachePath, CacheRefreshingMarker)); err == nil {
		return "", fmt.Errorf("%w: %s marker present at %s", ErrCacheUnavailable, CacheRefreshingMarker, cachePath)
	}
	ready, err := os.ReadFile(filepath.Join(cachePath, CacheReadyMarker))
	if err != nil {
		return "", fmt.Errorf("%w: %s marker missing at %s: %v", ErrCacheUnavailable, CacheReadyMarker, cachePath, err)
	}
	return strings.TrimSpace(string(ready)), nil
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
