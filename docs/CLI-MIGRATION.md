# CLI migration: explicit tower/prefix for graph-state commands

> Context: chunk 3 of [docs/design/spi-xplwy-runtime-contract.md](design/spi-xplwy-runtime-contract.md).
> Status: lands with spi-ypoqx.

## What changed

`pkg/executor.ResolveGraphStateStore` previously resolved its dolt
database name by walking up from `os.Getwd()` looking for
`.beads/metadata.json`, and falling back to the hardcoded string
`"spire"` when that walk found nothing. That ambient-CWD fallback is
permanently removed. The function now takes an explicit `RepoIdentity`
and returns a typed error when none is supplied:

- `executor.ErrNoTowerBound` — no active tower is resolvable.
- `executor.ErrAmbiguousPrefix` — the active tower has multiple
  registered repos and the caller did not choose one.

CLI entry points resolve `RepoIdentity` through a new helper
(`resolveRepoIdentity` in `cmd/spire/repo_identity.go`) that reads the
active tower via `config.ActiveTowerConfig` and picks a prefix from the
tower's registered-repo set.

## User-visible impact

### Running outside a bound repo

Commands that wrote graph state used to silently connect to database
`"spire"` even when run from an unrelated directory. Now they print:

```
no tower bound for this command. Run `spire tower create` to create one,
or `spire repo add` inside a registered repo. Use --tower <name> to target
a specific tower when multiple exist.
```

and fall back to a local file-backed graph store scoped by
`~/.config/spire`. The steward daemon logs the fallback once at
startup and keeps running in local mode; `spire execute` behaves the
same.

Fix: `spire tower create --name <tower>` or `cd` into a directory
registered with `spire repo add`.

### Multi-prefix towers

Single-prefix towers auto-resolve. Multi-prefix towers require
`--prefix=<one>`:

```
tower "my-team" has prefixes [api, spi, web] — rerun with --prefix=<one>
```

The existing global `--tower <name>` flag selects the active tower.

### `SPIRE_TOWER` env still works

Subprocess chains (wizard → apprentice → sage) that pass
`SPIRE_TOWER=<name>` through the environment are unchanged — the env
bypasses CWD resolution in `config.ActiveTowerConfig`, so agent
pipelines keep working without extra flags.

## How to fix, by scenario

| You see | Fix |
| --- | --- |
| `no tower bound ...` on a fresh install | `spire tower create --name <tower>` |
| `no tower bound ...` inside a repo you thought was registered | `spire repo add` (or check `spire repo list`) |
| `tower ... has prefixes [...] — rerun with --prefix=<one>` | Pass `--prefix=<p>` or switch towers with `--tower <n>` |
| Wizard subprocess running outside its repo | Ensure the parent sets `SPIRE_TOWER` (the executor does this automatically when dispatching work) |

## For developers

- New CLI commands that need a graph-state store should call
  `resolveGraphStateStoreForCLI("")` (strict — surface errors) or
  `resolveGraphStateStoreOrLocal("")` (lenient — fall back to local
  file store and log the reason).
- Commands that legitimately run outside a bound repo
  (e.g. `spire tower create`) MUST NOT call either helper.
- The `pkg/runtime` audit test (`pkg/runtime/audit_test.go`) gates
  any new `os.Getwd` or `dolt.ReadBeadsDBName` call inside
  `pkg/executor`, `pkg/wizard`, `pkg/apprentice`, or `pkg/agent`.
  Legitimate non-identity uses require an allowlist entry with a
  one-line justification.
