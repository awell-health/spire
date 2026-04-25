package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/config"
)

// fakeAuthObsReader is an in-memory authObservabilityReader for tests.
// Split is returned verbatim by CostSplitByAuthProfile; Recent maps
// slot → ordered slice of runs (test controls ordering).
type fakeAuthObsReader struct {
	Split  map[string]authCostAggregate
	Recent map[string][]authRunDisplay
	Err    error // if non-nil, both methods return it
}

func (f fakeAuthObsReader) CostSplitByAuthProfile() (map[string]authCostAggregate, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Split, nil
}

func (f fakeAuthObsReader) RecentRunsByAuthProfile(slot string, limit int) ([]authRunDisplay, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	runs := f.Recent[slot]
	if limit > 0 && limit < len(runs) {
		runs = runs[:limit]
	}
	return runs, nil
}

// withAuthObsReader swaps the package-level authObsReader for the
// duration of the test and restores it on cleanup. Tests call this
// instead of writing the variable directly so an unrelated panic
// doesn't leak a fake reader into subsequent tests.
func withAuthObsReader(t *testing.T, r authObservabilityReader) {
	t.Helper()
	orig := authObsReader
	authObsReader = r
	t.Cleanup(func() { authObsReader = orig })
}

// withCostStdout redirects costStdoutWriter for the duration of a test
// and returns the capture buffer.
func withCostStdout(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	orig := costStdoutWriter
	costStdoutWriter = buf
	t.Cleanup(func() { costStdoutWriter = orig })
	return buf
}

func TestCmdCost_SplitPerSlot(t *testing.T) {
	out := withCostStdout(t)
	withAuthObsReader(t, fakeAuthObsReader{
		Split: map[string]authCostAggregate{
			config.AuthSlotSubscription: {
				Slot:        config.AuthSlotSubscription,
				Runs:        7,
				TotalTokens: 120_000,
				CostUSD:     0, // subscription reports $0; real value is "metered"
			},
			config.AuthSlotAPIKey: {
				Slot:        config.AuthSlotAPIKey,
				Runs:        3,
				TotalTokens: 45_000,
				CostUSD:     1.2345,
			},
		},
	})
	if err := cmdCost(nil); err != nil {
		t.Fatalf("cmdCost: %v", err)
	}
	s := out.String()

	// Default top-line summary must be present (runs + tokens + total cost).
	if !strings.Contains(s, "Total runs: 10") {
		t.Errorf("top-line total missing/incorrect (expected 7+3=10 runs):\n%s", s)
	}
	if !strings.Contains(s, "Total cost: $1.23") {
		t.Errorf("top-line total cost missing/incorrect (expected $1.23):\n%s", s)
	}

	// Per-slot split rows.
	if !strings.Contains(s, "subscription:") || !strings.Contains(s, "metered") {
		t.Errorf("subscription row missing or not marked 'metered':\n%s", s)
	}
	if !strings.Contains(s, "api-key:") || !strings.Contains(s, "actual") {
		t.Errorf("api-key row missing or not marked 'actual':\n%s", s)
	}
	// subscription row must show tokens but no real dollar figure.
	if !strings.Contains(s, "120.0k") {
		t.Errorf("subscription tokens not rendered (want 120.0k):\n%s", s)
	}
	if !strings.Contains(s, "$0 metered") {
		t.Errorf("subscription dollar placeholder missing:\n%s", s)
	}
	// api-key row must carry the real spend.
	if !strings.Contains(s, "$1.23 actual") {
		t.Errorf("api-key actual-spend figure missing (want $1.23 actual):\n%s", s)
	}
}

func TestCmdCost_SwapAnnotation(t *testing.T) {
	out := withCostStdout(t)
	withAuthObsReader(t, fakeAuthObsReader{
		Split: map[string]authCostAggregate{
			// Two subscription runs, one of which 429-swapped to api-key.
			// Attribution: tokens + cost count toward subscription slot;
			// api-key slot keeps its own rows. SwapCount is on the
			// STARTING slot's bucket.
			config.AuthSlotSubscription: {
				Slot:        config.AuthSlotSubscription,
				Runs:        2,
				TotalTokens: 80_000,
				CostUSD:     0.5, // the swapped run's post-swap cost attributed here
				SwapCount:   1,
			},
			config.AuthSlotAPIKey: {
				Slot:        config.AuthSlotAPIKey,
				Runs:        1,
				TotalTokens: 10_000,
				CostUSD:     0.1,
			},
		},
	})
	if err := cmdCost(nil); err != nil {
		t.Fatalf("cmdCost: %v", err)
	}
	s := out.String()

	// Swap annotation must call out the count and attribution direction.
	if !strings.Contains(s, "promoted subscription → api-key") {
		t.Errorf("swap annotation missing direction marker:\n%s", s)
	}
	if !strings.Contains(s, "1 run promoted") {
		t.Errorf("swap annotation count incorrect (want 1 run):\n%s", s)
	}
	if !strings.Contains(s, "attributed to subscription") {
		t.Errorf("swap annotation missing attribution convention:\n%s", s)
	}

	// The swap run's tokens still belong to subscription in the row.
	if !strings.Contains(s, "subscription:") {
		t.Errorf("subscription row missing:\n%s", s)
	}
	// The api-key row is still present for the non-swapped run.
	if !strings.Contains(s, "api-key:") {
		t.Errorf("api-key row missing:\n%s", s)
	}
}

