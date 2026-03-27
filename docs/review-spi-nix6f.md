# Review Brief: spi-nix6f — DAG is Truth

## Epic

**spi-nix6f** — DAG is truth: runtime state lives in the bead graph

**Design bead:** spi-fuzrf (closed, revised 2026-03-27)

**Scope:** 19 commits, 22 files changed, +2,570 / -276 lines (v0.20.0 through v0.20.3)

## Problem being solved

Runtime truth in the **local/CLI control plane** was split across multiple systems: bead labels (phase:, owner:, review-ready, etc.), wizard registry (wizards.json), and executor state files. Roster, steward, board, and claim each assembled a different projection from different subsets of these sources. They disagreed when the system was mid-flight.

**Out of scope:** k8s operator state (SpireAgent.Status.CurrentWork, SpireWorkload phase) is a separate follow-on tracked as spi-q0vma, blocked until this local migration is stable.

## Solution

Three types of child beads replace label-based runtime state in cmd/spire/:

1. **Attempt beads** — one per execution attempt. Records agent name, model, branch, timestamps. Created on executor start, closed on terminal path.
2. **Review round beads** — one per sage review. Created open when sage dispatched, closed with structured verdict when sage returns.
3. **Workflow step beads** — one per formula phase (design, plan, implement, review, merge). Created at bead start from formula definition. Phase = which step bead is open.

Label writes for owner:, phase:, review-ready, review-feedback, review-assigned, implemented-by:, and review-round: are removed from the local/CLI control plane. The bead graph is the authority for all scheduling, routing, and gating decisions in cmd/spire/.

## Subtask breakdown

