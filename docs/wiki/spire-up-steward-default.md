# Flipping a CLI Default: `spire up` Starts the Steward

## What happened (spi-ehv14m)

`spire up` was the local-native startup command, but it only launched dolt + the sync daemon by default. The steward — the loop that owns work assignment, hooked-step resume, and lifecycle maintenance — required `--steward`. Forgetting that flag produced a "half-running" tower: sync infrastructure up, control plane down. Ready beads sat unclaimed, hooked steps never resumed, and `spire status` quietly reported `steward: not running`.

The fix flipped the default. Now `spire up` starts dolt + daemon + steward together, and `--no-steward` is the explicit opt-out for sync-only / debug runs. The old `--steward` flag is kept as a deprecated no-op so existing scripts and snippets keep working.

Concrete changes in `cmd/spire/up.go`:

- Cobra: replaced `Bool("steward", false, ...)` with `Bool("no-steward", false, ...)`. Kept `Bool("steward", ...)` and called `MarkDeprecated("steward", ...)` so the flag still parses but prints a deprecation notice.
- Hand-rolled parser: extracted into `parseUpArgs([]string) (upOpts, error)` and changed the default to `startSteward: true`. Added a `case "--no-steward":` arm. Kept the `case "--steward":` arm as a comment-only no-op.
- The unknown-flag usage string in `parseUpArgs` advertises `--no-steward` (not `--steward`).
- Step-3 comment in `cmdUp` updated from `// Step 3: Start steward (if --steward)` to `// Step 3: Start steward (default; skipped by --no-steward).`
- Six doc files updated (the bead listed three; a `grep -n "spire up --steward"` sweep across `docs/` and the root surfaced six): `README.md`, `docs/VISION-LOCAL.md`, `docs/troubleshooting.md`, `docs/getting-started.md`, `docs/cli-reference.md`, `docs/ARCHITECTURE.md`.

`spire down`, `spire shutdown`, and `spire status` already handled the steward unconditionally — the PID file lives at the same path either way — so no lifecycle plumbing needed to change.

## Why it matters

A startup command's default is a contract about what a working installation looks like. While `--steward` was opt-in, the only correct invocation for a local-native tower was `spire up --steward`, but the only documented quick-start command was `spire up`. The "obvious" command produced a broken-but-not-erroring state, and `spire status` reported the partial state truthfully — which made the bug invisible until someone noticed work wasn't being dispatched.

Flipping the default makes the obvious command correct. The opt-out exists because two legitimate sync-only scenarios remain: (1) debugging dolt/daemon in isolation, (2) a multi-machine tower where one designated machine owns assignment and the others run sync infrastructure only.

## The two-parser tripwire in `cmd/spire/up.go`

`up.go` parses its flags in **two** places:

1. The Cobra `RunE` (`upCmd.Flags()`), which forwards a normalized argv into `cmdUp`.
2. A hand-rolled `for i := 0; i < len(args); i++ { switch args[i] { ... } }` parser inside `cmdUp` (now `parseUpArgs`).

The hand-rolled parser exists because `cmdUp([]string{...})` is also the entry point for internal/test call paths that don't go through Cobra. **The defaults must be flipped in both places.** Flipping only the Cobra flag default would still leave `cmdUp(nil)` with `startSteward := false`, and the bug would persist for every internal caller.

This shows up in other `cmd/spire/*.go` files too — anywhere there's a Cobra command whose `RunE` immediately calls a `cmdX(args)` function, expect the same dual parsing.

## Back-compat pattern: deprecated alias + new opt-out flag

The repo already uses `--no-X` opt-outs in five other commands: `--no-log`, `--no-file`, `--no-changes`, `--no-assign`, `--no-follow`. Same shape both times:

```go
// Cobra side
upCmd.Flags().Bool("no-steward", false, "Don't start the steward (sync-only/debug mode)")
upCmd.Flags().Bool("steward", false, "Deprecated: steward starts by default; use --no-steward to opt out")
_ = upCmd.Flags().MarkDeprecated("steward", "steward starts by default; use --no-steward to opt out")
```

```go
// Hand-rolled parser
case "--steward":
    // Back-compat no-op: the steward starts by default now.
case "--no-steward":
    opts.startSteward = false
```

`MarkDeprecated` makes Cobra emit a warning when the flag is used but still parses it. Don't remove the old flag — every existing README, script, snippet, and tutorial uses it, and the user-facing message is "your old command still works, but the default has changed" rather than "your command is broken now."

## Why extract `parseUpArgs`

`cmdUp` does real I/O: starts dolt, starts the daemon, starts the steward, writes PID files, talks to the OS. None of that is unit-testable. But the parsing — defaults, flag arms, error messages — is pure. Extracting `parseUpArgs(args []string) (upOpts, error)` lets `up_test.go` exercise the bug directly: `parseUpArgs(nil)` must return `startSteward: true`, `parseUpArgs([]string{"--no-steward"})` must return `false`, `parseUpArgs([]string{"--steward"})` must accept the deprecated flag without flipping the default off.

The extracted struct (`upOpts{interval, startSteward, backendName, metricsPort}`) was named for clarity; the field-by-field assignment back into `cmdUp`'s locals at the call site (`interval := opts.interval`, etc.) is intentional churn-minimization — it keeps the diff for `cmdUp`'s body to a one-line replacement.

## Handling this kind of chore

- **Type:** `chore`. Behavior change for users, but mechanical and isolated to one command + its docs. Don't bundle in adjacent cleanup (e.g., moving `OrphanSweep` from daemon into steward, which is `spi-4d2i71`'s scope).
- **Sweep beyond the bead.** The bead listed 3 doc files; `grep -n "spire up --steward"` across `docs/` and the root found 6. Run the grep, decide on the spot how to handle each hit, and update them in the same commit.
- **Watch for conflicting old advice.** When a default flips, two doc paragraphs that were "consistent enough" before may now contradict. Here, `docs/getting-started.md` ("multiple stewards coexist via instance scoping") and `docs/troubleshooting.md` ("only one machine should run the steward") gave different advice; the chore reconciled them on the getting-started framing because attempt leases already prevent races.
- **Test the extracted parser, not the external processes.** The new `cmd/spire/up_test.go` covers: default starts steward, `--no-steward` opts out, `--steward` is back-compat no-op, `--no-steward` wins when both are passed, all-flags round-trip, unknown flag's usage string advertises `--no-steward` (and not the deprecated `--steward`).
- **Update both parsers when `cmd/spire/<cmd>.go` has a Cobra flag *and* a hand-rolled `cmdX(args)` switch.** It's an easy thing to miss.
- **Review bar:** Verdict-only sage review is sufficient. The change is mechanical, the test surface is the parser, and the doc sweep is a grep.
