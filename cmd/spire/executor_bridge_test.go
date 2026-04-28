package main

import (
	"reflect"
	"strings"
	"testing"
)

// TestBuildExecutorDepsWiresAllCallables verifies every func field on
// executor.Deps that graph_interpreter.go calls unconditionally is non-nil.
// Missing wiring causes nil-func panics deep in RunGraph (spi-qdmy8).
func TestBuildExecutorDepsWiresAllCallables(t *testing.T) {
	deps := buildExecutorDeps(nil)
	v := reflect.ValueOf(deps).Elem()
	required := []string{
		"CreateStepBead",
		"ActivateStepBead",
		"ReopenStepBead",
		"CloseStepBead",
		"HookStepBead",
		"UnhookStepBead",
	}
	for _, name := range required {
		f := v.FieldByName(name)
		if !f.IsValid() {
			t.Errorf("executor.Deps has no field %q", name)
			continue
		}
		if f.Kind() != reflect.Func {
			t.Errorf("field %q is not a func (got %s)", name, f.Kind())
			continue
		}
		if f.IsNil() {
			t.Errorf("field %q is nil — will segfault when called", name)
		}
	}
}

// TestBuildExecutorDepsForBead_EmptyBeadSkipsResolve is a layer-2 guard
// (spi-rpuzs6): when no bead context is available (archmage identity,
// legacy cwd-based callers) buildExecutorDepsForBead must not invoke
// the resolver and must return a nil error.
func TestBuildExecutorDepsForBead_EmptyBeadSkipsResolve(t *testing.T) {
	deps, err := buildExecutorDepsForBead("", nil)
	if err != nil {
		t.Fatalf("buildExecutorDepsForBead(\"\") err = %v, want nil", err)
	}
	if deps == nil {
		t.Fatal("deps is nil")
	}
}

// TestTerminalMerge_UnboundPropagatesError and siblings exercise the
// layer-2 guard (spi-rpuzs6): when the resolver reports an unbound
// prefix, the bridge must log and return the error instead of quietly
// substituting an empty repoPath. Callers that have an error-return
// (terminalMerge, terminalSplit, terminalDiscard, newGraphExecutor,
// computeWaves) propagate the error up; fire-and-forget escalation
// paths (wizardMessageArchmage, escalateHumanFailure) drop the action
// loudly.
func TestTerminalMerge_UnboundPropagatesError(t *testing.T) {
	withMockedWizardResolve(t)
	err := terminalMerge("spd-1jd", "feat/spd-1jd", "main", "", "", func(string, ...interface{}) {})
	if err == nil {
		t.Fatal("terminalMerge with unbound prefix = nil error, want propagated resolve error")
	}
	if !strings.Contains(err.Error(), "unbound") && !strings.Contains(err.Error(), "no local repo") {
		t.Errorf("error %q should surface the unbound-prefix reason", err)
	}
}

func TestTerminalSplit_UnboundPropagatesError(t *testing.T) {
	withMockedWizardResolve(t)
	err := terminalSplit("spd-1jd", "wizard-sage", nil, func(string, ...interface{}) {})
	if err == nil {
		t.Fatal("terminalSplit with unbound prefix = nil error, want propagated resolve error")
	}
}

func TestTerminalDiscard_UnboundPropagatesError(t *testing.T) {
	withMockedWizardResolve(t)
	err := terminalDiscard("spd-1jd", func(string, ...interface{}) {})
	if err == nil {
		t.Fatal("terminalDiscard with unbound prefix = nil error, want propagated resolve error")
	}
}

// withMockedWizardResolve swaps wizardResolveRepo for one that always
// returns "no local repo registered" — the same error wizard.ResolveRepo
// emits for an unbound prefix. Tests call t.Cleanup via the helper so
// restoration is automatic.
func withMockedWizardResolve(t *testing.T) {
	t.Helper()
	prevResolve := wizardResolveRepo
	prevSummon := wizardResolveRepoForSummon
	stub := func(beadID string) (string, string, string, error) {
		return "", "", "", errUnboundForTest
	}
	wizardResolveRepo = stub
	wizardResolveRepoForSummon = stub
	t.Cleanup(func() {
		wizardResolveRepo = prevResolve
		wizardResolveRepoForSummon = prevSummon
	})
}

var errUnboundForTest = &bridgeTestErr{msg: "no local repo registered for prefix \"spd\" (unbound)"}

type bridgeTestErr struct{ msg string }

func (e *bridgeTestErr) Error() string { return e.msg }
