# Decoupling the Steward Interval from the Daemon Interval

## What happened (spi-m9z864)

`spire up` had a single `--interval` flag (default `2m`) that was forwarded to *both* the daemon and the steward. The two services have wildly different cost/latency profiles:

- **Daemon tick** — heavy: dolt push/pull, Linear sync, webhook processing, OLAP ETL. `2m` is a sane minimum; faster wastes network and CPU.
- **Steward tick** — cheap: a few `ListBeads(status=...)` queries against the local dolt server, PID syscalls, and (after spi-4d2i71) `OrphanSweep`. No network. `2m` meant a `ready`-marked bead waited up to two minutes before a wizard was spawned for it — visible latency for no reason.

The only knob to speed up dispatch was `spire up --interval 30s`, which dragged the daemon along into the same 30s cadence. Bad trade.

The chore added a second flag, `--steward-interval` (default `10s`), and rewired the steward subprocess spawn at `cmd/spire/up.go:419` to use it. `--interval` keeps its `2m` default and now controls only the daemon. The standalone `spire steward` default at `cmd/spire/steward.go:95` was also dropped from `2m` to `10s` for parity.

Concrete touch-points:

- `cmd/spire/up.go`: added the cobra flag in `init()`, added `stewardInterval string` to `upOpts`, defaulted it to `"10s"` in `parseUpArgs`, added the `case "--steward-interval":` arm, threaded it into the steward `SpawnBackground` call, and updated the unknown-flag usage string.
- `cmd/spire/steward.go:95`: `interval := 2 * time.Minute` → `interval := 10 * time.Second`.
- `cmd/spire/up_test.go`: four new parser tests (default, override, daemon-doesn't-affect-steward independence, both-set, missing-value) plus an extension to the unknown-flag test.
- Docs: `README.md` services block, `docs/cli-reference.md` usage line + new flag explanation, `docs/getting-started.md` startup bullet list.

The startup log lines at `up.go:401` (daemon) and `up.go:445` (steward) already echoed `interval %s` per service, so once the values diverged at the source the "surface them separately" requirement in the bead was satisfied without touching `pkg/observability/status.go`.

## Why it matters

A single cadence knob is a category error when two co-tenant loops have different cost profiles. The steward is latency-sensitive (every tick where ready→spawn could have happened but didn't is user-visible); the daemon is cost-sensitive (every extra dolt push/Linear poll is wasted bandwidth and rate-limit pressure). One value can't satisfy both — picking 2m starves the steward, picking 30s overworks the daemon. Splitting the knob lets each loop run at its natural cadence.

10s for the steward is the sweet spot: sub-15s wall-clock latency from `ready` to "wizard running," with per-tick work that's effectively free when there's nothing to do.

## The two-parser tripwire (again)

`cmd/spire/up.go` parses flags in **two places**:

1. The cobra `RunE` (`upCmd.Flags()`), which forwards a normalized argv into `cmdUp`.
2. The hand-rolled `for i := 0; i < len(args); i++ { switch args[i] { ... } }` parser inside `parseUpArgs`.

The hand-rolled parser exists because `cmdUp([]string{...})` is also reachable from internal/test paths that bypass cobra. **Both must be updated** for any new flag — defaulting only on the cobra side leaves `parseUpArgs(nil)` with the wrong value.

This pattern recurs across `cmd/spire/*.go` (see also `spire-up-steward-default.md` for the same lesson on the `--steward`/`--no-steward` flip). Whenever `cmdX(args)` has a hand-rolled switch, expect the dual-parser shape and update both arms.

## Backward compatibility

`--interval` keeps its name and semantics (it just affects fewer things now). Scripts that pass only `--interval` keep working — the daemon picks it up, the steward gets the new `10s` default. Anyone who was relying on the old "both share `--interval`" behavior must now pass both flags explicitly, but no deprecation alias was needed because the flag itself didn't change shape.

## Handling this kind of chore

- **Type:** `chore`. User-visible behavior change (faster dispatch by default, new flag) but mechanical and isolated to one command + its docs.
- **Sweep beyond the bead.** The bead listed `cmd/spire/up.go`, `cmd/spire/steward.go`, and "README/docs/CLI help." A grep for `--interval` and `2m` across `docs/` and the root surfaced three doc files that needed updates (`README.md`, `docs/cli-reference.md`, `docs/getting-started.md`). `docs/troubleshooting.md` mentioned "default: 2 minutes" but in a daemon-specific context that's still accurate, so it was left alone — judgment call, not blanket find-and-replace.
- **K8s is already split.** `chart/spire/values.yaml` and friends already separate `steward.interval` from `syncer.interval`; this chore is local-native only. Don't churn the helm side.
- **Test the parser, not the spawned process.** `up_test.go` exercises `parseUpArgs` directly: defaults, override, independence (daemon override doesn't affect steward), both-set, missing-value, unknown-flag usage advertises the new flag. The actual `SpawnBackground` call is not unit-testable, but propagation from `opts.stewardInterval` → `stewardArgs[2]` is a one-line read in `cmdUp` that the parser tests effectively cover.
- **Match standalone-command defaults.** When `spire up` defaults differ from the standalone (`spire steward`) command's default, users get inconsistent behavior depending on how they launched the loop. Update both in the same commit.
- **Surface in the startup log, not status.** The `started (pid %d, interval %s)` lines for each service already differ by interval once decoupled. `spire status` doesn't display interval today; don't add it just for this chore.
- **Review bar:** Verdict-only sage review is sufficient. The change is mechanical, the test surface is the parser, and the doc sweep is a grep.
