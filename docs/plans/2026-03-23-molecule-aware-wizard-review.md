# Molecule-Aware Wizard + Review Agent

**Goal:** Wizards follow the spire-agent-work molecule (design → implement →
review → merge), close steps as they progress, and hand off to an Opus-powered
review agent before merge. Wizard and reviewer stay alive for the full
review loop.

**Date:** 2026-03-23

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
     - close design step
  3. IMPLEMENT phase (new claude instance, 15m timeout)
     - run claude with implementation prompt
     - validate (lint, build, test)
     - commit + push branch feat/spi-abc
     - close implement step
  4. REVIEW HANDOFF
     - spawn: spire wizard-review spi-abc --name wizard-1-review
     - send message to review bead: "Ready for review"
     - wizard stays alive, waiting for review outcome
  5. REVIEW LOOP (reviewer is Opus)
     - reviewer checks out branch, diffs against main
     - reviewer sends verdict (approve / request_changes)
     - if request_changes: wizard gets message, runs new claude
       instance to fix, pushes again, signals reviewer
     - loop up to 3 round trips
     - on 4th review: warn user, file complexity beads if needed
     - on approve: close review step
  6. MERGE (deferred — left for artificer or manual)
     - bead stays in review-ready state with review step closed
     - merge step stays open until artificer is built
  7. wizard + reviewer exit
```

### Separate claude instances per phase

Each molecule phase gets its own `claude` invocation with a fresh context
window. This gives:
- **Clean context**: design doesn't pollute implementation, implementation
  doesn't pollute review feedback handling
- **Per-phase timeouts**: design=10m, implement=15m, review-fix=10m
- **Better observability**: each phase has its own log section

The wizard process (`spire wizard-run`) stays alive across all phases —
it's the orchestrator. Only the claude subprocess is restarted per phase.

### Review agent

The review agent (`spire wizard-review`) is a separate process:
- Uses **Opus** (not Sonnet) for review quality
- Runs in the **same worktree** as the wizard (reads the branch)
- Communicates with the wizard via **comments on the review bead**
- Each review round is a fresh claude invocation

### Communication: wizard ↔ reviewer

All communication flows through the review molecule step's bead:

```
wizard:    storeAddComment(reviewBeadID, "Implementation complete. Ready for review.")
reviewer:  storeAddComment(reviewBeadID, '{"verdict":"request_changes","issues":[...]}')
wizard:    storeAddComment(reviewBeadID, "Fixed. Ready for re-review.")
reviewer:  storeAddComment(reviewBeadID, '{"verdict":"approve","summary":"LGTM"}')
```

The review bead serves as the communication channel. Both wizard and
reviewer poll for new comments. Structured JSON for machine-readable
verdicts, human-readable summaries.

### Round trip limit

| Round | Action |
|-------|--------|
| 1-3   | Normal review loop (reviewer requests changes, wizard fixes) |
| 4     | **Warning**: notify user, consider filing sub-beads for complexity |
| 5+    | Escalate: leave bead in review state, alert user |

The warning at round 4 is a comment on the bead + a `spire alert` if
configured. The goal is to catch beads that are too complex for a single
wizard pass and break them down.

---

## Implementation

### Phase 1: Molecule step closures in wizard

Wire the existing wizard to close steps as it progresses.

**Modified: `cmd/spire/wizard.go`**

```go
// After claiming + focus (molecule is poured by focus):
molID, stepIDs := wizardFindMoleculeSteps(beadID)

// After design phase:
storeCloseBead(stepIDs["design"])

// After implement phase:
storeCloseBead(stepIDs["implement"])

// After review approved:
storeCloseBead(stepIDs["review"])

// Merge left open (deferred to artificer)
```

New helper:
```go
func wizardFindMoleculeSteps(beadID string) (molID string, steps map[string]string) {
    // Find molecule by workflow:<beadID> label
    // Get children, match by title prefix ("Design approach", "Implement", etc.)
    // Return map: {"design": "spi-mol-xxx", "implement": "spi-mol-yyy", ...}
}
```

### Phase 2: Split wizard into phased claude invocations

Replace the single `wizardRunClaude` call with per-phase invocations.

```go
// Design phase
designPrompt := wizardBuildDesignPrompt(...)
wizardRunClaude(worktreeDir, designPrompt, model, "10m")  // 10m timeout
storeCloseBead(stepIDs["design"])

// Implement phase
implPrompt := wizardBuildImplementPrompt(...)
wizardRunClaude(worktreeDir, implPrompt, model, "15m")  // 15m timeout
wizardValidate(...)
wizardCommitAndPush(...)
storeCloseBead(stepIDs["implement"])
```

Design prompt: "Read the task, explore relevant code, form a plan. Do not
write code. Write your plan as a comment on the bead."

Implement prompt: "Implement the task. The design plan is below. [design
output]. Validation commands: [lint/build/test]."

### Phase 3: Review agent (`spire wizard-review`)

**New file: `cmd/spire/wizard_review.go`**

```go
func cmdWizardReview(args []string) error
    // Entry: spire wizard-review <bead-id> --name <name> --worktree <path>

func reviewLoop(beadID, worktreeDir, reviewBeadID string, maxRounds int)
    // For each round:
    //   1. Get diff (git diff main..feat/<bead-id>)
    //   2. Get bead spec (description + focus context)
    //   3. Run review claude instance (Opus model)
    //   4. Parse verdict
    //   5. Post verdict as comment on review bead
    //   6. If approve: return
    //   7. If request_changes: wait for wizard to signal re-review

func reviewBuildPrompt(spec, diff, testOutput string, round int) string
    // Mirrors artificer's buildReviewPrompt but adapted for local mode

func reviewWaitForWizardFix(reviewBeadID string, timeout time.Duration) bool
    // Poll review bead comments for wizard's "ready for re-review" signal
```

**Modified: `cmd/spire/main.go`**
- Add `case "wizard-review":` dispatch

**Modified: `cmd/spire/wizard.go`**
- After implement phase, spawn `spire wizard-review` subprocess
- Wait for review outcome (poll review bead comments)
- On `request_changes`: run new claude instance with review feedback
- On `approve`: close review step, exit

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
wizard-1  working  spi-abc  [implement] 3m12s / 15m00s  ███░░░░░░░
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

- **Artificer merge**: beads stay in review-ready state after review
  approval. The merge molecule step stays open. This is fine — we'll build
  the local artificer merge flow separately.
- **Custom molecules**: the formula is hardcoded as spire-agent-work. User-
  definable molecules are a future enhancement.
- **Docker mode**: process mode only.
- **Steward integration**: wizards self-serve via `spire summon`.
- **spire logs**: read wizard logs manually for now.

---

## File changes summary

| File | Change |
|------|--------|
| `cmd/spire/wizard.go` | Split into phased invocations, molecule step closures, review handoff |
| `cmd/spire/wizard_review.go` | New: review agent (Opus), review loop, verdict parsing |
| `cmd/spire/main.go` | Add `wizard-review` dispatch |
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
# wait...
spire roster        # shows wizard-1 [review] waiting for reviewer
                    # shows wizard-1-review [reviewing] 1m00s / 10m00s
# review approves...
spire board         # shows bead (3/4) — review closed, merge still open
```
