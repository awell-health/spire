package logexport

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/awell-health/spire/pkg/logartifact"
)

// StdoutRecord is the JSON shape one tailed line emits to stdout for
// Cloud Logging ingestion. The field set matches the design (spi-7wzwk2)
// so GKE / Cloud Logging filters can target tower / bead / attempt /
// run / agent / role / phase / provider / stream / file / sequence
// without parsing a free-form message column.
//
// Severity is a Cloud Logging-canonical level string (DEFAULT, INFO,
// ERROR). The exporter promotes lines that look like structured stderr
// to ERROR; everything else stays at DEFAULT and downstream filters
// classify by stream/severity together.
//
// Time is RFC3339Nano so a Cloud Logging entry's `timestamp` round-trips
// to the same wall-clock value the tailer observed; rendering callers
// should never depend on exporter-side wall-clock for ordering — the
// (file, sequence, byte_offset) cursor is the canonical order.
type StdoutRecord struct {
	Time      string `json:"time"`
	Severity  string `json:"severity"`
	Tower     string `json:"tower"`
	BeadID    string `json:"bead_id"`
	AttemptID string `json:"attempt_id"`
	RunID     string `json:"run_id"`
	AgentName string `json:"agent_name"`
	Role      string `json:"role"`
	Phase     string `json:"phase"`
	Provider  string `json:"provider,omitempty"`
	Stream    string `json:"stream"`
	File      string `json:"file"`
	Sequence  int    `json:"sequence"`
	// Offset is the byte offset of this record's first byte within
	// File. Lets a future live-follow consumer deduplicate or resume
	// after reconnect without re-emitting work.
	Offset int64 `json:"offset"`
	// Message is the tailed line, trimmed of its trailing newline.
	Message string `json:"message"`
}

// SeverityDefault is Cloud Logging's "no level" classification. The
// majority of tailed lines emit at this severity; the StdoutSink only
// promotes to ERROR when the source stream itself is a stderr or when
// an exporter operational event signals failure.
const (
	SeverityDefault = "DEFAULT"
	SeverityInfo    = "INFO"
	SeverityError   = "ERROR"
)

// StdoutSink writes one JSON record per tailed line to its underlying
// io.Writer. The sink is safe for concurrent use because lines from
// different per-file goroutines may interleave without corrupting one
// another — the writer is wrapped under a mutex and each record is
// emitted as a single Write call.
type StdoutSink struct {
	mu  sync.Mutex
	w   io.Writer
	enc *json.Encoder

	// nowFn is overridable so tests can pin the timestamp without
	// resorting to time-equality fuzzers.
	nowFn func() time.Time

	stats *atomicStats
}

// NewStdoutSink returns a StdoutSink writing to w. The caller usually
// passes os.Stdout — Cloud Logging's GKE collector picks records up
// from container stdout without further configuration.
//
// Returns an error when w is nil so callers fail fast at construction.
func NewStdoutSink(w io.Writer, stats *atomicStats) (*StdoutSink, error) {
	if w == nil {
		return nil, fmt.Errorf("logexport: NewStdoutSink: writer must not be nil")
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return &StdoutSink{w: w, enc: enc, nowFn: time.Now, stats: stats}, nil
}

// SetClock overrides the sink's clock. Called only from tests.
func (s *StdoutSink) SetClock(fn func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if fn != nil {
		s.nowFn = fn
	}
}

// Emit writes one StdoutRecord composed from id, file, seq, offset, and
// the raw line bytes. The trailing newline (if any) is trimmed from the
// message before encoding so the JSON record carries no embedded \n.
//
// Emit never returns the underlying writer's error to the caller — the
// exporter's contract is that stdout failures must not interrupt the
// tailer. Errors are accounted via the failed counter so operational
// status is still observable.
func (s *StdoutSink) Emit(id logartifact.Identity, file string, sequence int, offset int64, line []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()

	severity := SeverityDefault
	if id.Stream == logartifact.StreamStderr {
		severity = SeverityError
	}

	rec := StdoutRecord{
		Time:      s.nowFn().UTC().Format(time.RFC3339Nano),
		Severity:  severity,
		Tower:     id.Tower,
		BeadID:    id.BeadID,
		AttemptID: id.AttemptID,
		RunID:     id.RunID,
		AgentName: id.AgentName,
		Role:      string(id.Role),
		Phase:     id.Phase,
		Provider:  id.Provider,
		Stream:    string(id.Stream),
		File:      file,
		Sequence:  sequence,
		Offset:    offset,
		Message:   strings.TrimRight(string(line), "\r\n"),
	}
	if err := s.enc.Encode(rec); err != nil {
		// Encoder errors generally indicate a downstream stdout
		// failure (broken pipe, full disk on the node-level log
		// collector). The exporter is best-effort; record the failure
		// in stats and keep going.
		if s.stats != nil {
			s.stats.incFailed()
		}
		return
	}
	if s.stats != nil {
		s.stats.incLines()
	}
}

// EmitOperational writes a self-describing exporter event at the given
// severity. Used for "exporter started", "manifest finalize failed", and
// other internal events that are not associated with a tailed file.
func (s *StdoutSink) EmitOperational(severity, tower, message string, kv map[string]string) {
	if severity == "" {
		severity = SeverityInfo
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	type opRec struct {
		Time      string            `json:"time"`
		Severity  string            `json:"severity"`
		Component string            `json:"component"`
		Tower     string            `json:"tower,omitempty"`
		Message   string            `json:"message"`
		Fields    map[string]string `json:"fields,omitempty"`
	}
	rec := opRec{
		Time:      s.nowFn().UTC().Format(time.RFC3339Nano),
		Severity:  severity,
		Component: "spire-log-exporter",
		Tower:     tower,
		Message:   message,
		Fields:    kv,
	}
	_ = s.enc.Encode(rec) // best-effort; same rationale as Emit.
}
