package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/store"
)

// fakeServiceEnv records calls to the injected service deps so each test
// can assert exactly what RecordEvent did. Construct via newFakeEnv,
// install via withServiceDeps, then drive via RecordEvent.
type fakeServiceEnv struct {
	bead    store.Bead
	formula *formula.FormulaStepGraph

	getErr     error
	resolveErr error
	casErr     error
	casRows    int64

	getCalls     int
	resolveCalls int
	casCalls     int
	lastCAS      casCall
}

type casCall struct {
	beadID, expected, next string
}

func newFakeEnv(b store.Bead, f *formula.FormulaStepGraph) *fakeServiceEnv {
	return &fakeServiceEnv{bead: b, formula: f, casRows: 1}
}

func (e *fakeServiceEnv) deps() serviceDeps {
	return serviceDeps{
		GetBead: func(id string) (store.Bead, error) {
			e.getCalls++
			if e.getErr != nil {
				return store.Bead{}, e.getErr
			}
			return e.bead, nil
		},
		ResolveFormula: func(b *store.Bead) (*formula.FormulaStepGraph, error) {
			e.resolveCalls++
			if e.resolveErr != nil {
				return nil, e.resolveErr
			}
			return e.formula, nil
		},
		UpdateStatusCAS: func(ctx context.Context, beadID, expected, next string) (int64, error) {
			e.casCalls++
			e.lastCAS = casCall{beadID: beadID, expected: expected, next: next}
			if e.casErr != nil {
				return 0, e.casErr
			}
			return e.casRows, nil
		},
	}
}

// TestRecordEvent_SuccessfulTransition verifies the happy-path call:
// the service reads the bead, applies the event, and issues a CAS
// update with the prior status as the expectation.
func TestRecordEvent_SuccessfulTransition(t *testing.T) {
	env := newFakeEnv(store.Bead{ID: "spi-abc", Status: "ready", Type: "task"}, nil)
	restore := withServiceDeps(env.deps())
	defer restore()

	if err := RecordEvent(context.Background(), "spi-abc", WizardClaimed{}); err != nil {
		t.Fatalf("RecordEvent err = %v", err)
	}
	if env.casCalls != 1 {
		t.Errorf("casCalls = %d, want 1", env.casCalls)
	}
	if env.lastCAS.expected != "ready" || env.lastCAS.next != "in_progress" || env.lastCAS.beadID != "spi-abc" {
		t.Errorf("lastCAS = %+v, want {spi-abc ready in_progress}", env.lastCAS)
	}
}

// TestRecordEvent_NoOpSilence verifies the no-op contract: when
// ApplyEvent returns currentStatus unchanged, RecordEvent issues no
// CAS update.
func TestRecordEvent_NoOpSilence(t *testing.T) {
	// Escalated → currentStatus, so no UPDATE should fire.
	env := newFakeEnv(store.Bead{ID: "spi-abc", Status: "in_progress", Type: "task"}, nil)
	restore := withServiceDeps(env.deps())
	defer restore()

	if err := RecordEvent(context.Background(), "spi-abc", Escalated{}); err != nil {
		t.Fatalf("RecordEvent err = %v", err)
	}
	if env.casCalls != 0 {
		t.Errorf("casCalls = %d, want 0 (no-op should not write)", env.casCalls)
	}
}

// TestRecordEvent_TransitionConflict verifies that a CAS write whose
// RowsAffected is zero surfaces as ErrTransitionConflict — never a
// silent retry, never a swallowed error.
func TestRecordEvent_TransitionConflict(t *testing.T) {
	env := newFakeEnv(store.Bead{ID: "spi-abc", Status: "ready", Type: "task"}, nil)
	env.casRows = 0
	restore := withServiceDeps(env.deps())
	defer restore()

	err := RecordEvent(context.Background(), "spi-abc", WizardClaimed{})
	if !errors.Is(err, ErrTransitionConflict) {
		t.Errorf("RecordEvent err = %v, want ErrTransitionConflict", err)
	}
}

// TestRecordEvent_EvaluatorError ensures evaluator-level errors (e.g.
// a formula declaring a not-yet-introduced status) bubble up rather
// than getting swallowed before the CAS write.
func TestRecordEvent_EvaluatorError(t *testing.T) {
	f := &formula.FormulaStepGraph{
		Steps: map[string]formula.StepConfig{
			"implement": {Lifecycle: &formula.LifecycleConfig{OnComplete: "awaiting_review"}},
		},
	}
	env := newFakeEnv(store.Bead{ID: "spi-abc", Status: "in_progress", Type: "task"}, f)
	restore := withServiceDeps(env.deps())
	defer restore()

	err := RecordEvent(context.Background(), "spi-abc", FormulaStepCompleted{Step: "implement"})
	if err == nil {
		t.Fatal("expected evaluator error to propagate")
	}
	if env.casCalls != 0 {
		t.Errorf("casCalls = %d, want 0 (evaluator error should short-circuit before CAS)", env.casCalls)
	}
}

