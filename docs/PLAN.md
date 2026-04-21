# Spire v1.0 Roadmap

> From working v3 engine to production-ready open-source release.

---

## Current State (v0.39.0, 2026-04-15)

Spire is a working local-first coordination hub for AI engineering agents.
The v3 graph executor is the only execution engine. V2 code is fully
removed. Step beads carry operational status (hooked, in_progress, closed)
replacing label-based routing. The cleric (recovery agent) runs
git-aware, worktree-capable recovery with cooperative wizard handoff.
Multi-local steward safety uses instance identity and attempt leases.
The Helm chart deploys steward, Dolt, and ClickHouse with proper
bootstrap. `spire review`, `spire ready`, and `spire file --design`
are shipped.

**Runtime contract (spi-xplwy) — complete.** The four-type canonical
worker-runtime contract (`RepoIdentity`, `WorkspaceHandle`,
`HandoffMode`, `RunContext`) is landed and enforced identically across
local process, `pkg/agent`-k8s, and operator-k8s surfaces. Ambient-CWD
identity derivation is removed from runtime-critical packages and
gated by `pkg/runtime/audit_test.go`. Push transport is quarantined as
`HandoffMode=transitional`, Warn-logged and CI-gated under
`SPIRE_FAIL_ON_TRANSITIONAL_HANDOFF=1`. See
[docs/design/spi-xplwy-runtime-contract.md](design/spi-xplwy-runtime-contract.md)
and [docs/CLI-MIGRATION.md](CLI-MIGRATION.md). The phase-2 WizardGuild
repo-cache work (spi-sn7o3) now sits cleanly on top of this contract as
a new `WorkspaceOrigin=guild-cache` option — not a prerequisite, not a
blocker.

### What shipped: v0.28 to v0.39

| Version | Theme | Key changes |
|---------|-------|-------------|
| v0.29.0 | Reliability | Tower attach/daemon fixes, merge race handling, interrupted-work recovery UX (alerts, resummon, approval-gated repair) |
| v0.30.0 | Formula v3 engine | Graph interpreter, declarative step graphs with conditions and opcodes, nestable review loops, crash-safe resume, all built-in formulas migrated |
| v0.31.0 | V2 removal + recovery | V2 formula/workshop/focus stripped; recovery became first-class bead type with dedicated formula, structured metadata, prior-learning lookup |
| v0.32.0 | Tower formula sharing | Formulas stored in dolt and synced via daemon; `spire formula list/show/publish/remove` CLI; resolution chain updated to tower -> repo -> embedded |
| v0.33–0.35 | Formula polish + executor features | Canonical formula names (dropped -v3 suffix), v2 embedded TOML deleted, FormulaV2 types removed, hooked step status, `spire resolve`, deferred status, per-step provider override, inline prompts (`with.prompt`), `with` parameter interpolation, `human.approve` action, `spire summon` accepts bead IDs |
| v0.36–0.38 | Steward production + K8s foundation | Concurrency limiter, merge queue, trust gradient, A/B routing, health endpoints, K8s manifests, Helm chart, ClickHouse OLAP, Dolt StatefulSet, graph state persistence in Dolt |
| v0.39.0 | Operational state + cleric + multi-local | Status-based routing (hooked replaces interrupted labels), cleric recovery agent with cooperative wizard handoff, multi-local steward safety with instance leases, `spire review`/`spire ready`/`spire file --design`, v2 dead code fully removed, compact board attention line |

### What works today

- **V3 graph executor** -- declarative step graphs with conditions, opcodes, nestable review loops, crash-safe resume, hooked step status for gate actions, `with` parameter interpolation
- **Tower-level formula sharing** -- formulas in dolt, synced across machines, CLI for publish/list/show/remove
- **Formula resolution** -- tower -> repo -> embedded (first match wins); `spire resolve` command for manual gate resolution
- **Operational state model** -- step beads carry status (hooked, in_progress, closed); board and steward route by bead status, not labels; parent bead reflects hooked when any step is parked
- **Cleric (recovery agent)** -- git-aware, worktree-capable recovery with cooperative wizard handoff; collect_context → decide → execute → verify (polling loop) → learn → finish
- **Recovery system** -- first-class recovery bead type with structured metadata, prior-learning lookup, failure classification; cleric-default formula
- **Built-in formulas** -- task-default, bug-default, epic-default, chore-default, cleric-default, plus subgraph-review and subgraph-implement sub-graphs
- **Formula features** -- per-step provider override, inline prompts (`with.prompt`), `human.approve` action for approval gates
- **Interactive board TUI** -- Bubble Tea with Hooked/Backlog columns, status-based action menus, inspector with hooked step details, Shift-J/K fast scroll, compact attention line, command mode, tower switcher, search/filter
- **Local agent execution** -- `spire summon` spawns wizard executors (accepts bead IDs or count); apprentices work in isolated worktrees and write metadata.commits; sages review; arbiters break ties
- **Steward** -- active work assignment, concurrency limiter (per-tower), merge queue, trust gradient, health endpoints, hooked-step sweep, multi-local safety with instance leases
- **Full CLI surface** -- `spire file --design`, `spire review`, `spire ready`, `spire update`, plus tower management, repo registration, agent messaging, observability (board/roster/watch/logs/metrics)
- **Helm chart + operator** -- v3-aligned CRDs (WizardGuild, SpireWorkload, SpireConfig), agent/steward/dolt/syncer templates
- **CI/CD** -- goreleaser, GitHub Actions, Homebrew tap

