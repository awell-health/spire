# Spire Autonomous Agents — Full Design

**Date**: 2026-03-19
**Status**: Design
**Goal**: A self-hosted, open-source alternative to Devin. Agents that receive work,
clone repos, write code, run tests, create PRs, and respond to review feedback —
orchestrated by spire, running anywhere (local, k8s, or CI).

## What this replaces

Devin charges $500/mo for an AI that:
1. Takes a task
2. Plans an approach
3. Writes code in a sandbox
4. Runs tests
5. Creates a PR
6. Addresses review comments

We already have most of these pieces. This doc connects them into a
fully autonomous pipeline.

## End-to-end flow

```
Human files a bead
       │
       ▼
Mayor sees it ready ──────────────── (k8s operator or local daemon)
       │
       ▼
Mayor assigns to agent ──────────── spire send <agent> "claim <bead>"
       │
       ▼
Agent pod starts ─────────────────── (managed) or hook fires (external)
       │
       ▼
Clone repo ───────────────────────── git clone --depth=1
       │
       ▼
spire claim <bead> ───────────────── atomic: pull → verify → set in_progress → push
       │
       ▼
spire focus <bead> ───────────────── assemble context: spec, deps, parent epic
       │
       ▼
Read spec ────────────────────────── docs/superpowers/specs/<spec>.md
       │
       ▼
Plan ─────────────────────────────── claude generates implementation plan
       │
       ▼
Implement ────────────────────────── claude writes code on feat/<bead-id> branch
       │
       ▼
Test ─────────────────────────────── run test suite, fix failures
       │
       ▼
Create PR ────────────────────────── gh pr create, link to bead
       │
       ▼
bd close <bead> + bd dolt push ───── mark work as done
       │
       ▼
Wait for review ──────────────────── human reviews PR
       │
       ▼
Review feedback? ─────────────────── agent gets notified, addresses comments
       │
       ▼
Merge ────────────────────────────── human merges (or auto-merge if trusted)
```

## Components

### 1. Agent image (`Dockerfile.agent`)

A container that can execute work autonomously. Contains:

```
spire            — work coordination
bd               — beads CLI
dolt             — database engine
claude           — Claude Code CLI (or Agent SDK)
gh               — GitHub CLI (for PR creation)
git              — source control
node/go/python   — language runtimes (configurable per project)
```

Two variants:
- **claude-code agent**: runs `claude` CLI in headless/print mode
- **agent-sdk agent**: runs a Go/Python program using the Agent SDK directly

The claude-code variant is simpler and reuses all existing skills/hooks.
The agent-sdk variant is more controllable and cheaper (no CLI overhead).

### 2. Agent entrypoint (`agent-entrypoint.sh`)

The script that runs when an agent pod starts:

