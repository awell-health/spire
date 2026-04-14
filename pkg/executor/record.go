package executor

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// agentResultJSON matches the structure of result.json written by apprentices/sages.
type agentResultJSON struct {
	Result     string `json:"result"`
	Branch     string `json:"branch"`
	Commit     string `json:"commit"`
	ElapsedS   int    `json:"elapsed_s"`
	// Extended fields (populated when available)
	TotalTokens  int            `json:"total_tokens,omitempty"`
	ContextIn    int            `json:"context_tokens_in,omitempty"`
	ContextOut   int            `json:"context_tokens_out,omitempty"`
	FilesChanged int            `json:"files_changed,omitempty"`
	LinesAdded   int            `json:"lines_added,omitempty"`
	LinesRemoved int            `json:"lines_removed,omitempty"`
	Turns        int            `json:"turns,omitempty"`
	CostUSD      float64        `json:"cost_usd,omitempty"`
	ToolCalls    map[string]int `json:"tool_calls,omitempty"` // tool_name → invocation count
}

// recordOpt is a functional option for recordAgentRun.
type recordOpt func(*AgentRun)

// withReviewStep annotates an agent run with per-step review metadata.
func withReviewStep(step string, round int) recordOpt {
	return func(r *AgentRun) {
		r.ReviewStep = step
		r.ReviewRound = round
	}
}

// phaseToBucket maps fine-grained phase names to high-level attribution buckets.
func phaseToBucket(phase string) string {
	switch phase {
	case "implement", "build-fix":
		return "implement"
	case "review", "review-fix":
		return "review"
	case "validate-design", "enrich-subtasks", "auto-approve", "skip", "waitForHuman":
		return "design"
	default:
		return ""
	}
}

// withParentRun sets the parent run ID on an agent run, linking it to the
// executor's own run record for parent-child observability.
func withParentRun(parentRunID string) recordOpt {
	return func(r *AgentRun) {
		r.ParentRunID = parentRunID
	}
}

// withResult overrides the auto-derived result string.
func withResult(result string) recordOpt {
	return func(r *AgentRun) {
		r.Result = result
	}
}

// withSkipReason sets the skip reason on the run record.
func withSkipReason(reason string) recordOpt {
	return func(r *AgentRun) {
		r.SkipReason = reason
	}
}

// withAttemptNumber sets the attempt number (from StepState.CompletedCount + 1).
func withAttemptNumber(n int) recordOpt {
	return func(r *AgentRun) {
		r.AttemptNumber = n
	}
}

// withFailureClass sets the failure classification on the run record.
func withFailureClass(class string) recordOpt {
	return func(r *AgentRun) {
		r.FailureClass = class
	}
}

// withRecoveryBead links this run to a recovery bead.
func withRecoveryBead(beadID string) recordOpt {
	return func(r *AgentRun) {
		r.RecoveryBeadID = beadID
	}
}

// populateTimingBucket returns the bucket label for a given duration.
func populateTimingBucket(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1m"
	case d < 5*time.Minute:
		return "1-5m"
	case d < 30*time.Minute:
		return "5-30m"
	case d < 2*time.Hour:
		return "30m-2h"
	default:
		return ">2h"
	}
}

