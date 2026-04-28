# Step Bead Interactions

The step beads are the external pipeline surface for a v3 graph. They are not the routing authority, but they are what board and trace use to render step-level progress.

```mermaid
sequenceDiagram
  autonumber
  participant Exec as executor
  participant Approve as approve
  participant Stew as steward hooked sweep
  participant Reset as reset family
  participant UI as board / trace
  participant Step as step bead
  participant Parent as parent bead

  Exec->>Step: CreateStepBead or reuse existing bead
  Exec->>Step: ActivateStepBead when a graph step becomes active

  alt step succeeds
    Exec->>Step: CloseStepBead
  else step parks for approval or recovery
    Exec->>Step: HookStepBead
    Exec->>Parent: status = hooked
  else loop / rewind path
    Exec->>Step: ReopenStepBead to reopen a rewound pending step (→ open, not in_progress)
  end

  Approve->>Step: UnhookStepBead for human.approve / design-check
  Approve->>Parent: status = in_progress if no other step remains hooked

  Stew->>Step: UnhookStepBead when a hooked condition is resolved
  Stew->>Parent: status = in_progress before resummon

  Reset->>Step: unhook hooked steps before reset
  Reset->>Step: plain reset closes step beads
  Reset->>Step: reset --hard deletes step beads
  Reset->>Step: reset --to reopens only rewound steps
  Reset->>Step: reset --to --force may also close normalized active predecessors

  UI-->>Step: read GetStepBeads / GetActiveStep for phase and pipeline rendering

  Note right of Step: graph_state.json is the routing truth; step beads are the visible status surface
```

## Why The Step Beads Exist

- They expose graph progress to humans without making the UI read executor state files directly.
- They make parked work visible: a hooked step bead is how approval and recovery gates surface on the board.
- They let reset and resummon reconcile visible pipeline state with rewound graph state, which is exactly where the reset drift bug showed up.
