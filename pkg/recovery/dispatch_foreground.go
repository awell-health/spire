package recovery

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/awell-health/spire/pkg/store"
)

// PhaseEvent is one observation of a cleric phase completing. The
// foreground debug-dispatch path emits one PhaseEvent per phase run —
// retries produce multiple events for the same Phase name. Steward
// paths never construct a PhaseEvent; this surface is observability-
// only and carries no lifecycle authority.
type PhaseEvent struct {
	// Phase is the cleric step that just completed. One of
	// "collect_context", "decide", "execute", "verify", "learn",
	// "finish", or a formula-declared alias like
	// "finish_needs_human"/"retry_on_error".
	Phase string

	// Step is the formula-declared step name. Matches Phase for the
	// six canonical cleric phases; distinct for formula aliases.
	Step string

	// Branch names the priority-ladder rung that fired inside
	// recovery.Decide: "budget", "guidance", "recipe", "claude", or
	// "fallback". Empty for non-decide phases.
	Branch string

	// Action is the action the phase executed — e.g. a mechanical
	// name ("rebase-onto-base") on execute, or the final decision
	// action on decide/finish. Empty when unknown.
	Action string

	// Confidence is the decide-time confidence score when available.
	Confidence float64

	// Reason carries decide reasoning or finish-time summary text.
	Reason string

	// Verdict is the verify-step result: "pass", "fail", or "timeout".
	// Empty for non-verify phases.
	Verdict string

	// Details is an arbitrary key/value bag the formatter appends in
	// sorted order. Used for ad hoc fields that don't warrant a
	// top-level column.
	Details map[string]any

	// Err carries the error message when a phase completed with an
	// error (hooked path or on_error=record). Empty when the phase
	// succeeded cleanly.
	Err string

	// Ts is the event emission time, stamped by the foreground
	// dispatcher.
	Ts time.Time
}

// ForegroundRunner is the executor-side entry point the foreground
// dispatcher invokes. It runs the cleric-default formula synchronously
// against the recovery bead, emitting one PhaseEvent per phase on
// events, and returns the final RecoveryOutcome.
//
// The runner owns no channels — the caller (DispatchForeground) is
// responsible for draining and closing events.
//
// This is a function-type seam rather than a direct pkg/executor call
// because pkg/executor already imports pkg/recovery; wrapping the
// executor entry point in a closure breaks the import cycle without
// adding an intermediary package.
type ForegroundRunner func(ctx context.Context, bead *store.Bead, events chan<- PhaseEvent) (RecoveryOutcome, error)

// DispatchForeground loads the recovery bead, verifies it carries a
// caused-by / recovery-for edge, starts a formatter goroutine that
// renders each PhaseEvent as a single human-readable status line,
// invokes the runner synchronously, and returns the final
// RecoveryOutcome read back through ReadOutcome for durability.
//
// Lifecycle:
//  1. Load bead via store.GetBead and reject non-recovery beads with
//     a clear error — this is a debug surface and refusing early
//     avoids side-effects on production beads.
//  2. Create a buffered PhaseEvent channel and spin up a formatter
//     goroutine that renders into out.
//  3. Hand the channel to the runner; the runner emits events and
//     returns the outcome it read from durable state.
//  4. Close the channel and wait for the formatter to drain — the
//     caller is the sole channel owner.
//  5. Re-read the outcome via ReadOutcome so the value returned to
//     the CLI matches what the finish step durably wrote.
func DispatchForeground(ctx context.Context, beadID string, out io.Writer, runner ForegroundRunner) (RecoveryOutcome, error) {
	if runner == nil {
		return RecoveryOutcome{}, fmt.Errorf("dispatch foreground: runner is required")
	}
	if strings.TrimSpace(beadID) == "" {
		return RecoveryOutcome{}, fmt.Errorf("dispatch foreground: bead ID is required")
	}

	bead, err := store.GetBead(beadID)
	if err != nil {
		return RecoveryOutcome{}, fmt.Errorf("load bead %s: %w", beadID, err)
	}
	if err := ensureRecoveryBead(bead); err != nil {
		return RecoveryOutcome{}, err
	}

	events := make(chan PhaseEvent, 32)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ev := range events {
			FormatPhaseEvent(out, ev)
		}
	}()

	outcome, runErr := runner(ctx, &bead, events)
	close(events)
	wg.Wait()

	// Re-read the durably persisted outcome so the CLI exit code and
	// summary line reflect exactly what finish wrote, not whatever
	// in-memory value the runner handed back. On a successful run the
	// two agree; on partial/error paths ReadOutcome is the source of
	// truth for the steward contract.
	if runErr == nil {
		if fresh, err2 := store.GetBead(beadID); err2 == nil {
			if persisted, ok := ReadOutcome(fresh); ok {
				outcome = persisted
			}
		}
	}

	return outcome, runErr
}

