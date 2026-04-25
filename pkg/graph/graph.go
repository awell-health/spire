// Package graph assembles a "you are here" view of a bead's descendant
// subgraph for the desktop Trace tab. It powers GET /api/v1/beads/{id}/graph,
// returning the full subtree rooted at the requested bead plus per-bead
// run aggregates and any in-progress agent attached to a node.
//
// Boundary:
//   - graph owns the descendant CTE + agent_runs aggregate composition and
//     the response shape on the wire.
//   - graph does NOT touch pkg/trace — the legacy /api/v1/beads/{id}/trace
//     endpoint and `spire trace <bead>` CLI keep their existing data path.
//   - graph relies on store.ActiveDB for the *sql.DB and uses the descendant
//     CTE itself for existence + 404 mapping; it does not open its own
//     connection and does not issue a separate GetBead probe.
package graph


import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/store"
)

// DefaultMaxDepth is the depth used when no explicit max_depth is supplied.
const DefaultMaxDepth = 10

// MaxMaxDepth caps the requested depth so a pathological client cannot blow
// past the 200ms budget. Server-side clamp is independent of the gateway's
// query-param parsing — Collect refuses anything larger.
const MaxMaxDepth = 10

// RowLimit caps the descendant CTE result set. The query fetches RowLimit+1
// rows so Collect can flag Truncated when the cap was hit. Generous for
// real epics; revisit only if real workloads start brushing against it.
const RowLimit = 5000

// GraphResponse is the JSON shape returned to the desktop's GraphView.
//
// Field semantics:
//   - RootID — the bead the request was scoped to.
//   - Nodes — every descendant (including the root) keyed by bead id. The
//     desktop maps status enums to pills client-side, same as /trace.
//   - Edges — parent-child edges only. Cross-edge types (discovered-from,
//     blocks, etc.) belong to the Lineage tab; mixing them here would conflate
//     "the route" with "the network".
//   - Totals — sum across every node in Nodes plus by-status / by-type
//     histograms the UI uses for the legend.
//   - ActiveAgents — the latest in-progress agent_runs row per bead, when
//     present. Attaches to the node via BeadID.
//   - Truncated — true when the descendant CTE hit RowLimit. The desktop
//     surfaces a banner so the user knows nodes are missing.
type GraphResponse struct {
	RootID       string          `json:"root_id"`
	Nodes        map[string]Node `json:"nodes"`
	Edges        []Edge          `json:"edges"`
	Totals       Totals          `json:"totals"`
	ActiveAgents []ActiveAgent   `json:"active_agents"`
	Truncated    bool            `json:"truncated"`
}

// Node is a single bead in the subgraph. Status, Type, and Priority are the
// raw bead fields; Labels is split from the CTE's GROUP_CONCAT. Metrics is
// nil when the bead has no agent_runs rows — keeping the field absent in
// JSON rather than emitting zero values clarifies "never ran" vs "ran with
// zero cost" on the wire. Agent is non-nil only for the bead's currently
// in-progress agent_runs row (latest started); the desktop renders it inline
// on the in-progress node so no client-side join against ActiveAgents is
// needed. The same payload is also surfaced in GraphResponse.ActiveAgents
// for callers that want a flat list.
type Node struct {
	ID        string       `json:"id"`
	Title     string       `json:"title"`
	Status    string       `json:"status"`
	Type      string       `json:"type"`
	Parent    string       `json:"parent"`
	Priority  int          `json:"priority"`
	Labels    []string     `json:"labels"`
	UpdatedAt string       `json:"updated_at"`
	Depth     int          `json:"depth"`
	Metrics   *Metrics     `json:"metrics,omitempty"`
	Agent     *ActiveAgent `json:"agent,omitempty"`
}

// Edge is a parent-child link from From → To. Type is hard-coded "parent" so
// the wire shape matches /lineage's edge shape and the desktop can render
// both with the same component.
type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Type string `json:"type"`
}

// Metrics aggregates a single bead's agent_runs rows.
type Metrics struct {
	DurationMs int64   `json:"duration_ms"`
	CostUSD    float64 `json:"cost_usd"`
	RunCount   int     `json:"run_count"`
}

