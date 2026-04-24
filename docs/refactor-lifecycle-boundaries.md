# Refactor: Lifecycle Ownership Boundaries

**Status:** Revised draft — addressing both review rounds. Ready for implementation dispatch.
**Prefix:** This lives under `spi` (Go codebase). No cross-prefix work.
**Last updated:** 2026-04-24 (revision 2).

## Context

The board is showing three decoupled symptoms simultaneously:

- 3 "in-progress" beads with no live wizards
- 3 wizard-registry entries referring to dead / nonexistent / closed beads
- 3 stale alert beads whose owning beads are already closed

All three sets are disjoint. The code audit (this session) confirmed that **four state surfaces have no single owner**:

1. Task / epic bead status transitions
2. Attempt bead lifecycle (5 creation sites, 7 close sites)
3. Alert / archmage-message bead creation + close cascades
4. Wizard-registry JSON (5 add sites, 6 remove sites; double-registers; one clone in `pkg/summon/summon.go:187` deliberately omits bead-reopen)

These are *blended responsibilities*: the same transitions are written in multiple packages with drifted rules. The fix is to introduce single owners for each surface.

Clean surfaces already (leave alone):

- Step beads: owned by `pkg/executor/graph_interpreter.go` (single creation site, centralized transitions)
- Review-round beads: owned by `pkg/wizard/wizard_review.go`
- Recovery beads: owned by `pkg/executor/executor_escalate.go` (`createOrUpdateRecoveryBead`)

## Goals

1. **Single owner per artifact.** No package mutates someone else's artifact directly.
2. **Mechanical correctness under crashes.** Wizard dies → attempt bead closes + task bead reopens + registry entry removed. Today the daemon only does the last step.
3. **Steward daemon is the authoritative orphan reaper.** Not summon. Not reset. Not resummon. The daemon sweeps every tick.
4. **No more ghost state visible on the board.**
5. **Both local-native and cluster-native modes keep working.** The two modes have different state machines; this refactor must not paper over the difference.

## Non-goals

- Rewrite the formula engine or graph interpreter.
- Change step-bead ownership (already clean).
- Change review-round or recovery bead ownership (already clean).
- Touch sage / arbiter process topology.
- Data migration (no schema change — all existing beads / registry entries work as-is).
- Changing the on-disk format of `wizards.json`.
- Introducing a new SQL schema for the state machine. All state stays in beads + JSON.
- Delete the legacy DAG code paths in `pkg/executor/executor_dag.go` (out of scope for this refactor).

## Deployment mode compatibility

This is the load-bearing design constraint missed in revision 1.

### Local-native mode

```
spire summon
    → attempt bead created (store.CreateAttemptBeadAtomic)
    → bead status: ready/open → in_progress
    → wizards.json entry added (Upsert)
    → wizard process spawned

spire claim (local wizard calls it)
    → already in_progress (summon set it)
    → idempotent: reclaim if same agent, reject if different

Daemon.ReapDeadAgents tick:
    → reads wizards.json, finds dead PIDs
    → for each dead entry:
        close open attempt bead
        reopen task bead (in_progress → open)
        remove wizards.json entry
```

### Cluster-native mode

```
Steward.SummonWizards tick:
    → transitions bead: ready → dispatched
    → emits pod spec to cluster scheduler (no wizards.json entry)

Pod starts, wizard runs:
    → spire claim <bead-id>
        → accepts dispatched as valid pre-claim status
        → attempt bead created (store.CreateAttemptBeadAtomic)
        → bead status: dispatched → in_progress
        → NO wizards.json entry (cluster pod has no local config dir)

Steward.RecoverStaleDispatched:
    → finds beads in dispatched status too long
    → transitions dispatched → ready (no attempt bead to close, none was created)

k8s handles pod death:
    → steward.CheckBeadHealth finds in_progress beads with stale heartbeats
    → transitions: in_progress → open (reopen), closes attempt bead
    → NO wizards.json cleanup needed (it was never written)
```

### What this means for the new packages

