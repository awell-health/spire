// Package executor — runtime contract aliases onto pkg/runtime.
//
// IMPORT-CYCLE NOTE: the canonical runtime contract types (RepoIdentity,
// WorkspaceHandle, HandoffMode, RunContext, plus the WorkspaceKind /
// WorkspaceOrigin / SpawnRole enums they reference) live in pkg/runtime
// to resolve the cycle that would form if SpawnConfig in pkg/agent
// embedded executor-owned types: pkg/executor already imports pkg/agent
// for Backend/SpawnConfig, so reversing that edge would cycle. A
// neutral types-only pkg/runtime breaks the cycle; pkg/agent and
// pkg/executor both import it without importing each other in the
// problematic direction.
//
// The aliases below restore ergonomic executor.WorkspaceHandle /
// executor.RunContext references inside pkg/executor without relocating
// behavior. See docs/design/spi-xplwy-runtime-contract.md §1 for the
// contract spec and pkg/runtime/runtime_contract.go for the authoritative
// definitions.

package executor

import "github.com/awell-health/spire/pkg/runtime"

// Runtime contract type aliases. These are the SAME TYPES as in
// pkg/runtime — not conversions — so values flow across the
// pkg/executor ↔ pkg/agent boundary without casts.
type (
	RepoIdentity    = runtime.RepoIdentity
	WorkspaceKind   = runtime.WorkspaceKind
	WorkspaceOrigin = runtime.WorkspaceOrigin
	WorkspaceHandle = runtime.WorkspaceHandle
	HandoffMode     = runtime.HandoffMode
	RunContext      = runtime.RunContext
)

// Re-exported constants for executor call sites. NOTE: these are typed
// as WorkspaceKind / WorkspaceOrigin / HandoffMode, NOT plain strings,
// so they are distinct from the string-valued formula.WorkspaceKindRepo
// constants (which remain the source of truth for formula parsing).
const (
	WorkspaceKindRepo             = runtime.WorkspaceKindRepo
	WorkspaceKindOwnedWorktree    = runtime.WorkspaceKindOwnedWorktree
	WorkspaceKindBorrowedWorktree = runtime.WorkspaceKindBorrowedWorktree
	WorkspaceKindStaging          = runtime.WorkspaceKindStaging

	WorkspaceOriginLocalBind   = runtime.WorkspaceOriginLocalBind
	WorkspaceOriginOriginClone = runtime.WorkspaceOriginOriginClone
	WorkspaceOriginGuildCache  = runtime.WorkspaceOriginGuildCache

	HandoffNone         = runtime.HandoffNone
	HandoffBorrowed     = runtime.HandoffBorrowed
	HandoffBundle       = runtime.HandoffBundle
	HandoffTransitional = runtime.HandoffTransitional
)
