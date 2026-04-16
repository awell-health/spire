package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/executor"
)

type injectStubs struct {
	origGetBead        func(string) (Bead, error)
	origCreateBead     func(createOpts) (string, error)
	origIdentity       func() (string, error)
	origLoadGraphState func(string) (*executor.GraphState, error)
	origSaveGraphState func(string, *executor.GraphState) error
}

func stubInjectDeps(t *testing.T) *injectStubs {
	t.Helper()
	s := &injectStubs{
		origGetBead:        injectGetBeadFunc,
		origCreateBead:     injectCreateBeadFunc,
		origIdentity:       injectIdentityFunc,
		origLoadGraphState: injectLoadGraphStateFunc,
		origSaveGraphState: injectSaveGraphStateFunc,
	}
	t.Cleanup(func() {
		injectGetBeadFunc = s.origGetBead
		injectCreateBeadFunc = s.origCreateBead
		injectIdentityFunc = s.origIdentity
		injectLoadGraphStateFunc = s.origLoadGraphState
		injectSaveGraphStateFunc = s.origSaveGraphState
	})
	return s
}

func TestInject_HappyPath(t *testing.T) {
	_ = stubInjectDeps(t)

	injectGetBeadFunc = func(id string) (Bead, error) {
		switch id {
		case "spi-abc":
			return Bead{ID: "spi-abc", Type: "epic", Status: "in_progress"}, nil
		case "spi-abc.1":
			return Bead{ID: "spi-abc.1", Type: "task", Status: "open", Parent: "spi-abc"}, nil
		}
		return Bead{}, fmt.Errorf("not found")
	}

	savedState := (*executor.GraphState)(nil)
	injectLoadGraphStateFunc = func(name string) (*executor.GraphState, error) {
		return &executor.GraphState{
			BeadID:    "spi-abc",
			AgentName: "wizard-spi-abc",
			Steps:     map[string]executor.StepState{},
		}, nil
	}
	injectSaveGraphStateFunc = func(name string, gs *executor.GraphState) error {
		savedState = gs
		return nil
	}

	var createdMsg createOpts
	injectCreateBeadFunc = func(opts createOpts) (string, error) {
		createdMsg = opts
		return "msg-123", nil
	}
	injectIdentityFunc = func() (string, error) { return "archmage", nil }

	if err := cmdInject("spi-abc", "spi-abc.1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if savedState == nil {
		t.Fatal("graph state was not saved")
	}
	if len(savedState.InjectedTasks) != 1 || savedState.InjectedTasks[0] != "spi-abc.1" {
		t.Errorf("expected InjectedTasks=[spi-abc.1], got %v", savedState.InjectedTasks)
	}

	if createdMsg.Title != "Injected spi-abc.1: plan and dispatch" {
		t.Errorf("unexpected message title: %s", createdMsg.Title)
	}
	hasTo := false
	for _, l := range createdMsg.Labels {
		if l == "to:wizard-spi-abc" {
			hasTo = true
		}
	}
	if !hasTo {
		t.Errorf("expected to:wizard-spi-abc label, got %v", createdMsg.Labels)
	}
}

func TestInject_EpicNotFound(t *testing.T) {
	_ = stubInjectDeps(t)
	injectGetBeadFunc = func(id string) (Bead, error) {
		return Bead{}, fmt.Errorf("not found")
	}

	err := cmdInject("spi-nope", "spi-nope.1")
	if err == nil || !strings.Contains(err.Error(), "epic spi-nope not found") {
		t.Fatalf("expected epic-not-found error, got: %v", err)
	}
}

func TestInject_NotAnEpic(t *testing.T) {
	_ = stubInjectDeps(t)
	injectGetBeadFunc = func(id string) (Bead, error) {
		return Bead{ID: id, Type: "task", Status: "open"}, nil
	}

	err := cmdInject("spi-task", "spi-task.1")
	if err == nil || !strings.Contains(err.Error(), "not epic") {
		t.Fatalf("expected not-epic error, got: %v", err)
	}
}

func TestInject_TaskNotChildOfEpic(t *testing.T) {
	_ = stubInjectDeps(t)
	injectGetBeadFunc = func(id string) (Bead, error) {
		switch id {
		case "spi-abc":
			return Bead{ID: "spi-abc", Type: "epic", Status: "in_progress"}, nil
		case "spi-xyz.1":
			return Bead{ID: "spi-xyz.1", Type: "task", Status: "open", Parent: "spi-xyz"}, nil
		}
		return Bead{}, fmt.Errorf("not found")
	}

	err := cmdInject("spi-abc", "spi-xyz.1")
	if err == nil || !strings.Contains(err.Error(), "not a child") {
		t.Fatalf("expected not-a-child error, got: %v", err)
	}
}

func TestInject_NoActiveWizard(t *testing.T) {
	_ = stubInjectDeps(t)
	injectGetBeadFunc = func(id string) (Bead, error) {
		switch id {
		case "spi-abc":
			return Bead{ID: "spi-abc", Type: "epic", Status: "in_progress"}, nil
		case "spi-abc.1":
			return Bead{ID: "spi-abc.1", Type: "task", Status: "open", Parent: "spi-abc"}, nil
		}
		return Bead{}, fmt.Errorf("not found")
	}
	injectLoadGraphStateFunc = func(name string) (*executor.GraphState, error) {
		return nil, nil
	}

	err := cmdInject("spi-abc", "spi-abc.1")
	if err == nil || !strings.Contains(err.Error(), "no active wizard") {
		t.Fatalf("expected no-active-wizard error, got: %v", err)
	}
}

func TestInject_DuplicateIsIdempotent(t *testing.T) {
	_ = stubInjectDeps(t)
	injectGetBeadFunc = func(id string) (Bead, error) {
		switch id {
		case "spi-abc":
			return Bead{ID: "spi-abc", Type: "epic", Status: "in_progress"}, nil
		case "spi-abc.1":
			return Bead{ID: "spi-abc.1", Type: "task", Status: "open", Parent: "spi-abc"}, nil
		}
		return Bead{}, fmt.Errorf("not found")
	}
	injectLoadGraphStateFunc = func(name string) (*executor.GraphState, error) {
		return &executor.GraphState{
			BeadID:        "spi-abc",
			AgentName:     "wizard-spi-abc",
			Steps:         map[string]executor.StepState{},
			InjectedTasks: []string{"spi-abc.1"},
		}, nil
	}

	saveCalled := false
	injectSaveGraphStateFunc = func(name string, gs *executor.GraphState) error {
		saveCalled = true
		return nil
	}

	if err := cmdInject("spi-abc", "spi-abc.1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if saveCalled {
		t.Error("expected save NOT to be called for duplicate injection")
	}
}

func TestInject_TaskClosed(t *testing.T) {
	_ = stubInjectDeps(t)
	injectGetBeadFunc = func(id string) (Bead, error) {
		switch id {
		case "spi-abc":
			return Bead{ID: "spi-abc", Type: "epic", Status: "in_progress"}, nil
		case "spi-abc.1":
			return Bead{ID: "spi-abc.1", Type: "task", Status: "closed", Parent: "spi-abc"}, nil
		}
		return Bead{}, fmt.Errorf("not found")
	}

	err := cmdInject("spi-abc", "spi-abc.1")
	if err == nil || !strings.Contains(err.Error(), "already closed") {
		t.Fatalf("expected task-closed error, got: %v", err)
	}
}