| Artifact | Local | Cluster |
|---|---|---|
| `pkg/registry` | Full owner of wizards.json | **Not used.** Cluster pods do not write to wizards.json. |
| `beadlifecycle.BeginWork` | Called from summon. Creates attempt + sets in_progress + upserts registry. | **Not called from steward.** Cluster summon only sets dispatched. |
| `beadlifecycle.ClaimWork` | Called from claim. Reclaims existing attempt OR noop (attempt already exists). | Called from claim. Creates attempt + sets in_progress. No registry upsert. |
| `beadlifecycle.EndWork` | Called from actionBeadFinish, resummon, reset. Closes attempt + cascades + removes registry entry. | Called from actionBeadFinish, resummon, reset. Closes attempt + cascades. Registry remove is a noop (nothing to remove). |
| `beadlifecycle.OrphanSweep` | Called from daemon tick. Reads wizards.json to find dead PIDs. | **Not called / noop for cluster entries.** Cluster uses CheckBeadHealth + stale heartbeat for orphan detection. |

This means `BeginWork` and `ClaimWork` are split rather than merged:

- `BeginWork` = local-summon path only (creates everything atomically including registry)
- `ClaimWork` = both paths (creates attempt + sets in_progress; optionally upserts registry if not already present)

`cmd/spire/summon.go` calls `BeginWork`; `cmd/spire/claim.go` calls `ClaimWork`.
The cluster wizard (via `spire claim`) also hits `ClaimWork` — same code path.

## Dependency diagram

```
pkg/registry
    ← no deps on pkg/alerts or pkg/beadlifecycle

pkg/alerts
    ← no deps on pkg/registry or pkg/beadlifecycle
    ← imports pkg/store (for CreateBead, AddDepTyped)

pkg/beadlifecycle
    ← imports pkg/registry (for Upsert, Remove, Sweep)
    ← does NOT import pkg/alerts (to avoid circular; alerts close cascade uses BeadOps interface)

cmd/spire, pkg/steward
    ← import all three new packages
    ← import pkg/executor for formula execution

pkg/executor
    ← imports pkg/alerts (for Raise)
    ← does NOT import pkg/beadlifecycle (executor doesn't own task-bead transitions)
    ← does NOT import pkg/registry (executor doesn't own registry)
```

No import cycles. The one risk point: `pkg/beadlifecycle` needs to close alert children during `EndWork`. It does so via a `BeadOps.AlertCascadeClose(sourceBeadID)` interface method — the implementation (`pkg/recovery.CloseRelatedDependents`) is wired at call-site, not imported by the package.

## Three new packages

### `pkg/registry`

**Purpose:** exclusive owner of the wizard-registry JSON file (`~/.config/spire/wizards.json`). Local-native only. Cluster-native code never calls this.

**Contract:**

```go
package registry

// Entry is a single wizard registration.
type Entry struct {
    Name           string    `json:"name"`
    PID            int       `json:"pid"`
    BeadID         string    `json:"bead_id"`
    Worktree       string    `json:"worktree"`
    StartedAt      string    `json:"started_at"`
    Phase          string    `json:"phase,omitempty"`
    PhaseStartedAt string    `json:"phase_started_at,omitempty"`
    Tower          string    `json:"tower,omitempty"`
    InstanceID     string    `json:"instance_id,omitempty"`
}

// Upsert adds or replaces an entry keyed by Name. File-locked.
func Upsert(entry Entry) error

// Remove deletes the entry with the given Name. Idempotent —
// removing a nonexistent entry returns nil.
func Remove(name string) error

// Update runs the provided function against the entry with the
// given Name inside the file lock, persists, returns not-found
// if no such entry exists.
func Update(name string, fn func(*Entry)) error

// List returns a snapshot of all entries.
func List() ([]Entry, error)

// Sweep returns the subset of List() whose PID is no longer
// running (per syscall.Kill(pid, 0) probe). Sweep does NOT
// remove entries — caller decides what to do with them.
func Sweep() ([]Entry, error)
```

**Files:**

- `pkg/registry/registry.go` — new package body
- `pkg/registry/registry_test.go` — tests (see Test strategy below)
- `pkg/agent/registry.go` — DELETE after migration

