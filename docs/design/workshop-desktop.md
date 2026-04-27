# Design: Workshop view in Spire Desktop

> Bring formulas to the desktop. Read-first. Backed by `pkg/workshop` and a
> small set of new gateway routes. UX reference is the design package
> imported at [`workshop-desktop-handoff/`](workshop-desktop-handoff/).

## Purpose

Today the only way to inspect the formulas that drive bead lifecycles is
to `cat` TOML files on the steward host or run `spire workshop show
<name>` from a terminal attached to the tower. That is fine for authors;
it is invisible to operators using the desktop.

The Workshop view closes that gap. It surfaces the loaded formulas, lets
an operator read each one as a column-packed DAG, and lets them trace
the authored execution paths — without leaving the desk and without any
mutations against the tower.

This document is the engineering spec. The visual reference lives in the
imported handoff bundle; this doc is what the gateway and the desktop
view code must agree on.

---

## 1. Scope

### In scope (v1)

- Browse all formulas loaded on the active tower (embedded + custom).
- Render any formula as a column-packed DAG with kind-typed step cards
  (OP / CALL / DISPATCH).
- Surface entry, terminals, `when`-guards, `produces`, reset-edges, and
  `needs` visually.
- Step inspector rail showing kind, action, flow, role, model, timeout,
  workspace, `when`, `with`, `resets`, and incident edges.
- Path explorer listing every authored end-to-end path; selecting one
  highlights it on the canvas.
- Five tabs per formula: **Graph** (default) · **Steps** · **Vars** ·
  **Source** · **Validation**.
- Per-bead deep-link: clicking the new `FormulaPill` in `BeadDetail`
  switches to Workshop scoped to the bead's formula. The detail panel
  stays mounted so the operator can return.
- Keyboard: `5` enters the view; `Q` `W` `E` `R` `T` cycle the five
  tabs; `J` `K` cycle formulas in the rail; `[` `]` cycle paths.

### Out of scope (v1 — deferred)

These are deliberate omissions documented in the handoff. Each is
tracked as a follow-up exploration and **must not** leak into v1
handlers or types.

| Deferred | Why we are not doing it now |
|---|---|
| **Editing.** No TOML editor, no diff, no save flow, no validate-on-edit. | Requires a separate UX exploration. The Source tab is read-only file render. |
| **Inline sub-graph drilldown.** | When a `CALL` step is clicked, the canvas swaps to the named child formula. Inline expansion is a follow-up. |
| **Run history per formula.** | The handoff renders a `stats` strip (runs / success / p50). Backend has no run-by-formula aggregation. v1 omits the strip when stats are absent — this is a property of the response, not the view. |
| **Live run overlay.** | Highlight "this current run is at step X" needs gateway plumbing we don't have. |
| **`RUN ON BEAD…`** button. | Filing a bead with a formula override is not supported by `spire file` today. Defer. |
| **`FORK COPY`** button. | Authoring lives behind the editing exploration. Defer. |

### Not a goal

- Replacing the CLI workshop. `spire workshop *` keeps working.
- Live execution telemetry. That is metrics-view territory.
- Editing or publishing in v1.

---

## 2. Gateway API contract

All routes live under `/api/v1/workshop/*`, CORS- and Bearer-wrapped via
the existing middleware in `pkg/gateway/gateway.go`. Handlers are thin
pass-throughs to `pkg/workshop`; no new business logic in the gateway.

| Method | Path | Body | Returns | Backed by |
|---|---|---|---|---|
| `GET` | `/api/v1/workshop/formulas` | — | `FormulaInfo[]` | `workshop.ListFormulas()` + `formula.DefaultV3FormulaMap` |
| `GET` | `/api/v1/workshop/formulas/{name}` | — | `FormulaDetail` | `formula.LoadStepGraphByName()` + `workshop.DryRunStepGraph()` |
| `GET` | `/api/v1/workshop/formulas/{name}/source` | — | `{ name, source, toml }` | `loadRawFormula()` (workshop internal) |
| `GET` | `/api/v1/workshop/formulas/{name}/validate` | — | `{ issues: Issue[] }` | `workshop.Validate()` |

**Why no separate `/dry-run`.** The handoff bundles "what is it" + "what
would happen" into one detail fetch. Folding `paths[]` and `var_types`
into the detail response keeps the desktop client simple and matches the
prototype's `getFormula()` shape.

