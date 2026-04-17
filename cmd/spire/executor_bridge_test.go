package main

import (
	"reflect"
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