---

## V1.0 Priorities

Eight workstreams. Items 1, 2, 4, 6, 7, and 8 can run in parallel.
Item 3 benefits from the unified daemon (item 2). Item 5 benefits from
the workshop skill (item 4) and observability (item 7) for its modes.

```
1. V2 Removal ─────────────────────── (independent)

2. Operational Steward ─────────────── (independent)
        |
        v
3. K8s / Helm ──────────────────────── (benefits from unified daemon)

4. Workshop Skill ──────────────────── (independent)
        |
        v
5. Multi-Mode TUI ─────────────────── (workshop + observability feed into this)

6. Multi-Backend ───────────────────── (independent)

7. Observability ───────────────────── (independent, feeds into TUI metrics mode)

8. CI Pipeline ─────────────────────── (independent, gates all other work)
```

### 1. Complete V2 Removal

V2 formula types, resolution, embedded formulas, and FormulaV2 types
are fully removed. Formula files use canonical names (no `-v3` suffix).
Remaining v2 references are limited to `cmd/spire/` bridge files and
`pkg/board/dag.go`.

- [x] `cmd/spire/` bridge cleanup -- v2 fallbacks removed (`spi-b0ejk`)
- [x] `pkg/wizard/deps.go` -- remove v2 alias and LoadFormulaByName dep
- [x] `pkg/board/dag.go` -- v2 phaseIndex fallback removed (`spi-b0ejk.3`)
- [x] Test mass deletion -- v2-specific test functions removed; FormulaV2 types deleted
- [x] Rename `-v3` formula files to canonical names (drop suffix)
- [x] Delete remaining v2 embedded TOML files (spire-recovery-work.formula.toml)
- [x] Dead v2 types and dual v2/v3 paths removed (`spi-b0ejk`)

### 2. Operational Steward

The steward already assigns work, spawns agents, routes reviews, and
monitors health. But it runs as a sibling process, has no concurrency
limits, and spawning is immediate/eager with no wave batching.

- [ ] Unified daemon -- merge steward loop into `spire up` (single process, not sibling)
- [x] Single-daemon enforcement -- flock-based file locking (`spi-oelwk`)
- [x] Ready-gate workflow -- `spire ready <id>` marks beads for steward pickup; `GetReadyWork` filters by status=ready (`spi-lccod`)
- [x] Per-tower concurrency limits -- `max_concurrent` in tower config, `ConcurrencyLimiter` filters by tower (`spi-kol5t`, `spi-m0ic5.4`)
- [ ] Wave batching -- group ready work assignment into configurable waves
- [x] Capacity reporting -- active agents, queue depth in `spire status` and `/health/detailed` (`spi-eki8x`)
- [x] Steward health endpoint -- `/readyz`, `/livez`, `/health/detailed` with cycle stats and merge queue (`spi-eki8x`, `spi-m0ic5.2`)
- [x] Hooked-step sweep -- steward queries step beads by status=hooked (`spi-724yo`)
- [x] Multi-local safety -- instance identity, attempt leases, tower-scoped agent ownership (`spi-8wc0w`)

### 3. Kubernetes / Helm Operational

The Helm chart and operator are v3-aligned with clean CRDs, and all
three runtime surfaces (local process, `pkg/agent`-k8s, operator-k8s)
now satisfy the spi-xplwy canonical runtime contract. The main gaps
are cluster bootstrap and the operator reading the repos table.

