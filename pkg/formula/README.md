# pkg/formula

Runtime formula schema and graph semantics.

This package defines the data structures and low-level evaluation behavior
that the executor consumes. It is the runtime contract between formula
authoring (`pkg/workshop`) and formula execution (`pkg/executor`).

Today it contains both:
- **v2 phase-pipeline formulas**
- **v3-style step-graph primitives**, currently used most clearly by review

## What this package owns

- **Formula data structures**: `FormulaV2`, `FormulaStepGraph`, phase config,
  step config, vars, and revision policy.
- **Parsing and loading**: TOML parsing, embedded formula loading, and name
  resolution.
- **Graph semantics**: step readiness, entry detection, terminal detection,
  and graph validation.
- **Condition evaluation**: low-level predicate evaluation over declared
  runtime context.
- **Runtime defaults**: built-in formulas and default formula resolution.

## What this package does NOT own

- **Formula authoring UX**: composition, dry-run presentation, and publishing
  belong in `pkg/workshop`.
- **Live execution**: phase loops, step execution, state persistence, and
  runtime routing belong in `pkg/executor`.
- **Subprocess behavior**: prompt, Claude, validation, and commit logic belong
  in `pkg/wizard`.
- **Git semantics**: worktrees, branches, merges, and SHAs belong in `pkg/git`.

## Relationship To Workshop And Executor

The clean split is:
- **formula** defines the executable shape
- **workshop** helps humans and agents create and inspect that shape
- **executor** interprets that shape at runtime

If behavior matters during execution, it should be representable here.
If it is only a UI, authoring, or explanation concern, it should stay in
`pkg/workshop`.

## Current state vs target state

Current state:
- `FormulaV2` still describes most lifecycle behavior as ordered phases
- `FormulaStepGraph` exists for v3-style graph execution, especially review
- the executor still contains meaningful lifecycle policy that is not yet
  fully declared in formulas

Target state:
- formulas declare the full workflow graph
- workspaces, routing, retries, and terminal behavior are data
- the executor becomes a generic interpreter over that declared graph

See [docs/v3-formula.md](../../docs/v3-formula.md) for the current draft of
that target model.

## Key entrypoints

| Type / function | Purpose |
|-----------------|---------|
| `FormulaV2` | Current phase-pipeline formula model. |
| `FormulaStepGraph` | Graph-based formula model for v3-style execution. |
| `ParseFormulaV2` / `ParseFormulaStepGraph` | Parse formulas from TOML. |
| `LoadFormulaByName` | Resolve and load a lifecycle formula. |
| `LoadReviewPhaseFormula` | Load the current embedded review graph. |
| `NextSteps` | Determine which graph steps are ready. |
| `ValidateGraph` | Enforce basic structural graph correctness. |
| `EvalCondition` | Evaluate declared conditions against runtime context. |

## Practical rules

1. **Keep this package declarative.** It defines what a formula can say, not how a bead should be driven through it.
2. **Avoid application policy leakage.** Runtime branching policy should be declared in formula data, not smuggled into parser helpers or condition code.
3. **Prefer typed schema growth over stringly conventions.** If a behavior matters, add a real field or type instead of encoding it in magic strings.
4. **Make invalid workflows impossible or obvious.** Structural validation belongs here.
5. **Use this package to reduce executor code, not justify more of it.** New formula expressiveness should pull policy out of `pkg/executor`.

## Where new work usually belongs

- Add it to **`pkg/formula`** when the runtime schema, graph semantics, or condition model changes.
- Add it to **`pkg/workshop`** when authoring, validation UX, rendering, or publish behavior changes.
- Add it to **`pkg/executor`** when the interpreter changes but the schema does not.
- Add it to **`docs/v3-formula.md`** when the target execution model evolves.
