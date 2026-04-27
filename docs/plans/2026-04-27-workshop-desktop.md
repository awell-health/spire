# Workshop Desktop View — Implementation Plan

**Date:** 2026-04-27
**Spec:** [`docs/design/workshop-desktop.md`](../design/workshop-desktop.md)
**Reference:** [`docs/design/workshop-desktop-handoff/`](../design/workshop-desktop-handoff/)

> Goal: ship a read-only Workshop view in Spire Desktop that surfaces
> embedded + custom formulas, the column-packed DAG, the path explorer,
> and the BeadDetail deep-link. Backed by four new gateway routes that
> are thin pass-throughs to `pkg/workshop`.

---

## Architecture recap

Two surfaces, in this order:

1. **Gateway** — four new HTTP routes under `/api/v1/workshop/*`. No new
   business logic. Handlers shape responses from `pkg/workshop` types
   into the wire types defined in the design doc.
2. **Desktop** — one new view `src/views/Workshop.tsx`, a new `api.*`
   client method group, a TS type module, an entry in the nav-rail, a
   keyboard-`5` mapping, and a `<FormulaPill/>` in the existing
   `BeadDetail`.

Phase 1 must land first so the desktop has a live target. The two
phases can otherwise progress in parallel against fixtures.

---

## Phase 1 — Gateway endpoints

### New files

#### `pkg/gateway/workshop.go`

The four handlers + the wire-type structs. ~250 LOC. Pattern mirrors
`gateway.go` handlers (`handleBoard`, `handleRoster`).

```go
package gateway

// Wire types. JSON-serialized to the desktop. Mostly a 1:1 projection
// of pkg/workshop types with the deltas listed in the design doc:
//
//   - `category` (derived from formula.DefaultV3FormulaMap) replaces
//     the handoff's `type`.
//   - `Edge.Kind` is required and explicit ("needs"|"guard"|"reset").
//   - `Stats` and `AuthoredBy` are optional in v1.

type FormulaInfoWire struct {
    Name        string   `json:"name"`
    Description string   `json:"description"`
    Source      string   `json:"source"`
    Category    string   `json:"category"`
    DefaultFor  []string `json:"default_for"`
    Version     int      `json:"version"`
    StepCount   int      `json:"step_count"`
    AuthoredBy  string   `json:"authored_by,omitempty"`
}

type FormulaDetailWire struct {
    FormulaInfoWire
    Entry      string             `json:"entry"`
    Vars       []VarWire          `json:"vars"`
    Workspaces []WorkspaceWire    `json:"workspaces"`
    Steps      []StepWire         `json:"steps"`
    Edges      []EdgeWire         `json:"edges"`
    Paths      [][]string         `json:"paths"`
    Outputs    []OutputWire       `json:"outputs"`
    Issues     []workshop.Issue   `json:"issues"`
    Stats      *StatsWire         `json:"stats,omitempty"`
}
// + StepWire, EdgeWire, VarWire, WorkspaceWire, OutputWire, StatsWire
```

Handlers:

```go
// GET /api/v1/workshop/formulas
//   Optional query params: ?source=embedded|custom|all (default: all)
//                          ?category=task|bug|...|subgraph|custom (default: any)
func (s *Server) handleWorkshopFormulas(w http.ResponseWriter, r *http.Request) {
    // 1. workshop.ListFormulas() → []FormulaInfo
    // 2. apply ?source / ?category filters
    // 3. enrich each with derived Category + DefaultFor (reverse-lookup
    //    formula.DefaultV3FormulaMap)
    // 4. writeJSON
}

// GET /api/v1/workshop/formulas/{name}
func (s *Server) handleWorkshopFormulaByName(w http.ResponseWriter, r *http.Request) {
    // Method dispatch:
    //   GET <bare>            → handleWorkshopDetail
    //   GET <name>/source     → handleWorkshopSource
    //   GET <name>/validate   → handleWorkshopValidate
}

// handleWorkshopDetail — formula.LoadStepGraphByName + workshop.DryRunStepGraph,
// then materialize edges from needs[] + when[] + resets[] with explicit kind.
func (s *Server) handleWorkshopDetail(w http.ResponseWriter, r *http.Request, name string) { ... }

// handleWorkshopSource — read raw bytes via the workshop loader and emit
// { name, source, toml }.
func (s *Server) handleWorkshopSource(w http.ResponseWriter, r *http.Request, name string) { ... }

// handleWorkshopValidate — workshop.Validate(name) → { issues: [...] }.
func (s *Server) handleWorkshopValidate(w http.ResponseWriter, r *http.Request, name string) { ... }
```

Edge materialization is the only non-trivial bit. Pseudocode:

