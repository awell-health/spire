// Package observability provides log discovery, metrics aggregation, and
// lifecycle status rendering for the Spire CLI.
package observability

import (
	"fmt"
	"time"
)

// ANSI color constants for terminal output.
const (
	Bold   = "\033[1m"
	Dim    = "\033[2m"
	Red    = "\033[31m"
	Yellow = "\033[33m"
	Green  = "\033[32m"
	Cyan   = "\033[36m"
	Reset  = "\033[0m"
)

// FormatSyncAge returns a human-readable duration since the given RFC3339 timestamp.
func FormatSyncAge(timestamp string) string {
	t, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return "?"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// FormatDurationShort returns a compact duration string like "2m30s" or "1h5m".
func FormatDurationShort(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		m := int(d.Minutes())
		s := int(d.Seconds()) % 60
		if s == 0 {
			return fmt.Sprintf("%dm", m)
		}
		return fmt.Sprintf("%dm%ds", m, s)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}

// FormatFileSize returns a human-readable file size.
func FormatFileSize(bytes int64) string {
	switch {
	case bytes < 1024:
		return fmt.Sprintf("%dB", bytes)
	case bytes < 1024*1024:
		return fmt.Sprintf("%.1fK", float64(bytes)/1024)
	case bytes < 1024*1024*1024:
		return fmt.Sprintf("%.1fM", float64(bytes)/(1024*1024))
	default:
		return fmt.Sprintf("%.1fG", float64(bytes)/(1024*1024*1024))
	}
}
