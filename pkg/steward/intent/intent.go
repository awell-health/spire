// Package intent defines the scheduler-to-reconciler seam used in
// cluster-native Spire deployments. pkg/steward writes WorkloadIntent
// values describing what work should run; the Kubernetes operator consumes
// them and reconciles the actual runtime resources (apprentice pod, PVC,
// etc.) to match.
//
// This seam MUST NOT carry LocalBindings, LocalPath, or any machine-local
// workspace state. Cluster-native scheduling writes WorkloadIntent;
// operator reconciles it.
//
// The package deliberately imports nothing from k8s.io/*, from pkg/dolt, or
// from pkg/config. Intent values must be consumable without reading
// deployment mode or touching any persistence backend, because the operator
// reads them through a generic consumer transport (a CR watch, in
// practice) rather than by importing scheduler-side packages.
package intent

import "context"

// Phase classification.
//
// A WorkloadIntent's FormulaPhase string carries one of two semantic
// levels:
//
//   - bead-level: the steward's "claim a bead, run its formula" emit.
//     The operator routes these to a wizard pod. The phase value is
//     either the literal "wizard" or the bead's type (task / bug /
//     epic / feature / chore) — both classify as bead-level.
//   - step-level: a phase emitted from inside a wizard pod for one
//     formula step. The operator routes "implement"/"fix" to an
//     apprentice pod and "review"/"arbiter" to a sage pod.
//
// The helpers below are the single source of truth for that
// classification. Both sides of the seam (steward emit, operator
// reconcile) call them so the routing rule lives in one place.
const (
	// PhaseWizard is the canonical bead-level phase string. The
	// steward stamps it (or the bead's type, see IsBeadLevelPhase)
	// onto the intent so the operator routes to a wizard pod.
	PhaseWizard = "wizard"

	// Step-level phase names — emitted from inside a wizard pod when
	// it dispatches a one-shot step worker.
	PhaseImplement = "implement"
	PhaseFix       = "fix"

	// Review-level phase names — sage / arbiter.
	PhaseReview  = "review"
	PhaseArbiter = "arbiter"
)

// IsBeadLevelPhase reports whether s is a phase value the operator
// must route to a wizard pod. The literal "wizard" plus every
// registered bead type qualifies; bead types are accepted because
// formulas resolve by bead type and using the type as the phase
// avoids inventing a parallel naming axis.
func IsBeadLevelPhase(s string) bool {
	switch s {
	case PhaseWizard, "task", "bug", "epic", "feature", "chore":
		return true
	}
	return false
}

// IsStepLevelPhase reports whether s is a step-level phase the
// operator must route to an apprentice pod.
func IsStepLevelPhase(s string) bool {
	switch s {
	case PhaseImplement, PhaseFix:
		return true
	}
	return false
}

// IsReviewLevelPhase reports whether s is a review-level phase the
// operator must route to a sage pod.
func IsReviewLevelPhase(s string) bool {
	switch s {
	case PhaseReview, PhaseArbiter:
		return true
	}
	return false
}

// RepoIdentity is the minimal repo shape a WorkloadIntent carries. It is a
// local struct — NOT pkg/config.LocalRepoBinding — so the intent remains
// free of machine-local workspace fields. The operator resolves any
// additional per-cluster state (credentials, clone paths, etc.) via its
// own identity resolver rather than trusting values the scheduler might
// have derived from local filesystem state.
type RepoIdentity struct {
	URL        string
	BaseBranch string
	Prefix     string
}

// Resources carries the CPU and memory envelope the apprentice pod should
// run under. Values follow Kubernetes quantity-string conventions
// (e.g. "500m", "1Gi") so the reconciler can pass them through to the pod
// spec without re-parsing or re-deciding shape.
type Resources struct {
	CPURequest    string
	CPULimit      string
	MemoryRequest string
	MemoryLimit   string
}

// WorkloadIntent is the dispatch-time request the scheduler writes and the
// reconciler consumes. It describes what work to run for one task,
// never how to run it locally.
//
// TaskID is the bead ID of the task (or bug/feature/chore/epic) being
// dispatched — the authoritative work identity. DispatchSeq distinguishes
// multiple dispatches of the same task (retries) and together with TaskID
// forms the canonical ownership seam for the dispatch row.
//
// Attempt-bead lifecycle is entirely wizard-owned: the wizard sees
// SPIRE_BEAD_ID=<task_id> on startup and creates (or resumes) its own
// attempt bead. No attempt ID crosses this seam.
//
// Routing identity (Role, Phase, Runtime) crosses the wizard→operator
// seam explicitly: the operator routes by Role, validates the
// (Role, Phase) pair against Allowed, and materializes the pod from
// Runtime.Image / Command / Env. See contract.go for the canonical
// enums and Validate.
//
// The struct's fields include Runtime which contains a slice and a map,
// so equality under == is no longer meaningful for the full value;
// callers that need equality checks should compare the comparable
// fields explicitly.
//
// New fields that describe machine-local workspace state (local paths,
// local bindings, local workspace roots, or anything derived from
// pkg/config.LocalBindings) must NOT be added here; a reflection-based
// test in intent_test.go enforces that.
type WorkloadIntent struct {
	TaskID       string
	DispatchSeq  int
	Reason       string
	RepoIdentity RepoIdentity
	// FormulaPhase is the legacy bead-/step-/review-level phase string
	// that pre-dated the explicit Role/Phase/Runtime contract. The
	// operator no longer routes on FormulaPhase; producers may continue
	// to set it for log/metric continuity until spi-sb9yob retires
	// the field as part of the steward producer migration.
	//
	// deprecated: routing now uses Role/Phase; spi-sb9yob will retire this.
	FormulaPhase string
	Resources    Resources
	HandoffMode  string

	// Role is the cluster role the child run materializes as. The
	// operator routes by Role; pod-builder selection picks a builder
	// per (Role, Phase). See Allowed and Validate in contract.go.
	Role Role
	// Phase is the formula phase the child run is performing. The
	// (Role, Phase) pair must appear in Allowed.
	Phase Phase
	// Runtime is the explicit image/command/env/resources the operator
	// needs to materialize the pod. Validate rejects an intent with
	// empty Runtime.Image.
	Runtime Runtime
}

// AssignmentIntent carries upstream policy decisions that come before a
// WorkloadIntent is emitted — typically "which guild should this task
// go to" and "what capabilities the guild must have". The scheduler
// materializes a WorkloadIntent once an AssignmentIntent is accepted.
type AssignmentIntent struct {
	TaskID       string
	TargetGuild  string
	Capabilities []string
}

// IntentPublisher is the scheduler-side seam. pkg/steward writes a
// WorkloadIntent via Publish once it has claimed the backing attempt bead.
// The transport is implementation-specific (a Kubernetes CR apply, in
// cluster-native mode); callers depend only on this interface.
type IntentPublisher interface {
	Publish(ctx context.Context, intent WorkloadIntent) error
}

// IntentConsumer is the reconciler-side seam. The operator reads
// WorkloadIntent values from the channel returned by Consume and
// reconciles cluster resources to match. The channel is closed when the
// context is cancelled or the underlying transport terminates.
type IntentConsumer interface {
	Consume(ctx context.Context) (<-chan WorkloadIntent, error)
}