// Totals is the response-wide rollup. ByStatus and ByType are histograms over
// every node in Nodes so the legend renders without a second pass on the client.
type Totals struct {
	DurationMs int64          `json:"duration_ms"`
	CostUSD    float64        `json:"cost_usd"`
	RunCount   int            `json:"run_count"`
	ByStatus   map[string]int `json:"by_status"`
	ByType     map[string]int `json:"by_type"`
}

// ActiveAgent describes the latest in-progress agent_runs row for a bead in
// the subgraph. ElapsedMs is computed from started_at at query time; the
// desktop displays it inline on the in-progress node.
type ActiveAgent struct {
	BeadID    string `json:"bead_id"`
	Name      string `json:"name"`
	ElapsedMs int64  `json:"elapsed_ms"`
	Model     string `json:"model,omitempty"`
	Branch    string `json:"branch,omitempty"`
}

// Options configures Collect.
type Options struct {
	// MaxDepth caps the recursive walk. Zero or negative selects DefaultMaxDepth;
	// values above MaxMaxDepth are rejected with an error so the budget cannot
	// be exceeded by a hostile client.
	MaxDepth int
}

// NotFoundError is returned by Collect when the root bead does not resolve.
// Mirrors pkg/trace.NotFoundError so the gateway can surface 404 with a
// single errors.As branch.
type NotFoundError struct {
	ID  string
	Err error
}

func (e *NotFoundError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("bead not found: %s: %v", e.ID, e.Err)
	}
	return "bead not found: " + e.ID
}

func (e *NotFoundError) Unwrap() error { return e.Err }

// ErrMaxDepthExceeded is returned by Collect when the caller's MaxDepth is
// above the server cap. The gateway maps this to 400.
var ErrMaxDepthExceeded = errors.New("graph: max_depth exceeds server limit")

// descendantSQL walks the parent-child subgraph rooted at the bound bead id.
//
// Shape choices:
//   - The root row is included via the recursion seed (parent='', depth=0).
//   - Non-root rows project their parent from the walk itself rather than a
//     per-row correlated subquery — single round trip, no N+1.
//   - Labels come back via LEFT JOIN + GROUP_CONCAT; empty groups return NULL
//     which Go scans into sql.NullString and Collect splits into []string{}.
//   - LIMIT RowLimit+1 lets Collect detect truncation without a second query.
//
// The depth cap is interpolated rather than bound because Dolt's recursive
// CTE planner requires a literal in the WHERE — bound parameters there
// silently degrade to an unbounded walk. The value is clamped server-side
// before reaching this string.
const descendantSQLTemplate = `
WITH RECURSIVE walk(id, parent, depth) AS (
  SELECT ?, '', 0
  UNION ALL
  SELECT d.issue_id, d.depends_on_id, w.depth + 1
    FROM dependencies d JOIN walk w ON d.depends_on_id = w.id
   WHERE d.type = 'parent-child' AND w.depth < %d
)
SELECT i.id, i.title, i.status, i.priority, i.issue_type,
       w.parent,
       COALESCE(GROUP_CONCAT(l.label ORDER BY l.label SEPARATOR ','), '') AS labels,
       DATE_FORMAT(i.updated_at, '%%Y-%%m-%%dT%%H:%%i:%%sZ') AS updated_at,
       w.depth
  FROM walk w
  JOIN issues i ON i.id = w.id
  LEFT JOIN labels l ON l.issue_id = i.id
 GROUP BY i.id, i.title, i.status, i.priority, i.issue_type,
          w.parent, i.updated_at, w.depth
 ORDER BY w.depth, i.id
 LIMIT %d`

// runAggregateSQL returns one row per bead-with-runs in the supplied id list.
// Filtered to completed rows so a long-running attempt doesn't inflate
// duration_ms before it finishes. The active-agent projection is a separate
// query (runActiveSQL) — combining them in one SELECT would either need a
// second pass for completed_at IS NULL filtering or a UNION discriminator
// that complicates client-side parsing.
const runAggregateSQLTemplate = `
SELECT bead_id,
       COALESCE(SUM(duration_seconds), 0) AS duration_seconds,
       COALESCE(SUM(cost_usd), 0)         AS cost_usd,
       COUNT(*)                           AS run_count
  FROM agent_runs
 WHERE bead_id IN (%s)
   AND completed_at IS NOT NULL
 GROUP BY bead_id`