```go
edges := []EdgeWire{}
for stepName, step := range graph.Steps {
    for _, need := range step.Needs {
        kind := "needs"
        when := ""
        if step.When != nil || step.Condition != "" {
            kind = "guard"
            when = renderWhenPredicate(step.When) // existing helper
            if when == "" { when = step.Condition }
        }
        edges = append(edges, EdgeWire{From: need, To: stepName, Kind: kind, When: when})
    }
    for _, target := range step.Resets {
        edges = append(edges, EdgeWire{From: stepName, To: target, Kind: "reset"})
    }
}
```

### Routes

Wire all four into `gateway.go`'s init block, alongside the existing
`/api/v1/*` routes (~line 110-121):

```go
mux.Handle("/api/v1/workshop/formulas",  s.corsMiddleware(s.bearerAuth(s.handleWorkshopFormulas)))
mux.Handle("/api/v1/workshop/formulas/", s.corsMiddleware(s.bearerAuth(s.handleWorkshopFormulaByName)))
```

### Tests — `pkg/gateway/workshop_test.go`

Mirror `gateway_test.go` style — table-driven HTTP tests with an
in-memory store. Cover:

1. `GET /formulas` returns all 7 embedded formulas with correct `category`
   and `default_for` for each.
2. `GET /formulas?source=embedded` filters out custom.
3. `GET /formulas/task-default` returns the canonical sample payload
   from §2.2 of the design doc.
4. `GET /formulas/subgraph-review` returns reset-edge case with
   `{ "kind": "reset" }` and a path that records the loop point.
5. `GET /formulas/does-not-exist` returns 404.
6. `GET /formulas/task-default/source` returns the raw TOML.
7. `GET /formulas/task-default/validate` returns `{ "issues": [] }`.
8. `GET /formulas/<broken>/validate` returns issues with level
   `"error"`/`"warning"`.

### Phase 1 acceptance

- `go test ./pkg/gateway/...` passes.
- Manual: `curl localhost:3030/api/v1/workshop/formulas | jq` shows all
  embedded formulas with sane shapes.

---

## Phase 2 — Desktop view port

### New files

#### `spire-desktop/src/types/workshop.ts`

The TS types from the design doc §2.1, lifted out of `types/index.ts`
to keep the existing module focused on bead/agent shapes. Exports:
`Formula`, `FormulaInfo`, `FormulaDetail`, `Step`, `StepKind`, `Edge`,
`EdgeKind`, `Var`, `Workspace`, `Output`, `Issue`, `IssueLevel`,
`Stats`, `FormulaCategory`, `FormulaSource`.

#### `spire-desktop/src/api/workshop.ts`

Workshop-specific client methods. Mirrors `api/client.ts` shape:

```ts
import type { FormulaInfo, FormulaDetail, Issue } from '../types/workshop'

export const workshopApi = {
  listFormulas: (params?: { source?: 'embedded' | 'custom'; category?: string }) =>
    request<FormulaInfo[]>(`/api/v1/workshop/formulas${qs(params)}`),

  getFormula: (name: string) =>
    request<FormulaDetail>(`/api/v1/workshop/formulas/${name}`),

  getFormulaSource: (name: string) =>
    request<{ name: string; source: string; toml: string }>(
      `/api/v1/workshop/formulas/${name}/source`,
    ),

  getFormulaValidation: (name: string) =>
    request<{ issues: Issue[] }>(`/api/v1/workshop/formulas/${name}/validate`),
}
```

`request` and `qs` are reused from `api/client.ts`.

#### `spire-desktop/src/views/Workshop.tsx`

The view shell. Port of
`docs/design/workshop-desktop-handoff/prototype/workshop.jsx`. Owns:

- `selectedFormula`, `selectedStep`, `highlightedPath`, `tab` state.
- `useEffect` to re-sync `selectedFormula` from `initialFormula` prop
  (deep-link support — see `INTERACTIONS.md`).
- Dispatches to the five tab components.

Subcomponents — keep these in **one file** for v1 (the prototype does
this too). Splitting comes later if the file gets unwieldy.

| Component | Source in prototype |
|---|---|
| `FormulaRail`, `RailSection`, `RailItem` | `prototype/workshop.jsx` |
| `FormulaHeader` | `prototype/workshop.jsx` |
| `GraphCanvas`, `StepCard`, `CanvasLegend` | `prototype/workshop-canvas.jsx` |
| `StepInspector`, `StepDetails`, `PathExplorer`, `EdgeChip` | `prototype/workshop-canvas.jsx` |
| `StepsTable`, `VarsTable` | `prototype/workshop-canvas.jsx` |
| `SourceView` | `prototype/workshop-canvas.jsx` (but reads `/source` endpoint, not `formulaToToml`) |
| `ValidationView` | `prototype/workshop-canvas.jsx` (issues from `/validate`; structural checks computed client-side as in prototype) |
| `layoutFormula`, `layerSteps`, `routeEdge` helpers | `prototype/workshop.jsx` + `workshop-canvas.jsx` |

