package logexport

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/logartifact"
)

// fixedClock returns the same time on every call so the JSON output
// tests can pin a deterministic timestamp string.
func fixedClock() func() time.Time {
	t := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

// stableIdentity is a fully-populated Identity used across the sink
// tests so the assertion blocks don't repeat the field set verbatim.
func stableIdentity() logartifact.Identity {
	return logartifact.Identity{
		Tower:     "tower-a",
		BeadID:    "spi-bead",
		AttemptID: "spi-attempt",
		RunID:     "run-001",
		AgentName: "wizard-spi-bead",
		Role:      logartifact.RoleWizard,
		Phase:     "implement",
		Provider:  "claude",
		Stream:    logartifact.StreamTranscript,
	}
}

// TestStdoutSink_EmitWritesOneJSONLineWithFullLabelSet pins the canonical
// JSON shape produced for one tailed line. Cloud Logging filters target
// these field names directly — a rename or a missing field would silently
// break dashboards.
func TestStdoutSink_EmitWritesOneJSONLineWithFullLabelSet(t *testing.T) {
	var buf bytes.Buffer
	sink, err := NewStdoutSink(&buf, &atomicStats{})
	if err != nil {
		t.Fatalf("NewStdoutSink: %v", err)
	}
	sink.SetClock(fixedClock())

	id := stableIdentity()
	sink.Emit(id, "/var/spire/logs/foo/transcript.jsonl", 0, 1024, []byte(`{"event":"hello"}` + "\n"))

	// Output must be a single JSON object terminated by a newline.
	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("output not newline-terminated: %q", out)
	}
	if strings.Count(out, "\n") != 1 {
		t.Errorf("expected exactly one newline; got %d in %q", strings.Count(out, "\n"), out)
	}

	var rec StdoutRecord
	if err := json.Unmarshal([]byte(out), &rec); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if rec.Tower != id.Tower {
		t.Errorf("Tower = %q, want %q", rec.Tower, id.Tower)
	}
	if rec.BeadID != id.BeadID {
		t.Errorf("BeadID = %q, want %q", rec.BeadID, id.BeadID)
	}
	if rec.Provider != id.Provider {
		t.Errorf("Provider = %q, want %q", rec.Provider, id.Provider)
	}
	if rec.Stream != string(id.Stream) {
		t.Errorf("Stream = %q, want %q", rec.Stream, id.Stream)
	}
	if rec.Sequence != 0 {
		t.Errorf("Sequence = %d, want 0", rec.Sequence)
	}
	if rec.Offset != 1024 {
		t.Errorf("Offset = %d, want 1024", rec.Offset)
	}
	if rec.Severity != SeverityDefault {
		t.Errorf("Severity = %q, want %q", rec.Severity, SeverityDefault)
	}
	if rec.Message != `{"event":"hello"}` {
		t.Errorf("Message = %q, want %q", rec.Message, `{"event":"hello"}`)
	}
	if rec.Time != "2026-04-28T12:00:00Z" {
		t.Errorf("Time = %q, want 2026-04-28T12:00:00Z", rec.Time)
	}
}

// TestStdoutSink_StderrStreamPromotesSeverity asserts the sink classifies
// stderr lines at ERROR severity so Cloud Logging's standard filter on
// severity catches stderr without depending on stream-specific log
// names.
func TestStdoutSink_StderrStreamPromotesSeverity(t *testing.T) {
	var buf bytes.Buffer
	sink, err := NewStdoutSink(&buf, &atomicStats{})
	if err != nil {
		t.Fatalf("NewStdoutSink: %v", err)
	}
	sink.SetClock(fixedClock())

	id := stableIdentity()
	id.Stream = logartifact.StreamStderr
	sink.Emit(id, "/var/spire/logs/foo/stderr.jsonl", 0, 0, []byte("panic: nil pointer\n"))

	var rec StdoutRecord
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if rec.Severity != SeverityError {
		t.Errorf("Severity = %q, want %q", rec.Severity, SeverityError)
	}
}

// TestStdoutSink_OmitsProviderWhenEmpty covers the operational-stream
// case: the JSON emits no provider field at all when the source
// Identity has no provider segment, matching the path schema where
// operational logs sit at a sibling depth without provider.
func TestStdoutSink_OmitsProviderWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	sink, err := NewStdoutSink(&buf, &atomicStats{})
	if err != nil {
		t.Fatalf("NewStdoutSink: %v", err)
	}
	sink.SetClock(fixedClock())

	id := stableIdentity()
	id.Provider = ""
	id.Stream = logartifact.StreamStdout
	sink.Emit(id, "/var/spire/logs/foo/operational.log", 0, 0, []byte("hello\n"))

	if strings.Contains(buf.String(), `"provider"`) {
		t.Errorf("output unexpectedly contains a provider field: %s", buf.String())
	}
}

// TestStdoutSink_TrimsTrailingNewlineFromMessage ensures the JSON
// message value carries no embedded newline so consumers parsing one
// JSON record per line don't need to re-trim.
func TestStdoutSink_TrimsTrailingNewlineFromMessage(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"unix newline", "hello world\n", "hello world"},
		{"crlf", "hello world\r\n", "hello world"},
		{"no newline", "hello world", "hello world"},
		{"trailing whitespace preserved", "hello world  ", "hello world  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			sink, err := NewStdoutSink(&buf, &atomicStats{})
			if err != nil {
				t.Fatalf("NewStdoutSink: %v", err)
			}
			sink.SetClock(fixedClock())
			sink.Emit(stableIdentity(), "x", 0, 0, []byte(tc.raw))

			var rec StdoutRecord
			if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if rec.Message != tc.want {
				t.Errorf("Message = %q, want %q", rec.Message, tc.want)
			}
		})
	}
}

// TestStdoutSink_OperationalEventCarriesComponent verifies the
// EmitOperational path renders a self-describing record with the
// `spire-log-exporter` component label so Cloud Logging filters can
// distinguish exporter operational events from tailed agent lines.
func TestStdoutSink_OperationalEventCarriesComponent(t *testing.T) {
	var buf bytes.Buffer
	sink, err := NewStdoutSink(&buf, &atomicStats{})
	if err != nil {
		t.Fatalf("NewStdoutSink: %v", err)
	}
	sink.SetClock(fixedClock())

	sink.EmitOperational(SeverityError, "tower-a", "logexport: open artifact failed", map[string]string{
		"bead_id": "spi-bead",
	})

	out := buf.String()
	if !strings.Contains(out, `"component":"spire-log-exporter"`) {
		t.Errorf("operational event missing component label: %s", out)
	}
	if !strings.Contains(out, `"severity":"ERROR"`) {
		t.Errorf("operational event missing ERROR severity: %s", out)
	}
	if !strings.Contains(out, `"bead_id":"spi-bead"`) {
		t.Errorf("operational event missing fields.bead_id: %s", out)
	}
}

// TestStdoutSink_Counters verifies the lines counter increments per
// successful Emit so /healthz can surface tailing rate.
func TestStdoutSink_Counters(t *testing.T) {
	var buf bytes.Buffer
	stats := &atomicStats{}
	sink, err := NewStdoutSink(&buf, stats)
	if err != nil {
		t.Fatalf("NewStdoutSink: %v", err)
	}
	sink.SetClock(fixedClock())

	for i := 0; i < 5; i++ {
		sink.Emit(stableIdentity(), "x", 0, int64(i*4), []byte("a"))
	}
	if got := stats.Snapshot().LinesEmitted; got != 5 {
		t.Errorf("LinesEmitted = %d, want 5", got)
	}
}
