# Implementation Plan: Spire Open-Source Release

> From minikube prototype to `brew install spire`.

This plan takes Spire from its current state (working k8s operator, steward, wizard/artificer agents on minikube, bd as external binary) to a production-ready open-source project that works locally first and scales to k8s for teams.

---

## Current State (updated 2026-03-22)

What exists today:

- `spire` CLI with ~30 subcommands (Go, single package in `cmd/spire/`)
- `bd` called via `pkg/bd` subprocess wrapper (clean Client interface, callers isolated)
- `spire tower create` and `spire tower attach` — full tower bootstrap and second-machine attach
- `spire register-repo` — writes to dolt `repos` table, validates prefix uniqueness, pushes to DoltHub
- Credential management — `~/.config/spire/credentials` (chmod 600), env var overrides
- Dolt lifecycle — auto-download binary, managed server start/stop, version pinning
- Local dolt server lifecycle managed by `spire up/down/shutdown`
- Tower configs stored in `~/.config/spire/towers/<name>.json`
- Instance config in `~/.config/spire/config.json` (local cache, dolt is source of truth)
- k8s operator with CRDs: SpireAgent, SpireWorkload, SpireConfig
- Steward runs as k8s deployment or local process via `spire steward`
- Wizard/artificer agents run as k8s pods (managed by operator)
- goreleaser config and GitHub Actions CI (build, test, release on tag)
- `spire doctor` with 10 checks in 3 categories
- `spire push` / `spire pull` with credential injection

What works well and should not change:

- The steward cycle (assess, assign, stale-check, push)
- Bead-based messaging (`spire send/collect`)
- `spire.yaml` repo config with runtime auto-detection
- Operator CRD design (SpireAgent, SpireWorkload)
- DoltHub as sync intermediary

What remains in Phase 1:

- `bd` as embedded Go library (deferred — subprocess wrapper ships first, can run in parallel with Phase 2)

---

## Phase 1: Foundation

**Goal:** `brew install spire && spire tower create && spire file "task" -t task -p 2` works end-to-end on a clean machine.

### 1.1 Embed bd into spire binary

The highest-risk item. Currently `bd` is a separate Go binary in the beads repo, called via `exec.Command("bd", args...)` from ~15 call sites in spire.

**Approach:** Import bd as a Go library. The beads repo already has a clean package structure. Extract the core into an importable `pkg/beads` package with a programmatic API, then call it directly instead of shelling out.

Work items:
- [x] **Shipped:** `pkg/bd` subprocess wrapper with `Client` struct, `BeadsDir`, `RunDir` fields — callers are fully isolated behind clean interfaces
- [ ] In the beads repo, extract `pkg/beads` with functions: `Create()`, `List()`, `Show()`, `Update()`, `Close()`, `Ready()`, `Count()`, `Status()`, `Export()`, `Import()`, `DoltSQL()`, `VCCommit()`, `VCStatus()`
- [ ] Add `go.mod` dependency: `github.com/awell-health/beads`
- [ ] Replace all `bd()`, `bdJSON()`, `bdSilent()` calls in `cmd/spire/bd.go` with direct library calls
- [ ] Remove the subprocess wrapper; keep the same function signatures for minimal diff
- [ ] Ensure `spire version` reports both spire and embedded bd versions
- [ ] Fallback: if library extraction proves too invasive, bundle the `bd` binary inside the spire binary using `embed` and extract to a temp dir at runtime. This is worse but ships faster.

**Risk:** bd's internal state management (database connections, caching) may not compose cleanly as a library. Spike this first -- spend 2 days on a proof-of-concept before committing to the approach.

**Status (2026-03-22):** Deferred. The subprocess wrapper means zero callers change when this lands. Can run in parallel with Phase 2.

### 1.2 `spire tower create`

New command. Replaces the current manual bootstrap (run `bd init`, configure dolt, push to DoltHub, set up config).

```
spire tower create --name my-team [--dolthub org/repo]
```

