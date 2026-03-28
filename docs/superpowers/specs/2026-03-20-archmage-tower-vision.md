# The Archmage's Tower — Spire Command & Visibility

**Date**: 2026-03-20
**Status**: Design
**Builds on**: spire-product-vision.md, spire-autonomous-agents-v2.md, spire-artificer-design.md

> **Note (2026-03-28):** This design predates the end-state naming convention.
> References to "artificer" below refer to what is now the **wizard** (for
> orchestration) and **sage** (for review). The "sidecar" is now called the
> **familiar**. See docs/ARCHITECTURE.md for current naming.

## The experience

The archmage files an epic and summons capacity. The tower handles the rest: the steward dispatches, wizards implement, the artificer reviews and merges. The archmage comes back whenever they want and sees exactly where things stand. When something needs their attention, the tower tells them — they don't have to ask.

Three surfaces make this work: the scrying pool, the crystal ball, and the messenger.

## 1. The Scrying Pool — `spire board`

At-a-glance state of the tower. Columnar. Kanban-style but in the terminal.

```
READY (4)           WORKING (2)                REVIEW (1)            MERGED (3)
──────────          ──────────                 ──────────            ──────────
P1 spi-7v2.4       P2 spi-7v2.1               P2 spi-7v2.2         P2 spi-7v2.3
  CR-to-beads         PV/PVC setup               shared mount          write-back
  sync                 wizard-1 · 12m             artificer             2m ago
                       ██████░░░░ 60%              PR #42 reviewing
P2 spi-7v2.3
  write-back        P1 spi-4y1
  pattern              agent_runs table
                       wizard-2 · 3m
                       ████░░░░░░ 40%
```

Columns are lifecycle stages, not status buckets. A bead moves left to right: Ready → Working → Review → Merged. Blocked beads appear in a separate row below. The archmage sees **flow**, not a list.

### Data sources

- **Ready column**: `bd ready --json` — beads with no blocking deps, no owner
- **Working column**: beads with `status=in_progress` + `owner:<wizard>` label. Elapsed time from `updated_at`. Progress from familiar `/status` endpoint if available.
- **Review column**: beads where the wizard has pushed a branch and the artificer is reviewing. Detected by: bead in_progress + branch exists + artificer state shows "reviewing".
- **Merged column**: recently closed beads (last 24h) with a merge commit. Shows PR number and time-since-merge.

### Flags

```bash
spire board                  # full board
spire board --epic spi-7v2   # scoped to one epic and its children
spire board --mine           # only beads I own or filed
spire board --ready          # just the ready column
spire board --json           # machine-readable
```

## 2. The Crystal Ball — `spire watch`

Live, streaming view. Auto-refreshes every few seconds. This is "I want to watch my team work."

### Epic watch

```bash
spire watch spi-7v2
```

```
EPIC: spi-7v2 — Shared persistent dolt volume (2/5 done)
Updated: 3s ago

  ✓ spi-7v2.3  write-back pattern          PR #44  merged     2m ago
  ✓ spi-7v2.1  PV/PVC setup               PR #42  merged     5m ago
  ⏳ spi-7v2.2  shared mount read-only      PR #43  in review  artificer reviewing...
  ◐ spi-7v2.4  CR-to-beads sync           wizard-1  8m elapsed
  ○ spi-7v2.5  remove dolt from wizard    blocked by .2, .4

--- wizard-1 (spi-7v2.4) live ---
  Step: implement (2/4)
  Files changed: 3 (agent_monitor.go, bead_watcher.go, types.go)
  Last activity: 12s ago
  Tests: not yet run
```

### Single bead watch

```bash
spire watch spi-7v2.4
```

Tails the wizard's familiar `/status` endpoint. Shows: current molecule step, files touched, test results, elapsed time, last activity.

### Tower watch

```bash
spire watch
```

All active work across all epics. Compact format. Shows the steward cycle count, wizard count, total throughput.

```
TOWER STATUS — 3 wizards, 1 artificer, 12 beads active
Steward cycle: #47 (2m interval, last: 14s ago)

  wizard-1  ◐ spi-7v2.4  CR-to-beads sync       8m   implementing
  wizard-2  ◐ spi-4y1    agent_runs table        3m   running tests
  wizard-3  ◐ spi-7v2.3  write-back pattern     14m   pushing branch
  artificer ⏳ spi-7v2.2  shared mount           reviewing PR #43
```

### Implementation

- **k8s**: The steward pod runs a collector goroutine that scrapes every familiar's `/status` endpoint (discovered via pod label `spire.awell.io/managed=true`). Aggregates into an in-memory state or a local JSON file. `spire watch` connects to the steward's status endpoint.
- **Local**: `spire watch` reads bead state directly from dolt + familiar comms files on disk. Polls on a 2s interval, redraws the terminal.

## 3. The Messenger — interrupts that come to you

Not a command. These are events that the tower surfaces proactively. The archmage doesn't have to poll — the tower raises its hand.

