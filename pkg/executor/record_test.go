package executor

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWithReviewStep(t *testing.T) {
	tests := []struct {
		name      string
		step      string
		round     int
		wantStep  string
		wantRound int
	}{
		{"sage-review round 1", "sage-review", 1, "sage-review", 1},
		{"fix round 2", "fix", 2, "fix", 2},
		{"arbiter round 3", "arbiter", 3, "arbiter", 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var run AgentRun
			opt := withReviewStep(tt.step, tt.round)
			opt(&run)
			if run.ReviewStep != tt.wantStep {
				t.Errorf("ReviewStep = %q, want %q", run.ReviewStep, tt.wantStep)
			}
			if run.ReviewRound != tt.wantRound {
				t.Errorf("ReviewRound = %d, want %d", run.ReviewRound, tt.wantRound)
			}
		})
	}
}

func TestMapResultValue(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"success", "success"},
		{"test_failure", "test_failure"},
		{"no_changes", "no_changes"},
		{"timeout", "timeout"},
		{"review_rejected", "review_rejected"},
		{"empty_diff", "empty_diff"},
		{"error", "error"},
		{"", "success"},                              // empty → success
		{"some_unknown_value", "some_unknown_value"},  // passthrough
	}
	for _, tt := range tests {
		t.Run("input_"+tt.input, func(t *testing.T) {
			got := mapResultValue(tt.input)
			if got != tt.want {
				t.Errorf("mapResultValue(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestResultFromError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil error", nil, "success"},
		{"killed signal", errors.New("signal: killed"), "timeout"},
		{"terminated signal", errors.New("signal: terminated"), "timeout"},
		{"generic error", errors.New("exit status 1"), "error"},
		{"compound killed", errors.New("process exited: signal: killed (core dumped)"), "timeout"},
		{"unrelated error", errors.New("permission denied"), "error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resultFromError(tt.err)
			if got != tt.want {
				t.Errorf("resultFromError(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

func TestReadAgentResult(t *testing.T) {
	t.Run("no AgentResultDir dep", func(t *testing.T) {
		e := NewForTest("spi-test", "wizard-test", nil, &Deps{})
		got := e.readAgentResult("agent-foo")
		if got != nil {
			t.Errorf("expected nil when AgentResultDir is nil, got %+v", got)
		}
	})

	t.Run("dir returns empty", func(t *testing.T) {
		deps := &Deps{
			AgentResultDir: func(name string) string { return "" },
		}
		e := NewForTest("spi-test", "wizard-test", nil, deps)
		got := e.readAgentResult("agent-foo")
		if got != nil {
			t.Errorf("expected nil when dir is empty, got %+v", got)
		}
	})

	t.Run("file does not exist", func(t *testing.T) {
		dir := t.TempDir()
		deps := &Deps{
			AgentResultDir: func(name string) string { return dir },
		}
		e := NewForTest("spi-test", "wizard-test", nil, deps)
		got := e.readAgentResult("agent-foo")
		if got != nil {
			t.Errorf("expected nil when result.json missing, got %+v", got)
		}
	})

	t.Run("malformed JSON logs warning", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "result.json"), []byte("{bad json"), 0644)

		var logged string
		deps := &Deps{
			AgentResultDir: func(name string) string { return dir },
		}
		e := NewForTest("spi-test", "wizard-test", nil, deps)
		e.log = func(fmt string, args ...interface{}) {
			logged = fmt
		}

		got := e.readAgentResult("agent-foo")
		if got != nil {
			t.Errorf("expected nil on malformed JSON, got %+v", got)
		}
		if !strings.Contains(logged, "failed to parse") {
			t.Errorf("expected warning log about parse failure, got %q", logged)
		}
	})

	t.Run("valid result.json", func(t *testing.T) {
		dir := t.TempDir()
		ar := agentResultJSON{
			Result:       "test_failure",
			Branch:       "feat/spi-xyz",
			Commit:       "abc123",
			ElapsedS:     42,
			TotalTokens:  5000,
			ContextIn:    3000,
			ContextOut:   2000,
			FilesChanged: 3,
			LinesAdded:   100,
			LinesRemoved: 20,
			Turns:        5,
			CostUSD:      0.12,
		}
		data, _ := json.Marshal(ar)
		os.WriteFile(filepath.Join(dir, "result.json"), data, 0644)

		deps := &Deps{
			AgentResultDir: func(name string) string { return dir },
		}
		e := NewForTest("spi-test", "wizard-test", nil, deps)

		got := e.readAgentResult("agent-foo")
		if got == nil {
			t.Fatal("expected non-nil result")
		}
		if got.Result != "test_failure" {
			t.Errorf("Result = %q, want %q", got.Result, "test_failure")
		}
		if got.TotalTokens != 5000 {
			t.Errorf("TotalTokens = %d, want 5000", got.TotalTokens)
		}
		if got.FilesChanged != 3 {
			t.Errorf("FilesChanged = %d, want 3", got.FilesChanged)
		}
		if got.Turns != 5 {
			t.Errorf("Turns = %d, want 5", got.Turns)
		}
		if got.CostUSD != 0.12 {
			t.Errorf("CostUSD = %f, want 0.12", got.CostUSD)
		}
	})
}

func TestRecordAgentRunPopulatesCostAndReviewRounds(t *testing.T) {
	dir := t.TempDir()

	// Write result.json with cost and token data.
	ar := agentResultJSON{
		Result:      "success",
		TotalTokens: 5000,
		ContextIn:   3000,
		ContextOut:  2000,
		Turns:       5,
		CostUSD:     0.12,
	}
	data, _ := json.Marshal(ar)
	os.WriteFile(filepath.Join(dir, "result.json"), data, 0644)

	var recorded *AgentRun
	deps := &Deps{
		AgentResultDir: func(name string) string { return dir },
		RecordAgentRun: func(run AgentRun) (string, error) {
			recorded = &run
			return "", nil
		},
	}

	state := &State{ReviewRounds: 2}
	e := NewForTest("spi-test", "wizard-test", state, deps)
	e.recordAgentRun("test-agent", "spi-test", "", "claude-sonnet-4-6", "apprentice", "implement",
		time.Now().Add(-30*time.Second), nil)

	if recorded == nil {
		t.Fatal("RecordAgentRun was not called")
	}
	if recorded.Phase != "implement" {
		t.Errorf("Phase = %q, want %q", recorded.Phase, "implement")
	}
	if recorded.ReviewRounds != 2 {
		t.Errorf("ReviewRounds = %d, want 2", recorded.ReviewRounds)
	}
	if recorded.CostUSD != 0.12 {
		t.Errorf("CostUSD = %f, want 0.12", recorded.CostUSD)
	}
	if recorded.TotalTokens != 5000 {
		t.Errorf("TotalTokens = %d, want 5000", recorded.TotalTokens)
	}
	if recorded.ContextTokensIn != 3000 {
		t.Errorf("ContextTokensIn = %d, want 3000", recorded.ContextTokensIn)
	}
	if recorded.ContextTokensOut != 2000 {
		t.Errorf("ContextTokensOut = %d, want 2000", recorded.ContextTokensOut)
	}
}

func TestRecordAgentRunContextFields(t *testing.T) {
	t.Run("nil formula leaves FormulaName and FormulaVersion empty", func(t *testing.T) {
		var recorded *AgentRun
		deps := &Deps{
			RecordAgentRun: func(run AgentRun) (string, error) {
				recorded = &run
				return "", nil
			},
		}
		e := NewForTest("spi-test", "wizard-test", nil, deps)
		e.recordAgentRun("test-agent", "spi-test", "", "claude-opus-4-6", "apprentice", "implement",
			time.Now().Add(-10*time.Second), nil)

		if recorded == nil {
			t.Fatal("RecordAgentRun was not called")
		}
		if recorded.FormulaName != "" {
			t.Errorf("FormulaName = %q, want empty", recorded.FormulaName)
		}
		if recorded.FormulaVersion != 0 {
			t.Errorf("FormulaVersion = %d, want 0", recorded.FormulaVersion)
		}
	})

	t.Run("GetBead populates BeadType", func(t *testing.T) {
		var recorded *AgentRun
		deps := &Deps{
			RecordAgentRun: func(run AgentRun) (string, error) {
				recorded = &run
				return "", nil
			},
			GetBead: func(id string) (Bead, error) {
				return Bead{ID: id, Type: "epic"}, nil
			},
		}
		e := NewForTest("spi-test", "wizard-test", nil, deps)
		e.recordAgentRun("test-agent", "spi-test", "", "claude-opus-4-6", "wizard", "implement",
			time.Now().Add(-10*time.Second), nil)

		if recorded == nil {
			t.Fatal("RecordAgentRun was not called")
		}
		if recorded.BeadType != "epic" {
			t.Errorf("BeadType = %q, want %q", recorded.BeadType, "epic")
		}
	})

	t.Run("GetBead error leaves BeadType empty", func(t *testing.T) {
		var recorded *AgentRun
		deps := &Deps{
			RecordAgentRun: func(run AgentRun) (string, error) {
				recorded = &run
				return "", nil
			},
			GetBead: func(id string) (Bead, error) {
				return Bead{}, errors.New("bead not found")
			},
		}
		e := NewForTest("spi-test", "wizard-test", nil, deps)
		e.recordAgentRun("test-agent", "spi-test", "", "claude-opus-4-6", "wizard", "implement",
			time.Now().Add(-10*time.Second), nil)

		if recorded == nil {
			t.Fatal("RecordAgentRun was not called")
		}
		if recorded.BeadType != "" {
			t.Errorf("BeadType = %q, want empty", recorded.BeadType)
		}
	})

	t.Run("ActiveTowerConfig populates Tower", func(t *testing.T) {
		var recorded *AgentRun
		deps := &Deps{
			RecordAgentRun: func(run AgentRun) (string, error) {
				recorded = &run
				return "", nil
			},
			ActiveTowerConfig: func() (*TowerConfig, error) {
				return &TowerConfig{Name: "my-team"}, nil
			},
		}
		e := NewForTest("spi-test", "wizard-test", nil, deps)
		e.recordAgentRun("test-agent", "spi-test", "", "claude-opus-4-6", "apprentice", "implement",
			time.Now().Add(-10*time.Second), nil)

		if recorded == nil {
			t.Fatal("RecordAgentRun was not called")
		}
		if recorded.Tower != "my-team" {
			t.Errorf("Tower = %q, want %q", recorded.Tower, "my-team")
		}
	})

	t.Run("ActiveTowerConfig error leaves Tower empty", func(t *testing.T) {
		var recorded *AgentRun
		deps := &Deps{
			RecordAgentRun: func(run AgentRun) (string, error) {
				recorded = &run
				return "", nil
			},
			ActiveTowerConfig: func() (*TowerConfig, error) {
				return nil, errors.New("no tower configured")
			},
		}
		e := NewForTest("spi-test", "wizard-test", nil, deps)
		e.recordAgentRun("test-agent", "spi-test", "", "claude-opus-4-6", "apprentice", "implement",
			time.Now().Add(-10*time.Second), nil)

		if recorded == nil {
			t.Fatal("RecordAgentRun was not called")
		}
		if recorded.Tower != "" {
			t.Errorf("Tower = %q, want empty", recorded.Tower)
		}
	})

	t.Run("Branch from result.json takes priority over StagingBranch", func(t *testing.T) {
		dir := t.TempDir()
		ar := agentResultJSON{Result: "success", Branch: "feat/from-result", Commit: "abc123"}
		data, _ := json.Marshal(ar)
		os.WriteFile(filepath.Join(dir, "result.json"), data, 0644)

		var recorded *AgentRun
		deps := &Deps{
			RecordAgentRun: func(run AgentRun) (string, error) {
				recorded = &run
				return "", nil
			},
			AgentResultDir: func(name string) string { return dir },
		}
		state := &State{StagingBranch: "staging/spi-test"}
		e := NewForTest("spi-test", "wizard-test", state, deps)
		e.recordAgentRun("test-agent", "spi-test", "", "claude-opus-4-6", "apprentice", "implement",
			time.Now().Add(-10*time.Second), nil)

		if recorded == nil {
			t.Fatal("RecordAgentRun was not called")
		}
		if recorded.Branch != "feat/from-result" {
			t.Errorf("Branch = %q, want %q", recorded.Branch, "feat/from-result")
		}
		if recorded.CommitSHA != "abc123" {
			t.Errorf("CommitSHA = %q, want %q", recorded.CommitSHA, "abc123")
		}
	})

	t.Run("Branch falls back to StagingBranch when result has no branch", func(t *testing.T) {
		dir := t.TempDir()
		ar := agentResultJSON{Result: "success"}
		data, _ := json.Marshal(ar)
		os.WriteFile(filepath.Join(dir, "result.json"), data, 0644)

		var recorded *AgentRun
		deps := &Deps{
			RecordAgentRun: func(run AgentRun) (string, error) {
				recorded = &run
				return "", nil
			},
			AgentResultDir: func(name string) string { return dir },
		}
		state := &State{StagingBranch: "staging/spi-test"}
		e := NewForTest("spi-test", "wizard-test", state, deps)
		e.recordAgentRun("test-agent", "spi-test", "", "claude-opus-4-6", "apprentice", "implement",
			time.Now().Add(-10*time.Second), nil)

		if recorded == nil {
			t.Fatal("RecordAgentRun was not called")
		}
		if recorded.Branch != "staging/spi-test" {
			t.Errorf("Branch = %q, want %q", recorded.Branch, "staging/spi-test")
		}
	})

	t.Run("Branch empty when no result and no StagingBranch", func(t *testing.T) {
		var recorded *AgentRun
		deps := &Deps{
			RecordAgentRun: func(run AgentRun) (string, error) {
				recorded = &run
				return "", nil
			},
		}
		e := NewForTest("spi-test", "wizard-test", nil, deps)
		e.recordAgentRun("test-agent", "spi-test", "", "claude-opus-4-6", "apprentice", "implement",
			time.Now().Add(-10*time.Second), nil)

		if recorded == nil {
			t.Fatal("RecordAgentRun was not called")
		}
		if recorded.Branch != "" {
			t.Errorf("Branch = %q, want empty", recorded.Branch)
		}
	})

	t.Run("WaveIndex from state.Wave", func(t *testing.T) {
		var recorded *AgentRun
		deps := &Deps{
			RecordAgentRun: func(run AgentRun) (string, error) {
				recorded = &run
				return "", nil
			},
		}
		state := &State{Wave: 3}
		e := NewForTest("spi-test", "wizard-test", state, deps)
		e.recordAgentRun("test-agent", "spi-test", "", "claude-opus-4-6", "apprentice", "implement",
			time.Now().Add(-10*time.Second), nil)

		if recorded == nil {
			t.Fatal("RecordAgentRun was not called")
		}
		if recorded.WaveIndex != 3 {
			t.Errorf("WaveIndex = %d, want 3", recorded.WaveIndex)
		}
	})

}

func TestGitDiffStats(t *testing.T) {
	t.Run("empty base branch", func(t *testing.T) {
		fc, la, lr := gitDiffStats("/tmp", "", "feat/branch")
		if fc != 0 || la != 0 || lr != 0 {
			t.Errorf("expected zeros for empty base branch, got %d/%d/%d", fc, la, lr)
		}
	})

	t.Run("empty feature branch", func(t *testing.T) {
		fc, la, lr := gitDiffStats("/tmp", "main", "")
		if fc != 0 || la != 0 || lr != 0 {
			t.Errorf("expected zeros for empty feature branch, got %d/%d/%d", fc, la, lr)
		}
	})

	t.Run("nonexistent repo path", func(t *testing.T) {
		fc, la, lr := gitDiffStats("/nonexistent/path", "main", "feat/branch")
		if fc != 0 || la != 0 || lr != 0 {
			t.Errorf("expected zeros for nonexistent repo, got %d/%d/%d", fc, la, lr)
		}
	})

	t.Run("real git repo with diff", func(t *testing.T) {
		dir := t.TempDir()

		gitRun := func(args ...string) {
			t.Helper()
			cmd := exec.Command("git", args...)
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git %s failed: %v\n%s", args[0], err, out)
			}
		}

		gitRun("init", "-b", "main")
		gitRun("config", "user.email", "test@test.com")
		gitRun("config", "user.name", "Test")

		// Initial commit on main
		os.WriteFile(filepath.Join(dir, "file.txt"), []byte("line1\n"), 0644)
		gitRun("add", ".")
		gitRun("commit", "-m", "init")

		// Feature branch with changes
		gitRun("checkout", "-b", "feat/test")
		os.WriteFile(filepath.Join(dir, "file.txt"), []byte("line1\nline2\nline3\n"), 0644)
		os.WriteFile(filepath.Join(dir, "new.txt"), []byte("hello\n"), 0644)
		gitRun("add", ".")
		gitRun("commit", "-m", "changes")

		fc, la, lr := gitDiffStats(dir, "main", "feat/test")
		if fc != 2 {
			t.Errorf("FilesChanged = %d, want 2", fc)
		}
		if la != 3 { // 2 new lines in file.txt + 1 in new.txt
			t.Errorf("LinesAdded = %d, want 3", la)
		}
		if lr != 0 {
			t.Errorf("LinesRemoved = %d, want 0", lr)
		}
	})
}
