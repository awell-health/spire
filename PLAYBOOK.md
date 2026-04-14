# Agent Playbook

Operational reference for agents working in this repo. Not architecture, not conventions — just "how to actually do things" and what to watch out for.

CLAUDE.md tells you the rules. This file tells you the exact commands.

## Role hierarchy

| Role | Scope | Entry point | What |
|---|---|---|---|
| **Archmage** | The user (human) | — | Sets priorities, resolves gates, bounces between towers |
| **Steward** | Global coordinator | `spire steward` | Capacity, scheduling, starts wizards |
| **Wizard** | Per-bead orchestrator | `spire summon N` | Driven by formula. Orchestrates the full lifecycle for any bead (task, bug, epic). Dispatches apprentices and sages as the formula requires |
| **Apprentice** | Per-subtask implementer | dispatched by wizard | Writes code in a worktree. One-shot. Pure implementer (`--no-handoff`) |
| **Sage** | Per-review agent | dispatched by wizard | Reviews implementation, produces verdict (`--verdict-only`). One-shot |
| **Artificer** | Formula creator | Workshop CLI (not yet built) | Crafts and tests formulas (spells). Does NOT orchestrate epics or review code |
| **Familiar** | Per-agent companion | daemon (local) / container (k8s) | Messaging infrastructure, inbox file delivery |

A wizard is summoned to support a bead. The **formula** (derived from bead type) determines the orchestration. A bug gets `spire-bugfix` (implement → review → merge). An epic gets `spire-epic` (plan → wave dispatch → review with judgment → merge). Same executor, different formula.

## bd CLI quick reference

### Beads (CRUD)

```bash
# Create
bd create "Title" -p 1 -t task
bd create "Title" -p 0 -t epic --parent spi-abc
bd create "Title" -p 2 -t task -d "Description here"

# Read
bd show spi-abc                     # full details
bd show spi-abc --json              # machine-readable
bd list --json                      # all beads
bd ready --json                     # ready work (no blockers)
bd children spi-abc                 # children of a parent
bd children spi-abc --json          # children as JSON

# Update
bd update spi-abc -d "New desc"     # update fields
bd update spi-abc --claim           # atomic claim (sets assignee + in_progress)
bd update spi-abc --status open     # change status

# Close
bd close spi-abc                    # close a bead
bd reopen spi-abc                   # reopen
```

### Comments

```bash
bd comments spi-abc                 # list comments
bd comments spi-abc --json          # list as JSON
bd comments add spi-abc "Text"      # add a comment
bd comments add spi-abc -f file.txt # add from file
```

**Wrong**: `bd comment spi-abc "text"` — there is no `comment` command, only `comments`.

### Labels

```bash
bd label add spi-abc "my-label"     # add a label
bd label remove spi-abc "my-label"  # remove a label
bd label list spi-abc               # list labels on a bead
bd label list-all                   # all unique labels in db
```

**Wrong**: `bd label spi-abc "text"` — `label` requires a subcommand (`add`, `remove`, `list`).

### Notes

```bash
bd note spi-abc "Quick note text"   # append to notes field
bd note spi-abc --file notes.txt    # append from file
```

Notes append; they don't replace. Use `bd update --notes` to replace entirely.

### Dependencies

```bash
bd dep add spi-blocked spi-blocker  # spi-blocked depends on spi-blocker
bd dep spi-blocker --blocks spi-blocked  # same thing, reversed syntax
bd dep remove spi-blocked spi-blocker    # remove dependency
bd dep list spi-abc                 # list deps of a bead
bd dep tree spi-abc                 # show dependency tree
```

### Molecules & formulas

```bash
bd formula list                     # list available formulas
bd formula show spire-agent-work    # show formula details

bd cook spire-agent-work --persist  # cook formula into proto (idempotent)
bd mol pour spire-agent-work --var task=spi-abc --json  # instantiate molecule
bd mol progress spi-mol-xyz         # show (N/M) progress
bd mol show spi-mol-xyz             # show molecule structure
bd mol current spi-mol-xyz          # show current position in workflow
bd mol stale                        # find complete-but-unclosed molecules
```

**Pour returns JSON**: `{"new_epic_id": "spi-mol-abc"}` — parse this to get the molecule root ID.