**Call-site migration:**

| Old | New |
|---|---|
| `agent.RegistryAdd(entry)` | `registry.Upsert(entry)` |
| `agent.RegistryRemove(name)` | `registry.Remove(name)` |
| `agent.RegistryUpdate(name, fn)` | `registry.Update(name, fn)` |
| `agent.LoadRegistry()` / `.Wizards` iteration | `registry.List()` |
| `agent.RegisterSelf(...)` | **DELETE**. Callers go through `beadlifecycle.BeginWork` instead. |
| `pkg/summon/summon.go:187 cleanDeadWizards` clone | **DELETE**. Daemon tick handles it via `beadlifecycle.OrphanSweep`. |

**Test strategy:**

- `TestUpsert_NewAndReplace` — new entry appended; existing with same Name replaced in place.
- `TestRemove_Idempotent` — remove nonexistent returns nil.
- `TestUpdate_NotFound` — returns error.
- `TestSweep_LivePIDs` — stub pid-probe returns true for known PIDs; Sweep returns empty.
- `TestSweep_DeadPIDs` — stub pid-probe returns false for test PIDs; Sweep returns those entries; Sweep does NOT remove them.
- `TestFileLock_Contention` — two goroutines race Upsert; both land.

---

### `pkg/alerts`

**Purpose:** exclusive owner of alert bead + archmage-message bead creation + close cascades. Eliminates the `Prefix: "spi"` hardcode (already fixed in spi-ff7nrq for `MessageArchmage`). Unifies the 4 hand-rolled alert-creation sites.

**Contract:**

```go
package alerts

// Class is the kind of alert.
type Class string

const (
    ClassAlert       Class = "alert"
    ClassArchmageMsg Class = "msg"
)

// Option modifies how Raise behaves.
type Option func(*raiseConfig)

// WithTitle sets the bead title (required for ClassAlert).
func WithTitle(t string) Option

// WithSubclass appends "alert:<subclass>" to the bead's labels.
// E.g. WithSubclass("merge-failure") → label "alert:merge-failure".
func WithSubclass(s string) Option

// WithFrom stamps "from:<agent>" label.
func WithFrom(agent string) Option

// WithPriority overrides default priority (P0 for alerts, P1 for msgs).
func WithPriority(p int) Option

// WithExtraLabels appends additional labels.
func WithExtraLabels(labels ...string) Option

// Raise creates an alert or message bead attributed to sourceBeadID.
// It always:
//   - derives the new bead's prefix from sourceBeadID via store.PrefixFromID
//   - sets a caused-by dep from the new bead to sourceBeadID
//   - stamps appropriate labels for the class
//   - returns the new bead's ID for the caller to reference
//
// Errors if sourceBeadID prefix can't be derived, or the underlying
// CreateBead/AddDep call fails. On AddDep failure the bead is already
// created — the ID is returned along with the error so the caller can
// decide whether to retry the dep link.
func Raise(deps BeadOps, sourceBeadID string, class Class, message string, opts ...Option) (alertID string, err error)

// BeadOps is the narrow interface the caller's deps must satisfy.
type BeadOps interface {
    CreateBead(store.CreateOpts) (string, error)
    AddDepTyped(from, to, depType string) error
}
```

**Caller migration:**

Replace:

- `pkg/executor/executor_escalate.go:63-83` (`MessageArchmage`) — becomes `alerts.Raise(..., ClassArchmageMsg, message, WithFrom(from))`
- `pkg/executor/executor_escalate.go:109-126` (empty-implement alert) — becomes `alerts.Raise(..., ClassAlert, ..., WithSubclass("empty-implement"))`
- `pkg/executor/executor_escalate.go:164-181` (`EscalateHumanFailure` alert section) — becomes `alerts.Raise(..., ClassAlert, ..., WithSubclass(failureType))`
- `pkg/executor/executor_escalate.go:233-248` (`EscalateGraphStepFailure` alert section) — becomes `alerts.Raise(..., ClassAlert, ..., WithSubclass(failureType))`

