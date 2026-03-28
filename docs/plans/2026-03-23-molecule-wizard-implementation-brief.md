# Implementation Brief: Molecule-Aware Wizard + Review Agent

**Bead:** spi-yyf
**Plan:** `docs/plans/2026-03-23-molecule-aware-wizard-review.md` (rev 7)
**Scope:** 5 implementation phases across ~10 files

> **Note (2026-03-28):** References to "artificer" in this brief refer to the
> old `cmd/spire-artificer/` binary which has been removed. The wizard now
> handles all orchestration, and the sage handles reviews.

---

## What you're building

The wizard (`spire wizard-run`) currently runs as a single claude invocation
that claims a bead, does all work, and pushes. You're upgrading it to:

1. **Follow the spire-agent-work molecule** (design → implement → review →
   merge), closing each step as it progresses
2. **Split into two claude phases** — design (10m) then implement (15m),
   each a fresh context window
3. **Hand off to a review agent** (`spire wizard-review`) that runs Opus,
   reviews the diff, and either approves or requests changes
4. **Support review round-trips** — reviewer spawns `wizard-run --review-fix`
   on request_changes, wizard fixes and re-submits, up to 5 rounds
5. **Show progress** — board displays molecule progress (N/M), roster shows
   current phase + timer

---

## Required reading (in order)

Read these before writing any code:

1. **The plan** — `docs/plans/2026-03-23-molecule-aware-wizard-review.md`
   This is the authoritative spec. It went through 7 review rounds. Every
   design decision is there with rationale. Read it completely.

2. **Key source files** (read all before modifying any):
   - `cmd/spire/wizard.go` — existing wizard, your main modification target
   - `cmd/spire/summon.go` — wizard spawning + registry (wizards.json)
   - `cmd/spire/store.go` — store helpers (storeGetReadyWork, storeAddLabel, etc.)
   - `cmd/spire/steward.go` — work coordinator (findBusyAgents, checkBeadHealth,
     detectReviewReady, detectReviewFeedback)
   - `cmd/spire/focus.go` — molecule pouring + workflow label (workflow:<bead-id>)
   - `cmd/spire/main.go` — command dispatch
   - `cmd/spire/send.go`, `cmd/spire/collect.go`, `cmd/spire/read.go` — messaging
   - `cmd/spire/board.go` — card rendering
   - `cmd/spire/roster.go` — agent status display

3. **Artificer reference code** (read for patterns, do NOT modify):
   - `cmd/spire-artificer/standalone.go` — review flow: workspace init, branch
     resolution, reviewSingleBranch. Model your wizard-review on this.
   - `cmd/spire-artificer/review.go` — buildReviewPrompt, parseReview, Review struct.
     Reuse prompt format and JSON verdict structure.
   - `cmd/spire-artificer/staging.go` — closeMoleculeStep. Mirror this logic
     using spire's store helpers instead of bd().

4. **Formula** — `.beads/formulas/spire-agent-work.formula.toml`
   4 steps: design → implement → review → merge. Steps are child beads of
   a molecule root. The root carries `workflow:<bead-id>` label. Steps have
   no special label — they're identified by parent relationship.

5. **Background docs** — `README.md`, `docs/LOCAL.md`, `docs/ARCHITECTURE.md`

---

## Implementation phases

### Phase 1: Molecule step closures, owner label lifecycle, self-registration

**Files:** `cmd/spire/wizard.go`, `cmd/spire/summon.go`

Three things to wire:

**A. Owner label lifecycle.** The wizard adds `owner:<wizard-name>` after
claiming. At review handoff, it swaps to `implemented-by:<wizard-name>`.
This is critical: `findBusyAgents()` (steward.go:324) scans in_progress
beads for `owner:` labels. If you leave `owner:` on the bead after the
wizard exits, that wizard name stays "busy" forever.

```
claim → add owner:<name>
review handoff → remove owner:<name>, add implemented-by:<name>
--review-fix entry → add owner:<name>
--review-fix handoff → remove owner:<name>, add implemented-by:<name>
approval → reviewer removes implemented-by:<name>
```

**B. Self-registration.** Extract `wizardRegistryAdd(entry)` /
`wizardRegistryRemove(name)` from summon.go as shared helpers with
file locking. Every process (wizard-run, wizard-review, wizard-run
--review-fix) self-registers on start, self-unregisters on exit.
Use defer + signal handling for cleanup.

**C. Molecule step closures.** Write `wizardFindMoleculeSteps(beadID)`
and `wizardCloseMoleculeStep(beadID, stepName)`. Mirror the logic from
`cmd/spire-artificer/staging.go:closeMoleculeStep` but using spire's
`storeListBeads`, `storeGetChildren`, `storeCloseBead`. Find the molecule
root by querying for beads with label `workflow:<beadID>`. Match steps by
title prefix ("Design approach" → design, "Implement" → implement, etc.).

### Phase 2: Split wizard into phased claude invocations

**Files:** `cmd/spire/wizard.go`

Replace the single `wizardRunClaude` call with two phases:

**Design phase (10m):** Prompt says "read the task, explore code, write a
plan, do NOT write code." Capture claude's stdout (change `wizardRunClaude`
to return stdout via `cmd.Output()`). Write output to `DESIGN.md` in the
worktree. Post plan as bead comment via `storeAddComment`. Close the
design molecule step.

**Implement phase (15m):** Include `DESIGN.md` content in the prompt.
Standard implementation + validation. Close the implement molecule step.

Timeout values: read `agent.design-timeout` and `agent.timeout` from
spire.yaml (via repoconfig).

### Phase 3: Review agent + scheduling guards

