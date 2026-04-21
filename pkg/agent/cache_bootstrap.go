package agent

import (
	"context"
	"errors"
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

// MaterializeWorkspaceFromCache derives a writable working tree at
// workspacePath from the read-only guild repo cache at cachePath. The
// prefix argument is the canonical repo prefix (matching
// runtime.RepoIdentity.Prefix) — callers MUST supply it from the
// executor/runtime-contract surface rather than deriving it locally.
//
// The stub body returns a not-implemented error; spi-jetfb supplies the
// working implementation.
func MaterializeWorkspaceFromCache(ctx context.Context, cachePath, workspacePath, prefix string) error {
	return errors.New("not implemented")
}

// BindLocalRepo performs the local-only bind/bootstrap steps a wizard
// pod needs after its workspace is materialized (beads-dir wiring,
// local config). It MUST NOT call `spire repo add` or mutate shared
// repo registration — repo identity is supplied by the caller via
// prefix, which is resolved upstream from the canonical runtime
// contract (spi-xplwy).
//
// The stub body returns a not-implemented error; spi-jetfb supplies the
// working implementation.
func BindLocalRepo(ctx context.Context, workspacePath, prefix string) error {
	return errors.New("not implemented")
}
