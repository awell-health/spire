package executor

// executor workspace.go — v3 workspace contract: runtime state types, resolution, and cleanup.

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/awell-health/spire/pkg/formula"
	spgit "github.com/awell-health/spire/pkg/git"
)

// InitWorkspaceStates initializes State.Workspaces from a formula's workspace
// declarations. Each declared workspace gets a "pending" WorkspaceState entry.
// Called once when starting a v3 formula execution.
func (e *Executor) InitWorkspaceStates(workspaces map[string]formula.WorkspaceDecl) {
	if e.state.Workspaces == nil {
		e.state.Workspaces = make(map[string]WorkspaceState)
	}
	for name, decl := range workspaces {
		if _, exists := e.state.Workspaces[name]; exists {
			continue // already initialized (resume path)
		}
		e.state.Workspaces[name] = WorkspaceState{
			Name:       name,
			Kind:       decl.Kind,
			Branch:     decl.Branch,
			BaseBranch: decl.Base,
			Status:     "pending",
			Scope:      decl.Scope,
			Ownership:  decl.Ownership,
			Cleanup:    decl.Cleanup,
		}
	}
}

// resolveWorkspace looks up a workspace name in the executor's runtime state,
// creates or resumes the corresponding pkg/git object, persists WorkspaceState,
// and returns the *spgit.WorktreeContext (nil for "repo" kind).
//
// Resolution rules:
//   - scope=run + already active → resume (capture fresh StartSHA for session baseline)
//   - scope=run + pending → create, set active
//   - scope=step → always create fresh, set active
//   - borrowed_worktree → ResumeWorktreeContext (does not own lifecycle)
//   - owned_worktree → NewStagingWorktreeAt (owns lifecycle)
//   - staging → NewStagingWorktreeAt with staging branch semantics
func (e *Executor) resolveWorkspace(name string) (*spgit.WorktreeContext, error) {
	ws, ok := e.state.Workspaces[name]
	if !ok {
		return nil, fmt.Errorf("workspace %q not found in state", name)
	}

	// For repo kind, just set Dir to the main repo path and return nil context.
	if ws.Kind == formula.WorkspaceKindRepo {
		ws.Dir = e.state.RepoPath
		ws.Status = "active"
		e.state.Workspaces[name] = ws
		return nil, nil
	}

	// Active + scope=run → resume existing worktree, refresh session baseline.
	if ws.Status == "active" && ws.Scope == formula.WorkspaceScopeRun {
		if ws.Kind == formula.WorkspaceKindBorrowedWorktree {
			wc, err := spgit.ResumeWorktreeContext(ws.Dir, ws.Branch, ws.BaseBranch, e.state.RepoPath, e.log)
			if err != nil {
				return nil, fmt.Errorf("resume borrowed workspace %q: %w", name, err)
			}
			ws.StartSHA = wc.StartSHA
			e.state.Workspaces[name] = ws
			return wc, nil
		}
		// Owned or staging — resume via ResumeStagingWorktree for session baseline.
		sw := spgit.ResumeStagingWorktree(e.state.RepoPath, ws.Dir, ws.Branch, ws.BaseBranch, e.log)
		ws.StartSHA = sw.StartSHA
		e.state.Workspaces[name] = ws
		return &sw.WorktreeContext, nil
	}

	// Active + scope=step should not happen — the previous step should have released it.
	if ws.Status == "active" && ws.Scope == formula.WorkspaceScopeStep {
		return nil, fmt.Errorf("workspace %q (scope=step) is still active — should have been released", name)
	}

	// Pending → create fresh workspace.
	if ws.Status != "pending" {
		return nil, fmt.Errorf("workspace %q has unexpected status %q", name, ws.Status)
	}

	// Resolve branch and base branch patterns.
	branch := e.resolveWorkspaceBranch(ws.Branch)
	baseBranch := ws.BaseBranch
	if baseBranch == "" {
		baseBranch = e.state.BaseBranch
	} else {
		baseBranch = e.resolveWorkspaceBranch(baseBranch)
	}
	dir := e.workspaceDir(name)

	archName, archEmail := ArchmageIdentity(e.deps)

	switch ws.Kind {
	case formula.WorkspaceKindBorrowedWorktree:
		wc, err := spgit.ResumeWorktreeContext(dir, branch, baseBranch, e.state.RepoPath, e.log)
		if err != nil {
			return nil, fmt.Errorf("resume borrowed workspace %q: %w", name, err)
		}
		ws.Dir = dir
		ws.Branch = branch
		ws.BaseBranch = baseBranch
		ws.StartSHA = wc.StartSHA
		ws.Status = "active"
		e.state.Workspaces[name] = ws
		return wc, nil

	case formula.WorkspaceKindOwnedWorktree, formula.WorkspaceKindStaging:
		// Force-create the branch from base before creating worktree.
		rc := &spgit.RepoContext{Dir: e.state.RepoPath, BaseBranch: baseBranch, Log: e.log}
		if err := rc.ForceBranch(branch, baseBranch); err != nil {
			return nil, fmt.Errorf("create branch for workspace %q: %w", name, err)
		}

		sw, err := spgit.NewStagingWorktreeAt(
			e.state.RepoPath, dir, branch, baseBranch,
			archName, archEmail, e.log,
		)
		if err != nil {
			return nil, fmt.Errorf("create workspace %q: %w", name, err)
		}
		// Capture session baseline SHA (NewStagingWorktreeAt doesn't set StartSHA).
		startSHA := captureHeadSHA(sw.Dir)
		sw.StartSHA = startSHA
		ws.Dir = dir
		ws.Branch = branch
		ws.BaseBranch = baseBranch
		ws.StartSHA = startSHA
		ws.Status = "active"
		e.state.Workspaces[name] = ws
		return &sw.WorktreeContext, nil

	default:
		return nil, fmt.Errorf("workspace %q: unsupported kind %q for creation", name, ws.Kind)
	}
}

