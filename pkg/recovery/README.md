# pkg/recovery

Recovery owns **diagnosis and action proposal** for interrupted parent beads. It
inspects bead state, attempt history, git/worktree status, and executor runtime
state to classify the failure mode and produce a ranked list of recovery actions.

**Important distinction:** "Recovery" in this package is the **data model** —
beads, metadata, failure classification, learnings, and action proposals. The
**agent** that performs recovery is the **cleric** (runs `cleric-default`
formula, action handlers live in `pkg/executor/recovery_phase.go`,
`recovery_decide.go`, `recovery_collect.go`, `recovery_context.go`). This
package provides the diagnostic foundation; the cleric executor code drives the
actual recovery lifecycle.

## Boundaries

- **Executor/runtime** owns setting and clearing `interrupted:*` signals.
- **Board** owns displaying interrupted beads and alerts.
- **Recovery** owns diagnosis, classification, and action proposal.
- Action **execution** is delegated to existing commands (`cmdResummon`,
  `cmdReset`, `cmdClose`) via `cmd/spire/recover.go`.

## Key types

- `Diagnosis` — full diagnostic report for an interrupted bead
- `RecoveryAction` — a proposed action with name, description, destructive flag
- `FailureClass` — categorization of the interruption reason
- `VerifyResult` — post-recovery check that interrupted state is cleared
- `Deps` — dependency injection struct for testability

## Steward integration

The steward imports this package for automated recovery decisions. The `--auto`
flag on `spire recover` executes the first non-destructive action automatically.
If all actions are destructive, it exits with code 2 (escalate). If a wizard is
still running, it exits with code 3 (wait and retry).
