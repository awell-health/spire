package observability

import (
	"testing"
	"time"
)

func TestFormatDurationShort(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{30 * time.Second, "30s"},
		{59 * time.Second, "59s"},
		{1 * time.Minute, "1m"},
		{2*time.Minute + 30*time.Second, "2m30s"},
		{1 * time.Hour, "1h"},
		{1*time.Hour + 5*time.Minute, "1h5m"},
		{2*time.Hour + 30*time.Minute, "2h30m"},
	}
	for _, tt := range tests {
		got := FormatDurationShort(tt.d)
		if got != tt.want {
			t.Errorf("FormatDurationShort(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestFormatFileSize(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{0, "0B"},
		{512, "512B"},
		{1024, "1.0K"},
		{1536, "1.5K"},
		{1048576, "1.0M"},
		{1073741824, "1.0G"},
	}
	for _, tt := range tests {
		got := FormatFileSize(tt.bytes)
		if got != tt.want {
			t.Errorf("FormatFileSize(%d) = %q, want %q", tt.bytes, got, tt.want)
		}
	}
}

func TestFormatSyncAge_invalid(t *testing.T) {
	got := FormatSyncAge("not-a-timestamp")
	if got != "?" {
		t.Errorf("FormatSyncAge(invalid) = %q, want %q", got, "?")
	}
}
