package executor

import (
	"fmt"
	"testing"

	"github.com/awell-health/spire/pkg/formula"
)

func TestResolveDispatch_NoLabel_FallsBackToFormula(t *testing.T) {
	deps := &Deps{
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Labels: []string{"other-label", "ref:spi-abc"}}, nil
		},
	}
	e := NewForTest("spi-test", "wizard-spi-test", nil, nil, deps)

	pc := formula.PhaseConfig{Dispatch: "wave"}
	mode, source := e.resolveDispatch(pc)

	if mode != "wave" {
		t.Errorf("mode = %q, want %q", mode, "wave")
	}
	if source != "formula" {
		t.Errorf("source = %q, want %q", source, "formula")
	}
}

func TestResolveDispatch_SingleLabel_ReturnsOverride(t *testing.T) {
	deps := &Deps{
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Labels: []string{"dispatch:sequential", "other-label"}}, nil
		},
	}
	e := NewForTest("spi-test", "wizard-spi-test", nil, nil, deps)

	pc := formula.PhaseConfig{Dispatch: "direct"}
	mode, source := e.resolveDispatch(pc)

	if mode != "sequential" {
		t.Errorf("mode = %q, want %q", mode, "sequential")
	}
	if source != "override" {
		t.Errorf("source = %q, want %q", source, "override")
	}
}

func TestResolveDispatch_MultipleLabels_UsesFirstAndWarns(t *testing.T) {
	deps := &Deps{
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Labels: []string{"dispatch:wave", "dispatch:sequential"}}, nil
		},
	}

	var logged []string
	e := NewForTest("spi-test", "wizard-spi-test", nil, nil, deps)
	e.log = func(format string, args ...interface{}) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	pc := formula.PhaseConfig{Dispatch: "direct"}
	mode, source := e.resolveDispatch(pc)

	if mode != "wave" {
		t.Errorf("mode = %q, want %q", mode, "wave")
	}
	if source != "override" {
		t.Errorf("source = %q, want %q", source, "override")
	}
	if len(logged) == 0 {
		t.Error("expected a warning log about multiple dispatch labels")
	}
}

func TestResolveDispatch_GetBeadError_FallsBackToFormula(t *testing.T) {
	deps := &Deps{
		GetBead: func(id string) (Bead, error) {
			return Bead{}, fmt.Errorf("db unavailable")
		},
	}
	e := NewForTest("spi-test", "wizard-spi-test", nil, nil, deps)

	pc := formula.PhaseConfig{Dispatch: "sequential"}
	mode, source := e.resolveDispatch(pc)

	if mode != "sequential" {
		t.Errorf("mode = %q, want %q", mode, "sequential")
	}
	if source != "formula" {
		t.Errorf("source = %q, want %q", source, "formula")
	}
}

func TestResolveDispatch_EmptyLabels_FallsBackToFormula(t *testing.T) {
	deps := &Deps{
		GetBead: func(id string) (Bead, error) {
			return Bead{ID: id, Labels: nil}, nil
		},
	}
	e := NewForTest("spi-test", "wizard-spi-test", nil, nil, deps)

	pc := formula.PhaseConfig{} // empty dispatch → GetDispatch() returns "direct"
	mode, source := e.resolveDispatch(pc)

	if mode != "direct" {
		t.Errorf("mode = %q, want %q", mode, "direct")
	}
	if source != "formula" {
		t.Errorf("source = %q, want %q", source, "formula")
	}
}