Keep the three `Escalate*` functions as thin wrappers calling `alerts.Raise`, for minimal call-site churn. Each wrapper's job is to compose the alert + the recovery bead + the comment; `alerts.Raise` handles only the bead-creation + caused-by-dep part.

**Test strategy:**

- `TestRaise_DerivesPrefix_Spd` — sourceBead=`spd-ac5` → alertID starts with `spd-`
- `TestRaise_DerivesPrefix_Spi` — sourceBead=`spi-0fek6l` → alertID starts with `spi-`
- `TestRaise_InvalidSource` — sourceBead=`` or malformed → returns error, no bead created
- `TestRaise_ArchmageMsg_Labels` — ClassArchmageMsg produces labels `msg`, `to:archmage`, `from:<agent>`
- `TestRaise_Alert_Labels` — ClassAlert with subclass=`merge-failure` produces label `alert:merge-failure`
- `TestRaise_AddsCausedByDep` — new bead has a `caused-by` dep pointing to sourceBeadID
- `TestRaise_ErrorsPropagate` — CreateBead failure → returns error; AddDepTyped failure → returns (alertID, error)

---

### `pkg/beadlifecycle`

**Purpose:** exclusive owner of the **(task bead status × attempt bead × registry entry)** state machine. Three entrypoints: `BeginWork` (local summon), `ClaimWork` (local + cluster claim), `EndWork` (all close/interrupt paths). Plus `OrphanSweep` for the daemon.

**Contract:**

