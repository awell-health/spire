# Formula V3 Draft

> Proposed target model. This is not the current runtime contract.
>
> Current state:
> - lifecycle formulas are still mostly v2 phase configs
> - review is the only real step-graph flow today
> - the executor still contains significant lifecycle policy

This document describes the intended v3 direction: formulas define the
workflow, and the executor becomes a generic interpreter over that workflow.

The goal is to close the gap between the current formula-parameterized
executor and the target ZFC-compliant executor.

## Design Goals

1. **Formula owns workflow shape.** Ordering, routing, retries, and terminal
   paths should live in the formula, not in executor switch statements.
2. **Executor stays mechanical.** It should gather context, evaluate declared
   conditions, persist state, and invoke declared actions.
3. **Wizard owns subprocess lifecycle.** Prompt assembly, Claude execution,
   validation, commit behavior, and `result.json` remain in `pkg/wizard`.
4. **Git stays in `pkg/git`.** Workspace creation, resume, branch semantics,
   merge mechanics, and commit detection stay out of the executor.
5. **Workshop owns authoring.** The Workshop should validate, dry-run, build,
   and publish formulas that the executor can interpret directly.

## Boundaries

### Executor

Owns:
- loading a formula
- restoring and persisting execution state
- resolving ready steps
- evaluating declared conditions
- activating and closing workflow beads
- invoking declared runtime actions
- enforcing structural and policy checks

Does not own:
- hidden routing logic
- prompt composition details
- local git semantics
- formula authoring

### Wizard

Owns:
- running one subprocess role in one assigned workspace
- implementation, review-fix, build-fix, and sage-review flows
- prompt, timeout, validation, and commit semantics

Does not own:
- deciding which step runs next
- deciding whether a path uses staging or feature workspaces

### `pkg/git`

Owns:
- workspaces, branches, merges, refs, SHAs

Does not own:
- bead or workflow policy

### Workshop

Owns:
- building, validating, dry-running, and publishing formulas

Does not own:
- live bead execution

## Proposed Runtime Shape

The runtime uses a single graph-based model (`FormulaStepGraph`).

```toml
name = "spire-epic-v3"
version = 3
entry = "design-check"

[vars]
# typed runtime inputs and counters

[workspaces]
# named workspace declarations

[steps]
# graph nodes
```

## Core Concepts

### Vars

`vars` are typed runtime inputs and counters.

Examples:
- `bead_id`
- `base_branch`
- `max_review_rounds`
- `max_build_fix_rounds`

These should be typed, not stringly.

### Workspaces

Workspaces are first-class formula data. The formula decides where work runs.
The executor resolves the declaration and `pkg/git` implements it.

Proposed workspace kinds:
- `repo`
- `owned_worktree`
- `borrowed_worktree`
- `staging`

Suggested fields:
- `kind`
- `branch`
- `base`
- `scope` (`step` or `run`)
- `ownership` (`owned` or `borrowed`)
- `cleanup` (`always`, `terminal`, `never`)

This is the key fix for the class of bugs where code silently chooses the
wrong branch or worktree.

### Steps

Each step is a node in the workflow graph.

Suggested fields:
- `kind`: `op`, `dispatch`, or `call`
- `needs`: predecessor steps
- `when`: structured condition
- `action`: executor opcode
- `workspace`: named workspace
- `with`: typed inputs for the action
- `produces`: declared outputs
- `retry`: optional retry policy
- `terminal`: whether success ends the workflow

### State

Executor state should become generic and step-oriented.

Suggested shape:
- `steps.<name>.status`
- `steps.<name>.outputs`
- `counters.<name>`
- `workspaces.<name>`
- `active_step`

That is preferable to hardcoded fields like `ReviewRounds`,
`BuildFixRounds`, and step-specific executor locals.

## Conditions

The current review graph uses string conditions over a `map[string]string`.
That was a good bridge, but it is not the final target.

The v3 target should use structured predicates so the executor is evaluating
data, not parsing workflow logic from strings.

Example:

```toml
[steps.fix.when]
all = [
  { left = "steps.review.outputs.verdict", op = "eq", right = "request_changes" },
  { left = "state.counters.review_round", op = "lt", right = "vars.max_review_rounds" }
]
```

This keeps condition evaluation mechanical and ZFC-compliant.

## Minimum Opcode Set

The executor should only know how to run a small, stable set of actions.

Suggested starting set:

- `check.design-linked`
  Mechanical validation that the bead has the required linked design context.
- `wizard.run`
  Run one subprocess role through `pkg/wizard`.
- `beads.materialize_plan`
  Create child beads and dependency edges from structured plan output.
- `dispatch.children`
  Dispatch work across child beads using a declared strategy.
- `verify.run`
  Run build, test, or lint commands in a workspace.
- `graph.run`
  Execute a nested graph and return its declared outputs.
- `git.merge_to_main`
  Land a staging branch on the base branch.
- `bead.finish`
  Close, discard, relabel, or otherwise finalize the bead.

The important point is not the exact list. The point is that the executor
should interpret opcodes, not embed hidden workflow policy.

## Proposed `wizard.run` Flows

`wizard.run` should stay the bridge to subprocess work that requires model
judgment.

Proposed `flow` values:
- `task-plan`
- `epic-plan`
- `implement`
- `review-fix`
- `build-fix`
- `sage-review`

The formula chooses the flow and workspace. The wizard performs it.