func TestCmdCost_EmptyData(t *testing.T) {
	out := withCostStdout(t)
	withAuthObsReader(t, fakeAuthObsReader{
		Split: map[string]authCostAggregate{},
	})
	if err := cmdCost(nil); err != nil {
		t.Fatalf("cmdCost: %v", err)
	}
	s := out.String()

	// Default shape still renders — zeros everywhere, no swap annotation.
	if !strings.Contains(s, "Total runs: 0") {
		t.Errorf("empty data: expected 'Total runs: 0', got:\n%s", s)
	}
	if !strings.Contains(s, "Total cost: $0.00") {
		t.Errorf("empty data: expected 'Total cost: $0.00', got:\n%s", s)
	}
	if strings.Contains(s, "promoted") {
		t.Errorf("empty data must not render swap annotation:\n%s", s)
	}
	// Both slot rows still render as 0-runs — the split table is
	// structural, not conditional on data.
	if !strings.Contains(s, "subscription:") || !strings.Contains(s, "api-key:") {
		t.Errorf("empty data must still show both slot rows as zero:\n%s", s)
	}
}

func TestCmdCost_UnrecordedRowsSurfaced(t *testing.T) {
	out := withCostStdout(t)
	withAuthObsReader(t, fakeAuthObsReader{
		Split: map[string]authCostAggregate{
			// Historical rows with NULL auth_profile — must still appear
			// in the total and be called out in the split so operators
			// don't think tokens vanished.
			"": {
				Slot:        "",
				Runs:        4,
				TotalTokens: 22_000,
				CostUSD:     0.05,
			},
			config.AuthSlotAPIKey: {
				Slot:        config.AuthSlotAPIKey,
				Runs:        1,
				TotalTokens: 1_000,
				CostUSD:     0.01,
			},
		},
	})
	if err := cmdCost(nil); err != nil {
		t.Fatalf("cmdCost: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "Total runs: 5") {
		t.Errorf("unrecorded rows must contribute to total (want 5):\n%s", s)
	}
	if !strings.Contains(s, "(unrecorded)") {
		t.Errorf("unrecorded rows must be called out explicitly:\n%s", s)
	}
}

func TestCmdCost_RejectsArgs(t *testing.T) {
	withCostStdout(t)
	withAuthObsReader(t, fakeAuthObsReader{Split: map[string]authCostAggregate{}})
	if err := cmdCost([]string{"extra"}); err == nil {
		t.Error("cmdCost with extra args = nil error, want usage error")
	}
}

func TestCmdCost_PropagatesReaderError(t *testing.T) {
	withCostStdout(t)
	withAuthObsReader(t, fakeAuthObsReader{Err: fmt.Errorf("boom")})
	err := cmdCost(nil)
	if err == nil {
		t.Fatal("cmdCost with reader error = nil, want wrapped error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error must wrap the underlying reader error, got: %v", err)
	}
}

// TestHumanTokens pins the k/M boundary behavior the split view depends
// on — a regression here would right-align wrong in a column.
func TestHumanTokens(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1.0k"},
		{12_345, "12.3k"},
		{999_999, "1000.0k"}, // still renders as k under 1M threshold
		{1_000_000, "1.0M"},
		{12_500_000, "12.5M"},
	}
	for _, c := range cases {
		if got := humanTokens(c.in); got != c.want {
			t.Errorf("humanTokens(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatStartedAt(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "—"},
		{"2026-04-24 17:34:43", "2026-04-24 17:34"},
		{"2026-04-24T17:34:43Z", "2026-04-24 17:34"},
		{"not-a-timestamp", "not-a-timestamp"}, // passthrough
	}
	for _, c := range cases {
		if got := formatStartedAt(c.in); got != c.want {
			t.Errorf("formatStartedAt(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
