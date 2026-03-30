package observability

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestEstimateCost(t *testing.T) {
	// Sonnet default: $3/M in, $15/M out
	got := EstimateCost(1_000_000, 1_000_000, "")
	want := 18.0 // 3 + 15
	if got != want {
		t.Errorf("EstimateCost(1M, 1M, sonnet) = %f, want %f", got, want)
	}

	// Opus: $15/M in, $75/M out
	got = EstimateCost(1_000_000, 1_000_000, "claude-opus-4")
	want = 90.0 // 15 + 75
	if got != want {
		t.Errorf("EstimateCost(1M, 1M, opus) = %f, want %f", got, want)
	}

	// Zero tokens
	got = EstimateCost(0, 0, "")
	if got != 0 {
		t.Errorf("EstimateCost(0, 0) = %f, want 0", got)
	}
}

func TestPct(t *testing.T) {
	tests := []struct {
		n, total int
		want     string
	}{
		{0, 0, "0%"},
		{5, 10, "50%"},
		{10, 10, "100%"},
		{1, 3, "33%"},
	}
	for _, tt := range tests {
		got := Pct(tt.n, tt.total)
		if got != tt.want {
			t.Errorf("Pct(%d, %d) = %q, want %q", tt.n, tt.total, got, tt.want)
		}
	}
}

func TestFirstOr(t *testing.T) {
	// Empty
	got := FirstOr(nil)
	if len(got) != 0 {
		t.Errorf("FirstOr(nil) should return empty map, got %v", got)
	}

	// Non-empty
	rows := []MetricsRow{{"a": 1}, {"b": 2}}
	got = FirstOr(rows)
	if got["a"] != 1 {
		t.Errorf("FirstOr should return first row")
	}
}

func TestToInt(t *testing.T) {
	tests := []struct {
		v    any
		want int
	}{
		{nil, 0},
		{float64(42), 42},
		{42, 42},
		{"123", 123},
		{json.Number("99"), 99},
		{true, 0}, // unsupported type
	}
	for _, tt := range tests {
		got := ToInt(tt.v)
		if got != tt.want {
			t.Errorf("ToInt(%v) = %d, want %d", tt.v, got, tt.want)
		}
	}
}

func TestToFloat(t *testing.T) {
	tests := []struct {
		v    any
		want float64
	}{
		{nil, 0},
		{float64(3.14), 3.14},
		{42, 42.0},
		{"2.5", 2.5},
		{json.Number("1.5"), 1.5},
	}
	for _, tt := range tests {
		got := ToFloat(tt.v)
		if got != tt.want {
			t.Errorf("ToFloat(%v) = %f, want %f", tt.v, got, tt.want)
		}
	}
}

func TestToString(t *testing.T) {
	tests := []struct {
		v    any
		want string
	}{
		{nil, ""},
		{"hello", "hello"},
		{float64(42), "42"},
		{float64(3.14), "3.14"},
	}
	for _, tt := range tests {
		got := ToString(tt.v)
		if got != tt.want {
			t.Errorf("ToString(%v) = %q, want %q", tt.v, got, tt.want)
		}
	}
}

func TestSqlEsc(t *testing.T) {
	got := SqlEsc("it's a test")
	want := "it''s a test"
	if got != want {
		t.Errorf("SqlEsc = %q, want %q", got, want)
	}
}

func TestFmtCost(t *testing.T) {
	tests := []struct {
		cost float64
		want string
	}{
		{0.0, "$0.00"},
		{0.05, "$0.05"},
		{0.99, "$0.99"},
		{1.0, "$1"},
		{1.50, "$2"},
		{42.0, "$42"},
		{100.7, "$101"},
	}
	for _, tt := range tests {
		got := fmtCost(tt.cost)
		if got != tt.want {
			t.Errorf("fmtCost(%v) = %q, want %q", tt.cost, got, tt.want)
		}
	}
}

