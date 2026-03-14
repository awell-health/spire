# Spire Daemon — Design Spec

## Problem

Spire's shared Dolt database enables multi-repo agent coordination, but currently requires manual `bd dolt pull`/`push` to sync. Meanwhile, Linear webhook events arrive via a separate Vercel function (spi-7yx) that writes raw events to a DoltHub `webhook_queue` table. Nothing processes those events — no one creates the corresponding beads or notifies the agent that owns the epic.

The daemon closes this loop: it runs a continuous pull/push cycle, reads unprocessed webhook events from the database, creates or updates epic beads, and sends spire mail to notify owners.

## Architecture

The daemon is a new subcommand of the existing `spire` Go binary: `spire daemon`. It runs a tick loop:

```
┌─────────────────────────────────────────────────────────┐
│                    spire daemon loop                     │
│                                                          │
│  1. bd dolt pull          ← fetch remote changes         │
│  2. bd list --label webhook --status=open --json         │
│     ↓ for each event:                                    │
│     a. Parse Linear payload from description             │
│     b. Map Linear labels → repo prefix (rig)             │
│     c. Ensure epic bead exists (create or update)        │
│     d. If owner claimed, spire send notification         │
│     e. bd close <event-bead-id>                          │
│  3. bd dolt push          ← push local changes           │
│                                                          │
│  sleep(interval)                                         │
└─────────────────────────────────────────────────────────┘
```

### Design principles

- **Same process, new subcommand.** No separate binary. `spire daemon` is just another command in `cmd/spire/`.
- **Shells out to `bd`.** Same pattern as all other spire commands. No direct database access.
- **Idempotent processing.** Events are beads. Processing = closing them. A closed event is never reprocessed.
- **No Linear API calls.** The daemon only reads what the Vercel webhook receiver wrote. It does not call Linear's API. It maps webhook payloads to beads.
- **Graceful degradation.** If `bd dolt pull` fails (no remote, offline), the daemon still processes any local events and continues on the next cycle.

## Subcommand: `spire daemon`

```
spire daemon [--interval 2m] [--once]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--interval` | `2m` | Duration between sync cycles |
| `--once` | false | Run a single cycle and exit (for cron/testing) |

### Lifecycle

1. Log startup with interval and identity
2. Run a sync cycle immediately
3. If `--once`, exit after the first cycle
4. Otherwise, `time.Tick(interval)` loop
5. Handle `SIGINT`/`SIGTERM` for clean shutdown (finish current cycle, then exit)

### Sync cycle

Each cycle executes these steps in order:

#### Step 1: Pull

```bash
bd dolt pull
```

Non-fatal if it fails (no remote configured, network down). Log warning and continue.

#### Step 2: Process webhook events

```bash
bd list --label webhook --status=open --json
```

Each webhook event bead has:
- **Title**: event summary (e.g., "Issue updated: AWE-123")
- **Description**: raw Linear webhook JSON payload
- **Labels**: `webhook`, `event:<type>`, `linear:<identifier>`
  - `event:` values: `Issue.create`, `Issue.update`, `Issue.remove`, `Comment.create`, etc.
  - `linear:` value: the Linear issue identifier (e.g., `AWE-123`)

For each open webhook event bead:

1. **Parse**: Extract `event:<type>` and `linear:<identifier>` from labels. Parse the JSON payload from description.
2. **Map to rig**: Extract Linear issue labels from the payload. Map to repo prefix using a label-to-rig mapping:
   - `Panels*` (any label starting with "Panels") -> `pan`
   - `Grove*` -> `gro`
   - `Workstream: Platform` -> `awp`
   - No match -> skip (log and close the event)
3. **Ensure epic bead**: Look up `bd list --rig=<prefix> --label "linear:<identifier>" --type epic --json`
   - If exists: update title and priority if changed
   - If not exists: `bd create --rig=<prefix> --type=epic --title "<title>" -p <priority> --labels "linear:<identifier>"`
4. **Notify owner**: If the epic bead has an owner (someone ran `bd update <id> --claim`), send spire mail:
   ```bash
   spire send <owner> "<identifier> updated (<event-type>)" --ref <epic-bead-id>
   ```
5. **Close event**: `bd close <event-bead-id>` — marks it processed

#### Step 3: Push

```bash
bd dolt push
```

Non-fatal if it fails. Log warning.

### Label-to-rig mapping

The mapping is defined in a Go map, not a config file. It is small and stable:

```go
var labelRigMap = map[string]string{
    "Workstream: Platform": "awp",
}

var labelPrefixRigMap = map[string]string{
    "Panels": "pan",
    "Grove":  "gro",
}
```

Exact-match labels are checked first (`labelRigMap`), then prefix-match labels (`labelPrefixRigMap`). First match wins.

### Priority mapping

Linear priority (from webhook payload) maps to beads priority:

