# Handoff: Workshop view (`src/views/Workshop.tsx`)

## Overview

Workshop is a **new top-level view** in Spire Desktop. It surfaces the formulas that drive bead lifecycles — the DAGs the steward and wizards execute when they pick up a bead — and gives the operator a place to *read*, *trace*, and (eventually) *edit* those formulas without leaving the desk.

Today the formulas live in TOML files under `cfg/formulas/*.toml` and are only inspectable by SSHing into the steward and `cat`-ing files. That's the gap this view closes.

This is a **read-first** design. Editing is intentionally absent from this checkpoint — it's tracked as a separate UX exploration (see "Out of scope" below). What ships in this handoff is the entire reading + tracing surface.

The view answers four operator questions:

1. **What formulas are loaded on this tower right now?** → Left rail.
2. **What does this formula *do* — what are its steps, in what shape, with what guards?** → Graph canvas (default tab) + Steps table.
3. **Why is *this run* doing what it's doing?** → Path explorer + Validation tab.
4. **What inputs does it take and what does it produce?** → Vars tab + Source tab.

## About the design files

The files in this bundle are **design references created in HTML**. `workshop.jsx` and `workshop-canvas.jsx` are babel-transpiled React prototypes that run in a single-file HTML harness. They are **not** production code to copy verbatim — Spire Desktop is a TypeScript Electron + Vite app, and these prototypes use plain JS, inline styles, and a flat in-memory `FORMULAS` array.

Your task is to **port the design into a new `src/views/Workshop.tsx`**, keeping the existing primitives, theme tokens, and routing intact. Specifically:

- **Reuse** `STATUS_COLOR`, `Icon.*`, `relTime`, the existing tab-button visual treatment, and the `--bg-*` / `--ink-*` / `--sig-*` CSS variables. They map 1:1 to what's in this prototype.
- **Reuse** the existing slide-in `<DetailPanel/>`. Workshop does *not* introduce a new panel; instead it adds a new affordance (the FORMULA pill) inside the existing one — see `INTERACTIONS.md`.
- **Add** a new top-level route `workshop`. The prototype wires it as the 5th nav-rail entry (keyboard `5`), between Graph and Metrics.
- **Add** a gateway endpoint that returns the loaded `FormulaInfo[]` and per-formula `FormulaDetail` (schema in `FORMULAS.md`).

## Fidelity

**High-fidelity.** Match colors, spacing, typography, and the column-packed DAG layout in the prototype exactly. The kind colors (OP blue, CALL violet, DISPATCH amber) are a deliberate small palette — do not invent new ones.

## Files in this bundle

| File | Purpose |
|---|---|
| `README.md` | This file. |
| `workshop.jsx` | Top-level view shell: layout, formula state, tab dispatch, BeadDetail integration. Treat as the spec. |
| `workshop-canvas.jsx` | Graph canvas, step inspector, and the four non-graph tabs (Steps / Vars / Source / Validation). |
| `workshop-data.jsx` | Mock `FORMULAS` array + `KIND_META` step-kind palette + `formulaForBead(bead)` helper. **Reference only** — your implementation should fetch from the gateway. |
| `detail.jsx` | The existing BeadDetail panel, with the new `<FormulaPill/>` affordance + `onOpenFormula` callback wired in. Diff against your current `DetailPanel.tsx` to find the new bits. |
| `app.jsx` | Excerpted to show how `workshopFormula` state and the `onOpenFormula` handler are threaded from `App` → `WorkshopView` and `App` → `DetailPanel`. |
| `primitives.jsx` | Mock copies of `<Icon/>`, `<TypeBadge/>`, etc. Mirrors `src/primitives.tsx`. |
| `theme.css` | The CSS variables this prototype relies on. Should be a no-op against your existing theme. |
| `FORMULAS.md` | Data-model spec: `FormulaInfo`, `FormulaDetail`, `Step`, `Edge`, `Path`, `Issue`. Plus the five mock formulas in this bundle, annotated. |
| `COMPONENTS.md` | Component inventory + per-component contract. |
| `INTERACTIONS.md` | Selection model, path highlighting, tab semantics, keyboard, and the BeadDetail → Workshop deep-link. |
| `screenshots/` | Screenshots of four canonical states (default, sub-graph, step selected, path highlighted). The BeadDetail → Workshop pill is documented in `INTERACTIONS.md`; it's easier to demo live than to capture. |

