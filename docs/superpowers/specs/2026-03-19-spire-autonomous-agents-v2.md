# Spire Autonomous Agents — v2 Architecture

**Date**: 2026-03-19
**Status**: Design
**Supersedes**: spire-autonomous-agents.md (v1 was too naive)

## The pod model

Every agent is a **pod with three containers**:

```
┌─────────────────────────────────────────────────┐
│  Pod: spire-agent-<bead-id>                      │
│                                                  │
│  ┌──────────┐  ┌──────────┐  ┌───────────────┐  │
│  │ sidecar  │  │ worker   │  │ refinery      │  │
│  │          │  │          │  │ (epic pods     │  │
│  │ - inbox  │  │ - claude │  │  only)         │  │
│  │ - ctrl   │  │ - git    │  │               │  │
│  │ - health │  │ - tests  │  │ - PR lifecycle│  │
│  │          │  │          │  │ - merge queue │  │
│  └──────────┘  └──────────┘  └───────────────┘  │
│                                                  │
│  shared: /workspace (emptyDir)                   │
│  shared: /comms (emptyDir — sidecar ↔ worker)    │
└─────────────────────────────────────────────────┘
```

### Container 1: Sidecar (`spire-sidecar`)

Always running. Tiny. The agent's nervous system.

**What it does:**
- Polls `spire collect` on a short interval (10s)
- Maintains an inbox file at `/comms/inbox.json`
- Tracks message state: new, read, queued
- Exposes `/healthz` and `/readyz` endpoints
- Accepts control commands via `/comms/control`:
  - `STOP` — signal worker to abort gracefully
  - `STEER:<message>` — inject a course correction into worker context
  - `PAUSE` — stop accepting new work
  - `RESUME` — resume
- Reports agent status back to the operator via bead updates
- Heartbeat: updates SpireAgent CR `status.lastSeen`

```go
// Sidecar main loop
for {
    messages := spireCollect()
    writeJSON("/comms/inbox.json", messages)

    // Check for control messages (from mayor or human)
    for _, msg := range messages {
        if msg.IsControl() {
            writeFile("/comms/control", msg.Body)
        }
    }

    // Check if worker is still alive
    if !workerAlive() {
        reportStatus("worker-exited")
    }

    // Heartbeat
    updateAgentCR(lastSeen: now())

    sleep(10s)
}
```

**Image**: ~10MB. Just the spire binary + minimal shell. Starts in <1s.

### Container 2: Worker (`spire-worker`)

The actual agent. Does the work, then exits.

**Startup sequence:**
1. Read `/comms/inbox.json` for assignment (or read from env `SPIRE_BEAD_ID`)
2. Clone repo (shallow, specific branch)
3. Install dependencies (detected from package.json / go.mod / requirements.txt)
4. Read bead context via `spire focus`
5. Read spec if linked
6. Execute with Claude (Agent SDK)
7. Run tests
8. Commit to `feat/<bead-id>` branch
9. Push branch
10. Write result to `/comms/result.json`
11. Exit 0 (success) or exit 1 (failure)

**The worker does NOT create PRs.** That's the refinery's job.

**Mid-task steering:**
Worker watches `/comms/control` in a goroutine. On `STEER:<message>`,
it injects the message into the Claude conversation as a user turn.
On `STOP`, it commits whatever it has, pushes, and exits.

```go
// In the agent SDK loop
go func() {
    for {
        if data, err := os.ReadFile("/comms/control"); err == nil {
            os.Remove("/comms/control")
            switch {
            case strings.HasPrefix(string(data), "STEER:"):
                agent.InjectMessage(strings.TrimPrefix(string(data), "STEER:"))
            case string(data) == "STOP":
                agent.Stop()
                commitAndPush()
                os.Exit(0)
            }
        }
        time.Sleep(2 * time.Second)
    }
}()
```

### Container 3: Refinery (epic-level pods only)

Not every pod has a refinery. Only **epic pods** — pods coordinating
multiple child beads — run the refinery container.

**What the refinery does:**
- Watches child bead branches (`feat/<child-bead-id>`)
- When a child's branch is pushed and tests pass:
  - Creates a PR from the feature branch
  - Links PR to the bead
  - Adds PR to the epic's merge queue
