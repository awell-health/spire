# Internal Bead Taxonomy

Spire stores four bead types that exist for internal bookkeeping rather
than as user-facing work items: **`message`**, **`step`**, **`attempt`**,
and **`review`**. They live in the same `issues` table as task/bug/epic
beads but are programmatic-only, hidden from the board and steward
queue, and created exclusively by the engine (executor, wizard, dispatch
layer). This document is the canonical reference for what they model,
how they're filtered, and how to recognize a stray one in the wild.

## The four types

The set is defined in
[`pkg/store/internal_types.go`](../pkg/store/internal_types.go) as
`InternalTypes = {message, step, attempt, review}`. Each is a child of a
parent work bead (or, for direct DMs, unparented). Creators below are
the **only** realistic creation paths — each performs additional
bookkeeping (parent linkage, atomic claim, reset-cycle inheritance,
in_progress lease metadata) beyond a plain `CreateBead`.

| Type      | Models                                       | Creator                                              | Source                                                                  | Labels                                                          |
|-----------|----------------------------------------------|------------------------------------------------------|-------------------------------------------------------------------------|-----------------------------------------------------------------|
| `message` | Inter-agent routing (DM, ref-threaded reply) | `SendMessage(opts)`                                  | [`pkg/store/dispatch.go`](../pkg/store/dispatch.go)                     | `msg`, `to:<agent>`, `from:<agent>`, `ref:<bead-id>`            |
| `step`    | Formula phase progress (one per phase)       | `CreateStepBead(parentID, name)`                     | [`pkg/store/beadtypes.go`](../pkg/store/beadtypes.go)                   | `workflow-step`, `step:<name>`                                  |
| `attempt` | Per-claim execution try with result label    | `CreateAttemptBeadAtomic(parentID, agent, model, branch)` | [`pkg/store/beadtypes.go`](../pkg/store/beadtypes.go)              | `attempt`, `attempt:<N>`, `agent:<name>`, `branch:<branch>`, `reset-cycle:<C>` |
| `review`  | Review-round record (sage verdict, findings) | `CreateReviewBead(parentID, sage, round)`            | [`pkg/store/beadtypes.go`](../pkg/store/beadtypes.go)                   | `review-round`, `sage:<name>`, `round:<N>`, `reset-cycle:<C>`   |

### What gets created during a wizard run

For a standalone task running `task-default`, the bead graph builds up
roughly like this (parent work bead at top, internal children below):

```
task spi-xxxx                                     (work bead — top-level)
├── attempt: wizard-spi-xxxx              type=attempt   created at claim
├── step:research                         type=step      ┐
├── step:implement                        type=step      │  one step per
├── step:review                           type=step      │  formula phase
├── step:merge                            type=step      │  (created at
├── step:close                            type=step      ┘   summon time)
├── review-round-1                        type=review    created when sage
│                                                        dispatches
└── (messages with ref:spi-xxxx)          type=message   threaded under
                                                         the work bead
```

Steps are created up front (one per phase in the formula) and
transitioned `open → in_progress → completed`. Attempts are created at
claim and closed with a `result:<outcome>` label when the apprentice
finishes (or fails). Reviews are created at review-round dispatch and
closed with the verdict and findings written into description and
metadata. Messages are created on demand by `spire send` and threaded
either under their `--ref` bead or unparented as direct DMs.

## The `IsWorkBead` invariant

The invariant lives in
[`pkg/store/internal_types.go`](../pkg/store/internal_types.go):

```go
func IsWorkBead(b Bead) bool {
    return !InternalTypes[b.Type] && b.Parent == ""
}
```

A "work bead" is **top-level and non-internal**. Anything that is a
child of another bead, or whose type is one of the four internal types,
is not work — it's bookkeeping. Two layers enforce this on the steward
queue, and a handful of other call sites apply it to their own surface.

### Filter sites

