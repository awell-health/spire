# Delivery Evaluation — Design Notes

Status: **design / not yet built.** This document captures the design for a
human-facing delivery-evaluation feature for Spire, plus the existing data
model it would stand on and the risks to be aware of before building.

The near-term goal is narrow and explicit: **human review ergonomics.** Make
it painless for a human to sit down with a delivered task or epic, understand
what was asked, see what the agent actually did (grounded in captured truth),
and judge the quality of the delivery. A closed feedback loop (accumulating
score signal so agents measurably improve) is a *later* goal and is noted only
where it affects today's design.

---

## 1. Motivation

To make agents more efficient we want an evaluation on each delivery, and —
crucially — the ability to understand *what caused a poor delivery and what
fix would have prevented it*. That second step requires human judgment, and to
exercise it a reviewer needs two things in front of them:

1. **What the original specs were.**
2. **Which decisions the agent(s) took**, ideally grounded in truths (e.g. links
   to the documentation in the repo the agent actually read).

### Why not the cleric?

The cleric is the **failure-recovery** role — it only fires when a bead fails
(diagnose → execute → learn). It therefore gives no signal on the deliveries
that *succeeded but could have been better*, which is most of them. It is the
wrong instrument for an every-delivery eval.

### The every-delivery eval already (partly) exists: the sage review

Every bead runs through the review sub-graph
(`pkg/formula/embedded/formulas/subgraph-review.formula.toml`):
`sage-review → (fix loop / arbiter) → merge/discard`. The **sage runs on every
delivery**, not just failures, and emits a typed verdict. So Spire already has
a per-delivery evaluator — the gap is that its output is a gate decision
(`approve` / `request_changes`), not a structured, reviewable score, and the
review is spread across several beads and commands.

---

## 2. The problem with the status quo: a 4-command ritual

Today, reviewing one delivery means stitching together four sources by hand:

| Source | Command | What it gives |
|--------|---------|---------------|
| Spec + context | `spire focus <bead-id>` (or `grok`) | Title/description, linked design beads (`discovered-from`), related deps, comments, review summary |
| Execution timeline | `spire trace <bead-id>` | The execution DAG: `step` beads, `attempt` beads, `review` rounds (status, duration) |
| Decision trail | `spire attempt show <attempt-id>` | Per-invocation tool calls — `Read` file paths, `Bash` commands, `Grep` patterns — grounded in captured spans |
| Verdict + findings | `bd show <review-bead-id>` | `review_verdict`, `review_findings` (JSON), error/warning counts |

A four-command ritual won't be done twice. This is the case for a single
consolidated command.

---

## 3. Proposed command: `spire eval <bead-id>`

A **read-only** command that assembles the whole dossier in one shot.

```
spire eval <bead-id> [--json] [--all] [--since-cycle N]

SPEC
  Title / Description
  Design beads (discovered-from)          ← inlined
  Plan output (plan step)
EXECUTION
  Trace timeline: steps · attempts · review rounds (status, duration)
RELEVANT DECISIONS  (filtered tool-call trail, per attempt)
  ✓ Read  docs/architecture.md            ← doc read (always relevant)
  ✓ Read  docs/ZFC.md
  ✓ Bash  go test ./pkg/recovery/...
  · 14 generic file reads hidden (--all to show)
REVIEW
  verdict: request_changes (round 2/3)
  findings: [ ... review_findings ... ]
```

Design principles:

- **Human-first.** Default output is a rendered report. `--json` emits the same
  dossier as one structured object (the natural input to a future LLM rubric).
- **Complete, not truncated.** No paging-by-default, no "run again with `--full`"
  for the relevant content. Volume is controlled by the **relevance filter**
  (below), not by truncating output. `--all` reveals filtered-out noise.
- **Epics recurse one level**: a per-child summary, with `spire eval <child>` to
  drill in. (Walking an epic = walking each delivered child the same way.)
- **No new schema.** It composes existing readers: `storeGet*`
  (bead/design/comments/reviews), `buildTrace`, and
  `observability.ListAttemptToolCalls`.

### Where it would live

- New `cmd/spire/eval.go` (top-level, like `focus` / `trace` / `grok`).
- New `EvalConfig` block in `pkg/repoconfig/repoconfig.go`.
- Registered in the `pkg/scaffold` catalog and `docs/cli-reference.md`.

---

## 4. Configurable tool-call relevance

A reviewer does not care about *all* tool calls — but any read of a
documentation file is always relevant. Tool calls are captured as
`olap.ToolCallRecord{ ToolName, Attributes, Step, Success, Timestamp, ... }`,
where `Attributes` is the lifted-arg JSON (file path for `Read`, command for
`Bash`, pattern for `Grep`). That makes "relevant = any `Read` whose path is a
doc" a clean, buildable filter.

