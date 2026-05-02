package lifecycle

// Event is the sealed interface implemented by every lifecycle event type.
// The unexported isLifecycleEvent method prevents callers outside this
// package from declaring new event types — the evaluator's transition
// table is keyed on this closed set.
type Event interface {
	isLifecycleEvent()
}

// Filed is emitted when a bead is first created.
type Filed struct{}

func (Filed) isLifecycleEvent() {}

// ReadyToWork is emitted when a bead's blockers have cleared and it is
// eligible for dispatch.
type ReadyToWork struct{}

func (ReadyToWork) isLifecycleEvent() {}

// WizardClaimed is emitted when a wizard claims a bead and begins work.
type WizardClaimed struct{}

func (WizardClaimed) isLifecycleEvent() {}

// FormulaStepStarted is emitted when a formula step begins executing.
type FormulaStepStarted struct {
	Step string
}

func (FormulaStepStarted) isLifecycleEvent() {}

// FormulaStepCompleted is emitted when a formula step finishes
// successfully. Outputs carries the step's structured outputs which the
// evaluator may match against [steps.X.lifecycle.on_complete_match]
// clauses.
type FormulaStepCompleted struct {
	Step    string
	Outputs map[string]any
}

func (FormulaStepCompleted) isLifecycleEvent() {}

// FormulaStepFailed is emitted when a formula step fails.
type FormulaStepFailed struct {
	Step string
	Err  error
}

func (FormulaStepFailed) isLifecycleEvent() {}

// Escalated is emitted when a bead is escalated for human attention.
type Escalated struct{}

func (Escalated) isLifecycleEvent() {}

// Closed is emitted when a bead is closed.
type Closed struct{}

func (Closed) isLifecycleEvent() {}