| Subtask | What it did | Files |
|---------|-------------|-------|
| .1 | Attempt beads: shared query + executor writer + roster/steward/store readers | store.go, executor.go, roster.go, steward.go, board.go, attempt_test.go |
| .2 | Review round beads: structured verdicts, review history chain, board filtering | store.go, wizard_review.go, wizard.go, board.go, review_round_test.go |
| .3 | Workflow step beads: formula pour, phase transitions, board/phase.go readers | store.go, executor.go, phase.go, board.go, step_bead_test.go |
| .4 | **Split after repeated failed attempts.** Originally "drop dual-write" — too broad (19 setPhase call sites + steward routing migration). Two review rounds with request_changes plus failed implementation attempts. Arbiter split into .9-.12. Closed when all children landed. | — |
| .5 | Migrate claim.go: owner: label → storeGetActiveAttempt | claim.go, claim_test.go |
| .6 | Migrate steward.go: owner: label → storeGetActiveAttempt | steward.go, steward_local_test.go, store.go |
| .7 | Migrate webhook.go: owner: label → attempt bead query | webhook.go, webhook_test.go |
| .8 | Migrate wizard_review.go: review-round: label → storeGetReviewBeads | wizard_review.go, review_round_test.go |
| .9 | Remove setPhase + all 19 call sites (split from .4) | phase.go, executor.go, wizard.go, wizard_review.go, close_advance.go, workshop_implement.go, workshop_review.go |
| .10 | Remove owner: label writes (split from .4) | wizard.go, summon.go, claim.go |
| .11 | Migrate steward review routing from labels to review beads + remove review-ready/feedback/assigned label writes (split from .4) | steward.go, wizard_review.go, wizard.go, terminal_steps.go, summon.go, executor.go, workshop_review.go |
| .12 | Remove implemented-by: and review-round: label writes (split from .4, closed — covered by .11) | (included in .11's changes) |

## Invariants to verify

1. **At most one open attempt bead per parent.** storeGetActiveAttempt returns error if >1 exist. Check: is this enforced in every creation path? What if two executors race?

2. **At most one active (in_progress) step bead per parent.** storeGetActiveStep returns error if >1 exist. Check: is ensureStepBeads idempotent on resume?

3. **Attempt beads never appear on the board.** isAttemptBead/isAttemptBoardBead filters in storeGetReadyWork and categorizeColumnsFromStore. Check: any path that bypasses these filters?

4. **Step beads never appear on the board.** Same pattern. Check: same question.

5. **Review round beads never appear on the board.** Same pattern.

6. **No authoritative label reads remain.** After .9/.10/.11, no scheduling, routing, or gating decision uses owner:, phase:, review-ready, review-feedback, review-assigned, implemented-by:, or review-round: as input. Check: grep for ALL of these labels in non-test, non-comment code — any surviving authority paths?

7. **setPhase is fully removed.** Deleted from phase.go, all 19 call sites deleted. Check: any remaining references? isValidPhase kept (used by formula.go).

8. **Steward review routing uses review beads.** detectReviewReady and detectReviewFeedback now query beads, not labels. Check: is the bead query correct? Does it handle edge cases (no review beads, multiple review beads, review bead without verdict)?

## Files to focus on

**Highest priority (new shared infrastructure):**
- `cmd/spire/store.go` (+333 lines) — all new store helpers: attempt, review round, and step bead CRUD, shared queries, bead type detection, ready-work filtering

**High priority (writer changes):**
- `cmd/spire/executor.go` (+207 lines) — attempt bead lifecycle, step bead transitions, ensureStepBeads, ensureAttemptBead

**High priority (reader migrations):**
- `cmd/spire/steward.go` (+152 lines) — review routing rewritten from labels to review beads
- `cmd/spire/claim.go` — concurrency gating via attempt beads
- `cmd/spire/phase.go` — getPhase reads step beads first, labels as fallback

**Medium priority (label removal):**
- `cmd/spire/wizard.go` — owner:/review-ready/review-feedback/implemented-by writes removed
- `cmd/spire/wizard_review.go` — review-assigned/review-round/review-feedback writes removed, review bead creation/closure added

**Test files (verify coverage):**
- `cmd/spire/attempt_test.go` (584 lines)
- `cmd/spire/review_round_test.go` (342 lines)
- `cmd/spire/step_bead_test.go` (523 lines)
- `cmd/spire/steward_local_test.go` (256 lines)
- `cmd/spire/webhook_test.go` (113 lines)
- `cmd/spire/claim_test.go` (74 lines)

Run `go test ./cmd/spire/ -v -count=1 -timeout=30s` for current line counts and test names.

## Specific review questions

1. **Race condition:** Two executors claiming the same bead could both call storeCreateAttemptBead before either checks storeGetActiveAttempt. Is there a transaction or lock? (store.go)

2. **Resume correctness:** ensureAttemptBead handles resume (existing attempt in state) and reclaim (same agent's orphaned attempt). Is the orphan detection safe? (executor.go)

3. **Steward performance:** detectReviewReady now queries ALL in_progress beads, then filters in Go by checking review round beads for each. At scale (hundreds of in_progress beads), this is O(N) store queries. Is this acceptable? (steward.go)

4. **Review bead verdict parsing:** reviewBeadVerdict extracts verdict from bead description (format: "verdict: X\n\nSummary"). Is this robust enough? What if the format varies? (steward.go)

5. **Step bead idempotency:** ensureStepBeads creates step beads for all formula phases. On resume, it should not create duplicates. Is the check correct? (executor.go)

6. **Cleanup gap:** When the executor closes an attempt bead, does it also handle the case where the step beads are left in an inconsistent state (e.g., implement step still open when the attempt fails)?

7. **History cleanliness:** The commit history includes a revert (78c48ec) of an initial no-op approach that was later replaced by the full deletion (.9). Is this acceptable for mainline history?

## How to run the review

```bash
# See the full diff
git diff v0.20.0^..v0.20.3

# See just the store.go changes
git diff v0.20.0^..v0.20.3 -- cmd/spire/store.go

# Run the new tests
go test ./cmd/spire/ -run "TestAttempt|TestReview|TestStep|TestClaim|TestSteward|TestWebhook" -v -count=1

# Run all tests
go test ./cmd/spire/ -count=1 -timeout=30s

# Check for surviving label authority reads
grep -rn "owner:\|phase:\|review-ready\|review-feedback\|review-assigned\|implemented-by:\|review-round:" cmd/spire/*.go | grep -v test | grep -v _test.go | grep -v "//" | grep -v "addLabel\|removeLabel\|Label.*remove\|Label.*add"
```
