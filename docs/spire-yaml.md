# spire.yaml Configuration Reference

`spire.yaml` lives in the root of each registered repo. It tells wizards how to work in that repo: which backend and model to use, how to run tests, how to name branches, and which files to read before starting.

When `spire repo add` is run, `spire.yaml` is generated automatically if one doesn't exist. You can edit it at any time.

---

## Full example

```yaml
runtime:
  language: go
  test: go test ./...
  build: go build ./cmd/...
  lint: go vet ./...
  install: ""              # optional: run before test/build/lint

agent:
  backend: process
  model: claude-sonnet-4-6
  max-turns: 30
  stale: 10m
  timeout: 15m

branch:
  base: main
  pattern: "feat/{bead-id}"

pr:
  auto-merge: false
  reviewers: ["your-github-username"]
  labels: ["agent-generated"]

context:
  - CLAUDE.md
  - PLAYBOOK.md
  - docs/
```

---

## `runtime`

Controls how the wizard validates its work.

| Field | Type | Description |
|-------|------|-------------|
| `language` | string | Repository language. Auto-detected if omitted. Values: `go`, `typescript`, `python`, `rust`, `unknown`. |
| `test` | string | Command to run tests. Run after implementation. |
| `build` | string | Command to build the project. Run after tests. |
| `lint` | string | Command to lint the project. Run first. |
| `install` | string | Command to install dependencies. Run before lint/build/test. |

**Auto-detection:** If `spire.yaml` is absent or `language` is omitted, Spire infers the runtime by walking the repo for `go.mod`, `package.json`, `Cargo.toml`, etc.

**Validation order:** `install` → `lint` → `build` → `test`. If any step fails, the wizard does not push and reports the failure.

**Skipping steps:** Leave a field empty to skip that step:

```yaml
runtime:
  language: go
  lint: go vet ./...
  build: ""     # skip build
  test: ""      # skip tests
```

---

## `agent`

Controls how wizards run.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `backend` | string | `process` | Execution backend: `process`, `docker`, or `k8s`. |
| `model` | string | `claude-sonnet-4-6` | Claude model for implementation. |
| `max-turns` | int | `30` | Claude Code turn limit per phase. |
| `stale` | duration | `10m` | Steward warning threshold. After this time, steward flags the wizard as stale. |
| `timeout` | duration | `15m` | Hard kill threshold. Steward terminates the wizard after this time. |
| `design-timeout` | duration | unset | Optional override for the design phase timeout. |
| `formula` | string | (by bead type) | Default formula to use. Overrides bead-type mapping but is overridden by bead labels. |

**Model values:** Any Claude model identifier — `claude-sonnet-4-6`, `claude-opus-4-6`, `claude-haiku-4-5-20251001`.

**Timeout behavior:** The `stale` threshold generates a warning in `spire roster`. The `timeout` threshold sends SIGKILL and marks the bead as failed. Both are calculated from when the wizard starts working, not from when the bead was filed.

**Per-phase overrides:** Individual formula phases can override the model and timeout. See [formulas](#formulas) below.

**Docker backend options:** When `backend: docker`, Spire also reads the nested `agent.docker` block:

```yaml
agent:
  backend: docker
  docker:
    image: ghcr.io/awell-health/spire-agent:latest
    network: host
    extra-volumes: []
    extra-env: []
```

---

## `branch`

Controls how wizards manage git branches.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `base` | string | `main` | Base branch the executor lands approved work into. |
| `pattern` | string | `feat/{bead-id}` | Branch name pattern. `{bead-id}` is replaced with the actual bead ID. |

**Branch pattern variables:**

| Variable | Replaced with |
|----------|---------------|
| `{bead-id}` | Bead ID (e.g., `spi-a3f8`) |
| `{type}` | Bead type (e.g., `feat`, `fix`, `chore`) |

Examples:
```yaml
branch:
  pattern: "feat/{bead-id}"         # → feat/spi-a3f8
  pattern: "{type}/{bead-id}"       # → feat/spi-a3f8, fix/spi-b7d0
  pattern: "agent/{bead-id}"        # → agent/spi-a3f8
```

---

## `pr`

Controls GitHub PR metadata for PR-oriented workflows.

The current default local executor path does **not** open PRs. It lands
approved work by merging directly to `branch.base`. The `pr:` block is
still part of the schema for GitHub-oriented flows and future landing
paths.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `auto-merge` | bool | `false` | Advisory flag for PR-oriented workflows. Not used by the default local executor path. |
| `reviewers` | list | `[]` | GitHub usernames to request as reviewers. |
| `labels` | list | `["agent-generated"]` | Labels to add to the PR. |

---

## `context`

Files and directories that wizards read before starting work.

```yaml
context:
  - CLAUDE.md              # project-specific instructions
  - PLAYBOOK.md            # operational reference
  - docs/architecture.md   # specific file
  - docs/specs/            # all files in a directory
```

Context files are assembled by `spire focus` and injected into the wizard's prompt. Keep this list focused — reading every file in `docs/` adds tokens and latency.

**Best practice:** Include files that contain:
- Project-specific conventions the agent must follow
- Architecture decisions that affect how code is written
- API contracts the agent must respect

---

## Formulas

Formulas determine the phase pipeline a wizard follows. The mapping is automatic but can be overridden.

**Bead type → formula mapping:**

| Bead type | Formula | Phases |
|-----------|---------|--------|
| `task`, `feature`, `chore` | `task-default` | plan → implement → review → merge |
| `bug` | `bug-default` | plan → implement → review → merge |
| `epic` | `epic-default` | design → plan → implement → review → merge |

**Override per-repo** (affects all beads in this repo unless the bead has a label):

```yaml
agent:
  formula: bug-default    # use bug formula for everything
```

**Override per-bead** (highest priority):

```bash
bd label add spi-abc "formula:bug-default"
```

**Custom formulas:** Place `.toml` files in `.beads/formulas/` to override or extend built-in formulas:

```
.beads/formulas/task-default.formula.toml   # override default
.beads/formulas/my-custom.formula.toml      # add new formula
```

Formula files use TOML:

```toml
name = "my-custom"
version = 1

[phases.plan]
role = "wizard"
timeout = "5m"
model = "claude-opus-4-6"

[phases.implement]
role = "apprentice"
timeout = "20m"
model = "claude-sonnet-4-6"
worktree = true

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

A phase that isn't declared doesn't exist in the formula. A custom `task-default` that only has `[phases.implement]` skips review and merge entirely.

---

## Auto-detection

When `spire.yaml` is absent, Spire infers settings from the repo:

| File found | Detected language | Default test command |
|------------|-------------------|----------------------|
| `go.mod` | `go` | `go test ./...` |
| `package.json` | `typescript` | `npm test` or `pnpm test` |
| `Cargo.toml` | `rust` | `cargo test` |
| `pyproject.toml` or `requirements.txt` | `python` | `pytest` |

Auto-detection is best-effort. For reliable behavior, commit an explicit `spire.yaml`.

---

## Multiple repos in one tower

Each repo has its own `spire.yaml`. Settings are not inherited across repos.

```
~/code/
  frontend/
    spire.yaml      # runtime.language: typescript, agent.model: sonnet
  backend/
    spire.yaml      # runtime.language: go, agent.model: sonnet
  infra/
    spire.yaml      # runtime.language: python, agent.timeout: 30m
```

The wizard reads the `spire.yaml` from the repo it's working in, resolved via the bead's prefix → repo URL → local checkout.
