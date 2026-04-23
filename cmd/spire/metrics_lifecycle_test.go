package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/olap"
)

func TestFmtSeconds(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{0, "0s"},
		{45, "45s"},
		{119, "119s"},
		{120, "2.0m"},
		{7199, "120.0m"},
		{7200, "2.00h"},
		{30600, "8.50h"},
		{-1, "—"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := fmtSeconds(tt.in); got != tt.want {
				t.Errorf("fmtSeconds(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestFmtSecondsPtr(t *testing.T) {
	if got := fmtSecondsPtr(nil); got != "—" {
		t.Errorf("nil pointer should render as em-dash, got %q", got)
	}
	v := 61.5
	if got := fmtSecondsPtr(&v); got != "62s" {
		t.Errorf("fmtSecondsPtr(61.5) = %q, want 62s", got)
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly10c", 10, "exactly10c"},
		{"this-is-way-too-long", 10, "this-is-w…"},
		{"abc", 1, "a"},
		{"abc", 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := truncate(tt.in, tt.n); got != tt.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
			}
		})
	}
}

// captureRenderOutput redirects stdout, runs fn, returns what was written.
// Distinct from the file-wide captureStdout (which takes a fn that returns an
// error) because the renderer helpers are void — wrapping them in a shim
// helper that returns nil to match the signature is noisier than just using
// a local capture.
func captureRenderOutput(t *testing.T, fn func()) string {
	t.Helper()
	got, _ := captureStdout(t, func() error {
		fn()
		return nil
	})
	return got
}

func TestRenderBeadLifecycleBlock_Nil(t *testing.T) {
	out := captureRenderOutput(t, func() {
		renderBeadLifecycleBlock(nil)
	})
	if out != "" {
		t.Errorf("nil lifecycle should render nothing, got %q", out)
	}
}

func TestRenderBeadLifecycleBlock_AllMissing(t *testing.T) {
	// Pre-feature bead: lifecycle row exists but only filed_at is set.
	lc := &olap.BeadLifecycleIntervals{
		BeadLifecycle: olap.BeadLifecycle{BeadID: "spi-old"},
	}
	out := captureRenderOutput(t, func() {
		renderBeadLifecycleBlock(lc)
	})
	if !strings.Contains(out, "Filed:             —") {
		t.Errorf("missing filed_at should render em-dash, got:\n%s", out)
	}
	if !strings.Contains(out, "Queue (ready→start): —") {
		t.Errorf("missing queue should render em-dash, got:\n%s", out)
	}
}

func TestRenderBeadLifecycleBlock_QueueIsolated(t *testing.T) {
	// Motivating case: 21m13s queue delay, 2h52m49s execution. Ensure the
	// renderer surfaces both numbers independently so an operator can tell
	// queue delay apart from API/execution slowness.
	queue := 21.0*60 + 13
	exec := 2.0*3600 + 52*60 + 49
	lc := &olap.BeadLifecycleIntervals{
		BeadLifecycle:          olap.BeadLifecycle{BeadID: "spi-h32xj"},
		QueueSeconds:           &queue,
		StartedToClosedSeconds: &exec,
	}
	out := captureRenderOutput(t, func() {
		renderBeadLifecycleBlock(lc)
	})
	if !strings.Contains(out, "Started → closed:") {
		t.Errorf("missing execution line, got:\n%s", out)
	}
	if !strings.Contains(out, "Queue (ready→start):") {
		t.Errorf("missing queue line, got:\n%s", out)
	}
	// Verify the two numbers render distinctly — if we accidentally collapsed
	// queue into started_to_closed the same value would appear twice.
	if strings.Count(out, "21.2m") < 1 && strings.Count(out, "1273s") < 1 {
		t.Errorf("queue duration not rendered, got:\n%s", out)
	}
	// 2h52m49s = 10369s → rendered as "2.88h" (fmtSeconds uses 2-decimal hours above 2h).
	if !strings.Contains(out, "2.88h") {
		t.Errorf("execution duration not rendered, got:\n%s", out)
	}
}

func TestRenderReviewFixBlock_Empty(t *testing.T) {
	rf := &olap.ReviewFixCounts{BeadID: "spi-test"}
	out := captureRenderOutput(t, func() {
		renderReviewFixBlock(rf)
	})
	if out != "" {
		t.Errorf("all-zero review/fix counts should render nothing, got:\n%s", out)
	}
}

func TestRenderReviewFixBlock_PopulatedShowsArbiter(t *testing.T) {
	rf := &olap.ReviewFixCounts{
		BeadID:          "spi-test",
		ReviewCount:     3,
		FixCount:        2,
		ArbiterCount:    1,
		MaxReviewRounds: 3,
	}
	out := captureRenderOutput(t, func() {
		renderReviewFixBlock(rf)
	})
	for _, want := range []string{"Review dynamics", "Sage reviews:      3", "Fix loops:         2", "Arbiter rounds:    1", "Max review round:  3"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in output:\n%s", want, out)
		}
	}
}