- Manages the merge queue:
  - PRs merged in dependency order
  - Conflict detection: if two children touch the same files, rebase
  - Rolls back on test failure after merge
- Reports epic progress: "3/5 children merged, 1 in review, 1 in progress"

```
Epic: spi-abc (New onboarding flow)
  ├── spi-abc.1  feat/spi-abc.1  → PR #42  ✓ merged
  ├── spi-abc.2  feat/spi-abc.2  → PR #43  ⏳ in review
  ├── spi-abc.3  feat/spi-abc.3  → PR #44  ✓ merged
  ├── spi-abc.4  feat/spi-abc.4  (in progress, no PR yet)
  └── spi-abc.5  (blocked by spi-abc.4)
```

**This is better than Gastown's refinery because:**
- One refinery per epic (not one global refinery)
- The refinery has full context of the epic's spec and dependency graph
- It can reorder merges based on actual dependencies, not just FIFO
- It can detect cross-child conflicts early (before merge)
- It scales: 10 epics = 10 refineries, each independent

**Refinery as a standalone pod:**
For large epics, the refinery can run as its own pod (no worker).
It just watches branches and manages PRs. The mayor spins it up
when an epic has >1 active child.

## Pod lifecycle

### Task bead (single unit of work)

```
1. Mayor sees ready bead
2. Operator creates pod:
   - sidecar container (always runs)
   - worker container (runs once, exits)
3. Worker clones, claims, implements, pushes branch
4. Worker exits 0
5. Sidecar detects worker exit
6. Sidecar reports to operator: "work complete, branch pushed"
7. Refinery (epic-level) picks up the branch → creates PR
8. Operator deletes pod after cooldown (5 min for log collection)
```

### Epic bead (coordination)

```
1. Mayor sees ready epic
2. Operator creates epic pod:
   - sidecar container
   - refinery container (no worker — epic pods don't write code)
3. Refinery reads epic structure (bd children, bd graph)
4. For each ready child bead:
   - Mayor assigns to an agent pod (separate pod)
5. As child pods complete:
   - Refinery creates PRs
   - Refinery manages merge queue
6. When all children merged:
   - Refinery reports epic complete
   - bd close <epic-id>
7. Operator deletes epic pod
```

## Repo configuration

Each repo that wants autonomous agents adds a `spire.yaml` at the root:

```yaml
# spire.yaml — repo-level agent configuration
runtime:
  language: typescript           # or go, python, rust
  install: pnpm install          # dependency install command
  test: pnpm test                # test command
  build: pnpm build              # build command (optional)
  lint: pnpm lint                # lint command (optional)

agent:
  model: claude-sonnet-4-6       # default model for this repo
  max-turns: 50                  # safety limit
  timeout: 30m                   # max time per task

branch:
  base: main                     # base branch for PRs
  pattern: "feat/{bead-id}"      # branch naming

pr:
  auto-merge: false              # require human approval
  reviewers: ["jb"]              # default reviewers
  labels: ["agent-generated"]    # labels to add

# Files the agent should always read before starting
context:
  - CLAUDE.md
  - SPIRE.md
  - docs/architecture.md
```

This eliminates the "every repo is different" problem. The agent reads
`spire.yaml` and knows exactly how to install, test, and submit work.

## Scaling

### Scale to zero

- No work → no pods. The mayor only creates pods when beads are ready.
- Sidecar + worker = ephemeral. Pod lives for the duration of one task.
- Epic refineries persist while the epic is active, then scale down.

### Scale up

- More ready beads → more pods. Limited by:
  - k8s node pool size
  - Anthropic API rate limits
  - `SpireConfig.maxConcurrentAgents` (configurable ceiling)
- Mayor respects the ceiling: won't assign more work than the cluster can handle.

### Burst handling

For high-priority work (P0/P1), the mayor can:
- Preempt lower-priority work (send STOP to P3/P4 agents)
- Use the `heavy` token for faster/better model
- Skip the merge queue (refinery fast-tracks P0 PRs)

## Visibility: `spire watch`

```bash
# Watch a single bead
spire watch spi-xyz
# → tails the worker's stdout/stderr from the pod

# Watch an epic
spire watch spi-abc
# → shows all children, their status, agent pods, PRs
# → live updates as work progresses

# Watch everything
spire watch
# → spire board, but live-updating
```

