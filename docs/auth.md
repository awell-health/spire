# Authentication

Spire carries two credential slots for the `claude` CLI: a Claude
**subscription** token (OAuth token for Max/Team accounts) and an
Anthropic **api-key** (a standard `sk-ant-api03-…` key). Operators
install both, pick a default, and let the wizard decide at summon-time
which one a given run uses.

## Overview

Two slots exist so daily work can run on a subscription (fixed monthly
cost, rate-limited by Anthropic's quota) while api-key spend is reserved
for P0 urgency and headroom when the subscription throttles. Selection
at summon flows through three levers — CLI flags (`--auth` / `--turbo`),
per-run `-H` header overrides, and a hardcoded priority-0 rule — and one
reactive fallback, a 429 auto-promote from subscription to api-key.

## Installing credentials

Auth credentials live in a TOML file at `~/.config/spire/credentials.toml`
(separate from the legacy flat `~/.config/spire/credentials` file, which
still holds `github-token`, `dolthub-user`, etc.). On-disk shape:

```toml
# Spire auth credentials — chmod 600, do not commit to version control
[auth]
default = "subscription"
auto_promote_on_429 = true

[auth.subscription]
token = "sk-ant-..."

[auth.api-key]
key = "sk-ant-api03-..."
```

The file is written with mode `0600`. `auto_promote_on_429` defaults to
`true` when absent.

### CLI install

```bash
# Subscription (OAuth token from claude.ai / Max / Team)
spire config auth set subscription --token sk-ant-oat01-...

# API key (sk-ant-api03-...)
spire config auth set api-key --key sk-ant-api03-...
```

Both verbs accept a `--*-stdin` variant that reads the secret from
stdin, trimming a single trailing newline. Scripts use the stdin form so
secrets never land in `argv` (and therefore never in shell history or
`ps` output):

```bash
echo "$SUBSCRIPTION_TOKEN" | spire config auth set subscription --token-stdin
echo "$API_KEY"           | spire config auth set api-key      --key-stdin
```

The first slot configured on a fresh install is auto-marked as the
default. Change it later with:

```bash
spire config auth default subscription
spire config auth default api-key
```

`default <slot>` refuses to set an unconfigured slot:

```
slot "api-key" not configured — set it with "spire config auth set api-key --key <k>"
```

Inspect what's installed:

```bash
$ spire config auth show
Auth credentials (/home/op/.config/spire/credentials.toml)

* subscription   sk-ant-…wxyz
  api-key        sk-ant-…pqrs

default              = subscription
auto_promote_on_429  = enabled

Recent runs (last 10 per slot)
  subscription:
    2026-04-24 15:30  spi-abc123         implement         12.3k tokens
    2026-04-24 15:27  spi-def456         review             4.1k tokens
  api-key:
    2026-04-24 14:02  spi-a1b2c3         implement         18.9k tokens  (swap→api-key)
```

Leading `*` marks the default slot; `(not configured)` replaces the
masked secret when a slot has no credential. A row annotated
`(swap→api-key)` is a subscription run that auto-promoted mid-flight
(see 429 section below).

Remove a slot:

```bash
spire config auth remove api-key
```

Remove refuses to delete the slot that is currently the default — switch
the default to the other slot first.

> `spire auth …` (top-level) is not a command. The auth CLI lives only
> under `spire config auth …` so the top-level `spire auth` namespace
> stays free for Spire's own tower/Linear auth.

## Selection order at summon

When `spire summon <bead>` runs, `pkg/wizard.SelectAuth` walks a
hardcoded 6-step chain to pick the slot for that run. The order is fixed
(do not try to extend or reorder it via config):

1. `-H x-anthropic-api-key: <value>` → synthesize an **ephemeral**
   api-key context using `<value>`. No configured slot is touched.
2. `-H x-anthropic-token: <value>` → synthesize an **ephemeral**
   subscription context using `<value>`.
3. `--turbo` or `--auth=api-key` → use the configured `[auth.api-key]`
   slot. Errors if unconfigured.
4. `--auth=subscription` → use the configured `[auth.subscription]`
   slot. Errors if unconfigured.
5. Bead priority == 0 → use the configured `[auth.api-key]` slot.
   Errors if unconfigured (see *Hardcoded P0 rule* below).
6. Fallback: use the slot named in `[auth] default`. Errors if
   `default` is empty or points at an unconfigured slot.

An unconfigured slot at any branch is a **hard error**, not a silent
fallthrough. Sample errors (quoted verbatim from the code):

```
api-key slot required (--auth=api-key / --turbo) but not configured
  — run `spire config auth set api-key --key-stdin`

api-key slot required (priority-0 bead) but not configured
  — run `spire config auth set api-key --key-stdin`
  (P0 beads always use the api-key slot)

no auth slot selected and `[auth] default` is unset
  — run `spire config auth default <subscription|api-key>` or pass `--auth=<slot>`
```

The selected `AuthContext` is written onto the wizard's `GraphState`
(JSON file with mode `0600`, parent dir `0700`) before the wizard
subprocess is spawned. See *Uniform across spawns* below.

## `--turbo`

`--turbo` is strictly an alias for `--auth=api-key`. It does not change
the model, the timeout, the dispatch mode, or anything else — it only
picks the slot.

```bash
spire summon spi-xyz --turbo
```

Combining `--turbo` with a non-api-key `--auth` is rejected:

```
--turbo conflicts with --auth=subscription (--turbo is an alias for --auth=api-key)
```

## `-H` header usage

`-H` accepts only two header names. Anything else is rejected — there is
no silent passthrough of arbitrary Anthropic headers.

| Name                    | Slot synthesized | Secret placement                    |
|-------------------------|------------------|-------------------------------------|
| `x-anthropic-api-key`   | api-key          | env `ANTHROPIC_API_KEY` on spawn    |
| `x-anthropic-token`     | subscription     | env `CLAUDE_CODE_OAUTH_TOKEN` on spawn |

Name matching is case-insensitive; repeated headers of the same name
follow "last wins" semantics.

Examples:

```bash
# One-off api-key run without touching config
spire summon spi-abc -H "x-anthropic-api-key: sk-ant-api03-…"

# One-off subscription run with a different token
spire summon spi-def -H "x-anthropic-token: sk-ant-oat01-…"
```

Unsupported header names fail with:

```
unsupported header "x-something-else" (supported: x-anthropic-api-key, x-anthropic-token)
```

Values from `-H` are **not persisted** to the TOML file; they apply to
the single run. Ephemeral contexts are also exempted from the 429
auto-promote (see below) — an inline header is an explicit one-shot
instruction and must not be silently replaced.

## Hardcoded P0 rule

Priority-0 beads automatically select the api-key slot, regardless of
`[auth] default`. No config toggle turns this off. If you need a P0 bead
to run on the subscription (unusual), pass `--auth=subscription`
explicitly — step 4 of the selection chain fires before the P0 rule at
step 5.

If `[auth.api-key]` isn't configured when summoning a P0 bead, summon
aborts with the error quoted under *Selection order* above. The whole
summon call aborts on the first bead that fails selection, so an
operator batching multiple beads sees every problem at once rather than
ending up with a partial spawn.

## 429 auto-promote fallback

When the active Claude CLI call returns a 429, the wizard may swap its
in-memory `AuthContext` from subscription to api-key and retry the call
once. The swap is one-way: once promoted, a wizard run stays on api-key
for the remainder of its process.

### When the swap happens

All of the following must hold (`pkg/agent.ShouldAutoPromote`):

- Active slot is `subscription`
- `[auth.api-key]` is configured
- `auto_promote_on_429 = true` (the default)
- The context is **not** ephemeral (i.e. not from a `-H` override)

On a 429 with those preconditions, the wizard logs this structured INFO
line to stderr before retrying:

```
[wizard] auth: 429 on subscription, promoting to api-key (cooldown=in-memory) bead_id=<id> step=<label>
```

After the swap the wizard retries the same call exactly once with the
new env (`ANTHROPIC_API_KEY` replaces `CLAUDE_CODE_OAUTH_TOKEN`) and
returns whatever the retry produced. The run's `agent_runs.auth_profile`
stays at its starting value (`subscription`) and `auth_profile_final` is
set to `api-key` so cost analysis can see the promote.

### When the swap does NOT happen

- Already on api-key → 429 propagates normally; the caller's backoff
  handles it.
- `[auth.api-key]` is not configured → swap skipped; 429 surfaces to
  the caller.
- `auto_promote_on_429 = false` → swap skipped; 429 surfaces.
- Context is ephemeral (from `-H x-anthropic-*`) → swap skipped; the
  inline credential is respected even on a 429.

### Stickiness and lifetime

The swap mutates the wizard's in-memory `*AuthContext`. Every subsequent
Claude invocation in the same wizard process — apprentice wave/fix
spawns, sage reviews, cleric-worker, arbiter — sees the promoted slot.
Nothing is persisted: a wizard restart reloads the selected slot from
`GraphState`, which still records the *original* summon-time selection,
so a restart reverts to subscription (and will re-promote on the next
429 if the conditions still apply).

## Observability

Two places surface what credential each run used.

### `spire cost`

Aggregates `agent_runs` grouped by the **starting** `auth_profile` and
renders a total plus a per-slot split:

```
Cost (all recorded runs)
  Total runs: 142   Total tokens: 4.8M   Total cost: $12.47

By auth profile:
  subscription:     98 runs     3.2M tokens   $0 metered
  api-key:          44 runs     1.6M tokens   $12.47 actual
  (3 runs promoted subscription → api-key after 429; attributed to subscription)
```

Notes on the columns:

- `subscription` spend is always `$0 metered` — Anthropic doesn't
  expose per-token subscription cost, so the number would be misleading.
  Tokens are still counted so volume is visible.
- `api-key` spend is the real per-token dollar amount Spire computes
  from `cost_usd` on each row.
- 429 swap rows attribute the full run's tokens and spend to the
  **starting** slot (subscription), even though the tokens after the
  swap were actually billed on api-key. Token-level pre/post split
  isn't recoverable from the schema — Anthropic bills per call, not
  per run-segment — so the footer line flags the count of swap rows
  so operators can reconcile.
- A `(unrecorded)` row appears for historical rows whose `auth_profile`
  is NULL (pre-dating spawn-point plumbing).

### `spire config auth show` recent runs

The `show` footer appends up to 10 most-recent rows per slot so an
operator can sanity-check what each slot has been doing lately without
writing a SQL query:

```
Recent runs (last 10 per slot)
  subscription:
    2026-04-24 15:30  spi-abc123         implement         12.3k tokens
    2026-04-24 15:27  spi-def456         review             4.1k tokens
  api-key:
    2026-04-24 14:02  spi-a1b2c3         implement         18.9k tokens  (swap→api-key)
    (no runs yet)
```

A `(swap→api-key)` annotation flags rows whose `auth_profile_final`
differs from `auth_profile` — i.e. a mid-run 429 promote landed that
row in the api-key dollars bucket while the run started on subscription.
If the reader can't fetch runs (fresh install, Dolt unreachable), a
`(recent runs unavailable: …)` note replaces the block — `show` stays
useful for read-only config inspection even without the observability
tables.

### `agent_runs` schema

Two nullable `TEXT` columns (migration `006_auth_profile.sql`):

| Column               | Meaning                                                  |
|----------------------|----------------------------------------------------------|
| `auth_profile`       | Slot active at run-start: `"subscription"` or `"api-key"` |
| `auth_profile_final` | Set only when a 429 swap fires mid-run; holds the slot   |
|                      | the run *ended* on. NULL = no swap.                      |

Historical rows (pre-plumbing) keep both columns NULL. Query them
directly with `spire sql` for ad-hoc reporting.

## Uniform across spawns

Whatever slot the wizard selected at summon is attached to `GraphState`
and threads through every downstream `claude` invocation in that run —
apprentice wave spawns, apprentice fix spawns, sage review subprocess,
arbiter subprocess, cleric-worker. The only way a child ever sees a
different slot is the one-way 429 auto-promote above, which mutates the
shared in-memory `*AuthContext` so all subsequent spawns pick up the
promoted value.

This invariant is enforced at the spawn site: `AuthContext.InjectEnv`
appends `ANTHROPIC_API_KEY` for api-key slots and
`CLAUDE_CODE_OAUTH_TOKEN` for subscription slots, and
`MergeAuthEnv` strips conflicting env vars from the parent environment
before spawn so the child can't fall back to a leftover variable.

## Security notes

- **File mode 0600.** `~/.config/spire/credentials.toml` is written
  mode `0600` on every save. Verify with `ls -l` after `spire config
  auth set …`.
- **Never pass secrets in `argv`.** Use `--token-stdin` / `--key-stdin`
  for scripted installs so the secret doesn't land in shell history or
  a process listing. The stdin reader trims a single trailing newline,
  so `echo "$X" | spire config auth set …` is safe.
- **`-H` overrides are scoped to one run.** They never touch
  `credentials.toml` and they are exempt from 429 auto-promote.
- **Migration from the legacy flat file is automatic.** On the first
  call that runs `ReadAuthConfig` (any `spire config auth …` verb or a
  `spire summon`), any of `ANTHROPIC_API_KEY`, `anthropic-key`,
  `ANTHROPIC_AUTH_TOKEN`, or `CLAUDE_CODE_OAUTH_TOKEN` entries found
  in `~/.config/spire/credentials` are promoted into
  `~/.config/spire/credentials.toml` under the appropriate slot. The
  corresponding lines are stripped from the flat file; unrelated
  entries (GitHub token, DoltHub creds) are left in place. Migration
  is idempotent — a second call is a no-op.
- **Environment variable interop.** Downstream `claude` subprocesses
  read `ANTHROPIC_API_KEY` (api-key slot) or `CLAUDE_CODE_OAUTH_TOKEN`
  (subscription slot). Don't set these in your shell env before
  summoning; the wizard strips conflicting values before spawn, but
  relying on env-var fallback for auth makes the selection chain
  harder to reason about.

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `api-key slot required (--auth=api-key / --turbo) but not configured` | `--turbo` or `--auth=api-key` with no `[auth.api-key]` in TOML | `spire config auth set api-key --key-stdin` |
| `api-key slot required (priority-0 bead) but not configured` | Summoning a P0 bead with no api-key slot | Install api-key (see above) or override with `--auth=subscription` |
| `subscription slot required (--auth=subscription) but not configured` | Explicit `--auth=subscription` with no subscription token | `spire config auth set subscription --token-stdin` |
| `no auth slot selected and [auth] default is unset` | Both slots configured but no default | `spire config auth default <subscription\|api-key>` |
| `unsupported header "X" (supported: x-anthropic-api-key, x-anthropic-token)` | `-H` with an unrecognized name | Use one of the two supported names; other Anthropic headers are not passed through |
| `--turbo conflicts with --auth=subscription (--turbo is an alias for --auth=api-key)` | Combining `--turbo` with `--auth=subscription` | Drop one of the two flags |
| `cannot remove "subscription" while it is the default — switch first with "spire config auth default api-key"` | `remove` on the default slot | Switch the default, then remove |
| `slot "api-key" not configured — set it with "spire config auth set api-key --key <k>"` | `spire config auth default api-key` before installing it | Install the slot first, then switch the default |
| 429s keep failing after the swap attempt | Already on api-key (no further swap), or the api-key quota is also saturated | Wait; normal Anthropic backoff applies. Check `auto_promote_on_429` is enabled. |
| `spire config auth show` prints `(recent runs unavailable: …)` | Dolt unreachable or `agent_runs` table missing | Check `spire status`; the footer is non-fatal and `show` still lists configured slots. |

---

*Last reviewed against commit `7eec8a0d5c8725dac53f2a52e607a74665412ae8`
(epic `spi-gsmvr4` tip, 2026-04-24) — `pkg/config/auth.go`,
`cmd/spire/config_auth.go`, `cmd/spire/summon.go`,
`pkg/wizard/summon_select.go`, `pkg/agent/claude_client.go`,
`cmd/spire/cost.go`.*
