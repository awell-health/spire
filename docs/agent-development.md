# Agent Development Guide

This guide explains how Spire agents work and how to build agents that integrate with the Spire coordination system.

---

## Agent roles

Spire has a defined hierarchy of agent roles:

| Role | What it does | How it runs |
|------|-------------|-------------|
| **Wizard** | Per-bead orchestrator. Drives the formula lifecycle. | `spire execute <bead-id>` (local, spawned by `summon`) or wizard pod (k8s) |
| **Apprentice** | Implementer. Writes code in an isolated worktree. One-shot. | Dispatched by wizard |
| **Sage** | Reviewer. Reviews code, returns a verdict. One-shot. | Dispatched by wizard |
| **Steward** | Global coordinator. Assigns work, monitors health. | `spire steward` |
| **Familiar** | Per-agent sidecar. Messaging, health checks. | Container sidecar (k8s only) |

Most custom agent work falls into one of these patterns:
1. **Custom formula** — modify the phase pipeline a wizard follows
2. **External agent** — a process that claims and works beads using the Spire protocol
3. **Integration agent** — responds to messages and takes actions (send alerts, create beads, etc.)

---

## How wizards work

A wizard is driven by a **formula** — a TOML file that declares ordered phases. The executor runs each phase in sequence:

```
resolve repo → claim bead → load formula
→ for each phase in formula:
    design: validate linked design bead
    plan:   invoke Claude (Opus) to generate subtask breakdown
    implement: dispatch apprentice in worktree
    review: dispatch sage for verdict
    merge: ff-only merge to the configured base branch
→ close bead → write result → exit
```

### Formula resolution

The formula is selected in this order (first match wins):
1. Label `formula:<name>` on the bead
2. Bead type → formula mapping (`task`→`spire-agent-work`, `bug`→`spire-bugfix`, `epic`→`spire-epic`)
3. `agent.formula` field in `spire.yaml`
4. Default: `spire-agent-work`

### Writing a custom formula

Create `.beads/formulas/<name>.formula.toml` in your repo:

```toml
name = "my-workflow"
version = 1

# Only the phases declared here exist for this formula.
# Undeclared phases are skipped.

[phases.plan]
role = "wizard"
timeout = "5m"
model = "claude-opus-4-6"

[phases.implement]
role = "apprentice"
timeout = "20m"
model = "claude-sonnet-4-6"
worktree = true           # run apprentice in isolated worktree

[phases.review]
role = "sage"
timeout = "15m"
model = "claude-opus-4-6"
verdict_only = true

[phases.review.revision_policy]
max_rounds = 3
arbiter_model = "claude-opus-4-6"

[phases.merge]
strategy = "squash"
auto = true
```

**Phase fields:**

| Field | Type | Description |
|-------|------|-------------|
| `timeout` | duration | Time limit for this phase |
| `model` | string | Claude model to use |
| `worktree` | bool | Run in isolated git worktree (implement only) |
| `context` | list | Additional files to include in context |

**Revision policy fields:**

| Field | Type | Description |
|-------|------|-------------|
| `max_rounds` | int | Maximum review-fix cycles before escalation |
| `arbiter_model` | string | Model to use when review rounds are exhausted |

Apply the formula to a bead:

```bash
bd label add spi-abc "formula:my-workflow"
```

---

## Building an external agent

An external agent is any process that:
1. Registers an identity with the tower
2. Claims beads from the ready queue
3. Implements them using the Spire protocol
4. Reports results via bead state and messages

This is the protocol the built-in wizard uses. You can build agents in any language that can shell out to `spire` and `bd`.

### Registration

```bash
spire register my-agent
```

Registration creates routing labels for incoming messages. Unregister when done:

```bash
spire unregister my-agent
```

### Claiming work

```bash
# Find ready beads
bd ready --json | jq '.[0].id'

# Claim atomically (fails if already claimed)
spire claim spi-abc

# Get full context
spire focus spi-abc
```

`spire claim` is atomic: it pulls from DoltHub, verifies the bead is claimable, sets `status=in_progress` with your identity, and pushes — all in one step. If another agent claimed it first, the command fails cleanly.

### Implementing work

After claiming, do your work. For code changes:

```bash
# Create a branch
git checkout -b feat/spi-abc

# ... make changes ...

# Validate
go test ./...  # or whatever your spire.yaml says

# Commit with the bead reference
git commit -m "feat(spi-abc): implement the thing"

# Push
git push origin feat/spi-abc
```

The branch name must match `spire.yaml`'s `branch.pattern` (default: `feat/{bead-id}`).

### Landing the change

```bash
git checkout <base-branch>
git merge --ff-only feat/spi-abc
git push origin <base-branch>
```

Replace `<base-branch>` with the repo's configured `branch.base`. This matches the built-in local executor path: after review passes, approved work lands on the repo's base branch.

### Reporting results

Add a comment to the bead:

```bash
bd comments add spi-abc "Implemented and landed on the base branch."
```

Close the bead when done:

```bash
bd close spi-abc
```

### Messaging

Agents communicate through structured messages routed by bead references:

```bash
# Send a message to the steward
spire send steward "OAuth task needs API keys in secrets" --ref spi-abc --priority 1

# Check your inbox
spire collect my-agent

# Watch for new messages (blocks until one arrives)
spire inbox my-agent --watch

# Mark a message as read
spire read spi-msg-xyz
```

