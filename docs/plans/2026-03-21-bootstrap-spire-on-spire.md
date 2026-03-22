# Plan: Bootstrap Spire-on-Spire

**Date**: 2026-03-21
**Goal**: Get spire working well enough to use it to track work on spire itself.
**Prerequisite**: Architecture review at `docs/reviews/2026-03-21-k8s-architecture.md`

---

## Issues Found

### Issue 1: Zero logging on bd commands (blind debugging)

`cmd/spire/bd.go` is the gateway to all bead operations. It's 41 lines with no logging. When a bd command fails, the error propagates up but nothing records what was attempted, how long it took, or what the server returned.

The same bd helper is duplicated in three places with zero logging:
- `cmd/spire/bd.go` — main CLI (steward, claim, focus, file, etc.)
- `cmd/spire-artificer/comms.go:178` — artificer
- `cmd/spire-steward-sidecar/tools.go:474` — steward sidecar (as `runBD`)

**Impact**: Cannot diagnose any failure. The project_id mismatch, silent bd failures during steward cycles, sidecar tool execution problems — all invisible.

### Issue 2: Project ID mismatch (showstopper for k8s pods)

The `beads-seed` ConfigMap (`k8s/beads-seed.yaml:17`) hardcodes:
```json
"project_id": "3c8faa08-e7ef-4bc3-bd63-1b0ba55d9524"
```

When the dolt PVC is recreated (`make clean` then `make deploy`), the init script clones from DoltHub and gets a new project_id. Every pod starts with the stale ConfigMap value. Every `bd` command fails with `PROJECT IDENTITY MISMATCH`.

Two components have reactive realignment (`realignProjectID()`):
- `cmd/spire/steward.go:537` — triggered when `bd ready` fails
- `cmd/spire-steward-sidecar/main.go:389` — triggered when `spire collect` fails

But:
- It's reactive (only on error), not proactive (at startup)
- Wizard and artificer pods have no realignment at all
- The realignment function is duplicated, not shared
- Neither logs what was wrong or what it changed

### Issue 3: Documentation says push/pull but code doesn't

| Location | Claims | Reality |
|----------|--------|---------|
| `CLAUDE.md` (this repo) | claim = "pull → verify → set in_progress → push" | No pull, no push. Just verify + update. |
| `SPIRE.md:10` | Same | Same |
| `.claude/skills/spire-work/SKILL.md:42` | "pull → verify → claim → push" | Local-only |
| `.claude/skills/spire-work/SKILL.md:133` | `bd dolt push` after merge | `pushState()` is a no-op |
| `.claude/skills/spire-work/SKILL.md:151` | "Push dolt state after merge" | Not needed with shared server |

The code is correct — the docs are stale. But agents follow the docs, causing wasted time or errors.

### Issue 4: No end-to-end validation

Nobody has filed work as beads and worked through the full molecule lifecycle (focus → design → implement → review → merge) on this repo. The system hasn't been dogfooded.

---

## Principles

1. **Local dolt server is source of truth.** No component should push/pull to DoltHub in its normal operation. The syncer is the only thing that talks to DoltHub, and it's being removed.
2. **Log what you do.** Every bd command should be visible in logs with its arguments, duration, and outcome.
3. **Fail loud, not silent.** When something goes wrong (project_id mismatch, bd error), it should be immediately visible — not swallowed.
4. **Proactive, not reactive.** Fix state at startup, don't wait for errors to trigger realignment.

---

## Tasks

### Task 1: Add bd command logging

**Priority**: P1 — foundation for diagnosing everything else
**Type**: chore
**Files**:
- `cmd/spire/bd.go`
- `cmd/spire-artificer/comms.go`
- `cmd/spire-steward-sidecar/tools.go`
- `k8s/steward.yaml` (env var)
- `operator/controllers/agent_monitor.go` (env var on dynamic pods)

**Design**:

Gate logging behind `SPIRE_BD_LOG` environment variable. Background services (steward, operator, wizards, artificers) always set it. Interactive CLI commands stay quiet unless user opts in.

Changes to `cmd/spire/bd.go`:

