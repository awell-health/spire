# pkg/board — Spire TUI

Interactive terminal interface for Spire. Built on Bubble Tea
(charmbracelet/bubbletea) with Lipgloss styling.

## Current Architecture

The board is a single-mode Bubble Tea application with a monolithic Model.

```
cmd/spire/board.go          CLI entry point, wires callbacks
pkg/board/tui.go            Model definition, Update loop, key dispatch
pkg/board/render.go         View() — pure function, reads from Snapshot
pkg/board/fetch.go          Background data fetch (fetchSnapshotCmd)
pkg/board/board.go          Types, aliases, column definitions
pkg/board/snapshot.go       BoardSnapshot — pre-fetched state for View()
pkg/board/categorize.go     Bead → column categorization
pkg/board/phase.go          Phase derivation from step beads
pkg/board/dag.go            DAG progress tracking
pkg/board/action_menu.go    Context menu overlay
pkg/board/inspector.go      Inspector pane (details + logs tabs)
pkg/board/search.go         Search/filter
pkg/board/cmdline.go        Vim-style command mode
pkg/board/tower_switcher.go Tower switching overlay
pkg/board/roster.go         Roster display
pkg/board/watch.go          Watch mode
```

**Data flow today:**

```
fetchSnapshotCmd (background goroutine, 5s tick)
  → store.ListBoardBeads()        ← uses package-level store singleton
  → store.GetChildrenBatch()
  → store.GetBlockedIssues()
  → BuildDAGProgressMap()
  → sends snapshotMsg to Update()

Update() receives snapshotMsg
  → stores in Model.Snapshot

View() reads Model.Snapshot
  → pure render, zero I/O
```

**Known issue:** The store singleton (`pkg/store.activeStore`) is opened
once at startup and never reconnected. Tower switching via T-key calls
`store.Reset()` to force re-open (stopgap fix). A dolt server restart
leaves the board with a dead connection until manually restarted.

## Target Architecture: Multi-Mode TUI

The board will become a multi-mode terminal experience. Tab switches
between modes. Each mode owns its data lifecycle independently.

### Actor Model with Per-Mode Fetchers

```
┌─────────────────────────────────────────────────────────┐
│                     Root Model                          │
│  Owns: active mode, tower, identity, selected bead     │
│  Does: Tab routing, keyboard dispatch, mode lifecycle   │
│                                                         │
│  ┌────────┐ ┌────────┐ ┌────────┐ ┌──────┐ ┌───────┐  │
│  │ Board  │ │ Agents │ │Workshop│ │ Msgs │ │Metrics│  │
│  │  Mode  │ │  Mode  │ │  Mode  │ │ Mode │ │ Mode  │  │
│  └───┬────┘ └───┬────┘ └───┬────┘ └──┬───┘ └───┬───┘  │
│      │          │          │         │         │       │
│   tea.Msg    tea.Msg    tea.Msg   tea.Msg   tea.Msg    │
│      │          │          │         │         │       │
│  ┌───┴───┐  ┌───┴────┐ ┌──┴──┐  ┌───┴──┐  ┌───┴───┐  │
│  │ Board │  │ Agent  │ │Wksp │  │Inbox │  │ OLAP  │  │
│  │Fetcher│  │Fetcher │ │Fetch│  │Fetch │  │Fetcher│  │
│  │       │  │        │ │     │  │      │  │       │  │
│  │ dolt  │  │wizards.│ │dolt │  │ dolt │  │duckdb │  │
│  │:beads │  │json+log│ │ +fs │  │:beads│  │       │  │
│  │  5s   │  │   2s   │ │ 10s │  │  5s  │  │  30s  │  │
│  └───────┘  └────────┘ └─────┘  └──────┘  └───────┘  │
└─────────────────────────────────────────────────────────┘
```

### Principles

**No shared store singleton.** Each fetcher owns its own data source
connection. Board, Workshop, and Messages each open their own dolt
connection. Metrics talks to DuckDB. Agents reads the filesystem. A
failure or reconnect in one fetcher does not affect any other mode.

**Root model owns cross-mode state.** Active tower, identity, and
selected bead ID live in the root. When a user selects a bead in the
board and Tabs to metrics, the metrics mode picks up that bead ID.
Modes never share mutable state directly.

**Tower switching propagates through the root.** The root sends a
TowerChanged message to all modes. Each fetcher closes its connection
and re-opens against the new tower's database. No env var games, no
singleton reset.

**Each fetcher handles its own reconnect.** If a dolt connection dies
(server restart, network glitch), the fetcher detects it on the next
tick, reconnects, and re-fetches. The mode shows a transient warning
during the gap. Other modes are unaffected.

**Background vs active fetching.** Board and Messages fetch continuously
(cheap queries, benefit from freshness). Metrics and Workshop fetch only
when their mode is active (heavier queries, less time-sensitive). Agent
mode fetches continuously (filesystem reads are near-free and agent
visibility is the core pain point).

### Modes

| Mode | What it shows | Data source | Cadence |
|------|--------------|-------------|---------|
| Board | Beads in phase columns, DAG progress, epic summaries, alerts, interrupted | dolt: beads, children, deps | 5s |
| Agents | Who is working what, live status, log streaming, capacity | wizard registry (filesystem) + log files | 2s |
| Workshop | Formula browser, step graph rendering, dry-run, validation | dolt: formulas table + filesystem: .beads/formulas/ | on-demand |
| Messages | Inbox, threaded conversations, send/reply | dolt: message beads | 5s |
| Metrics | DORA metrics, formula performance, cost tracking, trends | DuckDB: analytics.db | 30s |

### Migration Path

1. Extract shared state into a RootModel (tower, identity, selected bead)
2. Wrap current board into a BoardMode sub-model
3. Add Tab key routing in RootModel
4. Build AgentMode first (highest user need — "who is working what")
5. Convert BoardMode's fetcher to own its connection (remove singleton dependency)
6. Add remaining modes incrementally

## Ownership

- **pkg/board** owns all TUI rendering, user interaction, and mode
  lifecycle.
- **pkg/board does NOT own** store queries (delegates to pkg/store),
  agent registry reads (delegates to pkg/agent), or formula parsing
  (delegates to pkg/formula).
- **cmd/spire/board.go** wires callbacks (inline actions, tower switch,
  agent fetch, design rejection) and handles CLI flags.
