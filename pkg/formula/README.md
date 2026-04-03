# pkg/formula

Runtime formula schema and graph semantics.

This package defines the data structures and low-level evaluation behavior
that the executor consumes. It is the runtime contract between formula
authoring (`pkg/workshop`) and formula execution (`pkg/executor`).

It supports two formula versions:
- **v3 step-graph formulas** — the default runtime model. `ResolveAny`
  resolves to v3 unless the bead carries a `formula-version:2` label.
- **v2 phase-pipeline formulas** — legacy sequential phases, opt-in via
  the `formula-version:2` label.

## What this package owns

- **Formula data structures**: `FormulaV2`, `FormulaStepGraph`, phase config,
  step config, vars, revision policy, and output declarations.
- **Parsing and loading**: TOML parsing (`ParseFormulaV2`, `ParseFormulaStepGraph`,
  `ParseFormulaAny`), embedded formula loading, and name resolution.
- **Graph semantics**: step readiness (`NextSteps`), entry detection (`EntryStep`),
  terminal detection (`IsTerminal`), and graph validation (`ValidateGraph`).
- **Condition evaluation**: legacy string conditions (`EvalCondition`) and typed
  `StructuredCondition` with `Predicate`-based evaluation (`EvalStructuredCondition`,
  `EvalStepCondition`).
- **Workspace declarations**: `WorkspaceDecl` types with kind/scope/ownership/cleanup
  constants and validation (`workspace.go`).
- **Opcode and step-kind registry**: `ValidOpcodes`, `ValidStepKind`, step kinds
  (`op`, `dispatch`, `call`), and the `noop` sentinel opcode (`opcode.go`).
- **Formula variable schema**: `FormulaVar` with typed variable validation
  (`string`, `int`, `bool`, `bead_id`).
- **Runtime defaults**: built-in formulas, default formula maps, and version-aware
  resolution (`ResolveAny`).

## What this package does NOT own

- **Formula authoring UX**: composition, dry-run presentation, and publishing
  belong in `pkg/workshop`.
- **Live execution**: phase loops, step execution, state persistence, and
  runtime routing belong in `pkg/executor`.
- **Subprocess behavior**: prompt, Claude, validation, and commit logic belong
  in `pkg/wizard`.
- **Git semantics**: worktrees, branches, merges, and SHAs belong in `pkg/git`.

## Relationship to workshop and executor

The clean split is:
- **formula** defines the executable shape
- **workshop** helps humans and agents create and inspect that shape
- **executor** interprets that shape at runtime

If behavior matters during execution, it should be representable here.
If it is only a UI, authoring, or explanation concern, it should stay in
`pkg/workshop`.

## Current state

V3 formulas now declare the full workflow graph with workspaces, typed
variables, conditional routing (`StructuredCondition`), reset loops
(`StepConfig.Resets`), and nested sub-graphs (`graph.run`). All bead types
(task, bug, epic, chore, feature) have v3 embedded formulas and `ResolveAny`
defaults to v3 resolution.

V2 phase-pipeline formulas remain available as a legacy opt-in via the
`formula-version:2` label.

Remaining gap: some executor policy (e.g., retry orchestration details) is
not yet fully formula-declared.

See [docs/v3-formula.md](../../docs/v3-formula.md) for the design document
behind the v3 execution model.

## Key entrypoints

