# Bead-Graph Context: Tool-Call Audit and Commit Metadata

This document explains the two seam-level mechanisms Spire uses to keep
the bead graph self-describing for sage / cleric / archmage audits:

1. **Per-tool-call audit visibility** — every `Bash`, `Read`, `Edit`,
   `Grep`, … call an agent makes during an attempt is queryable
   post-facto with its **arguments and results**, not just a counter.
2. **Commit metadata uniformity** — every closed bead has a reliable
   mapping to the commits that closed it, regardless of which close
   path was used (`spire wizard seal`, `bd update --status closed`,
   board approve, recovery flows, programmatic graph close).

Both pieces close gaps that previously left audit / context-coverage
verification incomplete or unreliable.

## 1. Tool-call audit

### Storage shape

Per-invocation tool-call data is recorded in two OLAP tables (DuckDB
locally; ClickHouse in cluster towers). Both are populated by Claude
Code's OTLP emit, ingested through `pkg/otel/receiver.go` (spans) and
`pkg/otel/log_handler.go` (logs):

| Table         | What                                                   | Source              |
|---------------|--------------------------------------------------------|---------------------|
| `tool_events` | One row per invocation: `tool_name`, `duration_ms`, `success`, `timestamp`. Thin event row — no rich payload. | OTLP logs + spans |
| `tool_spans`  | Hierarchical spans with a JSON `attributes` blob carrying the rich payload (Bash command text, Read file path, Grep pattern, span events with input/output). | OTLP spans |

The choice is **`tool_spans.attributes` is the canonical store for the
rich payload**; `tool_events` stays thin (one row per call, one record
of timing/outcome). When both signals describe the same logical call,
the read path prefers the span row.

### Ingestion enrichment

Two extraction paths are exercised on every batch:

- `pkg/otel/log_handler.go:parseToolEvent` extracts tool args / results
  from log-record attributes (`command`, `tool_input`, `input_value`,
  `file_path`, `pattern`, `tool_output`, `output_value`,
  `error_message`, `gen_ai.tool.input`, `gen_ai.tool.output`).
- `pkg/otel/receiver.go:buildToolSpan` reads `span.GetEvents()` (in
  addition to the existing `span.GetAttributes()` read) and merges
  per-event attributes into the span's `attributes` JSON under a
  top-level `events` array. Identity context (`session.id`,
  `user.email`, `organization.id`, `tool_name`) is preserved at the
  top level.

### Read paths

| Surface                                              | Where                                  |
|------------------------------------------------------|----------------------------------------|
| `GET /api/v1/attempts/{attempt_id}/tool_calls`       | `pkg/gateway/attempt.go`               |
| `spire attempt show <attempt-id>`                    | `cmd/spire/attempt.go`                 |
| Board "TOOL USAGE" → drill-down hint                 | `pkg/board/metrics_mode.go` (`renderToolUsageContent`) |
| Programmatic                                          | `pkg/observability.ListAttemptToolCalls` → `pkg/olap.QueryToolCallsBySession` |

Pagination defaults to **200 rows per page**; the cap is **1000**. The
endpoint joins span and log signals: span rows are emitted first
(`source: "span"`), then log-only rows that have no matching span are
appended (`source: "log"`).

### Audit query patterns

#### Empirical verification — args present in `tool_spans.attributes`

```sql
SELECT span_name,
       json_extract(attributes, '$.command')   AS cmd,
       json_extract(attributes, '$.file_path') AS fp
FROM tool_spans
WHERE kind='tool'
ORDER BY end_time DESC
LIMIT 10;
```

Expect non-NULL `cmd` for `Bash` rows and non-NULL `fp` for `Read` /
`Edit` rows after running an attempt that exercises those tools.

#### Per-attempt drill-down — what did the agent actually do?

```bash
spire attempt show <attempt-id> --page 1 --page-size 200
spire attempt show <attempt-id> --json | jq '.[] | {tool_name, args: .attributes}'
```

Sage uses this surface during review. The sage system prompt explicitly
references it (see `pkg/wizard/wizard_review.go:ReviewRunOpus`).

## 2. Commit metadata uniformity

### Three-layer pattern