| Location                                                                      | What it filters                                          |
|-------------------------------------------------------------------------------|----------------------------------------------------------|
| `GetReadyWork` SQL filter — [`pkg/store/queries.go`](../pkg/store/queries.go) (`ExcludeTypes` push) | Internal types never reach Go code in the steward queue |
| `GetReadyWork` Go safety net — [`pkg/store/queries.go`](../pkg/store/queries.go) (`!IsWorkBead` post-filter) | Backup against the SQL filter; also drops design/deferred/hooked beads |
| `skipBead` — [`pkg/board/categorize.go`](../pkg/board/categorize.go)          | Board columns (Backlog/Ready/In Progress/Hooked/Done)    |
| `isWorkBoardBead` — [`pkg/board/categorize.go`](../pkg/board/categorize.go)   | `BoardBead` mirror of `IsWorkBead` for board data        |
| Steward dispatch — [`pkg/steward/steward.go`](../pkg/steward/steward.go) (dispatch/health/merge sweeps) | Skips internal beads in every steward cycle              |
| Cluster dispatch — [`pkg/steward/cluster_dispatch.go`](../pkg/steward/cluster_dispatch.go) | Same gate for cluster-native dispatch                    |
| Inspector children — [`pkg/board/inspector.go`](../pkg/board/inspector.go)    | Hides step/attempt/review children from the inspector view (uses per-type predicates, not `IsInternalBead` — `message` children would slip through, intentionally, since messages thread separately) |
| Lifecycle status — [`cmd/spire/lifecycle_status.go`](../cmd/spire/lifecycle_status.go) | Uses `IsInternalBead` to keep the in-progress count clean |

## Legacy label fallback

Before internal types existed, these beads were identified by labels.
The fallback still ships:

- `labelToType` in
  [`pkg/store/internal_types.go`](../pkg/store/internal_types.go) maps
  legacy labels to the new types: `msg → message`, `workflow-step →
  step`, `attempt → attempt`, `review-round → review`.
- `MigrateInternalTypes()` is idempotent — it queries each label, sets
  `issue_type` if it isn't already set, and leaves the labels in place.
  It's called once per tower on `spire up` (see
  [`cmd/spire/up.go`](../cmd/spire/up.go)).
- `skipBead` keeps a parallel label-based check
  ([`pkg/board/categorize.go`](../pkg/board/categorize.go)) for any
  beads that haven't been touched by the migration.

The per-type label predicates (`IsAttemptBead`, `IsStepBead`,
`IsReviewRoundBead` in [`pkg/store/beadtypes.go`](../pkg/store/beadtypes.go))
also still check labels first for the same reason.

## The contract — programmatic-only

There is **no hard guard** in `spire file` against
`-t message|step|attempt|review`. `parseIssueType` accepts them. The
contract is enforced by:

- **`spire file` defaults to `task`** and internal types are not in any
  user-facing docs or help text.
- **The `Create*Bead` constructors are the only realistic creation
  path.** Each one wires up the parent linkage, atomic claim, reset-cycle
  inheritance, in_progress lease metadata, and label set that the rest
  of Spire expects to find.
- **Universal hiding** in board/steward/inspector means a hand-filed
  internal bead would still execute but would be invisible to every
  work surface.

So: programmatic-only by **convention**, not refused by the CLI.

## Guidance for agents

- **Do not file beads of these types.** Use `spire file` for tasks,
  bugs, features, epics, chores, or design beads. Internal types are
  created by the engine on your behalf.
- **A top-level internal bead is a bug indicator.** Any bead whose type
  is `message`/`step`/`attempt`/`review` and whose `Parent == ""` outside
  a tower-fresh state has either escaped `MigrateInternalTypes` or was
  hand-filed. `IsWorkBead` would treat it as work, and the steward would
  try to dispatch it. Report it (or close it) rather than working
  around it.
- **Don't grep for these by label alone.** Migrated beads carry both
  the new `issue_type` and the legacy label; new beads created by the
  current code path carry both as well. Label-only checks miss the
  contract; type-only checks miss unmigrated legacy data. Use the
  predicates in
  [`pkg/store/beadtypes.go`](../pkg/store/beadtypes.go) and
  [`pkg/store/internal_types.go`](../pkg/store/internal_types.go) —
  they handle both.