```go
package beadlifecycle

// Mode controls whether the registry is written.
type Mode int

const (
    ModeLocal   Mode = iota // local-native: writes wizards.json
    ModeCluster             // cluster-native: skips wizards.json
)

// BeginOpts configures BeginWork.
type BeginOpts struct {
    Mode      Mode   // ModeLocal or ModeCluster
    Worktree  string // path to the wizard's worktree (used in registry entry)
    Tower     string // tower name (used in registry entry)
    AgentName string // wizard agent name
    Backend   string // "process" or "docker"
    Model     string // model identifier for attempt bead
    Branch    string // git branch for attempt bead
}

// BeginWork establishes the full work state for a local-summon.
//
// Steps (local mode):
//   1. Calls OrphanSweep(deps, OrphanScope{BeadID: beadID}) to close
//      any in-progress attempts for this bead whose registry entries are dead.
//   2. Creates a new attempt bead via Deps.CreateAttemptBead.
//   3. Flips the task bead to status=in_progress.
//   4. Upserts the registry entry (ModeLocal only; skipped for ModeCluster).
//
// Returns the new attempt bead ID.
// Error if the bead is already in_progress with a live owner (caller must
// choose to resummon explicitly).
//
// NOTE: BeginWork is local-summon-only in the current architecture.
// Cluster-native dispatch transitions ready→dispatched in the steward
// without calling BeginWork. Cluster-native claim uses ClaimWork.
func BeginWork(deps Deps, beadID string, opts BeginOpts) (attemptID string, err error)

// ClaimWork is the claim-path entrypoint (both local and cluster).
// Called from cmd/spire/claim.go.
//
// Steps:
//   1. Reads the bead status. Accepts: ready, dispatched, in_progress, hooked.
//   2. If status is in_progress or hooked:
//      - If an attempt exists with the same agentName → reclaim (return existing attemptID).
//      - Otherwise → return error (different agent owns it).
//   3. If status is ready or dispatched:
//      - Creates a new attempt bead.
//      - Transitions status → in_progress.
//      - For ModeLocal: upserts registry entry.
//
// Returns the attempt bead ID (new or reclaimed).
func ClaimWork(deps Deps, beadID string, opts BeginOpts) (attemptID string, err error)

// EndResult describes the outcome of a work unit.
type EndResult struct {
    // Status is the outcome label written to the attempt bead.
    // One of: "success", "discarded", "interrupted", "reset".
    Status string

    // CascadeReason is appended to the cascade close comment on
    // recovery + alert children. Empty = "work complete".
    // Set this to "reset" or "resummon" when the close is not a
    // natural completion, so audit trails stay interpretable.
    CascadeReason string

    // ReopenTask instructs EndWork to flip the task bead back to
    // open after closing the attempt. Set for interrupted/reset paths.
    ReopenTask bool

    // StripLabels lists labels to remove from the task bead on close.
    // Primarily used to strip "review-approved" on resummon.
    StripLabels []string
}

// EndWork closes the work state.
//
// Steps:
//   1. Closes the current attempt bead with EndResult.Status as the result label.
//   2. Closes alert + recovery children of beadID via
//      Deps.AlertCascadeClose(beadID, []string{"caused-by", "related"}).
//   3. If ReopenTask: transitions task bead in_progress → open.
//      If !ReopenTask: transitions task bead in_progress → closed.
//   4. Strips EndResult.StripLabels from the task bead.
//   5. Removes registry entry (ModeLocal only; noop for ModeCluster).
//
// Idempotent: safe to call even if parts of the state are already clean.
func EndWork(deps Deps, beadID string, opts BeginOpts, result EndResult) error

// OrphanScope controls what OrphanSweep examines.
type OrphanScope struct {
    // BeadID limits the sweep to a single bead's registry entry.
    // When empty, All must be true.
    BeadID string

    // All sweeps every dead entry in the registry.
    All bool
}

// SweepReport summarizes what OrphanSweep found and fixed.
type SweepReport struct {
    // Examined is the number of registry entries checked.
    Examined int

    // Dead is the number of dead entries found (PID not running
    // AND no graph_state.json present — dual-signal check).
    Dead int

    // Cleaned is the count of attempts closed + beads reopened.
    Cleaned int

    // Errors is a list of non-fatal errors encountered.
    // OrphanSweep continues past per-entry errors.
    Errors []error
}

// OrphanSweep is the canonical cleanup pass. LOCAL-NATIVE ONLY.
// Cluster-native uses CheckBeadHealth + stale heartbeat instead.
//
// Dual-signal orphan detection: an entry is declared dead when BOTH
// conditions hold:
//   (a) syscall.Kill(entry.PID, 0) returns an error (PID not running), AND
//   (b) no graph_state.json exists at entry.Worktree/.spire/graph_state.json
//       (worktree is empty or absent).
//
// This preserves crash-safe resume: a wizard that just crashed but
// left a graph_state.json can still be resummon'd into existing state.
// An entry with no graph_state.json has no resumable state — it is
// safe to declare orphaned.
//
// Called every tick by the steward daemon AND defensively by BeginWork.
// Idempotent: second call on already-clean state is a no-op.
func OrphanSweep(deps Deps, scope OrphanScope) (SweepReport, error)

// Deps is the narrow dependency surface.
type Deps interface {
    GetBead(id string) (store.Bead, error)
    UpdateBead(id string, updates map[string]interface{}) error
    CreateAttemptBead(parentID, agentName, model, branch string) (string, error)
    CloseAttemptBead(attemptID string, resultLabel string) error
    ListAttemptsForBead(beadID string) ([]store.Bead, error)
    RemoveLabel(id, label string) error
    // AlertCascadeClose closes all caused-by and related alert+recovery
    // children of sourceBeadID.
    AlertCascadeClose(sourceBeadID string, depTypes []string) error
}
```

**Migration:**

| Old call site | New |
|---|---|
| `cmd/spire/summon.go:543-558` (status transition + attempt logic) | `beadlifecycle.BeginWork(deps, beadID, BeginOpts{Mode: ModeLocal, ...})` |
| `cmd/spire/claim.go:86-139` (attempt creation + status transition) | `beadlifecycle.ClaimWork(deps, beadID, BeginOpts{...})` |
| `cmd/spire/summon.go:861 reconcileOrphanAttempts` | **DELETE**. Daemon tick handles it. |
| `cmd/spire/summon.go:767 cleanDeadWizards` / `summon.go:799 reapDeadWizard` | **DELETE**. Daemon tick handles it. |
| `pkg/steward/daemon.go:699 ReapDeadAgents` | `beadlifecycle.OrphanSweep(deps, OrphanScope{All: true})` |
| `pkg/executor/graph_actions.go actionBeadFinish close path` | `beadlifecycle.EndWork(deps, beadID, opts, EndResult{Status: "success"})` |
| `pkg/executor/graph_actions.go discard path` | `beadlifecycle.EndWork(..., EndResult{Status: "discarded"})` |
| `cmd/spire/resummon.go:62-112` (state cleanup before re-summon) | `beadlifecycle.EndWork(..., EndResult{Status: "interrupted", ReopenTask: true, CascadeReason: "resummon", StripLabels: []string{"review-approved"}})` then `beadlifecycle.BeginWork(...)` |
| `cmd/spire/reset.go:1097,1280,1304` attempt-closing calls | `beadlifecycle.EndWork(..., EndResult{Status: "reset", ReopenTask: true, CascadeReason: "reset"})` |