**Why a separate `/source`.** The Source tab must render the actual TOML
file contents (so operators can copy-paste into a custom formula PR),
not a regenerated approximation (`COMPONENTS.md:124`). Keeping it
separate avoids forcing the detail handler to ship raw bytes.

### 2.1 Response shapes

Final TypeScript signatures the gateway is committed to. These match the
shapes in `workshop-desktop-handoff/FORMULAS.md` with the deltas listed
in §3.

```ts
type FormulaName = string;          // "task-default", "subgraph-review"
type StepName    = string;
type StepKind    = "op" | "call" | "dispatch";
type IssueLevel  = "warning" | "error";
type EdgeKind    = "needs" | "guard" | "reset";
type FormulaSource = "embedded" | "custom";
type FormulaCategory =
  | "task" | "bug" | "epic" | "chore" | "feature"
  | "recovery" | "subgraph" | "custom";

interface FormulaInfo {
  name:         FormulaName;
  description:  string;
  source:       FormulaSource;
  authored_by?: string;          // OPTIONAL in v1; see §3
  category:     FormulaCategory; // derived from DefaultV3FormulaMap; see §3
  default_for:  string[];        // bead types this formula is the default for
  version:      number;
  step_count:   number;
}

interface FormulaDetail extends FormulaInfo {
  entry:       StepName;
  vars:        Var[];
  workspaces:  Workspace[];
  steps:       Step[];
  edges:       Edge[];
  paths:       StepName[][];     // derived via DFS, not authored
  outputs:     Output[];
  issues:      Issue[];
  stats?:      Stats;            // OPTIONAL in v1; see §3
}

interface Step {
  name:       StepName;
  kind:       StepKind;
  action:     string;
  title:      string;
  needs:      StepName[];
  terminal?:  boolean;
  workspace?: string;
  graph?:     FormulaName;       // for kind === "call"
  flow?:      string;            // for wizard.run actions
  role?:      string;            // backward compat field
  model?:     string;
  timeout?:   string;
  with?:      Record<string, string | number | boolean>;
  when?:      string;            // rendered string form of StructuredCondition
  produces?:  string[];
  resets?:    StepName[];
  verdict_only?: boolean;
}

interface Edge {
  from: StepName;
  to:   StepName;
  kind: EdgeKind;                // "needs" | "guard" | "reset"
  when?: string;
}

interface Var {
  name:         string;
  type:         "string" | "int" | "bool" | "bead_id" | "enum";
  required?:    boolean;
  default?:     string;
  values?:      string[];
  description?: string;
}

interface Workspace {
  name:    string;
  kind:    "owned_worktree" | "shared_worktree" | "borrowed_worktree" | "staging" | "ephemeral";
  branch:  string;
  base:    string;
  scope?:  "step" | "run";
  ownership?: string;
  cleanup?: "terminal" | "always" | "never";
}

interface Output {
  name:        string;
  type:        "string" | "int" | "bool" | "enum";
  values?:     string[];
  description?: string;
}

interface Issue {
  level:    IssueLevel;
  phase:    StepName;            // or "" for formula-level findings
  message:  string;
}

interface Stats {                // OPTIONAL — may be undefined in v1
  runs:         number;
  success:      number;          // 0..1
  avg_cost:     number;          // USD
  p50_duration: string;          // human-formatted "8m 12s"
}
```

### 2.2 Sample payload — `GET /api/v1/workshop/formulas/task-default`

This is the canonical "open a formula" response.