| Linear priority | Meaning | Beads priority |
|----------------|---------|---------------|
| 0 | No priority | 3 (P3) |
| 1 | Urgent | 0 (P0) |
| 2 | High | 1 (P1) |
| 3 | Medium | 2 (P2) |
| 4 | Low | 3 (P3) |

This is the inverse of the mapping in `linear-client.js`.

### Signal handling

The daemon installs a handler for `SIGINT` and `SIGTERM`. When received:
1. Set a `stopping` flag
2. Let the current cycle finish
3. Exit cleanly

This prevents partial processing (e.g., pull without push).

## Webhook event bead schema

The Vercel webhook receiver (spi-7yx, future work) creates these beads. The daemon consumes them. The schema is documented here as a contract.

A webhook event bead:

```
ID:          spi-<hash>
Title:       "Issue updated: AWE-123"
Description: <raw JSON payload from Linear webhook>
Type:        task
Priority:    3
Labels:      [webhook, event:Issue.update, linear:AWE-123]
Status:      open (unprocessed) | closed (processed)
```

The `webhook` label is the query filter. The `event:` and `linear:` labels carry structured data. The description carries the full payload for parsing.

### Linear webhook payload structure (relevant fields)

```json
{
  "action": "update",
  "type": "Issue",
  "data": {
    "id": "uuid",
    "identifier": "AWE-123",
    "title": "The issue title",
    "priority": 2,
    "labels": [
      {"name": "Panels - Design"},
      {"name": "Bug"}
    ],
    "assignee": {
      "name": "JB",
      "email": "jb@awellhealth.com"
    }
  },
  "updatedFrom": {
    "priority": 3,
    "title": "Old title"
  }
}
```

## LaunchAgent

A macOS LaunchAgent plist keeps the daemon running. Added to `setup.sh` alongside the existing Dolt server LaunchAgent.

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.awell.spire-daemon</string>
    <key>ProgramArguments</key>
    <array>
        <string>/Users/jb/.local/bin/spire</string>
        <string>daemon</string>
        <string>--interval</string>
        <string>2m</string>
    </array>
    <key>WorkingDirectory</key>
    <string>/Users/jb/awell/spire</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/opt/homebrew/var/log/spire-daemon.log</string>
    <key>StandardErrorPath</key>
    <string>/opt/homebrew/var/log/spire-daemon.error.log</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>SPIRE_IDENTITY</key>
        <string>spi</string>
        <key>BEADS_DOLT_SERVER_HOST</key>
        <string>127.0.0.1</string>
        <key>BEADS_DOLT_SERVER_PORT</key>
        <string>3307</string>
        <key>BEADS_DOLT_SERVER_MODE</key>
        <string>1</string>
        <key>BEADS_DOLT_AUTO_START</key>
        <string>0</string>
    </dict>
</dict>
</plist>
```

## DoltHub remote setup

Before the daemon can pull/push, a DoltHub remote must be configured. This is a one-time setup step added to `setup.sh`:

```bash
bd dolt remote add origin <dolthub-url>
```

The exact URL depends on the DoltHub org/repo. The daemon assumes a remote named `origin` exists. If it does not, pull/push fail gracefully and the daemon logs a warning.

## File structure

New files:

```
cmd/spire/
  daemon.go     — spire daemon subcommand: tick loop, pull/push, process events
  webhook.go    — webhook event parsing, label-to-rig mapping, epic sync logic
```

Modified files:

```
cmd/spire/
  main.go       — add "daemon" case to the switch
  spire_test.go — add tests for webhook parsing and label mapping

setup.sh        — add LaunchAgent plist for spire daemon
```

## Error handling

- **Pull/push failures**: log warning, continue. The daemon should not crash because the network is down.
- **Malformed webhook payload**: log error with event bead ID, close the event (don't reprocess garbage).
- **No rig mapping**: log "no rig match for Linear labels [...]", close the event.
- **bd command failures**: log error with context. Do not close the event bead (it will be retried next cycle).
- **Epic bead creation failure**: log error, do not close the event bead (retry next cycle).

## Testing

- Unit tests for label-to-rig mapping (pure function, no bd dependency)
- Unit tests for priority mapping (pure function)
- Unit tests for webhook payload parsing (JSON parsing, no bd dependency)
- Integration test: create a webhook event bead, run `processCycle`, verify epic bead was created and event was closed

## Out of scope (v1)

- **Linear API calls from daemon** — the daemon is read-only with respect to Linear. The Vercel function (spi-7yx) is the only thing that talks to Linear's webhook endpoint.
- **Bidirectional sync** — beads-to-Linear sync remains in `index.js` (the epic agent). The daemon handles Linear-to-beads only.
- **Webhook signature verification** — that is the Vercel function's responsibility.
- **Multiple remotes** — single `origin` remote for now.
- **Config file for label mapping** — hardcoded map is sufficient for 3 repos.