Work items:
- [x] Initialize dolt database with beads schema (calls embedded bd)
- [x] Generate tower identity: `project_id` (UUID), `name`, auto-assigned hub prefix
- [x] Write tower metadata to dolt `metadata` table
- [x] If `--dolthub` provided: create DoltHub repo (reuse `ensureDoltHubDB()`), set remote, push
- [x] Write tower config to `~/.config/spire/towers/<name>.json`
- [x] Create `repos` table in dolt (see 1.5)
- [x] Update `~/.config/spire/config.json` to reference the tower

**Status (2026-03-22):** Complete.

### 1.3 `spire tower attach`

New command. Second developer joins an existing tower.

```
spire tower attach <dolthub-url> [--name local-name]
```

Work items:
- [x] Clone database from DoltHub using dolt CLI directly
- [x] Read tower identity from cloned database (raw dolt queries, no ambient db context)
- [x] Bootstrap `.beads/` in cloned data dir (metadata.json + config.yaml)
- [x] Write local tower config
- [x] Start local dolt server if not running
- [x] Print tower summary (name, prefix, repo count, bead count)

**Status (2026-03-22):** Complete.

### 1.4 Credential management

Replace scattered env vars with a structured credential store. Credentials live in `~/.config/spire/credentials` (chmod 600, not JSON -- flat key=value file). Environment variables override file values, so CI/CD pipelines can inject secrets without touching the filesystem.

```
spire config set anthropic-key sk-...
spire config set github-token ghp_...
spire config set dolthub-user myuser
spire config set dolthub-password dolt_token_...
```

Work items:
- [x] Store credentials in `~/.config/spire/credentials` (chmod 600, flat key=value format)
- [x] Read credentials from file; env vars override file values (not fallback -- override)
- [x] `spire config get <key>` reads a credential (masked by default, `--unmask` to show)
- [x] `spire config list` shows all configured credentials (masked)
- [x] Inject credentials into agent environments (Docker, process, k8s secrets)
- [x] CI/CD pattern: set `SPIRE_ANTHROPIC_KEY`, `SPIRE_GITHUB_TOKEN`, etc. -- no file needed

**Status (2026-03-22):** Complete.

### 1.5 Repos table in dolt

Move repo registration from local-only config (`~/.config/spire/config.json`) into the shared dolt database. The `repos` table in dolt is THE source of truth for all repo registrations -- the operator reads it to auto-create SpireAgent CRDs, and all tower participants see the same repos.

Schema:
```sql
CREATE TABLE repos (
    prefix       VARCHAR(16) PRIMARY KEY,
    repo_url     VARCHAR(512) NOT NULL,
    branch       VARCHAR(128) NOT NULL DEFAULT 'main',
    language     VARCHAR(32),
    registered_by VARCHAR(64),
    registered_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
```

Work items:
- [x] Create `repos` table in `spire tower create`
- [x] `spire register-repo` writes to dolt `repos` table (not just local config)
- [x] Validate prefix uniqueness against shared state
- [x] Keep local config as a cache/overlay -- read from dolt, write to both
- [ ] Migration: `spire doctor --fix` migrates existing local-only registrations to dolt (see 1.9)

**Status (2026-03-22):** Complete (except doctor --fix migration, tracked in 1.9).

### 1.6 `spire register-repo`

Rework of the current `spire init` flow. `spire init` currently does too many things (shell injection, prefix prompting, role selection, worktree setup). Split it:

- `spire tower create` -- creates the tower (new)
- `spire register-repo` -- registers a repo under the tower (extracted from init)
- `spire init` -- convenience wrapper that runs both if needed

```
spire register-repo [--prefix web] [--repo-url https://github.com/org/repo]
```

Work items:
- [x] Auto-detect prefix from directory name (existing logic)
- [x] Auto-detect repo URL from git remote
- [x] Auto-detect runtime from `spire.yaml` or file detection
- [x] Write to dolt `repos` table (via bdpkg.Client with explicit tower BeadsDir)
- [x] Write `.beads/` directory in repo root
- [x] Generate `spire.yaml` if missing (existing logic in `repoconfig.GenerateYAML`)
- [x] Push registration to DoltHub
- [x] Resolve tower identity from database, not ActiveTower

**Status (2026-03-22):** Complete.

### 1.7 Dolt lifecycle management

