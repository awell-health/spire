package wizard

import (
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/auth/pool"
)

// TestRateLimitEventSink_DiscardsWhenUnconfigured verifies that an unset
// stateDir or slotName collapses the sink to io.Discard so the legacy
// single-token path stays a no-op.
func TestRateLimitEventSink_DiscardsWhenUnconfigured(t *testing.T) {
	if w := newRateLimitEventSink("", "slot-a"); w != io.Discard {
		t.Errorf("empty stateDir: sink = %T, want io.Discard", w)
	}
	if w := newRateLimitEventSink("/tmp/x", ""); w != io.Discard {
		t.Errorf("empty slotName: sink = %T, want io.Discard", w)
	}
}

// TestRateLimitEventSink_AppliesEvent feeds a rate-limit-event JSONL line
// through the sink and verifies the slot's on-disk state was mutated.
func TestRateLimitEventSink_AppliesEvent(t *testing.T) {
	stateDir := t.TempDir()
	slot := "slot-a"

	sink := newRateLimitEventSink(stateDir, slot)
	resetsAt := time.Now().Add(2 * time.Hour).Unix()
	line := []byte(fmt.Sprintf(
		`{"type":"rate_limit_event","rateLimitType":"five_hour","overageStatus":"rejected","resetsAt":%d}`+"\n",
		resetsAt,
	))

	if _, err := sink.Write(line); err != nil {
		t.Fatalf("sink.Write: %v", err)
	}

	state, err := pool.ReadSlotState(stateDir, slot)
	if err != nil {
		t.Fatalf("ReadSlotState: %v", err)
	}
	if state.RateLimit.FiveHour.Status != pool.RateLimitStatusRejected {
		t.Errorf("FiveHour.Status = %q, want %q", state.RateLimit.FiveHour.Status, pool.RateLimitStatusRejected)
	}
	if state.RateLimit.FiveHour.ResetsAt.Unix() != resetsAt {
		t.Errorf("FiveHour.ResetsAt.Unix() = %d, want %d", state.RateLimit.FiveHour.ResetsAt.Unix(), resetsAt)
	}
	if state.RateLimit.Recent429 == nil {
		t.Error("Recent429 must be non-nil for a rejection event")
	}
}

// TestRateLimitEventSink_IgnoresNonEventLines verifies that arbitrary
// JSONL traffic does not perturb slot state — the sink only reacts to
// rate_limit_event lines.
func TestRateLimitEventSink_IgnoresNonEventLines(t *testing.T) {
	stateDir := t.TempDir()
	slot := "slot-b"

	sink := newRateLimitEventSink(stateDir, slot)
	lines := []string{
		`{"type":"assistant_message","content":"hi"}` + "\n",
		`{"type":"tool_use","name":"Read"}` + "\n",
		`not even json` + "\n",
	}
	for _, ln := range lines {
		if _, err := sink.Write([]byte(ln)); err != nil {
			t.Fatalf("sink.Write %q: %v", ln, err)
		}
	}

	state, err := pool.ReadSlotState(stateDir, slot)
	if err != nil {
		t.Fatalf("ReadSlotState: %v", err)
	}
	if state.RateLimit.FiveHour.Status != "" || state.RateLimit.Overage.Status != "" {
		t.Errorf("non-event lines unexpectedly mutated slot state: %+v", state.RateLimit)
	}
}

// TestRateLimitEventSink_HandlesPartialLines verifies the buffer joins
// fragments split across multiple Write calls.
func TestRateLimitEventSink_HandlesPartialLines(t *testing.T) {
	stateDir := t.TempDir()
	slot := "slot-c"
	sink := newRateLimitEventSink(stateDir, slot)

	resetsAt := time.Now().Add(time.Hour).Unix()
	full := fmt.Sprintf(
		`{"type":"rate_limit_event","rateLimitType":"overage","overageStatus":"allowed_warning","resetsAt":%d}`+"\n",
		resetsAt,
	)

	half := full[:len(full)/2]
	rest := full[len(full)/2:]

	if _, err := sink.Write([]byte(half)); err != nil {
		t.Fatalf("write half: %v", err)
	}
	if _, err := sink.Write([]byte(rest)); err != nil {
		t.Fatalf("write rest: %v", err)
	}

	state, err := pool.ReadSlotState(stateDir, slot)
	if err != nil {
		t.Fatalf("ReadSlotState: %v", err)
	}
	if state.RateLimit.Overage.Status != pool.RateLimitStatusAllowedWarning {
		t.Errorf("Overage.Status = %q, want %q", state.RateLimit.Overage.Status, pool.RateLimitStatusAllowedWarning)
	}
}

// TestRateLimitEventSink_WriteAlwaysSucceeds confirms the sink never
// reports an error to the io.MultiWriter chain — losing a beat must not
// kill claude.
func TestRateLimitEventSink_WriteAlwaysSucceeds(t *testing.T) {
	sink := newRateLimitEventSink(t.TempDir(), "slot-d")
	payload := []byte("garbage that is not json\n")
	n, err := sink.Write(payload)
	if err != nil {
		t.Errorf("Write returned err = %v, want nil", err)
	}
	if n != len(payload) {
		t.Errorf("Write returned n = %d, want %d", n, len(payload))
	}
}
