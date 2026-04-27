# Formula data model

The Workshop view is built around three shapes:

- `FormulaInfo` — the row in the left rail (cheap to fetch in bulk).
- `FormulaDetail` — everything needed to render the canvas + tabs (fetched on selection).
- `Step` — one node in the DAG.

This document defines all three, plus the secondary types (`Edge`, `Path`, `Var`, `Workspace`, `Issue`, `KindMeta`, `Stats`), and lists the five mock formulas in `workshop-data.jsx` annotated with what each one is *demonstrating* about the design.

## TypeScript signatures

Treat these as the gateway contract Workshop expects.

```ts
type FormulaName = string;          // e.g. "task-default", "subgraph-review"
type StepName    = string;          // e.g. "plan", "implement", "sage-review"

type StepKind = "op" | "call" | "dispatch";
type IssueLevel = "warning" | "error";

interface FormulaInfo {
  name:         FormulaName;
  description:  string;
  source:       "embedded" | "custom";
  authored_by?: string;       // present iff source === "custom"
  type:         "task" | "epic" | "bug" | "chore" | "subgraph" | "recovery" | "custom";
  version:      number;
  step_count:   number;       // for the rail summary
}

interface FormulaDetail extends FormulaInfo {
  entry:       StepName;
  vars:        Var[];
  workspaces:  Workspace[];
  steps:       Step[];
  edges:       Edge[];        // explicit edges (mostly resets); needs[] is implicit
  paths:       StepName[][];  // authored end-to-end traces
  outputs:     Output[];      // formula-level outputs (only sub-graphs use this)
  issues:      Issue[];       // authored validation issues (errors / warnings)
  stats:       Stats;
}

interface Step {
  name:       StepName;
  kind:       StepKind;
  action:     string;         // "wizard.run" | "graph.run" | "git.merge_to_main" | …
  title:      string;         // human readable
  needs:      StepName[];     // upstream dependencies (drives layout)
  terminal?:  boolean;        // ends the run
  workspace?: string;         // workspace name from formula.workspaces[]
  // CALL-specific:
  graph?:     FormulaName;    // for kind === "call"
  flow?:      string;         // for wizard.run actions, the named flow
  // DISPATCH-specific:
  fanout?:    string;         // expression — e.g. "{steps.plan.outputs.subtasks}"
  // Optional in any kind:
  with?:      Record<string, string | number | boolean>;
  when?:      string;         // guard expression — e.g. "steps.review.outputs.outcome == merge"
  produces?:  Output[];
  resets?:    StepName[];     // step names this step resets when it fires (for retry loops)
}

interface Edge {
  from: StepName;
  to:   StepName;
  when?: string;              // duplicates step.when when the guard is on the edge instead
}

interface Var {
  name:         string;
  type:         "string" | "int" | "bool" | "bead_id" | "enum";
  required?:    boolean;
  default?:     string;       // string-typed for the form even on numeric vars
  values?:      string[];     // for type === "enum"
  description?: string;
}

interface Workspace {
  name:    string;
  kind:    "owned_worktree" | "shared_worktree" | "staging" | "ephemeral";
  branch:  string;             // template — "feat/{vars.bead_id}"
  base:    string;             // template
  scope:   "step" | "run";
  cleanup: "terminal" | "always" | "never";
}

interface Output {
  name:        string;
  type:        "string" | "int" | "bool" | "enum";
  values?:     string[];
  description?: string;
}

interface Issue {
  level:    IssueLevel;
  phase:    StepName;          // step the issue is attached to
  message:  string;
}

interface Stats {
  runs:         number;
  success:      number;        // 0..1
  avg_cost:     number;        // USD
  p50_duration: string;        // human "8m 12s"
}
```

## Step kind palette (`KindMeta`)

The three step kinds are visually distinct. This palette is **load-bearing** — operators learn it once and read formulas faster. Do not invent new kinds without a corresponding palette entry.

| Kind | Label | FG | BG | Glyph | Description |
|---|---|---|---|---|---|
| `op` | OP | `#86b9ff` (blue) | `rgba(90,165,255,0.10)` | `▭` | Single in-process action. Most steps. |
| `call` | CALL | `#c8adff` (violet) | `rgba(179,140,255,0.10)` | `◇` | Invokes a sub-graph by name (`graph.run`). |
| `dispatch` | DISPATCH | `#f7c948` (amber) | `rgba(247,201,72,0.10)` | `❖` | Spawns N child workers (one per item in `fanout`). |