**Note on `actionBeadFinish` + close guard:**

The close-cascade guard (`childHasSuccessfulAttempt` / `childLandedOnBranch`) stays in `pkg/executor/graph_actions.go:actionBeadFinish`. `EndWork` is called AFTER the guard passes — it is the executor of the decision, not the decision-maker. This is the correct division: the executor determines when a close is legitimate; `beadlifecycle` handles the mechanical state transitions once the decision is made.

**Note on `RegisterSelf` replacement:**

`pkg/agent/registry.go:RegisterSelf` is deleted. Callers that need to stamp runtime fields (PID, phase, instance ID) after process start call `registry.Update(name, fn)` directly. The spawner (summon) is the sole creator of the registry entry via `BeginWork`; the wizard process itself may only update its own entry.

**Test strategy:**

Per-entrypoint unit tests with stubbed Deps:

- `TestBeginWork_Fresh_Bead_Local` — open bead, ModeLocal → attempt created, bead in_progress, registry upserted.
- `TestBeginWork_WithOrphan_Local` — prior attempt in_progress but owner dead (dead PID + no graph_state.json) → orphan closed as `interrupted:orphan`, new attempt created.
- `TestBeginWork_AlreadyInProgress_Live` — prior attempt in_progress with live owner → returns error.
- `TestClaimWork_Dispatched_Cluster` — dispatched bead, ModeCluster → attempt created, bead in_progress, no registry call.
- `TestClaimWork_Reclaim_Same_Agent` — in_progress bead with matching agentName → returns existing attemptID.
- `TestEndWork_HappyClose_Local` — attempt closed with result:success, bead closed, alerts+recovery cascaded (both caused-by and related deps), registry removed.
- `TestEndWork_Interrupted_Reopen` — ReopenTask=true → bead reopened, not closed.
- `TestEndWork_StripLabels` — StripLabels=["review-approved"] → label removed from task bead.
- `TestEndWork_ClusterMode_SkipsRegistry` — ModeCluster → registry.Remove not called.
- `TestOrphanSweep_DualSignal_Dead` — dead PID AND no graph_state.json → declared orphaned.
- `TestOrphanSweep_DualSignal_HasGraphState` — dead PID BUT graph_state.json present → NOT orphaned (resumable).
- `TestOrphanSweep_LivePID` — live PID → NOT orphaned.
- `TestOrphanSweep_DeadOnly_AllScope` — 2 dead + 1 live → sweeps 2, leaves 1.
- `TestOrphanSweep_Idempotent` — second call is a no-op.

Integration tests (against a real sqlite/dolt fixture):

- `TestLifecycle_SummonCloseClean` — BeginWork → simulated wizard work → EndWork(success) → all state clean.
- `TestLifecycle_CrashToSweep` — BeginWork → kill "wizard" (remove PID, remove graph_state.json) → OrphanSweep → bead reopened, attempt closed, registry empty.
- `TestLifecycle_CrashWithGraphState` — BeginWork → kill "wizard" (remove PID, keep graph_state.json) → OrphanSweep → bead NOT reopened (resumable), registry entry kept.
- `TestLifecycle_ResetMidFlight` — BeginWork → EndWork(reset, ReopenTask=true) → state clean.
- `TestLifecycle_ClaimCluster` — ClaimWork(ModeCluster) on dispatched bead → attempt created, status in_progress, no registry entry.