// recordAgentRun records an agent run to the agent_runs table.
// Safe to call even when RecordAgentRun is nil (tests, legacy callers).
//
// After the agent exits, it reads the agent's result.json to capture the
// actual outcome (test_failure, no_changes, etc.) rather than just
// spawn success/failure. Also populates review rounds from executor state
// and computes git diff stats when possible.
func (e *Executor) recordAgentRun(name, beadID, epicID, model, role, phase string, started time.Time, spawnErr error, opts ...recordOpt) string {
	if e.deps.RecordAgentRun == nil {
		return ""
	}

	completed := time.Now()
	run := AgentRun{
		BeadID:          beadID,
		EpicID:          epicID,
		AgentName:       name,
		Model:           model,
		Role:            role,
		Phase:           phase,
		PhaseBucket:     phaseToBucket(phase),
		DurationSeconds: int(completed.Sub(started).Seconds()),
		StartedAt:       started.Format(time.RFC3339),
		CompletedAt:     completed.Format(time.RFC3339),
	}
	// Populate context fields from executor state.
	if e.graph != nil {
		run.FormulaName = e.graph.Name
		run.FormulaVersion = e.graph.Version
	}
	if e.state != nil {
		if e.state.FormulaSource != "" {
			run.FormulaSource = e.state.FormulaSource
		}
	}
	if run.FormulaSource == "" && e.graphState != nil && e.graphState.FormulaSource != "" {
		run.FormulaSource = e.graphState.FormulaSource
	}

	// Bead type — swallow errors (bead may be deleted or unavailable in tests).
	if e.deps.GetBead != nil && beadID != "" {
		if b, err := e.deps.GetBead(beadID); err == nil {
			run.BeadType = b.Type
		}
	}

	// Tower name — swallow errors (unavailable in test contexts).
	if e.deps.ActiveTowerConfig != nil {
		if tc, err := e.deps.ActiveTowerConfig(); err == nil && tc != nil {
			run.Tower = tc.Name
		}
	}

	// Try to read the agent's result.json for actual outcome and metrics.
	if ar := e.readAgentResult(name); ar != nil {
		run.Result = mapResultValue(ar.Result)
		run.TotalTokens = ar.TotalTokens
		run.ContextTokensIn = ar.ContextIn
		run.ContextTokensOut = ar.ContextOut
		run.FilesChanged = ar.FilesChanged
		run.LinesAdded = ar.LinesAdded
		run.LinesRemoved = ar.LinesRemoved
		run.Turns = ar.Turns
		run.CostUSD = ar.CostUSD
		// Branch and commit from agent result take priority.
		if ar.Branch != "" {
			run.Branch = ar.Branch
		}
		if ar.Commit != "" {
			run.CommitSHA = ar.Commit
		}
		// Tool call tracking from result.json. Per-tool counts now come from
		// the OTel pipeline (tool_events table in DuckDB), so read_calls and
		// edit_calls are no longer populated here. Downstream consumers
		// (MetricsToolUsage, MetricsThrashingDetection, tool_usage_stats view)
		// all use COALESCE(read_calls, 0) so NULL/zero is safe. We still
		// persist the full tool_calls_json blob for historical reference.
		if len(ar.ToolCalls) > 0 {
			if blob, err := json.Marshal(ar.ToolCalls); err == nil {
				run.ToolCallsJSON = string(blob)
			}
		}
	} else {
		// No result.json available — derive result from the process error.
		run.Result = resultFromError(spawnErr)
	}

	// Classify failure when spawnErr is set or result indicates failure.
	if run.FailureClass == "" {
		run.FailureClass = classifyFailure(spawnErr, run.Result)
	}

	// Fall back to staging branch if result didn't provide a branch.
	stagingBranch := ""
	repoPath := ""
	baseBranch := ""
	if e.state != nil {
		stagingBranch = e.state.StagingBranch
		repoPath = e.state.RepoPath
		baseBranch = e.state.BaseBranch
	} else if e.graphState != nil {
		stagingBranch = e.graphState.StagingBranch
		repoPath = e.graphState.RepoPath
		baseBranch = e.graphState.BaseBranch
	}
	if run.Branch == "" && stagingBranch != "" {
		run.Branch = stagingBranch
	}

	// Compute git diff stats as fallback when result.json didn't provide them.
	if run.FilesChanged == 0 && run.Result == "success" && repoPath != "" {
		fc, la, lr := gitDiffStats(repoPath, baseBranch, e.resolveBranch(beadID))
		run.FilesChanged = fc
		run.LinesAdded = la
		run.LinesRemoved = lr
	}

	// Auto-compute timing bucket from duration.
	run.TimingBucket = populateTimingBucket(completed.Sub(started))

	// Apply functional options (e.g., review step/round metadata).
	for _, opt := range opts {
		opt(&run)
	}

	id, err := e.deps.RecordAgentRun(run)
	if err != nil {
		e.log("warning: record agent run: %s", err)
	}
	return id
}

