Now I have everything I need. Here is the plan:

---

## Implementation Plan: spi-hf9.1 — `spire pull` command

### Current state

`pull.go` already exists and is largely correct:
- CLI-based `dolt pull` with credential injection via `getCredential()` ✅
- Remote setup mirroring push.go ✅
- Already registered in `main.go` ✅

**Two gaps remain:**
1. Non-fast-forward message suggests `--force`, but the task spec says suggest `spire sync --merge` — and `spire sync` doesn't exist yet.
2. Daemon (`runTowerCycle`) doesn't call pull or push on interval.

---

### Files to change

#### 1. `cmd/spire/pull.go` — message update only

Change the non-fast-forward block (lines 112–116) to suggest `spire sync --merge` instead of `spire pull --force`. Update the `--help` text to describe the three-way split: `pull` (fast-forward), `sync --merge` (three-way merge), `pull --force` (overwrite).

No logic changes — just the user-facing message.

#### 2. `cmd/spire/sync.go` — new file

Implement `cmdSync(args)` + `runSync(merge bool)`.

Design: `spire sync --merge` runs `doltCLIPull(dataDir, false)` exactly like `spire pull`, but **without** the divergence fast-fail check. It lets dolt's three-way merge attempt proceed and surfaces the raw dolt output (including any conflict details) to the user. If merge succeeds, print confirmation. If it fails with conflicts, print dolt's output verbatim and instruct the user to resolve conflicts manually.

Credential injection and remote resolution are identical to `runPull()`.

`--merge` is the only flag. Running bare `spire sync` (no flags) prints usage.

#### 3. `cmd/spire/main.go` — register sync command

Add `case "sync": err = cmdSync(args)` to the switch.

Update `printUsage()` Sync section:
```
  push [url]            Push local database to DoltHub
  pull [url]            Pull from DoltHub (fast-forward; --force to overwrite)
  sync --merge          Three-way merge pull for diverged histories
```

#### 4. `cmd/spire/daemon.go` — dolt sync in runTowerCycle

Add `runDoltSync(tower TowerConfig)` function:
1. Construct `dataDir = filepath.Join(doltDataDir(), tower.Database)`
2. Inject credentials via `getCredential()` → `os.Setenv()`
3. Resolve remote URL via `bd("dolt", "remote", "list")` (works because `daemonDB` is set) → `setDoltCLIRemote(dataDir, "origin", url)` to sync CLI config
4. If no remote configured, log and return early (not an error — just not set up for sync)
5. Call `doltCLIPull(dataDir, false)` — on error, log warning, skip push, return
6. Call `doltCLIPush(dataDir, false)` — on non-fast-forward error, log warning (don't force — dangerous from daemon), return
7. Log success

Call `runDoltSync(tower)` at the **top** of `runTowerCycle()`, before `ensureWebhookQueue()`. This ensures we're working with the latest remote state before running Linear sync.

---

### Key design decisions

**Why no force-push in daemon**: The daemon auto-pushing with `--force` could silently overwrite changes made by other agents. On non-fast-forward push errors, log and skip — the next cycle will re-pull.

**Why `spire sync --merge` vs `spire pull --force`**: `pull --force` overwrites local history (destructive). `sync --merge` attempts a three-way merge (safer, preserves both sides). They're different recovery paths — split into separate commands is clearer.

**Why daemon pull before Linear sync**: We want the daemon to work with the freshest remote state. Pull first ensures the Linear epoch sync reflects committed beads from other nodes.

**Why not call `runPull()`/`runPush()` from daemon**: Both resolve the database via CWD (`readBeadsDBName()`). The daemon has no CWD with `.beads/`. Instead, call `doltCLIPull`/`doltCLIPush` directly with the known `dataDir` — same as what `runPull`/`runPush` do internally.

---

### Edge cases / risks

- **No remote configured**: daemon skips dolt sync silently (not all towers have DoltHub remotes; this is normal)
- **No credentials**: `getCredential()` returns `""`, env vars stay unset, dolt CLI will fail auth — daemon logs warning and continues
- **Merge conflicts during daemon pull**: log warning + skip push; user must run `spire sync --merge` manually to resolve
- **Race between pull and push**: push fails non-fast-forward → log + skip; fixed next cycle
- **`setDoltCLIRemote` in daemon**: this mutates `.dolt/config.json` in the data dir — safe since we're already doing it in `runPush` and `runPull` from the same process

---

### Change order

1. `pull.go` — message change (1 block, no logic change)
2. `sync.go` — new file (defines what the message points to)
3. `main.go` — register + update usage
4. `daemon.go` — `runDoltSync` function + call in `runTowerCycle`