The *what-counts* judgment is repo-specific, so it belongs in `spire.yaml`
(sibling to the existing `cleric:` block), not hardcoded — see ZFC note below.

```yaml
eval:
  relevant_tools: [Read, Grep, Bash]      # tools considered at all
  doc_globs:                               # always-relevant reads
    - "docs/**"
    - "**/*.md"
    - "adr/**"
  exclude_globs:                           # always-drop noise
    - "node_modules/**"
    - "**/*_test.go"
  default_reads: hidden                     # non-doc reads: hidden | shown
  include_failed: true
```

Filter pipeline, per attempt:

1. Keep `ToolName ∈ relevant_tools`.
2. Parse the path/command from `Attributes`.
3. `doc_globs` match ⇒ always keep.
4. `exclude_globs` match ⇒ drop.
5. Otherwise apply `default_reads`.

The stated rule ("ignore generic reads, keep doc reads") is just
`default_reads: hidden` + `doc_globs`. `--all` overrides to show everything.

### Beyond globs: spec-relative relevance (future)

The glob filter answers "which tool calls do I want to see." The sharper eval
question is "did the agent ground itself in the decisions it *should* have?"
Because a bead carries `discovered-from` links to its design bead — which in
turn references the docs that matter — a later version could compute
**expected context** (docs referenced by the spec/design) vs **actual context**
(doc reads in the attempt trail) and surface the *gap* as a finding. The glob
config is the cheap v1; the spec-relative version is where it becomes
genuinely diagnostic.

---

## 5. The data model `spire eval` stands on

### Spec side

- **Work bead** Title + Description — the ask.
- **Design beads** linked via `discovered-from` — pre-work exploration and
  decisions (`bd show <design-id>` + comments).
- **Plan step** output (tasks/epics run a `plan` phase).
- Epic decomposition = child beads (`parent-child`).

### Decisions side (internal bead taxonomy — see `docs/INTERNAL-BEADS.md`)

For one `task-default` run the engine creates, under the work bead:

```
task spi-xxxx                         (work bead)
├── attempt: wizard-spi-xxxx          type=attempt   (per claim; tool calls captured here)
├── step:plan / implement / review …  type=step      (one per formula phase)
└── review-round-1 …                  type=review    (sage verdict + findings)
```

- **`attempt` beads** — per execution try; labels `agent:<name>`,
  `branch:<branch>`, `result:<outcome>`. The captured tool calls hang off the
  attempt.
- **`review` beads** — `review_verdict`, `review_findings` (JSON array of
  `ReviewFinding`), error/warning counts, round.

### Triangulation for "what caused a low score"

The human-judgment input the reviewer needs comes from joining three things:

