# pkg/workshop

Formula authoring, validation, inspection, and publishing tools.

This package is the builder and workbench for formulas. It helps humans and
agents create formulas, inspect what they mean, validate them, dry-run them,
and publish them for executor use.

If `pkg/executor` runs formulas, `pkg/workshop` shapes them.

## What this package owns

- **Formula composition**: interactive and programmatic building of formulas.
- **Validation**: syntax, structure, and semantic checks for v2 and v3 formula
  shapes.
- **Dry-run and rendering**: explain what a formula would do without executing
  live work.
- **Publishing**: move formulas into the locations the runtime resolves from.
- **Defaults and templates**: provide authoring affordances for common formula
  patterns.

## What this package does NOT own

- **Live bead execution**: the workshop explains formulas; it does not run
  them against the work graph.
- **Per-bead runtime state**: state persistence and workflow progress belong in
  `pkg/executor`.
- **Subprocess lifecycle**: apprentice and sage execution belong in
  `pkg/wizard`.
- **Git semantics**: worktree, branch, merge, and commit mechanics belong in
  `pkg/git`.
- **Tower-wide coordination**: assignment and capacity management belong in
  `pkg/steward`.

## Relationship To Formula And Executor

The clean split is:
- **formula** defines the runtime data structures and graph semantics
- **workshop** helps create, validate, render, and publish those structures
- **executor** consumes the finished formula at runtime

The workshop should not hide live execution policy. If a behavior matters at
runtime, it should end up represented in the formula model, not as a workshop-
only convention.

## Key entrypoints

| Entry point | Purpose |
|-------------|---------|
| `FormulaBuilder` | Programmatic v2 formula builder. |
| `ComposeInteractive` | Interactive formula composition flow. |
| `Validate` | Validate a formula on disk or from embedded defaults. |
| `DryRun` / `DryRunStepGraph` | Simulate a formula without touching live beads. |
| `Show` | Render a human-readable explanation of a formula. |
| `Publish` | Publish a formula for runtime resolution. |

## Practical rules

1. **Keep workshop authoring-focused.** It should help humans and agents understand formulas, not execute them.
2. **Push runtime semantics into `pkg/formula`.** If a new field matters to execution, define it in the shared model instead of hiding it in workshop-only code.
3. **Validate aggressively.** The workshop is the right place to catch malformed graphs, missing terminals, invalid dispatch modes, and bad conditions before runtime.
4. **Prefer dry-run over prose.** If a workflow concept can be shown structurally, encode it and render it rather than describing it loosely in text.
5. **Track the v3 transition explicitly.** Today the builder is still mostly v2-shaped; new v3 work should narrow that gap rather than deepen the split.

## Where new work usually belongs

- Add it to **`pkg/workshop`** when the change affects formula authoring, validation, dry-run, rendering, or publishing UX.
- Add it to **`pkg/formula`** when the change affects the runtime formula schema or graph semantics.
- Add it to **`pkg/executor`** when the change affects how a declared formula is interpreted during live execution.
- Add it to **`pkg/wizard`** when the change affects how a declared subprocess step is actually run.
