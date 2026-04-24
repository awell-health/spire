I have enough context. Here's the concise implementation plan.

---

# Plan: `/api/v1/metrics` endpoint + supporting OLAP views (spi-3xbcmv)

## TL;DR

Replace the 501 stub at `pkg/gateway/gateway.go:114` with a real handler that parses `scope`/`window`/`aspirational` and dispatches to a new `pkg/metrics/report` package. That package assembles a `MetricsResponse` (shape defined by `spire-desktop/src/views/metrics/types.ts`, camelCase) from DuckDB. Add three new materialized views, reuse the seven existing ones. Performance target <500ms on a 6.7k-bead tower.

---

## 1. Authoritative contract note — the bead description is out of date

The bead prose shows snake_case/nested schema (`deploy_frequency`, `per_week`, `lead_time_p50_ms`, `scope:{kind,prefix}`). **The real TypeScript contract is different**:

- All fields camelCase
- `scope` is a plain string (`'all'` or a repo prefix), not an object
- `window` is a plain range string, not an object
- `hero` uses `deployFreqPerWeek`, `leadTimeP50Seconds`, `costPerWeekUSD`, etc.
- Lifecycle is `{byType:[{type, stages:[{stage:'F→R'|'F→S'|'F→C'|'R→S'|'R→C'|'S→C', p50, p75, p95, p99, outliers:[{beadId, durationSeconds}]}]}]}` — p75/p99 **required**, outliers are objects not plain IDs
- Times are **seconds**, not ms
- BugAttachment is `{weekly:[{weekStart, byParentType:[{parentType, parents, parentsWithBugs}], recentParents:[{beadId, title, bugCount}]}]}` — no `pct`, no cumulative; `parentType` is restricted to `'task'|'epic'|'feature'`
- Phases have `reachedFromStart` (the funnel count), not `entered`/`completed` pair
- Aspirational is `{lifecycleByType?, bugAttachmentWeekly?}` — a separate parallel object, **not** inline `synthetic: true` flags. The bead's "badge synth rows" language is wrong; the UI badges whole panels when `aspirational` is populated.