- **review_findings** (what the sage flagged) ×
- **attempt tool calls** (what the agent did / didn't read) ×
- **spec + design beads** (what it should have done).

Example: a finding plus an attempt trail that *never opened the design doc it
should have* is a precise, grounded diagnosis.

---

## 6. The eval rubric (criteria) — repo-owned

Per Spire's Zero Framework Cognition boundary (`docs/ZFC.md`): *"opinionated
quality judgment beyond structural or policy checks"* is **out of Spire's
scope**, and local code must not invent product reasoning that should come from
the model. Therefore the eval **rubric** — what "good" means for a delivery —
lives in the **repo being evaluated**, not in Spire core. Spire provides the
mechanism + storage; the repo provides the criteria.

What a repo can do **today** (no Spire change):

1. **Rubric-as-context** — author the rubric as a repo doc (e.g.
   `docs/eval-rubric.md`) and add it to `spire.yaml` `context: [...]` so every
   agent (and the sage) reads it. Also reference it in `AGENTS.md` / `SPIRE.md`.
2. **Deterministic gate** — encode the mechanical slice of the rubric as a
   repo script and wire it as `runtime.lint` / `ci_lint` / `ci_test` in
   `spire.yaml`. It runs on every delivery and must pass before merge.

What needs a (well-scoped) Spire change for a **first-class scored** rubric:

- A generic `evaluate` flow in `pkg/wizard` (sibling to `sage-review`) whose
  system prompt is generic ("score this delivery against the rubric below,
  emit JSON `{score, dimensions[]}`") and whose rubric body is read from a repo
  file (e.g. `.spire/eval-rubric.md`).
- An `evaluate` step in a repo-level formula override
  (`.beads/formulas/task-default.formula.toml`) that `produces` the score.
- Persistence (see §7).

This split keeps every repo able to supply its own rubric without forking
prompts. It is **not** required for the human-first command in §3.

---

## 7. Observability dependency — load-bearing

The "decisions grounded in truth" half of `spire eval` is only as good as the
tool-call capture, so understand its failure modes.

**How capture works.** The OTLP receiver lives **inside the daemon**
(`pkg/steward/daemon.go`). `spire up` starts a gRPC receiver on `localhost:4317`
(or `SPIRE_OTLP_PORT`) wired to the OLAP store (DuckDB locally at
`tower.OLAPPath()`; ClickHouse in cluster). Spire spawns Claude Code agents with
`CLAUDE_CODE_ENABLE_TELEMETRY=1` + `OTEL_EXPORTER_OTLP_ENDPOINT` pointing at it
(`pkg/agent/spawn_process.go`). Agents stream tool-call spans → daemon →
OLAP store. `spire attempt show` reads them back.

**Capture is silent-off and not retroactive.** If the receiver fails to start,
the daemon logs `"telemetry disabled"` and runs normally — you simply get no
tool calls, and there is no way to recover them after the fact.

### What can compromise it (≈ likelihood order, local setup)

1. **Daemon not running during the agent's run.** No daemon → no receiver →
   tool calls are lost permanently. Always `spire up` *before* summoning work
   you intend to review.
2. **Non-Claude provider.** Telemetry env is injected only for
   `provider: claude` (or unset). Beads run under `codex` / `cursor` emit no
   tool-call spans. Check `agent.provider` in `spire.yaml`.
3. **Port mismatch.** Receiver and spawner both default to `4317` and both honor
   `SPIRE_OTLP_PORT`; setting it for one environment but not the other (or a
   `4317` conflict) drops spans. Receiver-start failure is swallowed.
4. **Isolation backend.** Process mode uses `localhost:4317`. Docker/k8s require
   the endpoint be reachable from the container; an empty `OTLPEndpoint`
   disables injection entirely.
5. **Hard kill / drain deadline.** A SIGKILLed or drain-timeout agent loses its
   late spans. Clean completion is fine.
6. **Span → log degradation.** The rich args the doc-filter needs (a `Read`'s
   file path) ride on enhanced-telemetry spans
   (`CLAUDE_CODE_ENHANCED_TELEMETRY_BETA=1`). If that upstream beta changes,
   `attempt show` can fall back to `source: log` rows with the tool name but no
   path — outside Spire's control.
7. **Two pipelines — don't confuse them.** `spire metrics` reads DuckDB
   aggregates ETL'd from Dolt `agent_runs`; `attempt show` reads the live OTLP
   span ingest. One can be healthy while the other is empty. Never infer one
   from the other.

### Configurability

There is **no explicit "enable observability" switch**; capture is on by
default whenever the daemon is up and the provider is claude — the failure mode
is silent-off, not configured-off. Knobs: `SPIRE_OTLP_PORT` (set for both
daemon and spawner, or neither), backend (DuckDB vs ClickHouse),
`agent.provider`.

---

## 8. Open questions

- **Ephemeral vs persisted.** §3 is on-demand and thrown away — fine for human
  review today. A feedback loop would need durable, queryable eval records
  (score + findings-that-caused-it + the human's "what would've prevented
  this"). The reserved `golden_prompts` table (defined in
  `cmd/spire/tower.go`, currently **unpopulated** — no writer exists) or a new
  `delivery_evals` table is where that would go.
- **Audience.** Build human-first; if the rendered report is good, the LLM
  rubric just consumes the `--json`.
- **Relevance: globs vs spec-relative** (see §4).

---

## 9. Status & next steps

- This is a **design**, not yet implemented. No `spire eval` command exists.
- Before building anything: **verify capture is live.** `spire up`, run one real
  claimed task, then `spire attempt show <attempt-id>` — confirm span-sourced
  rows *with file paths*. If empty or log-only, the first work item is fixing
  capture, not building the command.
- Suggested build order: (1) read-only `spire eval` dossier with sensible
  hardcoded relevance defaults; (2) `spire.yaml` `eval:` config; (3) persistence
  + the LLM `--score` rubric hook (the closed-loop phase).

---

## Appendix — key source references

| Concern | Location |
|---------|----------|
| Review sub-graph (every-delivery eval) | `pkg/formula/embedded/formulas/subgraph-review.formula.toml` |
| Internal bead taxonomy (attempt/step/review) | `docs/INTERNAL-BEADS.md`, `pkg/store/beadtypes.go` |
| Tool-call record shape | `pkg/olap/olap.go` (`ToolCallRecord`) |
| `attempt show` | `cmd/spire/attempt.go`, `pkg/observability/attempt_calls.go` |
| `trace` | `cmd/spire/trace.go` |
| `focus` / `grok` | `cmd/spire/focus.go`, `cmd/spire/grok.go` |
| Repo config schema | `pkg/repoconfig/repoconfig.go` |
| OTLP receiver / capture | `pkg/steward/daemon.go`, `pkg/agent/spawn_process.go`, `pkg/otel/writer.go` |
| ZFC boundary | `docs/ZFC.md` |
| Golden prompts (reserved, unpopulated) | `cmd/spire/tower.go` |