## Proposed `dispatch.children` Behavior

This opcode handles the mechanical orchestration of child work once the
formula has declared the strategy.

Suggested declared inputs:
- source child set
- strategy: `direct`, `sequential`, `dependency-wave`
- child action: usually `wizard.run`
- child flow: usually `implement`
- child workspace template
- integration workspace
- verification command(s)
- build-fix retry policy

That keeps wave execution and integration declarative without making each
formula manually encode loop mechanics.

## Epic Example

Below is a proposed `spire-epic-v3` shape. This is not intended to parse
today; it is a target runtime contract.

```toml
name = "spire-epic-v3"
description = "Design-gated epic execution with declarative planning, wave implementation, staged review, and merge"
version = 3
entry = "design-check"

[vars.bead_id]
type = "bead_id"
required = true

[vars.base_branch]
type = "string"
required = true

[vars.max_review_rounds]
type = "int"
default = 3

[vars.max_build_fix_rounds]
type = "int"
default = 2

[workspaces.staging]
kind = "staging"
branch = "epic/{vars.bead_id}"
base = "{vars.base_branch}"
scope = "run"
ownership = "owned"
cleanup = "terminal"

[workspaces.child]
kind = "owned_worktree"
branch = "feat/{child.id}"
base = "{workspaces.staging.head}"
scope = "step"
ownership = "owned"
cleanup = "always"

[steps.design-check]
kind = "op"
action = "check.design-linked"
produces = ["design_ref"]

[steps.plan]
kind = "op"
needs = ["design-check"]
action = "wizard.run"
flow = "epic-plan"
with.context = ["CLAUDE.md", "docs/ARCHITECTURE.md", "docs/ZFC.md"]
produces = ["plan"]

[steps.materialize-plan]
kind = "op"
needs = ["plan"]
action = "beads.materialize_plan"
with.plan = "steps.plan.outputs.plan"
produces = ["children", "dependency_graph"]

[steps.implement]
kind = "dispatch"
needs = ["materialize-plan"]
action = "dispatch.children"
workspace = "staging"
with.children = "steps.materialize-plan.outputs.children"
with.strategy = "dependency-wave"
with.child_action = "wizard.run"
with.child_flow = "implement"
with.child_workspace = "child"
with.integration = "merge-into-staging"
with.verify_build = "{repo.runtime.build}"
with.on_build_failure = "wizard.run"
with.failure_flow = "build-fix"
with.max_failure_rounds = "vars.max_build_fix_rounds"
produces = ["implemented_children", "staging_head"]

[steps.review]
kind = "call"
needs = ["implement"]
action = "graph.run"
graph = "review-default"
workspace = "staging"
with.branch = "{workspaces.staging.branch}"
with.max_rounds = "vars.max_review_rounds"
produces = ["outcome"]

[steps.merge]
kind = "op"
needs = ["review"]
action = "git.merge_to_main"
workspace = "staging"
when.any = [
  { left = "steps.review.outputs.outcome", op = "eq", right = "approve" },
  { left = "steps.review.outputs.outcome", op = "eq", right = "merge" },
  { left = "steps.review.outputs.outcome", op = "eq", right = "split" }
]
with.build = "{repo.runtime.build}"
with.test = "{repo.runtime.test}"
produces = ["merge_sha"]

[steps.close-success]
kind = "op"
needs = ["merge"]
action = "bead.finish"
with.status = "closed"
terminal = true

[steps.discard]
kind = "op"
needs = ["review"]
action = "bead.finish"
when.all = [
  { left = "steps.review.outputs.outcome", op = "eq", right = "discard" }
]
with.status = "closed"
with.reason = "discarded_after_review"
terminal = true
```

### What this epic example moves into the formula

- design gating
- planning step
- child bead materialization
- dependency-wave implementation strategy
- use of a shared staging workspace
- build-fix retry policy
- review subgraph invocation
- merge vs discard routing
- terminal closure path

### What it leaves out of the formula

- how `pkg/git` creates or resumes the staging worktree
- how `pkg/wizard` structures prompts and validates output
- how state is persisted
- how step beads are tracked

That is the intended split.

## Review Subgraph Shape

The current review graph is the right pattern, but it should become a normal
formula-selected nested graph instead of an embedded executor special case.

Its outputs should be typed and stable:
- `outcome`: `approve | request_changes | merge | split | discard`
- `verdict`
- `arbiter_decision`
- `rounds_used`

Then outer graphs can route mechanically from those outputs.

## Migration Order

This is the recommended order from the current system to the v3 target:

1. **Formula-selectable review graph**
   Stop hardcoding review graph loading in the executor.
2. **First-class workspaces**
   Make staging, borrowed, and owned worktrees declarative.
3. **Structured conditions**
   Replace string condition parsing with typed predicates.
4. **Generic graph execution**
   Replace phase/behavior dispatch with step interpretation.
5. **Declarative child dispatch**
   Move wave, sequential, and direct child execution behind a declared opcode.
6. **Workshop parity**
   Teach Workshop to build, validate, dry-run, and publish the full v3 model.

## Why This Is More ZFC-Compliant

Under this model:
- formulas declare what should happen
- the model provides the judgment-heavy outputs inside wizard steps
- local code validates and executes those declarations mechanically

That is much closer to the ZFC rule than today's executor, which still
contains meaningful lifecycle policy in Go code.
