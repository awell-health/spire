# Components

The Workshop view is composed of seven components. This is the inventory.

```
WorkshopView                          ← workshop.jsx · entry point
├── FormulaRail                       ← workshop.jsx · left, 240px
│   └── RailSection × 2               ← embedded / custom
│       └── RailItem × N
├── FormulaHeader                     ← workshop.jsx · top, 110px
│   ├── Title block                   ← name, version, type, tags, description
│   ├── Stats strip                   ← entry, steps, terminal count, paths, runs, success, p50
│   └── TabBar                        ← graph | steps | vars | source | validation
└── (tab-dispatched body)
    ├── GraphCanvas                   ← workshop-canvas.jsx · default tab
    │   ├── (svg edge layer)
    │   └── StepCard × N
    └── StepInspector                 ← workshop-canvas.jsx · right, 320px
        ├── Step block                ← kind, action, args, when
        ├── Produces block            ← outputs declared by this step
        └── PathExplorer              ← all authored paths, click to highlight
    [or one of:]
    ├── StepsTable                    ← table of all steps
    ├── VarsTable                     ← input variables
    ├── SourceView                    ← TOML rendering
    └── ValidationView                ← structural checks + authored issues
```

## `WorkshopView`

The view shell. Owns three pieces of state:

- `selectedFormula: FormulaName` — which formula to render. Defaults to `task-default`. Updated by `FormulaRail` (click) or by the `initialFormula` prop (deep-link from BeadDetail).
- `selectedStep: StepName | null` — the focused step, drives the inspector. Cleared on formula change.
- `highlightedPath: number | null` — index into `formula.paths`. Cleared on formula change.
- `tab: "graph" | "steps" | "vars" | "source" | "validation"` — active tab.

Props:

```ts
interface WorkshopViewProps {
  onOpenBead: (bead: Bead) => void;     // unused at this checkpoint, reserved for the run-history follow-up
  initialFormula?: FormulaName;         // for deep-link from BeadDetail
}
```

When `initialFormula` changes externally, an effect re-syncs `selectedFormula`. (See `app.jsx` for how the parent threads this.)

## `FormulaRail`

Left rail. Two sections:

- **EMBEDDED** — formulas that ship with Spire.
- **CUSTOM** — formulas authored on this tower.

Each row is a `RailItem` showing:

- Formula name (mono, ink-1).
- Description (sans, ink-2, 2-line ellipsis).
- A `<TypeBadge/>` (task / epic / bug / chore / subgraph / recovery / custom).
- A small step-count chip ("6 steps").
- A red dot at the top-right *iff* `formula.issues.some(i => i.level === "error")`.

Selected row gets a 2px green left border + bg-3 background. Visual mirrors the existing Inbox/Roster row treatment in Spire — should feel native.

## `FormulaHeader`

Three rows:

1. **Title row.** Formula name (h1, 18px), version chip ("v3"), type chip ("task"), source chip ("embedded" / "custom"). On the right: a fork-copy button (ghost, no-op for now) and a "RUN ON BEAD…" green button (no-op, hooks into the file-bead modal in a future iteration).
2. **Stats strip.** Six small `Stat` cells: entry / steps / terminal count / paths / runs (30d) / success% / p50 duration. Mono labels above the value. Borrowed from the existing StatusBar style.
3. **TabBar.** Five tabs: GRAPH (with no count), STEPS (count), VARS (count), SOURCE (no count), VALIDATION (count, red if `issues.length > 0`). Active tab gets a green underline and ink-0 text.

## `GraphCanvas`

The headline view. Renders the formula as a column-packed DAG.

**Layout.** `layoutFormula(formula)` walks the steps in topological order from `entry`, assigning each step to its longest-path layer (column) and packing rows within a column in author-order. Cards are 168×60, columns are 200px wide, rows 78px tall. Padding 24px.