// releaseWorkspace applies the cleanup policy for a workspace.
// Called when a step completes (scope=step) or when the executor exits (scope=run).
//
// Cleanup rules:
//   - "always" → close worktree immediately
//   - "terminal" → close only if executor is on a terminal success path
//   - "never" → mark closed in state but do not remove worktree
//   - borrowed ownership → never removes worktree regardless of cleanup policy
func (e *Executor) releaseWorkspace(name string) error {
	ws, ok := e.state.Workspaces[name]
	if !ok {
		return fmt.Errorf("workspace %q not found in state", name)
	}
	if ws.Status == "closed" {
		return nil // already released
	}

	// Borrowed workspaces: mark closed but never remove the worktree.
	if ws.Ownership == "borrowed" {
		ws.Status = "closed"
		e.state.Workspaces[name] = ws
		return nil
	}

	// Repo kind: just mark closed, nothing to clean up.
	if ws.Kind == formula.WorkspaceKindRepo {
		ws.Status = "closed"
		e.state.Workspaces[name] = ws
		return nil
	}

	shouldRemove := false
	switch ws.Cleanup {
	case formula.WorkspaceCleanupAlways:
		shouldRemove = true
	case formula.WorkspaceCleanupTerminal:
		shouldRemove = e.terminated
	case formula.WorkspaceCleanupNever:
		shouldRemove = false
	}

	if shouldRemove && ws.Dir != "" {
		sw := spgit.ResumeStagingWorktree(e.state.RepoPath, ws.Dir, ws.Branch, ws.BaseBranch, e.log)
		sw.Close()
	}

	ws.Status = "closed"
	e.state.Workspaces[name] = ws
	return nil
}

// resolveWorkspaceBranch interpolates a branch pattern like "staging/{vars.bead_id}"
// using the executor's runtime vars and state.
func (e *Executor) resolveWorkspaceBranch(pattern string) string {
	if pattern == "" {
		return ""
	}
	r := strings.NewReplacer(
		"{vars.bead_id}", e.beadID,
		"{vars.base_branch}", e.state.BaseBranch,
	)
	return r.Replace(pattern)
}