```bash
#!/usr/bin/env bash
set -e

# --- Identity ---
AGENT_NAME="${SPIRE_AGENT_NAME:?must be set}"
BEAD_ID="${SPIRE_BEAD_ID:?must be set}"
REPO_URL="${SPIRE_REPO_URL:?must be set}"
REPO_BRANCH="${SPIRE_REPO_BRANCH:-main}"

# --- Setup ---
git clone --depth=1 --branch "$REPO_BRANCH" "$REPO_URL" /workspace
cd /workspace

# Initialize spire (standalone, syncs from DoltHub)
spire init --prefix=agent --standalone
spire sync --hard "$DOLTHUB_REMOTE"

# Register this agent
spire register "$AGENT_NAME"

# --- Work ---
# Claim the bead
spire claim "$BEAD_ID"

# Focus (assembles context: spec, deps, parent)
CONTEXT=$(spire focus "$BEAD_ID")

# Create feature branch
git checkout -b "feat/$BEAD_ID"

# --- Execute with Claude ---
# Option A: Claude Code CLI (headless)
claude --print --system-prompt "$CONTEXT

You are an autonomous coding agent. Your task:
$(bd show "$BEAD_ID")

Instructions:
1. Read the spec if one exists (check docs/superpowers/specs/)
2. Implement the changes
3. Run tests: determine the test command from package.json, Makefile, or go.mod
4. Fix any test failures
5. Commit with message: feat($BEAD_ID): <summary>

Do NOT create a PR — the entrypoint handles that.
Work on branch feat/$BEAD_ID.
When done, output AGENT_DONE on its own line."

# Option B: Agent SDK (direct API)
# spire-agent-worker --bead "$BEAD_ID" --context "$CONTEXT"

# --- PR ---
git push origin "feat/$BEAD_ID"

PR_URL=$(gh pr create \
    --title "feat($BEAD_ID): $(bd show "$BEAD_ID" --json | jq -r .title)" \
    --body "## Bead: $BEAD_ID

$(bd show "$BEAD_ID")

---
🤖 Autonomous agent: $AGENT_NAME
" \
    --base "$REPO_BRANCH" \
    --head "feat/$BEAD_ID")

echo "PR created: $PR_URL"

# Add PR link to bead
bd comment "$BEAD_ID" "PR: $PR_URL"

# --- Close ---
bd close "$BEAD_ID"
bd dolt push
spire send mayor "Completed $BEAD_ID: $PR_URL" --ref "$BEAD_ID"
```

### 3. Review feedback loop

When a human reviews the PR and leaves comments, the agent needs to respond.

**How it works:**

1. GitHub webhook fires on PR review comment → hits `spire serve` (or a GitHub Action)
2. Webhook handler creates a new bead: "Address review feedback on <bead-id>"
3. Mayor assigns it to an agent (same one if available, or a new one)
4. Agent starts, clones, checks out the PR branch, reads the review comments
5. Agent addresses feedback, pushes, comments on the PR

**GitHub Action approach** (simpler, no webhook server needed):

```yaml
# .github/workflows/spire-review-agent.yml
name: Spire Review Agent
on:
  pull_request_review:
    types: [submitted]

jobs:
  address-review:
    if: github.event.review.state == 'changes_requested'
    runs-on: ubuntu-latest
    container:
      image: ghcr.io/awell-health/spire-agent:latest
    env:
      ANTHROPIC_API_KEY: ${{ secrets.ANTHROPIC_API_KEY }}
      GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
    steps:
      - uses: actions/checkout@v4
        with:
          ref: ${{ github.event.pull_request.head.ref }}

      - name: Address review feedback
        run: |
          # Extract bead ID from PR branch name (feat/<bead-id>)
          BEAD_ID=$(echo "${{ github.event.pull_request.head.ref }}" | sed 's|feat/||')

          # Get review comments
          COMMENTS=$(gh pr view ${{ github.event.pull_request.number }} \
              --json reviews --jq '.reviews[-1].body')

          # Run claude to address feedback
          claude --print --system-prompt "
          You are addressing PR review feedback.

          Bead: $BEAD_ID
          PR: #${{ github.event.pull_request.number }}
          Branch: ${{ github.event.pull_request.head.ref }}

          Review comments to address:
          $COMMENTS

          Fix the issues, commit, and push. Do not create a new PR."

          git push
```

### 4. Operator changes (SpireAgent for managed agents)

The operator's `AgentMonitor` creates pods for managed agents. Updates needed:

**Pod spec for agent work:**

