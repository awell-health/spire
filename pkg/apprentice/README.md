# pkg/apprentice

Bundle-based apprentice delivery.

This package is the single authority for turning an apprentice's committed
worktree state into the delivery artifact that the parent executor/wizard can
consume later.

Today that means:

- verifying the apprentice worktree is ready for delivery
- creating a git bundle when changes exist
- uploading that bundle to the configured `BundleStore`
- writing the apprentice signal metadata and summary comment on the bead
- or writing a no-op signal when the apprentice intentionally has no changes

## What this package owns

- **Submission contract**: `Submit(opts)` and the exact signal metadata shape it
  writes.
- **Clean-worktree / commit validation** before submission.
- **Bundle creation and upload** for the bundle transport.
- **No-op submission semantics** for intentional no-change exits.
- **Signal payload shape** (`SignalPayload`) and its metadata encoding.

## What this package does NOT own

- **Transport selection**: whether a flow uses bundle or push transport belongs
  upstream in `pkg/wizard` / `pkg/executor`.
- **Bundle storage implementation**: the `BundleStore` interface and janitor
  belong in `pkg/bundlestore`.
- **Bundle consumption / merge**: applying a bundle into staging or falling back
  to feat-branch merge belongs in `pkg/executor`.
- **Worktree mechanics**: branch/worktree semantics belong in `pkg/git`.
- **Review or merge policy**: this package does not know about sages, review
  rounds, or merge outcomes.

## Relationship To Wizard, Executor, And Bundlestore

The clean split is:

- **wizard** decides that an apprentice should deliver work now
- **apprentice** performs the bundle/no-op submission contract
- **bundlestore** stores the artifact bytes
- **executor** later consumes the signal and integrates the resulting branch

This package should stay narrow. If a future change adds a second call site,
that call site should still use `Submit` rather than re-implementing signal
write logic.

## Important current constraint

This package owns the **bundle/no-op** delivery contract only.

Push transport exists today, but it is not implemented here. It is
classified as `HandoffMode=transitional` in the runtime contract (see
[docs/design/spi-xplwy-runtime-contract.md §1.3](../../docs/design/spi-xplwy-runtime-contract.md)),
quarantined, counted, and Warn-logged by `pkg/executor/handoff.go`.
When the runtime chooses push transport, the caller bypasses
`pkg/apprentice` and pushes the feature branch directly. That means:

- bundle metadata rules live here
- push transport policy does not
- phase 5b (separate bead) removes the push path entirely

If those two paths must become more uniform, the unification should happen at
the contract boundary above this package, not by turning `pkg/apprentice` into a
general transport router.

## Position in the runtime contract

The apprentice emits one of two artifacts, chosen by the **executor**
(never inferred here):

1. **The bundle signal artifact**, with `HandoffMode` set by the
   executor before dispatch. `Submit` writes the signal payload
   (including the mode) so the executor does not have to re-infer the
   selected mode when it integrates the bundle.
2. **An explicit no-op outcome**, when the apprentice intentionally
   produced no changes. This is still an explicit emission, not silence.

The apprentice does **NOT** own transport policy. Selecting between
`HandoffBorrowed` (same-owner continuation — no delivery needed),
`HandoffBundle` (canonical cross-owner), and `HandoffTransitional` (the
quarantined push path) is executor responsibility. This package simply
executes whichever bundle-or-no-op contract was chosen for it.

## Key types

| Type / function | Purpose |
|-----------------|---------|
| `Submit` | Run the apprentice delivery pipeline. |
| `Options` | Injected submission inputs and dependencies. |
| `SignalPayload` | JSON payload written to bead metadata for bundle/no-op delivery. |

## Practical rules

1. **Keep signal-write semantics centralized here.** Do not duplicate metadata
   or comment-writing logic in CLI wrappers or wizard exit paths.
2. **Operate on committed state only.** Submission is for durable apprentice
   output, not half-finished worktrees.
3. **Do not choose transport here.** This package executes the chosen bundle
   contract; it does not decide between bundle and push.
4. **Stay bead-scoped, not workflow-scoped.** This package delivers one
   apprentice result. It should not grow review or merge concepts.
5. **Dependency injection is intentional.** Keep external effects explicit so
   tests and alternate callers can reuse the same contract.

## Where new work usually belongs

- Add it to **`pkg/apprentice`** when the submission artifact or metadata
  contract changes.
- Add it to **`pkg/bundlestore`** when storage backends or retention behavior
  change.
- Add it to **`pkg/wizard`** when apprentice exit behavior or transport choice
  changes.
- Add it to **`pkg/executor`** when bundle consumption, fallback fetch/merge,
  or integration policy changes.
