package metrics

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"
)

// AgentRun represents a single agent execution record stored in the agent_runs table.
type AgentRun struct {
	ID                 string `json:"id"`
	BeadID             string `json:"bead_id"`
	EpicID             string `json:"epic_id,omitempty"`
	AgentName          string `json:"agent_name,omitempty"`
	Model              string `json:"model"`
	Role               string `json:"role"` // "wizard" or "artificer"
	ContextTokensIn    int    `json:"context_tokens_in,omitempty"`
	ContextTokensOut   int    `json:"context_tokens_out,omitempty"`
	TotalTokens        int    `json:"total_tokens,omitempty"`
	Turns              int    `json:"turns,omitempty"`
	DurationSeconds    int    `json:"duration_seconds,omitempty"`
	Result             string `json:"result"`
	ReviewRounds       int    `json:"review_rounds,omitempty"`
	ArtificerVerdict    string `json:"artificer_verdict,omitempty"`
	SpecFile           string `json:"spec_file,omitempty"`
	SpecSizeTokens     int    `json:"spec_size_tokens,omitempty"`
	FocusContextTokens int    `json:"focus_context_tokens,omitempty"`
	FilesChanged       int    `json:"files_changed,omitempty"`
	LinesAdded         int    `json:"lines_added,omitempty"`
	LinesRemoved       int    `json:"lines_removed,omitempty"`
	TestsAdded         int    `json:"tests_added,omitempty"`
	TestsPassed        bool   `json:"tests_passed,omitempty"`
	SystemPromptHash   string `json:"system_prompt_hash,omitempty"`
	GoldenRun          bool   `json:"golden_run,omitempty"`
	StartedAt          string `json:"started_at"`
	CompletedAt        string `json:"completed_at,omitempty"`
}

// GenerateID returns a random run ID in the form "run-" + 8 hex chars.
func GenerateID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		// Fallback: should never happen
		return "run-00000000"
	}
	return "run-" + hex.EncodeToString(b)
}

// Record inserts an AgentRun into the agent_runs table via bd sql.
func Record(run AgentRun) error {
	if run.ID == "" {
		run.ID = GenerateID()
	}

	cols := []string{
		"id", "bead_id", "model", "role", "result", "started_at",
	}
	vals := []string{
		esc(run.ID), esc(run.BeadID), esc(run.Model), esc(run.Role),
		esc(run.Result), esc(run.StartedAt),
	}

	if run.EpicID != "" {
		cols = append(cols, "epic_id")
		vals = append(vals, esc(run.EpicID))
	}
	if run.AgentName != "" {
		cols = append(cols, "agent_name")
		vals = append(vals, esc(run.AgentName))
	}
	if run.ContextTokensIn > 0 {
		cols = append(cols, "context_tokens_in")
		vals = append(vals, itoa(run.ContextTokensIn))
	}
	if run.ContextTokensOut > 0 {
		cols = append(cols, "context_tokens_out")
		vals = append(vals, itoa(run.ContextTokensOut))
	}
	if run.TotalTokens > 0 {
		cols = append(cols, "total_tokens")
		vals = append(vals, itoa(run.TotalTokens))
	}
	if run.Turns > 0 {
		cols = append(cols, "turns")
		vals = append(vals, itoa(run.Turns))
	}
	if run.DurationSeconds > 0 {
		cols = append(cols, "duration_seconds")
		vals = append(vals, itoa(run.DurationSeconds))
	}
	if run.ReviewRounds > 0 {
		cols = append(cols, "review_rounds")
		vals = append(vals, itoa(run.ReviewRounds))
	}
	if run.ArtificerVerdict != "" {
		cols = append(cols, "artificer_verdict")
		vals = append(vals, esc(run.ArtificerVerdict))
	}
	if run.SpecFile != "" {
		cols = append(cols, "spec_file")
		vals = append(vals, esc(run.SpecFile))
	}
	if run.SpecSizeTokens > 0 {
		cols = append(cols, "spec_size_tokens")
		vals = append(vals, itoa(run.SpecSizeTokens))
	}
	if run.FocusContextTokens > 0 {
		cols = append(cols, "focus_context_tokens")
		vals = append(vals, itoa(run.FocusContextTokens))
	}
	if run.FilesChanged > 0 {
		cols = append(cols, "files_changed")
		vals = append(vals, itoa(run.FilesChanged))
	}
	if run.LinesAdded > 0 {
		cols = append(cols, "lines_added")
		vals = append(vals, itoa(run.LinesAdded))
	}
	if run.LinesRemoved > 0 {
		cols = append(cols, "lines_removed")
		vals = append(vals, itoa(run.LinesRemoved))
	}
	if run.TestsAdded > 0 {
		cols = append(cols, "tests_added")
		vals = append(vals, itoa(run.TestsAdded))
	}
	if run.TestsPassed {
		cols = append(cols, "tests_passed")
		vals = append(vals, "TRUE")
	}
	if run.SystemPromptHash != "" {
		cols = append(cols, "system_prompt_hash")
		vals = append(vals, esc(run.SystemPromptHash))
	}
	if run.GoldenRun {
		cols = append(cols, "golden_run")
		vals = append(vals, "TRUE")
	}
	if run.CompletedAt != "" {
		cols = append(cols, "completed_at")
		vals = append(vals, esc(run.CompletedAt))
	}

	query := fmt.Sprintf("INSERT INTO agent_runs (%s) VALUES (%s)",
		strings.Join(cols, ", "),
		strings.Join(vals, ", "),
	)

	return bdSQL(query)
}

// MarkGolden sets golden_run=TRUE for the given run ID.
func MarkGolden(runID string) error {
	query := fmt.Sprintf("UPDATE agent_runs SET golden_run = TRUE WHERE id = %s", esc(runID))
	return bdSQL(query)
}

// bdSQL executes a SQL statement via bd sql.
func bdSQL(query string) error {
	cmd := exec.Command("bd", "sql", query)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("bd sql: %s\n%s", err, stderr.String())
	}
	return nil
}

// bdSQLOutput executes a SQL query via bd sql and returns stdout.
func bdSQLOutput(args ...string) (string, error) {
	cmdArgs := append([]string{"sql"}, args...)
	cmd := exec.Command("bd", cmdArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("bd sql: %s\n%s", err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

// esc escapes a string value for SQL insertion.
func esc(s string) string {
	s = strings.ReplaceAll(s, "'", "''")
	return "'" + s + "'"
}

// itoa converts an int to its string representation (no quoting).
func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}