Spire manages the dolt binary -- the user does NOT run `brew install dolt` separately. Dolt is a managed dependency, not an embedded one: spire auto-downloads the correct dolt binary if not present, and manages the server start/stop lifecycle.

Work items:
- [x] On first run (or `spire doctor --fix`), auto-download dolt binary to `~/.local/share/spire/bin/dolt`
- [x] Platform detection: download correct binary for darwin/linux, amd64/arm64
- [x] Version pinning: spire knows which dolt version it requires, downloads that specific version
- [x] `spire up` starts dolt server using the managed binary (existing behavior, but now from managed path)
- [x] `spire doctor` checks dolt version, offers to upgrade if stale

**Status (2026-03-22):** Complete.

### 1.8 Homebrew formula and release pipeline

Work items:
- [x] goreleaser config: cross-compile for darwin/linux, amd64/arm64
- [x] GitHub Actions workflow: test on push, release on tag
- [x] Homebrew tap: `awell-health/homebrew-spire` repo created; goreleaser `.goreleaser.yml` points at it
- [x] Formula installs `spire` binary only -- dolt is auto-managed, not a Homebrew dependency
- [x] `spire version` prints spire version + managed dolt version and path (or "not installed")
- [x] SHA256 checksums in release artifacts

**Status (2026-03-22):** Complete. Tap repo created, goreleaser reconciled (duplicate `.goreleaser.yaml` removed, `.goreleaser.yml` v2 is canonical), `spire version` prints both versions.

### 1.9 `spire doctor` expansion

`spire doctor` already exists. Expand it to validate the full local setup.

- [x] Check dolt installed and correct version (managed binary, not system-installed)
- [x] Check tower config exists and points to valid database
- [x] Check tower .beads/ data dir exists (metadata.json + config.yaml)
- [x] Check credentials configured (anthropic, github, dolthub)
- [x] Check credential file permissions (0600)
- [x] Check Docker available (for agent spawning)
- [x] `--fix` flag auto-repairs: download dolt binary, start dolt server, fix credential perms, regenerate .beads/

**Status (2026-03-22):** Complete. 11 checks in 3 categories. `--fix` auto-repairs system and tower issues.

---

## Phase 2: Local Agent Execution

**Goal:** `spire up` spawns a steward that assigns work to agents running as Docker containers or local processes on the developer's laptop.

### 2.1 Local steward adaptation

The steward (`cmd/spire/steward.go`) already runs locally via `spire steward`. But it currently tries to load the roster from k8s SpireAgent CRs first, falling back to bead registrations. For local mode, it needs to manage agent lifecycle directly.

Work items:
- [ ] Add `--mode=local` flag (default when not in k8s)
- [ ] In local mode, read agent config from `spire.yaml` or tower config instead of k8s CRs
- [ ] Track running agents via PID files or container IDs (similar to dolt server tracking)
- [ ] Integrate agent spawning into the steward cycle: after assigning a bead, start the agent

### 2.2 Docker agent spawning

The primary local execution mode. Each wizard task gets its own container.

Work items:
- [ ] Default agent image: publish `ghcr.io/awell-health/spire-agent:latest`
- [ ] Agent image includes: Go, Node, Python, git, dolt, bd, spire, claude-code CLI
- [ ] Steward creates container per assignment: pass repo URL + branch, inject credentials (no host mount of local repo)
- [ ] Agent clones the repo inside the container -- workspace is ephemeral, agent pushes results via git
- [ ] Container entrypoint: `git clone <repo-url> -b <branch> /workspace && cd /workspace && spire focus <bead-id> && claude-code --task "$(spire focus <bead-id> --prompt)"`
- [ ] Container lifecycle: start, poll for completion, collect exit code, cleanup
- [ ] Timeout enforcement: kill container after `agent.timeout` from `spire.yaml`
- [ ] Log collection: capture container stdout/stderr, store in tower (or just filesystem)

### 2.3 Process agent spawning

Alternative to Docker for faster iteration during development.

```
spire up --mode=process
```

Work items:
- [ ] Spawn `claude` CLI as subprocess with appropriate flags
- [ ] Set working directory to repo root
- [ ] Inject credentials via environment
- [ ] PID tracking and timeout enforcement
- [ ] Process mode is explicitly opt-in (Docker is default)