func TestRenderChildLifecycleBlock_Empty(t *testing.T) {
	out := captureRenderOutput(t, func() {
		renderChildLifecycleBlock(nil)
	})
	if out != "" {
		t.Errorf("no children should render nothing, got:\n%s", out)
	}
}

func TestRenderChildLifecycleBlock_OrderedByFiledAt(t *testing.T) {
	f2c1 := 1200.0
	f2c2 := 60.0
	kids := []olap.BeadLifecycleIntervals{
		{BeadLifecycle: olap.BeadLifecycle{BeadID: "spi-epic.1", BeadType: "step"}, FiledToClosedSeconds: &f2c1},
		{BeadLifecycle: olap.BeadLifecycle{BeadID: "spi-epic.2", BeadType: "attempt"}, FiledToClosedSeconds: &f2c2},
	}
	out := captureRenderOutput(t, func() {
		renderChildLifecycleBlock(kids)
	})
	if !strings.Contains(out, "spi-epic.1") || !strings.Contains(out, "spi-epic.2") {
		t.Errorf("missing child bead IDs in output:\n%s", out)
	}
	if !strings.Contains(out, "step") || !strings.Contains(out, "attempt") {
		t.Errorf("missing bead types in output:\n%s", out)
	}
	// Column order: BEAD first.
	idx1 := strings.Index(out, "spi-epic.1")
	idx2 := strings.Index(out, "spi-epic.2")
	if idx1 == -1 || idx2 == -1 || idx1 > idx2 {
		t.Errorf("children should render in input order, got:\n%s", out)
	}
}

func TestRenderLifecycleByType_Empty(t *testing.T) {
	out := captureRenderOutput(t, func() {
		renderLifecycleByType(nil)
	})
	if !strings.Contains(out, "no closed beads") {
		t.Errorf("empty rollup should explain absence, got:\n%s", out)
	}
}

func TestRenderLifecycleByType_Populated(t *testing.T) {
	taskF2C50, taskF2C95 := 3600.0, 7200.0
	taskQ50, taskQ95 := 30.0, 600.0
	bugF2C50, bugF2C95 := 60.0, 120.0
	bugQ50, bugQ95 := 10.0, 15.0
	rows := []olap.LifecycleByType{
		{BeadType: "task", Count: 42, FiledToClosedP50: &taskF2C50, FiledToClosedP95: &taskF2C95, QueueP50: &taskQ50, QueueP95: &taskQ95},
		{BeadType: "bug", Count: 5, FiledToClosedP50: &bugF2C50, FiledToClosedP95: &bugF2C95, QueueP50: &bugQ50, QueueP95: &bugQ95},
	}
	out := captureRenderOutput(t, func() {
		renderLifecycleByType(rows)
	})
	if !strings.Contains(out, "Bead lifecycle by type") {
		t.Errorf("missing header, got:\n%s", out)
	}
	if !strings.Contains(out, "task") || !strings.Contains(out, "bug") {
		t.Errorf("missing bead types, got:\n%s", out)
	}
	if !strings.Contains(out, "42") {
		t.Errorf("missing count for task, got:\n%s", out)
	}
}

