package lifecycle

// Migration mapping for Landing 2 (spi-u9iwt4) — direct status writes that
// move into pkg/lifecycle.RecordEvent. Wave 1 migration agents use this
// table to pick the right event without re-deriving from each call site.
//
//	pkg/wizard/wizard.go:926                    -> ApprenticeNoChanges{HandoffDone: false}
//	pkg/executor/action_dispatch.go:453         -> FormulaStepStarted{Step: "implement"}
//	pkg/executor/action_dispatch.go:618         -> FormulaStepStarted{Step: "implement"}
//	pkg/executor/executor_dag.go:248            -> FormulaStepStarted{Step: <review-substep>} after formula lifecycle declares OnStart="open" for the substep (treats reset as a fresh start); fall back to a new BeadReopened event if formula lifecycle authorship is deferred
//	pkg/executor/graph_interpreter.go:213       -> FormulaStepStarted{Step: stepName} (parent resumes when last hooked step is unhooked; rely on formula lifecycle OnStart="in_progress")
//	pkg/executor/graph_interpreter.go:342       -> FormulaStepFailed{Step: stepName, Err: result.Error} with formula lifecycle OnFail.Status="hooked"
//	pkg/executor/graph_interpreter.go:389       -> FormulaStepCompleted{Step: stepName, Outputs: result.Outputs} with formula lifecycle OnCompleteMatch clause emitting "hooked" when result.Hooked is captured in Outputs (or add a dedicated FormulaStepHooked event if the OnCompleteMatch shape feels strained)
//	pkg/executor/graph_interpreter.go:741       -> FormulaStepStarted{Step: "implement"} (injected task dispatch)
//	pkg/executor/graph_interpreter.go:1274      -> FormulaStepStarted{Step: <inferred>} via inferPreHookParentStatus's target; if the target is "open" without a step context, a small new event (e.g., HookCleared) keeps the call site readable

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

// ApprenticeNoChanges is emitted when an apprentice exits without
// handing off changes. HandoffDone reflects whether the apprentice
// completed its handoff: false means the apprentice produced no work
// and the bead should be returned to the pool; true means the bead
// should remain in its current status while the wizard finishes
// downstream steps.
//
// HandoffDone=false maps in_progress -> open, preserving the
// pkg/wizard/wizard.go:926 reopen-as-open semantics. Landing 3 will
// introduce needs_changes and may flip the false-branch target;
// HandoffDone=true is a deliberate no-op so callers can fire the event
// unconditionally without a branch on their side.
type ApprenticeNoChanges struct {
	HandoffDone bool
}

func (ApprenticeNoChanges) isLifecycleEvent() {}
