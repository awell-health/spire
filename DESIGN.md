## Implementation Plan: spi-1dl.1 — Local Steward Adaptation

### Current State (as of commit 6bb0c1b2)

The implementation is **complete**. Both commits have landed everything required:

**`cmd/spire/steward_local.go`**
- `StewardMode` type + constants (`auto`, `local`, `k8s`)
- `isInK8s()` — detects k8s via service account token path
- `resolveMode()` — auto-selects local outside k8s
- `loadLocalStewardConfig()` — reads `spire.yaml` via `repoconfig.Load()`, applies defaults
- PID file helpers: `wizardPIDPath`, `isWizardRunning`, `recordWizardPID`, `clearWizardPID`, `killLocalWizard`
- `localRoster()` — wizard names from summon registry (includes dead processes as open slots)
- `localBusyAgents()` — **hybrid check**: PID-file liveness AND bead ownership labels
- `spawnLocalAgent()` — correctly stubbed with TODO markers for spi-1dl.2/spi-1dl.3

**`cmd/spire/steward.go`**
- `--mode=auto|local|k8s` flag parsed in `cmdSteward`
- `stewardCycle` branches on mode: calls `localBusyAgents()` vs `findBusyAgents()`
- `loadLocalStewardConfig()` called once per cycle when in local mode
- `spawnLocalAgent()` called after successful assignment, PID recorded if > 0
- `killWizardProcess()` dispatches to `killLocalWizard` or `killWizardPod`

### Key Design Decisions (already in code)

1. **Hybrid busy detection** — `localBusyAgents()` checks both PID files (runtime signal) and `owner:<name>` labels on in-progress beads (persistent signal). This prevents double-assignment across cycles even when the spawn stub returns PID=0.

2. **Stub is correct** — `spawnLocalAgent()` returns `(0, nil)`. The steward only calls `recordWizardPID` when `pid > 0`, so no stale PID files are written. Bead ownership provides the busy signal until real backends land.

3. **`localRoster()` includes dead wizards** — an idle slot (no live PID, no owned bead) is a valid assignment target. When backends land, the PID file makes them busy.

4. **No changes to test files** — test failures are pre-existing: `database not initialized: issue_prefix config is missing`. These are integration tests requiring a live beads DB, unaffected by steward changes.

### What Remains

**Nothing to implement.** The task is functionally complete:

| Requirement | Status |
|---|---|
| `--mode=local` flag | Done |
| Default to local outside k8s | Done |
| Read config from `spire.yaml` | Done |
| Track agents via PID files | Done |
| Spawn after assignment | Done (stub, pending spi-1dl.2/3) |

### Non-Goals (confirmed out of scope)

- Actual agent spawning backends → spi-1dl.2 (Docker), spi-1dl.3 (process)
- `spire up` steward lifecycle integration → spi-1dl.5
- Tower config format/reading (undefined spec)

### Conclusion

The work for spi-1dl.1 is done. The branch is ready for review. Test failures are environment-level, not regressions from this change.