**Canvas.** `<svg>` for edges underneath, absolutely-positioned `<StepCard>` divs on top. Pan-on-drag and wheel-zoom; both are constrained — at this checkpoint we expect formulas to fit in the viewport at 1.0× and only enable zoom for very large epic graphs. (See `INTERACTIONS.md` for keyboard nav.)

**Edge routing.** `routeEdge(from, to)` returns an SVG path. Forward edges: right-out → orthogonal jog at the column gap → left-in. Reset edges: right-out → up over the top of the source column → left → down → left-in. Dashed amber. Multiple edges between the same column pair offset their jogs by 4px to avoid overlap.

**Selection.** Click a card → sets `selectedStep`. Click empty canvas → clears.

**Path highlight.** When `highlightedPath` is non-null, all cards and edges *not* on that path render at `opacity: 0.25`. The path's cards get a green ring; the path's edges render solid green.

## `StepCard`

The atomic unit. 168×60. Renders:

- Top row: kind chip (OP / CALL / DISPATCH) + step name (mono, ink-1).
- Title (sans, ink-2, 1-line ellipsis).
- Bottom row: small badges — `terminal` (filled red square if it's a discard-style terminal, mint if it's a close-style terminal), `when:` (amber chip if guarded), workspace icon (if step uses a workspace).

Hover: 1px green border. Selected: 2px green border + green-tinted bg. Dimmed (during path highlight): opacity 0.25.

## `StepInspector`

Right rail, 320px. Three blocks, top-down:

1. **Step block.**
   - Header: kind chip + step name + "× close" button.
   - Action row: `wizard.run` / `graph.run` / etc. with the flow or graph name as a clickable link.
   - For CALL steps: clicking the linked sub-graph name calls `onOpenFormula(name)` — swaps the canvas to that formula. (The "inline expand" follow-up is documented in `INTERACTIONS.md`.)
   - When clause: rendered in a code chip if present.
   - With args: key/value list if present.
   - Workspace: if present, name + branch-template + cleanup mode.
2. **Produces block.** One row per output. Name, type, description.
3. **PathExplorer.** "Paths through this step" — for each authored path that contains the selected step, a chip showing the full path with the selected step bolded. Click → sets `highlightedPath`. If no step is selected, shows *all* paths.

When `selectedStep === null`, the inspector renders the PathExplorer alone (full-height list of all authored paths).

## `StepsTable`

Tabular view of every step. Columns: name, kind, action, needs, terminal?, when?. Click a row → selects step + switches to Graph tab. Useful when the canvas is busy.

## `VarsTable`

The formula's input variables. Two-column layout: name + type on the left, default + description on the right. Required vars get a red ring on the type chip.

## `SourceView`

Read-only TOML rendering of the formula. The prototype uses a hand-rolled `formulaToToml(formula)` to keep the source tab self-contained. **In production, render the actual file contents** — not a regenerated approximation. This is important so operators can copy-paste from this view into a custom formula PR.

## `ValidationView`

Two stacked sections:

1. **Structural checks** (auto-derived in the prototype):
   - entry references a real step
   - at least one terminal step
   - all `needs[]` reference real steps
   - all `edges[]` connect existing steps
   - every CALL step names a sub-graph

   Each renders as a row with a green check or red cross. In production this is probably done server-side at parse time and returned as part of `FormulaDetail.checks` — feel free to refactor.

2. **Authored issues.** One card per `formula.issues[]` item: level chip (error red / warning amber), the step it's attached to (clickable, jumps to graph + selects), and the message.

Empty state: "No issues found in this formula." (mint check, ink-3).

## The new affordance: `FormulaPill`

Lives in `detail.jsx`, not in Workshop itself. It's a small button rendered at the top of the BeadDetail "details" tab that:

- Reads "FORMULA · `<name>`" with an arrow icon.
- Hovers green with a 3px phosphor halo.
- On click, calls `props.onOpenFormula(formulaName)`.

The button is a no-op `<button disabled>` when `onOpenFormula` is not passed (graceful fallback for screenshot harnesses or print views).

See `INTERACTIONS.md` for the full deep-link contract.
