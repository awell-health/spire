package wizard

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// CmdWizardEpic is the entry point for wizard epic orchestration.
// Usage: spire wizard-epic <epic-id>
func CmdWizardEpic(args []string, deps *Deps) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: spire wizard-epic <epic-id>")
	}

	epicID := args[0]

	// Resolve beads dir
	if d := deps.ResolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	// Verify the bead exists and is an epic
	bead, err := deps.GetBead(epicID)
	if err != nil {
		return fmt.Errorf("bead %s not found: %w", epicID, err)
	}
	if bead.Type != "epic" {
		return fmt.Errorf("bead %s is type %q, not epic", epicID, bead.Type)
	}

	// Claim the epic if not already claimed
	if bead.Status != "in_progress" {
		if err := deps.CmdClaim([]string{epicID}); err != nil {
			return fmt.Errorf("claim %s: %w", epicID, err)
		}
	}

	// Load or create epic state
	state, err := LoadEpicState(epicID, deps)
	if err != nil {
		return fmt.Errorf("load epic state: %w", err)
	}

	if state == nil {
		// New session — detect current phase from bead labels
		phase := deps.HasLabel(bead, "phase:")
		if phase == "" {
			phase = "plan"
		}

		now := time.Now().UTC().Format(time.RFC3339)
		state = &EpicState{
			EpicID:    epicID,
			Phase:     phase,
			Wave:      0,
			Subtasks:  make(map[string]SubtaskState),
			StartedAt: now,
		}
	}

	fmt.Fprintf(os.Stderr, "[wizard-epic] starting for %s (phase: %s)\n", epicID, state.Phase)

	backend := deps.ResolveBackend("")
	return EpicLoop(state, backend, deps)
}

// EpicRuntimeDir returns the directory for epic runtime state.
func EpicRuntimeDir(epicID string, deps *Deps) string {
	dir, _ := deps.ConfigDir()
	return filepath.Join(dir, "runtime", "wizard-"+epicID)
}

// EpicStatePath returns the path to the epic state file.
func EpicStatePath(epicID string, deps *Deps) string {
	return filepath.Join(EpicRuntimeDir(epicID, deps), "state.json")
}

// LoadEpicState loads an existing epic state from disk.
// Returns (nil, nil) if no state file exists (new session).
func LoadEpicState(epicID string, deps *Deps) (*EpicState, error) {
	path := EpicStatePath(epicID, deps)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no state file = new session
		}
		return nil, err
	}
	var state EpicState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

// SaveEpicState persists epic state to disk.
func SaveEpicState(state *EpicState, deps *Deps) error {
	dir := EpicRuntimeDir(state.EpicID, deps)
	os.MkdirAll(dir, 0755)
	state.LastActionAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(EpicStatePath(state.EpicID, deps), data, 0644)
}
