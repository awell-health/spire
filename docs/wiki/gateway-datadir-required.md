# Gateway dataDir Required at Construction

## What happened (spi-5hmz35)

`pkg/gateway/gateway.go`'s `effectiveDataDir()` silently fell back to
`os.Getenv("BEADS_DIR")` when the server's `dataDir` field was empty.
This meant any test that constructed `&Server{log: ...}` without
setting `dataDir` would route writes — via `store.Ensure`'s
package-level singleton cache — to whatever real beads store the
host's `BEADS_DIR` happened to point at.

The leak: `TestSendMessage_HeaderWinsOverBodyFrom` and
`TestSendMessage_AuthorAndFromUseHeader` accumulated 158
`from:Bob → to:wizard` "hi" messages in the awell-test tower across
multiple CI runs. Commit `3a6e2be` patched those two tests with
`dataDir = t.TempDir()`, but the constructor itself still accepted
empty input.

This chore closes the footgun at the boundary instead of the call
site. Three layers changed:

- **`pkg/gateway/gateway.go`** — `NewServer` now `panic`s if
  `dataDir == ""`. `effectiveDataDir()` lost its `os.Getenv` branch
  and is now `return s.dataDir`. With the env fallback gone, raw
  `&Server{...}` literals in tests that don't set `dataDir` either
  hit the panic (when they reach `effectiveDataDir`) or stay safe
  (when they stub the `*Func` indirection or never touch storage).
- **`cmd/spire/daemon.go`** — `--serve` resolves `BEADS_DIR` via
  `resolveBeadsDir()` at boot. If empty, log a warning and skip
  starting the gateway entirely. The webhook receiver runs without
  `/api/v1/*` rather than panicking on first storage-touching
  request.
- **`cmd/spire/integration_bridge.go`** — `cmdServe` (the Electron
  desktop API gateway) returns a clear error if `BEADS_DIR` is unset
  rather than letting the goroutine panic on construction. Note: this
  path uses `os.Getenv("BEADS_DIR")` directly, not `resolveBeadsDir()`
  — it predates the wider resolver and the chore left it as-is to
  match existing behavior. (See round-1 sage warning; out of scope to
  unify here.)

Test impact was a single line: `TestServer_RunAndShutdown` was the
only `NewServer` caller in tests with an empty dataDir, and it
doesn't exercise storage, so `t.TempDir()` satisfied the constructor
without any behavior change.

## Why it matters

Three independent failure modes met in the original code:

1. **Default-to-prod env fallback.** `os.Getenv("BEADS_DIR")` inside a
   request-path helper means anything missing config silently picks up
   ambient state. That's fine for a CLI, dangerous for a server.
2. **Singleton store cache.** `pkg/store/store.go`'s `Ensure` caches
   `activeStore` at package level. The first caller sets the path; every
   subsequent `Ensure(beadsDir)` call ignores its argument and returns
   the cached handle. So even after the fallback returns `""`, a prior
   real-store cache hit determines the store everyone uses.
3. **Test/prod env coupling.** Developer machines and CI runners both
   set `BEADS_DIR` for normal `bd` use. There's no reason a `pkg/gateway`
   unit test should ever touch that env var, but it did, by default.

Closing any one of these would have prevented the leak. Closing the
fallback is the cheapest and most local fix — the singleton cache is
orthogonal cleanup, and unsetting `BEADS_DIR` in tests is a discipline
that drifts.

## How to spot similar issues

1. **Grep for `os.Getenv` inside request/handler paths.** Anything
   that resolves config inside the hot path (vs. at boot) is a candidate
   — config should arrive as a struct field, not be fetched lazily.
2. **Look for "if X empty, fall back to Y" in helpers.** A pattern like
   `if s.field != "" { return s.field }; return os.Getenv(...)` means
   the constructor accepts empty values and the helper papers over it.
   Push the validation up to the constructor.
3. **Audit constructors that allow zero values for required fields.**
   `NewServer(addr, target, logger, "", "")` compiles fine but produces
   a broken server. A `panic` on zero-value required fields makes the
   contract explicit at the boundary.
4. **Watch for package-level singleton caches.** If `Open`/`Ensure`
   caches a handle keyed by nothing (or by first-call args), it amplifies
   any upstream sloppiness — a single misrouted call poisons every later
   one. `pkg/store/store.go:36` is the canonical example here.

## Handling this kind of chore

- **Type:** `chore`. No behavior change for any caller that was passing
  a real `dataDir`. The change only converts what was previously a
  silent footgun into a loud panic.
- **Scope discipline:** Resist auditing every `os.Getenv` in the codebase
  in the same pass. Each fallback has its own ownership and risk
  profile. The bead's `Out of scope` section called out the
  `commentsAddAsFunc` / `commentsStoreEnsureFunc` indirection cleanup
  and broader env-fallback audits — both were left for separate beads.
- **Test migration:** Categorize call sites first. Of 14 raw
  `&Server{...}` literals in `pkg/gateway/*_test.go`:
  - 4 already set `dataDir: t.TempDir()` (post-`3a6e2be` or always
    correct).
  - 4 stub `commentsStoreEnsureFunc` / `logsStoreEnsureFunc` so the
    return value of `effectiveDataDir()` is ignored. Empty `dataDir`
    is fine here because the stub never reads it.
  - 5 never reach `effectiveDataDir` (auth-only, sync-only, workshop).
    Empty `dataDir` is fine here too.
  Only the public-constructor caller (`TestServer_RunAndShutdown`)
  needed updating. The rest stay as-is — the panic only fires when
  storage code paths actually run.
- **Production callers:** Two — `cmd/spire/daemon.go` (`--serve` webhook)
  and `cmd/spire/integration_bridge.go` (`spire serve` Electron
  gateway). Both must resolve `BEADS_DIR` (or equivalent) at boot
  before calling `NewServer`. Choose between hard-fail (`integration_bridge`,
  user-facing CLI) and soft-skip-with-warning (`daemon.go`, latent
  webhook surface) based on whether the gateway is the primary or
  secondary purpose of the binary.
- **Review bar:** Verdict-only sage review is sufficient. The risk
  surface is narrow: did the panic fire in any test that used to pass,
  and do production callers still resolve `BEADS_DIR` correctly. Both
  are checkable with `go test ./pkg/gateway/...` and a code review of
  the two `NewServer` call sites in `cmd/spire/`.