```json
{
  "name": "task-default",
  "description": "Standard agent work: plan → implement → review → merge",
  "version": 3,
  "source": "embedded",
  "category": "task",
  "default_for": ["task", "feature"],
  "step_count": 6,
  "entry": "plan",

  "vars": [
    { "name": "bead_id", "type": "bead_id", "required": true,
      "description": "The bead being worked on" },
    { "name": "base_branch", "type": "string", "default": "main" },
    { "name": "max_review_rounds", "type": "string", "default": "3" }
  ],

  "workspaces": [
    { "name": "feature", "kind": "owned_worktree",
      "branch": "feat/{vars.bead_id}", "base": "{vars.base_branch}",
      "scope": "run", "cleanup": "terminal" }
  ],

  "steps": [
    { "name": "plan", "kind": "op", "action": "wizard.run",
      "flow": "task-plan", "title": "Plan implementation", "needs": [] },
    { "name": "implement", "kind": "op", "action": "wizard.run",
      "flow": "implement", "title": "Implement changes",
      "needs": ["plan"], "workspace": "feature" },
    { "name": "review", "kind": "call", "action": "graph.run",
      "graph": "subgraph-review", "title": "Review changes",
      "needs": ["implement"], "workspace": "feature" },
    { "name": "merge", "kind": "op", "action": "git.merge_to_main",
      "title": "Merge to main", "needs": ["review"],
      "workspace": "feature", "with": { "strategy": "squash" },
      "when": "steps.review.outputs.outcome == merge" },
    { "name": "close", "kind": "op", "action": "bead.finish",
      "title": "Close bead", "needs": ["merge"],
      "with": { "status": "closed" }, "terminal": true },
    { "name": "discard", "kind": "op", "action": "bead.finish",
      "title": "Discard branch", "needs": ["review"],
      "with": { "status": "discard" }, "terminal": true,
      "when": "steps.review.outputs.outcome == discard" }
  ],

  "edges": [
    { "from": "plan", "to": "implement", "kind": "needs" },
    { "from": "implement", "to": "review", "kind": "needs" },
    { "from": "review", "to": "merge", "kind": "guard",
      "when": "steps.review.outputs.outcome == merge" },
    { "from": "review", "to": "discard", "kind": "guard",
      "when": "steps.review.outputs.outcome == discard" },
    { "from": "merge", "to": "close", "kind": "needs" }
  ],

  "paths": [
    ["plan", "implement", "review", "merge", "close"],
    ["plan", "implement", "review", "discard"]
  ],

  "outputs": [],
  "issues": []
}
```

The cyclic case (`subgraph-review`) emits the same shape with
`{ "kind": "reset" }` edges and a path that records the loop point —
e.g. `["sage-review", "fix", "sage-review"]`. The DFS path enumerator
in `workshop.DryRunStepGraph` already produces this; we surface it
unchanged.

---

## 3. Deltas from the handoff package

Three places where this spec departs from `workshop-desktop-handoff/`.

### 3.1 `category` instead of `type`

The handoff uses `type` on `FormulaInfo`. That collides with `bead.type`
and confuses the desktop's existing `BeadType` union in
`spire-desktop/src/types/index.ts`. Renamed to `category`. Same values,
same semantics.

`category` is **derived**, not authored:

- Reverse-lookup of `formula.DefaultV3FormulaMap` → `task` / `bug` /
  `epic` / `chore` / `feature` / `recovery` (whichever bead type maps
  to this formula).
- If no default mapping and the name starts with `subgraph-` →
  `subgraph`.
- Otherwise → `custom`.

### 3.2 `stats` is optional in v1

The handoff renders runs / success% / p50 in the header. Spire has no
formula-keyed run aggregation today. The header conditionally renders
the stats strip (`workshop.jsx:295`), so we ship `stats: undefined` in
v1 and the strip simply hides itself. When run aggregation lands, the
field can populate without a contract change.

### 3.3 `authored_by` is optional in v1

The handoff shows author names on custom formulas in the rail. Spire
does not track formula authorship. Cheap sources later: dolt commit
author for the on-disk file, or a tower-level config default. v1
returns `authored_by: undefined` and the rail collapses the field
gracefully.

### 3.4 `Edge.kind` is required and explicit

The handoff uses `edges[]` for "explicit edges (mostly resets)" and
treats `needs[]` as implicit forward edges. The gateway materializes
**all** edges into `edges[]` with an explicit `kind` field
(`"needs" | "guard" | "reset"`) so the renderer never has to recompute
graph topology. `needs[]` stays on each step for completeness.

This is a small denormalization that pays for itself: it removes the
"compute successors from needs + sniff resets" logic from the
client. The server already builds this map via
`workshop.buildSuccessorMap`.

### 3.5 `paths[]` is derived

The handoff README's open-question 3 asks whether paths are authored
or derived. Answer: **derived** via DFS in
`workshop.DryRunStepGraph` (`pkg/workshop/dryrun.go:158-191`). The
empty-state copy on the path explorer should read "No paths reachable
from entry," not "No authored paths."

---

## 4. Desktop view shape

The desktop side ports the prototype in
`workshop-desktop-handoff/prototype/` to a single new view file
`spire-desktop/src/views/Workshop.tsx`, plus a small new affordance in
the existing `BeadDetail`. No new primitives. No new theme tokens.

### 4.1 Component inventory

