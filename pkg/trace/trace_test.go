package trace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/observability"
	"github.com/awell-health/spire/pkg/store"
)

func TestMapPhaseToStep(t *testing.T) {
	stepNames := map[string]bool{
		"plan": true, "implement": true, "review": true, "merge": true,
	}
	tests := []struct {
		phase, bucket string
		want          string
	}{
		{"implement", "", "implement"},
		{"plan", "", "plan"},
		{"review", "", "review"},
		{"build-fix", "", "implement"},      // phase fallback -> implement bucket
		{"review-fix", "", "review"},        // phase fallback -> review bucket
		{"validate-design", "", "plan"},     // design bucket -> plan
		{"unknown-phase", "", ""},           // no match
		{"unknown-phase", "implement", "implement"}, // explicit bucket
		{"unknown-phase", "design", "plan"},
	}
	for _, tc := range tests {
		got := mapPhaseToStep(tc.phase, tc.bucket, stepNames)
		if got != tc.want {
			t.Errorf("mapPhaseToStep(%q,%q) = %q, want %q", tc.phase, tc.bucket, got, tc.want)
		}
	}
}

func TestApplyStepMetrics(t *testing.T) {
	pipeline := []PipelineStep{
		{Step: "plan", Status: "closed"},
		{Step: "implement", Status: "in_progress"},
		{Step: "review", Status: "open"},
	}
	stepNames := map[string]bool{"plan": true, "implement": true, "review": true}
	runs := []observability.StepRunRow{
		{Phase: "plan", Duration: 75, CostUSD: 0.10,
			ToolCallsJSON: `{"Read":3,"Write":1,"Edit":2}`, CompletedAt: "done"},
		// Two implement rows — should count as 1 retry.
		{Phase: "implement", Duration: 120, CostUSD: 0.50,
			ToolCallsJSON: `{"Read":10,"Write":5}`, CompletedAt: "done"},
		{Phase: "build-fix", Duration: 60, CostUSD: 0.20,
			ToolCallsJSON: `{"Read":4,"Write":2}`, CompletedAt: "done"},
	}
	applyStepMetrics(pipeline, runs, stepNames)

	// plan: 75s -> 75000ms, $0.10, reads=3, writes=1, retries=0
	if p := pipeline[0]; p.DurationMs != 75000 || p.CostUSD != 0.10 ||
		p.Reads != 3 || p.Writes != 1 || p.Retries != 0 {
		t.Errorf("plan step = %+v", p)
	}
	// implement aggregated across both rows: 180s -> 180000ms, $0.70,
	// reads=14, writes=7, retries=1 (2 runs).
	if p := pipeline[1]; p.DurationMs != 180000 || p.CostUSD != 0.70 ||
		p.Reads != 14 || p.Writes != 7 || p.Retries != 1 {
		t.Errorf("implement step = %+v", p)
	}
	// review: no runs -> all zeros.
	if p := pipeline[2]; p.DurationMs != 0 || p.CostUSD != 0 ||
		p.Reads != 0 || p.Writes != 0 || p.Retries != 0 {
		t.Errorf("review step = %+v", p)
	}
}

func TestParseLogTS(t *testing.T) {
	tests := []struct {
		line       string
		wantTS     string
		wantRest   string
	}{
		{
			"2026-04-24T16:49:12Z hello world",
			"2026-04-24T16:49:12Z",
			"hello world",
		},
		{
			"2026-04-24 16:49:12 hello world",
			"2026-04-24 16:49:12",
			"hello world",
		},
		{
			"no timestamp here",
			"",
			"no timestamp here",
		},
		{
			"",
			"",
			"",
		},
	}
	for _, tc := range tests {
		ts, rest := parseLogTS(tc.line)
		if ts != tc.wantTS || rest != tc.wantRest {
			t.Errorf("parseLogTS(%q) = (%q, %q), want (%q, %q)",
				tc.line, ts, rest, tc.wantTS, tc.wantRest)
		}
	}
}

func TestReadLastLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log.txt")

	// File with 10 lines.
	var sb strings.Builder
	for i := 1; i <= 10; i++ {
		sb.WriteString("line ")
		sb.WriteString(string(rune('0' + i%10)))
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	tests := []struct {
		name string
		n    int
		want int
	}{
		{"n=3 returns last 3", 3, 3},
		{"n=5 returns last 5", 5, 5},
		{"n=10 returns all 10", 10, 10},
		{"n=100 returns all 10 (file has fewer)", 100, 10},
		{"n=0 returns nil", 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lines, err := ReadLastLines(path, tc.n)
			if err != nil {
				t.Fatalf("ReadLastLines: %v", err)
			}
			if len(lines) != tc.want {
				t.Fatalf("len = %d, want %d", len(lines), tc.want)
			}
		})
	}

	// Missing file returns os.ErrNotExist.
	_, err := ReadLastLines(filepath.Join(dir, "nonexistent.log"), 10)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestExtractAgentName(t *testing.T) {
	tests := []struct {
		name string
		bead store.Bead
		want string
	}{
		{
			"from agent: label",
			store.Bead{Labels: []string{"attempt", "agent:wizard-spi-abc"}},
			"wizard-spi-abc",
		},
		{
			"from attempt: title",
			store.Bead{Title: "attempt: wizard-spi-xyz"},
			"wizard-spi-xyz",
		},
		{
			"label wins over title",
			store.Bead{
				Title:  "attempt: from-title",
				Labels: []string{"agent:from-label"},
			},
			"from-label",
		},
		{
			"neither → empty",
			store.Bead{Title: "something else"},
			"",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := extractAgentName(tc.bead)
			if got != tc.want {
				t.Errorf("extractAgentName = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStepPos(t *testing.T) {
	order := map[string]int{"plan": 0, "implement": 1, "review": 2}
	if got := stepPos("plan", order); got != 0 {
		t.Errorf("plan = %d, want 0", got)
	}
	if got := stepPos("review", order); got != 2 {
		t.Errorf("review = %d, want 2", got)
	}
	if got := stepPos("unknown", order); got != 999 {
		t.Errorf("unknown = %d, want 999", got)
	}
	if got := stepPos("anything", nil); got != 0 {
		t.Errorf("nil order = %d, want 0", got)
	}
}

func TestNotFoundError(t *testing.T) {
	e := &NotFoundError{ID: "spi-xyz"}
	if !strings.Contains(e.Error(), "spi-xyz") {
		t.Errorf("Error() = %q, missing id", e.Error())
	}
	if e.Unwrap() != nil {
		t.Errorf("Unwrap() = %v, want nil", e.Unwrap())
	}
}