Convert inline-style objects to TS-typed `React.CSSProperties`. Keep
the CSS variables (`var(--bg-1)` etc.) — they map 1:1 to existing
theme tokens per the handoff README.

### Modified files

#### `spire-desktop/src/App.tsx`

Three changes:

1. Import `WorkshopView` and add `'workshop'` to the `ViewId` union.
2. Pass `initialFormula` from a new `workshopFormula` state and an
   `openFormula` handler — see `INTERACTIONS.md:101-115` for the
   exact threading.
3. Add the `5` keyboard shortcut alongside the existing `1`–`4`
   handlers.

#### `spire-desktop/src/chrome.tsx`

Add a `WORKSHOP` entry to the nav rail between `GRAPH` and `METRICS`.
Same icon style as the others (the prototype hints at `Icon.workshop`,
but a `flask` or `hammer` glyph fits — match what's already in
`primitives.tsx`).

#### `spire-desktop/src/views/BeadDetail.tsx`

Add the `<FormulaPill/>` component at the top of the "details" tab,
mirroring `prototype/detail.jsx:466-511`. Two changes:

1. New prop `onOpenFormula?: (name: string) => void` on
   `BeadDetailPanel`.
2. Inside the details tab, render `<FormulaPill name={…}
   onClick={onOpenFormula ? () => onOpenFormula(formulaName) : null}/>`.
   Until `bead.formula` is plumbed, fall back to the
   `FORMULA_FOR_TYPE` lookup from `prototype/detail.jsx:253`.

### Tests

- `spire-desktop/src/views/Workshop.test.tsx` — Vitest + React Testing
  Library.
  1. Renders the rail with a fixture `FormulaInfo[]` and selects
     `task-default` by default.
  2. Clicking a step card sets `selectedStep` and renders inspector
     details.
  3. Clicking a path chip highlights edges + dims non-path cards.
  4. Switching to the Source tab fetches `/source` and renders the
     raw TOML.
  5. Switching to the Validation tab renders `{level: 'error'}`
     issues with the right styling.
- `spire-desktop/src/api/workshop.test.ts` — fetch-mock the four
  endpoints and assert response shapes match the TS types.
- `spire-desktop/src/views/BeadDetail.test.tsx` — extend the existing
  test (if any) to assert `<FormulaPill/>` renders and calls
  `onOpenFormula` on click.

### Phase 2 acceptance

- `yarn test` passes in `spire-desktop/`.
- Manual against a live tower: open the desktop, click the new
  `WORKSHOP` rail entry, see all 7 embedded formulas. Selecting
  `task-default` renders the canonical screenshot 1 layout. Selecting
  `subgraph-review` renders screenshot 2 with the dashed reset edge.
  Clicking a step opens screenshot 3's inspector. Clicking a path
  chip dims off-path cards (screenshot 4).
- Manual: open any bead in `BeadDetail`, click the `FORMULA · …` pill,
  Workshop opens with the right formula selected. Press Esc → return
  to the bead.

---

## Phase 3 — Polish (post-merge follow-ups)

These are scoped now so they don't bleed into the merge. None are
blockers for v1.

| Follow-up | Trigger |
|---|---|
| `bead.formula` field in `pkg/store` and `ApiBead`. | Once a second consumer of `formula` exists; today the type-fallback in `BeadDetail` is fine. |
| Cluster gateway custom-formula access. | Once we have an operator-level need for editing custom formulas in cluster mode. |
| `stats?: Stats` population. | Once formula-keyed run aggregation lands (probably part of the metrics work). |
| `authored_by`. | Either dolt commit author or tower-level config; defer until a stakeholder asks. |
| Inline sub-graph drilldown (replace canvas-swap with inline expand). | Tracked as a separate UX exploration in the handoff. |
| Editing UX (Source-tab edit + publish). | Tracked as a separate UX exploration in the handoff. |

---

## Sequencing & ownership

- Gateway phase is small and self-contained. Single PR. ~300 LOC + tests.
- Desktop phase is larger but mechanically driven by the prototype.
  Single PR is fine; if the diff feels noisy, split into
  (a) types + api client + view, then (b) App/chrome/BeadDetail
  wiring.
- Both PRs reference the design doc and the handoff bundle so future
  readers can re-derive intent.

## Risks

- **`workshop.DryRunStepGraph` cyclic-path enumeration.** The DFS
  bounds cycles by recording the loop point (e.g. `["sage-review",
  "fix", "sage-review"]`). Confirm in Phase 1 tests that this
  matches what the path explorer expects to render.
- **`spire-desktop` build size.** The view adds an SVG canvas + edge
  routing. ~300 LOC of view code, no new dependencies. Should not
  move the bundle meaningfully.
- **Cluster custom-formula reads.** v1 returns `[]` in cluster mode
  for custom formulas. The desktop renders an empty CUSTOM rail
  section gracefully (the handoff already does this — verify in test).
