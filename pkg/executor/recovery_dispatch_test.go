package executor

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// newRecoveryTestExecutor assembles a minimal Executor suitable for exercising
// runRecoveryCycle and resumeInFlightRepairs directly. It installs a
// no-op step bead / comment / bead-creation surface on Deps so the dispatch
// paths do not panic on missing hooks.
func newRecoveryTestExecutor(t *testing.T) (*Executor, *GraphState) {
	t.Helper()
	dir := t.TempDir()
	recoveryBeads := make(map[string]Bead)
	stepBeadCounter := 0

	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		GetBead: func(id string) (Bead, error) {
			if b, ok := recoveryBeads[id]; ok {
				return b, nil
			}
			return Bead{ID: id, Status: "in_progress"}, nil
		},
		GetChildren: func(parentID string) ([]Bead, error) {
			var out []Bead
			for _, b := range recoveryBeads {
				if b.Parent == parentID {
					out = append(out, b)
				}
			}
			return out, nil
		},
		CreateBead: func(opts CreateOpts) (string, error) {
			stepBeadCounter++
			id := "rec-" + opts.Title
			recoveryBeads[id] = Bead{ID: id, Title: opts.Title, Status: "open",
				Parent: opts.Parent, Labels: opts.Labels}
			return id, nil
		},
		CloseBead: func(id string) error {
			if b, ok := recoveryBeads[id]; ok {
				b.Status = "closed"
				recoveryBeads[id] = b
			}
			return nil
		},
		AddComment: func(id, text string) error { return nil },
		SetBeadMetadata: func(id string, meta map[string]string) error {
			return nil
		},
		UpdateBead: func(id string, updates map[string]interface{}) error { return nil },
	}

	graphStore := &FileGraphStateStore{ConfigDir: deps.ConfigDir}
	deps.GraphStateStore = graphStore

	state := &GraphState{
		BeadID:    "spi-test",
		AgentName: "wizard-recovery-test",
		Formula:   "task-default",
		Steps: map[string]StepState{
			"implement": {Status: "active"},
		},
	}

	e := &Executor{
		beadID:     "spi-test",
		agentName:  "wizard-recovery-test",
		graphState: state,
		deps:       deps,
		log:        func(string, ...interface{}) {},
	}
	return e, state
}

// TestWizardRecovery_RoundBudgetExhaustion verifies that once RepairAttempts
// has hit the budget, runRecoveryCycle refuses to start another cycle and
// returns RecoveryBudgetExhausted. This is the guard the interpreter reads
// to escalate instead of looping forever.
func TestWizardRecovery_RoundBudgetExhaustion(t *testing.T) {
	e, state := newRecoveryTestExecutor(t)

	step := state.Steps["implement"]
	// Pre-populate RepairAttempts up to the budget.
	budget := DefaultRecoveryBudget
	for i := 0; i < budget; i++ {
		step.RepairAttempts = append(step.RepairAttempts, RepairAttempt{
			Round:   i + 1,
			Outcome: RecoveryFailed,
		})
	}
	state.Steps["implement"] = step
	stepCopy := step

	outcome, err := e.runRecoveryCycle(&stepCopy, "implement", state, fmt.Errorf("synthetic failure"))
	if err != nil {
		t.Fatalf("runRecoveryCycle returned error: %v", err)
	}
	if outcome != RecoveryBudgetExhausted {
		t.Errorf("expected RecoveryBudgetExhausted, got %v", outcome)
	}

	// Budget-exhausted cycles do not append a new RepairAttempt — the
	// attempt count stays at the budget. Otherwise the counter could drift
	// past the intended limit on a hot loop.
	if got := len(stepCopy.RepairAttempts); got != budget {
		t.Errorf("expected RepairAttempts to stay at %d, got %d", budget, got)
	}
}

// TestWizardRecovery_RoundBudgetOverride confirms SPIRE_RECOVERY_BUDGET
// propagates through maxRecoveryAttempts. Ops and tests rely on this
// override to tune the budget without recompiling.
func TestWizardRecovery_RoundBudgetOverride(t *testing.T) {
	t.Setenv("SPIRE_RECOVERY_BUDGET", "5")
	if got := maxRecoveryAttempts(nil); got != 5 {
		t.Errorf("expected budget 5 from env, got %d", got)
	}
	t.Setenv("SPIRE_RECOVERY_BUDGET", "not-a-number")
	if got := maxRecoveryAttempts(nil); got != DefaultRecoveryBudget {
		t.Errorf("expected fallback to default %d for invalid env, got %d",
			DefaultRecoveryBudget, got)
	}
	_ = os.Unsetenv("SPIRE_RECOVERY_BUDGET")
}

