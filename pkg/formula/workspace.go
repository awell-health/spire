package formula

import "fmt"

// WorkspaceDecl declares a named workspace that steps can reference.
type WorkspaceDecl struct {
	Kind      string `toml:"kind"`                // "repo", "owned_worktree", "borrowed_worktree", "staging"
	Branch    string `toml:"branch,omitempty"`    // template: "epic/{vars.bead_id}"
	Base      string `toml:"base,omitempty"`      // template: "{vars.base_branch}"
	Scope     string `toml:"scope,omitempty"`     // "step" | "run"
	Ownership string `toml:"ownership,omitempty"` // "owned" | "borrowed"
	Cleanup   string `toml:"cleanup,omitempty"`   // "always" | "terminal" | "never"
}

// Constants for workspace kind values.
const (
	WorkspaceKindRepo             = "repo"
	WorkspaceKindOwnedWorktree    = "owned_worktree"
	WorkspaceKindBorrowedWorktree = "borrowed_worktree"
	WorkspaceKindStaging          = "staging"
)

// Constants for workspace scope values.
const (
	WorkspaceScopeStep = "step"
	WorkspaceScopeRun  = "run"
)

// Constants for workspace ownership values.
const (
	WorkspaceOwnershipOwned    = "owned"
	WorkspaceOwnershipBorrowed = "borrowed"
)

// Constants for workspace cleanup values.
const (
	WorkspaceCleanupAlways   = "always"
	WorkspaceCleanupTerminal = "terminal"
	WorkspaceCleanupNever    = "never"
)

var validWorkspaceKinds = map[string]bool{
	WorkspaceKindRepo:             true,
	WorkspaceKindOwnedWorktree:    true,
	WorkspaceKindBorrowedWorktree: true,
	WorkspaceKindStaging:          true,
}

var validWorkspaceScopes = map[string]bool{
	"":                 true,
	WorkspaceScopeStep: true,
	WorkspaceScopeRun:  true,
}

var validWorkspaceOwnerships = map[string]bool{
	"":                         true,
	WorkspaceOwnershipOwned:    true,
	WorkspaceOwnershipBorrowed: true,
}

var validWorkspaceCleanups = map[string]bool{
	"":                       true,
	WorkspaceCleanupAlways:   true,
	WorkspaceCleanupTerminal: true,
	WorkspaceCleanupNever:    true,
}

// ValidateWorkspaces checks that all workspace declarations use valid field values.
func ValidateWorkspaces(workspaces map[string]WorkspaceDecl) error {
	for name, ws := range workspaces {
		if ws.Kind == "" {
			return fmt.Errorf("workspace %q: kind is required", name)
		}
		if !validWorkspaceKinds[ws.Kind] {
			return fmt.Errorf("workspace %q: invalid kind %q", name, ws.Kind)
		}
		if !validWorkspaceScopes[ws.Scope] {
			return fmt.Errorf("workspace %q: invalid scope %q", name, ws.Scope)
		}
		if !validWorkspaceOwnerships[ws.Ownership] {
			return fmt.Errorf("workspace %q: invalid ownership %q", name, ws.Ownership)
		}
		if !validWorkspaceCleanups[ws.Cleanup] {
			return fmt.Errorf("workspace %q: invalid cleanup %q", name, ws.Cleanup)
		}
	}
	return nil
}
