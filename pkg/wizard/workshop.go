package wizard

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// CmdWorkshop is the entry point for the spire workshop command.
// Usage: spire workshop <epic-id>
func CmdWorkshop(args []string, deps *Deps) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: spire workshop <epic-id>")
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

	// Load or create workshop state
	state, err := LoadWorkshopState(epicID, deps)
	if err != nil {
		return fmt.Errorf("load workshop state: %w", err)
	}

	if state == nil {
		// New session — detect current phase from bead labels
		phase := deps.HasLabel(bead, "phase:")
		if phase == "" {
			phase = "plan"
		}

		now := time.Now().UTC().Format(time.RFC3339)
		state = &WorkshopState{
			EpicID:    epicID,
			Phase:     phase,
			Wave:      0,
			Subtasks:  make(map[string]SubtaskState),
			StartedAt: now,
		}
	}

	fmt.Fprintf(os.Stderr, "[workshop] starting for %s (phase: %s)\n", epicID, state.Phase)

	backend := deps.ResolveBackend("")
	return WorkshopLoop(state, backend, deps)
}

// WorkshopRuntimeDir returns the directory for workshop runtime state.
func WorkshopRuntimeDir(epicID string, deps *Deps) string {
	dir, _ := deps.ConfigDir()
	return filepath.Join(dir, "runtime", "wizard-"+epicID)
}

// WorkshopStatePath returns the path to the workshop state file.
func WorkshopStatePath(epicID string, deps *Deps) string {
	return filepath.Join(WorkshopRuntimeDir(epicID, deps), "state.json")
}

// LoadWorkshopState loads an existing workshop state from disk.
// Returns (nil, nil) if no state file exists (new session).
func LoadWorkshopState(epicID string, deps *Deps) (*WorkshopState, error) {
	path := WorkshopStatePath(epicID, deps)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no state file = new session
		}
		return nil, err
	}
	var state WorkshopState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

// SaveWorkshopState persists workshop state to disk.
func SaveWorkshopState(state *WorkshopState, deps *Deps) error {
	dir := WorkshopRuntimeDir(state.EpicID, deps)
	os.MkdirAll(dir, 0755)
	state.LastActionAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(WorkshopStatePath(state.EpicID, deps), data, 0644)
}