### Gates

```bash
bd gate list                        # show open gates
bd gate list --all                  # include closed
bd gate resolve spi-abc             # manually close a gate
bd gate check                       # evaluate all open gates
```

## spire CLI quick reference

### Lifecycle

```bash
spire up                            # start dolt + daemon
spire down                          # stop daemon (dolt stays)
spire shutdown                      # stop everything
spire status                        # what's running
```

### Work

```bash
spire claim spi-abc                 # verify + claim (always do this first)
spire focus spi-abc                 # assemble context, pour molecule if needed
spire file "Title" -t task -p 2     # create a bead via spire
spire design "Title" -p 2           # create a design bead (brainstorm artifact)
spire board                         # kanban view
spire board --json                  # machine-readable board
```

### Messaging

```bash
spire register my-agent             # register agent identity
spire unregister my-agent           # unregister
spire collect                       # check inbox (all, queries DB)
spire collect my-agent              # check inbox for specific agent
spire send target-agent "message" --ref spi-abc  # send message
spire read spi-msg-id               # mark message as read
spire inbox [agent-name]            # read local inbox file (fast, no DB)
spire inbox --check                 # silent if empty, prints if new (for hooks)
spire inbox --watch                 # block until new messages (for wizard main loop)
```

`spire collect` queries the DB. `spire inbox` reads the cached local file written by the daemon. Use `spire inbox` for hot-path checks (hooks, wizard loops). Use `spire collect` for manual/CLI use.

### Agents

```bash
spire summon 3                          # summon 3 wizards (pick ready beads, use formula from bead type)
spire summon 1 --targets=spi-abc       # run exactly this bead
spire summon 2 --targets=spi-x,spi-y   # run exactly these beads (k8s/CI)
spire roster                            # show work grouped by epic and agent status
spire dismiss --all                     # dismiss all agents
```

`spire summon` spawns formula executors. Each wizard picks a ready bead, resolves its formula (from bead type → formula mapping), and runs the full lifecycle. The formula determines everything: which phases, how review works, whether to use wave dispatch.

### Formulas

```
.beads/formulas/spire-agent-work.formula.toml   # default: design → implement → review → merge
.beads/formulas/spire-bugfix.formula.toml        # quick: implement → review → merge
.beads/formulas/spire-epic.formula.toml          # epic: plan → wave implement → review (judgment) → merge
```

Formula resolution (automatic):
1. Bead label `formula:<name>` — explicit override
2. Bead type → formula: task→spire-agent-work, bug→spire-bugfix, epic→spire-epic
3. spire.yaml `agent.formula` field
4. Default: spire-agent-work

Override per-bead: `bd label add spi-abc "formula:spire-bugfix"`

## Common gotchas

### bd vs spire

| Use `spire` for | Use `bd` for |
|---|---|
| claim, focus, board, roster | create, update, show, list |
| send, collect, inbox, read (messages) | comments, labels, notes |
| up, down, shutdown, status | dep, mol, formula, gate |
| wizard-epic, dismiss | close, reopen, children |

Rule of thumb: `spire` for coordination and agent lifecycle. `bd` for data operations on beads.

### Subcommand traps

These commands require subcommands — bare invocations don't do what you'd expect:

| Wrong | Right |
|---|---|
| `bd comment spi-abc "text"` | `bd comments add spi-abc "text"` |
| `bd label spi-abc "label"` | `bd label add spi-abc "label"` |
| `bd dep spi-a spi-b` | `bd dep add spi-a spi-b` |
| `bd mol pour spire-agent-work` | `bd mol pour spire-agent-work --var task=spi-abc` |

### --json everywhere

Almost every read command supports `--json`. Always use it for programmatic access:

```bash
bd show spi-abc --json | jq '.status'
bd list --json | jq '.[] | select(.priority == 0)'
bd ready --json | jq '.[].id'
bd comments spi-abc --json
spire board --json
```

### Claiming work

Always claim before working. The sequence matters:

```bash
spire claim spi-abc     # 1. verify + claim (atomic)
spire focus spi-abc     # 2. get context + pour molecule
# 3. do the work
bd close spi-abc        # 4. close when done (TODO: replace with spire close, see spi-39u)
```