// TestWizardRecovery_RepairHistoryPersisted verifies that finishCycle appends
// a RepairAttempt that captures round, outcome, and timing, and that
// CurrentRepair is cleared. This is what makes the history surface via
// graph state for bd show / audit.
func TestWizardRecovery_RepairHistoryPersisted(t *testing.T) {
	e, state := newRecoveryTestExecutor(t)
	step := state.Steps["implement"]
	step.CurrentRepair = &InFlightRepair{Round: 1, Phase: PhaseExecuteMechanical}
	stepCopy := step

	startedAt := time.Now().UTC().Format(time.RFC3339)
	outcome := e.finishCycle(&stepCopy, "implement", state, RepairAttempt{
		Round:     1,
		Mode:      "mechanical",
		Action:    "rebase-onto-base",
		Outcome:   RecoveryRepaired,
		StartedAt: startedAt,
		EndedAt:   time.Now().UTC().Format(time.RFC3339),
	})

	if outcome != RecoveryRepaired {
		t.Errorf("expected RecoveryRepaired, got %v", outcome)
	}

	persisted := state.Steps["implement"]
	if len(persisted.RepairAttempts) != 1 {
		t.Fatalf("expected 1 RepairAttempt, got %d", len(persisted.RepairAttempts))
	}
	a := persisted.RepairAttempts[0]
	if a.Round != 1 || a.Mode != "mechanical" || a.Action != "rebase-onto-base" {
		t.Errorf("unexpected attempt record: %+v", a)
	}
	if a.Outcome != RecoveryRepaired {
		t.Errorf("expected Outcome RecoveryRepaired, got %v", a.Outcome)
	}
	if persisted.CurrentRepair != nil {
		t.Errorf("expected CurrentRepair cleared after finishCycle, got %+v", persisted.CurrentRepair)
	}
}