```go
import (
    "log"
    "os"
    "time"
)

var bdVerbose = os.Getenv("SPIRE_BD_LOG") != ""

func bd(args ...string) (string, error) {
    start := time.Now()
    if bdVerbose {
        log.Printf("[bd] exec: bd %s", strings.Join(args, " "))
    }
    cmd := exec.Command("bd", args...)
    var stdout, stderr bytes.Buffer
    cmd.Stdout = &stdout
    cmd.Stderr = &stderr
    err := cmd.Run()
    elapsed := time.Since(start)
    if err != nil {
        errStr := strings.TrimSpace(stderr.String())
        if bdVerbose {
            log.Printf("[bd] FAIL (%.1fs): bd %s — %s", elapsed.Seconds(),
                strings.Join(args, " "), errStr)
        }
        return "", fmt.Errorf("bd %s: %s\n%s", strings.Join(args, " "), err, errStr)
    }
    result := strings.TrimSpace(stdout.String())
    if bdVerbose {
        log.Printf("[bd] OK (%.1fs): bd %s — %d bytes", elapsed.Seconds(),
            strings.Join(args, " "), len(result))
    }
    return result, nil
}
```

Same pattern for `cmd/spire-artificer/comms.go` bd() and `cmd/spire-steward-sidecar/tools.go` runBD().

Add `SPIRE_BD_LOG: "1"` to:
- `k8s/steward.yaml` — steward container env
- `k8s/steward.yaml` — sidecar container env
- `operator/controllers/agent_monitor.go` — wizardEnv, artificerEnv, sidecarEnv in all pod builders

**Verification**: Deploy to minikube, `make logs` should show `[bd] exec:` and `[bd] OK` lines for every steward cycle.

---

### Task 2: Proactive project_id alignment

**Priority**: P1 — blocks k8s pods from working
**Type**: fix
**Depends on**: Task 1 (need logging to verify the fix)
**Files**:
- `cmd/spire/steward.go`
- `cmd/spire-steward-sidecar/main.go`
- `cmd/spire/bd.go` (new shared helper)
- `k8s/beads-seed.yaml`

**Design**:

