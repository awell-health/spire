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

	e := NewForTest("spi-test", "wizard-test", nil, deps)
	e.graphState = &GraphState{Counters: map[string]int{"review_rounds": 2}}
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
	t.Run("graph populates FormulaName and FormulaVersion", func(t *testing.T) {
		var recorded *AgentRun
		deps := &Deps{
			RecordAgentRun: func(run AgentRun) (string, error) {
				recorded = &run
				return "", nil
			},
		}
		e := NewForTest("spi-test", "wizard-test", nil, deps)
		e.graph = &FormulaStepGraph{Name: "task-default", Version: 3}
		e.recordAgentRun("test-agent", "spi-test", "", "claude-opus-4-6", "apprentice", "implement",
			time.Now().Add(-10*time.Second), nil)

		if recorded == nil {
			t.Fatal("RecordAgentRun was not called")
		}
		if recorded.FormulaName != "task-default" {
			t.Errorf("FormulaName = %q, want %q", recorded.FormulaName, "task-default")
		}
		if recorded.FormulaVersion != 3 {
			t.Errorf("FormulaVersion = %d, want 3", recorded.FormulaVersion)
		}
	})

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

}

func TestClassifyFailure(t *testing.T) {
	tests := []struct {
		name     string
		spawnErr error
		result   string
		want     string
	}{
		// Successful outcomes → empty class.
		{"success result", nil, "success", ""},
		{"no_changes result", nil, "no_changes", ""},
		{"empty result", nil, "", ""},

		// Result-based classification.
		{"timeout result", nil, "timeout", "timeout"},
		{"test_failure result", nil, "test_failure", "test_fail"},
		{"review_rejected result", nil, "review_rejected", "review_reject"},

		// Error-based classification (result is "error" but spawnErr gives details).
		{"killed signal", errors.New("signal: killed"), "error", "timeout"},
		{"terminated signal", errors.New("signal: terminated"), "error", "timeout"},
		{"merge conflict", errors.New("merge conflict in file.go"), "error", "merge_conflict"},
		{"CONFLICT marker", errors.New("CONFLICT (content): Merge conflict"), "error", "merge_conflict"},
		{"build fail", errors.New("build fail: exit 1"), "error", "build_fail"},
		{"compilation error", errors.New("compilation error in main.go"), "error", "build_fail"},

		// Auth failures.
		{"permission denied", errors.New("permission denied"), "error", "auth_fail"},
		{"Permission Denied case insensitive", errors.New("Permission Denied on /path"), "error", "auth_fail"},
		{"401 error", errors.New("HTTP 401 Unauthorized"), "error", "auth_fail"},
		{"403 error", errors.New("403 Forbidden"), "error", "auth_fail"},
		{"authentication failed", errors.New("authentication failed for token"), "error", "auth_fail"},
		{"unauthorized", errors.New("unauthorized access to resource"), "error", "auth_fail"},

		// Rate limiting.
		{"rate limit", errors.New("rate limit exceeded"), "error", "rate_limit"},
		{"429 error", errors.New("HTTP 429 Too Many Requests"), "error", "rate_limit"},
		{"too many requests", errors.New("too many requests, please retry"), "error", "rate_limit"},
		{"throttled", errors.New("request throttled by API"), "error", "rate_limit"},
		{"throttling", errors.New("throttling applied"), "error", "rate_limit"},

		// Network errors.
		{"connection refused", errors.New("dial tcp: connection refused"), "error", "network_error"},
		{"ECONNRESET", errors.New("read: ECONNRESET"), "error", "network_error"},
		{"dns resolution", errors.New("dns resolution failed for host"), "error", "network_error"},
		{"DNS uppercase", errors.New("DNS lookup error"), "error", "network_error"},
		{"no such host", errors.New("dial tcp: no such host api.example.com"), "error", "network_error"},
		{"network unreachable", errors.New("network is unreachable"), "error", "network_error"},

		// Resource limits.
		{"out of memory", errors.New("out of memory"), "error", "resource_limit"},
		{"OOM uppercase", errors.New("process OOM killed"), "error", "resource_limit"},
		{"killed process", errors.New("process killed by system"), "error", "resource_limit"},
		{"resource exhausted", errors.New("resource exhausted: too many open files"), "error", "resource_limit"},

		// Git errors (beyond merge conflict).
		{"rebase error", errors.New("rebase in progress; abort first"), "error", "git_error"},
		{"detached HEAD", errors.New("detached HEAD state, cannot commit"), "error", "git_error"},
		{"dirty worktree", errors.New("dirty worktree: uncommitted changes"), "error", "git_error"},
		{"not a git repo", errors.New("not a git repository"), "error", "git_error"},

		// Lint/format failures.
		{"lint error", errors.New("lint errors found in 3 files"), "error", "lint_fail"},
		{"eslint", errors.New("eslint: 5 errors, 2 warnings"), "error", "lint_fail"},
		{"prettier", errors.New("prettier: 2 files would be reformatted"), "error", "lint_fail"},
		{"format check", errors.New("format check failed"), "error", "lint_fail"},

		// Context/token limits.
		{"context length", errors.New("context length exceeded: 200k tokens"), "error", "context_limit"},
		{"max tokens", errors.New("max tokens reached"), "error", "context_limit"},
		{"token limit", errors.New("token limit exceeded"), "error", "context_limit"},
		{"context window", errors.New("context window full"), "error", "context_limit"},

		// Spawn failures.
		{"spawn error", errors.New("spawn failed: cannot create process"), "error", "spawn_fail"},
		{"exec error", errors.New("exec: claude not found in PATH"), "error", "spawn_fail"},
		{"not found", errors.New("binary not found: /usr/bin/claude"), "error", "spawn_fail"},
		{"no such file", errors.New("no such file or directory: /tmp/agent"), "error", "spawn_fail"},

		// Fallback to "unknown" for error/empty_diff without recognizable spawnErr.
		{"generic error result", nil, "error", "unknown"},
		{"error with unrecognized spawnErr", errors.New("exit status 1"), "error", "unknown"},
		{"empty_diff result", nil, "empty_diff", "unknown"},
		{"empty_diff with spawnErr", errors.New("exit status 1"), "empty_diff", "unknown"},

		// spawnErr present but result indicates success → empty class.
		{"spawnErr but success result", errors.New("something"), "success", ""},

		// Unrecognized result string without spawnErr → empty class.
		{"unknown result string", nil, "some_custom_value", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyFailure(tt.spawnErr, tt.result)
			if got != tt.want {
				t.Errorf("classifyFailure(%v, %q) = %q, want %q", tt.spawnErr, tt.result, got, tt.want)
			}
		})
	}
}