- [x] Canonical runtime contract (spi-xplwy) -- `RepoIdentity`, `WorkspaceHandle`, `HandoffMode`, `RunContext` hold across local/`pkg/agent`-k8s/operator-k8s. Push transport quarantined as `HandoffMode=transitional`. See [design](design/spi-xplwy-runtime-contract.md).
- [ ] Bootstrap job -- `spire tower attach <dolthub-url>` as a Helm hook on install
- [ ] Operator reads repos table -- auto-derive WizardGuild CRs from dolt repos table
- [ ] Image version alignment -- Dockerfiles should track latest beads release
- [ ] Syncer pod formalization -- configurable via SpireConfig CR, health reporting
- [ ] End-to-end cluster smoke test -- tower attach -> file work -> agent executes -> bead closes
- [ ] Optional ingress for webhook receiver

#### Phase 2 (on top of the runtime contract): spi-sn7o3

`spi-sn7o3` — the WizardGuild repo-cache — is **phase 2 on top of the
spi-xplwy runtime contract**, not a prerequisite for it and not a
blocker for any V1.0 item above. It introduces a new
`WorkspaceOrigin=guild-cache` alongside the existing `local-bind` and
`origin-clone` origins, letting the k8s backend materialize worktrees
from a pre-warmed shared repo cache instead of cloning from origin on
every pod start. Because the `WorkspaceOrigin` type is already
reserved (see `pkg/executor/runtime_contract.go`) and backends already
route by `Workspace.Kind`/`Origin`, adding `guild-cache` is an
additive change: no contract edits, no parity-matrix change, and no
migration for other workstreams.

- [ ] spi-sn7o3 -- WizardGuild repo-cache; materializes workspaces with `Origin=guild-cache` for faster cold-pod start. Ships when spi-xplwy parity is green (it is) and cluster-image bootstrap (above) is stable enough to attach a cache PVC.

### 4. Workshop Skill

The Workshop CLI (`spire workshop`) handles formula authoring, validation,
dry-run, and publishing. Make this accessible as a Claude Code skill so
agents can help humans design, simulate, test, and install formulas.

- [ ] Workshop skill definition -- conversational interface for formula design
- [ ] Formula review -- agent reads a formula, explains the step graph, identifies issues
- [ ] Formula simulation -- dry-run with synthetic bead context, report expected behavior
- [ ] Formula testing -- run test harness, report results
- [ ] Formula installation -- validate and publish to tower or repo
- [ ] Template library -- common patterns (bugfix, feature, epic) as starting points

### 5. Multi-Mode TUI

The board is already highly interactive (navigation, inline actions,
inspector, command mode). The next step is a multi-mode terminal
experience where Tab switches between views. Core motivation: "who is
working what" is not visible enough today.

- [ ] Tab-based mode switching -- Board | Agents | Workshop | Messages | Metrics
- [ ] Agent mode -- who is working what, live status, log streaming, capacity view
- [ ] Workshop mode -- formula browser, inline editing, dry-run, publish
- [ ] Messages mode -- inbox, threaded conversations, send/reply
- [ ] Metrics mode -- DORA metrics, formula performance, cost tracking
- [ ] Mode-aware command palette -- actions contextual to the active mode

### 6. Multi-Backend Agent Support

Spire currently dispatches agents via Claude Code CLI. Support Codex CLI
and Cursor CLI as alternative backends.

- [ ] Backend abstraction -- extract Claude-specific dispatch into a pluggable interface
- [ ] Codex CLI backend -- spawn, monitor, collect results
- [ ] Cursor CLI backend -- spawn, monitor, collect results
- [ ] Backend selection -- per-repo in `spire.yaml`, per-formula in step declarations, per-summon via flag
- [ ] Model mapping -- formula model declarations map to backend-specific model identifiers
- [ ] Result normalization -- all backends produce the same result.json contract

### 7. Observability

The agent_runs table captures per-run metrics (tokens, cost, timing,
code changes, review rounds, formula provenance). But several recording
gaps exist, the DORA metrics display has bugs, and the data that IS
collected isn't surfaced well enough to drive formula tuning or
operational decisions.

**Recording gaps -- data not captured:**

- [ ] Record all phases -- validate-design, enrich-subtasks, auto-approve, skip, and waitForHuman phases never call recordAgentRun; execution time in these phases is invisible
- [ ] Populate timing buckets -- startup_seconds, working_seconds, queue_seconds, review_seconds fields exist in agent_runs but are never written
- [ ] Parent-child run linkage -- ParentRunID is always empty; impossible to trace which wizard spawned which apprentice or reconstruct wave parallelism
- [ ] Per-phase token breakdown -- design vs implement tokens are accumulated together; no way to attribute cost to individual phases
- [ ] Fix build-fix timing -- multiple retry rounds share the same start timestamp, producing inaccurate durations

**Display and query bugs:**