### 2.4 `spire status` and `spire logs`

Make the local experience observable.

Work items:
- [ ] `spire status`: dolt server state, steward state, active agents (with bead IDs), last sync time, work queue summary
- [ ] `spire logs [agent-name]`: tail agent output, filter by agent or bead ID
- [ ] `spire logs --steward`: tail steward output
- [ ] Status data written to `~/.local/share/spire/` (existing pattern from `doltGlobalDir()`)

### 2.5 Integrate `spire up` with steward

Currently `spire up` starts dolt + daemon (webhook/Linear sync). Add steward to the lifecycle.

Work items:
- [ ] `spire up` starts: dolt server, sync daemon, steward
- [ ] `spire down` stops: steward, daemon (dolt stays for other repos)
- [ ] `spire shutdown` stops: steward, daemon, dolt
- [ ] Steward PID tracked alongside daemon PID in `doltGlobalDir()`
- [ ] `spire up --no-agents` starts without steward (for manual work only)

---

## Phase 3: Sync and Multiplayer

**Goal:** Dev A and Dev B both run `spire tower attach`, file work, and see each other's beads and agent results.

### 3.1 `spire pull`

Planned command. `spire sync` exists but is too heavy (handles divergent histories, hard resets, merging). `spire pull` is the clean counterpart to `spire push`. Canonical command surface: `tower create`, `tower attach`, `register-repo`, `push`, `pull`. No `sync` as primary, no `init` as primary.

Work items:
- [ ] `spire pull`: wrapper around dolt pull with credential injection
- [ ] Use CLI-based pull (like `doltCLIPush` but for pull) to inherit env credentials
- [ ] Handle non-fast-forward gracefully: suggest `spire sync --merge`
- [ ] Background daemon calls `spire pull` + `spire push` on interval

### 3.2 Background sync daemon

Rework the existing daemon to handle bidirectional DoltHub sync (currently only does webhook/Linear processing).

Work items:
- [ ] Add pull-from-DoltHub step to `runCycle()` in `daemon.go`
- [ ] Add push-to-DoltHub step after pull
- [ ] Configurable interval (existing `--interval` flag)
- [ ] Report sync status in `spire status` (last pull time, last push time, sync errors)
- [ ] Conflict detection: log warnings when merge produces unexpected results

### 3.3 Merge ownership enforcement

**Core design decision (decided, needs implementation).** This is not future work or speculative -- it is the foundation of the sync model. Field-level ownership determines who wins on merge conflicts:

- **Cluster owns status fields:** `status`, `owner`, `assignee`. The cluster/daemon is authoritative. Stale local state must never regress these.
- **User owns content fields:** `title`, `description`, `priority`, `type`. The user is authoritative. Cluster never overwrites these.
- **Append-only fields:** `comments`, `messages`. Both sides append. No overwrites, no deletes during merge.

Work items:
- [ ] Annotate each column in the beads schema with its ownership class (status/content/append-only)
- [ ] Implement post-merge fixup: after pull, scan for status regressions and restore cluster values for status fields
- [ ] Preserve user values for content fields when cluster has a stale copy
- [ ] Append-only merge for comments and messages (union of both sides, ordered by timestamp)
- [ ] Test harness: concurrent writers modifying the same bead, verify correct field wins per ownership class

### 3.4 Prefix uniqueness enforcement

Prevent two developers from registering the same prefix.

Work items:
- [x] `spire register-repo` checks repos table before writing
- [x] Check `repos` table for existing prefix
- [x] If conflict: clear error message with the conflicting repo URL
- [x] Race condition resolution: first-push-wins (dolt merge detects the duplicate row, reject on push)

**Status (2026-03-22):** Complete. Shipped as part of Phase 1 (spi-n1aa.1).

---

## Phase 4: Production Cluster

**Goal:** `helm install spire` deploys a working cluster that adopts an existing tower from DoltHub.

> **Note:** Cluster-first bootstrap is explicitly out of scope for v1. The flow is: user creates tower locally (`tower create`) -> builds backlog -> `helm install` attaches to the existing tower. The cluster never creates a tower from scratch.

