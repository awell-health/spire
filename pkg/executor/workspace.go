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