Messages are stored as beads with labels: `msg`, `to:<agent>`, `from:<agent>`, `ref:<bead-id>`.

---

## Writing a wizard in Go

If you want to extend the built-in wizard behavior, the core types are in `cmd/spire/`:

| File | Contents |
|------|---------|
| `formula.go` | Formula loading, parsing, phase configuration |
| `executor.go` | Formula execution loop, phase dispatch |
| `wizard.go` | Wizard process entry point, worktree management |
| `store.go` | Store API (preferred over shelling out to `bd`) |

### Using the store API

**Always use the store API for bead operations in Go code.** Never shell out to `bd` in production code paths:

```go
// Good: store API
bead, err := storeGetBead(id)
beads, err := storeListBoardBeads(filter)
err := storeAddComment(id, "Implementation complete")
err := storeAddLabel(id, "needs-human")
err := storeCloseBead(id)

// Bad: subprocess
out, err := bd("show", id, "--json")
out, err := bdJSON("list")
```

The store API uses a direct SQL connection to the dolt database — it's faster, works from any directory, and doesn't require `.beads/` in the cwd.

### resolveBeadsDir

Every command that reads beads must call `resolveBeadsDir()` at entry:

```go
func cmdMyAgent(args []string) error {
    if d := resolveBeadsDir(); d != "" {
        os.Setenv("BEADS_DIR", d)
    }
    // ... rest of command
}
```

This makes the command work from any directory (not just inside a registered repo).

---

## Writing a wizard in Claude Code prompts

The wizard executor builds a prompt and passes it to `claude --dangerously-skip-permissions`. You can see the prompt format in `wizard.go` and the formula `context` fields.

Key elements of an effective agent prompt:

1. **Role and task** — what the agent is and what it's doing
2. **Context paths** — which files to read before starting (mirrors `spire.yaml`'s `context:`)
3. **Bead description** — from `spire focus <bead-id>` or `bd show <bead-id> --json`
4. **Validation commands** — from `spire.yaml` (`runtime.lint`, `runtime.build`, `runtime.test`)
5. **Commit format** — `<type>(<bead-id>): <message>`
6. **Finishing instructions** — close the bead, commit, push

The `spire focus` command assembles most of this automatically.

---

## Container agents (k8s)

In Kubernetes, agents run as pods with two containers:
- **wizard container**: runs `agent-entrypoint.sh`
- **familiar container**: runs `spire-sidecar` for messaging and health checks

### Communication protocol

Containers communicate via the shared `/comms` emptyDir volume:

| File | Writer | Reader | Purpose |
|------|--------|--------|---------|
| `inbox.json` | familiar | wizard | Messages from other agents |
| `control` | familiar | wizard | STOP, STEER, PAUSE, RESUME |
| `result.json` | wizard | familiar | Run outcome (triggers shutdown) |
| `wizard-alive` | wizard | familiar | Heartbeat (liveness) |
| `heartbeat` | familiar | operator | Familiar liveness |

The wizard writes `result.json` when done. The familiar detects this and exits, which triggers the operator to reap the pod.

### Custom agent image

To add tooling your agents need:

```dockerfile
FROM ghcr.io/awell-health/spire-agent:latest

# Add your tools
RUN apt-get install -y your-tool

# Or add language-specific deps
RUN pip install your-package
```

Reference your image in the WizardGuild CRD:

```yaml
apiVersion: spire.awell.io/v1alpha1
kind: WizardGuild
metadata:
  name: my-agent
  namespace: spire
spec:
  mode: managed
  image: your-registry/your-image:tag
  repo: https://github.com/org/repo.git
  prefixes: ["myp-"]
```

---

## Agent health

### Local (process mode)

Wizard processes are tracked in `~/.config/spire/wizards.json`. The steward checks process liveness and cleans up dead processes.

```bash
spire roster         # view all agents and their health
spire logs wizard-spi-abc  # tail a specific agent's output
```

### Kubernetes

The operator monitors WizardGuild `status.phase`:

| Phase | Description |
|-------|-------------|
| `Idle` | Ready for work |
| `Working` | Has assigned work, pod running |
| `Provisioning` | Pod starting up |
| `Stale` | Exceeded `spire.yaml`'s `agent.stale` threshold |
| `Offline` | Heartbeat timeout |

The familiar writes `heartbeat` every 30 seconds. The operator marks agents `Offline` if the heartbeat is too old.

---

## Debugging

### Watch what a wizard is doing

```bash
spire logs wizard-spi-abc    # tail wizard output
spire roster           # see phase and elapsed time
spire board            # see what board column the bead is in
```

### Inspect a wizard's prompt

After a wizard runs, inspect the prompt it was given:

```bash
cat /tmp/spire-wizard/wizard-spi-abc/spi-abc/.spire-prompt.txt
```

### Run a wizard manually (for debugging)

```bash
spire execute spi-abc --name debug-wizard
```

This runs the full executor synchronously in the foreground with full output, instead of as a background process.

### Check bead state

```bash
bd show spi-abc --json      # full bead details
bd comments spi-abc --json  # read wizard comments
spire trace spi-abc         # inspect workflow steps and timing
```
