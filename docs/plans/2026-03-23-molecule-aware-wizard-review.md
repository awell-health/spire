# Molecule-Aware Wizard + Review Agent

**Goal:** Wizards follow the spire-agent-work molecule (design → implement →
review → merge), close steps as they progress, and hand off to an Opus-powered
review agent. Wizard exits after implementation; reviewer is a separate
one-shot process. Both use the existing label-based review state machine.

**Date:** 2026-03-23 (rev 7 — addresses round 6 review findings)

---

## Architecture

### Per-bead lifecycle

```
spire summon 1
  → picks ready bead spi-abc
  → spawns: spire wizard-run spi-abc --name wizard-1

wizard-run spi-abc:
  1. claim bead, add owner:<wizard-name> label, pour molecule
  2. DESIGN phase (new claude instance, 10m timeout)
     - read focus context, explore code, form plan
     - write plan as bead comment (captured artifact for implement phase)
     - close design step
  3. IMPLEMENT phase (new claude instance, 15m timeout)
     - run claude with implementation prompt (design plan included)
     - validate (lint, build, test)
     - commit + push branch feat/spi-abc
     - close implement step
  4. REVIEW HANDOFF (wizard exits)
     - replace owner:<wizard-name> → implemented-by:<wizard-name>
       (frees capacity: findBusyAgents only reads owner: labels)
     - add labels: review-ready, feat-branch:feat/spi-abc
     - self-register reviewer in wizards.json
     - spawn: spire wizard-review spi-abc --name wizard-1-review
     - wizard self-unregisters from wizards.json, exits
  5. wizard-review runs (separate process, Opus model)
     - self-registers in wizards.json on start
     - creates its own worktree (reads pushed commits, not live edits)
     - diffs feat/spi-abc against main
     - runs Opus review (reuses artificer's review prompt/parsing)
     - APPROVE → close review step, add review-approved label,
       remove review-ready, remove implemented-by label,
       bead status stays in_progress
     - REQUEST_CHANGES → remove review-ready, add review-feedback,
       send feedback via spire send --ref spi-abc to implemented-by agent,
       self-register re-engaged wizard in wizards.json,
       spawn: spire wizard-run spi-abc --name wizard-1 --review-fix
       reviewer self-unregisters, exits
  6. RE-ENGAGEMENT (if request_changes — LOCAL MODE)
     - reviewer directly spawns wizard-run with --review-fix flag
     - wizard self-registers in wizards.json on start
     - wizard re-adds owner:<wizard-name> (back to active work)
     - wizard collects feedback: spire collect, filter by ref:spi-abc,
       close consumed messages via spire read <msg-id>
     - wizard fixes, pushes, replaces owner → implemented-by,
       re-adds review-ready → loop back to 5
     - max 3 round trips; 4th triggers warning + complexity assessment
     NOTE: In k8s mode, steward.detectReviewFeedback() sends a message
     and removes the label. It does NOT spawn processes or create CRDs.
     K8s re-engagement requires a running wizard to poll for messages.
  7. POST-APPROVAL STATE
     - labels: review-approved, feat-branch:feat/spi-abc
     - labels removed: review-ready, review-assigned, review-feedback,
       owner:*, implemented-by:*
     - molecule: design ✓, implement ✓, review ✓, merge open
     - bead: in_progress (steward skips review-approved beads)
     - merge deferred to artificer or manual
```

### Key design decisions