func TestRenderPhaseMetrics_Empty(t *testing.T) {
	// Empty rows should print the "no data" message.
	f, err := os.CreateTemp("", "metrics-test-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	defer f.Close()

	if err := RenderPhaseMetrics(nil, false, f); err != nil {
		t.Fatalf("RenderPhaseMetrics(nil) = %v", err)
	}

	f.Seek(0, 0)
	buf := make([]byte, 4096)
	n, _ := f.Read(buf)
	out := string(buf[:n])

	if !strings.Contains(out, "no per-phase data yet") {
		t.Errorf("expected empty message, got: %q", out)
	}
}

func TestRenderPhaseMetrics_Text(t *testing.T) {
	rows := []MetricsRow{
		{
			"phase":           "implement",
			"total":           float64(10),
			"succeeded":       float64(8),
			"avg_duration":    float64(120),
			"total_tokens_in": float64(0),
			"total_tokens_out": float64(0),
			"total_cost":      float64(5.50),
		},
		{
			"phase":           "review",
			"total":           float64(4),
			"succeeded":       float64(4),
			"avg_duration":    float64(60),
			"total_tokens_in": float64(0),
			"total_tokens_out": float64(0),
			"total_cost":      float64(0.75),
		},
	}

	f, err := os.CreateTemp("", "metrics-test-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	defer f.Close()

	if err := RenderPhaseMetrics(rows, false, f); err != nil {
		t.Fatalf("RenderPhaseMetrics = %v", err)
	}

	f.Seek(0, 0)
	buf := make([]byte, 8192)
	n, _ := f.Read(buf)
	out := string(buf[:n])

	if !strings.Contains(out, "implement") {
		t.Errorf("expected 'implement' in output, got: %q", out)
	}
	if !strings.Contains(out, "review") {
		t.Errorf("expected 'review' in output, got: %q", out)
	}
	if !strings.Contains(out, "80%") {
		t.Errorf("expected '80%%' success rate for implement, got: %q", out)
	}
	if !strings.Contains(out, "100%") {
		t.Errorf("expected '100%%' success rate for review, got: %q", out)
	}
	// total_cost > 0 should be preferred over estimated cost
	if !strings.Contains(out, "$6") {
		t.Errorf("expected cost '$6' for implement (from recorded cost), got: %q", out)
	}
	if !strings.Contains(out, "$0.75") {
		t.Errorf("expected cost '$0.75' for review (from recorded cost), got: %q", out)
	}
}

func TestRenderPhaseMetrics_JSON(t *testing.T) {
	rows := []MetricsRow{
		{"phase": "plan", "total": float64(2)},
	}

	f, err := os.CreateTemp("", "metrics-test-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	defer f.Close()

	if err := RenderPhaseMetrics(rows, true, f); err != nil {
		t.Fatalf("RenderPhaseMetrics JSON = %v", err)
	}

	f.Seek(0, 0)
	var decoded []MetricsRow
	if err := json.NewDecoder(f).Decode(&decoded); err != nil {
		t.Fatalf("JSON decode: %v", err)
	}
	if len(decoded) != 1 {
		t.Fatalf("expected 1 row, got %d", len(decoded))
	}
	if ToString(decoded[0]["phase"]) != "plan" {
		t.Errorf("expected phase=plan, got %v", decoded[0]["phase"])
	}
}

func TestPercentile(t *testing.T) {
	tests := []struct {
		name   string
		vals   []float64
		p      float64
		want   float64
		approx bool // allow small floating-point tolerance
	}{
		{"empty", nil, 0.5, 0, false},
		{"single", []float64{5}, 0.5, 5, false},
		{"single p90", []float64{5}, 0.9, 5, false},
		{"two values p50", []float64{1, 3}, 0.5, 2, false},
		{"three values p50", []float64{1, 2, 3}, 0.5, 2, false},
		{"four values p90", []float64{1, 2, 3, 100}, 0.9, 70.9, true},
		{"ten values p50", []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0.5, 5.5, false},
		{"ten values p90", []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0.9, 9.1, true},
		{"p0", []float64{1, 2, 3}, 0.0, 1, false},
		{"p100", []float64{1, 2, 3}, 1.0, 3, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := percentile(tt.vals, tt.p)
			if tt.approx {
				diff := got - tt.want
				if diff < 0 {
					diff = -diff
				}
				if diff > 0.2 {
					t.Errorf("percentile(%v, %.2f) = %.2f, want ~%.2f (diff %.2f)", tt.vals, tt.p, got, tt.want, diff)
				}
			} else {
				if got != tt.want {
					t.Errorf("percentile(%v, %.2f) = %f, want %f", tt.vals, tt.p, got, tt.want)
				}
			}
		})
	}
}

func TestAvg(t *testing.T) {
	tests := []struct {
		vals []float64
		want float64
	}{
		{nil, 0},
		{[]float64{10}, 10},
		{[]float64{1, 2, 3}, 2},
		{[]float64{0, 100}, 50},
	}
	for _, tt := range tests {
		got := avg(tt.vals)
		if got != tt.want {
			t.Errorf("avg(%v) = %f, want %f", tt.vals, got, tt.want)
		}
	}
}

func TestRenderPhaseMetrics_CostSource(t *testing.T) {
	// When total_cost is 0, should use estimated cost from tokens.
	rows := []MetricsRow{
		{
			"phase":            "implement",
			"total":            float64(1),
			"succeeded":        float64(1),
			"avg_duration":     float64(60),
			"total_tokens_in":  float64(1_000_000),
			"total_tokens_out": float64(100_000),
			"total_cost":       float64(0), // no recorded cost
		},
	}

	f, err := os.CreateTemp("", "metrics-test-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	defer f.Close()

	if err := RenderPhaseMetrics(rows, false, f); err != nil {
		t.Fatalf("RenderPhaseMetrics = %v", err)
	}

	f.Seek(0, 0)
	buf := make([]byte, 8192)
	n, _ := f.Read(buf)
	out := string(buf[:n])

	// With 1M in tokens and 100K out tokens at sonnet rates ($3/M in, $15/M out):
	// cost = 3.0 + 1.5 = $4.50 → formatted as "$4" (>= $1, $%.0f rounds half-to-even)
	if !strings.Contains(out, "$4") {
		t.Errorf("expected estimated cost '$4' when total_cost=0, got: %q", out)
	}
}