```yaml
# Generated by the operator when assigning work to a managed agent
apiVersion: v1
kind: Pod
metadata:
  name: spire-agent-<agent-name>-<bead-id>
  namespace: spire
  labels:
    spire.awell.io/agent: <agent-name>
    spire.awell.io/bead: <bead-id>
spec:
  restartPolicy: Never  # one-shot: do the work, exit
  initContainers:
    - name: clone
      image: alpine/git:latest
      command: ["git", "clone", "--depth=1", "<repo-url>", "/workspace"]
      volumeMounts:
        - name: workspace
          mountPath: /workspace
  containers:
    - name: agent
      image: ghcr.io/awell-health/spire-agent:latest
      env:
        - name: SPIRE_AGENT_NAME
          value: "<agent-name>"
        - name: SPIRE_BEAD_ID
          value: "<bead-id>"
        - name: SPIRE_REPO_URL
          value: "<repo-url>"
        - name: ANTHROPIC_API_KEY
          valueFrom:
            secretKeyRef:
              name: spire-credentials
              key: <resolved-token-key>
        - name: GITHUB_TOKEN
          valueFrom:
            secretKeyRef:
              name: spire-credentials
              key: GITHUB_TOKEN
        - name: DOLTHUB_REMOTE
          value: "<from SpireConfig>"
        - name: DOLT_REMOTE_USER
          valueFrom:
            secretKeyRef:
              name: spire-credentials
              key: DOLT_REMOTE_USER
        - name: DOLT_REMOTE_PASSWORD
          valueFrom:
            secretKeyRef:
              name: spire-credentials
              key: DOLT_REMOTE_PASSWORD
      workingDir: /workspace
      volumeMounts:
        - name: workspace
          mountPath: /workspace
  volumes:
    - name: workspace
      emptyDir: {}
```

**Key difference from Devin**: the pod is ephemeral. One bead = one pod.
No long-lived sandbox. Fresh clone, clean state, no accumulated cruft.

### 5. Agent SDK worker (Go alternative to Claude CLI)

For production, a Go program that uses the Anthropic Agent SDK directly:

```go
// cmd/spire-agent-worker/main.go
package main

import (
    "context"
    "os"
    "os/exec"

    "github.com/anthropics/claude-agent-sdk-go"
)

func main() {
    beadID := os.Getenv("SPIRE_BEAD_ID")
    agentName := os.Getenv("SPIRE_AGENT_NAME")

    // 1. Claim
    exec.Command("spire", "claim", beadID).Run()

    // 2. Focus (get context)
    focusOut, _ := exec.Command("spire", "focus", beadID).Output()

    // 3. Create branch
    exec.Command("git", "checkout", "-b", "feat/"+beadID).Run()

    // 4. Run agent
    agent := claude.NewAgent(claude.AgentConfig{
        Model: "claude-sonnet-4-6",
        System: string(focusOut) + "\n\nImplement the task. Run tests. Commit.",
        Tools: []claude.Tool{
            claude.BashTool(),
            claude.FileEditTool(),
            claude.FileReadTool(),
        },
        MaxTurns: 50,
    })

    result, err := agent.Run(context.Background(), "Begin work on "+beadID)
    if err != nil {
        // Report failure
        exec.Command("spire", "send", "mayor",
            "Failed "+beadID+": "+err.Error(), "--ref", beadID).Run()
        os.Exit(1)
    }

    // 5. Push + PR
    exec.Command("git", "push", "origin", "feat/"+beadID).Run()
    exec.Command("gh", "pr", "create",
        "--title", "feat("+beadID+"): "+result.Summary,
        "--body", "Bead: "+beadID+"\n\n"+result.Summary,
    ).Run()

    // 6. Close
    exec.Command("bd", "close", beadID).Run()
    exec.Command("bd", "dolt", "push").Run()
    exec.Command("spire", "send", "mayor",
        "Completed "+beadID, "--ref", beadID).Run()
}
```

### 6. Security model

Agents run code. They need guardrails.

**Git access:**
- Agents push to feature branches only (`feat/<bead-id>`)
- Branch protection prevents direct push to main
- PRs require human approval before merge

**Secrets:**
- Anthropic keys: per-token, scoped via k8s Secrets
- GitHub token: scoped to repo, PR creation + push only
- DoltHub: read-write to beads database only
- No access to production databases, cloud consoles, or other infra

**Sandboxing:**
- Each agent runs in its own pod (k8s namespace isolation)
- No persistent storage (emptyDir volumes, wiped on exit)
- Resource limits enforced (CPU, memory)
- Network policy: only GitHub, DoltHub, Anthropic API (optional)