func TestClassifyFailureCatchAllOnly(t *testing.T) {
	// Verify that "unknown" only triggers when no specific pattern matches.
	// All specific patterns should classify to something other than "unknown".
	specificPatterns := []struct {
		spawnErr string
		want     string
	}{
		{"permission denied", "auth_fail"},
		{"rate limit", "rate_limit"},
		{"connection refused", "network_error"},
		{"out of memory", "resource_limit"},
		{"rebase in progress", "git_error"},
		{"lint errors", "lint_fail"},
		{"context length exceeded", "context_limit"},
		{"spawn failed", "spawn_fail"},
		{"merge conflict", "merge_conflict"},
		{"build fail", "build_fail"},
		{"signal: killed", "timeout"},
	}
	for _, tt := range specificPatterns {
		t.Run("not_unknown/"+tt.want, func(t *testing.T) {
			got := classifyFailure(errors.New(tt.spawnErr), "error")
			if got == "unknown" {
				t.Errorf("classifyFailure(%q, \"error\") = \"unknown\", want %q — pattern should match before catch-all", tt.spawnErr, tt.want)
			}
			if got != tt.want {
				t.Errorf("classifyFailure(%q, \"error\") = %q, want %q", tt.spawnErr, got, tt.want)
			}
		})
	}

	// Only truly unrecognizable errors should fall through to "unknown".
	unknowns := []string{
		"exit status 1",
		"exit status 2",
		"unexpected EOF",
		"broken pipe",
	}
	for _, msg := range unknowns {
		t.Run("unknown/"+msg, func(t *testing.T) {
			got := classifyFailure(errors.New(msg), "error")
			if got != "unknown" {
				t.Errorf("classifyFailure(%q, \"error\") = %q, want \"unknown\" — unrecognizable error should fall through", msg, got)
			}
		})
	}
}

func TestClassifyFailureSpawnErrPrecedence(t *testing.T) {
	// When result is "error" and spawnErr is non-empty, spawnErr-based
	// classification takes precedence over the catch-all.
	got := classifyFailure(errors.New("connection refused"), "error")
	if got != "network_error" {
		t.Errorf("spawnErr should take precedence: got %q, want \"network_error\"", got)
	}

	// When spawnErr is empty and result is "error", falls to "unknown".
	got = classifyFailure(nil, "error")
	if got != "unknown" {
		t.Errorf("nil spawnErr with error result: got %q, want \"unknown\"", got)
	}
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
