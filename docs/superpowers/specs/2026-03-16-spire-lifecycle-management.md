# Spire Lifecycle Management — Design Spec

**Date**: 2026-03-16
**Bead**: spi-ohs
**Status**: Draft

## Problem

Spire currently relies on macOS LaunchAgents (created by `setup.sh`) to manage the dolt server and spire daemon as persistent background services. This has several issues:

1. **Setup is macOS-only.** LaunchAgent plists do not work on Linux.
2. **No user control.** There is no way to start/stop the daemon or dolt server without `launchctl` commands that are arcane and error-prone.
3. **Invisible state.** Users cannot easily check whether dolt or the daemon is running.
4. **Fragile lifecycle.** Starting dolt or the daemon is embedded in `setup.sh` and coupled to LaunchAgent configuration. Re-running setup restarts services unexpectedly.
5. **No guard rails.** Commands that require dolt (collect, send, focus, serve) fail with cryptic errors when the dolt server is not running.

## Solution

Replace LaunchAgent-based service management with three new `spire` subcommands that give users explicit, portable control:

| Command | What it does |
|---------|-------------|
| `spire up` | Start dolt server + spire daemon from zero |
| `spire down` | Stop daemon only (dolt keeps running for other repos) |
| `spire shutdown` | Stop daemon + dolt server (full teardown) |
| `spire status` | Show whether dolt and daemon are running |

Plus two cross-cutting concerns:
- A `requireDolt()` guard that user-facing commands call before doing database work
- Stripping the LaunchAgent plists from `setup.sh` (they are replaced by `spire up`)

## Architecture

### PID-file based process management

Both dolt and the daemon are managed via PID files stored in `.spire/`:

```
.spire/
  dolt.pid              — dolt server process ID
  daemon.pid            — spire daemon process ID
  dolt-config.yaml      — dolt server configuration
```

The `.spire/` directory lives in the spire hub root (same level as `.beads/`). It is created by `spire up` if it does not exist.

### doltserver.go — dolt server lifecycle

A new file `cmd/spire/doltserver.go` encapsulates all dolt server management:

```go
// Core functions:
doltConfig()          // returns config struct (port, host, data dir)
doltWriteConfig()     // writes .spire/dolt-config.yaml
doltStart()           // starts dolt sql-server as background process, writes PID file
doltStop()            // reads PID file, sends SIGTERM then SIGKILL after timeout
doltStatus()          // returns running state: (pid, running bool, error)
doltIsReachable()     // TCP dial to port 3307, returns bool
```

#### Start logic

1. Check if already running (PID file exists + process alive + port reachable)
2. If running, return early with "already running"
3. Ensure dolt data directory exists and is initialized
4. Write `dolt-config.yaml` with port 3307, host 127.0.0.1
5. Start `dolt sql-server --config .spire/dolt-config.yaml` as a background process
6. Write PID to `.spire/dolt.pid`
7. Wait up to 5 seconds for port to become reachable
8. Return success or timeout error

#### Stop logic

1. Read `.spire/dolt.pid`
2. If PID file does not exist or process is not running, return "not running"
3. Send SIGTERM
4. Wait up to 5 seconds for process to exit
5. If still running, send SIGKILL
6. Remove PID file

#### Status logic

1. Check PID file existence
2. If exists, check if process is alive (`kill -0`)
3. If alive, check if port is reachable (TCP dial)
4. Return structured status: `(pid int, running bool, reachable bool)`

### Process spawning

The dolt server and daemon are spawned as detached child processes using `os/exec` with `SysProcAttr` to ensure they survive the parent process exiting. This replaces the LaunchAgent approach.

For the **daemon**, `spire up` effectively runs:
```
spire daemon --interval 2m &
```
As a background process, writing its own PID to `.spire/daemon.pid` at startup.

For **dolt**, `spire up` runs:
```
dolt sql-server --config .spire/dolt-config.yaml &
```
As a background process, with PID captured and written to `.spire/dolt.pid`.

### Data directory

The dolt data directory remains at `/opt/homebrew/var/dolt` (the existing location used by `setup.sh`). This is where `bd init` stores the beads database. The dolt server config points here via `WorkingDirectory`.

On Linux, the directory defaults to `~/.local/share/dolt`. The config reads from `DOLT_DATA_DIR` env var if set.

## Commands

### `spire up`

```
spire up [--interval 2m]
```

1. Create `.spire/` directory if needed
2. Check if dolt data dir exists and is initialized
   - If not, run `bd init` to set up the database
3. Start dolt server via `doltStart()` (no-op if already running)
4. Start spire daemon as background process (no-op if already running)
   - The daemon writes its PID to `.spire/daemon.pid`