```
WorkshopView                          ← src/views/Workshop.tsx (new)
├── FormulaRail                       ← left, 252px
├── FormulaHeader                     ← top, identity + stats + tabs
└── (tab-dispatched body)
    ├── GraphCanvas + StepInspector   ← default tab
    ├── StepsTable
    ├── VarsTable
    ├── SourceView                    ← reads /source endpoint
    └── ValidationView                ← reads /validate endpoint
```

The component contracts in
[`workshop-desktop-handoff/COMPONENTS.md`](workshop-desktop-handoff/COMPONENTS.md)
are the source of truth for component-level behaviors. The port should
not reinvent layout, palette, or selection model — the handoff is
authoritative on those.

### 4.2 New affordance in BeadDetail

A `<FormulaPill>` button at the top of the BeadDetail "details" tab.
Clicking it calls `props.onOpenFormula(formulaName)` and the App
switches to Workshop scoped to that formula. The detail panel stays
mounted behind. See
[`workshop-desktop-handoff/INTERACTIONS.md`](workshop-desktop-handoff/INTERACTIONS.md)
for the full deep-link contract.

The formula name comes from `bead.formula` if the gateway populates it
(see §6). Until then, the desktop falls back to a `FORMULA_FOR_TYPE`
lookup keyed on `bead.type` — exactly what the prototype does in
`detail.jsx:253`.

### 4.3 Selection model

Two independent selections (`COMPONENTS.md:31` and `INTERACTIONS.md:6`):

- `selectedStep: StepName | null` — drives the inspector.
- `highlightedPath: number | null` — drives the path-highlight pass.

Both reset when the formula changes. Both can be active at once.

### 4.4 Routing

Workshop is a new top-level view. Prototype assigns it the 5th nav-rail
entry between Graph and Metrics, with keyboard shortcut `5`. The
desktop's existing view-switching pattern in `App.tsx` is the only
integration point.

---

## 5. Open questions resolved

The handoff README raises four engineering questions. All answered now:

1. **Schema source of truth.** Gateway parses formulas via
   `pkg/workshop.ListFormulas()` / `Show()`. Those already do the
   embedded + on-disk merge with the right resolution order (custom
   overrides embedded). The TOML on disk stays authoritative; no
   gateway-side cache.

2. **Hot reload.** Defer. Custom-formula churn is rare and v1 has no
   editing path. Client-side polling on tab focus is sufficient until
   editing lands.

3. **`paths[]` authored vs derived.** Derived via DFS — see §3.5.

4. **Validation severity.** `workshop.Issue.Level` is already
   `"error" | "warning"`. Matches the handoff's `IssueLevel` enum 1:1.

---

## 6. Cluster mode caveats

In cluster deployments, custom formulas live in the tower's dolt-side
`.beads/formulas/` directory. The gateway pod must read those files for
the `custom` rail section to populate.

**v1 cluster behavior:** the four endpoints work; embedded formulas
return correctly; custom formulas return `[]` until the gateway's
formula-resolution path can read tower-side formula bytes. This matches
the existing cluster pattern where `reset` returns `501` until plumbed.

A small follow-up: a `bead.formula` field on `ApiBead` so the
`FormulaPill` can read it directly without the type-fallback in
`detail.jsx:253`. Cheap to add in `pkg/store` once we need it.

---

## 7. Reference materials

- **`workshop-desktop-handoff/`** — frozen design package from
  claude-design. Treat as a read-only port reference. Includes:
  - `README.md` — overview, fidelity, file inventory.
  - `COMPONENTS.md` — per-component contracts.
  - `FORMULAS.md` — TS signatures + annotated mock formulas.
  - `INTERACTIONS.md` — selection, path highlighting, keyboard, deep-link.
  - `screenshots/` — four canonical states.
  - `prototype/` — JSX prototype (port reference; not production code).

- **`pkg/workshop/`** — Go side. Already implements `ListFormulas`,
  `Show`, `Validate`, `DryRunStepGraph`, `Publish`, `Unpublish`. The
  gateway endpoints are thin pass-throughs. See `pkg/workshop/README.md`.

- **`pkg/formula/`** — runtime schema. `FormulaStepGraph`, `StepConfig`,
  `WorkspaceDecl`, `StructuredCondition`, `OutputDecl`. See
  `pkg/formula/README.md`.

- **`pkg/gateway/gateway.go`** — existing route registration patterns
  (lines 110-121). New routes follow the same `corsMiddleware(bearerAuth(...))`
  wrapping.

- **Implementation plan:** [`docs/plans/2026-04-27-workshop-desktop.md`](../plans/2026-04-27-workshop-desktop.md).