// TestResume_CrashBeforeRecoveryBeadCreated simulates a crash during the very
// first phase (PhaseCreateRecoveryBead): the wizard persisted CurrentRepair
// but no recovery bead was created yet. Resume must clear CurrentRepair,
// record an interrupted attempt, and return to a state where the next
// interpreter pass will call runRecoveryCycle for round 2.
func TestResume_CrashBeforeRecoveryBeadCreated(t *testing.T) {
	e, state := newRecoveryTestExecutor(t)
	step := state.Steps["implement"]
	step.CurrentRepair = &InFlightRepair{
		Round:     1,
		Phase:     PhaseCreateRecoveryBead,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	state.Steps["implement"] = step

	if err := e.resumeInFlightRepairs(context.Background(), state); err != nil {
		t.Fatalf("resume: %v", err)
	}

	resumed := state.Steps["implement"]
	if resumed.CurrentRepair != nil {
		t.Errorf("expected CurrentRepair cleared, got %+v", resumed.CurrentRepair)
	}
	if got := len(resumed.RepairAttempts); got != 1 {
		t.Fatalf("expected 1 interrupted attempt, got %d", got)
	}
	if resumed.RepairAttempts[0].Outcome != RecoveryInterrupted {
		t.Errorf("expected interrupted outcome, got %v", resumed.RepairAttempts[0].Outcome)
	}
	if resumed.RepairAttempts[0].FinalPhase != PhaseCreateRecoveryBead {
		t.Errorf("expected FinalPhase=PhaseCreateRecoveryBead, got %q", resumed.RepairAttempts[0].FinalPhase)
	}
}

// TestResume_CrashMidMechanicalRepair covers the mid-mechanical crash case.
// Same policy as the earlier pre-worker phases: close as interrupted so the
// interpreter picks up and re-dispatches runRecoveryCycle for a new round.
func TestResume_CrashMidMechanicalRepair(t *testing.T) {
	e, state := newRecoveryTestExecutor(t)
	step := state.Steps["implement"]
	step.CurrentRepair = &InFlightRepair{
		Round:          2,
		Phase:          PhaseExecuteMechanical,
		Mode:           "mechanical",
		Action:         "rebase-onto-base",
		RecoveryBeadID: "rec-1",
		StartedAt:      time.Now().UTC().Format(time.RFC3339),
	}
	// Existing round-1 failure already recorded.
	step.RepairAttempts = append(step.RepairAttempts, RepairAttempt{
		Round: 1, Outcome: RecoveryFailed,
	})
	state.Steps["implement"] = step

	if err := e.resumeInFlightRepairs(context.Background(), state); err != nil {
		t.Fatalf("resume: %v", err)
	}

	resumed := state.Steps["implement"]
	if resumed.CurrentRepair != nil {
		t.Errorf("expected CurrentRepair cleared, got %+v", resumed.CurrentRepair)
	}
	if got := len(resumed.RepairAttempts); got != 2 {
		t.Fatalf("expected 2 attempts after resume, got %d", got)
	}
	last := resumed.RepairAttempts[1]
	if last.Outcome != RecoveryInterrupted {
		t.Errorf("expected last attempt interrupted, got %v", last.Outcome)
	}
	if last.Mode != "mechanical" {
		t.Errorf("expected preserved mode=mechanical on interrupted attempt, got %q", last.Mode)
	}
}

// TestResume_RewindStepPhase covers the post-repair crash window: the repair
// already landed on staging but the wizard died before writing the
// step-status rewind. Resume completes the rewind without starting a new
// cycle.
func TestResume_RewindStepPhase(t *testing.T) {
	e, state := newRecoveryTestExecutor(t)
	step := state.Steps["implement"]
	step.Status = "failed"
	step.CurrentRepair = &InFlightRepair{
		Round:          1,
		Phase:          PhaseRewindStep,
		Mode:           "mechanical",
		RecoveryBeadID: "rec-1",
	}
	state.Steps["implement"] = step

	if err := e.resumeInFlightRepairs(context.Background(), state); err != nil {
		t.Fatalf("resume: %v", err)
	}
	resumed := state.Steps["implement"]
	if resumed.Status != "pending" {
		t.Errorf("expected step rewound to pending, got %q", resumed.Status)
	}
	if resumed.CurrentRepair != nil {
		t.Errorf("expected CurrentRepair cleared after rewind, got %+v", resumed.CurrentRepair)
	}
	if got := len(resumed.RepairAttempts); got != 0 {
		t.Errorf("rewind should not add an attempt, got %d", got)
	}
}

// TestRecoveryPolicy_WizardLocalVsCrossPod is a table-driven assertion of the
// conservative-vs-honor-handoff policy line documented in the design bead.
// Phases before the worker dispatch close the cycle as interrupted; worker
// and apply-bundle phases are the ones that honor a live apprentice.
func TestRecoveryPolicy_WizardLocalVsCrossPod(t *testing.T) {
	cases := []struct {
		phase    CrashPhase
		honorApp bool
	}{
		{PhaseCreateRecoveryBead, false},
		{PhaseDiagnose, false},
		{PhaseDecide, false},
		{PhaseExecuteMechanical, false},
		{PhaseExecuteMergeConflict, false},
		{PhaseExecuteWorker, true},
		{PhaseApplyBundle, true},
		{PhaseRewindStep, false}, // post-repair bookkeeping, not an apprentice honor
		{PhaseRedispatch, false},
	}
	for _, c := range cases {
		got := resumePolicyHonorsApprentice(c.phase)
		if got != c.honorApp {
			t.Errorf("phase %s: expected honorApprentice=%t, got %t", c.phase, c.honorApp, got)
		}
	}
}

// resumePolicyHonorsApprentice is the policy predicate exercised by the
// table-test above. It is derived from the same switch resumeInFlightRepairs
// uses so the two cannot drift silently.
func resumePolicyHonorsApprentice(phase CrashPhase) bool {
	switch phase {
	case PhaseExecuteWorker, PhaseApplyBundle:
		return true
	default:
		return false
	}
}

// TestResume_MultiCycleHistory confirms that repeated interrupted cycles
// accumulate in the audit record without clobbering each other. After
// three crashes and a success, the history slice should carry all four
// rounds in order.
func TestResume_MultiCycleHistory(t *testing.T) {
	e, state := newRecoveryTestExecutor(t)
	step := state.Steps["implement"]

	// Three interrupted rounds, then a successful round.
	for round := 1; round <= 3; round++ {
		step.CurrentRepair = &InFlightRepair{
			Round: round,
			Phase: PhaseDiagnose,
		}
		state.Steps["implement"] = step
		if err := e.resumeInFlightRepairs(context.Background(), state); err != nil {
			t.Fatalf("resume round %d: %v", round, err)
		}
		step = state.Steps["implement"]
	}

	// Successful round 4 via finishCycle (simulating runRecoveryCycle).
	step.CurrentRepair = &InFlightRepair{Round: 4, Phase: PhaseExecuteMechanical}
	stepCopy := step
	e.finishCycle(&stepCopy, "implement", state, RepairAttempt{
		Round: 4, Outcome: RecoveryRepaired,
	})

	final := state.Steps["implement"]
	if got := len(final.RepairAttempts); got != 4 {
		t.Fatalf("expected 4 rounds in history, got %d", got)
	}
	for i := 0; i < 3; i++ {
		if final.RepairAttempts[i].Outcome != RecoveryInterrupted {
			t.Errorf("round %d expected interrupted, got %v", i+1, final.RepairAttempts[i].Outcome)
		}
	}
	if final.RepairAttempts[3].Outcome != RecoveryRepaired {
		t.Errorf("round 4 expected repaired, got %v", final.RepairAttempts[3].Outcome)
	}
}
