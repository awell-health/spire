package main

import (
	"fmt"
	"os"

	"github.com/awell-health/spire/pkg/executor"
	"github.com/spf13/cobra"
)

var injectCmd = &cobra.Command{
	Use:   "inject <epic-id> <task-id>",
	Short: "Inject a task into a running epic",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmdInject(args[0], args[1])
	},
}

var injectGetBeadFunc = storeGetBead
var injectCreateBeadFunc = func(opts createOpts) (string, error) { return storeCreateBead(opts) }
var injectIdentityFunc = func() (string, error) { return detectIdentity("") }
var injectLoadGraphStateFunc = func(wizardName string) (*executor.GraphState, error) {
	return executor.LoadGraphState(wizardName, configDir)
}
var injectSaveGraphStateFunc = func(wizardName string, gs *executor.GraphState) error {
	return gs.Save(wizardName, configDir)
}

func cmdInject(epicID, taskID string) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	epic, err := injectGetBeadFunc(epicID)
	if err != nil {
		return fmt.Errorf("epic %s not found: %w", epicID, err)
	}
	if epic.Type != "epic" {
		return fmt.Errorf("bead %s is type %q, not epic", epicID, epic.Type)
	}

	task, err := injectGetBeadFunc(taskID)
	if err != nil {
		return fmt.Errorf("task %s not found: %w", taskID, err)
	}
	if task.Status == "closed" {
		return fmt.Errorf("task %s is already closed", taskID)
	}
	if task.Parent != epicID {
		return fmt.Errorf("task %s is not a child of epic %s (parent=%q); reparent it first with: bd update %s --parent %s", taskID, epicID, task.Parent, taskID, epicID)
	}

	wizardName := "wizard-" + epicID

	state, err := injectLoadGraphStateFunc(wizardName)
	if err != nil {
		return fmt.Errorf("load graph state for %s: %w", wizardName, err)
	}
	if state == nil {
		return fmt.Errorf("no active wizard for %s (no graph state found)", epicID)
	}

	for _, id := range state.InjectedTasks {
		if id == taskID {
			fmt.Printf("Task %s already injected into %s (idempotent, no change).\n", taskID, epicID)
			return nil
		}
	}

	state.InjectedTasks = append(state.InjectedTasks, taskID)

	if err := injectSaveGraphStateFunc(wizardName, state); err != nil {
		return fmt.Errorf("save graph state for %s: %w", wizardName, err)
	}

	identity, _ := injectIdentityFunc()
	if identity == "" {
		identity = "human"
	}
	_, _ = injectCreateBeadFunc(createOpts{
		Title:  fmt.Sprintf("Injected %s: plan and dispatch", taskID),
		Type:   "message",
		Labels: []string{"msg", "to:" + wizardName, "from:" + identity, "ref:" + epicID},
	})

	fmt.Printf("Injected %s into %s. Wizard will process on next idle.\n", taskID, epicID)
	return nil
}
