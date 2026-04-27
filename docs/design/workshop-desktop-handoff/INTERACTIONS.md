# Interactions

Selection, path highlighting, tab semantics, keyboard, and the BeadDetail → Workshop deep-link.

## Selection model

There are two independent selections:

| Selection | State | Set by | Cleared by |
|---|---|---|---|
| **Step** | `selectedStep: StepName \| null` | clicking a `StepCard`, clicking a `StepsTable` row, clicking a step name in `ValidationView` | clicking the canvas background, clicking the inspector "× close", switching formula |
| **Path** | `highlightedPath: number \| null` | clicking a chip in `PathExplorer` | clicking the same chip again, switching formula, switching tab |

Both are valid simultaneously: highlighting a path while a step is selected is the common workflow ("show me every path that goes through `merge`").

## Path highlighting

When a path is highlighted:

- Every card *not* on the path: `opacity: 0.25`.
- Every card on the path: green ring (`box-shadow: 0 0 0 2px var(--sig-green)`).
- Every edge *not* on the path: ink-4, opacity 0.20.
- Every edge on the path: solid green, opacity 1.0.
- The path-explorer chip stays "selected" (green bg) until cleared.

Reset edges that happen to be part of a path render *green* during highlight (overriding their default amber-dashed). This is intentional — the "take the fix-loop" path is a real thing the operator might want to highlight.

## Canvas pan & zoom

- **Click + drag empty canvas** → pan.
- **Wheel** → zoom around the cursor. Clamped 0.5× to 2.0×.
- **Double-click empty canvas** → fit-to-view.
- **Click empty canvas (no drag)** → clears `selectedStep`.

The canvas is *not* the focus of this checkpoint — most formulas fit at 1.0×. Don't over-engineer.

## Tab semantics

Tabs are independent of selection — switching tabs preserves `selectedStep`. The canvas is the default; the other four are reading aids.

| Tab | Body | Notes |
|---|---|---|
| **GRAPH** | `<GraphCanvas/>` | Default. The headline view. |
| **STEPS** | `<StepsTable/>` | Tabular dump. Click row → switches to GRAPH and selects step. |
| **VARS** | `<VarsTable/>` | Read-only. The future "Run on bead…" modal will render the same schema as a form. |
| **SOURCE** | `<SourceView/>` | Read-only TOML. |
| **VALIDATION** | `<ValidationView/>` | Click an issue's step name → switches to GRAPH and selects step. |

## Keyboard

Workshop participates in the global keyboard map but adds no keys at the *App* level beyond `5` to enter the view.

Inside Workshop, with no input focused:

| Key | Action |
|---|---|
| `1`–`5` | Reserved (global view switching). |
| `Q` | GRAPH tab |
| `W` | STEPS tab |
| `E` | VARS tab |
| `R` | SOURCE tab |
| `T` | VALIDATION tab |
| `Esc` | Clear path highlight, then clear step selection (two presses) |
| `J` / `K` | Cycle formulas in the rail (down / up) |
| `[` / `]` | Cycle paths in the path explorer (prev / next) |

(The Q/W/E/R/T row mirrors the existing pattern in Inbox where letter keys advance through tabs without conflicting with `1`-`5` view-switch.)

## The BeadDetail → Workshop deep-link

This is the **headline interaction added in this checkpoint**.

### Where it lives

At the top of the BeadDetail panel, immediately above the formula-lifecycle strip, there is now a small `<FormulaPill/>` button:

```
[ FORMULA · task-default  ↗ ]
```

- The pill is rendered inside the `details` tab of `<DetailPanel/>`.
- The formula name is derived from the bead's `type` field via `formulaForBead(bead)` in the prototype, or from `bead.formula` directly in production.

### What it does

On click:

1. The parent (`<App/>`) sets `view = "workshop"` and `workshopFormula = <name>`.
2. `<WorkshopView/>` re-renders with `initialFormula={workshopFormula}`. An effect re-syncs `selectedFormula`. The graph canvas paints.
3. `<DetailPanel/>` is **not** unmounted. It stays open behind the Workshop view.

This last point is the bit that matters. The mental model is "I want to see the formula behind this bead — but I'm not done with the bead." When the operator presses Esc (to close the panel) or clicks back into the bead surface, BeadDetail is right where they left it, on the comments tab or wherever they were.

### What it does *not* do

- It does not scope Workshop to "this run" — there's no live-trace overlay yet. Workshop renders the formula's static shape regardless of what the bead is currently doing.
- It does not auto-select the current step. (Future enhancement: select the step matching `bead.phase`.)
- It does not change the URL hash or browser history — Spire Desktop is an Electron app with internal routing only. If you're porting to a web build, decide separately whether the deep-link should be linkable.

### Threading the prop

```ts
// In App.tsx
const [workshopFormula, setWorkshopFormula] = useState<FormulaName>("task-default");

const openFormula = (name: FormulaName) => {
  setWorkshopFormula(name);
  setView("workshop");
};

// passed down twice:
<WorkshopView initialFormula={workshopFormula} onOpenBead={openBead} />
<DetailPanel  bead={selectedBead} onOpenFormula={openFormula} … />
```

The prototype's wiring in `app.jsx` is identical — see lines 23 (state), 56-65 (handler), 102-105 (render).

## Multiple ways to switch formulas

A reading view should make it cheap to change what you're reading. Workshop has four:

1. **Click in the rail.**
2. **Click a CALL step's sub-graph link** in the inspector. (The currently-selected formula is replaced; consider showing a small "← back" affordance — see follow-ups.)
3. **`J` / `K`** with no input focused.
4. **Deep-link from BeadDetail** as described above.

All four converge on `setSelectedFormula(name)`. There is no separate "loaded" state — the formula is the source of truth.

## Follow-ups documented elsewhere

- **Inline sub-graph drilldown** — instead of CALL clicks *replacing* the canvas, the child formula expands inline below its CALL site. Open exploration; not built.
- **Editing.** Three options sketched in the `editing-ux/` exploration folder (not part of this handoff yet).
- **Live run overlay.** Highlight "this run is at step X right now" using a green pulse. Requires gateway plumbing.
