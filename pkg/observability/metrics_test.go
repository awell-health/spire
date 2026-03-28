package observability

import (
	"encoding/json"
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
