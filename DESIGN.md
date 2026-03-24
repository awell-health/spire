Now I have a complete picture. Let me write the plan.

---

## Implementation Plan: spi-1dl.1 — Local Steward Adaptation

### Current State

The previous commit (`4be1be31`) already landed the core structure:

- `--mode=auto|local|k8s` flag on `spire steward`
- `resolveMode()` defaulting to local outside k8s
- `loadLocalStewardConfig()` reading from `spire.yaml` via `repoconfig.Load()`
- PID file helpers (`wizardPIDPath`, `recordWizardPID`, `clearWizardPID`, `killLocalWizard`, `isWizardRunning`)
- `localRoster()` — wizard names from summon registry
- `localBusyAgents()` — PID-file liveness check
- `spawnLocalAgent()` — correctly stubbed (backends are spi-1dl.2/spi-1dl.3)
- `killWizardProcess()` dispatching local vs k8s kill

**Test failures are pre-existing**: all failures are `database not initialized: issue_prefix config is missing` — integration tests that require a live beads DB. These are not caused by the steward changes.

---

### Gap: `localBusyAgents()` Will Cause Double-Assignment

**The bug**: `localBusyAgents()` uses only PID-file liveness. With `spawnLocalAgent` returning 0 (stub), `recordWizardPID` is never called, so PID files never exist. On every cycle, all wizards appear idle → steward repeatedly tries to re-assign the same in-progress bead.

**Fix**: Make `localBusyAgents()` a hybrid — check PID files **and** bead ownership labels (the same `owner:<agent>` + in_progress check that `findBusyAgents()` uses in k8s mode). A wizard is busy if either: (a) it has a live PID file, or (b) it owns an in-progress bead.

This is the critical correctness fix. The assignment guard within a single cycle (`busy[agent] = true`) prevents same-cycle double-assign, but cross-cycle protection requires bead ownership.

---

### Files to Change

**`cmd/spire/steward_local.go`** — primary file

1. **Fix `localBusyAgents()`**: hybrid check (PID file OR bead ownership):
   - Call `storeListBeads(IssueFilter{Status: in_progress})` and collect agents with `owner:<name>` labels
   - Union with PID-alive wizards from registry
   - A wizard is busy if either signal is present

**`cmd/spire/steward.go`** — no changes needed; the structure is correct.

---

### Key Design Decisions

1. **PID files are the runtime signal; bead ownership is the persistent signal.** PID files disappear when a process exits (or when the stub is used). Bead ownership labels survive process crashes and give the steward cross-cycle memory. Both must be checked.

2. **`localRoster()` includes dead-process wizards intentionally.** An idle slot (no live PID, no owned bead) is a valid assignment target — the steward will assign to it and call `spawnLocalAgent`. When spi-1dl.2/spi-1dl.3 land actual spawning, the PID gets written and the wizard goes busy.

3. **`spawnLocalAgent()` stub is correct for this task.** The integration point (call after assignment) is wired. The actual execution backends are spi-1dl.2 (Docker) and spi-1dl.3 (process). No additional stub work is needed here.

4. **Tower config fallback is out of scope.** No tower config format is defined. `loadLocalStewardConfig()` correctly reads from `spire.yaml` and applies defaults. Extend later when tower config format is specified.

5. **`resolveMode()` k8s detection via `/var/run/secrets/kubernetes.io/serviceaccount/token` is standard** and the right approach.

---

### Edge Cases / Risks

| Risk | Mitigation |
|------|-----------|
| Wizard crashes after assignment, PID file gone, bead still in_progress | Bead-ownership check in `localBusyAgents()` prevents re-assignment |
| Stale PID file (process died, file not cleaned up) | `processAlive()` uses `kill -0` which handles this correctly |
| Wizard assigned bead but spawn stub returns 0 — no PID file written | Bead ownership check covers this; no double-assign |
| `loadWizardRegistry()` returns empty if no wizards summoned | Correct behavior — steward logs "0 agents" and skips |
| `repoconfig.Load()` fails (no spire.yaml) | Already handled — returns defaults silently |

---

### Order of Changes

1. **Fix `localBusyAgents()`** in `steward_local.go` — add bead-ownership check alongside PID check. This is the only true gap preventing correct multi-cycle behavior.

2. **Verify test failures are pre-existing** — confirm they also fail on `main` (environment issue, not regression). No test changes needed for this task.

3. **No other files need changes.** The mode flag, config loading, PID infrastructure, cycle integration, and kill dispatch are all implemented correctly.

---

### Non-Goals for spi-1dl.1

- Actual agent spawning (spi-1dl.2, spi-1dl.3)
- `spire up` integration with steward lifecycle (spi-1dl.5)
- Tower config format/reading
- Docker or process execution backends
