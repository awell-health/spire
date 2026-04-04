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
	TotalTokens  int     `json:"total_tokens,omitempty"`
	ContextIn    int     `json:"context_tokens_in,omitempty"`
	ContextOut   int     `json:"context_tokens_out,omitempty"`
	FilesChanged int     `json:"files_changed,omitempty"`
	LinesAdded   int     `json:"lines_added,omitempty"`
	LinesRemoved int     `json:"lines_removed,omitempty"`
	Turns        int     `json:"turns,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
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

// recordAgentRun records an agent run to the agent_runs table.
// Safe to call even when RecordAgentRun is nil (tests, legacy callers).
//
// After the agent exits, it reads the agent's result.json to capture the
// actual outcome (test_failure, no_changes, etc.) rather than just
// spawn success/failure. Also populates review rounds from executor state
// and computes git diff stats when possible.
func (e *Executor) recordAgentRun(name, beadID, epicID, model, role, phase string, started time.Time, spawnErr error, opts ...recordOpt) {
	if e.deps.RecordAgentRun == nil {
		return
	}

	completed := time.Now()
	run := AgentRun{
		BeadID:          beadID,
		EpicID:          epicID,
		AgentName:       name,
		Model:           model,
		Role:            role,
		Phase:           phase,
		DurationSeconds: int(completed.Sub(started).Seconds()),
		StartedAt:       started.Format(time.RFC3339),
		CompletedAt:     completed.Format(time.RFC3339),
	}
	if e.state != nil {
		run.ReviewRounds = e.state.ReviewRounds
	}

	// Populate context fields from executor state.
	if e.formula != nil {
		run.FormulaName = e.formula.Name
		run.FormulaVersion = e.formula.Version
	} else if e.graph != nil {
		run.FormulaName = e.graph.Name
		run.FormulaVersion = e.graph.Version
	}
	if e.state != nil {
		run.WaveIndex = e.state.Wave
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

	// TODO: populate ParentRunID — requires callers to thread the parent run ID
	// through to recordAgentRun. Deferred to a follow-up (Tier 2, spi-md5mv).

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
	} else {
		// No result.json available — derive result from the process error.
		run.Result = resultFromError(spawnErr)
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

	// Apply functional options (e.g., review step/round metadata).
	for _, opt := range opts {
		opt(&run)
	}

	if err := e.deps.RecordAgentRun(run); err != nil {
		e.log("warning: record agent run: %s", err)
	}
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
