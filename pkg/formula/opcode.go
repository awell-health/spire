package formula

// Step kinds — determines how the executor dispatches a step.
const (
	StepKindOp       = "op"       // executor runs an opcode directly
	StepKindDispatch = "dispatch" // executor dispatches child beads
	StepKindCall     = "call"     // executor calls a nested graph or flow
	// StepKindWait declares a step that does not dispatch any work — its
	// completion is driven externally. The interpreter treats the step as
	// pending until every key declared in `produces` has a value in
	// outputs. Wait steps must declare `produces` with at least one key;
	// validation rejects empty-produce wait steps. Cleric foundation
	// (spi-h2d7yn) introduces this so the future cleric formula's
	// wait_for_gate step can be parked until the gateway sets the
	// approve / reject / takeover output.
	StepKindWait = "wait"
)

// Opcodes — the minimum executor action set.
const (
	OpcodeCheckDesignLinked    = "check.design-linked"
	OpcodeWizardRun            = "wizard.run"
	OpcodeBeadsMaterializePlan = "beads.materialize_plan"
	OpcodeDispatchChildren     = "dispatch.children"
	OpcodeVerifyRun            = "verify.run"
	OpcodeGraphRun             = "graph.run"
	OpcodeGitMergeToMain       = "git.merge_to_main"
	OpcodeBeadFinish           = "bead.finish"
	OpcodeHumanApprove         = "human.approve"
	OpcodeNoop                 = "noop"
	// Cleric runtime (spi-hhkozk) opcodes — drive the open-loop
	// recovery formula. publish: parse Claude's ProposedAction JSON,
	// write it to the recovery bead, and transition to awaiting_review.
	// execute: run the approved action via the gateway. takeover: mark
	// source bead needs-manual and close the recovery. finish: record
	// outcome for the promotion/demotion tally. reject: record a
	// rejection outcome on the recovery bead so the promotion/demotion
	// learning loop (spi-kl8x5y) sees the signal; replaces the legacy
	// noop on requeue_after_reject.
	OpcodeClericPublish  = "cleric.publish"
	OpcodeClericExecute  = "cleric.execute"
	OpcodeClericTakeover = "cleric.takeover"
	OpcodeClericFinish   = "cleric.finish"
	OpcodeClericReject   = "cleric.reject"
)

// ValidOpcodes is the set of recognized executor opcodes.
var ValidOpcodes = map[string]bool{
	OpcodeCheckDesignLinked:    true,
	OpcodeWizardRun:            true,
	OpcodeBeadsMaterializePlan: true,
	OpcodeDispatchChildren:     true,
	OpcodeVerifyRun:            true,
	OpcodeGraphRun:             true,
	OpcodeGitMergeToMain:       true,
	OpcodeBeadFinish:           true,
	OpcodeHumanApprove:         true,
	OpcodeNoop:                 true,
	OpcodeClericPublish:        true,
	OpcodeClericExecute:        true,
	OpcodeClericTakeover:       true,
	OpcodeClericFinish:         true,
	OpcodeClericReject:         true,
}

// ValidOpcode returns true if the opcode is in the recognized set.
func ValidOpcode(op string) bool {
	return ValidOpcodes[op]
}

// ValidStepKind returns true if the kind is a recognized step kind.
// Empty string is valid for backward compatibility (v2-style steps without kind).
func ValidStepKind(kind string) bool {
	if kind == "" {
		return true
	}
	return kind == StepKindOp || kind == StepKindDispatch || kind == StepKindCall || kind == StepKindWait
}

// Var types — constrains the runtime type of formula variables.
const (
	VarTypeString = "string"
	VarTypeInt    = "int"
	VarTypeBool   = "bool"
	VarTypeBeadID = "bead_id"
)

// ValidVarType returns true if the var type is recognized.
// Empty string is valid (defaults to "string").
func ValidVarType(t string) bool {
	if t == "" {
		return true
	}
	return t == VarTypeString || t == VarTypeInt || t == VarTypeBool || t == VarTypeBeadID
}

// OnError directives — controls interpreter reaction to action errors.
const (
	OnErrorPark   = "park"   // default: hook the step for human recovery
	OnErrorRecord = "record" // record error as outputs.error and continue
)

// ValidOnError returns true if the on_error directive is recognized.
// Empty string is valid (defaults to "park").
func ValidOnError(s string) bool {
	if s == "" {
		return true
	}
	return s == OnErrorPark || s == OnErrorRecord
}
