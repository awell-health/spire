package recovery

// RecoveryActionKind identifies a bounded remediation action that the executor
// can execute mechanically. The formula/model layer chooses which action to
// declare in a recovery step; the executor only dispatches and executes.
type RecoveryActionKind string

const (
	ActionReset              RecoveryActionKind = "reset"
	ActionResetToStep        RecoveryActionKind = "reset-to-step"
	ActionResummon           RecoveryActionKind = "resummon"
	ActionVerifyClean        RecoveryActionKind = "verify-clean"
	ActionAnnotateResolution RecoveryActionKind = "annotate-resolution"
	ActionEscalate           RecoveryActionKind = "escalate"

	// Agentic recovery actions (v3 formula: collect_context → decide → execute → verify → learn → finish).
	ActionCollectContext RecoveryActionKind = "collect_context"
	ActionDecide         RecoveryActionKind = "decide"
	ActionLearn          RecoveryActionKind = "learn"
	ActionFinish         RecoveryActionKind = "finish"
)

// KnownActions is the bounded set of recovery actions the executor recognizes.
// Any action not in this set is rejected at dispatch time.
var KnownActions = map[RecoveryActionKind]bool{
	ActionReset:              true,
	ActionResetToStep:        true,
	ActionResummon:           true,
	ActionVerifyClean:        true,
	ActionAnnotateResolution: true,
	ActionEscalate:           true,
	ActionCollectContext:     true,
	ActionDecide:             true,
	ActionLearn:              true,
	ActionFinish:             true,
}

// RecoveryActionRequest is the structured input for a recovery action dispatch.
type RecoveryActionRequest struct {
	Kind         RecoveryActionKind `json:"kind"`
	BeadID       string             `json:"bead_id"`               // the recovery bead
	SourceBeadID string             `json:"source_bead_id"`        // the bead being recovered
	StepTarget   string             `json:"step_target,omitempty"` // for reset-to-step
	Params       map[string]string  `json:"params,omitempty"`
}

// RecoveryActionResult is the structured output from a recovery action execution.
// Fields map directly onto recovery bead metadata keys defined in metadata.go.
type RecoveryActionResult struct {
	Kind               RecoveryActionKind `json:"kind"`
	Success            bool               `json:"success"`
	Output             string             `json:"output,omitempty"`
	Error              string             `json:"error,omitempty"`
	ResolutionKind     string             `json:"resolution_kind,omitempty"`
	VerificationStatus string             `json:"verification_status,omitempty"`
	// Metadata holds additional key/value pairs to merge into the recovery bead's
	// issue metadata. Keys should use the constants from metadata.go.
	Metadata map[string]string `json:"metadata,omitempty"`
}
