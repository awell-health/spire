package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// workshopState is the persistent state for a wizard workshop session.
type workshopState struct {
	EpicID       string                  `json:"epic_id"`
	Phase        string                  `json:"phase"`
	SessionID    string                  `json:"session_id,omitempty"`
	Wave         int                     `json:"wave"`
	Subtasks     map[string]subtaskState `json:"subtasks"`
	ReviewRounds int                     `json:"review_rounds"`
	StartedAt    string                  `json:"started_at"`
	LastActionAt string                  `json:"last_action_at"`
}

type subtaskState struct {
	Status string `json:"status"` // "open", "in_progress", "closed"
	Branch string `json:"branch"`
	Agent  string `json:"agent,omitempty"`
}

// cmdWorkshop is the entry point for the spire workshop command.
// Usage: spire workshop <epic-id>
func cmdWorkshop(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: spire workshop <epic-id>")
	}

	epicID := args[0]

	// Resolve beads dir
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	// Verify the bead exists and is an epic
	bead, err := storeGetBead(epicID)
	if err != nil {
		return fmt.Errorf("bead %s not found: %w", epicID, err)
	}
	if bead.Type != "epic" {
		return fmt.Errorf("bead %s is type %q, not epic", epicID, bead.Type)
	}

	// Claim the epic if not already claimed
	if bead.Status != "in_progress" {
		if err := cmdClaim([]string{epicID}); err != nil {
			return fmt.Errorf("claim %s: %w", epicID, err)
		}
	}

	// Load or create workshop state
	state, err := loadWorkshopState(epicID)
	if err != nil {
		return fmt.Errorf("load workshop state: %w", err)
	}

	if state == nil {
		// New session — detect current phase from bead labels
		phase := hasLabel(bead, "phase:")
		if phase == "" {
			phase = "plan"
		}

		now := time.Now().UTC().Format(time.RFC3339)
		state = &workshopState{
			EpicID:    epicID,
			Phase:     phase,
			Wave:      0,
			Subtasks:  make(map[string]subtaskState),
			StartedAt: now,
		}
	}

	fmt.Fprintf(os.Stderr, "[workshop] starting for %s (phase: %s)\n", epicID, state.Phase)

	backend := ResolveBackend("")
	return workshopLoop(state, backend)
}

// workshopRuntimeDir returns the directory for workshop runtime state.
func workshopRuntimeDir(epicID string) string {
	dir, _ := configDir()
	return filepath.Join(dir, "runtime", "wizard-"+epicID)
}

// workshopStatePath returns the path to the workshop state file.
func workshopStatePath(epicID string) string {
	return filepath.Join(workshopRuntimeDir(epicID), "state.json")
}

// loadWorkshopState loads an existing workshop state from disk.
// Returns (nil, nil) if no state file exists (new session).
func loadWorkshopState(epicID string) (*workshopState, error) {
	path := workshopStatePath(epicID)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no state file = new session
		}
		return nil, err
	}
	var state workshopState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

// saveWorkshopState persists workshop state to disk.
func saveWorkshopState(state *workshopState) error {
	dir := workshopRuntimeDir(state.EpicID)
	os.MkdirAll(dir, 0755)
	state.LastActionAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(workshopStatePath(state.EpicID), data, 0644)
}

// workshopLoop is the wizard's main event loop.
// Implemented in workshop_loop.go (spi-3y8.2).
