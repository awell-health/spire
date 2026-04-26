// Package close is the shared entry point for the bead close lifecycle.
//
// The close lifecycle is: traverse direct workflow-step children + legacy
// workflow molecule children and close each through the same lifecycle,
// strip phase:* / interrupted:* labels, close the bead, then cascade-close
// any open caused-by alert beads.
//
// Two callers share this code:
//
//   - cmd/spire's `cmdClose` (CLI: `spire close <id>`) in direct mode
//   - pkg/gateway's `handleBeadClose` (HTTP: POST /api/v1/beads/{id}/close)
//     for gateway-mode towers
//
// Both reach the same close-children → strip-labels → close-parent →
// cascade-alerts sequence so close semantics are identical regardless of
// which surface initiated the close. In gateway mode, `cmdClose` short-
// circuits to the gatewayclient and the actual lifecycle runs server-side
// against direct Dolt — which is required because the workflow-child
// discovery (GetChildren, RemoveLabel, GetDependentsWithMeta) has no
// gateway-client equivalent.
//
// # Wiring
//
// The actual implementation lives in cmd/spire (it depends on a number of
// internal helpers — phase label resolution, alert cascade, step bead
// detection). This package exposes a small, public API and a package-level
// seam (RunFunc) that cmd/spire wires in its init(). The gateway calls
// RunLifecycle directly; the CLI calls cmdClose which delegates to the
// same RunFunc internally.
package close

import "errors"

// ErrNotWired is returned by RunLifecycle when RunFunc has not been wired.
// In production cmd/spire wires it during init(); a test that imports
// pkg/close without booting cmd/spire will see this error.
var ErrNotWired = errors.New("close: RunFunc is not wired")

// RunFunc is the package-level seam through which RunLifecycle invokes
// the real close-lifecycle implementation. cmd/spire wires this in init()
// to a function that runs the close-children + strip-labels + close-bead
// + cascade-alerts sequence. Tests may swap it.
var RunFunc func(beadID string) error

// RunLifecycle performs the close lifecycle for one bead: traverses
// workflow-step children + legacy molecule children, strips phase:* /
// interrupted:* labels, closes the bead, then cascade-closes open
// caused-by alert beads. Idempotent on already-closed parents (the
// child traversal still runs so stale descendants get repaired).
//
// Returns ErrNotWired if the implementation has not been registered;
// otherwise returns whatever the wired implementation returns. The
// implementation is responsible for translating "not found" into an
// error whose message contains "not found" so HTTP callers can map it
// to 404. Errors from the workflow-step child discovery (e.g.
// store.ErrGatewayUnsupported) MUST propagate — the parent must NOT be
// closed when child cleanup cannot be verified.
func RunLifecycle(beadID string) error {
	if RunFunc == nil {
		return ErrNotWired
	}
	return RunFunc(beadID)
}