// FormatPhaseEvent renders a PhaseEvent as a single human-readable
// status line on out. One event, one line — plus an optional indented
// "ERR:" line when Err is non-empty. Unknown phases fall through to a
// generic "[phase] details" shape so callers can emit formula-aliased
// phases (finish_needs_human, retry_on_error) without format changes.
func FormatPhaseEvent(out io.Writer, ev PhaseEvent) {
	if out == nil {
		return
	}
	switch ev.Phase {
	case "collect_context":
		if details := renderDetails(ev.Details); details != "" {
			fmt.Fprintf(out, "[collect_context] %s\n", details)
		} else {
			fmt.Fprintf(out, "[collect_context]\n")
		}
	case "decide":
		fmt.Fprintf(out, "[decide] branch=%s action=%s confidence=%.2f reason=%q\n",
			ev.Branch, ev.Action, ev.Confidence, ev.Reason)
	case "execute":
		line := fmt.Sprintf("[execute] action=%s", ev.Action)
		if details := renderDetails(ev.Details); details != "" {
			line += " " + details
		}
		fmt.Fprintln(out, line)
	case "verify":
		line := fmt.Sprintf("[verify] verdict=%s", ev.Verdict)
		if details := renderDetails(ev.Details); details != "" {
			line += " " + details
		}
		fmt.Fprintln(out, line)
	case "learn":
		line := "[learn]"
		if details := renderDetails(ev.Details); details != "" {
			line += " " + details
		}
		fmt.Fprintln(out, line)
	case "finish", "finish_needs_human", "finish_needs_human_on_error":
		line := fmt.Sprintf("[%s]", ev.Phase)
		if details := renderDetails(ev.Details); details != "" {
			line += " " + details
		}
		fmt.Fprintln(out, line)
	default:
		line := fmt.Sprintf("[%s]", ev.Phase)
		if details := renderDetails(ev.Details); details != "" {
			line += " " + details
		}
		fmt.Fprintln(out, line)
	}
	if ev.Err != "" {
		fmt.Fprintf(out, "  ERR: %s\n", ev.Err)
	}
}

// renderDetails joins ev.Details into a stable "k=v k=v" string with
// keys sorted alphabetically. Values are formatted via %v. Empty map
// yields "".
func renderDetails(details map[string]any) string {
	if len(details) == 0 {
		return ""
	}
	keys := make([]string, 0, len(details))
	for k := range details {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s=%v", k, details[k])
	}
	return b.String()
}

// ensureRecoveryBead returns an error when bead is not a recovery
// bead — i.e. carries neither the "recovery-bead" label nor a
// caused-by/recovery-for dependency edge. The foreground debug
// surface refuses to run cleric against arbitrary beads; requiring
// one of these markers prevents accidental dispatch against a normal
// work bead.
func ensureRecoveryBead(bead store.Bead) error {
	for _, l := range bead.Labels {
		if l == "recovery-bead" {
			return nil
		}
	}
	deps, err := store.GetDepsWithMeta(bead.ID)
	if err != nil {
		return fmt.Errorf("verify recovery bead %s: %w", bead.ID, err)
	}
	for _, d := range deps {
		dt := string(d.DependencyType)
		if dt == "caused-by" || dt == "recovery-for" {
			return nil
		}
	}
	return fmt.Errorf("bead %s is not a recovery bead (no recovery-bead label and no caused-by/recovery-for edge)", bead.ID)
}
