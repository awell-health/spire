package executor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// GraphState is the generic step-oriented state model for v3 graph execution.
// It replaces the hardcoded ReviewRounds/BuildFixRounds/Phase/Wave fields with
// a graph-generic representation that tracks step status, outputs, counters,
// workspace state, and vars.
type GraphState struct {
	BeadID     string                     `json:"bead_id"`
	AgentName  string                     `json:"agent_name"`
	Formula    string                     `json:"formula"`
	Entry      string                     `json:"entry"`
	Steps      map[string]StepState       `json:"steps"`
	Counters   map[string]int             `json:"counters"`
	Workspaces map[string]WorkspaceState  `json:"workspaces"`
	Vars       map[string]string          `json:"vars"`
	ActiveStep string                     `json:"active_step"`

	// Bookkeeping (carried over from State for compatibility)
	StartedAt     string            `json:"started_at"`
	LastActionAt  string            `json:"last_action_at"`
	StagingBranch string            `json:"staging_branch,omitempty"`
	BaseBranch    string            `json:"base_branch,omitempty"`
	RepoPath      string            `json:"repo_path,omitempty"`
	AttemptBeadID string            `json:"attempt_bead_id,omitempty"`
	StepBeadIDs   map[string]string `json:"step_bead_ids,omitempty"`
	WorktreeDir   string            `json:"worktree_dir,omitempty"`
}

// StepState tracks the status and outputs of a single graph step.
type StepState struct {
	Status      string            `json:"status"` // pending, active, completed, failed, skipped
	Outputs     map[string]string `json:"outputs,omitempty"`
	StartedAt   string            `json:"started_at,omitempty"`
	CompletedAt string            `json:"completed_at,omitempty"`
}

// WorkspaceState tracks the runtime state of a declared workspace.
type WorkspaceState struct {
	Name       string `json:"name,omitempty"`        // matches the key in formula [workspaces]
	Kind       string `json:"kind,omitempty"`         // resolved kind from WorkspaceDecl
	Dir        string `json:"dir,omitempty"`          // absolute path (worktree types only)
	Branch     string `json:"branch,omitempty"`       // resolved branch name
	BaseBranch string `json:"base_branch,omitempty"`  // resolved base branch
	StartSHA   string `json:"start_sha,omitempty"`    // session baseline SHA
	Status     string `json:"status,omitempty"`       // "pending", "active", "closed"
	Scope      string `json:"scope,omitempty"`        // "run" or "step"
	Ownership  string `json:"ownership,omitempty"`    // "owned" or "borrowed"
	Cleanup    string `json:"cleanup,omitempty"`      // "always", "terminal", "never"
}

// NewGraphState creates a fresh GraphState from a graph definition, initializing
// all steps to "pending".
func NewGraphState(graph *FormulaStepGraph, beadID, agentName string) *GraphState {
	steps := make(map[string]StepState, len(graph.Steps))
	for name := range graph.Steps {
		steps[name] = StepState{Status: "pending"}
	}

	entry := graph.Entry
	if entry == "" {
		// Fall back to the step with no needs.
		for name, step := range graph.Steps {
			if len(step.Needs) == 0 {
				entry = name
				break
			}
		}
	}

	return &GraphState{
		BeadID:      beadID,
		AgentName:   agentName,
		Formula:     graph.Name,
		Entry:       entry,
		Steps:       steps,
		Counters:    make(map[string]int),
		Workspaces:  make(map[string]WorkspaceState),
		Vars:        make(map[string]string),
		StepBeadIDs: make(map[string]string),
		StartedAt:   time.Now().UTC().Format(time.RFC3339),
	}
}

// GraphStatePath returns the path to the graph state file for the given agent.
func GraphStatePath(agentName string, configDirFn func() (string, error)) string {
	dir, _ := configDirFn()
	return filepath.Join(dir, "runtime", agentName, "graph_state.json")
}

// LoadGraphState loads the graph state from disk for the given agent.
// Returns nil, nil when no state file exists (fresh start).
func LoadGraphState(agentName string, configDirFn func() (string, error)) (*GraphState, error) {
	path := GraphStatePath(agentName, configDirFn)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var state GraphState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse graph state: %w", err)
	}
	return &state, nil
}

// Save persists the graph state to disk.
func (s *GraphState) Save(agentName string, configDirFn func() (string, error)) error {
	path := GraphStatePath(agentName, configDirFn)
	os.MkdirAll(filepath.Dir(path), 0755)
	s.LastActionAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal graph state: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}

// RemoveGraphState deletes the graph state file on terminal success.
func RemoveGraphState(agentName string, configDirFn func() (string, error)) {
	path := GraphStatePath(agentName, configDirFn)
	os.Remove(path)
}