```
Write-time     — wizard.recordCommitMetadata appends each commit's SHA
                 to bead.metadata.commits[] when the wizard lands a
                 commit. (pkg/wizard/wizard.go:recordCommitMetadata)

Close-time     — when a bead transitions out of any other status into
                 `closed`, the post-close sweep (best-effort, async)
                 runs `git log --grep=<bead-id> --all --oneline` from
                 the bead's prefix-mapped repo path and appends every
                 novel SHA to bead.metadata.commits[].
                 (pkg/store/close_sweep.go:firePostCloseSweep)

Read-time      — pkg/store.LookupBeadCommits returns every recorded
                 SHA paired with its reachability flag (via
                 `git cat-file -e <sha>^{commit}`) and source
                 ("metadata" or "grep"). Squash-merge fallback: when a
                 wizard-recorded SHA is unreachable post-squash, the
                 grep results provide the squash commit on main.
                 (pkg/store/lookup_commits.go:LookupBeadCommits)
```

### Single sweep hook — every close path benefits

Every direct close path on Spire funnels through `pkg/store`'s
`updateBeadDirect` or `CloseBead`, both of which now invoke
`firePostCloseSweepIfTransitioned` after the status transition lands.
This means CLI close, board approve, programmatic close in the
executor, recovery flows, and apprentice fix landings all feed the
same metadata.commits[] without each caller having to remember.

The sweep is gated on **prior status** — if the bead was already
`closed` (replays, admin re-closes, status-only updates that don't
actually transition state), the sweep does not fire. This avoids
redundant `git log` invocations.

### Failure modes (silent by design)

| Condition                       | Behaviour                                        |
|---------------------------------|--------------------------------------------------|
| Prefix unbound to a local repo  | Skip silently. Tower coordinates beads but doesn't host the repo. |
| `git log` fails (no repo, etc.) | Log at debug, skip. Close transaction never blocked. |
| Append fails for one SHA        | Log, continue with the rest.                     |
| Re-close (prior status closed)  | Skip the sweep entirely (nothing to do).         |

The sweep is **best-effort**: any failure leaves the close successful
and the metadata in its prior state.

### Squash-merge fallback example

```
Pre-squash:
  bead.metadata.commits[] = ["abc1234"]
  LookupBeadCommits → [{SHA: abc1234, Reachable: true, Source: metadata, Subject: "feat(spi-x): foo"}]

Post-squash to main (apprentice branch deleted, abc1234 garbage-collected):
  LookupBeadCommits → [
    {SHA: abc1234, Reachable: false, Source: metadata},
    {SHA: 9999999, Reachable: true,  Source: grep, Subject: "feat(spi-x): foo (#42)"},
  ]
```

Renderers can show "(squash-merged elsewhere)" for unreachable
metadata rows and pivot to the grep-found SHA for change-set
inspection.

### Audit query patterns

#### Inspect a single bead's commit history

```go
refs, err := store.LookupBeadCommits("spi-xyz", repoPath)
for _, r := range refs {
    fmt.Printf("%s [%s, reachable=%v] %s\n", r.SHA, r.Source, r.Reachable, r.Subject)
}
```

#### Sweep is asynchronous

The sweep is post-commit and runs in a goroutine. Tests that close a
bead and immediately verify metadata.commits[] should call
`pkg/store.WaitCloseSweeps()` first to drain in-flight goroutines.
Production code should never call this — the sweep is fire-and-forget.

## Out of scope

- Backfilling commits onto already-closed beads (one-time migration).
  Could be a follow-up; today only newly-closed beads pick up the
  sweep.
- Cross-repo commit lookup (a bead in repo A whose work landed in
  repo B). The sweep resolves the bead's prefix → registered repo
  path; multi-repo beads would need a different strategy.
- Redaction / privacy filtering of logged tool args. Bash command
  text including any embedded secrets will land in OLAP; a redaction
  pass is a follow-up if needed for sensitive deployments. There is
  no per-tower "disable args capture" flag in v1.

## Related

- `pkg/wizard/wizard.go:recordCommitMetadata` — write-time recording.
- `pkg/store/close_sweep.go` — close-time sweep hook implementation.
- `pkg/store/lookup_commits.go` — read-time helper with reachability.
- `pkg/otel/log_handler.go`, `pkg/otel/receiver.go` — OTLP enrichment
  paths.
- `pkg/observability/attempt_calls.go` — gateway/CLI read path for
  per-call data.
- `cmd/spire/attempt.go` — `spire attempt show` rendering.