| Type / function | Purpose |
|-----------------|---------|
| **Data structures** | |
| `FormulaV2` | Legacy phase-pipeline formula model. |
| `FormulaStepGraph` | Graph-based v3 formula model with `Workspaces`, `Vars`, explicit `Entry`, and conditional step routing. |
| `StepConfig` | Single step in a graph: `Kind`, `Action`, `When`, `Workspace`, `With`, `Produces`, `Retry`, `Resets`, `Flow`, `Graph`. |
| `StepConfig.Resets` | Steps to reset to pending after this step completes (enables review loops). |
| `FormulaVar` | Typed variable declaration (`string`, `int`, `bool`, `bead_id`). |
| `WorkspaceDecl` | Named workspace declaration with kind/branch/base/scope/ownership/cleanup. |
| `StructuredCondition` | Typed predicate condition: `All` (AND) + `Any` (OR) of `Predicate` structs. |
| `OutputDecl` | Declares graph outputs that terminal steps populate into `GraphResult.Outputs`. |
| **Parsing and loading** | |
| `ParseFormulaAny` | Version-peeking parser: returns `*FormulaV2` or `*FormulaStepGraph` + version int. |
| `ParseFormulaV2` | Parse v2 formula from TOML bytes. |
| `ParseFormulaStepGraph` | Parse v3 step-graph formula from TOML bytes (applies workspace defaults, runs `ValidateGraph`). |
| `LoadFormulaByName` | Layered v2 resolution: on-disk override → embedded default. |
| `LoadStepGraphByName` | Layered v3 resolution: on-disk override → embedded default. |
| `LoadEmbeddedStepGraph` | Load a v3 formula from embedded defaults compiled into the binary. |
| `LoadReviewPhaseFormula` | Convenience loader for the embedded review-phase graph. |
| **Resolution** | |
| `ResolveAny` | Default resolution path — v3 by default, v2 only when `formula-version:2` label present. |
| `ResolveV3` | Explicit v3 resolution for a bead. |
| `Resolve` | Legacy v2-only resolution for a bead. |
| `DefaultV3FormulaMap` | Bead-type → v3 formula name mapping (task→`spire-agent-work-v3`, etc.). |
| `DefaultFormulaMap` | Bead-type → v2 formula name mapping (legacy). |
| **Graph semantics** | |
| `NextSteps` | Determine which graph steps are ready given `completed map[string]bool` and condition context. |
| `EntryStep` | Return the graph's entry step (explicit `Entry` field or implicit no-needs step). |
| `IsTerminal` | Check whether a step is terminal. |
| `ValidateGraph` | Structural validation: needs refs, self-refs, entry/terminal counts, resets targets, workspace refs, opcode validity, step kind validity, `When`/`Condition` exclusion, predicate ops, var types. |
| `ValidateWorkspaces` | Workspace declaration validation: kind, scope, ownership, cleanup, kind-specific rules. |
| **Conditions** | |
| `EvalStepCondition` | Unified entry point — dispatches to `When` (structured) or `Condition` (string), errors if both set. |
| `EvalStructuredCondition` | Evaluate a `StructuredCondition` against a dotted-key context map. |
| `EvalCondition` | Evaluate legacy string condition expressions (`==`, `!=`, `<`, `>`, `&&`, `\|\|`). |
| **Opcodes and kinds** | |
| `OpcodeNoop` | Sentinel opcode for steps where the parent graph handles real work (nested sub-graphs). |
| `ValidOpcodes` | Registry of recognized executor opcodes. |
| `ValidStepKind` | Validates step kind: `op`, `dispatch`, `call` (empty valid for v2 compat). |
| `ValidVarType` | Validates variable type: `string`, `int`, `bool`, `bead_id`. |

## Embedded v3 formulas

The `embedded/formulas/` directory contains built-in formulas compiled into the
binary. V3 formulas and their default mappings:

| Formula | Default for | Purpose |
|---------|-------------|---------|
| `spire-agent-work-v3` | task, chore, feature | Standard agent work lifecycle. |
| `spire-bugfix-v3` | bug | Bug-fix lifecycle. |
| `spire-epic-v3` | epic | Epic lifecycle with design-link check, plan materialization, child dispatch, and merge. |
| `review-phase` | (nested) | Review sub-graph with sage review, arbiter escalation, and revision loops. Uses `noop` terminals. |
| `epic-implement-phase` | (nested) | Epic implementation sub-graph for dispatching and verifying child beads. |

Nested formulas (`review-phase`, `epic-implement-phase`) are invoked via
`graph.run` steps in parent formulas. Their terminal steps use `OpcodeNoop`
because the parent graph handles the actual lifecycle transition.

## Practical rules

1. **Keep this package declarative.** It defines what a formula can say, not how a bead should be driven through it.
2. **Avoid application policy leakage.** Runtime branching policy should be declared in formula data, not smuggled into parser helpers or condition code.
3. **Prefer typed schema growth over stringly conventions.** If a behavior matters, add a real field or type instead of encoding it in magic strings.
4. **Make invalid workflows impossible or obvious.** Structural validation belongs here.
5. **Use this package to reduce executor code, not justify more of it.** New formula expressiveness should pull policy out of `pkg/executor`.

## Where new work usually belongs

- Add it to **`pkg/formula`** when the runtime schema, graph semantics, or condition model changes.
- Add it to **`pkg/formula`** when adding new opcodes, step kinds, workspace kinds, or structured condition operators.
- Add it to **`pkg/workshop`** when authoring, validation UX, rendering, or publish behavior changes.
- Add it to **`pkg/executor`** when the interpreter changes but the schema does not.
- Add it to **`docs/v3-formula.md`** when the target execution model evolves.
