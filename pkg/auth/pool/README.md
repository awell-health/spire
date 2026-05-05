# pkg/auth/pool

Multi-slot auth pool selection state owned by the wizard.

Design reference: **spi-9hdpji** ("Multi-token auth pool with rate-limit-aware
rotation"). Read it before changing this package — the architectural
commitments live there, not here.

## What this package owns

- **Pool config types**: `Config`, `SlotConfig`, `SelectionPolicy` — the
  in-memory shape of the per-tower `auth.toml`.
- **Per-slot runtime state**: `SlotState`, `RateLimitInfo`, `RateLimitWindow`,
  `RateLimitStatus`, `InFlightClaim` — the cached data persisted at
  `<stateDir>/<slot-name>.json`.
- **POSIX advisory file locks**: `WithExclusiveLock`, `WithSharedLock` — the
  cross-process coordination primitive used by the per-slot state cache.
- (Follow-up beads, not yet present:) config loading & legacy migration,
  state cache I/O, selection policies, JSONL rate-limit-event parsing,
  wait-for-release wake primitive, the selector kernel, heartbeat helper,
  stale-claim sweep.

## What this package does NOT own

- **Slot selection at dispatch time**: the wizard calls into the selector;
  this package is the data layer the selector reads/writes.
- **Wizard or apprentice lifecycle**: starting subprocesses, parking on
  rate-limits, retry policy. Those live in `pkg/wizard` and `pkg/executor`.
- **Steward sweep scheduling**: the goroutine that periodically reaps stale
  claims is wired up in `pkg/steward`; the sweep helper here is pure logic.
- **CLI surface**: `spire config auth ...` verbs live in `cmd/spire`.

## Files

| File | Owner bead | Purpose |
|------|------------|---------|
| `doc.go` | spi-z3nui7 (this) | Package comment. |
| `config.go` | spi-z3nui7 (this) | TOML-shaped types: `Config`, `SlotConfig`, `SelectionPolicy`. |
| `state.go` | spi-z3nui7 (this) | JSON-shaped types: `SlotState`, `RateLimitInfo`, `RateLimitWindow`, `RateLimitStatus`, `InFlightClaim`. |
| `lock.go` | spi-z3nui7 (this) | `WithExclusiveLock`, `WithSharedLock` flock helpers. |
| `lock_test.go` | spi-z3nui7 (this) | In-process and cross-process flock tests. |
| `config_load.go` | spi-4z3v1n | TOML loader with `credentials.toml` legacy fallback. |
| `cache.go` | spi-aj98q9 | Read/write/mutate per-slot state JSON files. |
| `policy.go` | spi-oojqhj | Round-robin and preemptive ranking. |
| `event.go` | spi-u254wr | Parse `rate_limit_event` lines from the apprentice JSONL stream. |
| `wake.go` | spi-dx47xa | Wait-for-release primitive (sync.Cond + inotify). |
| `selector.go` | follow-up | The `Pick`/`Release` kernel. |
| `heartbeat.go` | spi-qly1mt | Heartbeat helper for in-flight claims. |
| `sweep.go` | spi-xmvz1s | Stale-claim sweep helper. |

## Rules

1. **Boundary**: do not import from `pkg/wizard`, `pkg/executor`,
   `pkg/steward`, `pkg/agent`, or `cmd/spire`. The selector is consumed by
   those packages, not the other way around.
2. **No domain language in lock helpers**: `lock.go` knows about file paths
   and `flock(2)`, not about slots or pools.
3. **Pure data in `state.go` and `config.go`**: no methods, no I/O, no JSON or
   TOML parsing logic in the type definitions. Persistence belongs in
   `cache.go` and `config_load.go`.
4. **One writer per slot file**: callers that mutate a `<slot>.json` must hold
   `WithExclusiveLock` on that file (or its sibling `<slot>.lock`, depending
   on the cache layer's choice). The cache helpers will enforce this.