// TestRenderLifecycleByType_NilRendersEmDash verifies that nil percentiles
// (the "no data" case — e.g. pre-feature beads whose ready_at/started_at are
// NULL) render as em-dash rather than misreporting as 0s.
func TestRenderLifecycleByType_NilRendersEmDash(t *testing.T) {
	f2c50, f2c95 := 3600.0, 7200.0
	rows := []olap.LifecycleByType{
		{
			BeadType: "task", Count: 10,
			FiledToClosedP50: &f2c50, FiledToClosedP95: &f2c95,
			// R→C, S→C, Q intentionally nil — historical beads with no
			// ready/started stamps.
		},
	}
	out := captureRenderOutput(t, func() {
		renderLifecycleByType(rows)
	})
	if !strings.Contains(out, "—") {
		t.Errorf("nil percentiles should render em-dash, got:\n%s", out)
	}
}

// TestCmdMetrics_LifecycleByTypeFlag_ParsedCorrectly verifies the new flag is
// recognized and doesn't trip the unknown-flag guard.
func TestCmdMetrics_LifecycleByTypeFlag_ParsedCorrectly(t *testing.T) {
	err := cmdMetrics([]string{"--lifecycle-by-type", "--nonexistent"})
	if err == nil {
		t.Fatal("expected error for unknown flag")
	}
	if !strings.Contains(err.Error(), "unknown flag: --nonexistent") {
		t.Errorf("--lifecycle-by-type should be accepted, got: %v", err)
	}
}

// TestCmdMetrics_LifecycleByType_RequiresOLAP verifies the flag refuses to
// run without the DuckDB OLAP database — there's no Dolt fallback for
// quantile_cont aggregates.
func TestCmdMetrics_LifecycleByType_RequiresOLAP(t *testing.T) {
	skipIfNoDuckDB(t)
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)
	t.Setenv("SPIRE_TOWER", "")

	err := cmdMetrics([]string{"--lifecycle-by-type"})
	if err == nil {
		t.Fatal("expected error when DuckDB unavailable")
	}
	// Fails on OLAP requirement before reaching the lifecycle path.
	if !strings.Contains(err.Error(), "OLAP database unavailable") &&
		!strings.Contains(err.Error(), "requires the DuckDB") {
		t.Errorf("expected OLAP/DuckDB error, got: %v", err)
	}
}

// TestRenderBeadMetrics_EmptyJSONIncludesLifecycleFields verifies that the
// JSON output for a bead includes the new lifecycle/review/child keys even
// when empty — downstream tools that consume the JSON must see the schema
// consistently.
func TestRenderBeadMetrics_EmptyJSON_StructureIntact(t *testing.T) {
	// Use a minimal in-memory fixture by writing a DuckDB file and opening it.
	// If DuckDB isn't available (nocgo build), skip.
	skipIfNoDuckDB(t)
	tmp := t.TempDir()
	configDir := filepath.Join(tmp, "config")
	dataDir := filepath.Join(tmp, "data")
	towersDir := filepath.Join(configDir, "towers")
	if err := os.MkdirAll(towersDir, 0755); err != nil {
		t.Fatal(err)
	}
	olapDir := filepath.Join(dataDir, "spire", "test-tower")
	if err := os.MkdirAll(olapDir, 0755); err != nil {
		t.Fatal(err)
	}
	towerJSON := `{"name":"test-tower","project_id":"test","hub_prefix":"tst","database":"test"}`
	if err := os.WriteFile(filepath.Join(towersDir, "test-tower.json"), []byte(towerJSON), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPIRE_CONFIG_DIR", configDir)
	t.Setenv("SPIRE_TOWER", "test-tower")
	t.Setenv("XDG_DATA_HOME", dataDir)

	out := captureRenderOutput(t, func() {
		_ = cmdMetrics([]string{"--bead", "spi-doesnotexist", "--json"})
	})
	if !json.Valid([]byte(out)) {
		t.Fatalf("expected valid JSON, got: %s", out)
	}
	// Structure check: the beadSummary fields remain present even when the
	// lifecycle sidecar has no rows.
	for _, key := range []string{"\"bead_id\"", "\"total_runs\"", "\"success_rate\""} {
		if !strings.Contains(out, key) {
			t.Errorf("missing key %s in JSON:\n%s", key, out)
		}
	}
}
