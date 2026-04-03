package workshop

// --- v3 known-value functions ---

// KnownStepKinds returns the recognized v3 step kinds.
func KnownStepKinds() []string {
	return []string{"op", "dispatch", "call"}
}

// KnownActions returns the recognized v3 executor opcodes.
func KnownActions() []string {
	return []string{
		"check.design-linked",
		"wizard.run",
		"beads.materialize_plan",
		"dispatch.children",
		"verify.run",
		"graph.run",
		"git.merge_to_main",
		"bead.finish",
	}
}

// KnownWorkspaceKinds returns the recognized workspace kinds.
func KnownWorkspaceKinds() []string {
	return []string{"repo", "owned_worktree", "borrowed_worktree", "staging"}
}

// KnownVarTypes returns the recognized formula variable types.
func KnownVarTypes() []string {
	return []string{"bead_id", "string", "int", "bool"}
}

// KnownWorkspaceScopes returns the recognized workspace scopes.
func KnownWorkspaceScopes() []string {
	return []string{"step", "run"}
}

// KnownWorkspaceOwnerships returns the recognized workspace ownership values.
func KnownWorkspaceOwnerships() []string {
	return []string{"owned", "borrowed"}
}

// KnownWorkspaceCleanups returns the recognized workspace cleanup policies.
func KnownWorkspaceCleanups() []string {
	return []string{"always", "terminal", "never"}
}

// KnownWhenOps returns the recognized structured condition operators.
func KnownWhenOps() []string {
	return []string{"eq", "ne", "lt", "gt", "le", "ge"}
}