### 4.1 Helm chart

Replace the current kustomize manifests in `k8s/` with a proper Helm chart.

Work items:
- [ ] `charts/spire/` with `Chart.yaml`, `values.yaml`, templates
- [ ] `values.yaml` inputs: dolthub URL, dolthub credentials secret, anthropic key secret, github token secret, tower name
- [ ] Bootstrap job: `spire tower attach <dolthub-url>` (not create -- tower already exists)
- [ ] Deployments: dolt-server, steward, operator, syncer
- [ ] CRDs: SpireAgent, SpireWorkload, SpireConfig (existing definitions in `operator/api/v1alpha1/`)
- [ ] PVCs: dolt data, beads seed
- [ ] Service accounts and RBAC
- [ ] Optional ingress for webhook receiver

### 4.2 Operator reads repos table

The dolt `repos` table is THE source of truth for repo registration. The operator reads it and auto-creates agent configurations. SpireAgent CRDs are derived from the repos table, not manually managed.

Work items:
- [ ] Operator polls dolt `repos` table on interval (or watches via dolt diff)
- [ ] New repo in `repos` table -> operator auto-creates SpireAgent CR (derived, not manually authored)
- [ ] Agent image determined by `language` field in repos table (or per-repo override in `spire.yaml`)
- [ ] No manual SpireAgent YAML -- all agent CRs are operator-managed, sourced from `repos` table
- [ ] Reconcile loop: repo removed from table -> operator marks agent offline and cleans up CR
- [ ] Operator labels managed CRs (e.g., `spire.io/managed-by=repos-table`) to distinguish from any manual overrides

### 4.3 Cluster-local dolt syncs with DoltHub

Formalize the syncer pod pattern (currently ad-hoc in `k8s/syncer.yaml`).

Work items:
- [ ] Syncer runs `spire pull` + `spire push` on interval
- [ ] Configurable via SpireConfig CR (interval, remote URL)
- [ ] Health checks: syncer reports last-sync time to SpireConfig status
- [ ] Handles credential rotation (reads from k8s secret on each cycle)

### 4.4 Agent image registry

Work items:
- [ ] Default agent image published to `ghcr.io/awell-health/spire-agent`
- [ ] Image includes: standard toolchains, spire binary, claude-code CLI
- [ ] Per-repo custom images specified in `repos` table or SpireAgent CR
- [ ] Operator pulls image spec from agent config

---

## Phase 5: Polish and Launch

**Goal:** Public GitHub release with documentation, CI, and a clear getting-started path.

### 5.1 README.md

- [ ] One-paragraph pitch (from VISION.md)
- [ ] 5-command quickstart (tower create, register-repo, file, up, watch)
- [ ] Architecture diagram (ASCII or linked SVG)
- [ ] Links to docs, contributing guide, license

### 5.2 Documentation

- [ ] Getting started guide (laptop setup, first task, first PR)
- [ ] Concepts: towers, beads, agents, prefixes, sync model
- [ ] CLI reference (auto-generated from help text)
- [ ] `spire.yaml` configuration reference
- [ ] Cluster deployment guide (Helm)
- [ ] Agent development guide (how to build custom agents)
- [ ] Troubleshooting and FAQ

### 5.3 License and contributing

- [ ] Apache 2.0 license (standard for Go infrastructure projects)
- [ ] CONTRIBUTING.md with development setup, PR process, code style
- [ ] DCO (Developer Certificate of Origin) sign-off requirement

### 5.4 CI/CD

- [ ] GitHub Actions: lint, test, build on every push
- [ ] goreleaser on tag push: build binaries, create GitHub release, update Homebrew tap
- [ ] Container image build and push to ghcr.io on release
- [ ] Helm chart publish to OCI registry or GitHub Pages

### 5.5 Demo

- [ ] Terminal recording (asciinema or vhs): install, tower create, register repo, file task, agent opens PR
- [ ] Under 60 seconds
- [ ] Embedded in README

---

## Dependency Graph

