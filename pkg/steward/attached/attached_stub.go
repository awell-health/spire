// Package attached is a reserved stub for the attached deployment mode
// (DeploymentModeAttachedReserved in pkg/config). It exists so that every
// dispatch path has a typed, addressable not-implemented surface for
// attached mode — rather than a silent fallback to local-native or
// cluster-native — and so that a future track implementing attached mode
// has an unambiguous place to grow.
//
// Attached mode is a local control plane targeting a remote cluster
// execution surface through an explicit remote seam. It will reuse the
// existing WorkloadIntent / IntentPublisher / IntentConsumer seams from
// pkg/steward/intent and MUST NOT change the spi-xplwy runtime contract,
// repo-identity ownership, or attempt-bead ownership. See
// docs/attached-mode.md for the full reservation.
//
// This package intentionally exports exactly one function,
// AttachedDispatch, and exactly one error, ErrAttachedNotImplemented.
// Reviewers should reject any change that adds further exported symbols
// here before attached mode has been designed end-to-end. A test in this
// package enforces that invariant.
package attached

import (
	"context"
	"errors"

	"github.com/awell-health/spire/pkg/steward/intent"
)

// ErrAttachedNotImplemented is the typed sentinel every attached-mode
// call path returns today. Consumers that observe
// config.DeploymentModeAttachedReserved MUST return this error (or wrap
// it with errors.Is-compatible wrapping) rather than silently falling
// back to another deployment mode. The message points at
// docs/attached-mode.md so an operator who hits this error at runtime
// knows where to read for context.
var ErrAttachedNotImplemented = errors.New(
	"attached deployment mode is reserved and not implemented; see docs/attached-mode.md",
)

// AttachedDispatch is the reserved dispatch entry point for attached
// mode. It accepts the same intent.WorkloadIntent the cluster-native
// dispatcher consumes so callers can wire the seam once and let the
// mode switch decide which dispatcher runs.
//
// Today it always returns ErrAttachedNotImplemented. The ctx and intent
// arguments are accepted and ignored; no side effects are performed.
// When attached mode graduates from reserved to implemented, this
// function's body — and only this function's body — is the expected
// growth point.
func AttachedDispatch(ctx context.Context, i intent.WorkloadIntent) error {
	_ = ctx
	_ = i
	return ErrAttachedNotImplemented
}