### Events

| Event | Source | Priority | When |
|-------|--------|----------|------|
| Escalation | Artificer | P0 | Review failed after N rounds — needs human eyes |
| Epic complete | Artificer | P1 | All children merged — the work is done |
| Wizard failure | Familiar | P1 | Pod crashed, timeout, or unrecoverable error |
| Stale wizard | Steward | P1 | Working longer than `staleThreshold` |
| PR ready | Artificer | P2 | Approved by artificer, waiting for human merge |
| Budget alert | Steward | P1 | Daily API cost exceeds configured threshold |

### Delivery channels

**Phase 1 — spire inbox**:
Events are beads. The steward/artificer creates a message bead with `label:alert` and `ref:<bead-id>`. The archmage sees them via `spire collect` or at the top of `spire board`.

```
ALERTS (2)
  ⚠ P0  spi-7v2.2 needs your review — artificer escalated after 3 rounds
  ✓ P1  spi-7v2 epic complete — 5/5 merged
```

**Phase 2 — Slack**:
Events are also posted to a Slack channel via `spire send --slack`. The archmage gets a DM for P0/P1 events.

**Phase 3 — terminal notifications**:
If `spire watch` is running, alerts appear inline immediately.

## 4. `spire summon` — conjure capacity

The archmage summons capacity, not individuals. Wizards are fungible.

```bash
spire summon 3                    # summon 3 wizards to the tower
spire summon --for spi-7v2        # summon enough for this epic's ready children
spire dismiss 1                   # send one wizard home (least busy first)
spire dismiss --all               # dismiss all wizards
spire roster                      # who's in the tower right now
```

### `spire roster` output

```
TOWER ROSTER — 3 wizards, 1 artificer

  wizard-1   ◐ working    spi-7v2.4  CR-to-beads sync       8m
  wizard-2   ◐ working    spi-4y1    agent_runs table        3m
  wizard-3   ○ idle       —                                  —
  artificer  ⏳ reviewing  spi-7v2    Shared persistent dolt  watching 5 branches

Capacity: 2/3 wizards busy (1 idle)
Steward cycle: #47 (next in 1m 46s)
```

### Implementation

**k8s mode**:
- `spire summon 3` creates 3 SpireAgent CRs with `mode: managed`, auto-generated names (`wizard-1`, `wizard-2`, `wizard-3`), and the current repo's config.
- The operator's AgentMonitor sees the new CRs and creates pods when work is assigned.
- `spire dismiss 1` deletes the least-busy SpireAgent CR. The operator cleans up the pod.

**Local mode**:
- `spire summon 3` spawns 3 background processes, each in its own git worktree, each running `spire-work` in a loop: poll ready → claim → focus → execute → push → repeat.
- `spire dismiss 1` sends SIGTERM to the least-busy process.
- State tracked in `~/.config/spire/wizards.json`.

## 5. `spire roster` — who's in the tower

Shows registered agents, their status, current work, elapsed time.

### Data sources

**k8s**: Query SpireAgent CRs + their pods. For each managed agent, hit the familiar's `/status` endpoint.

**Local**: Read `~/.config/spire/wizards.json` + check process liveness + read `/comms/` state.

## 6. The steward cycle — defined and transparent

The steward is the heartbeat of the tower. Every cycle follows this exact sequence:

```
1. COMMIT   — flush local dolt changes
2. PULL     — sync from DoltHub (get beads filed from other machines)
3. ASSESS   — bd ready to find unblocked, unassigned work
4. ASSIGN   — match ready beads to idle wizards, send messages
5. STALE    — check for wizards working too long, send reminders
6. PUSH     — sync to DoltHub
```

The cycle interval is configurable (default: 2 minutes). Each cycle logs a structured summary:

```
[steward] ═══ cycle 47 ═══════════════════════════════
[steward] pull: synced (3 new beads from DoltHub)
[steward] ready: 4 beads | roster: 3 wizards (2 busy, 1 idle)
[steward] assigned: spi-7v2.4 → wizard-1 (P1)
[steward] assigned: spi-7v2.3 → wizard-2 (P2)
[steward] stale: none
[steward] push: synced to DoltHub
[steward] ═══ cycle 47 complete (2.1s) ════════════════
```

Between cycles, the steward is idle. Work happens asynchronously — wizards work, familiars report, the artificer reviews. The steward only wakes up to dispatch and check.

## Information flow — when data is collected

Every piece of information the archmage sees has a source and a moment it becomes available:

