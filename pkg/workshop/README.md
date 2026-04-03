# pkg/workshop

Formula authoring, validation, inspection, and publishing tools. **This package is v3-only.**

This package is the builder and workbench for v3 step-graph formulas. It helps
humans and agents create formulas, inspect what they mean, validate them,
dry-run them, and publish them for executor use.

If `pkg/executor` runs formulas, `pkg/workshop` shapes them.

## What this package owns

- **Formula composition**: interactive and programmatic building of formulas.
  `GraphBuilder` handles v3 step-graph construction (steps, workspaces, vars).
  `ComposeInteractive` drives interactive authoring of v3 step graphs.
- **Validation**: syntax, structure, and semantic checks for v3 formula shapes.
  Covers step kinds, actions/opcodes, workspace refs, structured `when`
  predicates, `produces` entries, and typed vars.
- **Dry-run and rendering**: explain what a formula would do without executing
  live work. `DryRunStepGraph` simulates v3 step graphs by walking
  entry-to-terminal paths and resolving conditions. `Show` renders v3 formulas
  with an ASCII DAG diagram and detailed step listing.
- **Publishing**: move formulas into the locations the runtime resolves from.

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
- **formula** defines the runtime data structures (`FormulaStepGraph`), graph semantics (`ValidateGraph`, `EntryStep`, step kinds, opcodes), and workspace/var declarations
- **workshop** helps create (`GraphBuilder`, `ComposeInteractive`), validate (v3 step-kind/action/workspace/when/var checks), render (`Show` with ASCII DAG and topology views), dry-run (`DryRunStepGraph`), and publish
- **executor** consumes the finished formula at runtime

The workshop should not hide live execution policy. If a behavior matters at
runtime, it should end up represented in the formula model, not as a workshop-
only convention.

## Key entrypoints

| Entry point | Purpose |
|---|---|
| `GraphBuilder` | Programmatic v3 step-graph builder (steps, workspaces, vars). |
| `ComposeInteractive` | Interactive v3 formula composition. |
| `Validate` | Multi-level v3 validation. |
| `DryRunStepGraph` | Simulate a v3 step graph -- walks entry-to-terminal paths, resolves conditions. |
| `Show` | Human-readable rendering with ASCII DAG diagram. |
| `Publish` / `Unpublish` | Publish or unpublish a formula for runtime resolution. |
| `ListFormulas` | Enumerate all available formulas with metadata. |

## Practical rules

1. **Keep workshop authoring-focused.** It should help humans and agents understand formulas, not execute them.
2. **Push runtime semantics into `pkg/formula`.** If a new field matters to execution, define it in the shared model instead of hiding it in workshop-only code.
3. **Validate aggressively.** The workshop is the right place to catch malformed graphs, missing terminals, invalid dispatch modes, and bad conditions before runtime.
4. **Prefer dry-run over prose.** If a workflow concept can be shown structurally, encode it and render it rather than describing it loosely in text.

## Where new work usually belongs

- Add it to **`pkg/workshop`** when the change affects formula authoring (`GraphBuilder`), validation, dry-run, rendering, or publishing UX.
- Add it to **`pkg/formula`** when the change affects the runtime formula schema or graph semantics.
- Add it to **`pkg/executor`** when the change affects how a declared formula is interpreted during live execution.
- Add it to **`pkg/wizard`** when the change affects how a declared subprocess step is actually run.
