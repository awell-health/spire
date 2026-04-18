# Post-Orchestrator Worktree & Branch Cleanup

## What happened (spi-tcui1)

The `worktree-merge-orchestrator` skill dispatches multiple parallel apprentices, each in their own `.worktrees/<name>` directory on a `feat/<name>` branch. When the staging merge lands on main, the orchestrator does **not** automatically remove the per-apprentice worktrees or branches.

For the claude-subprocess visibility feature, three worktrees + branches were left behind:

- `.worktrees/visibility-runner` → `feat/visibility-runner`
- `.worktrees/visibility-board` → `feat/visibility-board`
- `.worktrees/visibility-logs-cli` → `feat/visibility-logs-cli`

Each apprentice's work landed on main as a separate commit (`d1021e9`, `d9ad62b`, `8aada84`). The cleanup is pure git-metadata housekeeping — remove the worktrees, delete the branches.

## Why it matters

Leftover worktrees clutter `git worktree list` and make it harder to see which worktrees belong to active work. Leftover branches shadow future branch names and pollute tab-completion. The orchestrator leaves them behind intentionally as a safety net (so a failed merge can be re-attempted from the apprentice worktree), but once main has the commits, they are dead state.

## The `-d` vs `-D` gotcha

The natural instinct is `git branch -d feat/visibility-runner`, which only deletes a branch if it's an ancestor of `HEAD` (or its upstream). **That usually fails after an orchestrator run.**

The orchestrator rebases apprentice commits onto the staging branch and then onto main — the final commits on main have **different SHAs** than the apprentice branch tips, even though the diffs are identical. `git merge-base --is-ancestor <branch> main` returns false, so `-d` refuses with "not fully merged."

To verify safety before `-D`:

```bash
# Compare patch-ids (diff fingerprint, SHA-independent)
for branch in feat/visibility-runner feat/visibility-board feat/visibility-logs-cli; do
    apprentice_pid=$(git log -1 --format=%H "$branch" | xargs git show | git patch-id | awk '{print $1}')
    # Find the matching subject on main, compute its patch-id, compare.
done
```

If every apprentice-branch tip has a matching patch-id somewhere on main, the diffs landed — `-D` is safe. If not, the branch has work not yet merged; **do not force-delete.**

## The cleanup procedure

Run from the **main checkout**, not from a research/feature worktree (pruning admin metadata from inside a worktree can get confused):

```bash
# 1. Remove worktrees first (prunes .git/worktrees/<name> admin metadata)
git worktree remove .worktrees/visibility-runner
git worktree remove .worktrees/visibility-board
git worktree remove .worktrees/visibility-logs-cli

# 2. Delete the branches (use -D after patch-id verification; -d will usually fail)
git branch -D feat/visibility-runner feat/visibility-board feat/visibility-logs-cli

# 3. Verify clean state
git worktree list             # should show no visibility entries
git branch | grep visibility  # should be empty
ls .worktrees/ | grep visibility  # should be empty
```

## Handling this kind of chore

- **Type:** `chore`. No code changes — pure filesystem + git-metadata cleanup.
- **Where to run:** Main checkout. Worktree removal from inside another worktree can mis-prune admin state.
- **Branch deletion flag:** Default to `-D` after verifying patch-id parity with main. `-d` nearly always fails post-orchestrator because of the rebase.
- **Testing:** None. The compiler is not involved; `git worktree list` + `git branch` are the verification.
- **Review bar:** Verdict-only. If the worktrees are gone and no beads/docs reference the branches, the chore is done.
- **Commit:** An empty commit (`git commit --allow-empty`) referencing the bead is fine — the cleanup lives in git state, not in the tree.
