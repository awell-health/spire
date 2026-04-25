package wizardregistry

import (
	"context"
	"errors"
	"time"
)

// Mode identifies the runtime environment a wizard is executing in.
//
// The Mode determines which fields of [Wizard] are populated:
//
//   - [ModeLocal]: PID is populated; PodName and Namespace are empty.
//   - [ModeCluster]: PodName and Namespace are populated; PID is zero.
//
// Readers MUST inspect Mode before reading mode-specific fields.
type Mode string

const (
	// ModeLocal indicates a wizard running as an OS process on the
	// machine that owns the registry.
	ModeLocal Mode = "local"

	// ModeCluster indicates a wizard running as a Kubernetes pod
	// managed by the operator.
	ModeCluster Mode = "cluster"
)

// Wizard is a single wizard registration.
//
// The fields are mode-tagged: [Wizard.PID] is meaningful only when
// [Wizard.Mode] is [ModeLocal]; [Wizard.PodName] and [Wizard.Namespace]
// are meaningful only when [Wizard.Mode] is [ModeCluster]. The unused
// fields for the other mode MUST be zero-valued.
//
// [Wizard.ID] is the caller-chosen unique key used by all [Registry]
// methods. Implementations MUST treat ID as opaque — no parsing or
// suffix-stripping.
type Wizard struct {
	// ID is an opaque, caller-chosen unique key for the entry.
	// All Registry methods key on ID.
	ID string

	// Mode identifies the runtime environment. Drives which of the
	// fields below are populated.
	Mode Mode

	// PID is the OS process ID. Populated when Mode == ModeLocal,
	// zero otherwise.
	PID int

	// PodName is the Kubernetes pod name. Populated when
	// Mode == ModeCluster, empty otherwise.
	PodName string

	// Namespace is the Kubernetes namespace containing PodName.
	// Populated when Mode == ModeCluster, empty otherwise.
	Namespace string

	// BeadID is the bead this wizard is orchestrating.
	BeadID string

	// StartedAt is when the wizard started.
	StartedAt time.Time
}

// ErrNotFound is returned by [Registry.Get], [Registry.Remove], and
// [Registry.IsAlive] when no entry with the requested ID exists.
var ErrNotFound = errors.New("wizardregistry: wizard not found")

// ErrReadOnly is returned by [Registry.Upsert] and [Registry.Remove]
// when the backend does not accept writes from clients. This is the
// expected behavior for cluster-mode backends, where the operator owns
// writes via reconciliation; clients query liveness but do not mutate
// the registry.
var ErrReadOnly = errors.New("wizardregistry: backend is read-only from clients")

// Registry is the unified contract for tracking wizards across local
// and cluster modes.
//
// # Race-safety guarantee
//
// IsAlive and Sweep MUST consult the authoritative source on each
// call. Implementations MUST NOT cache liveness across calls or operate
// on a snapshot of the wizard set captured before the per-entry
// liveness check. This rule prevents the OrphanSweep race in which a
// wizard upserted between snapshot capture and predicate evaluation is
// mis-classified as dead.
//
// Concretely, an implementation that lists entries, captures the slice,
// and then evaluates liveness for each captured entry violates the
// guarantee: a wizard that was upserted after the list call but before
// the per-entry check will appear in the list with no fresh liveness
// check, or — worse — will be checked against a now-stale snapshot of
// the authoritative source. Sweep implementations MUST hold the lock
// (or equivalent serialization) across both list and per-entry liveness
// evaluation, OR re-read the authoritative source on each per-entry
// check.
//
// # Write discipline
//
// Backends fall into two write disciplines:
//
//   - Read/write backends (e.g. local file-backed) accept Upsert and
//     Remove from any client. The wizard process registers itself on
//     start and removes itself on clean exit.
//   - Read-mostly backends (e.g. cluster, where the operator owns the
//     pod-to-wizard mapping via reconciliation) MUST return [ErrReadOnly]
//     from Upsert and Remove. Clients query liveness but do not mutate.
//
// The conformance test suite skips Upsert/Remove cases when
// [ErrReadOnly] is returned, so a read-mostly backend can pass the
// suite by faithfully reporting its constraint.
//
// # Sweep semantics
//
// Sweep is a predicate, not an action. It returns the subset of
// currently-registered wizards whose underlying process or pod is no
// longer alive. The caller is responsible for follow-up bead-store work
// (closing orphan attempts, reverting parent status, etc.). Sweep MUST
// NOT call Remove on the dead entries it returns; the entries remain
// visible via List until the caller (or, in cluster mode, the operator)
// removes them.
//
// The order of the returned slice is unspecified. Callers that need a
// stable order MUST sort the result themselves.
type Registry interface {
	// List returns a snapshot of all currently-registered wizards.
	// The returned slice is safe for the caller to retain and mutate
	// — implementations MUST return a copy of any internal storage.
	List(ctx context.Context) ([]Wizard, error)

	// Get returns the wizard with the given ID. Returns [ErrNotFound]
	// when no entry exists.
	Get(ctx context.Context, id string) (Wizard, error)

	// Upsert adds or replaces the entry keyed by w.ID.
	// Read-mostly backends MUST return [ErrReadOnly].
	Upsert(ctx context.Context, w Wizard) error

	// Remove deletes the entry with the given ID.
	// Returns [ErrNotFound] when no such entry exists.
	// Read-mostly backends MUST return [ErrReadOnly] regardless of
	// whether the entry exists.
	Remove(ctx context.Context, id string) error

	// IsAlive reports whether the wizard with the given ID is alive
	// according to a fresh authoritative-source read.
	//
	// Returns (false, [ErrNotFound]) when no entry exists.
	// Returns (false, nil) when the entry exists but its underlying
	// process or pod is gone.
	// Returns (true, nil) when the entry exists and the underlying
	// process or pod is alive.
	//
	// Implementations MUST NOT cache the result across calls.
	IsAlive(ctx context.Context, id string) (bool, error)

	// Sweep returns the subset of currently-registered wizards whose
	// underlying process or pod is dead. Sweep is predicate-only —
	// it MUST NOT remove entries.
	//
	// Implementations MUST satisfy the race-safety guarantee
	// described in the [Registry] type documentation. A wizard
	// upserted concurrently with a Sweep call MUST NOT be
	// mis-classified as dead.
	//
	// The returned slice order is unspecified.
	Sweep(ctx context.Context) ([]Wizard, error)
}