`spire claim` fails if the bead is closed or owned by someone else. That's intentional — it prevents double-work.

### Store API vs bd subprocess (Go code)

When writing Go code in spire, never shell out to `bd`. Use the store API:

```go
// Right
bead, err := storeGetBead(id)
beads, err := storeListBeads(filter)
err := storeAddComment(id, "text")
err := storeAddLabel(id, "label")
err := storeCloseBead(id)

// Wrong
out, err := bd("show", id, "--json")
out, err := bdJSON("list")
```

The `bd()` helper is only used for molecule operations (`bd cook`, `bd mol pour`) that don't have store API equivalents yet.

### Design beads

Design beads (`-t design`) are thinking artifacts, not work items. They capture brainstorming, exploration, rejected approaches, and design decisions.

```bash
spire design "Auth system overhaul" -p 2   # create design bead
bd comments add spi-xxx "approach A: ..."   # capture thinking incrementally
bd comments add spi-xxx "rejected because..."
bd close spi-xxx                            # close when settled

# then create work and link:
spire file "Auth overhaul" -t epic -p 1 --label "ref:spi-xxx"
```

Design beads are:
- **Visible** on the board (they're real beads)
- **Not work items** — filtered out of `bd ready` and `spire summon`
- **Not formula-driven** — no phases, no wizard, no lifecycle
- **Linked** to work items via `ref:` labels (surfaced by `spire focus`)

Use the `/spire-design` skill to brainstorm interactively. It creates the design bead at the start and captures decisions as the conversation progresses.

### Phase labels (convention)

```bash
bd label add spi-abc "phase:design"
bd label remove spi-abc "phase:design"
bd label add spi-abc "phase:plan"
```

One phase label at a time. Remove the old one before adding the new one.

### Review round labels

```bash
bd label add spi-abc "review-round:1"
# On next round:
bd label remove spi-abc "review-round:1"
bd label add spi-abc "review-round:2"
```

Replace, don't accumulate. At most one `review-round:N` label at a time.

## Phase pipeline

### PHASE DISCIPLINE — DO NOT SKIP PHASES

Agents MUST follow the phase pipeline in order. Do NOT jump to implementation.

For each epic or task with a formula:
1. **phase:design** — explore the problem, produce design decisions
2. **phase:plan** — break into subtasks, file them, post the plan as a comment
3. **phase:implement** — ONLY after plan is complete and subtasks are filed
4. **phase:review** — ONLY after implementation is complete
5. **phase:merge** — ONLY after review approves

Transition phases explicitly:
```bash
bd label remove <id> "phase:design"
bd label add <id> "phase:plan"
```

**Even if the task seems small, follow the phases.** The plan is the checkpoint where the archmage can course-correct before tokens are spent on implementation. Skipping plan means no opportunity to catch design misunderstandings before they become wasted code.

### The 5 universal phases

Every piece of work moves through a subset of these phases:

```
READY → DESIGN → PLAN → IMPLEMENT → REVIEW → MERGE → DONE
```

These are not pipeline-specific — they're the vocabulary of how all work progresses. A formula configures which phases are enabled for a given work type.

### Two systems, two jobs

| System | What it tracks | How |
|---|---|---|
| `phase:X` label on the bead | Where the bead is RIGHT NOW | Board column routing, agent dispatch |
| GraphState step beads | What has been COMPLETED | Audit trail, progress record |

On each phase transition, the code calls `setPhase()` (update board routing). Step bead lifecycle (activate/close) is managed by the v3 graph interpreter via `GraphState.StepBeadIDs`.

### Phase transitions

```bash
# Check current phase
bd label list spi-abc | grep phase:

# Transition (always remove old, then add new)
bd label remove spi-abc "phase:design"
bd label add spi-abc "phase:plan"
```

Valid phases: `design`, `plan`, `implement`, `review`, `merge`. A bead with no `phase:` label is in READY state. A closed bead is DONE.

### Board columns map to phases

| Board column | Routing rule |
|---|---|
| READY | open, no `phase:` label, not blocked |
| DESIGN | has `phase:design` |
| PLAN | has `phase:plan` |
| IMPLEMENT | has `phase:implement` |
| REVIEW | has `phase:review` |
| MERGE | has `phase:merge` |
| DONE | closed |
| BLOCKED | open, has unresolved blockers |

Empty columns collapse automatically.

### Formula v2 configures phases

```toml
formula = "spire-agent-work"

[phases.design]
timeout = "10m"
model = "sonnet"

[phases.plan]
timeout = "5m"
model = "sonnet"

[phases.implement]
timeout = "15m"
model = "opus"
worktree = true

[phases.review]
timeout = "20m"
model = "opus"

[phases.review.revision]
max_rounds = 3
on_exhaust = "arbitrate"

[phases.merge]
auto = true
```

A phase that isn't declared doesn't exist for that formula. A bugfix formula might only have `implement`, `review`, and `merge`.

### Review rounds and escalation

The bead stays in `phase:review` across all review rounds. Round tracking via labels:

```bash
# Round labels (replace, don't accumulate)
bd label remove spi-abc "review-round:1"
bd label add spi-abc "review-round:2"
```

At `max_rounds` (from formula revision policy), the arbiter role is activated. Arbiter outcomes:
- **merge** — override reviewer, proceed to merge
- **discard** — close bead as wontfix, clean up branch
- **split** — create child beads for remaining issues, close current bead

## Orchestrating multi-agent work

### Wave-based execution

When an epic has subtasks with dependencies, the executor (using `spire-epic` formula with `dispatch = "wave"`) dispatches apprentices in waves:

```
Wave 0 (parallel):  spi-zpp.1    spi-zpp.2     ← no deps, start immediately
Wave 1 (parallel):  spi-zpp.3    spi-zpp.4     ← depend on wave 0
Wave 2:             spi-zpp.5                   ← depends on wave 1
Wave 3:             spi-zpp.6                   ← depends on wave 2
```

Each wave's apprentices run in isolated worktrees. The executor:
1. Claims subtasks: `spire claim spi-zpp.N`
2. Dispatches apprentices with `isolation: "worktree"`
3. Waits for all apprentices in the wave to complete
4. Merges worktree branches back to staging
5. Verifies the build: `go build ./...`
6. Closes completed subtasks: `bd close spi-zpp.N`
7. Proceeds to next wave

### Apprentice limitations

- Apprentices **cannot receive messages** mid-work. They work with the context given at dispatch.
- If an apprentice fails: the wizard reads the output, diagnoses the issue, and re-dispatches with corrected instructions (not blind retry).
- Each apprentice must get ALL necessary context upfront — bead description, design decisions, file paths, corrections from comments.

### Bead comment corrections

**Bead descriptions can become stale.** When design decisions are refined after subtasks are filed, the corrections land in comments on the epic, not in the subtask descriptions.

Before dispatching an apprentice:
1. Read `bd comments <epic-id> --json` for the latest corrections
2. Check if any corrections contradict the subtask description
3. Update the subtask description if needed: `bd update <id> -d "corrected description"`
4. Include critical corrections verbatim in the agent prompt

The apprentice reads its bead description and trusts it. If the description is wrong, the apprentice will do the wrong thing. Fix the description before dispatch.

### Context injection for apprentices

Every apprentice prompt should include:

1. **Bead description** — from `bd show spi-zpp.N --json`
2. **Critical design decisions** — from epic comments, copied verbatim
3. **File reading instructions** — CLAUDE.md, PLAYBOOK.md, and specific files being modified
4. **Commit format** — `feat(spi-zpp.N): <message>`
5. **Build verification** — `go build ./...` before finishing

### Phase transitions during orchestration

The wizard manages the epic's phase label:

```bash
# Before wave 0
bd label remove spi-zpp "phase:plan"
bd label add spi-zpp "phase:implement"

# After all waves complete
bd label remove spi-zpp "phase:implement"
bd label add spi-zpp "phase:review"
```

Individual subtasks don't need phase labels — they're implementation units, not workflow-tracked beads. Only the epic moves through phases.

## Lifecycle reference (worked example: spi-zpp)

This is the full cycle that produced the phase pipeline itself — dogfooding the process. This predates the formula executor; the phases are the same but the execution was manual. With the executor, `spire summon 1 --targets=spi-zpp` would run the same lifecycle automatically via the `spire-epic` formula.

### Design (interactive)
- Human + agent explored the problem space in conversation
- Produced: design decisions as bead comments, PLAYBOOK.md
- Transitioned: `phase:design` → `phase:plan`

### Plan (single agent)
- Agent read design comments, broke epic into 6 subtasks with dependency graph
- Produced: 4-wave execution plan, open questions
- **Gate pattern**: 3 open questions blocked plan→implement transition
  - Research questions → agent can answer (grep the codebase)
  - Technical judgment → agent can decide if given decision criteria
  - Scope/product → must escalate to human
- Human resolved all 3 gates, answers posted as comments
- Transitioned: `phase:plan` → `phase:implement`

### Implement (wizard + apprentices)
- Wizard dispatched wave 0 (2 parallel worktree apprentices), waited, merged, verified build, then wave 1, etc.
- **Critical step**: before dispatch, wizard checked epic comments for design corrections and updated subtask descriptions. Without this, spi-zpp.5 would have deleted molecule code that should have been preserved.
- 4 waves, 6 subtasks, 6 commits, clean build
- Transitioned: `phase:implement` → `phase:review`

### Review (sage)
- Sage reads all commits against design decisions
- Checks: does the code match the intent? Are invariants preserved?
- Outcomes: approve → merge, or request changes → wizard re-dispatches apprentice

### Key takeaways

**Description/comment divergence.** Design refinements landed as comments on the epic after subtask descriptions were already written. The playbook now requires reconciliation before dispatch — but tooling should eventually enforce this (e.g., `spire dispatch` checks for comments newer than the subtask description).

**Review checklists verify what's listed, not what's missing.** The sage verified 7 specific checks and approved — but missed that the design spec called for `[review rN]` on board cards, which wasn't implemented. A checklist catches correctness bugs but not omissions. Always include "check for design completeness" as an explicit review item.

## Sage (review agent) prompt template

When dispatching a sage, include all of the following. The template is designed to catch both correctness issues AND design omissions.

```
You are a sage — reviewing the implementation of <epic-id> (<title>).

## Context recovery

bd show <epic-id> --json
bd comments <epic-id> --json

Read ALL comments — they contain design decisions and critical corrections
that may contradict subtask descriptions. The comments are authoritative.

## Files to review

<list each changed file with a one-line description of what changed>

Also read CLAUDE.md and PLAYBOOK.md for repo conventions.

## Verification checklist

<list specific invariants to verify — these catch correctness bugs>
- If any function's deps usage changed (new GetBead, AddLabel, etc. calls), verify the test file updates mocks/stubs to match. Missing mock updates cause nil pointer dereference panics.

## Design completeness

In addition to the checklist above, review the implementation against
the FULL design spec (in the epic comments). Check for:

- Features described in the design but NOT implemented
- Behaviors specified in the design but missing from the code
- Edge cases called out in the design but not handled
- Display/output formats specified but not matching

Omissions are as important as bugs. If the design says "show [review rN]
on cards" and the code only shows [review], that's a finding.

## Output format

For each file, state: APPROVE or REQUEST_CHANGES with specific issues.
Then give an overall verdict covering both correctness AND completeness.

Post your review as a comment:
  bd comments add <epic-id> "Review: <verdict and findings>"
```

The key addition vs a naive review prompt: the **Design completeness** section. Without it, sages verify what exists but don't notice what's absent.

## Error recovery

### "no .beads/ directory found"

The command can't find the beads database. Fix:

```bash
# Check BEADS_DIR is set, or you're in a registered repo
echo $BEADS_DIR
spire repo list

# In Go code: ensure resolveBeadsDir() is called at entry
```

### "issue not found"

Check the ID is correct and includes the prefix:

```bash
bd show spi-abc        # right
bd show abc            # wrong — needs prefix
```

### "already claimed"

Someone else (or a previous session) owns this bead. Check who:

```bash
bd show spi-abc --json | jq '.owner'
```

### Stale molecules

Molecules that should be closed but aren't:

```bash
bd mol stale           # find them
bd close spi-mol-xyz   # close the root to close the molecule
```