// readAgentResult reads the result.json written by the named agent.
// Returns nil if the file doesn't exist, the dep is not wired, or parsing fails.
func (e *Executor) readAgentResult(agentName string) *agentResultJSON {
	if e.deps.AgentResultDir == nil {
		return nil
	}
	dir := e.deps.AgentResultDir(agentName)
	if dir == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		return nil
	}
	var ar agentResultJSON
	if err := json.Unmarshal(data, &ar); err != nil {
		e.log("warning: %s/result.json exists but failed to parse: %s", agentName, err)
		return nil
	}
	return &ar
}

// mapResultValue normalizes result.json result strings to canonical values
// stored in the agent_runs table.
func mapResultValue(raw string) string {
	switch raw {
	case "success", "test_failure", "no_changes", "timeout",
		"review_rejected", "empty_diff", "error":
		return raw
	case "":
		return "success"
	default:
		return raw
	}
}

// resultFromError derives a result string from a process error when no
// result.json is available.
func resultFromError(err error) string {
	if err == nil {
		return "success"
	}
	msg := err.Error()
	if strings.Contains(msg, "signal: killed") || strings.Contains(msg, "signal: terminated") {
		return "timeout"
	}
	return "error"
}

// classifyFailure derives a failure_class from the spawn error and result string.
// Returns "" for successful runs.
func classifyFailure(spawnErr error, result string) string {
	switch result {
	case "success", "no_changes", "":
		return ""
	case "timeout":
		return "timeout"
	case "test_failure":
		return "test_fail"
	case "review_rejected":
		return "review_reject"
	}

	if spawnErr != nil {
		msg := spawnErr.Error()
		low := strings.ToLower(msg)
		switch {
		case strings.Contains(msg, "signal: killed") || strings.Contains(msg, "signal: terminated"):
			return "timeout"
		case strings.Contains(low, "merge conflict") || strings.Contains(msg, "CONFLICT"):
			return "merge_conflict"
		case strings.Contains(low, "build fail") || strings.Contains(low, "compilation"):
			return "build_fail"
		case strings.Contains(low, "permission denied") || strings.Contains(low, "401") ||
			strings.Contains(low, "403") || strings.Contains(low, "authentication") ||
			strings.Contains(low, "unauthorized"):
			return "auth_fail"
		case strings.Contains(low, "rate limit") || strings.Contains(low, "429") ||
			strings.Contains(low, "too many requests") || strings.Contains(low, "throttl"):
			return "rate_limit"
		case strings.Contains(low, "connection refused") || strings.Contains(low, "econnreset") ||
			strings.Contains(low, "dns") || strings.Contains(low, "no such host") ||
			strings.Contains(low, "network"):
			return "network_error"
		case strings.Contains(low, "out of memory") || strings.Contains(msg, "OOM") ||
			strings.Contains(low, "killed") || strings.Contains(low, "resource"):
			return "resource_limit"
		case strings.Contains(low, "rebase") || strings.Contains(low, "detached head") ||
			strings.Contains(low, "dirty worktree") || strings.Contains(low, "not a git"):
			return "git_error"
		case strings.Contains(low, "lint") || strings.Contains(low, "eslint") ||
			strings.Contains(low, "prettier") || strings.Contains(low, "format"):
			return "lint_fail"
		case strings.Contains(low, "context length") || strings.Contains(low, "max tokens") ||
			strings.Contains(low, "token limit") || strings.Contains(low, "context window"):
			return "context_limit"
		case strings.Contains(low, "spawn") || strings.Contains(low, "exec") ||
			strings.Contains(low, "not found") || strings.Contains(low, "no such file"):
			return "spawn_fail"
		}
	}

	if result == "error" || result == "empty_diff" {
		return "unknown"
	}
	return ""
}

// gitDiffStats computes files-changed, lines-added, lines-removed by running
// git diff --numstat against the base branch. Returns zeros on any failure.
// Best-effort: uses a 10-second timeout to avoid blocking the recorder if the
// repo is large or branches don't exist.
func gitDiffStats(repoPath, baseBranch, featureBranch string) (filesChanged, linesAdded, linesRemoved int) {
	if baseBranch == "" || featureBranch == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "diff", "--numstat", baseBranch+"..."+featureBranch)
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}
		filesChanged++
		if a, err := strconv.Atoi(parts[0]); err == nil {
			linesAdded += a
		}
		if r, err := strconv.Atoi(parts[1]); err == nil {
			linesRemoved += r
		}
	}
	return
}
