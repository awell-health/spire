package graph

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// withFakes installs the package-level seams (db provider, clock) for the
// duration of a test and restores them in cleanup. Tests can't use
// t.Parallel because these are package-globals, same shape as gateway's
// withFakeCollect.
func withFakes(t *testing.T, db *sql.DB, now time.Time) {
	t.Helper()
	origDB := dbProvider
	origNow := nowFunc
	dbProvider = func() (*sql.DB, bool) { return db, db != nil }
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() {
		dbProvider = origDB
		nowFunc = origNow
	})
}

func descendantCols() []string {
	return []string{"id", "title", "status", "priority", "issue_type", "parent", "labels", "updated_at", "depth"}
}

func aggregateCols() []string {
	return []string{"bead_id", "duration_seconds", "cost_usd", "run_count"}
}

func activeCols() []string {
	return []string{"bead_id", "agent_name", "model", "branch", "started_at"}
}

// TestCollect_NotFound exercises the existence-via-CTE path: the descendant
// CTE INNER JOINs issues, so a non-existent root id yields zero rows and
// Collect maps that to *NotFoundError without a separate GetBead probe.
func TestCollect_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	withFakes(t, db, time.Now())

	mock.ExpectQuery(`WITH RECURSIVE walk`).
		WithArgs("spi-missing").
		WillReturnRows(sqlmock.NewRows(descendantCols()))

	_, err = Collect("spi-missing", Options{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var nf *NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("error type = %T, want *NotFoundError", err)
	}
	if nf.ID != "spi-missing" {
		t.Errorf("NotFoundError.ID = %q, want spi-missing", nf.ID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCollect_MaxDepthRejected(t *testing.T) {
	withFakes(t, nil, time.Now())

	_, err := Collect("spi-root", Options{MaxDepth: MaxMaxDepth + 1})
	if !errors.Is(err, ErrMaxDepthExceeded) {
		t.Fatalf("err = %v, want ErrMaxDepthExceeded", err)
	}
}

func TestCollect_DBNotInitialised(t *testing.T) {
	withFakes(t, nil, time.Now())

	_, err := Collect("spi-root", Options{})
	if err == nil || !strings.Contains(err.Error(), "active db not initialised") {
		t.Fatalf("err = %v, want active-db error", err)
	}
}

// TestCollect_LeafBead exercises the most common case: a single bead with no
// descendants. The CTE returns one row (the root via the seed) and there are
// no agent_runs rows.
func TestCollect_LeafBead(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	withFakes(t, db, time.Now())

	rows := sqlmock.NewRows(descendantCols()).
		AddRow("spi-leaf", "Leaf bead", "open", 1, "task", "", nil, "2026-04-25T15:00:00Z", 0)
	mock.ExpectQuery(`WITH RECURSIVE walk`).WithArgs("spi-leaf").WillReturnRows(rows)
	mock.ExpectQuery(`FROM agent_runs.+GROUP BY bead_id`).
		WithArgs("spi-leaf").
		WillReturnRows(sqlmock.NewRows(aggregateCols()))
	mock.ExpectQuery(`MAX\(started_at\)`).
		WithArgs("spi-leaf").
		WillReturnRows(sqlmock.NewRows(activeCols()))

	resp, err := Collect("spi-leaf", Options{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if resp.RootID != "spi-leaf" {
		t.Errorf("RootID = %q, want spi-leaf", resp.RootID)
	}
	if len(resp.Nodes) != 1 {
		t.Fatalf("Nodes count = %d, want 1", len(resp.Nodes))
	}
	leaf, ok := resp.Nodes["spi-leaf"]
	if !ok {
		t.Fatal("root not in Nodes")
	}
	if leaf.Status != "open" || leaf.Type != "task" {
		t.Errorf("leaf node = %+v", leaf)
	}
	if len(leaf.Labels) != 0 {
		t.Errorf("leaf.Labels = %v, want empty", leaf.Labels)
	}
	if leaf.Metrics != nil {
		t.Errorf("leaf.Metrics = %+v, want nil for never-ran bead", leaf.Metrics)
	}
	if len(resp.Edges) != 0 {
		t.Errorf("Edges = %v, want empty", resp.Edges)
	}
	if resp.Truncated {
		t.Error("Truncated = true, want false")
	}
	if resp.Totals.ByStatus["open"] != 1 {
		t.Errorf("ByStatus[open] = %d, want 1", resp.Totals.ByStatus["open"])
	}
	if resp.Totals.ByType["task"] != 1 {
		t.Errorf("ByType[task] = %d, want 1", resp.Totals.ByType["task"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestCollect_FullSubgraph exercises the populated-response path: root with
// children, agent_runs aggregates folded into Metrics, and an in-progress
// run surfaced as ActiveAgent with elapsed_ms computed against nowFunc.
func TestCollect_FullSubgraph(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Date(2026, 4, 25, 16, 0, 0, 0, time.UTC)
	started := time.Date(2026, 4, 25, 15, 58, 0, 0, time.UTC) // 2 minutes ago
	withFakes(t, db, now)

	rows := sqlmock.NewRows(descendantCols()).
		AddRow("spi-epic", "Root epic", "in_progress", 0, "epic", "", "epic,trace", "2026-04-25T15:48:00Z", 0).
		AddRow("spi-task1", "Task one", "closed", 1, "task", "spi-epic", nil, "2026-04-25T15:50:00Z", 1).
		AddRow("spi-task2", "Task two", "in_progress", 1, "task", "spi-epic", "in_flight", "2026-04-25T15:55:00Z", 1).
		AddRow("spi-step1", "step:implement", "in_progress", 3, "step", "spi-task2", "step:implement,workflow-step", "2026-04-25T15:55:00Z", 2)
	mock.ExpectQuery(`WITH RECURSIVE walk`).WithArgs("spi-epic").WillReturnRows(rows)

	mock.ExpectQuery(`FROM agent_runs.+GROUP BY bead_id`).
		WithArgs("spi-epic", "spi-task1", "spi-task2", "spi-step1").
		WillReturnRows(sqlmock.NewRows(aggregateCols()).
			AddRow("spi-task1", 120, 0.30, 2).
			AddRow("spi-task2", 60, 0.10, 1))

	mock.ExpectQuery(`MAX\(started_at\)`).
		WithArgs("spi-epic", "spi-task1", "spi-task2", "spi-step1").
		WillReturnRows(sqlmock.NewRows(activeCols()).
			AddRow("spi-task2", "wizard-spi-task2", "claude-opus-4-7", "feat/spi-task2", started))

	resp, err := Collect("spi-epic", Options{MaxDepth: 5})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(resp.Nodes) != 4 {
		t.Fatalf("Nodes = %d, want 4", len(resp.Nodes))
	}
	root := resp.Nodes["spi-epic"]
	if got := root.Labels; len(got) != 2 || got[0] != "epic" || got[1] != "trace" {
		t.Errorf("root labels = %v, want [epic trace]", got)
	}
	if root.Depth != 0 {
		t.Errorf("root depth = %d, want 0", root.Depth)
	}
	step := resp.Nodes["spi-step1"]
	if step.Depth != 2 || step.Parent != "spi-task2" {
		t.Errorf("step depth/parent = %d/%q", step.Depth, step.Parent)
	}
	if got := step.Labels; len(got) != 2 || got[0] != "step:implement" || got[1] != "workflow-step" {
		t.Errorf("step labels = %v", got)
	}
	// Three non-root nodes → three edges.
	if len(resp.Edges) != 3 {
		t.Fatalf("Edges = %d, want 3", len(resp.Edges))
	}
	for _, e := range resp.Edges {
		if e.Type != "parent" {
			t.Errorf("edge type = %q, want parent", e.Type)
		}
	}
	// Metrics fold-in.
	t1 := resp.Nodes["spi-task1"]
	if t1.Metrics == nil || t1.Metrics.DurationMs != 120000 || t1.Metrics.CostUSD != 0.30 || t1.Metrics.RunCount != 2 {
		t.Errorf("task1 metrics = %+v", t1.Metrics)
	}
	// Bead with no aggregate row keeps Metrics nil.
	if resp.Nodes["spi-epic"].Metrics != nil {
		t.Errorf("epic should have nil metrics, got %+v", resp.Nodes["spi-epic"].Metrics)
	}
	// Totals roll up across only the beads with metrics.
	if resp.Totals.DurationMs != 180000 {
		t.Errorf("Totals.DurationMs = %d, want 180000", resp.Totals.DurationMs)
	}
	if resp.Totals.CostUSD != 0.40 {
		t.Errorf("Totals.CostUSD = %v, want 0.40", resp.Totals.CostUSD)
	}
	if resp.Totals.RunCount != 3 {
		t.Errorf("Totals.RunCount = %d, want 3", resp.Totals.RunCount)
	}
	// ByStatus / ByType histograms.
	if resp.Totals.ByStatus["in_progress"] != 3 || resp.Totals.ByStatus["closed"] != 1 {
		t.Errorf("ByStatus = %v", resp.Totals.ByStatus)
	}
	if resp.Totals.ByType["epic"] != 1 || resp.Totals.ByType["task"] != 2 || resp.Totals.ByType["step"] != 1 {
		t.Errorf("ByType = %v", resp.Totals.ByType)
	}
	// Active agent surfaced for spi-task2 with elapsed = now - started.
	if len(resp.ActiveAgents) != 1 {
		t.Fatalf("ActiveAgents = %d, want 1", len(resp.ActiveAgents))
	}
	a := resp.ActiveAgents[0]
	if a.BeadID != "spi-task2" || a.Name != "wizard-spi-task2" || a.Model != "claude-opus-4-7" || a.Branch != "feat/spi-task2" {
		t.Errorf("active agent = %+v", a)
	}
	wantElapsed := int64(2 * 60 * 1000)
	if a.ElapsedMs != wantElapsed {
		t.Errorf("ElapsedMs = %d, want %d", a.ElapsedMs, wantElapsed)
	}
	// Agent should also be embedded on the active node (no client-side join
	// needed). Other in-progress nodes without an active run keep Agent nil.
	t2 := resp.Nodes["spi-task2"]
	if t2.Agent == nil {
		t.Fatal("spi-task2 Agent should be embedded, got nil")
	}
	if t2.Agent.BeadID != "spi-task2" || t2.Agent.Name != "wizard-spi-task2" ||
		t2.Agent.Model != "claude-opus-4-7" || t2.Agent.Branch != "feat/spi-task2" ||
		t2.Agent.ElapsedMs != wantElapsed {
		t.Errorf("embedded agent = %+v", t2.Agent)
	}
	if resp.Nodes["spi-step1"].Agent != nil {
		t.Errorf("spi-step1 Agent = %+v, want nil (no in-progress run)", resp.Nodes["spi-step1"].Agent)
	}
	if resp.Nodes["spi-epic"].Agent != nil {
		t.Errorf("spi-epic Agent = %+v, want nil (no in-progress run)", resp.Nodes["spi-epic"].Agent)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCollect_Truncation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	withFakes(t, db, time.Now())

	// Build RowLimit+1 rows so the truncation branch fires.
	rows := sqlmock.NewRows(descendantCols())
	rows.AddRow("spi-mega", "Root", "in_progress", 0, "epic", "", nil, "2026-04-25T15:00:00Z", 0)
	for i := 1; i < RowLimit+1; i++ {
		id := fmt.Sprintf("spi-c%04d", i)
		rows.AddRow(id, "child", "open", 2, "task", "spi-mega", nil, "2026-04-25T15:00:00Z", 1)
	}
	mock.ExpectQuery(`WITH RECURSIVE walk`).WithArgs("spi-mega").WillReturnRows(rows)

	// Aggregate query should be called with exactly RowLimit ids (the kept
	// set after truncation).
	mock.ExpectQuery(`FROM agent_runs.+GROUP BY bead_id`).
		WillReturnRows(sqlmock.NewRows(aggregateCols()))
	mock.ExpectQuery(`MAX\(started_at\)`).
		WillReturnRows(sqlmock.NewRows(activeCols()))

	resp, err := Collect("spi-mega", Options{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !resp.Truncated {
		t.Error("Truncated = false, want true")
	}
	if len(resp.Nodes) != RowLimit {
		t.Errorf("Nodes count = %d, want %d", len(resp.Nodes), RowLimit)
	}
	// Edges referencing dropped nodes should be filtered out.
	for _, e := range resp.Edges {
		if _, ok := resp.Nodes[e.To]; !ok {
			t.Errorf("edge %+v references dropped node", e)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCollect_DescendantQueryError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	withFakes(t, db, time.Now())

	mock.ExpectQuery(`WITH RECURSIVE walk`).
		WithArgs("spi-x").
		WillReturnError(errors.New("dolt down"))

	if _, err := Collect("spi-x", Options{}); err == nil ||
		!strings.Contains(err.Error(), "descendants query") {
		t.Fatalf("err = %v, want descendants query error", err)
	}
}

func TestCollect_DefaultsMaxDepth(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	withFakes(t, db, time.Now())

	// Match the literal DefaultMaxDepth interpolated into the SQL string.
	pattern := fmt.Sprintf(`w\.depth < %d`, DefaultMaxDepth)
	mock.ExpectQuery(pattern).
		WithArgs("spi-root").
		WillReturnRows(sqlmock.NewRows(descendantCols()).
			AddRow("spi-root", "r", "open", 1, "task", "", nil, "2026-04-25T15:00:00Z", 0))
	mock.ExpectQuery(`FROM agent_runs.+GROUP BY bead_id`).WillReturnRows(sqlmock.NewRows(aggregateCols()))
	mock.ExpectQuery(`MAX\(started_at\)`).WillReturnRows(sqlmock.NewRows(activeCols()))

	if _, err := Collect("spi-root", Options{MaxDepth: 0}); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestCollect_NodesContainRoot guards AC #1: leaf-bead Trace must not render
// empty. The recursion seed includes the root row regardless of whether it
// has children.
func TestCollect_NodesContainRoot(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	withFakes(t, db, time.Now())

	mock.ExpectQuery(`WITH RECURSIVE walk`).
		WithArgs("spi-only").
		WillReturnRows(sqlmock.NewRows(descendantCols()).
			AddRow("spi-only", "Only", "open", 1, "task", "", nil, "2026-04-25T15:00:00Z", 0))
	mock.ExpectQuery(`FROM agent_runs.+GROUP BY bead_id`).WillReturnRows(sqlmock.NewRows(aggregateCols()))
	mock.ExpectQuery(`MAX\(started_at\)`).WillReturnRows(sqlmock.NewRows(activeCols()))

	resp, err := Collect("spi-only", Options{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if _, ok := resp.Nodes[resp.RootID]; !ok {
		t.Fatalf("Nodes does not contain root %q; nodes=%v", resp.RootID, resp.Nodes)
	}
}

func TestSplitLabels(t *testing.T) {
	cases := []struct {
		name string
		in   sql.NullString
		want []string
	}{
		{"null returns empty", sql.NullString{}, []string{}},
		{"empty string returns empty", sql.NullString{Valid: true, String: ""}, []string{}},
		{"single label", sql.NullString{Valid: true, String: "epic"}, []string{"epic"}},
		{"multi label", sql.NullString{Valid: true, String: "epic,trace,ux"}, []string{"epic", "trace", "ux"}},
		{"trims whitespace", sql.NullString{Valid: true, String: " a , b , c "}, []string{"a", "b", "c"}},
		{"drops empty parts", sql.NullString{Valid: true, String: "a,,b"}, []string{"a", "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitLabels(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d (got=%v)", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}