**Audit:**
- Every bead has a full history (who claimed, what was done, when)
- Every PR links back to a bead
- Agent commits use a distinct author (`spire-agent <agent@spire.local>`)
- `bd audit` records LLM calls if enabled

**Kill switch:**
- `kubectl delete pod spire-agent-*` stops all agents immediately
- `spire mayor --pause` stops new assignments
- Close or defer beads to prevent agents from claiming them

### 7. Cost model

Per autonomous task (rough estimates):

| Component | Cost |
|-----------|------|
| Claude API (Sonnet, ~50 turns, ~200K tokens) | ~$1-3 |
| Claude API (Opus, complex task, ~100 turns) | ~$10-30 |
| k8s pod (5 min, 200m CPU) | ~$0.001 |
| GitHub Actions (review feedback) | free (public) / ~$0.008/min (private) |

**vs Devin**: $500/mo for ~250 tasks = $2/task flat.
**vs self-hosted spire**: $1-3/task for routine work, pay-per-use.

Break-even at ~200 tasks/mo. Below that, self-hosted is cheaper.
Above that, self-hosted still wins because you control the model and prompts.

## What we already have

| Piece | Status | Where |
|-------|--------|-------|
| Work queue (beads) | Done | `bd ready`, `spire board` |
| Atomic claiming | Done | `spire claim` |
| Context assembly | Done | `spire focus` |
| Spec scaffolding | Done | `spire spec` |
| Work lifecycle | Done | `spire claim → focus → close` |
| Mayor (coordinator) | Done | `spire mayor` |
| Agent messaging | Done | `spire send / collect` |
| Hooks (context injection) | Done | SessionStart, SubagentStart |
| Molecule workflow | Done | design → implement → review → merge |
| CRDs | Done (schema) | `k8s/crds/` |
| Operator controllers | Scaffolded | `operator/controllers/` |
| Dockerfile (mayor) | Done | `Dockerfile.mayor` |
| k8s manifests | Done | `k8s/` |
| Minikube demo script | Done | `k8s/minikube-demo.sh` |

## What we need to build

| Piece | Effort | Priority |
|-------|--------|----------|
| `Dockerfile.agent` (agent image with claude CLI) | 1 day | P0 |
| `agent-entrypoint.sh` (clone → claim → execute → PR) | 1 day | P0 |
| Operator go.mod + controller-runtime wiring | 1 day | P0 |
| `spire-agent-worker` (Agent SDK Go binary) | 2 days | P1 |
| Review feedback GitHub Action | 1 day | P1 |
| `spire review` (CLI for PR review pipeline) | 1 day | P1 |
| Token routing in operator | 1 day | P2 |
| Network policies | 0.5 day | P2 |
| Helm chart | 1 day | P2 |
| `spire dashboard` (terminal UI) | 2 days | P3 |

**P0 = working demo in 3 days.**

## Implementation order

### Day 1: Agent image + entrypoint

1. `Dockerfile.agent` — based on `Dockerfile.mayor` + claude CLI + gh + node/go
2. `agent-entrypoint.sh` — clone → init → claim → focus → claude → push → PR → close
3. Test locally: build image, `docker run` with a real bead + Anthropic key
4. Verify: bead claimed, code written, PR created, bead closed

### Day 2: Operator wiring

5. `operator/go.mod` — add controller-runtime, k8s client-go dependencies
6. Wire `BeadWatcher` to actually create SpireWorkload CRs
7. Wire `WorkloadAssigner` to create agent pods (not just send messages)
8. Wire `AgentMonitor` to track pod lifecycle
9. Test on minikube: file bead → mayor sees it → agent pod starts → PR appears

### Day 3: End-to-end

10. Review feedback GitHub Action
11. Test full cycle: file bead → agent works → PR → review → feedback → agent revises → merge
12. Document the setup for other teams

### Week 2: Production hardening

13. Agent SDK worker (Go binary, cheaper than CLI)
14. Token routing
15. Network policies
16. Helm chart
17. `spire dashboard`
