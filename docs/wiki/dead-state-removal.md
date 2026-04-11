# Dead State Removal in Board Modes

## What happened (spi-g3ngn)

`AgentsMode` in `pkg/board/agents_mode.go` carried two struct fields that were never meaningfully used:

- **`registryPath`** — never set or read. `fetchAgents` calls `agent.LoadRegistry` directly with its own path resolution; the field was vestigial.
- **`active`** — written in `OnActivate`/`OnDeactivate` but never read by any rendering or logic path. The TUI framework already tracks which mode is active externally.

Both fields were removed. `OnActivate` was simplified to just re-fetch + restart tick, and `OnDeactivate` became a no-op (still required by the `Mode` interface).

## Why it matters

Dead fields create confusion for future readers — they imply invariants that don't exist and invite code that depends on stale state. Removing them early keeps the struct honest.

## How to spot similar issues

1. **Grep for writes with no reads.** If a struct field is only ever assigned (including in constructors) but never appears in a conditional, return value, or method call argument, it's dead.
2. **Check interface contracts.** A method like `OnDeactivate` may need to exist for an interface but doesn't need to do anything. An empty body with a comment is cleaner than fake bookkeeping.
3. **Constructor residue.** Fields that were set during a refactor's intermediate state but never wired into the final design are common — check struct literals in `New*` functions.

## Handling this kind of chore

- **Type:** `chore`, not `refactor`. No behavior changes, just dead-code removal.
- **Testing:** None needed if the removal is purely mechanical (field delete + simplify methods that only touched that field). The compiler catches any remaining references.
- **Review bar:** Low — a sage verdict-only review is sufficient for pure dead-state removal.
