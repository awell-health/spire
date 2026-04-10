package metrics

import (
	"bytes"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Formula source constants identify where a formula was loaded from.
const (
	FormulaSourceEmbedded = "embedded"
	FormulaSourceRepo     = "repo"
	FormulaSourceTower    = "tower"
)

// AgentRun represents a single agent execution record stored in the agent_runs table.
type AgentRun struct {
	ID                 string `json:"id"`
	BeadID             string `json:"bead_id"`
	EpicID             string `json:"epic_id,omitempty"`
	AgentName          string `json:"agent_name,omitempty"`
	Model              string `json:"model"`
	Role               string `json:"role"`  // "wizard" or "worker"
	Phase              string `json:"phase,omitempty"`        // "implement", "review", "build-fix", "review-fix"
	PhaseBucket        string `json:"phase_bucket,omitempty"` // "design", "implement", "review"
	FormulaName        string `json:"formula_name,omitempty"`
	FormulaVersion     int    `json:"formula_version,omitempty"`
	FormulaSource      string `json:"formula_source,omitempty"` // "embedded", "repo", or "tower"
	Branch             string `json:"branch,omitempty"`
	CommitSHA          string `json:"commit_sha,omitempty"`
	BeadType           string `json:"bead_type,omitempty"`
	Tower              string `json:"tower,omitempty"`
	ParentRunID        string `json:"parent_run_id,omitempty"`
	WaveIndex          int    `json:"wave_index,omitempty"`
	ContextTokensIn    int    `json:"context_tokens_in,omitempty"`
	ContextTokensOut   int    `json:"context_tokens_out,omitempty"`
	TotalTokens        int    `json:"total_tokens,omitempty"`
	Turns              int     `json:"turns,omitempty"`
	CostUSD            float64 `json:"cost_usd,omitempty"`
	DurationSeconds    int     `json:"duration_seconds,omitempty"`
	StartupSeconds     int    `json:"startup_seconds,omitempty"`
	WorkingSeconds     int    `json:"working_seconds,omitempty"`
	QueueSeconds       int    `json:"queue_seconds,omitempty"`    // bead filed → wizard assigned
	ReviewSeconds      int    `json:"review_seconds,omitempty"`   // branch pushed → review verdict
	Result             string `json:"result"`
	ReviewRounds       int    `json:"review_rounds,omitempty"`
	ReviewVerdict       string `json:"artificer_verdict,omitempty" db:"artificer_verdict"` // legacy column name
	ReviewStep         string `json:"review_step,omitempty"`                               // "sage-review", "fix", "arbiter"
	ReviewRound        int    `json:"review_round,omitempty"`                              // 1-indexed round within the review cycle
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
	TimingBucket       string `json:"timing_bucket,omitempty"`
	SkipReason         string `json:"skip_reason,omitempty"`
	FailureClass       string `json:"failure_class,omitempty"`      // timeout, build_fail, test_fail, review_reject, merge_conflict, escalation, unknown
	AttemptNumber      int    `json:"attempt_number,omitempty"`     // which attempt (from StepState.CompletedCount)
	RecoveryBeadID     string `json:"recovery_bead_id,omitempty"`   // link to recovery bead if this run is a recovery
	ReadCalls          int    `json:"read_calls,omitempty"`         // count of Read tool invocations
	EditCalls          int    `json:"edit_calls,omitempty"`         // count of Edit + Write tool invocations
	ToolCallsJSON      string `json:"tool_calls_json,omitempty"`    // full {"Read": 12, "Edit": 3, ...} blob
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
// Returns the run ID (generated if not set) and any error from the SQL insert.
func Record(run AgentRun) (string, error) {
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

	if run.Phase != "" {
		cols = append(cols, "phase")
		vals = append(vals, esc(run.Phase))
	}
	if run.PhaseBucket != "" {
		cols = append(cols, "phase_bucket")
		vals = append(vals, esc(run.PhaseBucket))
	}
	if run.FormulaName != "" {
		cols = append(cols, "formula_name")
		vals = append(vals, esc(run.FormulaName))
	}
	if run.FormulaVersion > 0 {
		cols = append(cols, "formula_version")
		vals = append(vals, itoa(run.FormulaVersion))
	}
	if run.FormulaSource != "" {
		cols = append(cols, "formula_source")
		vals = append(vals, esc(run.FormulaSource))
	}
	if run.Branch != "" {
		cols = append(cols, "branch")
		vals = append(vals, esc(run.Branch))
	}
	if run.CommitSHA != "" {
		cols = append(cols, "commit_sha")
		vals = append(vals, esc(run.CommitSHA))
	}
	if run.BeadType != "" {
		cols = append(cols, "bead_type")
		vals = append(vals, esc(run.BeadType))
	}
	if run.Tower != "" {
		cols = append(cols, "tower")
		vals = append(vals, esc(run.Tower))
	}
	if run.ParentRunID != "" {
		cols = append(cols, "parent_run_id")
		vals = append(vals, esc(run.ParentRunID))
	}
	if run.WaveIndex > 0 {
		cols = append(cols, "wave_index")
		vals = append(vals, itoa(run.WaveIndex))
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
	if run.CostUSD > 0 {
		cols = append(cols, "cost_usd")
		vals = append(vals, ftoa(run.CostUSD))
	}
	if run.DurationSeconds > 0 {
		cols = append(cols, "duration_seconds")
		vals = append(vals, itoa(run.DurationSeconds))
	}
	if run.StartupSeconds > 0 {
		cols = append(cols, "startup_seconds")
		vals = append(vals, itoa(run.StartupSeconds))
	}
	if run.WorkingSeconds > 0 {
		cols = append(cols, "working_seconds")
		vals = append(vals, itoa(run.WorkingSeconds))
	}
	if run.QueueSeconds > 0 {
		cols = append(cols, "queue_seconds")
		vals = append(vals, itoa(run.QueueSeconds))
	}
	if run.ReviewSeconds > 0 {
		cols = append(cols, "review_seconds")
		vals = append(vals, itoa(run.ReviewSeconds))
	}
	if run.ReviewRounds > 0 {
		cols = append(cols, "review_rounds")
		vals = append(vals, itoa(run.ReviewRounds))
	}
	if run.ReviewVerdict != "" {
		cols = append(cols, "artificer_verdict")
		vals = append(vals, esc(run.ReviewVerdict))
	}
	if run.ReviewStep != "" {
		cols = append(cols, "review_step")
		vals = append(vals, esc(run.ReviewStep))
	}
	if run.ReviewRound > 0 {
		cols = append(cols, "review_round")
		vals = append(vals, itoa(run.ReviewRound))
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
	if run.TimingBucket != "" {
		cols = append(cols, "timing_bucket")
		vals = append(vals, esc(run.TimingBucket))
	}
	if run.SkipReason != "" {
		cols = append(cols, "skip_reason")
		vals = append(vals, esc(run.SkipReason))
	}
	if run.FailureClass != "" {
		cols = append(cols, "failure_class")
		vals = append(vals, esc(run.FailureClass))
	}
	if run.AttemptNumber > 0 {
		cols = append(cols, "attempt_number")
		vals = append(vals, itoa(run.AttemptNumber))
	}
	if run.RecoveryBeadID != "" {
		cols = append(cols, "recovery_bead_id")
		vals = append(vals, esc(run.RecoveryBeadID))
	}
	if run.ReadCalls > 0 {
		cols = append(cols, "read_calls")
		vals = append(vals, itoa(run.ReadCalls))
	}
	if run.EditCalls > 0 {
		cols = append(cols, "edit_calls")
		vals = append(vals, itoa(run.EditCalls))
	}
	if run.ToolCallsJSON != "" {
		cols = append(cols, "tool_calls_json")
		vals = append(vals, esc(run.ToolCallsJSON))
	}
	if run.CompletedAt != "" {
		cols = append(cols, "completed_at")
		vals = append(vals, esc(run.CompletedAt))
	}

	query := fmt.Sprintf("INSERT INTO agent_runs (%s) VALUES (%s)",
		strings.Join(cols, ", "),
		strings.Join(vals, ", "),
	)

	return run.ID, bdSQL(query)
}

// MarkGolden sets golden_run=TRUE for the given run ID.
func MarkGolden(runID string) error {
	query := fmt.Sprintf("UPDATE agent_runs SET golden_run = TRUE WHERE id = %s", esc(runID))
	return bdSQL(query)
}

// ReviewRoundMetrics holds per-round durations and verdict info.
type ReviewRoundMetrics struct {
	Round         int           `json:"round"`
	SageDuration  time.Duration `json:"sage_duration"`
	FixDuration   time.Duration `json:"fix_duration"`
	SageVerdict   string        `json:"sage_verdict,omitempty"`   // verdict after sage review this round
}

// ReviewCycleMetrics aggregates review efficiency data for a bead.
type ReviewCycleMetrics struct {
	BeadID       string               `json:"bead_id"`
	TotalRounds  int                  `json:"total_rounds"`
	// TotalDuration spans from the first sage-review start to the last review step
	// (sage-review, fix, or arbiter) end. This intentionally covers only the review
	// cycle itself — the terminal merge/discard step is not a review step and is
	// excluded. Use the bead's overall timestamps for end-to-end duration.
	TotalDuration time.Duration       `json:"total_duration"`
	Rounds       []ReviewRoundMetrics `json:"rounds"`
	HadArbiter   bool                 `json:"had_arbiter"`
	ArbiterDuration time.Duration     `json:"arbiter_duration,omitempty"`
	ParseErrors  int                  `json:"parse_errors,omitempty"` // count of rows with malformed round/timestamp data
}

// GetReviewCycleMetrics queries agent_runs for per-step review data and returns
// structured metrics for the given bead. Returns nil with no error if no review
// steps exist.
func GetReviewCycleMetrics(beadID string) (*ReviewCycleMetrics, error) {
	query := fmt.Sprintf(
		`SELECT review_round, review_step, started_at, completed_at, result `+
			`FROM agent_runs WHERE bead_id = %s AND review_step IS NOT NULL `+
			`ORDER BY review_round, started_at`,
		esc(beadID),
	)

	out, err := bdSQLOutput(sql2csv(query)...)
	if err != nil {
		return nil, fmt.Errorf("query review metrics: %w", err)
	}
	if out == "" {
		return nil, nil
	}

	return parseReviewCycleMetrics(beadID, out)
}

// parseReviewCycleMetrics parses CSV output from the review metrics query into
// structured metrics. Exported for testing. Returns nil with no error if the CSV
// contains no data rows. Rows with malformed round numbers or timestamps are
// counted in ParseErrors and skipped.
func parseReviewCycleMetrics(beadID string, csvData string) (*ReviewCycleMetrics, error) {
	r := csv.NewReader(strings.NewReader(csvData))
	records, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parse review metrics CSV: %w", err)
	}

	// Skip header row if present.
	if len(records) > 0 && records[0][0] == "review_round" {
		records = records[1:]
	}
	if len(records) == 0 {
		return nil, nil
	}

	m := &ReviewCycleMetrics{BeadID: beadID}
	roundMap := make(map[int]*ReviewRoundMetrics)

	var earliest, latest time.Time

	for _, row := range records {
		if len(row) < 5 {
			m.ParseErrors++
			continue
		}
		round, err := strconv.Atoi(row[0])
		if err != nil {
			m.ParseErrors++
			continue
		}
		step := row[1]
		startedAt, err := time.Parse(time.RFC3339, row[2])
		if err != nil {
			m.ParseErrors++
			continue
		}
		completedAt, err := time.Parse(time.RFC3339, row[3])
		if err != nil {
			m.ParseErrors++
			continue
		}
		result := row[4]
		dur := completedAt.Sub(startedAt)

		// Track overall time span.
		if earliest.IsZero() || startedAt.Before(earliest) {
			earliest = startedAt
		}
		if completedAt.After(latest) {
			latest = completedAt
		}

		switch step {
		case "sage-review":
			rm, ok := roundMap[round]
			if !ok {
				rm = &ReviewRoundMetrics{Round: round}
				roundMap[round] = rm
			}
			rm.SageDuration = dur
			rm.SageVerdict = result
		case "fix":
			rm, ok := roundMap[round]
			if !ok {
				rm = &ReviewRoundMetrics{Round: round}
				roundMap[round] = rm
			}
			rm.FixDuration = dur
		case "arbiter":
			m.HadArbiter = true
			m.ArbiterDuration = dur
		}
	}

	// Flatten round map to sorted slice.
	maxRound := 0
	for rnd := range roundMap {
		if rnd > maxRound {
			maxRound = rnd
		}
	}
	for i := 1; i <= maxRound; i++ {
		if rm, ok := roundMap[i]; ok {
			m.Rounds = append(m.Rounds, *rm)
		}
	}

	m.TotalRounds = len(m.Rounds)
	if !earliest.IsZero() && !latest.IsZero() {
		m.TotalDuration = latest.Sub(earliest)
	}

	return m, nil
}

// sql2csv returns the args for bdSQLOutput to produce CSV output.
func sql2csv(query string) []string {
	return []string{"-r", "csv", query}
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

// ftoa converts a float64 to its string representation (no quoting).
func ftoa(f float64) string {
	return fmt.Sprintf("%g", f)
}