5. Print status summary:
   ```
   dolt server: running (pid 12345, port 3307)
   spire daemon: running (pid 12346, interval 2m)
   ```

### `spire down`

```
spire down
```

1. Read `.spire/daemon.pid`
2. Send SIGTERM to daemon process
3. Remove PID file
4. Print: `daemon stopped (dolt still running)`
5. If daemon not running: print `daemon not running`

Does NOT stop dolt — other repos may be using it.

### `spire shutdown`

```
spire shutdown
```

1. Stop daemon (same as `spire down`)
2. Stop dolt server via `doltStop()`
3. Print status for each:
   ```
   daemon: stopped
   dolt server: stopped
   ```

### `spire status`

```
spire status
```

Displays running state of both services:

```
dolt server: running (pid 12345, port 3307, reachable)
spire daemon: running (pid 12346)
```

Or:

```
dolt server: not running
spire daemon: not running
```

Checks: PID file -> process alive -> port reachable (for dolt).

### `requireDolt()` guard

A helper function that user-facing commands call at the top:

```go
func requireDolt() error {
    if !doltIsReachable() {
        return fmt.Errorf("dolt not reachable on 127.0.0.1:3307 — run: spire up")
    }
    return nil
}
```

Commands that need this guard:
- `collect`
- `send`
- `focus`
- `grok`
- `read`
- `serve`
- `daemon`

Commands that do NOT need it (they work offline or manage dolt themselves):
- `register` / `unregister` (these use `bd` which has its own dolt handling)
- `connect` / `disconnect`
- `up` / `down` / `shutdown` / `status`
- `version` / `help`

Actually, since all these commands shell out to `bd` which handles dolt connectivity itself, the guard is a quick TCP dial check that gives a clear error message before `bd` tries and fails with a less clear one. The guard goes in commands where a fast-fail with a helpful message is valuable.

### setup.sh changes

Remove from `setup.sh`:
1. **Step 2** (lines ~93-180): The entire "Central dolt server" section that creates the LaunchAgent plist for dolt. Keep the dolt data dir initialization and env var setup.
2. **Step 8** (lines ~396-458): The "Spire daemon LaunchAgent" section.

Update the "Next steps" output to say `spire up` instead of referencing daemon/LaunchAgent.

Keep in `setup.sh`:
- Beads installation (step 1)
- Dolt data dir creation + initialization
- Env var setup (`BEADS_DOLT_SERVER_*` in `~/.zshrc`)
- Repo verification (step 3)
- Beads hub initialization (step 4)
- Routes and redirects (step 5)
- Cursor integration (step 6)
- Spire CLI build (step 7)

## File Structure

New files:
```
cmd/spire/
  doltserver.go  — dolt server config, start, stop, status, reachability
  up.go          — spire up command
  down.go        — spire down command
  shutdown.go    — spire shutdown command
  status.go      — spire status command
```

Modified files:
```
cmd/spire/
  main.go        — add up/down/shutdown/status cases to switch
  daemon.go      — write daemon.pid on startup; read interval flag
  collect.go     — add requireDolt() at top
  send.go        — add requireDolt() at top
  focus.go       — add requireDolt() at top
  grok.go        — add requireDolt() at top
  read.go        — add requireDolt() at top
  serve.go       — add requireDolt() at top
  spire_test.go  — add tests for dolt server management and lifecycle commands

setup.sh         — strip LaunchAgent sections, update printed instructions
```

## PID file format

Plain text file containing a single integer (the process ID). No trailing newline needed but tolerated.

```
12345
```

## Error handling

- **dolt start fails**: print error, do not start daemon
- **daemon start fails**: print error, dolt remains running
- **stale PID file**: if PID file exists but process is dead, remove PID file and treat as "not running"
- **port already in use**: if dolt port is occupied by another process (not our PID), print error: "port 3307 already in use"
- **requireDolt failure**: print clear message with the command to fix it: `spire up`

## Testing

- Unit tests for PID file read/write/cleanup
- Unit tests for dolt config generation
- Unit test for `requireDolt()` (mock TCP dial)
- Integration tests for `spire up` / `spire down` / `spire shutdown` / `spire status`
- Test stale PID file cleanup
- Test idempotent `spire up` (running twice is safe)

## Out of scope

- **Linux systemd units** — users run `spire up` manually or from their init system
- **Windows support** — SIGTERM/SIGKILL are Unix-only; Windows can be added later
- **Log rotation** — daemon and dolt logs go to stdout/stderr; users can redirect
- **Automatic restart** — if dolt or daemon crashes, user runs `spire up` again
- **Multi-hub** — one dolt server per machine, one daemon per hub
