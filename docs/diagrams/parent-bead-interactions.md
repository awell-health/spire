# Parent Bead Interactions

The parent bead is the overall task lifecycle surface. It is what commands, steward policy, and the UI treat as "the work item."

```mermaid
sequenceDiagram
  autonumber
  participant Summon as summon
  participant Resummon as resummon
  participant Reset as reset family
  participant Approve as approve
  participant Exec as executor / claim
  participant Stew as steward
  participant Sweep as orphan sweep
  participant Parent as parent bead

  alt targeted local summon
    Summon->>Parent: status = in_progress via BeginWork
  else generic local summon
    Summon-->>Exec: spawn wizard
    Exec->>Parent: status = in_progress via claim
  else cluster dispatch
    Stew->>Parent: ready -> dispatched
    Exec->>Parent: dispatched -> in_progress via claim
  end

  Exec->>Parent: status = hooked when a step parks or recovery escalates
  Approve->>Parent: hooked -> in_progress when the last approval hook clears
  Stew->>Parent: hooked -> in_progress before hooked-step resummon
  Sweep->>Parent: in_progress -> open when the active attempt is orphaned

  Reset->>Parent: strip interrupted:* and needs-human
  Reset->>Parent: plain reset -> open
  Reset->>Parent: reset --hard -> open
  Reset->>Parent: reset --to -> open after rewind

  Resummon->>Parent: strip stale labels, preserve graph state, then re-enter summon
  Exec->>Parent: closed on terminal success / merge / discard flows

  Note right of Parent: board and trace read this surface directly for overall status
```

## Why The Parent Bead Exists

- It is the queueing and operator surface: `ready`, `dispatched`, `in_progress`, `hooked`, `open`, and `closed` all describe the work item as a whole.
- Commands like `summon`, `resummon`, `reset`, and `approve` act on the parent bead because they are steering the task, not just one execution lease or one formula step.
