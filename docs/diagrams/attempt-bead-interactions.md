# Attempt Bead Interactions

The attempt bead is the execution lease. If you are asking "who owns this run?" or "is the wizard still alive?", this is the surface that matters.

```mermaid
sequenceDiagram
  autonumber
  participant Summon as summon
  participant Claim as claim / wizard startup
  participant Exec as executor
  participant Stew as steward
  participant Sweep as orphan sweep
  participant Reset as reset to / resummon
  participant ResetFull as reset / reset hard
  participant Attempt as attempt bead

  alt targeted local summon
    Summon->>Attempt: CreateAttemptBead via BeginWork
  else generic local summon or direct executor start
    Claim->>Attempt: CreateAttemptBeadAtomic or reclaim existing attempt
  end

  Claim->>Attempt: StampAttemptInstance(instance_id, session_id, started_at, last_seen_at)
  Exec->>Attempt: restamp instance ownership on reclaim / direct graph start

  loop every ~30s while running or blocked on child wait
    Exec->>Attempt: UpdateAttemptHeartbeat(last_seen_at)
  end

  Stew->>Attempt: GetActiveAttempt + GetAttemptInstance
  Stew-->>Stew: compare heartbeat age to stale / shutdown thresholds

  alt owner alive and heartbeat older than shutdown
    Stew-->>Exec: backend.Kill(owner)
  else heartbeat missing or owner already gone
    Stew-->>Stew: warn only leave closure to orphan sweep
  end

  Sweep->>Attempt: CloseAttemptBead(interrupted:orphan) when owner is dead and heartbeat is stale
  Exec->>Attempt: CloseAttemptBead(result) on success, parked, or failure paths
  ResetFull->>Attempt: plain reset and reset --hard close internal attempt children
  Reset-->>Attempt: resummon and reset --to usually leave the old attempt for later orphan cleanup

  Note right of Attempt: last_seen_at lives here, not on the parent bead
```

## Why The Attempt Bead Exists

- It carries execution ownership: which agent owns the run, which instance stamped it, and which branch/model labels describe that run.
- It carries liveness: the steward reads `last_seen_at` from attempt metadata because parent-bead `updated_at` is not a reliable heartbeat.
- It preserves history per run: success, parked, interrupted, and orphaned outcomes belong to attempts, not to the parent bead's one long-lived identity.