**New file:** `cmd/spire/wizard_review.go`
**Modified:** `cmd/spire/main.go`, `cmd/spire/wizard.go`, `cmd/spire/store.go`,
`cmd/spire/steward.go`, `cmd/spire/summon.go`, `operator/controllers/bead_watcher.go`

**A. wizard-review command.** Entry point: `spire wizard-review <bead-id>
--name <name>`. Self-registers, creates own worktree, fetches branch (read
`feat-branch:` label from bead), diffs against main, runs Opus review,
handles verdict. Model on `cmd/spire-artificer/standalone.go:runReviewMode`.

Review prompt: reuse the structure from `cmd/spire-artificer/review.go:
buildReviewPrompt`. Output must be parseable JSON with verdict
(approve/request_changes) and feedback text.

**B. Verdict handling:**
- Approve: remove review-ready + review-assigned + implemented-by, add
  review-approved, close review molecule step, self-unregister, exit.
- Request changes: remove review-ready + review-assigned, add review-feedback,
  replace review-round:N (remove old, add N+1; at most one label), send
  feedback via `spire send <wizard> "<feedback>" --ref <bead-id>`, register
  re-engaged wizard, spawn `spire wizard-run <bead-id> --name <wizard> --review-fix`,
  self-unregister, exit.
- Round escalation: if round >= 4, add warning comment. If round >= 5,
  exit without spawning (escalation).

**C. --review-fix flag in wizard.** When set: re-add owner label, collect
feedback messages (`spire collect` → filter by `ref:<bead-id>` → close
consumed via `spire read <msg-id>`), remove review-feedback label, skip
design phase, include feedback text in implement prompt, then normal
implement → handoff → exit.

**D. Scheduling guards:**
- `storeGetReadyWork()` (store.go): post-filter to exclude beads whose
  parent carries a `workflow:*` label, and beads with `msg` label.
- `bead_watcher.go`: same parent-label check after parsing `bd ready --json`.
- `summonLocal()`: keep epic filter (epics are valid k8s work, not wizard work).
  Remove msg filter (now in storeGetReadyWork).
- `steward.go`: `checkBeadHealth` and `detectReviewReady` skip beads with
  `review-approved` label.

**E. main.go dispatch:** Add `case "wizard-review":` before `case "doctor":`.

### Phase 4: Board progress display

**Files:** `cmd/spire/board.go`

For in-progress task beads, query for molecule root by label
`workflow:<task-id>` (same pattern as `focus.go:28`). The `workflow:` label
is on the molecule root, NOT on the task bead. Fetch root's children, count
closed vs total, display `(N/M)` after the type. Cache lookups per render.

### Phase 5: Roster phase display

**Files:** `cmd/spire/roster.go`, `cmd/spire/summon.go`

Add `Phase` and `PhaseStartedAt` fields to `localWizard` struct. Wizard
updates these in the registry when entering each phase. Roster reads them
and displays `[phase] elapsed / timeout` with progress bar.

---

## Critical invariants

These were the hardest-won design decisions from review. Violating any
of them will break the system:

1. **owner: label = active work capacity.** `findBusyAgents()` uses it.
   MUST be removed at review handoff, re-added only during active --review-fix.
   `implemented-by:` is the routing label for the review period.

2. **Workflow step beads MUST NOT be scheduled.** Open molecule steps
   (especially the merge step) look like normal ready work. Both store.go
   and bead_watcher.go need the parent-workflow check.

3. **Reviewer spawns wizard directly in local mode.** The steward's
   `detectReviewFeedback()` only sends a message — it does NOT spawn
   processes. If you rely on the steward, the review loop dead-ends.

4. **review-round:N is replace, not append.** At most one label exists.
   Remove old before adding new. The reviewer reads N to decide escalation.

5. **Messages must be consumed.** Re-engaged wizard filters by `ref:<bead-id>`,
   closes each with `spire read <msg-id>`. Otherwise stale feedback accumulates
   and summon will try to schedule msg beads as work (without the store.go guard).

6. **Every process self-registers and self-unregisters.** Otherwise roster
   can't see reviewer and review-fix processes.

---

## What NOT to do

- Do NOT modify `cmd/spire-artificer/` — read it for patterns only.
- Do NOT modify `bd` or `pkg/bd/` — the beads library boundary stays intact.
- Do NOT merge after review approval — beads park at `review-approved`.
  Merge is out of scope.
- Do NOT make the wizard stay alive during review — it's one-shot.
- Do NOT put epic filtering in storeGetReadyWork — epics are valid k8s work.

---

## Build and test

```bash
cd /Users/jb/awell/spire
go build ./cmd/spire/
go test ./cmd/spire/ -run TestWizard -v
go vet ./cmd/spire/
```

## Commit format

```
feat(spi-yyf): <message>
```

One commit per phase is fine. Push branch `feat/spi-yyf`.

---

## Verification

After all phases, this sequence should work:

```bash
spire file "Add a hello-world endpoint" -t task -p 2
spire summon 1
spire roster        # wizard-1 [design] 0m30s / 10m00s
bd show spi-abc     # labels: owner:wizard-1
# wait...
spire roster        # wizard-1 [implement] 2m15s / 15m00s
spire board         # bead (2/4)
# wizard exits, reviewer spawns...
bd show spi-abc     # labels: implemented-by:wizard-1, review-ready (no owner:)
spire roster        # wizard-1-review [review] — wizard-1 NOT listed
# review approves...
spire board         # bead (3/4) — review closed, merge open
bd show spi-abc     # labels: review-approved, feat-branch:feat/spi-abc
```
