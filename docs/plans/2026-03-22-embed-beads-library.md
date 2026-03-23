# Plan: Embed beads as Go library in spire

**Date:** 2026-03-22
**Status:** In progress

## Context

Spire calls the `bd` CLI binary via `exec.Command` for all bead operations — 130 subprocess calls across 25 files. The beads repo already has a clean library API (`beads.OpenFromConfig()` returning a `Storage` interface with CRUD, search, labels, comments, deps, config). Embedding it eliminates the subprocess overhead, version drift, and the requirement for users to install `bd` separately.

## What's already done

- `go.mod` has `github.com/steveyegge/beads` with `replace => /Users/jb/dev/beads`
- `cmd/spire/store.go` exists with `ensureStore()`, `openStoreAt()`, `resetStore()`, conversion helpers (`issueToBead`, `issueToBoardBead`)
- `cmd/spire/claim.go` partially migrated (needs to use convenience helpers)
- Dependencies resolved, project compiles

## Architecture

### Store layer (`store.go`)

Singleton pattern — one active store at a time. Interactive commands call `ensureStore()` (auto-discovers `.beads/`). Daemon calls `openStoreAt(beadsDir)` per tower cycle (replaces the `BEADS_DIR` env var approach).

### Convenience helpers (added to `store.go`)

Thin wrappers that hide ensureStore + type conversion:

| Helper | Replaces | Returns |
|--------|----------|---------|
| `storeGetBead(id)` | `bd("show", id, "--json")` + `parseBead` | `Bead, error` |
| `storeListBeads(filter)` | `bdJSON(&result, "list", ...)` | `[]Bead, error` |
| `storeListBoardBeads(filter)` | `bdJSON(&result, "list", ...)` | `[]BoardBead, error` |
| `storeCreateBead(opts)` | `bdSilent("create", ...)` | `string (ID), error` |
| `storeCloseBead(id)` | `bd("close", id)` | `error` |
| `storeUpdateBead(id, updates)` | `bd("update", id, ...)` | `error` |
| `storeAddLabel(id, label)` | `bd("update", id, "--add-label", ...)` | `error` |
| `storeRemoveLabel(id, label)` | `bd("update", id, "--remove-label", ...)` | `error` |
| `storeGetConfig(key)` | `bd("config", "get", key)` | `string, error` |
| `storeSetConfig(key, val)` | `bd("config", "set", key, val)` | `error` |
| `storeDeleteConfig(key)` | `bd("config", "unset", key)` | `error` (type-assert) |
| `storeGetReadyWork(filter)` | `bdJSON(&result, "ready")` | `[]Bead, error` |
| `storeGetComments(id)` | `bdJSON(&comments, "comments", id)` | `[]*beads.Comment, error` |
| `storeAddComment(id, text)` | `bd("comments", "add", id, text)` | `error` |
| `storeGetChildren(id)` | `bdJSON(&result, "children", id)` | `[]Bead, error` |
| `storeCommitPending()` | `bd("dolt", "commit", ...)` | `error` (type-assert) |

For `storeCreateBead`, a struct replaces positional args:
```go
type createOpts struct {
    Title, Description string
    Priority           int
    Type               beads.IssueType
    Labels             []string
    Parent             string  // creates DepParentChild after create
    Prefix             string  // sets Issue.PrefixOverride (the --rig equivalent)
}
```

### Sub-interface access via local interfaces

`DeleteConfig` and `CommitPending` are on sub-interfaces in internal packages. Define local Go interfaces and structurally type-assert:

```go
type configDeleter interface {
    DeleteConfig(ctx context.Context, key string) error
}
type pendingCommitter interface {
    CommitPending(ctx context.Context, actor string) (bool, error)
}
```

### Key mapping details

| bd pattern | Library equivalent |
|---|---|
| `--rig=spi` (in list) | `IssueFilter{IDPrefix: "spi-"}` (with dash) |
| `--rig=spi` (in create) | `Issue.PrefixOverride = "spi"` (no dash) |
| `--label "msg,to:X"` | `IssueFilter{Labels: []string{"msg", "to:X"}}` |
| `--status=open` | `IssueFilter{Status: statusPtr(beads.StatusOpen)}` |
| `bd list` (no status) | `IssueFilter{ExcludeStatus: []beads.Status{beads.StatusClosed}}` |
| `--parent X` | `AddDependency` with `DepParentChild` after create |
| `bd config get` returning "(not set)" | `GetConfig` returning error — wrapper returns `""` |

## Migration tiers

