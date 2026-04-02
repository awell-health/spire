package formula

import "fmt"

// Workspace kind constants.
const (
	WorkspaceKindRepo             = "repo"
	WorkspaceKindOwnedWorktree    = "owned_worktree"
	WorkspaceKindBorrowedWorktree = "borrowed_worktree"
	WorkspaceKindStaging          = "staging"
)

// Workspace scope constants.
const (
	WorkspaceScopeRun  = "run"  // created once per executor run, shared across steps
	WorkspaceScopeStep = "step" // created fresh per step, cleaned up after step
)

// Workspace cleanup constants.
const (
	WorkspaceCleanupAlways   = "always"   // clean up when scope ends
	WorkspaceCleanupTerminal = "terminal" // clean up only on terminal success
	WorkspaceCleanupNever    = "never"    // caller manages lifecycle
)

// validWorkspaceKinds is the set of allowed workspace kinds.
var validWorkspaceKinds = map[string]bool{
	WorkspaceKindRepo:             true,
	WorkspaceKindOwnedWorktree:    true,
	WorkspaceKindBorrowedWorktree: true,
	WorkspaceKindStaging:          true,
}

// validWorkspaceScopes is the set of allowed workspace scopes.
var validWorkspaceScopes = map[string]bool{
	WorkspaceScopeRun:  true,
	WorkspaceScopeStep: true,
}

// validWorkspaceCleanups is the set of allowed workspace cleanup policies.
var validWorkspaceCleanups = map[string]bool{
	WorkspaceCleanupAlways:   true,
	WorkspaceCleanupTerminal: true,
	WorkspaceCleanupNever:    true,
}

// WorkspaceDecl declares a named workspace in a v3 formula.
type WorkspaceDecl struct {
	Kind      string `toml:"kind"`                // repo, owned_worktree, borrowed_worktree, staging
	Branch    string `toml:"branch,omitempty"`    // branch pattern, e.g. "staging/{vars.bead_id}"
	Base      string `toml:"base,omitempty"`      // base branch, e.g. "{vars.base_branch}"
	Scope     string `toml:"scope,omitempty"`     // "run" (default) or "step"
	Ownership string `toml:"ownership,omitempty"` // "owned" (default) or "borrowed"
	Cleanup   string `toml:"cleanup,omitempty"`   // "always", "terminal" (default), "never"
}

// DefaultWorkspaceDecl fills zero-value fields with defaults.
// Exported for tests and for use in ParseFormulaStepGraph.
func DefaultWorkspaceDecl(decl *WorkspaceDecl) {
	if decl.Scope == "" {
		decl.Scope = WorkspaceScopeRun
	}
	if decl.Ownership == "" {
		decl.Ownership = "owned"
	}
	if decl.Cleanup == "" {
		decl.Cleanup = WorkspaceCleanupTerminal
	}
}

// ValidateWorkspaces checks workspace declarations for structural correctness.
func ValidateWorkspaces(workspaces map[string]WorkspaceDecl) error {
	for name, ws := range workspaces {
		if !validWorkspaceKinds[ws.Kind] {
			return fmt.Errorf("workspace %q: invalid kind %q", name, ws.Kind)
		}
		if !validWorkspaceScopes[ws.Scope] {
			return fmt.Errorf("workspace %q: invalid scope %q", name, ws.Scope)
		}
		if ws.Ownership != "owned" && ws.Ownership != "borrowed" {
			return fmt.Errorf("workspace %q: invalid ownership %q", name, ws.Ownership)
		}
		if !validWorkspaceCleanups[ws.Cleanup] {
			return fmt.Errorf("workspace %q: invalid cleanup %q", name, ws.Cleanup)
		}

		// repo kind must not declare branch/base.
		if ws.Kind == WorkspaceKindRepo {
			if ws.Branch != "" || ws.Base != "" {
				return fmt.Errorf("workspace %q: repo kind must not declare branch or base", name)
			}
		}

		// borrowed_worktree must have ownership "borrowed".
		if ws.Kind == WorkspaceKindBorrowedWorktree && ws.Ownership != "borrowed" {
			return fmt.Errorf("workspace %q: borrowed_worktree must have ownership \"borrowed\"", name)
		}
	}
	return nil
}