// runActiveSQL returns the latest in-progress agent_runs row per bead in the
// id list. "Latest" is the row with the most recent started_at among rows
// where completed_at IS NULL. Multiple historical attempts on the same bead
// are common; this picks the active one without a Go-side dedupe pass.
const runActiveSQLTemplate = `
SELECT r.bead_id, r.agent_name, r.model, r.branch, r.started_at
  FROM agent_runs r
  JOIN (
        SELECT bead_id, MAX(started_at) AS latest
          FROM agent_runs
         WHERE bead_id IN (%s)
           AND completed_at IS NULL
         GROUP BY bead_id
       ) m ON m.bead_id = r.bead_id AND m.latest = r.started_at
 WHERE r.completed_at IS NULL`

// dbProvider yields the active *sql.DB. The package-level seam lets graph
// tests inject a sqlmock without standing up the real dolt store. Mirrors
// the trace package's collector indirection used by the gateway.
var dbProvider = func() (*sql.DB, bool) {
	return store.ActiveDB()
}

// nowFunc is the wall-clock used to compute ActiveAgent.ElapsedMs. Tests
// freeze it; production reads time.Now.
var nowFunc = time.Now

// Collect assembles the GraphResponse for beadID.
//
// Errors:
//   - *NotFoundError when the root bead does not resolve (gateway → 404).
//     The descendant CTE's INNER JOIN to issues drops rows for non-existent
//     beads, so a zero-row result is the existence signal — no separate
//     GetBead probe round-trip.
//   - ErrMaxDepthExceeded when opts.MaxDepth > MaxMaxDepth (gateway → 400).
//   - Wrapped SQL errors when the descendant or aggregate queries fail.
//
// For beads that exist but have no descendants, returns a GraphResponse with
// Nodes containing just the root and empty Edges/ActiveAgents — matching
// the "leaf bead" rendering contract.
func Collect(beadID string, opts Options) (*GraphResponse, error) {
	depth := opts.MaxDepth
	if depth <= 0 {
		depth = DefaultMaxDepth
	}
	if depth > MaxMaxDepth {
		return nil, ErrMaxDepthExceeded
	}

	db, ok := dbProvider()
	if !ok {
		return nil, errors.New("graph: active db not initialised")
	}

	resp := &GraphResponse{
		RootID:       beadID,
		Nodes:        map[string]Node{},
		Edges:        []Edge{},
		ActiveAgents: []ActiveAgent{},
		Totals: Totals{
			ByStatus: map[string]int{},
			ByType:   map[string]int{},
		},
	}

	rows, err := db.Query(fmt.Sprintf(descendantSQLTemplate, depth, RowLimit+1), beadID)
	if err != nil {
		return nil, fmt.Errorf("graph: descendants query: %w", err)
	}
	defer rows.Close()

	ids := make([]string, 0, 32)
	for rows.Next() {
		var (
			n         Node
			labelsCSV sql.NullString
		)
		if err := rows.Scan(&n.ID, &n.Title, &n.Status, &n.Priority, &n.Type,
			&n.Parent, &labelsCSV, &n.UpdatedAt, &n.Depth); err != nil {
			return nil, fmt.Errorf("graph: scan descendants: %w", err)
		}
		n.Labels = splitLabels(labelsCSV)
		// Defensive: a bead may appear twice in the walk if a stray duplicate
		// parent-child edge exists (the upstream tree CTE guards the same
		// way with LIMIT 1). First-write-wins keeps the Edges count honest.
		if _, dup := resp.Nodes[n.ID]; dup {
			continue
		}
		resp.Nodes[n.ID] = n
		ids = append(ids, n.ID)
		if n.ID != beadID && n.Parent != "" {
			resp.Edges = append(resp.Edges, Edge{From: n.Parent, To: n.ID, Type: "parent"})
		}
		resp.Totals.ByStatus[n.Status]++
		resp.Totals.ByType[n.Type]++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("graph: descendants iter: %w", err)
	}
	if len(ids) == 0 {
		// CTE returned no rows → the seed id has no matching issues row →
		// the bead does not exist. Mapped to 404 by the gateway.
		return nil, &NotFoundError{ID: beadID}
	}
	if len(ids) > RowLimit {
		// Drop the overflow row from every projection so the client sees a
		// stable count. The truncated flag tells it the count is partial.
		extra := ids[RowLimit:]
		ids = ids[:RowLimit]
		for _, id := range extra {
			n := resp.Nodes[id]
			delete(resp.Nodes, id)
			resp.Totals.ByStatus[n.Status]--
			resp.Totals.ByType[n.Type]--
		}
		// Edges may reference dropped nodes; filter in place.
		kept := resp.Edges[:0]
		for _, e := range resp.Edges {
			if _, ok := resp.Nodes[e.To]; !ok {
				continue
			}
			kept = append(kept, e)
		}
		resp.Edges = kept
		resp.Truncated = true
	}
	if err := loadRunMetrics(db, ids, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// loadRunMetrics issues the two agent_runs queries and folds results into
// resp. Split out so tests can exercise CTE-only paths without standing up
// agent_runs expectations on every case.
func loadRunMetrics(db *sql.DB, ids []string, resp *GraphResponse) error {
	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		args[i] = id
	}

	aggRows, err := db.Query(fmt.Sprintf(runAggregateSQLTemplate, placeholders), args...)
	if err != nil {
		return fmt.Errorf("graph: aggregate query: %w", err)
	}
	defer aggRows.Close()
	for aggRows.Next() {
		var (
			beadID   string
			duration sql.NullInt64
			cost     sql.NullFloat64
			count    int
		)
		if err := aggRows.Scan(&beadID, &duration, &cost, &count); err != nil {
			return fmt.Errorf("graph: scan aggregate: %w", err)
		}
		n, ok := resp.Nodes[beadID]
		if !ok {
			// Aggregate row for a bead the CTE didn't return — possible if the
			// bead was reparented mid-query. Skip rather than synthesizing
			// an orphan node.
			continue
		}
		m := &Metrics{
			DurationMs: duration.Int64 * 1000,
			CostUSD:    cost.Float64,
			RunCount:   count,
		}
		n.Metrics = m
		resp.Nodes[beadID] = n
		resp.Totals.DurationMs += m.DurationMs
		resp.Totals.CostUSD += m.CostUSD
		resp.Totals.RunCount += m.RunCount
	}
	if err := aggRows.Err(); err != nil {
		return fmt.Errorf("graph: aggregate iter: %w", err)
	}

	activeRows, err := db.Query(fmt.Sprintf(runActiveSQLTemplate, placeholders), args...)
	if err != nil {
		return fmt.Errorf("graph: active query: %w", err)
	}
	defer activeRows.Close()
	now := nowFunc()
	for activeRows.Next() {
		var (
			a         ActiveAgent
			agentName sql.NullString
			model     sql.NullString
			branch    sql.NullString
			startedAt sql.NullTime
		)
		if err := activeRows.Scan(&a.BeadID, &agentName, &model, &branch, &startedAt); err != nil {
			return fmt.Errorf("graph: scan active: %w", err)
		}
		a.Name = agentName.String
		a.Model = model.String
		a.Branch = branch.String
		if startedAt.Valid {
			a.ElapsedMs = now.Sub(startedAt.Time).Milliseconds()
			if a.ElapsedMs < 0 {
				a.ElapsedMs = 0
			}
		}
		resp.ActiveAgents = append(resp.ActiveAgents, a)
		// Mirror the same payload onto the node so the desktop can render
		// active info inline without a client-side join against the
		// top-level ActiveAgents array. Both representations stay in sync.
		if n, ok := resp.Nodes[a.BeadID]; ok {
			agent := a
			n.Agent = &agent
			resp.Nodes[a.BeadID] = n
		}
	}
	if err := activeRows.Err(); err != nil {
		return fmt.Errorf("graph: active iter: %w", err)
	}
	return nil
}

// splitLabels turns the GROUP_CONCAT'd label list into a stable []string.
// Empty CSV (no labels for the bead) returns []string{} so the JSON shape
// is always a list, never null.
func splitLabels(csv sql.NullString) []string {
	if !csv.Valid || csv.String == "" {
		return []string{}
	}
	parts := strings.Split(csv.String, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}
