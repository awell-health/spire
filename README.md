# Spire

A wizard's tower for AI agents. Summon capacity, dispatch work, watch it happen.

Spire turns an engineer into the archmage of a tower — you file work, summon wizards, and watch them implement, get reviewed, and merge. You don't write the code. You steer. You review. You make the architecture calls. The tower does the rest.

```
                         ╔═══════════╗
                         ║   SPIRE   ║
                         ╠═══════════╣
                         ║ Archmage  ║  ← you
                         ║ (specs,   ║
                         ║  reviews, ║
                         ║  steers)  ║
                         ╠═══════════╣
                         ║ Steward   ║  ← dispatches work
                         ║ Artificer ║  ← reviews code
                         ╠═══════════╣
                         ║ Wizards   ║  ← write code
                         ║ ░░░░░░░░░ ║
                         ╚═══════════╝
```

## The roles

| Role | What | Binary |
|------|------|--------|
| **Archmage** | You. Files work, writes specs, reviews PRs, steers agents. | — |
| **Steward** | Dispatches beads to idle wizards, monitors staleness, enforces timeouts. | `spire steward` |
| **Wizard** | Writes code in an isolated pod. Clones, claims, implements, pushes a branch. | worker pod |
| **Artificer** | Reviews wizard output against specs using Opus. Creates PRs, manages merge queue. | `spire-artificer` |
| **Familiar** | Sidecar in every pod. Health checks, messaging, real-time status. | `spire-sidecar` |

## Why Spire

AI agents are powerful but isolated. An agent in your frontend repo doesn't know what the agent in your backend repo is doing. They can't coordinate. They can't share context. They don't learn from each other's work.

Spire connects them through a shared graph — [beads](https://github.com/steveyegge/beads) on a [Dolt](https://github.com/dolthub/dolt) database. Agents register, communicate, track work, and stay in sync. The steward assigns work. The artificer reviews it. Everything is versioned, queryable, and durable.

The result: you go from managing one agent at a time to managing a team.

## Quick start

```bash
# Install
brew tap awell-health/tap && brew install spire

# Initialize in your repo
cd my-project && spire init

# Start services
spire up

# File some work
spire file "Add user authentication" -t feature -p 1

# Summon wizards
spire summon 3

# Watch them work
spire watch
```

## The archmage's toolkit

### See what's happening

```bash
spire board                  # kanban columns: Ready → Working → Review → Merged
spire board --epic spi-x2mk  # scoped to one epic
spire watch                  # live tower status
spire watch spi-x2mk         # live epic progress with countdown
spire roster                 # who's in the tower, what they're working on
```

### Manage capacity

```bash
spire summon 3               # conjure 3 wizards
spire summon --for spi-x2mk  # summon enough for this epic's ready children
spire dismiss 1              # send one home
spire roster                 # check capacity
```

### File and structure work

```bash
spire file "Auth system overhaul" -t epic -p 1
spire file "Add OAuth2" -t task -p 2 --parent spi-abc
spire file "Add MFA" -t task -p 2 --parent spi-abc
bd dep add spi-abc.2 spi-abc.1    # MFA depends on OAuth2
```

### Steer and intervene

```bash
spire alert "Check the OAuth implementation" --ref spi-abc.1 --type review -p 1
spire steer spi-abc.1 "use the REST API, not GraphQL"   # course-correct a wizard
spire stop spi-abc.1                                      # abort a wizard
```

## Metrics

Spire measures every phase of a wizard's lifecycle:

```
filed → queued → startup → working → review → merged
```

This gives you [DORA metrics](https://dora.dev) out of the box:

| DORA metric | Spire equivalent |
|-------------|-----------------|
| Deployment frequency | Beads merged per day |
| Lead time | Filed → merged |
| Change failure rate | Failed runs / total |
| Time to restore | Failed → next success |

Two thresholds keep wizards honest:

```yaml
# spire.yaml
agent:
  stale: 10m      # warning — wizard exceeded guidelines
  timeout: 15m    # fatal — tower kills the pod
```

```bash
spire metrics              # summary: today + this week
spire metrics --model      # cost breakdown by model
```

See [docs/metrics.md](docs/metrics.md) for the full metrics reference.

## Configuration

Each repo has a `spire.yaml` that tells wizards how to work:

```yaml
runtime:
  language: typescript
  install: pnpm install
  test: pnpm test
  build: pnpm build
  lint: pnpm lint

agent:
  model: claude-sonnet-4-6
  max-turns: 30
  stale: 10m
  timeout: 15m

branch:
  base: main
  pattern: "feat/{bead-id}"

pr:
  auto-merge: false
  reviewers: ["jb"]
  labels: ["agent-generated"]
```

## Kubernetes

Spire runs on k8s for production workloads. The steward runs as an operator, wizards run as one-shot pods, the artificer runs in epic (workshop) pods.

```bash
# Deploy to minikube
k8s/minikube-demo.sh

# Or manually
kubectl apply -f k8s/namespace.yaml
kubectl apply -f k8s/crds/
kubectl apply -f k8s/steward.yaml
```

See [docs/k8s-architecture.md](docs/k8s-architecture.md) for the full deployment guide.

## Architecture

```
spire/
├── cmd/spire/             # CLI: board, roster, summon, watch, steward, etc.
├── cmd/spire-sidecar/     # Familiar: health, messaging, status endpoint
├── cmd/spire-artificer/   # Artificer: Opus review, PR creation, merge queue
├── operator/              # k8s operator: pod lifecycle, workload assignment
├── pkg/metrics/           # Agent run recording
├── pkg/repoconfig/        # spire.yaml reader
├── k8s/                   # Manifests, CRDs, entrypoints
├── agent-entrypoint.sh    # Wizard pod lifecycle
├── docs/
│   ├── metrics.md         # Metrics reference
│   ├── k8s-architecture.md
│   └── superpowers/specs/ # Design documents
└── spire.yaml             # This repo's agent config
```

| Component | Technology | Role |
|-----------|-----------|------|
| CLI | Go (stdlib) | Single binary — all commands |
| Database | [Dolt](https://github.com/dolthub/dolt) | Git-native SQL — versioned state |
| Work tracking | [beads](https://github.com/steveyegge/beads) | Dependency-aware, agent-optimized |
| Review | Claude Opus (1M context) | Spec-aware code review |
| Orchestration | Kubernetes | Pod lifecycle, scaling |

## The shift

Six months ago you wrote code all day. Now you write specs, review PRs, and steer agents. You didn't become a manager — you became a technical director. You still understand every line. You still catch bugs in review. You still make the architecture calls. But your hands are on the steering wheel, not the keyboard.

Spire is the operating system for that shift.

## License

[Apache License 2.0](LICENSE)