Plus three modifiers that any kind can carry, rendered as state on the card:

| Modifier | Visual | Meaning |
|---|---|---|
| Entry | green dot at top-left of card; entry chip in header | The first step that runs. |
| Terminal | heavier border + status icon | Ends the run. |
| Reset target | dashed amber inbound edge | Some other step `resets:` this one. |

## Edge rendering

Edges fall into three classes. They look different on purpose.

| Class | Source | Visual | Notes |
|---|---|---|---|
| **Implicit forward** | `step.needs[]` | solid 1px ink-3 | The default — forward-only DAG arrows. |
| **Guarded forward** | `edges[]` with `when:` | solid 1px in the kind color, plus a small `when:` chip on the midpoint | Used when a single step has multiple outgoing branches. |
| **Reset (back-edge)** | `edges[]` where `from`'s layer ≥ `to`'s layer | dashed 1px amber, with an "RESET" pill | Shows up in `subgraph-review` (arbiter → fix → sage-review) and the `cleric-default` `retry_on_error` loop. |

**Edge routing.** The prototype uses orthogonal routing (right-out, down/up, left-in) at the layer boundaries. For very long jumps it adds a small horizontal offset per edge to avoid overlap. See `routeEdge()` in `workshop-canvas.jsx`.

## The five mock formulas in this bundle

These are the formulas in `workshop-data.jsx`. Each one is calibrated to demonstrate something specific about the design — keep them as test fixtures when you port.

### 1. `task-default` — the canonical happy path
**Demonstrates:** the smallest readable formula. Linear plan → implement → review → merge → close, with one `discard` terminal off the review step. Use this to validate the basic graph rendering, the linear-path highlight, the entry/terminal markers, and "review" being a CALL into a sub-graph.

- 6 steps, 1 sub-graph reference, 2 terminals, 2 paths.
- No issues.
- Stats present (runs: 142, p50: 8m 12s) so the header pill renders.

### 2. `epic-default` — branch + escalate
**Demonstrates:** non-linear topology. Epic-plan dispatches subtasks; if the implement sub-graph fails, the formula escalates via an `implement-failed` terminal instead of going to review. Validates: when-guards on outgoing edges, terminal-other-than-the-end-of-the-spine, and authored warnings (this formula has one — "implement-failed has no observed runs in 30 days").

- 8 steps, 2 sub-graph calls (`subgraph-implement`, `subgraph-review`), 3 terminals.
- 1 authored warning issue.

### 3. `bug-default` — convention shift
**Demonstrates:** a formula that's *almost* identical to task-default but differs in one important way (max_review_rounds: 2 instead of 3, and a "find the introducing commit" instruction in the plan step). Validates: the Vars tab and the Source tab — operators should be able to spot the difference at a glance.

### 4. `chore-default` — extra phase
**Demonstrates:** a formula with a phase that none of the others have (`document` between review and merge). Validates: the canvas adapts to formulas with more columns; the Steps tab and step rail label this step prominently.

### 5. `subgraph-review` — cycles + outputs
**Demonstrates:** the reset-edge case. Sage requests changes → fix step runs → sage re-reviews. Also has formula-level `outputs[]` (because it's called from other formulas). Validates: dashed back-edges, the "produces" inspector section, and how a CALL site references this formula by name from `task-default`.

- Has explicit `edges[]` with `when:` clauses (sage outputs `merge` / `request_changes` / `escalate` / `discard`).
- Has explicit reset-edge: `arbiter → sage-review`.
- 4 paths authored (merge / fix-loop / arbitration / discard).

## Two extra fixtures (custom formulas)

Two more formulas in the mock data show the rail's `custom` section in action:

- `cleric-default` (recovery) — has self-resets (`retry_on_error` step) and demonstrates the "self-loop" reset visual. Authored by `archmage`.
- `hotfix-fastpath` (custom) — **has 2 authored issues** (one error, one warning). Use this to validate the Validation tab's empty-state vs. populated state, and the red dot in the rail item that signals "this formula has issues".

## What you can drop

The `formulaForBead(bead)` helper at the bottom of `workshop-data.jsx` is a stopgap for the per-bead deep-link. In production, the bead record itself should carry `formula: FormulaName` (set when the bead was filed). Drop the helper and read `bead.formula` directly.