---

## Additional one-line fixes (included in same refactor)

These are small but urgent — they close the specific leaks visible on the board today:

1. **Widen `actionBeadFinish` success-close cascade** — `pkg/executor/graph_actions.go:1051`: current cascade passes `[]string{recovery.KindRecovery}`; change to `[]string{recovery.KindRecovery, recovery.KindAlert}`. Eliminates the "bead closed, alert still open" class.

   **This is Agent B's only `graph_actions.go` touch.** Agent A does not modify `graph_actions.go`. The Phase 1 conflict from revision 1 is resolved by moving this fix to Agent B.

2. **Remove `Prefix: "spi"` hardcode in `MessageArchmage`** — Already fixed in spi-ff7nrq. The `pkg/alerts` migration will absorb this call site and the fix will be preserved.

3. **Daemon Sweep replaces `ReapDeadAgents`** — `pkg/steward/daemon.go:699`: stops calling `agent.RegistryRemove` directly; calls `beadlifecycle.OrphanSweep(deps, OrphanScope{All: true})` which encapsulates dual-signal detection AND attempt+bead cleanup.

4. **Alert cascade on close covers `related` deps** — `EndWork` calls `AlertCascadeClose(beadID, []string{"caused-by", "related"})`. The resummon path links alert beads via `related` (not `caused-by`); both must be closed to prevent stale alerts.

5. **Dead-letter labeling preserved** — `OrphanSweep` adds label `dead-letter` to registry entries it marks as orphaned before removing them. This preserves audit history: grep `bd list --label dead-letter` to see what was reaped.

---

## Migration ordering

**Phase 1 (parallel-safe):**

- **Agent A:** build `pkg/registry` + tests. Delete / migrate callers of `pkg/agent/registry.go`. Does not touch `pkg/executor/` or `pkg/steward/`.

- **Agent B:** build `pkg/alerts` + tests. Migrate the 4 `executor_escalate.go` call sites. **Also:** widen the cascade in `graph_actions.go:1051` (adds `recovery.KindAlert`). These two are in the same package and same agent to avoid the Phase 1 parallelism conflict.

**Phase 2 (sequential after Phase 1):**

- **Agent C:** build `pkg/beadlifecycle` (BeginWork + ClaimWork + EndWork + OrphanSweep) + tests. Wire the call sites in `cmd/spire/summon.go`, `cmd/spire/claim.go`, `cmd/spire/resummon.go`, `cmd/spire/reset.go`, `pkg/executor/graph_actions.go:actionBeadFinish`, and `pkg/steward/daemon.go:699`.

**Phase 3 (cleanup):**

- Delete `cmd/spire/summon.go:reconcileOrphanAttempts` and `cleanDeadWizards` + `reapDeadWizard` helpers.
- Delete `pkg/agent/registry.go:RegisterSelf`.
- Delete `pkg/summon/summon.go:187 cleanDeadWizards` clone.

Each agent works in its own worktree. The merge agent integrates in order Phase 1A → Phase 1B → Phase 2 → Phase 3 on `integrate/lifecycle-boundaries`. Phase 2 cannot start until both Phase 1 branches are on the integration branch (it imports both new packages).

---

## Risk assessment

| Risk | Likelihood | Mitigation |
|---|---|---|
| Double-close a bead (multiple owners writing) | Low (store ops are idempotent) | `CloseBead` is already a no-op on already-closed beads; tests verify. |
| Registry file-lock contention | Low | Existing `RegistryLock()` handles it; ported as-is. |
| Daemon aggressively reaps live work | Medium | Dual-signal check: entry is orphaned only if PID dead AND no graph_state.json. Grace period logic not needed — the two-signal check is sufficient. |
| Caller misses a migrated call site | Medium | Delete the old function bodies outright (not just deprecate). Compiler catches misses. Grep for every removed symbol after migration. |
| EndWork cascades close something still needed | Low | `EndWork` only cascades kinds=[recovery, alert]. Both are safe to close on task completion. |
| External `spire resummon` CLI breaks | Low | CLI now calls `beadlifecycle.EndWork` + `beadlifecycle.BeginWork`. Integration test covers CLI path. |
| Cluster-native breaks | Low | Cluster path uses `ClaimWork(ModeCluster)` which skips registry. `OrphanSweep` is only called by daemon in local mode. CheckBeadHealth + heartbeat handles cluster orphan detection as before. |
| `ClaimWork` vs. `BeginWork` confusion | Medium | Clear naming and doc: `BeginWork` = summon (local only); `ClaimWork` = claim (both modes). Compiler errors if wrong one is called at wrong site. |