- [ ] Fix DORA deployment frequency -- shows "no successful deployments" despite successful merges landing on main
- [ ] Fix review friction -- shows 0.0 reviews/bead when review rounds are clearly happening
- [ ] Fix change failure rate -- shows 0% which likely undercounts failures
- [ ] Surface per-step timing in metrics views (data exists in agent_runs but isn't shown)

**New metrics to build:**

- [ ] Formula performance comparison -- success rate, cost, and review rounds by formula name + version; answer "is the latest bug-default better than last month's?"
- [ ] Cost breakdown -- per-tower, per-repo, per-formula, per-phase cost attribution
- [ ] Queue time tracking -- time from bead filed to wizard assigned (requires steward coordination)
- [ ] Wave efficiency -- parallel vs sequential execution metrics for epic child dispatch
- [ ] Trend lines -- week-over-week success rate, cost per task, review friction

### 8. CI Pipeline

The existing CI (`ci.yml`) runs build, test, and vet on PRs. Not enough
for a v1.0 gate.

- [ ] Integration tests -- `SPIRE_INTEGRATION=1` env var gates tests that need a live dolt server; CI spins up dolt as a service container
- [ ] Smoke tests -- wire `test/smoke/Dockerfile` into CI; full install-to-bead-close lifecycle
- [ ] Lint -- `golangci-lint` or equivalent
- [ ] Race detection -- `go test -race`
- [ ] Cross-compile -- build all release targets (darwin/linux, amd64/arm64) to catch platform issues
- [ ] Test coverage reporting

---

## Not V1.0

Captured but explicitly deferred:

| Item | Why deferred |
|------|-------------|
| MCP tools / agent-authored tools | Needs observability foundation first |
| Hosted towers (managed compute) | Only pursue if open-source gains adoption |
| GitHub App integration | PAT works for v1; App for multi-org in v2 |
| bd as embedded Go library | Subprocess wrapper + store API ships first (spi-770) |
| Autonomous exploration | Needs trust gradient and guardrails design |
| Field-level merge ownership | Design decided, implementation when multi-machine conflicts are real |

---

## Risk Register

### 1. V2 removal breaks implicit consumers -- LOW

V2 code paths in `cmd/spire/` may have callers that aren't obvious from
the dead code analysis. **Mitigation:** Full test suite after each
removal pass. The analysis identifies all v2 references.

### 2. Steward concurrency under load -- MEDIUM

Adding concurrency limits and wave batching introduces scheduling
complexity: partial waves, agent crashes mid-wave, priority changes
during execution. **Mitigation:** Start with simple `max_concurrent`
limit. Add wave batching only if needed.

### 3. Operator repos-table derivation -- MEDIUM

Switching from explicit WizardGuild CRDs to repos-table derivation
changes the contract. **Mitigation:** Support both modes during
transition. CRDs remain the override mechanism.

### 4. Multi-backend dispatch divergence -- HIGH

Codex and Cursor CLIs have different invocation patterns, output formats,
and failure modes. The result normalization layer may need per-backend
quirks. **Mitigation:** Claude backend is the reference implementation.
Extract the interface, then add Codex/Cursor. Accept that some formula
features may be backend-specific.

### 5. Multi-mode TUI complexity -- MEDIUM

Tab-based mode switching with shared state across modes is significant UI
engineering. **Mitigation:** Ship modes incrementally. Board mode exists.
Add Agent mode first (highest user need), then others.

---

## Decision Log

### Made

| Decision | Rationale |
|----------|-----------|
| V3 graph executor is the only executor | V2 removed in v0.31.0. No backward compatibility. |
| Tower -> repo -> embedded resolution | Tower provides shared team defaults. Repo overrides locally. |
| Recovery as first-class bead type | Recovery beads get structured metadata, dedicated formulas, prior-learning lookup. |
| Formula sharing via dolt | Tower formulas sync automatically via daemon. Full history via dolt. |
| User-first bootstrap | Tower exists before cluster. Developer builds backlog, cluster adopts it. |
| DoltHub as sync layer | No direct laptop-cluster connectivity. Versioned, mergeable, auditable. |
| Single binary | One install, one upgrade, one thing in PATH. |
| Tower-scoped prefixes | Bead IDs globally unique within a tower. |
| Apache 2.0 license | Standard for Go infrastructure. Permissive. Enterprise-compatible. |

### Deferred

| Decision | Depends on | Notes |
|----------|-----------|-------|
| bd as library vs embedded binary | Adoption | Library preferred; subprocess wrapper works |
| GitHub App vs PAT | Post-v1.0 | PAT for v1. App for multi-org. |
| Hosted offering | Post-launch traction | Only if open-source gains adoption |
| MCP tool surface | Observability | Measure formula performance before adding extensibility |
| Merge ownership enforcement | Multiplayer adoption | Design decided; implement when multi-machine conflicts are real |
