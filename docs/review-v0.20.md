# Review Brief: v0.19.12 → v0.20.3

## Scope

64 commits, 77 files changed, +9,139 / -805 lines. One session.

## What to review

This review covers the Spire v0.20 release — the "DAG is truth" arc.
The central architectural change is migrating runtime state from scattered
labels, registries, and state files to first-class child beads in the
dependency graph.

## Tag progression

| Tag | What landed |
|-----|-------------|
| v0.19.13 | Docs update (PLAN, ARCHITECTURE, LOCAL, VISION, metrics) + roster UX fix |
| v0.19.14 | Interactive board: navigation (h/j/k/l), selection, actions (s/f/c/L) |
| v0.19.15 | Fix bead.Parent — storeGetBead now populates deps from GetDependenciesWithMetadata |
| v0.19.16 | All formulas use Opus for implementation |
| v0.19.17 | ref: label → discovered-from dep migration + docs/Helm chart |
| v0.19.18 | Review DAG: arbiter split merges staging, discard deletes branches |
| v0.19.19 | SPIRE.md rewrite with full agent work instructions |
| v0.19.20 | Embedded SPIRE.md template — single source of truth for agent instructions |
| v0.19.21 | Wizard git config uses --worktree to avoid polluting main repo |
| v0.19.22 | Apprentice prompt: commit before validate, never revert work |
| v0.19.23 | Board sort stability and time parsing robustness |
| v0.19.24 | Board phase labels on subtask beads + review-round matching fix |
| v0.19.25 | Detect Claude's own commits — stop false "finished without changes" |
| v0.19.26 | Review formula DAG: step-graph formula, terminal steps, human escalation |
| v0.19.27 | All wizard commits use archmage identity (author + committer) |
| v0.19.28 | spire reset, spire dismiss --targets, dead wizard cleanup |
| v0.19.29 | TUI: agent panel, type filter, contextual footer |
| v0.20.0 | **DAG Phase 1:** attempt beads — every execution is a child bead |
| v0.20.1 | **DAG Phase 2+3:** review round beads + workflow step beads |
| v0.20.2 | Migrate all label authority reads to DAG queries |
| v0.20.3 | **DAG Phase 4:** drop dual-write — labels no longer authoritative |

## Architectural changes

### 1. DAG is truth (spi-nix6f) — the core change

**Before:** Runtime state scattered across 5 systems:
- Bead labels (phase:, owner:, review-ready, review-feedback, etc.)
- Wizard registry (wizards.json — PIDs, bead IDs, phases)
- Executor state files (~/.config/spire/runtime/<name>/state.json)
- k8s status (SpireAgent.Status.CurrentWork)
- Git state (branches, worktrees)

**After:** Three types of child beads replace all label-based state:
- **Attempt beads** — one per execution attempt, records agent/model/branch
- **Review round beads** — one per sage review, records verdict/summary
- **Workflow step beads** — one per formula phase (design/plan/implement/review/merge)

**Key invariants:**
- At most one open attempt bead per parent
- At most one open step bead per parent
- storeGetActiveAttempt() is the shared query for roster, steward, board, claim
- setPhase() is deleted — phase = which step bead is open
- owner: labels no longer written — attempt beads are the authority
- review-ready/feedback/assigned labels removed — review beads are the authority

**Files to review carefully:**
- `cmd/spire/store.go` — storeGetActiveAttempt, storeGetActiveStep, storeGetReviewBeads, attempt/step/review bead CRUD
- `cmd/spire/executor.go` — attempt bead creation/closure, step bead transitions
- `cmd/spire/phase.go` — getPhase now reads step beads first, falls back to labels
- `cmd/spire/steward.go` — review routing migrated from labels to review beads
- `cmd/spire/claim.go` — concurrency gating via attempt beads
- `cmd/spire/board.go` — filters attempt/step/review beads from display

### 2. Review DAG (spi-0ky8g)

Declarative review formula (review-phase.formula.toml) with terminal step
enforcement. Every review path ends with branch merged or deleted.

**Files:** terminal_steps.go (new), review-phase.formula.toml, wizard_review.go

### 3. Board interactivity (spi-1syd)

Navigation (h/j/k/l), card selection (▶ cursor), epic scoping (e key),
actions (s summon, f focus, c claim, L logs), agent panel, type filter.

**Files:** board.go, board_actions.go

### 4. Pipeline reliability fixes

- Apprentice prompt: commit before validate, never revert (v0.19.22)
- Detect Claude's own commits (v0.19.25)
- Wizard git config scoped to worktree (v0.19.21)
- Archmage identity for all commits (v0.19.27)
- spire reset command (v0.19.28)
- Dead wizard cleanup on roster/up/summon (v0.19.28)
- Plan phase enriches pre-filed subtasks with change specs (spi-qcl17)

## Test coverage

New test files:
- `cmd/spire/attempt_test.go` (~590 lines) — 21 tests for attempt bead lifecycle
- `cmd/spire/review_round_test.go` (~300 lines) — 12 tests for review round beads
- `cmd/spire/step_bead_test.go` (~523 lines) — 17 tests for step bead lifecycle
- `cmd/spire/claim_test.go` (~77 lines) — claim gate via attempt beads
- `cmd/spire/steward_local_test.go` (~51 lines) — steward skips beads with active attempts
- `cmd/spire/board_test.go` — sort stability, height budget, type filter tests

## Known gaps / open items

1. **spi-1syd.3** — TUI inspector pane (bead detail view). Filed, not implemented.
2. **spi-0ky8g.2** — Executor walks review molecule (pours formula as actual beads). The step-graph formula exists but the executor still uses a hardcoded loop internally.
3. **spi-q0vma** — k8s operator: demote SpireAgent.Status.CurrentWork to DAG projection. Blocked until local control plane is stable.
4. **spi-xomcj** — Adaptive model selection (Sonnet-first with Opus escalation). Design complete, implementation partial.
5. **Integration tests gated behind SPIRE_INTEGRATION=1** — the dolt-dependent test hangs without a live server.

## Design beads (closed, for context)

- **spi-fuzrf** — DAG is truth: single source of runtime state (revised with corrected file boundaries)
- **spi-vu29d** — Review formula: declarative review DAG with terminal invariants
- **spi-dxxxo** — Adaptive model selection: Sonnet-first with Opus escalation
- **spi-x4jdg** — Board phase tracking for subtask beads
- **spi-2r5tf** — Board/roster/watch: show executor DAG progress (OPEN — ongoing monitoring design)

## Questions for the reviewer

1. Are the attempt/step/review bead invariants (at-most-one-open) enforced everywhere? What happens if two executors race on the same bead?
2. Is the steward review routing migration (labels → review beads) correct? The steward now queries all in_progress beads and filters in Go — is this a performance concern at scale?
3. The board filters attempt/step/review beads from display. Should any of these be visible? The inspector pane (spi-1syd.3) would show them when you drill into a bead.
4. setPhase is deleted with 19 call sites removed. Are there any remaining code paths that expect phase: labels to exist?
5. The revert of the initial setPhase no-op (78c48ec → fe04fa9) is in the history. Is this clean enough or should the history be rewritten?
