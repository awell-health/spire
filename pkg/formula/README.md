# pkg/formula

Runtime formula schema and graph semantics (v3-only).

This package defines the data structures and low-level evaluation behavior
that the executor consumes. It is the runtime contract between formula
authoring (`pkg/workshop`) and formula execution (`pkg/executor`).

All formulas are **v3 step-graph formulas**. V2 phase-pipeline resolution,
parsing, and embedded formulas have been removed.

## What this package owns

- **Formula data structures**: `FormulaStepGraph`, `StepConfig`, `FormulaVar`,
  `RevisionPolicy`, `WorkspaceDecl`, `OutputDecl`.
- **Parsing and loading**: TOML parsing (`ParseFormulaStepGraph`, `ParseFormulaAny`),
  embedded formula loading, and v3 name resolution.
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
- **Runtime defaults**: built-in v3 formulas and v3-only resolution (`ResolveAny`, `ResolveV3`).

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

V3 formulas declare the full workflow graph with workspaces, typed
variables, conditional routing (`StructuredCondition`), reset loops
(`StepConfig.Resets`), and nested sub-graphs (`graph.run`). All bead types
(task, bug, epic, chore, feature) have v3 embedded formulas. `ResolveAny`
resolves exclusively to v3.

Remaining gap: some executor policy (e.g., retry orchestration details) is
not yet fully formula-declared.

See [docs/v3-formula.md](../../docs/v3-formula.md) for the design document
behind the v3 execution model.

## Key entrypoints

| Type / function | Purpose |
|-----------------|---------|
| **Data structures** | |
| `FormulaStepGraph` | Graph-based v3 formula model with `Workspaces`, `Vars`, explicit `Entry`, and conditional step routing. |
| `StepConfig` | Single step in a graph: `Kind`, `Action`, `When`, `Workspace`, `With`, `Produces`, `Retry`, `Resets`, `Flow`, `Graph`. |
| `StepConfig.Resets` | Steps to reset to pending after this step completes (enables review loops). |
| `FormulaVar` | Typed variable declaration (`string`, `int`, `bool`, `bead_id`). |
| `WorkspaceDecl` | Named workspace declaration with kind/branch/base/scope/ownership/cleanup. |
| `StructuredCondition` | Typed predicate condition: `All` (AND) + `Any` (OR) of `Predicate` structs. |
| `OutputDecl` | Declares graph outputs that terminal steps populate into `GraphResult.Outputs`. |
| `RevisionPolicy` | Review loop configuration (max rounds, arbiter model). Used by v3 arbiter actions. |
| **Parsing and loading** | |
| `ParseFormulaStepGraph` | Parse v3 step-graph formula from TOML bytes (applies workspace defaults, runs `ValidateGraph`). |
| `ParseFormulaAny` | Parses v3 formula from TOML bytes. Returns `*FormulaStepGraph`. |
| `LoadStepGraphByName` | Layered v3 resolution: on-disk override -> embedded default. |
| `LoadEmbeddedStepGraph` | Load a v3 formula from embedded defaults compiled into the binary. |
| `LoadReviewPhaseFormula` | Convenience loader for the embedded review-phase graph. |
| **Resolution** | |
| `ResolveAny` | Default resolution path -- v3 only. Returns `*FormulaStepGraph`. |
| `ResolveV3` | Explicit v3 resolution for a bead. |
| `ResolveV3Name` | Returns the v3 formula name for a bead without loading it. |
| `DefaultV3FormulaMap` | Bead-type -> v3 formula name mapping (task->`spire-agent-work-v3`, etc.). |
| **Graph semantics** | |
| `NextSteps` | Determine which graph steps are ready given `completed map[string]bool` and condition context. |
| `EntryStep` | Return the graph's entry step (explicit `Entry` field or implicit no-needs step). |
| `IsTerminal` | Check whether a step is terminal. |
| `ValidateGraph` | Structural validation: needs refs, self-refs, entry/terminal counts, resets targets, workspace refs, opcode validity, step kind validity, `When`/`Condition` exclusion, predicate ops, var types. |
| `ValidateWorkspaces` | Workspace declaration validation: kind, scope, ownership, cleanup, kind-specific rules. |
| **Conditions** | |
| `EvalStepCondition` | Unified entry point -- dispatches to `When` (structured) or `Condition` (string), errors if both set. |
| `EvalStructuredCondition` | Evaluate a `StructuredCondition` against a dotted-key context map. |
| `EvalCondition` | Evaluate legacy string condition expressions (`==`, `!=`, `<`, `>`, `&&`, `\|\|`). |
| **Opcodes and kinds** | |
| `OpcodeNoop` | Sentinel opcode for steps where the parent graph handles real work (nested sub-graphs). |
| `ValidOpcodes` | Registry of recognized executor opcodes. |
| `ValidStepKind` | Validates step kind: `op`, `dispatch`, `call` (empty valid for v2 compat). |
| `ValidVarType` | Validates variable type: `string`, `int`, `bool`, `bead_id`. |

## Embedded v3 formulas

The `embedded/formulas/` directory contains built-in v3 formulas compiled into
the binary:

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