**Wizard is one-shot (finding #3).** The wizard exits after implementation
and review handoff. This preserves the ephemeral worker model, frees local
capacity immediately, and matches the existing k8s architecture. Re-engagement
for review feedback is spawned directly by the reviewer in local mode (see
"Reviewer spawns wizard" below), or goes through the steward in k8s mode.

**Reviewer gets its own worktree (finding #4).** The review agent creates a
separate git worktree from the pushed branch, never shares the wizard's
workspace. This prevents observing uncommitted state and matches how the
artificer initializes its own workspace in `standalone.go:33`.

**Reuse existing review protocol (finding #2).** Communication uses the
established label state machine (`review-ready` → `review-assigned` →
`review-feedback` / `review-approved`) and `spire send` for feedback
delivery, not bead comments. The `storeActor()` problem is avoided because
messages carry `from:<agent>` labels natively.

**Post-approval state (finding #1).** A new `review-approved` label
distinguishes "approved, awaiting merge" from "actively reviewing." The
steward's `detectReviewReady` and `checkBeadHealth` skip beads with
`review-approved`. The bead stays `in_progress` but is effectively parked.

**Design artifact captured (finding #5).** The design phase writes its plan
as a bead comment. The wizard captures claude's stdout from the design
phase, writes it to a file in the worktree (`DESIGN.md`), and includes it
in the implement prompt. This gives the implement phase explicit input.

**Owner label lifecycle (round 2 #3, round 3 #1).** After claiming a bead,
the wizard adds `owner:<wizard-name>`. At review handoff, it replaces
`owner:` with `implemented-by:<wizard-name>` and exits. This is critical
because `findBusyAgents()` (steward.go:324) scans all `in_progress` beads
for `owner:` labels — if the wizard left `owner:` in place, it would
appear permanently busy through review and post-approval, defeating the
one-shot capacity model. The `implemented-by:` label preserves routing
information for the reviewer without blocking the wizard name from being
reused by summon. On re-engagement (`--review-fix`), the wizard re-adds
`owner:` while actively working, then swaps back to `implemented-by:`
before the next review handoff. On approval, the reviewer removes
`implemented-by:` entirely.

**Reviewer spawns wizard on request_changes (round 2, finding #1).** In
local process mode, the reviewer directly spawns `spire wizard-run <bead-id>
--name <wizard-name> --review-fix` on request_changes, then exits. This
keeps local mode self-contained — the reviewer already has the bead ID,
branch, and feedback. The steward's `detectReviewFeedback()` only sends a
message and removes the label; it does NOT spawn wizard processes or create
SpireWorkload CRDs. Relying on it would dead-end the review loop in local
mode.

**Message consume contract (round 3, finding #2).** Review feedback is
sent via `spire send <wizard-name> "<feedback>" --ref <bead-id>`. This
creates a message bead with labels `[msg, to:<wizard>, from:<reviewer>,
ref:<bead-id>]`. The re-engaged wizard consumes feedback by:
1. `spire collect` — returns all open messages for the wizard name
2. Filter by `ref:<bead-id>` label to isolate this bead's feedback
3. `spire read <msg-id>` for each consumed message — closes the msg bead
This prevents stale feedback accumulation. Additionally, `summonLocal()`
must filter out beads with the `msg` label from ready work candidates,
since message beads are not actionable tasks.

**Process self-registration (round 3, finding #3).** Every spawned process
(wizard-run, wizard-review, wizard-run --review-fix) self-registers in
`wizards.json` on start and self-unregisters on exit. Today only
`summonLocal()` writes to the registry, so reviewer and review-fix
processes would be invisible to `spire roster`. Self-registration uses a
shared `wizardRegistryAdd(entry)` / `wizardRegistryRemove(name)` helper
with file locking to avoid races between concurrent processes.

**Workflow step beads are not schedulable work (round 4 #1, round 5 #1).**
When `spire focus` pours a molecule, the step beads (design, implement,
review, merge) are created as children of the molecule root. The molecule
root carries a `workflow:<bead-id>` label. The step beads themselves are
ordinary open child beads — they have no distinguishing label. This means
the open merge step (and any other unclosed step) will appear in
`storeGetReadyWork()` and get picked up by any consumer: `summonLocal()`,
`steward.assessAndAssign()`, and the k8s operator's `BeadWatcher`
(`operator/controllers/bead_watcher.go:67`), which reads `bd ready --json`
and creates a SpireWorkload for every ready bead.

Fix: each consumer of ready work adds its own workflow-step filter,
because they use different data paths:

- **Local (cmd/spire/store.go):** `storeGetReadyWork()` post-filters
  results to exclude beads whose parent carries a `workflow:*` label.
  This covers `summonLocal()` and `steward.assessAndAssign()`.
- **K8s (operator/controllers/bead_watcher.go):** The operator shells out
  to `bd ready --json` directly, bypassing Spire's store wrapper. The
  `BeadWatcher` must add the same parent-label check after parsing `bd`
  output, before creating SpireWorkload CRDs.

Both filters use the same logic: if `bead.Parent != ""`, fetch the parent
and skip if it carries any `workflow:*` label. The ready set is small
(typically <20 beads), so the parent lookup cost is negligible.

An alternative would be to push the filter into `bd ready` itself, but
that crosses the beads library boundary and affects all `bd` consumers.
The per-consumer approach is more targeted and doesn't change `bd`'s
contract.

**Review round is a single monotonic label (round 4, finding #2).**
The `review-round:N` label uses replace semantics, not append. On each
request_changes verdict, the reviewer:
1. Reads the current `review-round:*` label (if any) to get the current N
2. Removes the old `review-round:N` label
3. Adds `review-round:{N+1}`
If no `review-round:*` label exists, the first request_changes sets
`review-round:1`. There is always at most one `review-round:*` label on
a bead. The reviewer reads N directly from this label to decide
escalation (N >= 4 → warning, N >= 5 → stop). On approval, the
`review-round:*` label is left in place as an audit trail.

### Cross-mode divergence

**Local vs k8s review protocols are intentionally different for now
(round 2, finding #2).**

| Concern | Local (`wizard-review`) | K8s (`artificer standalone`) |
|---------|------------------------|------------------------------|
| On approve | Parks at `review-approved`, merge deferred | Merges via staging branch, closes bead |
| On request_changes | Reviewer spawns `wizard-run --review-fix` | Steward sends message + removes label (no CRD, no spawn) |
| Re-engagement | Reviewer spawns wizard directly | Wizard must poll for messages (gap — no auto-spawn) |
| Merge | Deferred to artificer or manual | Artificer `handleStandaloneApproval` |
| Capacity tracking | `owner:` → `implemented-by:` swap frees wizard | N/A (k8s pods are ephemeral) |

Note: the k8s column describes **current code**, not target behavior.
`detectReviewFeedback()` (steward.go:462) sends a message via `spire send`
and removes the `review-feedback` label. It does NOT create SpireWorkload
CRDs or spawn new pods. K8s re-engagement currently depends on a running
wizard process polling for messages, which is a pre-existing gap.

The artificer's `handleStandaloneApproval` (standalone.go:121) merges +
closes the bead immediately on approval. The local `wizard-review` parks
at `review-approved` because there is no local merge agent yet. These are
different protocols until the artificer's merge flow is adapted for local
mode. This is acceptable — see "What's NOT in this plan."

### Separate claude instances per phase

Each molecule phase gets its own `claude` invocation with a fresh context
window:
- **Clean context**: design doesn't pollute implementation, implementation
  doesn't pollute review feedback handling
- **Per-phase timeouts**: design=10m, implement=15m, review-fix=10m
- **Better observability**: each phase has its own log section

The wizard process orchestrates the phases but only lives through design +
implement. Review is a separate process.

### Label state machine

```
wizard claims bead
  → adds: owner:<wizard-name>

wizard pushes branch (review handoff)
  → removes: owner:<wizard-name>
  → adds: implemented-by:<wizard-name>, review-ready, feat-branch:feat/spi-abc
  → self-registers reviewer in wizards.json
  → spawns wizard-review, self-unregisters, exits
  (wizard is now free — findBusyAgents sees no owner: label)

reviewer spawns (or steward routes in k8s)
  → self-registers in wizards.json
  → adds: review-assigned
  → reviews diff via Opus

verdict = approve
  → removes: review-ready, review-assigned, implemented-by:<wizard-name>
  → adds: review-approved
  → closes: review molecule step
  → self-unregisters from wizards.json, exits

verdict = request_changes
  → removes: review-ready, review-assigned, review-round:{old N}
  → adds: review-feedback, review-round:{N+1}
    (replace semantics: at most one review-round:* label exists at a time)
  → sends feedback: spire send <wizard-name> "<feedback>" --ref <bead-id>
  → if N+1 >= 4: add warning comment + spire alert
  → if N+1 >= 5: exit without re-engaging (escalation)
  → LOCAL: self-registers re-engaged wizard, spawns wizard-run --review-fix,
    self-unregisters, exits
  → K8S: steward.detectReviewFeedback sends message + removes label (no spawn)

wizard re-engagement (local, --review-fix)
  → self-registers in wizards.json
  → re-adds: owner:<wizard-name>
  → collects: spire collect, filter ref:<bead-id>, close via spire read
  → removes: review-feedback
  → fixes, pushes
  → replaces: owner:<wizard-name> → implemented-by:<wizard-name>
  → re-adds: review-ready
  → self-registers reviewer, spawns wizard-review, self-unregisters, exits
  → cycle repeats
```

### Round trip limit

| Round | Action |
|-------|--------|
| 1-3   | Normal: reviewer requests changes, re-engages wizard (local: reviewer spawns; k8s: steward routes) |
| 4     | **Warning**: comment on bead + spire alert, assess if bead should be split |
| 5+    | **Escalate**: leave bead in review state, alert user, stop re-engaging |

The reviewer tracks round count via a single `review-round:N` label with
replace semantics (remove old, add new). At most one `review-round:*`
label exists at a time. The reviewer reads N to decide escalation before
spawning — if N >= 5, it exits without re-engaging.

---

## Implementation

### Phase 1: Molecule step closures, owner label lifecycle, self-registration

Wire the existing wizard to find and close steps, manage the owner/
implemented-by label swap, and self-register in wizards.json.

**Modified: `cmd/spire/wizard.go`**

Owner label lifecycle:

```go
// After claim succeeds:
storeAddLabel(beadID, "owner:"+wizardName)

// At review handoff (before exit):
storeRemoveLabel(beadID, "owner:"+wizardName)
storeAddLabel(beadID, "implemented-by:"+wizardName)

// On --review-fix re-entry:
storeAddLabel(beadID, "owner:"+wizardName)  // back to active
// ... after fix + push:
storeRemoveLabel(beadID, "owner:"+wizardName)
storeAddLabel(beadID, "implemented-by:"+wizardName)
```

Self-registration (shared helpers in `cmd/spire/summon.go`):

```go
func wizardRegistryAdd(entry localWizard) error
    // File-locked read-modify-write of wizards.json
    // Appends entry, deduplicates by name

func wizardRegistryRemove(name string) error
    // File-locked read-modify-write of wizards.json
    // Removes entry by name
```

Every process calls `wizardRegistryAdd` on start, `wizardRegistryRemove`
on exit (including deferred cleanup on panic/signal).

Molecule step closures (mirrors `closeMoleculeStep` from
`cmd/spire-artificer/staging.go` but using the spire CLI's store helpers):

```go
func wizardFindMoleculeSteps(beadID string) (molID string, steps map[string]string) {
    // Find molecule by workflow:<beadID> label via storeListBeads
    // Get children via storeGetChildren(molID)
    // Match by title prefix: "design" → "Design approach for..."
    // Return map: {"design": "spi-mol-xxx", "implement": "spi-mol-yyy", ...}
}

func wizardCloseMoleculeStep(beadID, stepName string) {
    // Reuse the same logic as artificer's closeMoleculeStep
    // but via storeCloseBead instead of bd("close", ...)
}
```

Close points:
- After design phase completes → close `design`
- After implement + push → close `implement`
- Review and merge are closed by the reviewer / artificer

### Phase 2: Split wizard into phased claude invocations

Replace the single `wizardRunClaude` call with design + implement phases.

**Design phase:**
- Prompt: "Read the task. Explore the relevant code. Write a plan.
  Do NOT write code. Output your plan to stdout."
- Capture claude stdout → write to `DESIGN.md` in worktree
- Also post plan as bead comment for visibility
- Timeout: 10m (from `agent.design-timeout` in spire.yaml, default 10m)

**Implement phase:**
- Prompt includes: focus context + design plan from `DESIGN.md`
- "Implement the task according to the plan. [validation commands]."
- Timeout: 15m (existing `agent.timeout`)

Claude stdout capture: change `wizardRunClaude` to return stdout as a
string. Currently it routes stdout to stderr; instead, capture it via
`cmd.Output()` and log separately.

### Phase 3: Review agent (`spire wizard-review`)

**New file: `cmd/spire/wizard_review.go`**

Reuses logic from `cmd/spire-artificer/standalone.go` and
`cmd/spire-artificer/review.go`:

```go
func cmdWizardReview(args []string) error
    // Entry: spire wizard-review <bead-id> --name <name>
    // 1. Resolve repo + branch from bead labels (feat-branch:)
    // 2. Create own worktree (separate from wizard's)
    // 3. Fetch + checkout branch
    // 4. Run tests
    // 5. Get diff (git diff main..feat/<bead-id>)
    // 6. Call review via claude Opus (reuse artificer prompt format)
    // 7. Parse structured verdict
    // 8. Handle verdict: approve → labels + close step; request_changes → labels + spire send
    // 9. Clean up worktree, exit

func reviewRunOpus(worktreeDir, spec, diff, testOutput string, round int) (*Review, error)
    // Build review prompt (same structure as artificer's buildReviewPrompt)
    // Run: claude --dangerously-skip-permissions -p <prompt> --model claude-opus-4-6
    // Parse JSON verdict from output

func reviewHandleApproval(beadID string)
    // Remove: review-ready, review-assigned
    // Add: review-approved
    // Close: review molecule step
    // Send: summary to steward via spire send

func reviewHandleRequestChanges(beadID, wizardName string, review *Review, round int)
    // Remove: review-ready, review-assigned
    // Add: review-feedback
    // Replace review-round: remove review-round:{round}, add review-round:{round+1}
    //   (at most one review-round:* label at a time; round 0 if none exists)
    // Send: spire send <wizardName> "<feedback>" --ref <beadID> --as <reviewerName>
    // If round+1 >= 4: add warning comment + spire alert
    // If round+1 >= 5: exit without re-engaging (escalation)
    // LOCAL: wizardRegistryAdd for re-engaged wizard,
    //   spawn spire wizard-run <bead-id> --name <wizardName> --review-fix
    // wizardRegistryRemove(self), exit

func reviewGetRound(beadID string) int
    // Read review-round:* label from bead, parse N, return 0 if absent

func reviewResolveWizardName(beadID string) string
    // Read implemented-by:<name> label from bead (not owner: — wizard already exited)
    // Fallback: derive from bead labels or use "wizard-1"
```

**Modified: `cmd/spire/main.go`**
- Add `case "wizard-review":` dispatch

**Modified: `cmd/spire/wizard.go`**
- After claiming: add `owner:<wizard-name>` label
- At review handoff: swap `owner:` → `implemented-by:`, add `review-ready`
  + `feat-branch:` labels, `wizardRegistryAdd` reviewer,
  spawn `spire wizard-review`, `wizardRegistryRemove` self, exit
- Support `--review-fix` flag:
  1. `wizardRegistryAdd` self on start
  2. Re-add `owner:<wizard-name>` (active work)
  3. `spire collect` → filter by `ref:<bead-id>` → extract feedback
  4. `spire read <msg-id>` for each consumed message (close msg bead)
  5. Remove `review-feedback` label
  6. Skip design phase, include feedback in implement prompt
  7. After fix + push: swap `owner:` → `implemented-by:`, re-add
     `review-ready`, register reviewer, spawn review, unregister, exit

**Modified: `cmd/spire/store.go`**
- `storeGetReadyWork`: post-filter results to exclude workflow step beads
  (parent carries `workflow:*` label) and `msg` beads. Covers local summon
  and steward paths.

**Modified: `cmd/spire/steward.go`**
- `checkBeadHealth`: skip beads with `review-approved` label
- `detectReviewReady`: skip beads with `review-approved` label
- `detectReviewFeedback`: no changes needed — in local mode the reviewer
  handles re-engagement directly; in k8s mode the steward sends a message
  and removes the label (pre-existing gap: no auto-spawn)

**Modified: `operator/controllers/bead_watcher.go`**
- After parsing `bd ready --json`, skip beads whose parent carries a
  `workflow:*` label (same logic as store.go, different data path)

**Modified: `cmd/spire/summon.go`**
- Keep existing epic filter in `summonLocal()` — epics are valid work in
  k8s (routed to workshop/artificer) but not assignable to local wizards
- Remove `msg` filter (now handled by `storeGetReadyWork`)
- Extract `wizardRegistryAdd` / `wizardRegistryRemove` as shared helpers
  with file locking for concurrent access
- `summonLocal` uses `wizardRegistryAdd` instead of inline append+save

### Phase 4: Board progress display

**Modified: `cmd/spire/board.go`**

Add molecule progress to card rendering for in-progress beads:

```
P1 spi-abc task  (2/4)
  Local steward adaptation
  wizard-1 5m ago
```

Implementation:
- For each in-progress task bead, query for a molecule root by label
  `workflow:<task-id>` (same query as `focus.go:28`). The `workflow:`
  label is on the molecule root, NOT on the task bead itself.
- If a molecule root is found, fetch its children (the step beads)
- Count closed vs total children
- Display `(N/M)` after the type in the card
- Cache molecule lookups per board render to avoid repeated queries

### Phase 5: Timer display in roster

**Modified: `cmd/spire/roster.go`**

Show current phase + phase-specific timeout:

```
wizard-1          working  spi-abc  [implement] 3m12s / 15m00s  ███░░░░░░░
wizard-1-review   working  spi-abc  [review]    1m02s / 10m00s  █░░░░░░░░░
```

The wizard writes its current phase to the registry:

```json
{
  "name": "wizard-1",
  "pid": 12345,
  "bead_id": "spi-abc",
  "phase": "implement",
  "phase_started_at": "2026-03-23T13:00:00Z",
  "worktree": "/tmp/spire-wizard/wizard-1/spi-abc",
  "started_at": "2026-03-23T12:50:00Z"
}
```

---

## What's NOT in this plan

- **Artificer / merge**: beads park at `review-approved` after review.
  Merge step stays open. The artificer merge flow is a separate effort.
- **Custom molecules**: formula is hardcoded as spire-agent-work.
- **Docker mode**: process mode only.
- **Multi-round in single process**: the wizard doesn't stay alive for
  review loops. Re-engagement is spawned by the reviewer (local) or
  routed by the steward (k8s).
- **K8s artificer unification**: the artificer's `handleStandaloneApproval`
  still merges + closes on approval. Local mode parks at `review-approved`.
  Unifying these protocols is out of scope.
- **spire logs**: read wizard logs manually for now.

---

## File changes summary

| File | Change |
|------|--------|
| `cmd/spire/wizard.go` | Split into design + implement phases, molecule step closures, `owner:`↔`implemented-by:` swap, `--review-fix` flag with message consume, self-registration, review handoff + exit |
| `cmd/spire/wizard_review.go` | New: review agent (Opus), own worktree, verdict handling, label state machine, spawns wizard on request_changes, self-registration |
| `cmd/spire/main.go` | Add `wizard-review` dispatch |
| `cmd/spire/store.go` | `storeGetReadyWork` post-filter: exclude workflow step beads + `msg` beads (covers local + steward) |
| `cmd/spire/steward.go` | Skip `review-approved` in health checks and review routing |
| `operator/controllers/bead_watcher.go` | Skip workflow step beads after `bd ready --json` parse (same logic, separate data path) |
| `cmd/spire/board.go` | Add molecule progress `(N/M)` to cards via `workflow:<task-id>` lookup |
| `cmd/spire/roster.go` | Show current phase in roster display |
| `cmd/spire/summon.go` | `Phase` + `PhaseStartedAt` in registry, `wizardRegistryAdd`/`Remove` with file locking, keep epic filter (wizard-local), remove `msg` filter (now in store.go) |

---

## Verification

```bash
spire file "Add a hello-world endpoint" -t task -p 2
spire summon 1
spire roster        # shows wizard-1 [design] 0m30s / 10m00s
bd show spi-abc     # labels: owner:wizard-1
# wait...
spire roster        # shows wizard-1 [implement] 2m15s / 15m00s
spire board         # shows bead (2/4)
# wizard exits, reviewer spawns...
bd show spi-abc     # labels: implemented-by:wizard-1, review-ready (no owner:)
spire roster        # shows wizard-1-review [review] 1m00s / 10m00s
                    # wizard-1 is NOT listed (exited, capacity freed)
# review approves...
spire board         # shows bead (3/4) — review closed, merge open
bd show spi-abc     # labels: review-approved, feat-branch:feat/spi-abc
                    # (no implemented-by:, no owner:, no review-ready)
```
