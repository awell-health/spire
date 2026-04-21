# Wizard: Fetch Apprentice Bundle via BundleStore

## What happened (spi-rfee2)

The apprentice and wizard no longer share a git worktree. The apprentice
`submit` command (spi-1fugj) writes a git bundle to the `BundleStore`
(spi-8qsmr) and stamps a JSON signal on the task bead under
`apprentice_signal_<role>`. The wizard's dispatch sites, which previously
assumed the apprentice's `feat/<beadID>` branch was already reachable as
a local ref, now read that signal, stream the bundle out of the store,
fetch it into staging as a local branch, then proceed with the existing
`MergeBranch` path.

All four dispatch sites were converted together:

- `dispatchDirectCore` — one apprentice, single bead
- `dispatchSequentialCore` — ordered sub-apprentices
- `dispatchWaveCore` — parallel fan-out waves
- `dispatchInjectedTasks` — graph-injected ad-hoc tasks

Each site now populates `SpawnConfig.AttemptID` (via `e.attemptID()`)
and `SpawnConfig.ApprenticeIdx` (`"0"` today — fan-out within a single
bead is a future bead) so the apprentice can compute its signal role.

## Why it was needed

The old design required the apprentice to land commits into a branch
the wizard could name locally. That only works when both run in the
same repo on the same filesystem. The k8s model spawns apprentices in
remote pods without push credentials; bundle transport is the decoupling.
Dolt carries only the opaque `BundleHandle`; the artifact lives in
whichever backend the tower is configured with.

## The flow (per successful spawn)

```
apprentice submit (spi-1fugj)          wizard dispatch (this bead)
--------------------------             ---------------------------
git bundle create base..HEAD
BundleStore.Put → handle
md[apprentice_signal_<role>] = JSON
                                       bundlestore.SignalForRole(md, role)
                                       → Signal{Kind, BundleKey, …}
                                       BundleStore.Get(handle) → io.ReadCloser
                                       stream → <staging>/.git/tmp-bundles/*.bundle
                                       git fetch <tmp> +HEAD:feat/<beadID>
                                       stagingWt.MergeBranch(branch, resolver)
                                       BundleStore.Delete(handle)   ← only after merge
```

The helper that owns this is `applyApprenticeBundle` in
`pkg/executor/apprentice_bundle.go`. It returns a `bundleOutcome`
carrying `{Applied, NoOp, Branch, Handle}` so the dispatch site can
decide explicitly between merge / skip / legacy-fallback. The bundle
itself is unbundled via `WorktreeContext.ApplyBundle` in `pkg/git/worktree.go`
(`git fetch <bundlePath> +HEAD:<targetBranch>` — the `+` makes replays
idempotent).

## Two things to get right

**Delete AFTER merge, never before.** `applyApprenticeBundle` returns
the handle; the dispatch site calls `deleteApprenticeBundle(beadID, h)`
only after `MergeBranch` succeeds. An earlier version deleted inside the
apply helper, which meant a merge conflict left the bundle gone and
unrecoverable. The janitor (in `pkg/bundlestore`) is the correctness
net for crashes between merge and delete — the in-process delete is
the optimization, not the guarantee.

**No-op short-circuits MergeBranch.** When the apprentice ran `submit
--no-changes`, `Signal.Kind == "no-op"` and the wizard must skip the
fetch + merge entirely. `MergeBranch` on an absent ref would fail
loudly; the outcome's `NoOp` flag routes around it.

## Legacy fallback

`e.deps.BundleStore` is nil in test harnesses and older tower setups.
Every dispatch site nil-checks and falls back to the pre-migration
`MergeBranch(feat/<beadID>, resolver)` path. That fallback becomes
dead code once the full submit+fetch path is the only path, but it's
worth keeping through the rollout since it makes the feature bisectable.

## Signal shape (consumer side)

`pkg/bundlestore/signal.go` holds the shared types. Keep it in lockstep
with `cmd/spire/apprentice.go` — the JSON is the wire format.

```go
type Signal struct {
    Kind        string   // "bundle" | "no-op"
    Role        string   // "apprentice-<beadID>-<idx>"
    BundleKey   string   // empty for no-op
    Commits     []string
    SubmittedAt string
}

// Metadata key: "apprentice_signal_" + role
// Role:         apprentice-<beadID>-<ApprenticeIdx>
```

`SignalForRole` returns three states: `(zero, false, nil)` for "no signal",
`(parsed, true, nil)` for success, and `(zero, true, err)` when the value
exists but won't parse. Callers must distinguish — an unparseable signal
is a wire-protocol bug, not an absence.

## Handling similar work

- **Cross-process wire formats live in their own file.** A shared
  `signal.go` with the producer + consumer importing the same constants
  is the cheapest insurance against field-name drift.
- **Convert every dispatch site in the same bead.** The review of this
  chore flagged three uncovered sites because the initial commit only
  touched `dispatchDirectCore`. Spec-driven audits ("find every site
  where…") mean every site.
- **Populate all `SpawnConfig` fields.** Missing `AttemptID`/`ApprenticeIdx`
  silently collapses fan-out signals onto the same key. Check for literal
  `agent.SpawnConfig{}` constructions during review.
- **Test each branch of new helpers.** `applyApprenticeBundle` has six
  early-return paths; `SignalForRole` has three return states.
  `apprentice_bundle_test.go` and `signal_test.go` cover each —
  mirror that pattern.
- **Retain-then-delete > delete-then-hope.** Any flow where a downstream
  step can fail (merge, push, publish) must hold the source artifact
  until the downstream step commits. Surface the handle on the outcome
  so the caller owns the delete.

## Tmp-file placement

The bundle stream is written to a temp file via
`pkg/git.WorktreeContext.ApplyBundleFromReader`, which uses
`os.TempDir()`. `git fetch <bundle>` does not require the bundle to
live on the same filesystem as the worktree, so this works against
linked worktrees (where `<dir>/.git` is a pointer file, not a
directory). Path layout decisions for git artifacts live in
`pkg/git`, not in the executor — see `pkg/git/README.md`.
