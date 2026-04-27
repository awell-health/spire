// Package reset is the shared entry point for the soft-reset code path.
//
// "Soft reset" means: SIGTERM any registered wizard PID (5s grace, then
// SIGKILL), remove the registry entry, strip interrupted:* and needs-human
// labels off the bead, unhook hooked step children, and walk the bead back
// via softResetV3 (when Opts.To is set) or resetV3 (otherwise). It does
// NOT perform the destructive `--hard` worktree+graph deletion — that path
// stays in cmd/spire.
//
// Two callers share this code:
//
//   - cmd/spire's `cmdReset` (CLI: `spire reset <id>`)
//   - pkg/gateway's `handleResetBead` (HTTP: POST /api/v1/beads/{id}/reset)
//
// Both reach the same kill-wizard → strip-labels → unhook → walk-back
// sequence so unsummon semantics are identical regardless of which surface
// initiated the reset.
//
// # Wiring
//
// The actual implementation lives in cmd/spire (it depends on a number of
// internal helpers that haven't been moved out yet — formula resolution,
// graph-state management, the protected-bead set, recovery cascade). This
// package exposes a small, public API and a package-level seam (RunFunc)
// that cmd/spire wires in its init(). The gateway calls ResetBead directly;
// the CLI calls cmdReset which delegates to the same RunFunc internally.
//
// Tests in cmd/spire continue to exercise the soft-reset internals
// (resetV3, softResetV3, parseSetFlag, …) directly. Tests in this package
// validate the API contract (ErrNotWired when unwired, delegation when
// wired).
package reset

import (
	"context"
	"errors"

	"github.com/awell-health/spire/pkg/store"
)

// Opts mirrors the CLI's `--to / --force / --set` flag set so the gateway
// body schema and the CLI flag set produce the same call shape.
type Opts struct {
	// BeadID is the bead to reset (e.g. "spi-abc123"). Required.
	BeadID string

	// To, when non-empty, names the target step to rewind to. Empty
	// performs a plain (non-`--to`) soft reset that walks the bead back
	// to "open".
	To string

	// Force, with To, drops the "target must have been reached"
	// precondition; pending steps outside the rewind set are advanced
	// to completed with empty outputs.
	Force bool

	// Set, with To, overrides step outputs. Keys are
	// "<step>.outputs.<key>"; values are the override value (may
	// contain '=').
	Set map[string]string

	// Hard, when true, runs the destructive worktree+branch+graph-state
	// reset path instead of the soft rewind. Mutually exclusive with To
	// (callers should reject Hard+To at the surface — CLI does this; the
	// gateway does too). Hard is incompatible with Force/Set; those flags
	// are ignored when Hard is true.
	Hard bool
}

// ErrNotWired is returned by ResetBead when RunFunc has not been wired.
// In production cmd/spire wires it during init(); a test that imports
// pkg/reset without booting cmd/spire will see this error.
var ErrNotWired = errors.New("reset: RunFunc is not wired")

// ErrInvalidStep wraps validation failures where Opts.To names a step
// that doesn't exist in the bead's resolved formula. HTTP callers map
// this to 400.
var ErrInvalidStep = errors.New("invalid step")

// ErrSetSyntax wraps validation failures from the Set map: malformed
// `<step>.outputs.<key>` paths, unknown step references inside Set, or
// attempts to write `<step>.status=...` (rejected because --set is
// scoped to outputs, matching CLI behavior). HTTP callers map this to
// 400.
var ErrSetSyntax = errors.New("invalid --set token")

// ErrTargetNotReached wraps the "target must have been reached"
// precondition failure: To names a step the bead's graph state hasn't
// reached, and Force is false. HTTP callers map this to 409.
var ErrTargetNotReached = errors.New("target step not reached")

// ErrNoGraphState wraps the "no graph state to rewind" condition: the
// bead's wizard has no graph_state.json on disk, so a soft rewind is
// impossible. HTTP callers map this to 409.
var ErrNoGraphState = errors.New("no graph state to rewind")

// ErrConflict wraps generic "cannot proceed in current state" errors
// such as a live wizard owning the bead during a --force pass. HTTP
// callers map this to 409.
var ErrConflict = errors.New("reset state conflict")

// RunFunc is the package-level seam through which ResetBead invokes the
// real soft-reset implementation. cmd/spire wires this in init() to a
// function that runs the kill-wizard + strip-labels + unhook + walk-back
// sequence and returns the post-reset bead. Tests may swap it.
var RunFunc func(ctx context.Context, opts Opts) (*store.Bead, error)

// ResetBead performs a soft reset. SIGTERMs any registered wizard PID
// (5s grace, then SIGKILL), removes the registry entry, strips
// interrupted:* and needs-human labels, unhooks step children, and walks
// the bead back via softResetV3 (when Opts.To is set) or resetV3
// (otherwise). Returns the post-reset bead so callers can re-render
// without a follow-up GET.
//
// Returns ErrNotWired if the implementation has not been registered;
// otherwise returns whatever the wired implementation returns. The
// implementation is responsible for translating "not found" into an
// error whose message contains "not found" so HTTP callers can map it
// to 404.
func ResetBead(ctx context.Context, opts Opts) (*store.Bead, error) {
	if RunFunc == nil {
		return nil, ErrNotWired
	}
	return RunFunc(ctx, opts)
}