**Action:** mirror `spire-desktop/src/views/metrics/types.ts` exactly. Cross-validate with `spire-desktop/fixtures/metrics-sample.json` to catch casing slips. Commit a JSON schema generated from the TS (per acceptance #1) and wire it into CI as a round-trip test.

## 2. Files to change

### New — `pkg/metrics/report/` (new package, co-located with existing `pkg/metrics/recorder.go`)
- `report.go` — exported `Build(ctx, r Reader, scope, window, aspirational) (*Response, error)` — the orchestrator called by the gateway handler. Runs each panel function sequentially (they're cheap and already aggregated) and returns a fully populated struct.
- `types.go` — Go structs mirroring `MetricsResponse`, all tagged `json:"camelCase"`. One struct per TS interface.
- `hero.go`, `throughput.go`, `lifecycle.go`, `bug_attachment.go`, `formulas.go`, `cost_daily.go`, `phases.go`, `failures.go`, `models.go`, `tools.go` — one file per panel. Each exports a function that takes the `Reader` interface + window + scope and returns its sub-struct.
- `scope.go` — scope filter helpers: prefix → SQL `WHERE bead_id LIKE '<prefix>-%'` clause builder.
- `window.go` — maps `'24h'|'7d'|'30d'|'90d'|'custom'` → `(since, until time.Time)`. For `custom`, accepts explicit `since`/`until` query params; otherwise reject.
- `aspirational.go` — gap-filler. Takes the live lifecycle + bug_attachment sub-structs; if rows are sparse (<threshold), generates a deterministic synth layer seeded by scope+window hash and returns it in `Response.Aspirational`. Hero/DORA/throughput are **never** synthesized.
- `report_test.go` + per-panel `_test.go` — table-driven tests, a gold fixture compared against `spire-desktop/fixtures/metrics-sample.json` shape.

**Why a new package, not extending `pkg/olap`:** `pkg/olap` is the data layer (queries + ETL); response assembly is a higher-level concern that cares about HTTP contract, camelCase JSON, and the aspirational overlay. Keeping them separate preserves the ZFC boundary: olap doesn't know about frontends. Uses `pkg/metrics` as home because `pkg/metrics` already exists for agent-run recording, which is adjacent enough; alternative `pkg/olap/report` also fine if reviewers prefer — no strong preference, will pick during implementation based on import cycles.

### Modified — `pkg/gateway/gateway.go`
- Line 114: keep the route, swap the handler target from `handleMetricsStub` to `handleMetrics`.
- Replace `handleMetricsStub` (lines 838–847) with `handleMetrics(w, r)`:
  - Method gate: GET only.
  - Parse `scope`/`window`/`aspirational`/`since`/`until` query params.
  - Open `*olap.DB` from `config.ActiveTowerConfig().OLAPPath()` — same pattern as `cmd/spire/metrics.go:131-141`. Return 503 + JSON error if unavailable.
  - Call `report.Build(ctx, db, scope, window, aspirational)` with a **request-scoped context** (use `r.Context()` so client disconnect cancels queries).
  - `writeJSON(w, 200, response)` on success; `writeJSON(w, 400, err)` on invalid params.
- No new field on `Server` — opening the DB per-request is fine at 60s poll cadence (DuckDB open is cheap; this also sidesteps lifecycle concerns with the shared-writer `*olap.DB` and lets us later swap to a cached DB holder if profiling shows open-cost matters).

### Modified — `pkg/olap/duckdb/views.go`
Add three view sections inside `viewRefreshStatements()`:

- **`mv_bug_attachment_rate_weekly`** — weekly parent-type × (parents, parentsWithBugs). Computing this needs the parent-child graph from Dolt, which `bead_lifecycle_olap` doesn't carry today. Plan: extend the ETL (`pkg/olap/duckdb/etl.go` + `pkg/olap/schema.go`) to populate a new `bead_parents_olap` table (columns: bead_id, bead_type, parent_id, parent_type, filed_at, closed_at, is_bug). Then the view is `GROUP BY week_start, parent_type` counting distinct parent_ids. "recentParents" is a lateral query joined in at response-assembly time (it needs bead titles, which live in Dolt — fetch via `store.GetBead` for the top-N parent_ids per week).
- **`mv_lifecycle_quantiles_by_type_window`** — per (bead_type, window_range) → all 6 stages' `{p50, p75, p95, p99}` + outlier bead IDs. We materialize the 6 stages inline via UNION ALL or a LATERAL UNNEST of stage names; quantiles via `quantile_cont(..., 0.50/0.75/0.95/0.99)`. Outliers = top-3 beads whose stage duration is above p95, with their bead_ids. All sourced from `bead_lifecycle_olap` (no new ETL).
- **`mv_scope_repo_map`** — trivially derivable. Rather than a materialized view, implement as a pure SQL helper in `report/scope.go` (`AND bead_id LIKE ? || '-%'`). This keeps ETL cheaper and is equivalent — the "view" abstraction adds nothing when the mapping is one string operation. **Deviation from bead description, worth noting in the PR.**

### Modified — `pkg/olap/schema.go`
- New table `bead_parents_olap`: `(bead_id TEXT, bead_type TEXT, parent_id TEXT, parent_type TEXT, filed_at TIMESTAMP, closed_at TIMESTAMP, is_bug BOOLEAN, PRIMARY KEY (bead_id))`. Populated by ETL from Dolt `beads` + `dependencies` tables.

### Modified — `pkg/olap/duckdb/etl.go` (and `pkg/olap/etl.go` if it mirrors)
- Add an upsert loop that reads each bead's parent (via `dependency_type='parent-child'`) and its type, writes to `bead_parents_olap`. Runs inside the existing ETL transaction.

### Modified — `pkg/olap/olap.go` (the `MetricsReader` interface) and `pkg/olap/duckdb/queries.go` / `pkg/olap/queries.go`
Add 3 new Reader methods: `QueryLifecycleQuantilesByType(since, until)`, `QueryBugAttachmentWeekly(since, until)`, `QueryHeroDelta(since)` (for WoW deltas). Also extend scope-filter support on **every** existing Reader method used by the report — each needs an optional `prefix string` param (empty = all). Options:
- **Preferred**: new variants like `QueryTrendsScoped(since, prefix)` that add `AND repo = ?` to the WHERE. Keeps existing callers untouched.
- Alternative: a `Filter{Since, Until, Prefix}` struct replacing `since time.Time` everywhere. Cleaner long term but much broader blast radius — defer unless reviewers want it now.

Go with new `Scoped` methods for now to keep this bead bounded.

### Modified — `pkg/gateway/gateway_test.go`
- Add handler tests using `httptest`: valid request returns 200 + expected JSON shape; invalid window returns 400; scope=repo filters correctly (use a fake reader). Follow the existing `summonRunner`-style injectable-seam pattern to swap the OLAP reader.

### New — CI contract test
- `pkg/metrics/report/schema_test.go` — reads `spire-desktop/src/views/metrics/types.ts` (or a committed JSON schema generated from it), decodes a sample response into it, fails on extra/missing fields. Acceptance #1 needs this; without it the contract will silently drift.
- Needs a mechanism to pull the TS type. Two ways: (a) check in a generated `metrics.schema.json` alongside the TS file and require `make schema` to regenerate; (b) dev-dep on `typescript-json-schema` invoked in CI. (a) is simpler, proposing that.

## 3. Key design decisions

1. **Seam boundary**: `report` depends on a narrow `Reader` interface (subset of `olap.MetricsReader` + the new scoped methods), not `*olap.DB` directly. Keeps tests trivial (fake reader), keeps assembly testable without cgo/DuckDB, and avoids circular imports.
2. **Per-request DB open** vs persistent: per-request for v1. DuckDB file-open is ~1ms for a reader connection; keeps `Server` struct unchanged. Revisit if p95 suffers.
3. **Quantile source of truth**: DuckDB's `quantile_cont` for all stage percentiles. Don't compute in Go — bytes over the wire matter for 6.7k beads.
4. **Aspirational overlay** lives outside the panel data, matching the TS `aspirational: {lifecycleByType?, bugAttachmentWeekly?}` layout. Ignore the bead's "synthetic: true per row" — that contradicts the TS. Only these two panels are synth-eligible per the TS.
5. **Scope filter**: prefix-based `bead_id LIKE 'spi-%'`. `repo` column on `agent_runs_olap` is derived from bead_id at ETL time via `repoFromBeadID` (pkg/olap/duckdb/etl.go:1-10) — reuse that same prefix. For `bead_lifecycle_olap` / `bead_parents_olap`, bead_id prefix is sufficient (no `repo` column).
6. **WoW deltas**: compute `(thisWeek - lastWeek) / lastWeek` per hero metric. Need 14 days minimum even on a 7d window — handle by always querying 2× the window for hero WoW inputs, then slicing.
7. **Outliers on lifecycle**: top-3 bead_ids per stage where `duration > p95`. Cheap in SQL with `QUALIFY ROW_NUMBER()` or window function. Titles are NOT needed for lifecycle outliers (TS: `{beadId, durationSeconds}` only).
8. **Titles for bug-attachment recentParents**: DO need titles. Fetch via `store.GetBead(parentID).Title` in a batch after the SQL returns bead IDs. Small N (<20 total), acceptable cost.

## 4. Edge cases & risks

- **Empty/new tower (no beads)**: every panel must return `[]` not `null`, every numeric must be 0/`null` per the TS nullability. Set up a "no data" test fixture.
- **Null percentiles**: `quantile_cont` returns NULL with <1 sample. Scan into `sql.NullFloat64`, convert to 0 in the response (TS has them as `number`, not nullable). Document this — it's a data semantics choice the UI needs to be aware of.
- **Custom window without since/until**: 400 Bad Request with a clear error.
- **Custom window range > retention (90d)**: return partial data, not error — add a `window.truncated: true` flag? Not in the TS. Just return whatever's in-range.
- **Scope=unknown-prefix**: return empty panels (not 404). The frontend might be stale.
- **ETL lag**: metrics are only as fresh as the last ETL sync. For now, surface `generatedAt` from `time.Now()`. Future improvement: use `etl_cursor.last_run`.
- **OLAP file missing / open error**: return 503 with a helpful message pointing at `spire up`. Don't leak raw error to the browser.
- **Performance**: 6.7k beads is trivial for DuckDB — the risk is N+1 in `recentParents` titles (store.GetBead per bead). Batch-fetch or add a `store.GetBeads([]string)` if one doesn't exist.
- **Concurrency**: OLAP reads are safe under the existing mutex; we're never writing. No special care needed.
- **cgo/nocgo build tags**: `pkg/olap/duckdb/*` is `//go:build cgo`. New scoped queries need to live in that build tag; non-cgo fallback in `pkg/olap/factory_duckdb_nocgo.go` should return "not implemented" for the new methods so release builds (cgo off) return 503 rather than crash.
- **Fixture drift**: once the real endpoint lands, the frontend falls back to the fixture if the request errors. Keep the fixture in sync with any new contract fields so dev mode stays functional.

## 5. Rough ordering of changes

**Wave 1 — foundation, parallel-friendly** (4 subtasks, dispatchable in parallel)
1. Schema + ETL for `bead_parents_olap` (pkg/olap/schema.go + etl.go) — foundational, unblocks bug_attachment.
2. Three new materialized views in `pkg/olap/duckdb/views.go` (lifecycle_quantiles, bug_attachment_rate; drop mv_scope_repo_map per decision #5).
3. Scoped Reader methods on `olap.MetricsReader` + impls in `pkg/olap/queries.go` + `pkg/olap/duckdb/queries.go` (plus `nocgo` stubs).
4. `pkg/metrics/report/` skeleton: `types.go`, `window.go`, `scope.go`, `Build()` signature.

**Wave 2 — panel assemblers** (10 small subtasks, parallel-safe once Wave 1 ships)
One subtask per panel file under `pkg/metrics/report/`: hero, throughput, lifecycle, bug_attachment, formulas, cost_daily, phases, failures, models, tools. Each has a <50-line function + a table-driven test with a fake reader.

**Wave 3 — gateway + aspirational + schema**
1. `pkg/gateway/gateway.go` — replace stub, wire handler, add handler tests.
2. `pkg/metrics/report/aspirational.go` — the synth overlay.
3. CI schema test — committed JSON schema + round-trip decode test.

**Wave 4 — integration**
1. End-to-end integration test against a real DuckDB fixture file (seed with 6.7k-ish rows, assert <500ms).
2. Manual desktop smoke: run `spire up` + `npm run dev` in spire-desktop, confirm MetricsView renders live data with no fixture fallback (acceptance #6).

Wave 1 gates Wave 2. Wave 2 can run fully parallel — panel files are independent. Wave 3 can start the gateway wiring in parallel with Wave 2 since it only needs the `Build()` signature from Wave 1.

## 6. Open questions to raise at plan review

1. New package location: `pkg/metrics/report/` vs `pkg/olap/report/`? Leaning `pkg/metrics` (separation of concerns) but either works. I'll pick during implementation based on import cycles unless there's a strong preference.
2. Dropping `mv_scope_repo_map` as a materialized view — is that OK given the bead lists it explicitly? My read: it was a red-herring deliverable; the filter is a one-liner.
3. `Scoped` method variants vs a `Filter` struct refactor on `MetricsReader` — the refactor is cleaner but outside the bead's scope. Stick with `Scoped` variants?
4. Schema-contract CI approach: check in a generated `metrics.schema.json`, or generate on CI? Former is more tamper-evident; latter is zero-drift.