**Implementation**: `kubectl logs -f` for single beads.
For epics, aggregate logs from all child pods + refinery.

```
spire watch spi-abc

EPIC: spi-abc — New onboarding flow (3/5 done)

  ✓ spi-abc.1  Add signup form         PR #42  merged    2m ago
  ✓ spi-abc.3  Email verification      PR #44  merged    5m ago
  ⏳ spi-abc.2  OAuth integration       PR #43  in review
  ◐ spi-abc.4  Profile setup wizard    agent: worker-4   12m elapsed
  ○ spi-abc.5  Welcome email           blocked by spi-abc.4

--- worker-4 (spi-abc.4) ---
  Reading spec...
  Implementing ProfileWizard component...
  Running tests... 14/14 passed
  Committing...
```

## Intervention

### From the CLI

```bash
# Steer an agent
spire steer spi-xyz "use the REST API, not GraphQL"
# → writes STEER: message → sidecar picks it up → injected into claude

# Stop an agent
spire stop spi-xyz
# → writes STOP → worker commits, pushes, exits

# Reassign
spire reassign spi-xyz --to different-agent

# Reprioritize (mayor will adjust)
bd update spi-xyz -p 0
```

### From Slack/GitHub (future)

Comment on a PR: `@spire revise: use the factory from test/helpers`
→ GitHub webhook → new bead → agent addresses it

## Secrets architecture

```
┌─────────────────────────────────────┐
│ k8s Secret: spire-credentials       │
│                                     │
│  DOLT_REMOTE_USER                   │
│  DOLT_REMOTE_PASSWORD               │
│  GITHUB_TOKEN                       │ → mounted in all agent pods
│  ANTHROPIC_API_KEY_DEFAULT          │
│  ANTHROPIC_API_KEY_HEAVY            │ → mounted selectively by token routing
│  ANTHROPIC_API_KEY_CI               │
│  LINEAR_API_KEY                     │ → mayor only
│                                     │
└─────────────────────────────────────┘
```

Token routing: the operator reads `SpireConfig.routing`, matches the bead
properties, selects the token name, and mounts only that key into the pod.

## What we build, in order

### Phase 1: One bead through the pipeline (3 days)

1. **`spire.yaml` schema** — repo config that agents read
2. **`Dockerfile.agent`** — worker image (Agent SDK + git + gh + runtimes)
3. **`spire-sidecar`** — tiny binary (poll collect, write inbox, healthz)
4. **Operator go.mod** — wire controller-runtime, make controllers compile
5. **AgentMonitor: create pods** — when SpireWorkload is assigned, create the pod
6. **End-to-end test**: file bead → pod starts → code written → branch pushed → bead closed

### Phase 2: PR lifecycle + refinery (3 days)

7. **Refinery container** — watch branches, create PRs, manage merge queue
8. **Epic pod template** — sidecar + refinery, no worker
9. **`spire watch`** — tail pod logs, aggregate epic progress
10. **Review feedback**: PR comment → new bead → agent revises

### Phase 3: Production (1 week)

11. **Token routing** in operator
12. **`spire steer` / `spire stop`** — control plane via sidecar
13. **Network policies**
14. **Helm chart**
15. **`spire dashboard`** — terminal UI (bubbletea)
16. **Slack integration** — notify on PR, accept commands

## Open questions

- **Git auth in pods**: Deploy key per repo? GitHub App installation token?
  App tokens are better (short-lived, scoped, rotatable). The operator
  could manage token refresh.

- **Codebase indexing**: Devin indexes the codebase for better context.
  We rely on `spire focus` + spec + CLAUDE.md. Is that enough? Could
  add a codebase summary step in the worker startup.

- **Multi-repo epics**: An epic might span repos (frontend + backend).
  Each child bead targets a different repo. The refinery needs to
  coordinate cross-repo PRs. This is where the mayor's routing
  by prefix becomes critical.

- **Cost controls**: Agent gets stuck in a loop, burns $50 in API calls.
  Need: max-turns limit (in spire.yaml), timeout per task,
  budget alerts, and the sidecar's ability to STOP a runaway agent.

- **Testing in containers**: Some repos need databases, Redis, etc.
  Options: sidecar services in the pod, or a test-infrastructure
  operator that provisions dependencies. Start simple (unit tests only),
  add integration test support later.
