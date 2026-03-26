# spire.yaml Configuration Reference

`spire.yaml` lives in the root of each registered repo. It tells wizards how to work in that repo: which model to use, how to run tests, how to name branches, and which files to read before starting.

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
| `language` | string | Repository language. Auto-detected if omitted. Values: `go`, `typescript`, `python`, `rust`. |
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
| `model` | string | `claude-sonnet-4-6` | Claude model for implementation. |
| `max-turns` | int | `30` | Claude Code turn limit per phase. |
| `stale` | duration | `10m` | Steward warning threshold. After this time, steward flags the wizard as stale. |
| `timeout` | duration | `15m` | Hard kill threshold. Steward terminates the wizard after this time. |
| `formula` | string | (by bead type) | Default formula to use. Overrides bead-type mapping but is overridden by bead labels. |

**Model values:** Any Claude model identifier — `claude-sonnet-4-6`, `claude-opus-4-6`, `claude-haiku-4-5-20251001`.

**Timeout behavior:** The `stale` threshold generates a warning in `spire roster`. The `timeout` threshold sends SIGKILL and marks the bead as failed. Both are calculated from when the wizard starts working, not from when the bead was filed.

**Per-phase overrides:** Individual formula phases can override the model and timeout. See [formulas](#formulas) below.

---

## `branch`

Controls how wizards manage git branches.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `base` | string | `main` | Base branch for pull requests. |
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

Controls pull request behavior.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `auto-merge` | bool | `false` | Merge PR automatically after sage approval. If false, the wizard creates the PR but waits for human merge. |
| `reviewers` | list | `[]` | GitHub usernames to request as reviewers. |
| `labels` | list | `["agent-generated"]` | Labels to add to the PR. |
| `draft` | bool | `false` | Create PRs as drafts. |

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
| `task`, `feature`, `chore` | `spire-agent-work` | implement → review → merge |
| `bug` | `spire-bugfix` | implement → review → merge |
| `epic` | `spire-epic` | design → plan → implement → review → merge |

**Override per-repo** (affects all beads in this repo unless the bead has a label):

```yaml
agent:
  formula: spire-bugfix    # use bugfix formula for everything
```

**Override per-bead** (highest priority):

```bash
bd label add spi-abc "formula:spire-bugfix"
```

**Custom formulas:** Place `.toml` files in `.beads/formulas/` to override or extend built-in formulas:

```
.beads/formulas/spire-agent-work.formula.toml   # override default
.beads/formulas/my-custom.formula.toml          # add new formula
```

Formula files use TOML:

```toml
formula = "my-custom"

[phases.implement]
timeout = "20m"
model = "claude-opus-4-6"
worktree = true

[phases.review]
timeout = "15m"
model = "claude-opus-4-6"

[phases.review.revision]
max_rounds = 3
on_exhaust = "arbitrate"   # or "approve", "discard"

[phases.merge]
auto = true
```

A phase that isn't declared doesn't exist in the formula. A custom `spire-agent-work` that only has `[phases.implement]` skips review and merge entirely.

---

## Auto-detection

When `spire.yaml` is absent, Spire infers settings from the repo:

| File found | Detected language | Default test command |
|------------|-------------------|----------------------|
| `go.mod` | `go` | `go test ./...` |
| `package.json` | `typescript` or `javascript` | `npm test` or `pnpm test` |
| `Cargo.toml` | `rust` | `cargo test` |
| `pyproject.toml` or `setup.py` | `python` | `pytest` |

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
