# Bead Surface Overview

This map shows which commands and runtime actors touch the parent bead, the attempt bead, and the step beads. The surface is broken into three diagrams so each category stays readable on its own.

## 1. Operator Commands

Commands the user runs explicitly. Solid arrows are direct writes; dotted arrows are indirect side effects.

```mermaid
flowchart LR
  summon["summon"]
  resummon["resummon"]
  reset["reset"]
  resetHard["reset --hard"]
  resetTo["reset --to"]
  approve["approve"]

  parent["parent work bead"]
  attempt["attempt bead"]
  step["step beads"]

  summon -->|targeted local path: BeginWork marks in_progress| parent
  summon -->|targeted local path: pre-create execution lease| attempt

  resummon -->|strip stale labels; preserve graph state; then call summon| parent
  resummon -.->|old attempt is usually reaped on the next orphan sweep| attempt

  reset -->|clear stuck labels; reopen real subtask children; set open| parent
  reset -->|close internal attempt children| attempt
  reset -->|close step children| step

  resetHard -->|clear stuck labels; set open; remove worktree state| parent
  resetHard -->|close internal attempt children| attempt
  resetHard -->|delete step children| step

  resetTo -->|rewind graph, then set parent open| parent
  resetTo -.->|does not directly close the old attempt| attempt
  resetTo -->|reopen only rewound steps| step

  approve -->|resume parent when the last approval hook clears| parent
  approve -->|unhook human.approve or design-check gate| step
```

## 2. Runtime Actors

Background processes that mutate beads as work progresses.

```mermaid
flowchart LR
  exec["executor / wizard"]
  steward["steward"]
  orphan["orphan sweep"]

  parent["parent work bead"]
  attempt["attempt bead"]
  step["step beads"]

  exec -->|status moves, labels, terminal close| parent
  exec -->|ownership stamp, heartbeat, result close| attempt
  exec -->|create, activate, hook, unhook, close| step

  steward -->|cluster dispatch, hooked resume, stale dispatched recovery| parent
  steward -->|read owner + heartbeat; kill stale owners| attempt
  steward -->|check and sometimes unhook resolved gates| step

  orphan -->|reopen parent when owner is dead and heartbeat is stale| parent
  orphan -->|close orphaned attempt| attempt
```

## 3. Read-Only Surfaces

UI surfaces that only read bead state — they never mutate it.

```mermaid
flowchart LR
  board["board"]
  trace["trace"]

  parent["parent work bead"]
  attempt["attempt bead"]
  step["step beads"]

  board -.->|queue state and current phase| parent
  board -.->|active owner summary| attempt
  board -.->|pipeline rendering| step

  trace -.->|overall execution timeline| parent
  trace -.->|active attempt metadata| attempt
  trace -.->|per-step timeline| step
```

## Why These Surfaces Exist

- The parent bead is the user-facing work item: queue state, ownership visibility, board actions, and terminal completion all land here.
- The attempt bead is the execution lease: instance ownership, heartbeat, and attempt result belong here, not on the parent bead.
- The step beads are the external pipeline surface: they make graph progress visible to board and trace without making the UI parse graph state files.
