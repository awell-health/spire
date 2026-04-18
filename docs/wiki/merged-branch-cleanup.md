# Merged Feature Branch Cleanup

## What happened (spi-iwvjz)

Three `feat/spi-*` branches from work completed on 2026-04-03 were still
lingering as local refs weeks later, even though their commits had landed
on main via normal merge commits:

| Branch | Parent bead | Merge commit |
|--------|-------------|--------------|
| `feat/spi-2sqwc` | workshop v3-only | `3dfbbba` |
| `feat/spi-4112n` | focus v3-only | `2f15ed8` |
| `feat/spi-ftswz` | formula v2 removal | `448681f` |

No worktrees existed for any of them (`git worktree list` + `git worktree
prune --dry-run` both clean). The cleanup was a single `git branch -d`
invocation from the main checkout.

## Why it matters

Merged feature branches stick around indefinitely unless something deletes
them. They clutter `git branch` output, pollute tab-completion, and
shadow future branch names if the same bead ID is reused (rare but
possible). Periodically sweeping them keeps the local ref namespace clean
without any risk to history.

## `-d` is the right default here

Use lowercase `git branch -d`, not uppercase `-D`:

```bash
git branch -d feat/spi-2sqwc feat/spi-4112n feat/spi-ftswz
```

Lowercase `-d` refuses to delete a branch that isn't an ancestor of
`HEAD` (or its upstream). That's exactly the safety you want — if the
branch isn't actually merged, the delete fails loudly instead of
destroying work.

For these branches, `-d` succeeds because they landed on main via normal
merge commits, so `git merge-base --is-ancestor <branch> main` returns
true.

## When `-d` fails — check the merge mechanism

If `-d` complains "not fully merged" for a branch you know is done,
consider how it landed on main:

- **Normal merge** (`git merge feat/X`) → commits are preserved, `-d` works.
- **Rebase or squash-merge** → commits get new SHAs on main, `-d` fails
  even though the diffs are identical. This is common after
  orchestrator-style workflows.

For the rebase/squash case, see [orchestrator-worktree-cleanup.md](orchestrator-worktree-cleanup.md)
— it covers patch-id verification before resorting to `-D`.

## Where to run it from

From the **main checkout** (e.g. `/Users/jb/awell/spire`), not a feature
worktree. Running branch deletes from inside a worktree whose own branch
is in scope will fail, and running from a worktree that shares git admin
state with the target can produce confusing errors. The main checkout is
the simplest, safest place.

## Handling this kind of chore

- **Type:** `chore`. No code changes — pure ref-state cleanup.
- **Scope discipline:** Stick to the bead IDs in the title. Each closed
  parent bead may have leftover `in_progress` attempt children with
  `branch:feat/<id>` labels — leave those alone unless archmage asks.
- **Verification:** `git branch --list 'feat/spi-<id>'` returning empty
  for each target.
- **Testing:** None. `git` is the verifier.
- **Review bar:** Verdict-only. If the branches are gone and nothing
  else was touched, the chore is done.
- **Commit:** Include the command and a one-line note that `-d`
  preserved the merged-safety check. No tree changes, so the commit is
  effectively an empty git-state commit — that's fine.