---

## Acceptance

1. `~/.config/spire/wizards.json` writes happen **only** from `pkg/registry`. `grep -rn "RegistryAdd\|RegistryRemove\|RegistryUpdate\|SaveRegistry" pkg/ cmd/` returns zero hits outside `pkg/registry/`.
2. Alert beads are created **only** via `pkg/alerts.Raise` (or thin `Escalate*` wrappers). No direct `CreateBead(..., Labels: []string{"alert:..."})` outside the new package.
3. Task bead status transitions happen **only** via `pkg/beadlifecycle.{BeginWork,ClaimWork,EndWork,OrphanSweep}`. Grep `UpdateBead.*status` confirms.
4. Archmage message beads and alert beads carry the source bead's prefix. Unit tests verify against spd-parent fixtures.
5. Steward daemon tick closes orphan attempt beads + reopens their parents within one tick. Integration test: start fake wizard (local mode), kill it (remove PID + graph_state.json), wait one tick, verify state clean.
6. Steward daemon tick does NOT reap a wizard that crashed but left graph_state.json. Integration test: kill PID, keep graph_state.json, wait one tick, verify bead still in_progress (resumable).
7. After successful bead close, all caused-by AND related alert children are closed. Integration test: 2 alert deps (one caused-by, one related) + 1 recovery dep; all three closed.
8. `spire claim <dispatched-bead>` in cluster mode creates attempt bead + transitions in_progress + NO registry entry. Integration test.
9. Full `go test ./...` green. No tests skipped. Pre-existing test suites for summon / claim / reset / resummon / executor all pass.
10. `wizards.json` never contains entries for beads whose status is `closed`. Daemon tick guarantees this.
11. Design-doc-level acceptance: the 5 boundary violations from the earlier audit are mechanically impossible after this refactor.

---

## Related / superseded beads

- spi-pwdhs5 (Bug B — reset cascade + orphan reconciliation) — **partially lands this**. The orphan reconciler went into `cmd/spire/summon.go:861`; this refactor moves it to the daemon where it belongs.
- spi-gdzd7d (inline recovery default) — adjacent; this refactor depends on the recovery cycle running. Prerequisite landed.
- spi-pqjzc0 (close-cascade commit attribution) — landed. Orthogonal.
- spi-ofx4b2 (resummon self-collision) — landed. Orthogonal.
- spi-rpuzs6 (unbound-prefix silent CWD fallback) — landed. Orthogonal.
- spi-ff7nrq (cross-prefix message/alert prefix) — landed. The `pkg/alerts` migration absorbs the remaining call sites.

---

## Summary diff (rough scale)

- New files: `pkg/registry/*.go` (~3 files), `pkg/alerts/*.go` (~2 files), `pkg/beadlifecycle/*.go` (~5 files including integration tests) — ~1800 lines including tests.
- Modified: `pkg/executor/executor_escalate.go`, `pkg/executor/graph_actions.go`, `pkg/steward/daemon.go`, `cmd/spire/summon.go`, `cmd/spire/claim.go`, `cmd/spire/resummon.go`, `cmd/spire/reset.go`, `pkg/summon/summon.go`, `pkg/agent/registry.go`.
- Deleted: `RegisterSelf` (~18 lines), `pkg/summon/summon.go:187 cleanDeadWizards` (~40 lines), `cmd/spire/summon.go:reconcileOrphanAttempts` + `cleanDeadWizards` + `reapDeadWizard` (~150 lines).

Net line change: approximately -200 lines of scattered duplication, +1800 lines of owned packages with tests.
