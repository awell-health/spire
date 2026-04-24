package report

import (
	"fmt"
	"time"
)

// Window is the resolved time range for a metrics query.
// Range is the frontend-facing label ("24h"|"7d"|"30d"|"90d"|"custom");
// Since/Until are the absolute bounds the SQL layer needs.
type Window struct {
	Range string
	Since time.Time
	Until time.Time
}

// ParseWindow turns the raw query params into a resolved Window. The
// "custom" range requires explicit since/until RFC3339 strings; named
// ranges ignore since/until. now() is injected so tests can pin time.
func ParseWindow(rangeStr, sinceStr, untilStr string, now time.Time) (Window, error) {
	w := Window{Range: rangeStr, Until: now}
	switch rangeStr {
	case "", "7d":
		w.Range = "7d"
		w.Since = now.AddDate(0, 0, -7)
	case "24h":
		w.Since = now.Add(-24 * time.Hour)
	case "30d":
		w.Since = now.AddDate(0, 0, -30)
	case "90d":
		w.Since = now.AddDate(0, 0, -90)
	case "custom":
		if sinceStr == "" || untilStr == "" {
			return w, fmt.Errorf("window=custom requires since and until query params")
		}
		s, err := time.Parse(time.RFC3339, sinceStr)
		if err != nil {
			return w, fmt.Errorf("invalid since: %w", err)
		}
		u, err := time.Parse(time.RFC3339, untilStr)
		if err != nil {
			return w, fmt.Errorf("invalid until: %w", err)
		}
		if !s.Before(u) {
			return w, fmt.Errorf("since must be before until")
		}
		w.Since = s
		w.Until = u
	default:
		return w, fmt.Errorf("invalid window range %q (want 24h|7d|30d|90d|custom)", rangeStr)
	}
	return w, nil
}

// SinceISO returns the RFC3339 representation of Since, used for
// debugging / response echo.
func (w Window) SinceISO() string { return w.Since.UTC().Format(time.RFC3339) }

// UntilISO returns the RFC3339 representation of Until.
func (w Window) UntilISO() string { return w.Until.UTC().Format(time.RFC3339) }
