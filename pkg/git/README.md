# pkg/git

Git worktree, branch, and repository operations for Spire.

## What this package owns

- **Worktree lifecycle**: creating, resuming, and removing git worktrees.
- **Branch operations**: creating, deleting, checking out, force-resetting branches.
- **Merge mechanics**: fast-forward, rebase, conflict detection.
- **Staging worktrees**: a shared worktree where multiple branches are merged together. Owns the merge-to-main path (rebase, verify, ff-only).
- **Session baselines**: capturing HEAD SHA when a worktree is opened, so callers can detect whether new commits were added during their session.
- **Identity scoping**: configuring git user name/email per-worktree without polluting the main repo.

## What this package does NOT own

- **Work policy**: which worktree to use, when to merge, whether to skip a merge step. That's the executor's job.
- **Apprentice lifecycle**: prompting, Claude invocation, validation, committing after AI runs. That's the wizard's job.
- **Bead/task semantics**: this package has no knowledge of beads, formulas, agents, or work types. It knows about directories, branches, and SHAs.

## Key types

| Type | Purpose |
|------|---------|
| `RepoContext` | Operations on the main repository (branch management, worktree creation, push/pull). |
| `WorktreeContext` | Operations inside a worktree (commit, diff, merge, status). Never switches branches. |
| `StagingWorktree` | Embeds `WorktreeContext`. Manages a worktree where child branches are merged together. Owns the merge-to-main path. |

## Key constructors

| Function | When to use |
|----------|-------------|
| `RepoContext{Dir: path}` | You need to manage branches or create worktrees from the main repo. |
| `rc.CreateWorktreeNewBranch(...)` | You're starting fresh work on a new branch. Captures a session baseline. |
| `ResumeWorktreeContext(dir, ...)` | You need to work in an existing worktree you don't own. Captures a session baseline. Detects the branch if you pass `""`. |
| `NewStagingWorktree(...)` | You need a temporary staging worktree (creates a temp dir). |
| `NewStagingWorktreeAt(...)` | You need a staging worktree at a known path (discoverable by other processes). |
| `ResumeStagingWorktree(...)` | You're reopening a staging worktree from persisted state. |

## Rules

1. **No domain language.** Comments and identifiers describe git concepts (worktree, branch, SHA, ref), not application concepts (wizard, apprentice, bead, formula).
2. **Callers say where, this package says how.** The caller provides a directory and branch name. This package handles the git operations to make it work.
3. **Worktree ownership is explicit.** Constructors that create worktrees (`New*`) own them — the caller must call `Close()`. Constructors that resume worktrees (`Resume*`) do not own them — the caller must not call `Close()`.
4. **Local refs only in worktrees.** `WorktreeContext` does not fetch from or push to remotes. Use `StagingWorktree.FetchBranch` or `RepoContext.Fetch` for remote operations.