## What's in scope for this checkpoint

- ✅ Browse the loaded formulas (left rail, embedded vs. custom split).
- ✅ Render any formula as a column-packed DAG with per-kind step cards (OP / CALL / DISPATCH).
- ✅ Surface entry, terminals, when-guards, produces, and reset-edges visually.
- ✅ Step inspector rail: title, action, args, when-clause, produces, paths-through-this-step.
- ✅ Path explorer: list every authored path; clicking one highlights it on the graph and dims the rest.
- ✅ Tabs: Graph (default), Steps (table), Vars (input schema), Source (TOML), Validation (structural checks + authored issues).
- ✅ Per-bead deep-link: clicking the FORMULA pill in BeadDetail jumps into Workshop scoped to that formula. The detail panel stays mounted behind so the operator can return.
- ✅ Keyboard: `5` to enter the view; tab Q/W/E/R/T to switch sub-tabs (see `INTERACTIONS.md`).

## Out of scope for this checkpoint

These are **deliberate omissions**, not oversights. Each is tracked as a follow-up exploration:

- ❌ **Editing.** The Source tab is read-only TOML rendering. No diff view, no save flow, no validation-on-edit. Editing UX is the next design exploration.
- ❌ **Sub-graph drilldown.** A `CALL` step references another formula by name (e.g. `subgraph-review`), and clicking it in the inspector currently *swaps* the canvas to that formula. The "inline expand the child DAG in place" interaction is an open exploration.
- ❌ **Run history.** The `stats` block (runs, success, p50) is rendered in the header but doesn't link to a runs list. That's a Metrics-view concern, not Workshop.
- ❌ **Live run overlay.** Highlighting "this current run is at step X" on the canvas would be lovely but requires gateway plumbing we don't have yet.

## Glossary

A few terms from the prototype that may not match your gateway vocabulary 1:1 — confirm before porting:

| Term | Meaning |
|---|---|
| **Formula** | A named DAG that drives a bead lifecycle. Lives in `cfg/formulas/<name>.toml`. Has steps, edges, vars, workspaces. Equivalent to "graph" in some internal docs. |
| **Step** | A node in the formula. Has a `kind` (op / call / dispatch), an `action`, optional `when` clause, optional `with` args, optional `produces` outputs, and a `needs[]` array of upstream step names. |
| **Edge** | An explicit `from → to` link. *Implicit* edges from `needs[]` are also rendered; `edges[]` is mostly used for **reset-edges** (cycles back upstream when the formula needs to retry). |
| **Path** | An authored end-to-end trace through the formula — a sequence of step names from entry to a terminal. The operator clicks a path to highlight it. |
| **OP step** | Single in-process action. Blue. (`wizard.run`, `git.merge_to_main`, `bead.finish`.) |
| **CALL step** | Invokes a sub-graph. Violet. (`graph.run` with a `graph: <name>` arg.) |
| **DISPATCH step** | Spawns N child workers. Amber. (Used in `subgraph-implement` to dispatch one wizard per planned subtask.) |
| **Reset-edge** | An edge that points from a step back to an *earlier* step. Renders dashed amber. The simplest example: `arbiter → fix → sage-review` in `subgraph-review`. |
| **Terminal step** | A step with `terminal: true`. Renders with a heavier border and a status icon. Every formula must have at least one. |
| **Embedded formula** | Ships in the Spire binary (`cfg/formulas/embedded/`). Listed first in the rail. |
| **Custom formula** | Authored on this tower, lives in dolt. Listed below embedded with the authoring agent's name. |

## Open questions for engineering

1. **Schema source of truth.** The TOML files in `cfg/formulas/*.toml` are the authoritative source today. Does the gateway already parse and cache them, or does Workshop need to read TOML directly?
2. **Hot reload.** When a custom formula is saved (out of scope for this checkpoint), should Workshop subscribe to a tower event and re-render?
3. **`paths[]` authoritativeness.** In the prototype, `paths[]` is hand-authored on each formula. In production, are paths *derived* from the DAG (via topological enumeration) or *declared* by the formula author? The UI works for either, but the wording on the path-explorer empty state ("No authored paths" vs "Could not enumerate paths") depends on the answer.
4. **Validation severity.** The `Issue.level` enum is `warning` | `error` in this prototype. Is there an existing severity scale on the gateway side?