// TestRecordEvent_GetBeadErrorPropagates exercises the early-exit path
// when the store cannot return the bead (e.g. ErrNotFound).
func TestRecordEvent_GetBeadErrorPropagates(t *testing.T) {
	env := newFakeEnv(store.Bead{}, nil)
	env.getErr = errors.New("not found")
	restore := withServiceDeps(env.deps())
	defer restore()

	err := RecordEvent(context.Background(), "spi-abc", Closed{})
	if err == nil {
		t.Fatal("expected get-bead error to propagate")
	}
	if env.resolveCalls != 0 || env.casCalls != 0 {
		t.Errorf("subsequent calls fired despite get-bead error: resolve=%d cas=%d", env.resolveCalls, env.casCalls)
	}
}

// TestRecordEvent_ResolveFormulaErrorPropagates ensures formula
// resolution failures also short-circuit before CAS.
func TestRecordEvent_ResolveFormulaErrorPropagates(t *testing.T) {
	env := newFakeEnv(store.Bead{ID: "spi-abc", Status: "ready", Type: "task"}, nil)
	env.resolveErr = errors.New("formula missing")
	restore := withServiceDeps(env.deps())
	defer restore()

	err := RecordEvent(context.Background(), "spi-abc", WizardClaimed{})
	if err == nil {
		t.Fatal("expected resolve-formula error to propagate")
	}
	if env.casCalls != 0 {
		t.Errorf("casCalls = %d, want 0", env.casCalls)
	}
}

// TestRecordEvent_CASErrorIsNotConflict verifies a generic CAS write
// failure (non-zero rows-affected error path) propagates as a wrapped
// error and is NOT confused with ErrTransitionConflict.
func TestRecordEvent_CASErrorIsNotConflict(t *testing.T) {
	env := newFakeEnv(store.Bead{ID: "spi-abc", Status: "ready", Type: "task"}, nil)
	env.casErr = errors.New("connection reset")
	restore := withServiceDeps(env.deps())
	defer restore()

	err := RecordEvent(context.Background(), "spi-abc", WizardClaimed{})
	if err == nil {
		t.Fatal("expected CAS error to propagate")
	}
	if errors.Is(err, ErrTransitionConflict) {
		t.Error("generic CAS failure was misclassified as ErrTransitionConflict")
	}
}

// TestRecordEvent_VerboseLogEmission asserts the audit log line is
// emitted only when verboseTransitions is on. The log line content is
// verified to include the bead ID, both statuses, and the event type
// per the design's audit-signal requirement.
func TestRecordEvent_VerboseLogEmission(t *testing.T) {
	env := newFakeEnv(store.Bead{ID: "spi-abc", Status: "ready", Type: "task"}, nil)
	restore := withServiceDeps(env.deps())
	defer restore()

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	// Off path — no log line.
	SetVerboseTransitions(false)
	if err := RecordEvent(context.Background(), "spi-abc", WizardClaimed{}); err != nil {
		t.Fatalf("RecordEvent err = %v", err)
	}
	if strings.Contains(buf.String(), "lifecycle: transition") {
		t.Errorf("verbose=false should not emit transition log; got %q", buf.String())
	}

	// On path — log line emitted with the expected fields.
	buf.Reset()
	SetVerboseTransitions(true)
	defer SetVerboseTransitions(false)
	if err := RecordEvent(context.Background(), "spi-abc", WizardClaimed{}); err != nil {
		t.Fatalf("RecordEvent err = %v", err)
	}
	out := buf.String()
	for _, want := range []string{"lifecycle: transition", "bead=spi-abc", "old=ready", "new=in_progress", "event=WizardClaimed"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q missing %q", out, want)
		}
	}
}

// TestRecordEvent_VerboseLogIncludesStep makes sure step-scoped events
// surface the step name in the audit log so operators can grep by step.
func TestRecordEvent_VerboseLogIncludesStep(t *testing.T) {
	f := &formula.FormulaStepGraph{
		Steps: map[string]formula.StepConfig{
			"review": {Lifecycle: &formula.LifecycleConfig{OnComplete: "ready"}},
		},
	}
	env := newFakeEnv(store.Bead{ID: "spi-abc", Status: "in_progress", Type: "task"}, f)
	restore := withServiceDeps(env.deps())
	defer restore()

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	SetVerboseTransitions(true)
	defer SetVerboseTransitions(false)

	if err := RecordEvent(context.Background(), "spi-abc", FormulaStepCompleted{Step: "review"}); err != nil {
		t.Fatalf("RecordEvent err = %v", err)
	}
	if !strings.Contains(buf.String(), "step=review") {
		t.Errorf("verbose log missing step name: %q", buf.String())
	}
}

// TestRecordEvent_GuardClauses pins the input-validation behavior:
// empty beadID and nil event must error before any deps are called.
func TestRecordEvent_GuardClauses(t *testing.T) {
	env := newFakeEnv(store.Bead{ID: "spi-abc", Status: "ready", Type: "task"}, nil)
	restore := withServiceDeps(env.deps())
	defer restore()

	if err := RecordEvent(context.Background(), "", WizardClaimed{}); err == nil {
		t.Error("empty beadID should error")
	}
	if err := RecordEvent(context.Background(), "spi-abc", nil); err == nil {
		t.Error("nil event should error")
	}
	if env.getCalls != 0 {
		t.Errorf("guards should short-circuit before GetBead; getCalls = %d", env.getCalls)
	}
}
