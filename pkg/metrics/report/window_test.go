package report

import (
	"strings"
	"testing"
	"time"
)

func TestParseWindow(t *testing.T) {
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		rangeStr   string
		sinceStr   string
		untilStr   string
		wantRange  string
		wantError  string
		wantOffset time.Duration // expected (now - since)
	}{
		{name: "default empty → 7d", rangeStr: "", wantRange: "7d", wantOffset: 7 * 24 * time.Hour},
		{name: "24h", rangeStr: "24h", wantRange: "24h", wantOffset: 24 * time.Hour},
		{name: "7d", rangeStr: "7d", wantRange: "7d", wantOffset: 7 * 24 * time.Hour},
		{name: "30d", rangeStr: "30d", wantRange: "30d", wantOffset: 30 * 24 * time.Hour},
		{name: "90d", rangeStr: "90d", wantRange: "90d", wantOffset: 90 * 24 * time.Hour},
		{name: "custom without since/until errors", rangeStr: "custom", wantError: "requires since and until"},
		{name: "custom with bad since errors", rangeStr: "custom", sinceStr: "not-a-date", untilStr: now.Format(time.RFC3339), wantError: "invalid since"},
		{name: "custom with bad until errors", rangeStr: "custom", sinceStr: now.Add(-time.Hour).Format(time.RFC3339), untilStr: "not-a-date", wantError: "invalid until"},
		{name: "custom with since>=until errors", rangeStr: "custom", sinceStr: now.Format(time.RFC3339), untilStr: now.Add(-time.Hour).Format(time.RFC3339), wantError: "since must be before until"},
		{name: "bogus range errors", rangeStr: "bogus", wantError: "invalid window range"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, err := ParseWindow(tc.rangeStr, tc.sinceStr, tc.untilStr, now)
			if tc.wantError != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil", tc.wantError)
				}
				if !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error = %q, want contains %q", err.Error(), tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if w.Range != tc.wantRange {
				t.Errorf("range = %q, want %q", w.Range, tc.wantRange)
			}
			if got := now.Sub(w.Since); got != tc.wantOffset {
				t.Errorf("since offset = %v, want %v", got, tc.wantOffset)
			}
		})
	}
}

func TestParseWindow_Custom(t *testing.T) {
	since := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)

	w, err := ParseWindow("custom", since.Format(time.RFC3339), until.Format(time.RFC3339), now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if w.Range != "custom" {
		t.Errorf("range = %q, want custom", w.Range)
	}
	if !w.Since.Equal(since) {
		t.Errorf("since = %v, want %v", w.Since, since)
	}
	if !w.Until.Equal(until) {
		t.Errorf("until = %v, want %v", w.Until, until)
	}
}