```
Phase        │ Source       │ Data collected              │ Available via
─────────────┼──────────────┼─────────────────────────────┼──────────────────
Bead filed   │ archmage     │ title, priority, type, deps │ board (ready column)
Summoned     │ spire summon │ wizard registered, familiar  │ roster
             │              │ starts, /status live         │
Assigned     │ steward      │ owner label set, bead →      │ board (working column)
             │              │ in_progress                  │
Working      │ familiar     │ heartbeat (5s), /status:     │ watch (live), roster
             │              │ molecule step, files changed,│
             │              │ test status, last activity   │
Pushed       │ wizard       │ result.json written, branch  │ board (review column)
             │              │ exists on remote             │
Reviewing    │ artificer    │ verdict, issues, PR created  │ watch (epic), board
             │              │ metric recorded              │
Changes req  │ artificer    │ review sent to wizard via    │ watch (shows round #)
             │              │ spire send                   │
Merged       │ artificer    │ PR merged, bead closed,      │ board (merged column)
             │              │ metric recorded              │
Epic done    │ artificer    │ all children closed, epic    │ messenger (alert)
             │              │ closed, message sent         │
Escalation   │ artificer    │ max rounds exceeded,         │ messenger (P0 alert)
             │              │ message to steward           │
Stale        │ steward      │ in_progress > threshold      │ messenger (P1 alert)
Failure      │ familiar     │ result.json with error,      │ messenger (P1 alert)
             │              │ pod phase=Failed             │
```

### The familiar is the real-time sensor

The familiar already runs 4 loops: inbox polling (10s), control channel (2s), wizard monitoring (5s), heartbeat (30s). It already serves `/healthz`, `/readyz`, `/status`.

**What needs to change**: The `/status` endpoint needs to be richer. Today it returns sidecar state (phase, message count, wizard alive). It should also return:

```json
{
  "phase": "polling",
  "wizardAlive": true,
  "agentName": "wizard-1",
  "startedAt": "2026-03-20T15:00:00Z",

  "work": {
    "beadId": "spi-7v2.4",
    "title": "Steward as single dolt writer",
    "branch": "feat/spi-7v2.4",
    "moleculeStep": "implement",
    "moleculeProgress": "2/4",
    "startedAt": "2026-03-20T15:02:00Z",
    "elapsed": "8m14s"
  },

  "git": {
    "filesChanged": 3,
    "linesAdded": 120,
    "linesRemoved": 15,
    "lastCommit": "abc1234",
    "lastCommitAt": "2026-03-20T15:08:12Z"
  },

  "tests": {
    "passed": true,
    "total": 12,
    "failed": 0,
    "lastRun": "2026-03-20T15:09:30Z"
  }
}
```

The familiar gets this from `/comms/` files that the wizard writes during execution. The wizard already writes `bead.json`, `focus.txt`, `result.json`. We add a `progress.json` that the wizard updates as it works (or the familiar scrapes from git status in /workspace).

### The collector aggregates familiar data

Runs in the steward pod. Every 10 seconds:

1. List pods with label `spire.awell.io/managed=true`
2. For each pod, GET `<pod-ip>:8080/status`
3. Aggregate into `tower-state.json`
4. Serve aggregated state on steward's own `/tower` endpoint

`spire watch` and `spire board` read from this endpoint (k8s) or from local state (non-k8s).

## Implementation plan

### Phase 1 — The Board (columnar `spire board`)

**Files**: `cmd/spire/board.go` (rewrite display logic)

Rewrite `printBoard` to render columns side-by-side. Use terminal width detection. Each column: header, separator, cards. Cards show: bead ID, priority badge, title (truncated), owner/wizard name, elapsed time.

Keep the existing data fetching (bd list --json). Add a "Merged" column for recently closed beads (bd list --status=closed, filter to last 24h).

### Phase 2 — The Roster (`spire roster`)

**Files**: `cmd/spire/roster.go` (new)

Query registered agents (beads with label "agent"). For each, show name, status (idle/working/offline), current bead, elapsed time. In k8s mode, also query SpireAgent CRs for richer data.

### Phase 3 — Summon & Dismiss

**Files**: `cmd/spire/summon.go`, `cmd/spire/dismiss.go` (new)

`spire summon N`:
- k8s: create N SpireAgent CRs via kubectl
- Local: spawn N background processes with worktrees

`spire dismiss N`:
- k8s: delete N SpireAgent CRs (least-busy first)
- Local: send SIGTERM to N processes

### Phase 4 — Familiar enrichment

**Files**: `cmd/spire-sidecar/main.go` (enrich /status), `agent-entrypoint.sh` (write progress.json)

Add `work`, `git`, `tests` sections to the /status response. The familiar reads these from /comms files that the wizard writes during execution.

### Phase 5 — Watch

**Files**: `cmd/spire/watch.go` (new)

Live-updating terminal view. Polls bead state + familiar /status on a 2-5s interval. Clears and redraws. Shows epic progress, wizard activity, merge queue status.

### Phase 6 — Messenger (alerts in board)

**Files**: `cmd/spire/board.go` (add alerts section), steward/artificer changes to create alert beads

Add an ALERTS row at the top of the board. Query beads with label "alert" + status "open". Show priority badge + message. Steward and artificer create alert beads when events occur.

### Phase 7 — Steward cycle logging

**Files**: `cmd/spire/steward.go` (structured cycle output)

Rewrite the cycle logging to use the structured format with separator lines and summary stats.
