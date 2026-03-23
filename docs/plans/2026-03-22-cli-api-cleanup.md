# CLI API Cleanup (spi-yyf)

Break the CLI surface before Phase 2. Remove the old hub/satellite/init model,
add `pull`, rename `register-repo` to `repo add`, make `repo list` query shared
state. No backward compat — internal users only.

## Current surface (33 commands)

```
init, config, sync, push, register-repo, repo [list|remove], worktree remove,
tower [create|attach|list], up, down, shutdown, status, doctor, metrics,
file, spec, claim, focus, grok, register, unregister, send, collect, read,
connect, disconnect, serve, daemon, steward, board, roster, summon, dismiss,
watch, alert, version, help
```

## Target surface

```
Setup:
  tower create|attach|list       Tower lifecycle
  repo add|list|remove           Repo registration (shared state)
  config get|set|list            Configuration and credentials
  doctor [--fix]                 Health checks and auto-repair

Sync:
  push                           Push to DoltHub
  pull                           Pull from DoltHub (NEW)

Lifecycle:
  up                             Start local system
  down                           Stop daemon
  shutdown                       Stop everything
  status                         Process health + sync state

Work:
  file, spec, claim, focus, grok

Agents:
  summon, dismiss, roster

Messaging:
  send, collect, read

Observability:
  board                          Work queue view
  watch                          Live activity feed
  metrics                        Agent run metrics
  alert                          Alert on bead state changes

Advanced:
  daemon, steward, serve         Internal processes
  register, unregister           Agent message routing
  version, help
```

**Removed:** `init`, `sync`, `worktree`, `register-repo`
**Added:** `pull`, `repo add`
**Moved:** `register`/`unregister` to Advanced

## Changes by file

### Phase 1: Extract + Add (3 new files, no existing file changes)

These are additive — create new files only, zero merge conflicts possible.
All three can run in parallel worktrees.

#### Task A: Create `cmd/spire/dolthub.go`

Extract shared helpers from `sync.go` into a standalone file:
- `normalizeDolthubURL()` — used by push.go, tower.go
- `readBeadsDBName()` — used by push.go
- `parseOriginURL()` — used by push.go
- `resolveDataDir()` — new helper (common pattern in push.go, will be reused by pull.go)

**No changes to existing files.** The old copies in sync.go will be removed in Phase 2.

#### Task B: Create `cmd/spire/pull.go`

New `cmdPull` command mirroring `push.go`:
1. Parse `[<dolthub-url>]` positional arg and `--help`
2. Call `resolveDataDir()` (from dolthub.go)
3. Set up remote if URL provided (same logic as push)
4. Inject DoltHub credentials from credential store
5. Run `dolt pull origin main` via CLI (like `doltCLIPush` but for pull)
6. Handle non-fast-forward: print clear error suggesting `--force` or manual resolution
7. Support `--force` flag for overwrite

**No changes to existing files.**

#### Task C: Create `cmd/spire/scaffolding.go`

Extract doctor-support functions from `init.go`:
- `spireWorkProtocol(prefix string) string`
- `writeSpireMD(repoPath, prefix string) error`
- `writeSpireHooks(repoPath, prefix string)`

These are used by `doctor.go` check functions. They generate CLAUDE.md sections,
SPIRE.md files, and .claude/settings.json hooks for repos.

**No changes to existing files.** The old copies in init.go will be removed in Phase 2.

### Phase 2: Rewrite + Delete (modify/delete existing files)

These changes are interconnected and must happen atomically. Run in a single
worktree after Phase 1 merges.

#### Task D: Delete `init.go` (1079 lines)

The entire hub/satellite/standalone bootstrap flow. All of its logic is either:
- Replaced by `tower create` + `repo add` (already shipped)
- Extracted to `scaffolding.go` (Phase 1, Task C)
- Dead code (hub/satellite role management, shell injection, worktree setup)

Functions to verify are extracted before deleting:
- `spireWorkProtocol` → scaffolding.go
- `writeSpireMD` → scaffolding.go
- `writeSpireHooks` → scaffolding.go

Functions that die with the file:
- `cmdInit`, `regenerateRoutes`, `containsStr`, satellite/hub setup logic

#### Task E: Delete `sync.go` (302 lines)

The `cmdSync`/`runSync` flow is replaced by `pull`/`push`. Helpers extracted to `dolthub.go`.

Functions to verify are extracted before deleting:
- `normalizeDolthubURL` → dolthub.go
- `readBeadsDBName` → dolthub.go
- `parseOriginURL` → dolthub.go

Functions that die with the file:
- `cmdSync`, `runSync`, `bootstrapDatabase`

#### Task F: Rewrite `repo.go`

Remove worktree command entirely. Add `repo add` subcommand that delegates to
`cmdRegisterRepo`. Make `repo list` query the dolt repos table instead of
local config.json.

Before:
```
repo list    → reads local config.json
repo remove  → removes from local config
worktree remove → removes worktree path
```

After:
```
repo add [path]  → delegates to register logic (cmdRegisterRepo)
repo list        → queries dolt repos table (shared state)
repo remove      → removes from dolt repos table + local config
```

Remove all hub/satellite cleanup code from `repoRemove`. Remove `worktreeAdd`,
`worktreeRemove`, `cmdWorktree`, `cleanSatelliteDir`, `removeEnvrcEntry`.

#### Task G: Update `register_repo.go`

- Remove `--database` flag (tower context is implicit via detectDatabase)
- Accept optional positional path argument: `spire repo add [path]`
- Change error message from "spire register-repo" to "spire repo add"
- Remove `printRegisterRepoUsage()` or update it

#### Task H: Simplify `config.go` Instance struct

Remove fields that only exist for the hub/satellite model:
- `Role` field — set to empty string or remove entirely
- `Hub` field — remove
- `Satellites` field — remove

Keep: Path, Paths, Prefix, Database, DoltPort, DaemonInterval, DolthubRemote,
Identity, Tower.

#### Task I: Rewrite `main.go`

- Remove `init`, `sync`, `worktree`, `register-repo` from switch
- Add `pull` case
- Change bare `spire` (no args) to show help instead of falling into init
- Rewrite `printUsage()` to target surface layout above
- Wire `repo add` through `cmdRepo`

#### Task J: Fix references in other files

- `push.go`: Change help text "Counterpart to 'spire sync'" → "Counterpart to 'spire pull'"
- `doctor.go` line 445: Change "run spire init" → "run spire repo add"
- `identity.go` line 57: Update comment "For hubs/standalones..." → simpler
- `welcome.go`: `currentDirName` can stay (used elsewhere?) or remove if orphaned

### Phase 3: Tests + build verification

- Update `register_repo_test.go` — tests are on helper functions, should mostly pass
- Run `go build ./cmd/spire/` — verify clean
- Run `go test ./cmd/spire/` — verify tests pass
- Run `go vet ./cmd/spire/` — verify no issues

## Worktree parallelization strategy

```
main ─────────────────────────────────────────────────────────>
       \                                              /
        staging/cli-cleanup ─────────────────────────/
         \   \   \                    /
          A   B   C  (parallel)      /
           \   \ /                  /
            merge ──> D-J (single) /
                       \          /
                        merge ───/
```

Phase 1 tasks (A, B, C) run in parallel worktrees — all create new files only.
Phase 2 tasks (D-J) run in a single worktree after Phase 1 merges — they're
interconnected.
Phase 3 runs in the same worktree as Phase 2 (build/test verification).