// releaseStepWorkspaces releases all step-scoped workspaces after a step completes.
func (e *Executor) releaseStepWorkspaces(stepName string) error {
	var errs []string
	for name, ws := range e.state.Workspaces {
		if ws.Scope == formula.WorkspaceScopeStep && ws.Status == "active" {
			if err := e.releaseWorkspace(name); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %s", name, err))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("release step workspaces for %s: %s", stepName, strings.Join(errs, "; "))
	}
	return nil
}

// releaseRunWorkspaces releases all run-scoped workspaces on executor exit.
func (e *Executor) releaseRunWorkspaces() error {
	var errs []string
	for name, ws := range e.state.Workspaces {
		if ws.Scope == formula.WorkspaceScopeRun && ws.Status == "active" {
			if err := e.releaseWorkspace(name); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %s", name, err))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("release run workspaces: %s", strings.Join(errs, "; "))
	}
	return nil
}

// --- Graph (v3) workspace helpers ---
// These methods operate on GraphState.Workspaces (the v3 path) instead of
// the v2 State.Workspaces field. They mirror the v2 resolveWorkspace /
// releaseWorkspace logic but accept an explicit *GraphState parameter so
// the v2 State does not need to be populated.

// resolveGraphWorkspace looks up a workspace name in the graph state, creates
// or resumes the corresponding git workspace, updates the GraphState entry,
// and returns the workspace directory path.
func (e *Executor) resolveGraphWorkspace(name string, state *GraphState) (string, error) {
	ws, ok := state.Workspaces[name]
	if !ok {
		return "", fmt.Errorf("workspace %q not found in graph state", name)
	}

	// For repo kind, set Dir to the main repo path and return.
	if ws.Kind == formula.WorkspaceKindRepo {
		ws.Dir = state.RepoPath
		ws.Status = "active"
		state.Workspaces[name] = ws
		return ws.Dir, nil
	}

	if ws.Kind == formula.WorkspaceKindStaging {
		branch := e.resolveGraphWorkspaceBranch(ws.Branch, state)
		baseBranch := ws.BaseBranch
		if baseBranch == "" {
			baseBranch = state.BaseBranch
		} else {
			baseBranch = e.resolveGraphWorkspaceBranch(baseBranch, state)
		}
		if branch != "" {
			state.StagingBranch = branch
		}
		if baseBranch != "" {
			state.BaseBranch = baseBranch
		}
		if ws.Dir == "" {
			ws.Dir = filepath.Join(state.RepoPath, ".worktrees", e.beadID+"-"+name)
			state.Workspaces[name] = ws
		}

		sw, err := e.ensureGraphStagingWorktree(state)
		if err != nil {
			return "", fmt.Errorf("ensure staging workspace %q: %w", name, err)
		}

		ws = state.Workspaces[name]
		ws.Name = name
		ws.Kind = formula.WorkspaceKindStaging
		ws.Dir = sw.Dir
		ws.Branch = state.StagingBranch
		ws.BaseBranch = state.BaseBranch
		ws.StartSHA = sw.StartSHA
		ws.Status = "active"
		state.Workspaces[name] = ws
		return sw.Dir, nil
	}

	// Active + scope=run → already resolved, return existing Dir.
	if ws.Status == "active" && ws.Scope == formula.WorkspaceScopeRun && ws.Dir != "" {
		return ws.Dir, nil
	}

	// Active + scope=step should not happen.
	if ws.Status == "active" && ws.Scope == formula.WorkspaceScopeStep {
		return "", fmt.Errorf("workspace %q (scope=step) is still active — should have been released", name)
	}

	// Pending → create fresh workspace.
	if ws.Status != "pending" {
		return "", fmt.Errorf("workspace %q has unexpected status %q", name, ws.Status)
	}

	branch := e.resolveGraphWorkspaceBranch(ws.Branch, state)
	baseBranch := ws.BaseBranch
	if baseBranch == "" {
		baseBranch = state.BaseBranch
	} else {
		baseBranch = e.resolveGraphWorkspaceBranch(baseBranch, state)
	}

	dir := ws.Dir
	if dir == "" {
		dir = filepath.Join(state.RepoPath, ".worktrees", e.beadID+"-"+name)
	}

	archName, archEmail := ArchmageIdentity(e.deps)

	switch ws.Kind {
	case formula.WorkspaceKindBorrowedWorktree:
		wc, err := spgit.ResumeWorktreeContext(dir, branch, baseBranch, state.RepoPath, e.log)
		if err != nil {
			return "", fmt.Errorf("resume borrowed workspace %q: %w", name, err)
		}
		ws.Dir = dir
		ws.Branch = branch
		ws.BaseBranch = baseBranch
		ws.StartSHA = wc.StartSHA
		ws.Status = "active"
		state.Workspaces[name] = ws
		return dir, nil

	case formula.WorkspaceKindOwnedWorktree, formula.WorkspaceKindStaging:
		rc := &spgit.RepoContext{Dir: state.RepoPath, BaseBranch: baseBranch, Log: e.log}
		if err := rc.ForceBranch(branch, baseBranch); err != nil {
			return "", fmt.Errorf("create branch for workspace %q: %w", name, err)
		}

		sw, err := spgit.NewStagingWorktreeAt(
			state.RepoPath, dir, branch, baseBranch,
			archName, archEmail, e.log,
		)
		if err != nil {
			return "", fmt.Errorf("create workspace %q: %w", name, err)
		}
		startSHA := captureHeadSHA(sw.Dir)
		sw.StartSHA = startSHA
		ws.Dir = dir
		ws.Branch = branch
		ws.BaseBranch = baseBranch
		ws.StartSHA = startSHA
		ws.Status = "active"
		state.Workspaces[name] = ws
		return dir, nil

	default:
		return "", fmt.Errorf("workspace %q: unsupported kind %q for creation", name, ws.Kind)
	}
}

// resolveGraphWorkspaceBranch interpolates a branch pattern using graph state
// vars (e.g. "staging/{vars.bead_id}" → "staging/spi-abc").
func (e *Executor) resolveGraphWorkspaceBranch(pattern string, state *GraphState) string {
	if pattern == "" {
		return ""
	}
	r := strings.NewReplacer(
		"{vars.bead_id}", e.beadID,
		"{vars.base_branch}", state.BaseBranch,
	)
	return r.Replace(pattern)
}

// releaseGraphRunWorkspaces releases all run-scoped workspaces in the graph state.
// Called as a deferred cleanup in RunGraph.
func (e *Executor) releaseGraphRunWorkspaces(state *GraphState) {
	for name, ws := range state.Workspaces {
		if ws.Scope == formula.WorkspaceScopeRun && ws.Status == "active" {
			if err := e.releaseGraphWorkspace(name, state); err != nil {
				e.log("warning: release graph workspace %s: %s", name, err)
			}
		}
	}
}

// releaseGraphWorkspace applies the cleanup policy for a graph workspace.
func (e *Executor) releaseGraphWorkspace(name string, state *GraphState) error {
	ws, ok := state.Workspaces[name]
	if !ok {
		return fmt.Errorf("workspace %q not found in graph state", name)
	}
	if ws.Status == "closed" {
		return nil
	}

	// Borrowed workspaces: mark closed but never remove the worktree.
	if ws.Ownership == "borrowed" {
		ws.Status = "closed"
		state.Workspaces[name] = ws
		return nil
	}

	// Repo kind: just mark closed.
	if ws.Kind == formula.WorkspaceKindRepo {
		ws.Status = "closed"
		state.Workspaces[name] = ws
		return nil
	}

	shouldRemove := false
	shouldClose := true
	switch ws.Cleanup {
	case formula.WorkspaceCleanupAlways:
		shouldRemove = true
	case formula.WorkspaceCleanupTerminal:
		shouldRemove = e.terminated
		// When cleanup="terminal" and the executor did NOT terminate (interrupt/
		// failure), leave the workspace "active" so resume can find and reuse it.
		// Marking it "closed" would poison resume: resolveGraphWorkspace rejects
		// any status other than "pending" or "active".
		if !e.terminated {
			shouldClose = false
		}
	case formula.WorkspaceCleanupNever:
		shouldRemove = false
		shouldClose = false
	}

	if shouldRemove && ws.Dir != "" {
		sw := spgit.ResumeStagingWorktree(state.RepoPath, ws.Dir, ws.Branch, ws.BaseBranch, e.log)
		sw.Close()
	}

	if shouldClose {
		ws.Status = "closed"
		state.Workspaces[name] = ws
	}
	return nil
}

// captureHeadSHA reads the current HEAD SHA in the given directory.
func captureHeadSHA(dir string) string {
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output(); err == nil {
		return strings.TrimSpace(string(out))
	}
	return ""
}

// workspaceDir returns the worktree directory path for a workspace name.
// For owned/staging kinds: .worktrees/<bead-id>-<name>
// For borrowed: reads Dir from persisted state.
func (e *Executor) workspaceDir(name string) string {
	ws, ok := e.state.Workspaces[name]
	if ok && ws.Dir != "" {
		return ws.Dir
	}
	return filepath.Join(e.state.RepoPath, ".worktrees", e.beadID+"-"+name)
}
