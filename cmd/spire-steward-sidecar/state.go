package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// StewardState captures the sidecar's reasoning state that isn't in the bead graph.
// This is what carries across session restarts.
type StewardState struct {
	// Active directives — standing instructions applied to beads/wizards.
	Directives []Directive `json:"directives,omitempty"`

	// Tracking conditions — things the sidecar is watching for.
	Tracking []TrackingCondition `json:"tracking,omitempty"`

	// Pending actions — things started but not yet confirmed.
	Pending []PendingAction `json:"pending,omitempty"`

	// Recent decisions — rationale log for continuity.
	RecentDecisions []Decision `json:"recent_decisions,omitempty"`

	// Timestamp of last state write.
	UpdatedAt string `json:"updated_at"`
}

// Directive is a standing instruction (e.g., "wizards on epic X must reference doc Y").
type Directive struct {
	Description string   `json:"description"`
	TargetBeads []string `json:"target_beads,omitempty"`
	AppliedTo   []string `json:"applied_to,omitempty"`
	SteeredTo   []string `json:"steered_to,omitempty"`
	CreatedAt   string   `json:"created_at"`
}

// TrackingCondition is something the sidecar is watching for (e.g., "when X closes, assign Y").
type TrackingCondition struct {
	Description string `json:"description"`
	Condition   string `json:"condition"`
	Action      string `json:"action"`
	CreatedAt   string `json:"created_at"`
}

// PendingAction is an in-flight action awaiting confirmation.
type PendingAction struct {
	Description string `json:"description"`
	WaitingFor  string `json:"waiting_for"`
	CreatedAt   string `json:"created_at"`
}

// Decision records a judgment call with rationale.
type Decision struct {
	Description string `json:"description"`
	Rationale   string `json:"rationale"`
	CreatedAt   string `json:"created_at"`
}

// StewardSnapshot combines semantic state with operational metrics.
// Written after every API call; read by the runner to monitor context usage.
type StewardSnapshot struct {
	Timestamp     string        `json:"timestamp"`
	ContextPct    float64       `json:"context_pct"`
	InputTokens   int           `json:"input_tokens"`
	OutputTokens  int           `json:"output_tokens"`
	SessionRounds int           `json:"session_rounds"`
	State         *StewardState `json:"state,omitempty"`
}

// WriteState saves the steward state to disk.
func WriteState(commsDir string, state *StewardState) error {
	state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return atomicWriteJSON(filepath.Join(commsDir, "steward-state.json"), state)
}

// ReadState loads the steward state from disk.
func ReadState(commsDir string) (*StewardState, error) {
	path := filepath.Join(commsDir, "steward-state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &StewardState{}, nil
		}
		return nil, err
	}
	var state StewardState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

// WriteSnapshot saves the combined state + context metrics snapshot.
func WriteSnapshot(commsDir string, session *Session, state *StewardState, rounds int) error {
	snap := StewardSnapshot{
		Timestamp:     time.Now().UTC().Format(time.RFC3339),
		ContextPct:    session.ContextUsage(),
		InputTokens:   session.totalInputTk,
		OutputTokens:  session.totalOutputTk,
		SessionRounds: rounds,
		State:         state,
	}
	return atomicWriteJSON(filepath.Join(commsDir, "steward-snapshot.json"), snap)
}

// FormatCheckpoint generates a text summary of the state for session restoration.
func FormatCheckpoint(state *StewardState) string {
	if state == nil {
		return ""
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return ""
	}
	return string(data)
}

// atomicWriteJSON marshals and writes JSON atomically.
func atomicWriteJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