```
Phase 1 (Foundation)
  1.1 embed bd ─────┐
                     ├─> 1.2 tower create ──> 1.3 tower attach
  1.4 credentials ──┘         │
                              v
                        1.5 repos table ──> 1.6 register-repo
                              │
  1.7 dolt lifecycle ─────────┘ (needed by tower create)
  1.8 homebrew ───────────────── (parallel, needs 1.1 + 1.7)
  1.9 doctor ───────────────── (parallel)

Phase 2 (Local Execution) — depends on Phase 1 complete
  2.1 local steward ─┬─> 2.2 docker spawn
                     ├─> 2.3 process spawn
                     └─> 2.5 integrate with spire up
  2.4 status/logs ──── (parallel)

Phase 3 (Multiplayer) — depends on Phase 1 complete, Phase 2 optional
  3.1 spire pull ────> 3.2 background sync
  3.3 merge ownership ── (parallel)
  3.4 prefix uniqueness ── (depends on 1.5)

Phase 4 (Cluster) — depends on Phase 1, Phase 3
  4.1 helm chart ────> 4.2 operator repos table
                  ────> 4.3 cluster sync
                  ────> 4.4 image registry

Phase 5 (Launch) — depends on Phase 1, Phase 2
  All items parallel. Can start README/docs during Phase 2.
```

Phases 2 and 3 can run in parallel after Phase 1 completes. Phase 4 depends on the sync model being solid (Phase 3). Phase 5 starts incrementally as features land.

---

## Risk Register

### 1. bd embedding (Phase 1.1) -- HIGH

The entire plan depends on shipping a single binary. If bd cannot be cleanly extracted as a Go library, the fallback (embedding the binary via `go:embed`) adds complexity to the build and a runtime extraction step. **Mitigation:** Spike the library extraction in a 2-day timebox before committing to the full plan.

### 2. Dolt merge semantics (Phase 3.3) -- MEDIUM

Multi-writer conflicts on the same bead are the hardest sync problem. The ownership model is decided (cluster owns status, user owns content, append-only for comments/messages), but dolt's three-way merge may not handle field-level ownership rules natively. **Mitigation:** Build a test harness with two concurrent writers early in Phase 1. Implement post-merge fixup: read both sides, apply ownership rules per field class, write winner.

### 3. Agent reliability in local mode (Phase 2.2) -- MEDIUM

k8s handles restarts, health checks, and resource limits for agent pods. Local Docker mode needs equivalent resilience built from scratch. **Mitigation:** Start with simple timeout-and-kill. Add health checks and restart logic iteratively. Process mode (2.3) is the escape hatch for debugging.

### 4. DoltHub rate limits (Phase 3.2) -- LOW

Frequent push/pull from multiple developers could hit DoltHub fair-use limits. **Mitigation:** Default sync interval is 2 minutes (existing). Document DoltHub's limits. Add exponential backoff on 429 responses.

### 5. Agent image size (Phase 2.2, 4.4) -- LOW

An image with Go, Node, Python, git, dolt, and claude-code will be large (>2GB). **Mitigation:** Multi-stage build. Offer slim variants per language. Local process mode avoids the image entirely.

---

## Decision Log

Decisions that are already made and should not be revisited:

| Decision | Rationale |
|----------|-----------|
| User-first bootstrap | Tower exists before cluster. Developer builds backlog, cluster adopts it. |
| DoltHub as sync layer | No direct connectivity between laptop and cluster. Versioned, mergeable, auditable. |
| Explicit sync (push/pull) | Developer controls when state moves. Daemon is convenience, not requirement. |
| Single binary | One install, one upgrade, one thing in PATH. |
| Tower-scoped prefixes | Bead IDs are globally unique within a tower. No cross-tower conflicts. |
| Merge ownership | Cluster owns status, user owns content. Prevents status regressions from stale local state. |
| Apache 2.0 license | Standard for Go infrastructure. Permissive. Compatible with enterprise use. |

Decisions that are deferred:

| Decision | Depends on | Notes |
|----------|-----------|-------|
| bd as library vs embedded binary | Phase 1.1 spike | Library is preferred. Embedded binary is fallback. |
| GitHub App vs PAT | Phase 5 or later | PAT for v1. GitHub App for multi-org access in v2. |
| Hosted offering | Post-launch traction | Only pursue if open-source gains adoption. |
