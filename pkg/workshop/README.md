# pkg/workshop

Formula authoring, validation, inspection, and publishing tools.

This package is the builder and workbench for formulas. It helps humans and
agents create formulas, inspect what they mean, validate them, dry-run them,
and publish them for executor use.

If `pkg/executor` runs formulas, `pkg/workshop` shapes them.

## What this package owns

- **Formula composition**: interactive and programmatic building of formulas.
  `FormulaBuilder` handles v2 phase-pipeline construction; `GraphBuilder`
  handles v3 step-graph construction (steps, workspaces, vars).
  `ComposeInteractive` drives interactive authoring and delegates to
  `ComposeInteractiveV3` when the user selects v3.
- **Validation**: syntax, structure, and semantic checks for v2 and v3 formula
  shapes. v3 validation covers step kinds, actions/opcodes, workspace refs,
  structured `when` predicates, `produces` entries, and typed vars.
- **Dry-run and rendering**: explain what a formula would do without executing
  live work. `DryRun` simulates v2 phase pipelines; `DryRunStepGraph` simulates
  v3 step graphs by walking entry→terminal paths and resolving conditions.
  `Show` dispatches to `renderV2` or `renderV3` for human-readable output.
- **Publishing**: move formulas into the locations the runtime resolves from.
- **Defaults and templates**: provide authoring affordances for common formula
  patterns.

## What this package does NOT own

- **Live bead execution**: the workshop simulates v3 graphs (walks paths,
  resolves conditions) and explains formulas, but it never executes opcodes
  or touches the work graph. That boundary belongs to `pkg/executor`.
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
- **formula** defines the runtime data structures (`FormulaV2`, `FormulaStepGraph`), graph semantics (`ValidateGraph`, `EntryStep`, step kinds, opcodes), and workspace/var declarations
- **workshop** helps create (`FormulaBuilder`, `GraphBuilder`, `ComposeInteractive`), validate (v2 phases + v3 step-kind/action/workspace/when/var checks), render (`Show` with v2 pipeline and v3 topology views), dry-run (`DryRun` for v2, `DryRunStepGraph` for v3), and publish
- **executor** consumes the finished formula at runtime

The workshop should not hide live execution policy. If a behavior matters at
runtime, it should end up represented in the formula model, not as a workshop-
only convention.

## Key entrypoints

| Entry point | Version | Purpose |
|---|---|---|
| `FormulaBuilder` | v2 | Programmatic v2 phase-pipeline builder. |
| `GraphBuilder` | v3 | Programmatic v3 step-graph builder (steps, workspaces, vars). |
| `ComposeInteractive` | v2/v3 | Interactive formula composition; delegates to `ComposeInteractiveV3` for v3. |
| `Validate` | v2/v3 | Multi-level validation; dispatches `validateV2` or `validateV3` by version. |
| `DryRun` | v2 | Simulate a v2 phase pipeline without touching live beads. |
| `DryRunStepGraph` | v3 | Simulate a v3 step graph — walks entry→terminal paths, resolves conditions. |
| `Show` | v2/v3 | Human-readable rendering; dispatches `renderV2` or `renderV3` by version. |
| `Publish` / `Unpublish` | v2/v3 | Publish or unpublish a formula for runtime resolution. |
| `ListFormulas` | v2/v3 | Enumerate all available formulas with version-aware metadata. |

## Practical rules

1. **Keep workshop authoring-focused.** It should help humans and agents understand formulas, not execute them.
2. **Push runtime semantics into `pkg/formula`.** If a new field matters to execution, define it in the shared model instead of hiding it in workshop-only code.
3. **Validate aggressively.** The workshop is the right place to catch malformed graphs, missing terminals, invalid dispatch modes, and bad conditions before runtime.
4. **Prefer dry-run over prose.** If a workflow concept can be shown structurally, encode it and render it rather than describing it loosely in text.
5. **Maintain v2/v3 parity in workshop features.** Both `FormulaBuilder` and `GraphBuilder` are first-class; new validation, dry-run, or rendering features should cover both versions.

## Where new work usually belongs

- Add it to **`pkg/workshop`** when the change affects formula authoring (`FormulaBuilder` or `GraphBuilder`), validation, dry-run, rendering, or publishing UX.
- Add it to **`pkg/formula`** when the change affects the runtime formula schema or graph semantics.
- Add it to **`pkg/executor`** when the change affects how a declared formula is interpreted during live execution.
- Add it to **`pkg/wizard`** when the change affects how a declared subprocess step is actually run.
