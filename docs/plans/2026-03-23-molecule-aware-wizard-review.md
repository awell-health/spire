# Molecule-Aware Wizard + Review Agent

**Goal:** Wizards follow the spire-agent-work molecule (design → implement →
review → merge), close steps as they progress, and hand off to an Opus-powered
review agent. Wizard exits after implementation; reviewer is a separate
one-shot process. Both use the existing label-based review state machine.

**Date:** 2026-03-23 (rev 2 — addresses review findings)

---

## Architecture

### Per-bead lifecycle

```
spire summon 1
  → picks ready bead spi-abc
  → spawns: spire wizard-run spi-abc --name wizard-1

wizard-run spi-abc:
  1. claim bead, pour molecule (design → implement → review → merge)
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
     - add labels: review-ready, feat-branch:feat/spi-abc
     - send message to reviewer via spire send
     - spawn: spire wizard-review spi-abc --name wizard-1-review
     - wizard process exits (one-shot, capacity freed)
  5. wizard-review runs (separate process, Opus model)
     - creates its own worktree (reads pushed commits, not live edits)
     - diffs feat/spi-abc against main
     - runs Opus review (reuses artificer's review prompt/parsing)
     - APPROVE → close review step, add review-approved label,
       remove review-ready, bead status stays in_progress
     - REQUEST_CHANGES → remove review-ready, add review-feedback,
       send feedback via spire send to wizard owner
  6. RE-ENGAGEMENT (if request_changes)
     - steward.detectReviewFeedback() picks up review-feedback label
     - steward re-spawns wizard with feedback context
     - wizard fixes, pushes, re-adds review-ready → loop back to 5
     - max 3 round trips; 4th triggers warning + complexity assessment
  7. POST-APPROVAL STATE
     - labels: review-approved, feat-branch:feat/spi-abc
     - labels removed: review-ready, review-assigned, review-feedback
     - molecule: design ✓, implement ✓, review ✓, merge open
     - bead: in_progress (steward skips review-approved beads)
     - merge deferred to artificer or manual
```

### Key design decisions

**Wizard is one-shot (finding #3).** The wizard exits after implementation
and review handoff. This preserves the ephemeral worker model, frees local
capacity immediately, and matches the existing k8s architecture. Re-engagement
for review feedback goes through the steward, same as k8s mode.

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
wizard pushes branch
  → adds: review-ready, feat-branch:feat/spi-abc
  → wizard exits

reviewer spawns (or steward routes in k8s)
  → adds: review-assigned
  → reviews diff via Opus

verdict = approve
  → removes: review-ready, review-assigned
  → adds: review-approved
  → closes: review molecule step
  → reviewer exits

verdict = request_changes
  → removes: review-ready, review-assigned
  → adds: review-feedback
  → sends feedback via spire send
  → reviewer exits

steward.detectReviewFeedback()
  → removes: review-feedback
  → re-spawns wizard with feedback message
  → wizard fixes, pushes, adds review-ready
  → cycle repeats
```

### Round trip limit

| Round | Action |
|-------|--------|
| 1-3   | Normal: reviewer requests changes, steward re-engages wizard |
| 4     | **Warning**: comment on bead + spire alert, assess if bead should be split |
| 5+    | **Escalate**: leave bead in review state, alert user, stop re-engaging |

The steward tracks round count via a `review-round:N` label incremented
each time `review-feedback` is added.

---

## Implementation

### Phase 1: Molecule step closures in wizard

Wire the existing wizard to find and close steps.

**Modified: `cmd/spire/wizard.go`**

New helper (mirrors `closeMoleculeStep` from `cmd/spire-artificer/staging.go`
but using the spire CLI's store helpers):

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

func reviewHandleRequestChanges(beadID string, review *Review)
    // Remove: review-ready, review-assigned
    // Add: review-feedback
    // Increment: review-round:N label
    // Send: structured feedback via spire send to wizard owner
    // If round >= 4: add warning comment + spire alert
```

**Modified: `cmd/spire/main.go`**
- Add `case "wizard-review":` dispatch

**Modified: `cmd/spire/wizard.go`**
- After implement phase: add `review-ready` + `feat-branch:` labels
- Spawn `spire wizard-review` as background process
- Wizard exits (does NOT wait for review)

**Modified: `cmd/spire/steward.go`**
- `checkBeadHealth`: skip beads with `review-approved` label
- `detectReviewReady`: skip beads with `review-approved` label
- No other steward changes needed — `detectReviewFeedback` already
  handles re-engagement

### Phase 4: Board progress display

**Modified: `cmd/spire/board.go`**

Add molecule progress to card rendering for in-progress beads:

```
P1 spi-abc task  (2/4)
  Local steward adaptation
  wizard-1 5m ago
```

Implementation:
- For beads with `workflow:` label, fetch molecule children
- Count closed vs total
- Display `(N/M)` after the type in the card

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
  review loops. Re-engagement is through the steward, same as k8s.
- **spire logs**: read wizard logs manually for now.

---

## File changes summary

| File | Change |
|------|--------|
| `cmd/spire/wizard.go` | Split into design + implement phases, molecule step closures, review handoff + exit |
| `cmd/spire/wizard_review.go` | New: review agent (Opus), own worktree, verdict handling, label state machine |
| `cmd/spire/main.go` | Add `wizard-review` dispatch |
| `cmd/spire/steward.go` | Skip `review-approved` in health checks and review routing |
| `cmd/spire/board.go` | Add molecule progress `(N/M)` to cards |
| `cmd/spire/roster.go` | Show current phase in roster display |
| `cmd/spire/summon.go` | Add `Phase` + `PhaseStartedAt` to wizard registry |

---

## Verification

```bash
spire file "Add a hello-world endpoint" -t task -p 2
spire summon 1
spire roster        # shows wizard-1 [design] 0m30s / 10m00s
# wait...
spire roster        # shows wizard-1 [implement] 2m15s / 15m00s
spire board         # shows bead (2/4)
# wizard exits, reviewer spawns...
spire roster        # shows wizard-1-review [review] 1m00s / 10m00s
# review approves...
spire board         # shows bead (3/4) — review closed, merge open
bd show spi-abc     # labels: review-approved, feat-branch:feat/spi-abc
```