Add a shared `ensureProjectID()` function that:
1. Reads `.beads/metadata.json` project_id
2. Queries dolt server for `_project_id`
3. Logs both values at INFO level
4. If mismatch: updates metadata.json, logs the change
5. If server unreachable: logs warning, continues with local value (may fail later, but at least we'll see why)

```go
func ensureProjectID() {
    metaPath := ".beads/metadata.json"
    data, err := os.ReadFile(metaPath)
    if err != nil {
        log.Printf("[project-id] cannot read %s: %s", metaPath, err)
        return
    }
    var meta map[string]any
    if err := json.Unmarshal(data, &meta); err != nil {
        log.Printf("[project-id] cannot parse %s: %s", metaPath, err)
        return
    }
    localPID, _ := meta["project_id"].(string)
    log.Printf("[project-id] local: %s", localPID)

    host, port := doltHost(), doltPort()
    out, err := exec.Command("dolt", "--host", host, "--port", port,
        "--user", "root", "-p", "", "--no-tls", "sql", "-q",
        "USE spi; SELECT value FROM metadata WHERE `key`='_project_id'",
        "-r", "csv").Output()
    if err != nil {
        log.Printf("[project-id] cannot query server at %s:%s: %s", host, port, err)
        return
    }
    lines := strings.Split(strings.TrimSpace(string(out)), "\n")
    if len(lines) < 2 {
        log.Printf("[project-id] unexpected server response: %s", string(out))
        return
    }
    serverPID := strings.TrimSpace(lines[len(lines)-1])
    log.Printf("[project-id] server: %s", serverPID)

    if localPID == serverPID {
        log.Printf("[project-id] aligned")
        return
    }

    log.Printf("[project-id] MISMATCH — updating local %s → %s", localPID, serverPID)
    meta["project_id"] = serverPID
    updated, _ := json.MarshalIndent(meta, "", "  ")
    if err := os.WriteFile(metaPath, updated, 0644); err != nil {
        log.Printf("[project-id] cannot write %s: %s", metaPath, err)
        return
    }
    log.Printf("[project-id] realigned successfully")
}
```

Call at startup (before first bd command) in:
- `cmdSteward()` — before the first `stewardCycle()`
- steward-sidecar `main()` — before the first `spire collect`

Remove the reactive `realignProjectID()` error-handling blocks in:
- `steward.go:165-170` (the `PROJECT IDENTITY MISMATCH` catch)
- `steward-sidecar/main.go:244-253` (same)

The duplicate `realignProjectID()` functions in both files are replaced by the shared `ensureProjectID()`.

Update `k8s/beads-seed.yaml` — remove the hardcoded project_id:

```yaml
metadata.json: |
  {
    "database": "dolt",
    "backend": "dolt",
    "dolt_mode": "server",
    "dolt_database": "spi"
  }
```

The startup code will populate it from the server.

**Verification**: `make clean && make deploy`. Watch logs. Should see:
```
[project-id] local:
[project-id] server: <new-uuid>
[project-id] MISMATCH — updating local  → <new-uuid>
[project-id] realigned successfully
[bd] OK (0.1s): bd ready — 42 bytes
```

---

### Task 3: Fix documentation to match local-is-truth

**Priority**: P2 — prevents agents from running stale push/pull commands
**Type**: docs
**Independent of**: Tasks 1 and 2

**Files and changes**:

`CLAUDE.md` (this repo):
- Change claim description from "atomic: pull → verify → set in_progress → push" to "verify not closed/owned → set in_progress"
- Remove any mention of `bd dolt push` from the "Completing work" section

`SPIRE.md`:
- Line 10: Change `spire claim <bead-id>` comment from "atomic: pull → verify → set in_progress → push" to "claim a task (verify → set in_progress)"

`.claude/skills/spire-work/SKILL.md`:
- Line 36-42: Remove "pull → " and " → push" from Step 0 description
- Line 42: Change explanation from "does: bd dolt pull -> verify ... -> bd dolt push" to "does: verify bead exists and isn't closed/owned -> bd update --claim --status in_progress"
- Lines 128-133 (Merge step): Remove `bd dolt push` line
- Line 151 (Rules): Remove "Push dolt state after merge: `bd dolt push`"

`../CLAUDE.md` (parent awell repo, if it has the same claim description):
- Same fix to claim description

**Verification**: Read the updated docs. No mention of push/pull in claim or merge steps.

---

### Task 4: Audit stale push/pull in non-syncer code

**Priority**: P2 — correctness
**Type**: chore
**Independent of**: Tasks 1 and 2

**Audit results** (already done in the architecture review):

| Location | Code | Verdict |
|----------|------|---------|
| `steward.go:159` | `bd("dolt", "commit", "steward cycle sync")` | **Keep**. This is a local dolt commit (working set → committed state), not a remote push. |
| `steward.go:532` | `func pushState() {}` | **Keep**. Already a no-op. Add comment clarifying it's intentionally empty. |
| `sync.go` | All push/pull operations | **Leave alone**. This is the explicit sync command for the syncer pod. |
| `push.go` | Explicit push to DoltHub | **Leave alone**. This is the explicit push command. |
| `SKILL.md:133` | `bd dolt push` | **Remove**. Covered by Task 3. |

**Conclusion**: No code changes needed beyond docs (Task 3). The Go code is already clean — `pushState()` is a no-op, claim doesn't push, the steward doesn't push. Only the documentation is stale.

Add a comment to `pushState()` to make intent explicit:

```go
// pushState is intentionally a no-op. The shared dolt server is the source
// of truth — there is no remote to push to. DoltHub backup, if desired,
// is handled by the syncer pod, not the steward cycle.
func pushState() {}
```

---

### Task 5: Dogfood — use spire to track this work

**Priority**: P1 — the whole point
**Type**: chore
**Depends on**: nothing (can start immediately, validates the workflow)

**Steps**:

```bash
cd ~/awell/spire
spire up                                              # ensure dolt + daemon running

# File the tasks from this plan as beads
spire file "Add bd command logging" -t chore -p 1
spire file "Proactive project_id alignment" -t fix -p 1
spire file "Fix docs: remove push/pull from claim" -t docs -p 2
spire file "Audit stale push/pull in code" -t chore -p 2

# Work through them
spire claim <task-1-id>
spire focus <task-1-id>
# implement, close molecule steps, close bead
# commit: chore(<task-1-id>): add bd command logging
```

**Success criteria**: All four tasks filed as beads, worked through the molecule lifecycle, committed with bead references, and closed.

---

## Execution Order

```
Task 1 (logging)  ───→  Task 2 (project_id)  ───→  verify in k8s
                                                 ↗
Task 3 (docs)     ──────────────────────────────
Task 4 (audit)    ──────────────────────────────

Task 5 (dogfood)  — runs in parallel with all of the above, validates the workflow
```

Tasks 1 and 2 are sequential: logging first, then use it to validate the project_id fix.
Tasks 3 and 4 are independent, can run alongside anything.
Task 5 starts immediately — filing beads for the other tasks IS the dogfooding.

## Out of Scope

- DoltHub sync removal (user said to ignore syncer for now)
- K8s automated agent work (wizards, artificers) — get the human workflow working first
- Linear integration
- Multi-repo coordination