### Tier 1: Base Storage interface (~90 calls, 18 files)

Everything that maps directly to `Storage` methods: list, show, create, update, close, labels, config get/set, ready, comments, children.

### Tier 2: Type-assert (~5 calls, 2 files)

- `connect.go`: config unset (4 calls) → `configDeleter.DeleteConfig`
- `steward.go`: dolt commit (1 call) → `pendingCommitter.CommitPending`

### Tier 3: Keep as subprocess (~35 calls, 5 files)

Operations not on the Storage interface or requiring CLI orchestration:

- **push.go** (10 calls): dolt remote list/add/remove, vc status/commit, credential injection
- **sync.go** (22 calls): dolt fetch/reset/pull/push, schema checks, export/import, vc ops
- **focus.go** (3 calls): `bd cook`, `bd mol pour`, `bd mol progress`
- **board.go** (2 calls): needs dependency data that `SearchIssues` doesn't populate — keep as subprocess until bulk dependency API is available
- **init.go, register_repo.go, tower.go**: database bootstrap via `rawDoltQuery`

`bd()` stays in `bd.go` for these callers.

## File migration order

### Phase 0: Foundation — `store.go`
Add all convenience helpers. Estimated: ~150 lines.

### Phase 1: Simple files (1-3 calls each)
| File | Calls | bd commands replaced |
|------|-------|---------------------|
| `claim.go` | 2 | show, update |
| `read.go` | 2 | show, close |
| `alert.go` | 1 | create |
| `send.go` | 1 | create |
| `register.go` | 3 | list, create, close |
| `summon.go` | 1 | ready |

### Phase 2: Medium files (2-5 calls each)
| File | Calls | Notes |
|------|-------|-------|
| `identity.go` | 2 | config get (affects detectDBName) |
| `collect.go` | 3 | list, show |
| `file.go` | 3 | create, add-label x2 |
| `roster.go` | 4 | list (agents, in_progress) |
| `watch.go` | 4 | list (all, agents, closed) |
| `daemon.go` | 3 | list, close + openStoreAt per tower |

### Phase 3: Complex files (8+ calls each)
| File | Calls | Notes |
|------|-------|-------|
| `steward.go` | 9 | ready, list x4, label ops, CommitPending |
| `epic_sync.go` | 10 | list x2, label ops, comments, config |
| `webhook.go` | 12 | list, create, show, close, config — lazy-init for init() |
| `focus.go` | 9 | show, list x3, children, comments — keep mol/cook as subprocess |
| `grok.go` | 9 | same as focus minus mol, plus config |
| `connect.go` | 9 | config get/set/unset (DeleteConfig type-assert) |

### Phase 4: Cleanup
- Remove `bdJSON`, `bdSilent` if no callers remain (keep `bd()` for Tier 3)
- Clean up stale `ensureProjectID()` in `bd.go`
- Run tests, verify compilation

## Risks and mitigations

1. **`SearchIssues` with nil Status returns ALL issues including closed.** `bd list` excludes closed by default. Mitigation: use `ExcludeStatus: []beads.Status{beads.StatusClosed}` in the convenience helpers when no explicit status is set.

2. **`SearchIssues` doesn't populate Dependencies.** Board.go needs deps for blocked/ready categorization. Mitigation: keep board.go as subprocess (Tier 3).

3. **`webhook.go` init() timing.** Package init runs before store is available. Mitigation: convert to lazy `sync.Once` that loads config on first access.

4. **`CreateIssue` doesn't auto-create parent-child deps.** `bd create --parent X` creates the dep. Mitigation: `storeCreateBead` calls `AddDependency` after `CreateIssue` when `opts.Parent` is set.

5. **Config get returns error vs "(not set)" string.** Mitigation: `storeGetConfig` catches the not-found error and returns `""`.

## Verification

1. `go build ./cmd/spire/` after each phase
2. Run `go test ./cmd/spire/ -run "TestIntegration"` with live dolt server
3. Manual test: `spire claim`, `spire file`, `spire collect`, `spire focus` against an existing tower
4. Daemon test: `spire up` → check daemon.log for per-tower cycles using library
5. Steward test: `spire steward --once` to verify single cycle with library calls

## Files modified

- `cmd/spire/store.go` — major expansion (convenience helpers, local interfaces)
- `cmd/spire/bd.go` — cleanup (remove bdJSON/bdSilent if unused, keep bd())
- `cmd/spire/claim.go` through `cmd/spire/connect.go` — 18 files migrated
- `cmd/spire/spire_test.go` — update test helpers to use store
