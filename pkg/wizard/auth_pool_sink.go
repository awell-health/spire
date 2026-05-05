package wizard

// auth_pool_sink.go — io.Writer that scans the apprentice's claude JSONL
// stream for rate_limit_event lines and applies them back to the per-slot
// state in pkg/auth/pool's on-disk cache.
//
// The wizard's runWizardClaudeOnce already builds an io.MultiWriter for
// the claude subprocess's stdout (so the JSONL gets buffered and tee'd
// to a per-attempt log file). This sink is layered onto that
// MultiWriter when the env vars SPIRE_AUTH_SLOT and
// SPIRE_AUTH_POOL_STATE_DIR are set; absence of either silences the
// sink (the legacy single-token path is untouched).
//
// Mutate semantics: cache.MutateSlotState already takes the slot's
// per-file flock, so concurrent apprentices applying events to the
// same slot serialize at the cache layer. Errors from individual
// events are logged but never propagated — losing a rate-limit beat
// must not kill the apprentice.

import (
	"bytes"
	"io"
	"log"
	"sync"
	"time"

	"github.com/awell-health/spire/pkg/auth/pool"
)

// rateLimitEventSink is an io.Writer that buffers partial JSONL lines
// and applies each completed line as a candidate rate-limit event to
// the slot's cached state. It is safe to use as one sink in an
// io.MultiWriter chain — Write returns len(p), nil unconditionally so
// upstream writers don't observe a sink failure as a stream failure.
type rateLimitEventSink struct {
	stateDir string
	slotName string

	mu  sync.Mutex
	buf []byte
}

// newRateLimitEventSink returns an io.Writer that applies rate-limit
// events to the slot at <stateDir>/<slotName>.json. When stateDir or
// slotName is empty, returns io.Discard so callers can splice the sink
// in unconditionally without nil-checks.
func newRateLimitEventSink(stateDir, slotName string) io.Writer {
	if stateDir == "" || slotName == "" {
		return io.Discard
	}
	return &rateLimitEventSink{stateDir: stateDir, slotName: slotName}
}

// Write buffers p, scans for completed lines (newline-terminated), and
// hands each one to processLine. Always returns len(p), nil — the sink
// is downstream-of-tee, so upstream writers must not observe its work.
func (s *rateLimitEventSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)

	for {
		idx := bytes.IndexByte(s.buf, '\n')
		if idx < 0 {
			break
		}
		line := s.buf[:idx]
		s.buf = s.buf[idx+1:]
		s.processLine(line)
	}
	// Cap the partial-line buffer so a stream missing newlines forever
	// does not balloon memory. 1 MiB is well above any realistic JSONL
	// message; past that, we drop the buffer and resync at the next
	// newline.
	if len(s.buf) > 1<<20 {
		s.buf = s.buf[:0]
	}
	return len(p), nil
}

// processLine inspects line for a rate-limit event and applies it to
// the slot's cached state. Non-events and unparseable JSON are
// silently ignored; malformed rate-limit shapes log at debug level.
func (s *rateLimitEventSink) processLine(line []byte) {
	if len(line) == 0 {
		return
	}
	event, isRL, parseErr := pool.ParseRateLimitEvent(line)
	if !isRL {
		return
	}
	if parseErr != nil {
		log.Printf("wizard: rate-limit event parse failed for slot %q: %v", s.slotName, parseErr)
		return
	}
	if event == nil {
		return
	}
	now := time.Now()
	if mutateErr := pool.MutateSlotState(s.stateDir, s.slotName, func(st *pool.SlotState) error {
		event.ApplyTo(st, now)
		return nil
	}); mutateErr != nil {
		log.Printf("wizard: apply rate-limit event to slot %q: %v", s.slotName, mutateErr)
	}
}
