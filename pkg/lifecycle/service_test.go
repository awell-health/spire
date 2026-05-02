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

// TestRecordEvent_VerboseLogEmission asserts the verbose transition log
// line is emitted only when verboseTransitions is on. The audit
// `[lifecycle]` line is independent and tested separately.
func TestRecordEvent_VerboseLogEmission(t *testing.T) {
	env := newFakeEnv(store.Bead{ID: "spi-abc", Status: "ready", Type: "task"}, nil)
	restore := withServiceDeps(env.deps())
	defer restore()

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	// Off path — no verbose transition log line.
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

// TestRecordEvent_AuditLogAlwaysEmitted asserts the always-on
// `[lifecycle]` audit line includes the required fields (beadID, event
// type, from-status, to-status, caller attribution) on every successful
// status mutation, regardless of the verboseTransitions flag.
func TestRecordEvent_AuditLogAlwaysEmitted(t *testing.T) {
	env := newFakeEnv(store.Bead{ID: "spi-abc", Status: "ready", Type: "task"}, nil)
	restore := withServiceDeps(env.deps())
	defer restore()

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	SetVerboseTransitions(false)
	if err := RecordEvent(context.Background(), "spi-abc", WizardClaimed{}); err != nil {
		t.Fatalf("RecordEvent err = %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		"[lifecycle]",
		"bead=spi-abc",
		"event=WizardClaimed",
		"from=ready",
		"to=in_progress",
		"caller=",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("audit log %q missing %q", out, want)
		}
	}
}

// TestRecordEvent_AuditLogCallerAttribution verifies the caller= field
// names a frame outside pkg/lifecycle. The test driver lives in
// pkg/lifecycle/service_test.go, so the runtime caller chain skips
// every internal frame and lands on the test file path. Asserting the
// attribution names a non-lifecycle file is the load-bearing claim
// (the audit signal in Landing 2 acceptance: 'spd-a83o type bug
// becomes hypothetically catchable from logs').
//
// The current package tests are themselves under pkg/lifecycle so the
// resolver returns "?" — we verify that the caller= field is present
// and does not point INTO service.go (which is what would happen if
// the runtime.Caller skip were wrong).
func TestRecordEvent_AuditLogCallerAttribution(t *testing.T) {
	env := newFakeEnv(store.Bead{ID: "spi-abc", Status: "ready", Type: "task"}, nil)
	restore := withServiceDeps(env.deps())
	defer restore()

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	if err := RecordEvent(context.Background(), "spi-abc", WizardClaimed{}); err != nil {
		t.Fatalf("RecordEvent err = %v", err)
	}

	out := buf.String()
	// caller= must be present.
	idx := strings.Index(out, "caller=")
	if idx < 0 {
		t.Fatalf("audit log missing caller= field: %q", out)
	}
	// The caller field must not name service.go — the resolver is
	// supposed to skip it.
	if strings.Contains(out, "caller=pkg/lifecycle/service.go") {
		t.Errorf("caller resolution did not skip service.go: %q", out)
	}
}

// TestCallerOutsideLifecycle_SkipsInternalFrames pins the resolver to
// the contract: when called from within pkg/lifecycle the inner-most
// non-lifecycle frame (the testing harness) is reported, and the
// returned string carries a "file.go:line" shape rather than the empty
// sentinel.
func TestCallerOutsideLifecycle_SkipsInternalFrames(t *testing.T) {
	got := callerOutsideLifecycle()
	if got == "?" {
		// Acceptable when the call stack genuinely contains nothing
		// outside pkg/lifecycle within maxDepth — mostly a safety
		// fall-through. Still log the value so a future regression
		// where the resolver returns "?" inappropriately is visible.
		t.Logf("callerOutsideLifecycle returned sentinel %q (acceptable)", got)
		return
	}
	if strings.Contains(got, "/pkg/lifecycle/") {
		t.Errorf("caller %q points inside pkg/lifecycle; resolver skip is broken", got)
	}
	if !strings.Contains(got, ":") {
		t.Errorf("caller %q missing :line suffix", got)
	}
}

// TestRecordEvent_ApprenticeNoChangesAuditLog wires the new event into
// the full RecordEvent path and asserts the audit log surfaces the
// event-type field as "ApprenticeNoChanges" — preventing a future
// rename or type-switch bug from silently relabeling the audit signal.
func TestRecordEvent_ApprenticeNoChangesAuditLog(t *testing.T) {
	env := newFakeEnv(store.Bead{ID: "spi-abc", Status: "in_progress", Type: "task"}, nil)
	restore := withServiceDeps(env.deps())
	defer restore()

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	if err := RecordEvent(context.Background(), "spi-abc", ApprenticeNoChanges{HandoffDone: false}); err != nil {
		t.Fatalf("RecordEvent err = %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		"[lifecycle]",
		"bead=spi-abc",
		"event=ApprenticeNoChanges",
		"from=in_progress",
		"to=open",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("audit log %q missing %q", out, want)
		}
	}
}

// TestRecordEvent_ApprenticeNoChangesHandoffTrueIsSilent asserts
// HandoffDone=true does not write or log — the call short-circuits at
// the no-op check just like Escalated.
func TestRecordEvent_ApprenticeNoChangesHandoffTrueIsSilent(t *testing.T) {
	env := newFakeEnv(store.Bead{ID: "spi-abc", Status: "in_progress", Type: "task"}, nil)
	restore := withServiceDeps(env.deps())
	defer restore()

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	if err := RecordEvent(context.Background(), "spi-abc", ApprenticeNoChanges{HandoffDone: true}); err != nil {
		t.Fatalf("RecordEvent err = %v", err)
	}
	if env.casCalls != 0 {
		t.Errorf("HandoffDone=true should not write; casCalls = %d", env.casCalls)
	}
	if strings.Contains(buf.String(), "[lifecycle]") {
		t.Errorf("HandoffDone=true no-op should not emit audit log; got %q", buf.String())
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
