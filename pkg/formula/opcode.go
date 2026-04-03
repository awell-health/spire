package formula

// Step kinds — determines how the executor dispatches a step.
const (
	StepKindOp       = "op"       // executor runs an opcode directly
	StepKindDispatch = "dispatch" // executor dispatches child beads
	StepKindCall     = "call"     // executor calls a nested graph or flow
)

// Opcodes — the minimum executor action set.
const (
	OpcodeCheckDesignLinked     = "check.design-linked"
	OpcodeWizardRun             = "wizard.run"
	OpcodeBeadsMaterializePlan  = "beads.materialize_plan"
	OpcodeDispatchChildren      = "dispatch.children"
	OpcodeVerifyRun             = "verify.run"
	OpcodeGraphRun              = "graph.run"
	OpcodeGitMergeToMain        = "git.merge_to_main"
	OpcodeBeadFinish            = "bead.finish"
	OpcodeNoop                  = "noop"
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
	OpcodeNoop:                 true,
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
	return kind == StepKindOp || kind == StepKindDispatch || kind == StepKindCall
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
