# Vestigial CloseMoleculeStep Removal

## What happened (spi-vm91e)

The wizard and executor carried `CloseMoleculeStep` / `FindMoleculeSteps` infrastructure from the pre-v3 molecule-based workflow tracking system. In v3, `GraphState` manages step bead lifecycle (activate/close) through the graph interpreter via `StepBeadIDs`, making the molecule approach redundant.

Removed across four layers:

- **`pkg/wizard/wizard.go`** — deleted `CloseMoleculeStep()` and `FindMoleculeSteps()` implementations (~60 lines) and three call sites in the implement/design phase transitions.
- **`pkg/wizard/deps.go`** — removed `FindMoleculeSteps` and `CloseMoleculeStep` fields from the `Deps` struct.
- **`pkg/executor/deps.go`** — removed `CloseMoleculeStep` field from the executor `Deps` struct.
- **`cmd/spire/wizard_bridge.go` + `executor_bridge.go`** — removed bridge functions and wiring that connected the dep fields to their implementations.
- **`PLAYBOOK.md`** — updated the phase-tracking table to reference `GraphState step beads` instead of `Molecule children`, and the transition description to reflect graph interpreter ownership.

## Why it matters

The molecule step system predated GraphState. Keeping the call sites around implied that molecule tracking was still load-bearing for step lifecycle — it wasn't. The `CloseMoleculeStep` calls silently failed (no molecule beads existed to close) and added confusion about which system actually managed step transitions.

## How to spot similar issues

1. **Look for functions that fail silently.** `CloseMoleculeStep` printed a stderr warning when no molecule was found, then returned. If a function's error path is "warn and continue" and it always hits that path, the function is vestigial.
2. **Check Deps structs after architectural migrations.** When a subsystem is replaced (molecules -> GraphState), the old dep fields often survive because they compile fine as unused function pointers. Grep for `Deps` fields that have no callers outside their own wiring.
3. **Audit PLAYBOOK.md and other docs.** Documentation that references the old system is a reliable signal that call sites may still exist.

## Handling this kind of chore

- **Type:** `chore`. No behavior change — the removed calls were already no-ops in practice.
- **Scope:** Follow the dep field from its struct definition -> bridge wiring -> call sites -> implementation. Remove all four layers in one pass to avoid partial cleanup.
- **Testing:** Compiler-driven. Deleting the `Deps` field causes build failures at every remaining reference, so no call site is missed. No new tests needed.
- **Review bar:** Low — verdict-only sage review is sufficient for pure dead-code removal.
