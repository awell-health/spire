package executor

import (
	"time"

	"github.com/awell-health/spire/pkg/agent"
)

// attemptHeartbeatInterval is the maximum age the executor allows between
// parent-attempt heartbeat writes while the wizard is running or blocked in a
// child wait. Tests override it to keep wall-clock time short.
var attemptHeartbeatInterval = 30 * time.Second

func (e *Executor) heartbeatInterval() time.Duration {
	if attemptHeartbeatInterval <= 0 {
		return 30 * time.Second
	}
	return attemptHeartbeatInterval
}

// maybeHeartbeatAttempt keeps the active attempt bead's last_seen_at fresh.
// The helper is shared by the main graph loop and any synchronous child-wait
// path so steward sees a live parent wizard even while the executor is blocked
// on a long-running sage/apprentice/repair worker.
func (e *Executor) maybeHeartbeatAttempt(attemptID string) {
	if e == nil || attemptID == "" || e.deps == nil || e.deps.UpdateAttemptHeartbeat == nil {
		return
	}

	interval := e.heartbeatInterval()

	e.heartbeatMu.Lock()
	if e.heartbeatInFlight {
		e.heartbeatMu.Unlock()
		return
	}
	if !e.lastHeartbeat.IsZero() && time.Since(e.lastHeartbeat) < interval {
		e.heartbeatMu.Unlock()
		return
	}
	e.heartbeatInFlight = true
	e.heartbeatMu.Unlock()

	err := e.deps.UpdateAttemptHeartbeat(attemptID)

	e.heartbeatMu.Lock()
	if err == nil {
		e.lastHeartbeat = time.Now()
	}
	e.heartbeatInFlight = false
	e.heartbeatMu.Unlock()

	if err != nil && e.log != nil {
		e.log("warning: heartbeat: %s", err)
	}
}

// waitHandleWithHeartbeat blocks on a child handle while continuing to refresh
// the parent attempt heartbeat. This closes the gap where the executor would
// stop heartbeating after entering a long handle.Wait(), causing steward to
// kill live nested review / repair work.
func (e *Executor) waitHandleWithHeartbeat(handle agent.Handle) error {
	if handle == nil {
		return nil
	}
	if e == nil {
		return handle.Wait()
	}
	attemptID := e.attemptID()
	if attemptID == "" || e.deps == nil || e.deps.UpdateAttemptHeartbeat == nil {
		return handle.Wait()
	}

	// Prime the heartbeat before blocking so a fresh attempt does not sit
	// heartbeat-less for the first interval.
	e.maybeHeartbeatAttempt(attemptID)

	done := make(chan error, 1)
	go func() {
		done <- handle.Wait()
	}()

	ticker := time.NewTicker(e.heartbeatInterval())
	defer ticker.Stop()

	for {
		select {
		case err := <-done:
			return err
		case <-ticker.C:
			e.maybeHeartbeatAttempt(attemptID)
		}
	}
}
