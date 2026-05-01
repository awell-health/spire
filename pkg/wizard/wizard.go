package wizard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/apprentice"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/executor"
	spgit "github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/promptctx"
	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/awell-health/spire/pkg/runtime"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// wizardLogSink returns a printf-style log closure that stamps every line
// with the canonical RunContext field vocabulary (docs/design/
// spi-xplwy-runtime-contract.md §1.4). The backend spawning this wizard
// populates SPIRE_TOWER, SPIRE_BEAD_ID, SPIRE_REPO_PREFIX, SPIRE_ROLE, and
// the workspace/handoff vars — runtime.RunContextFromEnv() reads that set
// and LogFields() renders it as a stable suffix. Missing values emit "".
//
// The wrapped closure snapshots env at call time (not sink construction)
// so late-bound writes by the wizard (e.g. SPIRE_FORMULA_STEP updates on
// phase transitions) are picked up automatically.
func wizardLogSink(label string) func(string, ...interface{}) {
	return func(format string, a ...interface{}) {
		fmt.Fprintf(os.Stderr, "[%s] %s%s\n", label, fmt.Sprintf(format, a...), runtime.LogFields(runtime.RunContextFromEnv()))
	}
}

// ClaudeMetrics captures token usage, cost, and tool call counts from a Claude CLI invocation.
type ClaudeMetrics struct {
	InputTokens      int
	OutputTokens     int
	TotalTokens      int
	Turns            int
	MaxTurns         int    // the cap in effect for this run (set by caller, not from result event)
	StopReason       string // "end_turn" | "max_turns" | "tool_use" | ... (from Claude result.stop_reason)
	Subtype          string // Claude result subtype, e.g. "success" or "error_max_turns"
	IsError          bool   // Claude result.is_error
	TerminalReason   string // Claude result.terminal_reason, e.g. "max_turns"
	APIErrorStatus   int    // Claude result.api_error_status, when present
	CacheReadTokens  int64
	CacheWriteTokens int64
	CostUSD          float64
	ToolCalls        map[string]int // tool_name → invocation count (e.g. {"Read": 12, "Edit": 3})
	// AuthProfileFinal is set to "api-key" when a 429 on a subscription-slot
	// invocation triggered a subscription→api-key swap (spi-mdxtww). Empty
	// otherwise. Propagates to agent_runs.auth_profile_final via result.json.
	AuthProfileFinal string
}

// Add returns the sum of two ClaudeMetrics values.
// MaxTurns and StopReason are per-run identity, not summable: prefer the
// receiver's value, falling back to the other's when the receiver is unset.
func (m ClaudeMetrics) Add(other ClaudeMetrics) ClaudeMetrics {
	merged := ClaudeMetrics{
		InputTokens:      m.InputTokens + other.InputTokens,
		OutputTokens:     m.OutputTokens + other.OutputTokens,
		TotalTokens:      m.TotalTokens + other.TotalTokens,
		Turns:            m.Turns + other.Turns,
		CacheReadTokens:  m.CacheReadTokens + other.CacheReadTokens,
		CacheWriteTokens: m.CacheWriteTokens + other.CacheWriteTokens,
		CostUSD:          m.CostUSD + other.CostUSD,
		MaxTurns:         m.MaxTurns,
		StopReason:       m.StopReason,
		Subtype:          m.Subtype,
		IsError:          m.IsError || other.IsError,
		TerminalReason:   m.TerminalReason,
		APIErrorStatus:   m.APIErrorStatus,
	}
	if merged.MaxTurns == 0 {
		merged.MaxTurns = other.MaxTurns
	}
	if merged.StopReason == "" {
		merged.StopReason = other.StopReason
	}
	if merged.Subtype == "" {
		merged.Subtype = other.Subtype
	}
	if merged.TerminalReason == "" {
		merged.TerminalReason = other.TerminalReason
	}
	if merged.APIErrorStatus == 0 {
		merged.APIErrorStatus = other.APIErrorStatus
	}
	// AuthProfileFinal is sticky: if either side observed a 429 auto-promote
	// anywhere in the run, the merged metrics carry "api-key" forward so
	// downstream agent_runs reflects the final credential used.
	merged.AuthProfileFinal = m.AuthProfileFinal
	if merged.AuthProfileFinal == "" {
		merged.AuthProfileFinal = other.AuthProfileFinal
	}
	// Merge tool call maps.
	if len(m.ToolCalls) > 0 || len(other.ToolCalls) > 0 {
		merged.ToolCalls = make(map[string]int)
		for k, v := range m.ToolCalls {
			merged.ToolCalls[k] += v
		}
		for k, v := range other.ToolCalls {
			merged.ToolCalls[k] += v
		}
	}
	return merged
}

// parseClaudeResultJSON scans Claude CLI JSON output for the result event
// and extracts the text result, usage metrics, and tool call counts.
// Returns zero metrics on any parse failure (best effort, never errors).
func parseClaudeResultJSON(output []byte) (resultText string, metrics ClaudeMetrics) {
	lines := bytes.Split(output, []byte("\n"))

	// Forward scan to count tool_use events by tool name.
	toolCalls := make(map[string]int)
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || !bytes.Contains(line, []byte(`"type"`)) {
			continue
		}
		var evt struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}
		if json.Unmarshal(line, &evt) == nil && evt.Type == "tool_use" && evt.Name != "" {
			toolCalls[evt.Name]++
		}
	}

	// Reverse scan for the result event (typically the last line).
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		if !bytes.Contains(line, []byte(`"type"`)) {
			continue
		}
		var evt struct {
			Type           string  `json:"type"`
			Subtype        string  `json:"subtype"`
			Result         string  `json:"result"`
			IsError        bool    `json:"is_error"`
			APIErrorStatus int     `json:"api_error_status"`
			NumTurns       int     `json:"num_turns"`
			StopReason     string  `json:"stop_reason"`
			TerminalReason string  `json:"terminal_reason"`
			TotalCostUSD   float64 `json:"total_cost_usd"`
			Usage          struct {
				InputTokens              int   `json:"input_tokens"`
				OutputTokens             int   `json:"output_tokens"`
				CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
				CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(line, &evt); err != nil {
			continue
		}
		if evt.Type == "result" {
			resultText = evt.Result
			metrics = ClaudeMetrics{
				InputTokens:      evt.Usage.InputTokens,
				OutputTokens:     evt.Usage.OutputTokens,
				TotalTokens:      evt.Usage.InputTokens + evt.Usage.OutputTokens,
				Turns:            evt.NumTurns,
				StopReason:       evt.StopReason,
				Subtype:          evt.Subtype,
				IsError:          evt.IsError,
				TerminalReason:   evt.TerminalReason,
				APIErrorStatus:   evt.APIErrorStatus,
				CacheReadTokens:  evt.Usage.CacheReadInputTokens,
				CacheWriteTokens: evt.Usage.CacheCreationInputTokens,
				CostUSD:          evt.TotalCostUSD,
			}
			if len(toolCalls) > 0 {
				metrics.ToolCalls = toolCalls
			}
			return
		}
	}
	return
}

type implementClaudeFailure struct {
	Err            string
	IsError        bool
	Subtype        string
	TerminalReason string
	StopReason     string
	Turns          int
	APIErrorStatus int
}

func newImplementClaudeFailure(runErr error, metrics ClaudeMetrics) *implementClaudeFailure {
	terminal := strings.TrimSpace(metrics.TerminalReason)
	abnormalTerminal := terminal != "" && terminal != "completed" && terminal != "end_turn"
	stopReason := strings.TrimSpace(metrics.StopReason)
	abnormalStop := stopReason == "max_turns"
	abnormalSubtype := strings.HasPrefix(strings.TrimSpace(metrics.Subtype), "error")
	if runErr == nil && !metrics.IsError && !abnormalTerminal && !abnormalStop && !abnormalSubtype {
		return nil
	}
	f := &implementClaudeFailure{
		IsError:        metrics.IsError,
		Subtype:        metrics.Subtype,
		TerminalReason: metrics.TerminalReason,
		StopReason:     metrics.StopReason,
		Turns:          metrics.Turns,
		APIErrorStatus: metrics.APIErrorStatus,
	}
	if runErr != nil {
		f.Err = runErr.Error()
	}
	return f
}

func (f *implementClaudeFailure) reason() string {
	if f == nil {
		return ""
	}
	switch {
	case f.APIErrorStatus != 0:
		return fmt.Sprintf("api-%d", f.APIErrorStatus)
	case f.TerminalReason != "":
		return f.TerminalReason
	case f.Subtype != "":
		return f.Subtype
	case f.StopReason != "":
		return f.StopReason
	case f.Err != "":
		return "cli-error"
	default:
		return "unknown"
	}
}

func implementResult(committed, buildPassed, testsPassed bool, failure *implementClaudeFailure) string {
	if !buildPassed {
		return "build_failure"
	}
	if !committed {
		if failure != nil {
			return "implement_failure"
		}
		return "no_changes"
	}
	if failure != nil {
		return "partial"
	}
	if !testsPassed {
		return "test_failure"
	}
	return "success"
}

func annotateImplementClaudeFailure(beadID, result string, committed, buildPassed bool, failure *implementClaudeFailure, deps *Deps, log func(string, ...interface{})) {
	if failure == nil || deps == nil {
		return
	}
	reason := sanitizeResultLabelPart(failure.reason())
	labelPrefix := "implement:failed-"
	if result == "partial" {
		labelPrefix = "implement:partial-"
	}
	label := labelPrefix + reason
	if deps.AddLabel != nil {
		if err := deps.AddLabel(beadID, label); err != nil && log != nil {
			log("warning: could not add %s label: %s", label, err)
		}
	}

	note := fmt.Sprintf("Claude implement subprocess did not finish cleanly; recorded result=%s (committed=%t build_passed=%t reason=%s err=%q is_error=%t subtype=%q terminal_reason=%q stop_reason=%q turns=%d api_error_status=%d).",
		result, committed, buildPassed, reason, failure.Err, failure.IsError, failure.Subtype,
		failure.TerminalReason, failure.StopReason, failure.Turns, failure.APIErrorStatus)
	if deps.GetComments != nil {
		if comments, err := deps.GetComments(beadID); err == nil {
			for _, c := range comments {
				if c != nil && strings.Contains(c.Text, "Claude implement subprocess did not finish cleanly") &&
					strings.Contains(c.Text, "reason="+reason) {
					return
				}
			}
		}
	}
	if deps.AddComment != nil {
		if err := deps.AddComment(beadID, note); err != nil && log != nil {
			log("warning: could not add implement failure comment: %s", err)
		}
	}
}

func sanitizeResultLabelPart(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "unknown"
	}
	return out
}

// wizardAuthState caches the AuthContext for the wizard subprocess so the
// 429 auto-promote swap (spi-mdxtww) is sticky across Claude invocations
// for the rest of the subprocess's lifetime. The cache holds a single
// pointer; (*config.AuthContext).SwapToAPIKey mutates *that* pointer, so
// every subsequent WizardRunClaude / WizardRunClaudeCapture call in the
// same process sees the promoted slot. The cache is never persisted — a
// wizard restart clears it (matches the "cooldown in-memory only" rule
// from the epic).
var wizardAuthState struct {
	mu      sync.Mutex
	ctx     *config.AuthContext
	promote string // "" or config.AuthSlotAPIKey once a 429-driven swap fires
}

// InitWizardAuth primes the wizard's AuthContext cache. The wizard's
// entry points (CmdWizardRun, CmdWizardReview) should call this once at
// startup with the AuthContext loaded from GraphState so subsequent
// claude invocations see the operator's selected credential slot. Safe
// to call with a nil ctx — callers that never configured auth (legacy
// path) keep the pre-auth behavior (no env injection, no 429 handling).
func InitWizardAuth(ctx *config.AuthContext) {
	wizardAuthState.mu.Lock()
	defer wizardAuthState.mu.Unlock()
	wizardAuthState.ctx = ctx
	wizardAuthState.promote = ""
}

// LoadWizardAuthFromState reads the wizard's AuthContext from the graph
// state file written by `spire summon`. Returns nil when the agent name
// can't be resolved, the state file doesn't exist, or no Auth was
// selected — callers treat all three as "no auth configured" and fall
// through to legacy env behavior.
func LoadWizardAuthFromState(agentName string, configDirFn func() (string, error)) *config.AuthContext {
	if agentName == "" || configDirFn == nil {
		return nil
	}
	state, err := executor.LoadGraphState(agentName, configDirFn)
	if err != nil || state == nil {
		return nil
	}
	return state.Auth
}

// currentWizardAuth returns the cached AuthContext (or nil). Access is
// through this helper so the mutex stays encapsulated.
func currentWizardAuth() *config.AuthContext {
	wizardAuthState.mu.Lock()
	defer wizardAuthState.mu.Unlock()
	return wizardAuthState.ctx
}

// recordWizardPromotion is invoked when InvokeClaudeWithAuth reports a
// 429-driven swap. Subsequent WizardWriteResult calls surface the final
// slot to result.json so the executor's recordAgentRun propagates it to
// agent_runs.auth_profile_final. The flag is sticky: once set it never
// clears for the remainder of the wizard process.
func recordWizardPromotion(slot string) {
	wizardAuthState.mu.Lock()
	defer wizardAuthState.mu.Unlock()
	if wizardAuthState.promote == "" && slot != "" {
		wizardAuthState.promote = slot
	}
}

// wizardPromotionSlot returns the promote slot ("" or "api-key").
func wizardPromotionSlot() string {
	wizardAuthState.mu.Lock()
	defer wizardAuthState.mu.Unlock()
	return wizardAuthState.promote
}

// resetWizardAuthStateForTest resets the package-level cache so each
// test starts from a clean slate.
func resetWizardAuthStateForTest() {
	wizardAuthState.mu.Lock()
	defer wizardAuthState.mu.Unlock()
	wizardAuthState.ctx = nil
	wizardAuthState.promote = ""
}

// CmdWizardRun is the internal entry point for a local wizard process.
// It claims a bead, creates a worktree, runs design + implement phases,
// validates, commits, updates the bead, and hands off to review.
//
// Usage: spire apprentice run <bead-id> [--name <wizard-name>] [--review-fix] [--apprentice] [--build-fix] [--no-review] [--worktree-dir <path>]
func CmdWizardRun(args []string, deps *Deps) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: spire apprentice run <bead-id> [--name <name>] [--review-fix] [--apprentice] [--build-fix] [--no-review] [--worktree-dir <path>] [--start-ref <ref>]")
	}

	// 1. Parse args
	beadID := args[0]
	wizardName := "wizard"
	reviewFix := false
	apprenticeMode := false
	buildFixMode := false
	noReview := false
	worktreeDirOverride := ""
	startRef := ""
	customPromptFile := ""
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--name":
			if i+1 < len(args) {
				i++
				wizardName = args[i]
			}
		case "--review-fix":
			reviewFix = true
		case "--apprentice":
			apprenticeMode = true
		case "--build-fix":
			buildFixMode = true
		case "--no-review":
			noReview = true
		case "--worktree-dir":
			if i+1 < len(args) {
				i++
				worktreeDirOverride = args[i]
			}
		case "--start-ref":
			if i+1 < len(args) {
				i++
				startRef = args[i]
			}
		case "--custom-prompt-file":
			if i+1 < len(args) {
				i++
				customPromptFile = args[i]
			}
		}
	}

	// Read and remove the custom prompt temp file immediately.
	var customPrompt string
	if customPromptFile != "" {
		data, err := os.ReadFile(customPromptFile)
		if err != nil {
			return fmt.Errorf("read custom prompt file %s: %w", customPromptFile, err)
		}
		customPrompt = strings.TrimSpace(string(data))
		os.Remove(customPromptFile)
	}
	if os.Getenv("SPIRE_APPRENTICE") == "1" {
		apprenticeMode = true
	}

	startedAt := time.Now()
	log := wizardLogSink(wizardName)

	// Prime the wizard's AuthContext cache from the graph state file
	// written by `spire summon`. A nil result means no auth was selected
	// (legacy path); the helper treats that as "no injection, no 429
	// handling" — pre-auth-config behavior.
	InitWizardAuth(LoadWizardAuthFromState(wizardName, deps.ConfigDir))

	// --- Build-fix mode: early return path ---
	// The executor spawns the apprentice with --build-fix --apprentice --worktree-dir <path>.
	// The apprentice works directly in the existing staging worktree to fix build errors.
	if buildFixMode {
		return cmdBuildFix(beadID, wizardName, worktreeDirOverride, startedAt, deps, log)
	}

	// Cluster wizard pods receive a GitHub PAT via the GITHUB_TOKEN env var
	// (pod_builder wires it as an Optional SecretKeyRef). Install a global
	// url.insteadOf rewrite so `git push origin <base>` in the merge phase
	// authenticates transparently. Local-native wizards typically have no
	// GITHUB_TOKEN set; they fall back to the user's credential helper / SSH
	// keys and get a clear warning if neither is in play.
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		if err := spgit.ConfigureGitHubTokenAuth(tok); err != nil {
			log("WARN: configure github token auth: %v", err)
		} else {
			log("configured github token auth for push")
		}
	} else if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		log("WARN: no GITHUB_TOKEN configured — cluster push requires this env var")
	}

	if err := deps.RequireDolt(); err != nil {
		return err
	}

	// 2. Resolve repo for this bead (prefix -> repo URL + path)
	repoPath, repoURL, baseBranch, err := deps.ResolveRepo(beadID)
	if err != nil {
		return fmt.Errorf("resolve repo: %w", err)
	}
	log("repo: %s (base: %s)", repoURL, baseBranch)

	// Load repo config (spire.yaml)
	repoCfg, err := repoconfig.Load(repoPath)
	if err != nil {
		log("warning: could not load spire.yaml: %s (using defaults)", err)
		repoCfg = &repoconfig.RepoConfig{}
	}

	model := repoconfig.ResolveModel("", repoCfg.Agent.Model)
	timeout := repoconfig.ResolveTimeout("", repoCfg.Agent.Timeout, repoconfig.DefaultTimeout)
	maxTurns := wizardConfiguredMaxTurns(repoCfg)
	designTimeout := repoconfig.ResolveDesignTimeout(repoCfg.Agent.DesignTimeout)
	branchPattern := repoconfig.ResolveBranchPattern(repoCfg.Branch.Pattern)
	if repoCfg.Branch.Base != "" && baseBranch == "" {
		baseBranch = repoCfg.Branch.Base
	}

	// Bead-level base-branch override (from spire file --branch) takes
	// precedence over repo config and tower defaults. Walks up the parent
	// chain so child tasks inherit the base branch from their epic.
	if bb := findBaseBranchInParentChain(beadID, deps); bb != "" {
		log("using bead base-branch override: %s (was: %s)", bb, baseBranch)
		baseBranch = bb
	}

	branchName := strings.ReplaceAll(branchPattern, "{bead-id}", beadID)

	// Override start point when the executor passes --start-ref (e.g. staging
	// branch tip for later-wave children). This replaces baseBranch as the
	// start point for CreateWorktreeNewBranch without changing anything else.
	if startRef != "" {
		log("using start-ref override: %s (was: %s)", startRef, baseBranch)
		baseBranch = startRef
	}

	// 3. Create or resume git worktree.
	var wc *spgit.WorktreeContext
	if worktreeDirOverride != "" {
		// The executor provided a workspace directory. Resume it via pkg/git —
		// captures a session baseline (StartSHA) and detects the actual checked-
		// out branch. Pass "" for branch so pkg/git reads it from the worktree.
		// The worktree is borrowed, not owned: no Cleanup, no branch creation.
		// This covers both review-fix (borrowed staging) and implement (executor-
		// managed feature/staging workspace from v3 graph declarations).
		var wcErr error
		wc, wcErr = spgit.ResumeWorktreeContext(worktreeDirOverride, "", baseBranch, repoPath, log)
		if wcErr != nil {
			return fmt.Errorf("resume provided worktree: %w", wcErr)
		}
		// Use the actual branch from the worktree for prompts, bead comments,
		// result.json, and agent_runs.Branch.
		branchName = wc.Branch
		log("using provided worktree: %s (branch: %s, baseline: %s)", wc.Dir, branchName, wc.StartSHA)

		// Remove .beads/ so Claude's test runs and exploratory commands don't
		// touch the production database — same safeguard as WizardCreateWorktree.
		os.RemoveAll(filepath.Join(wc.Dir, ".beads"))
	} else {
		var wcErr error
		wc, wcErr = WizardCreateWorktree(repoPath, beadID, wizardName, baseBranch, branchName, deps, log)
		if wcErr != nil {
			return fmt.Errorf("create worktree: %w", wcErr)
		}
		defer wc.Cleanup()
	}
	worktreeDir := wc.Dir
	log("worktree: %s", worktreeDir)

	// 4. Stamp phase and worktree on the existing registry entry created by BeginWork.
	// OrphanSweep and EndWork are the sole removers — the wizard no longer owns
	// registry cleanup. Log but don't fail if entry not found (e.g. test modes
	// that don't call BeginWork).
	if uerr := agent.RegistryUpdate(wizardName, func(re *agent.Entry) {
		re.Phase = "init"
		re.PhaseStartedAt = time.Now().UTC().Format(time.RFC3339)
		re.Worktree = worktreeDir
	}); uerr != nil {
		log("warning: registry stamp for %s: %v", wizardName, uerr)
	}

	// Signal handler — registry cleanup happens via OrphanSweep/EndWork
	// (BeginWork created the entry). We exit on SIGINT/SIGTERM. The
	// diagnostic forensic dump and cgo SA_SIGINFO sender-PID capture
	// were ripped out after spi-od41sr identified the killer (a test
	// reading the prod registry without isolation) — keeping them in
	// production added a slow cgo build dep for no live signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		os.Exit(1)
	}()

	// 5. Claim the bead (skip if --review-fix or --apprentice — already claimed by executor)
	os.Setenv("SPIRE_IDENTITY", wizardName)
	if !reviewFix && !apprenticeMode {
		log("claiming %s", beadID)
		if err := deps.CmdClaim([]string{beadID}); err != nil {
			return fmt.Errorf("claim: %w", err)
		}
	}

	// 6. Track whether review handoff completed (guards bead reopen on early exit)
	handoffDone := false

	// 7. Capture focus context
	log("assembling focus context")
	focusContext, err := deps.CaptureFocus(beadID)
	if err != nil {
		log("warning: focus failed: %s", err)
		focusContext = fmt.Sprintf("Bead %s — focus context unavailable", beadID)
	}

	// Get bead JSON and extract title
	beadJSON, err := deps.GetBeadJSON(beadID)
	if err != nil {
		log("warning: could not get bead JSON: %s", err)
		beadJSON = "{}"
	}
	beadTitle := WizardExtractTitle(beadJSON)

	// Install dependencies
	if repoCfg.Runtime.Install != "" {
		log("installing dependencies: %s", repoCfg.Runtime.Install)
		if err := WizardRunCmd(worktreeDir, repoCfg.Runtime.Install); err != nil {
			log("warning: install failed: %s", err)
		}
	}

	// 7b. Check for recovery retry request before starting phase execution.
	// This must happen after setup (worktree, registration, focus) but before
	// any phase work. Only checked at this entry point — not mid-execution.
	retry, retryErr := checkRetryRequest(beadID, log)
	if retryErr != nil {
		log("retry request check failed: %s — exiting", retryErr)
		elapsed := time.Since(startedAt)
		WizardWriteResult(wizardName, beadID, "retry_error", branchName, "", elapsed, ClaudeMetrics{}, deps, log)
		return retryErr
	}

	// VerifyPlan dispatch (design spi-h32xj §5): narrow-check and
	// recipe-postcondition run inline in the wizard's worktree and bypass
	// the normal phase loop. rerun-step falls through to the standard
	// skip-to-target-step handling below.
	if retry.runVerifyPlanIfNonStep(worktreeDir) {
		log("verify plan executed inline — exiting cleanly")
		elapsed := time.Since(startedAt)
		WizardWriteResult(wizardName, beadID, "retry_complete", branchName, "", elapsed, ClaudeMetrics{}, deps, log)
		return nil
	}

	// 8-9. Phase execution
	var accMetrics ClaudeMetrics
	var implFailure *implementClaudeFailure
	// reviewFixSubprocessErr captures a non-zero exit from the review-fix
	// claude subprocess (kill, timeout, bad prompt, etc.). It's checked
	// after the commit step: if the subprocess failed AND nothing was
	// staged, we must fail the whole step — see the guard after
	// WizardCommit below.
	var reviewFixSubprocessErr error
	if reviewFix {
		// --review-fix path: skip design, collect feedback, implement with feedback
		feedback := WizardCollectReviewHistory(beadID, wizardName, deps)

		// Update phase
		deps.RegistryUpdate(wizardName, func(w *Entry) {
			w.Phase = "implement"
			w.PhaseStartedAt = time.Now().UTC().Format(time.RFC3339)
		})

		// Build implement prompt with feedback
		var implPrompt string
		if customPrompt != "" {
			implPrompt = WizardBuildCustomPrompt(wizardName, beadID, repoCfg, focusContext, beadJSON, customPrompt)
		} else {
			implPrompt = WizardBuildImplementPrompt(wizardName, beadID, branchName, baseBranch,
				model, maxTurns, timeout, repoCfg, focusContext, beadJSON, "", feedback)
		}
		implPromptPath := filepath.Join(worktreeDir, ".spire-prompt.txt")
		if err := os.WriteFile(implPromptPath, []byte(implPrompt), 0644); err != nil {
			return fmt.Errorf("write implement prompt: %w", err)
		}

		reviewFixTimeout := designTimeout // spec: review-fix gets 10m, not 15m
		claudeStartedAt := time.Now()
		log("starting implement phase with review feedback (timeout: %s)", reviewFixTimeout)
		metrics, runErr := WizardRunClaude(worktreeDir, implPromptPath, model, reviewFixTimeout, maxTurns,
			WizardAgentResultDir(deps, wizardName), "review-fix")
		if runErr != nil {
			log("claude implement failed: %s", runErr)
			reviewFixSubprocessErr = runErr
		}
		accMetrics = accMetrics.Add(metrics)
		log("implement finished (%.0fs)", time.Since(claudeStartedAt).Seconds())

	} else {
		// Normal path: design phase then implement phase

		// --- Design phase (skipped in apprentice mode, custom prompt, or retry past design) ---
		var designOutput string
		skipDesign := apprenticeMode || customPrompt != "" || retry.shouldSkipTo("design")
		if !skipDesign {
			retry.enterStep("design")
			deps.RegistryUpdate(wizardName, func(w *Entry) {
				w.Phase = "design"
				w.PhaseStartedAt = time.Now().UTC().Format(time.RFC3339)
			})

			designPrompt := WizardBuildDesignPrompt(wizardName, beadID, repoCfg, focusContext, beadJSON)
			designPromptPath := filepath.Join(worktreeDir, ".spire-design-prompt.txt")
			if err := os.WriteFile(designPromptPath, []byte(designPrompt), 0644); err != nil {
				return fmt.Errorf("write design prompt: %w", err)
			}

			designStartedAt := time.Now()
			log("starting design phase (timeout: %s)", designTimeout)
			var designMetrics ClaudeMetrics
			designOutput, designMetrics, err = WizardRunClaudeCapture(worktreeDir, designPromptPath, model, designTimeout, maxTurns/2,
				WizardAgentResultDir(deps, wizardName), "design")
			if err != nil {
				log("design phase failed: %s", err)
				if retry.handleStepFailure(err.Error()) {
					elapsed := time.Since(startedAt)
					WizardWriteResult(wizardName, beadID, "retry_failure", branchName, "", elapsed, accMetrics, deps, log)
					return nil
				}
			}
			accMetrics = accMetrics.Add(designMetrics)
			log("design finished (%.0fs)", time.Since(designStartedAt).Seconds())

			// Write DESIGN.md
			designPath := filepath.Join(worktreeDir, "DESIGN.md")
			os.WriteFile(designPath, []byte(designOutput), 0644)

			// Post plan as bead comment
			deps.AddComment(beadID, fmt.Sprintf("Design plan:\n%s", designOutput))

			retry.handleStepSuccess()
		} else if retry.retrying {
			log("skipping design phase (retry target: %s)", retry.request.FromStep)
		}

		// --- Implement phase ---
		if !retry.shouldSkipTo("implement") {
			retry.enterStep("implement")
			deps.RegistryUpdate(wizardName, func(w *Entry) {
				w.Phase = "implement"
				w.PhaseStartedAt = time.Now().UTC().Format(time.RFC3339)
			})

			var implPrompt string
			if customPrompt != "" {
				implPrompt = WizardBuildCustomPrompt(wizardName, beadID, repoCfg, focusContext, beadJSON, customPrompt)
			} else {
				implPrompt = WizardBuildImplementPrompt(wizardName, beadID, branchName, baseBranch,
					model, maxTurns, timeout, repoCfg, focusContext, beadJSON, designOutput, "")
			}
			implPromptPath := filepath.Join(worktreeDir, ".spire-prompt.txt")
			if err := os.WriteFile(implPromptPath, []byte(implPrompt), 0644); err != nil {
				return fmt.Errorf("write implement prompt: %w", err)
			}

			claudeStartedAt := time.Now()
			log("starting implement phase (timeout: %s)", timeout)
			implMetrics, runErr := WizardRunClaude(worktreeDir, implPromptPath, model, timeout, maxTurns,
				WizardAgentResultDir(deps, wizardName), "implement")
			implFailure = newImplementClaudeFailure(runErr, implMetrics)
			if runErr != nil {
				log("claude implement failed: %s", runErr)
				if retry.handleStepFailure(runErr.Error()) {
					elapsed := time.Since(startedAt)
					WizardWriteResult(wizardName, beadID, "retry_failure", branchName, "", elapsed, accMetrics.Add(implMetrics), deps, log)
					return nil
				}
			}
			accMetrics = accMetrics.Add(implMetrics)
			log("implement finished (%.0fs)", time.Since(claudeStartedAt).Seconds())

			retry.handleStepSuccess()
		} else if retry.retrying {
			log("skipping implement phase (retry target: %s)", retry.request.FromStep)
		}
	}

	// 10. Commit
	if !retry.shouldSkipTo("commit") {
		retry.enterStep("commit")
	}
	commitSHA, committed := WizardCommit(wc, beadID, beadTitle, log)
	if retry.retrying && retry.currentStep == "commit" {
		if !committed {
			if retry.handleStepFailure("no changes to commit") {
				elapsed := time.Since(startedAt)
				WizardWriteResult(wizardName, beadID, "retry_failure", branchName, "", elapsed, accMetrics, deps, log)
				return nil
			}
		} else {
			retry.handleStepSuccess()
		}
	}

	// 10b. Hard-failure guard for the review-fix apprentice. See
	// reviewFixFailureGuard below for why this check is necessary and
	// why we deliberately skip WizardWriteResult on this path.
	if guardErr := reviewFixFailureGuard(reviewFix, committed, reviewFixSubprocessErr); guardErr != nil {
		elapsed := time.Since(startedAt)
		log("review-fix apprentice failed with no staged changes — failing step (%.0fs total)", elapsed.Seconds())
		return guardErr
	}

	// 11. Build gate — the apprentice can't go home until the build passes.
	// On failure, invokes Claude to fix build errors, up to N rounds.
	if !retry.shouldSkipTo("build-gate") {
		retry.enterStep("build-gate")
	}
	buildPassed := WizardBuildGate(wc, beadID, beadTitle, worktreeDir, model, repoCfg, &accMetrics,
		WizardAgentResultDir(deps, wizardName), log)
	if retry.retrying && retry.currentStep == "build-gate" {
		if !buildPassed {
			if retry.handleStepFailure("build gate failed") {
				elapsed := time.Since(startedAt)
				WizardWriteResult(wizardName, beadID, "retry_failure", branchName, commitSHA, elapsed, accMetrics, deps, log)
				return nil
			}
		} else {
			retry.handleStepSuccess()
		}
	}

	// 12. Run tests (informational only — does not gate completion).
	if !retry.shouldSkipTo("test") {
		retry.enterStep("test")
	}
	testsPassed := true
	if repoCfg.Runtime.Test != "" {
		log("validating: test")
		if err := WizardRunCmd(worktreeDir, repoCfg.Runtime.Test); err != nil {
			log("test failed: %s (informational)", err)
			testsPassed = false
			if retry.retrying && retry.currentStep == "test" {
				if retry.handleStepFailure(err.Error()) {
					elapsed := time.Since(startedAt)
					WizardWriteResult(wizardName, beadID, "retry_failure", branchName, commitSHA, elapsed, accMetrics, deps, log)
					return nil
				}
			}
		}
	}
	if retry.retrying && retry.currentStep == "test" {
		retry.handleStepSuccess()
	}

	// 13. Update bead (comment)
	wizardUpdateBead(beadID, wizardName, branchName, commitSHA, committed, testsPassed, deps, log)

	result := implementResult(committed, buildPassed, testsPassed, implFailure)
	if implFailure != nil {
		annotateImplementClaudeFailure(beadID, result, committed, buildPassed, implFailure, deps, log)
	}

	// 14. Review handoff if committed and build passes.
	if !retry.shouldSkipTo("review") {
		retry.enterStep("review")
	}
	if committed && buildPassed {
		if !testsPassed {
			log("tests failed but build passed — proceeding")
			deps.AddLabel(beadID, "test-failure")
		}
		if !apprenticeMode && !noReview {
			handoffDone = true
			WizardReviewHandoff(beadID, wizardName, branchName, deps, log)
		} else if noReview {
			handoffDone = true
			log("--no-review mode — skipping review handoff")
		} else {
			handoffDone = true
			apprenticeIdx := 0
			if s := os.Getenv("SPIRE_APPRENTICE_IDX"); s != "" {
				if v, err := strconv.Atoi(s); err == nil {
					apprenticeIdx = v
				}
			}
			attemptID := os.Getenv("SPIRE_ATTEMPT_ID")
			if err := deliverApprenticeWork(wc, beadID, apprenticeIdx, attemptID, deps, log); err != nil {
				log("apprentice delivery failed: %s", err)
				return fmt.Errorf("apprentice delivery: %w", err)
			}
		}
		if retry.retrying && retry.currentStep == "review" {
			retry.handleStepSuccess()
		}
	} else if !buildPassed {
		log("build failed — apprentice cannot hand off")
		deps.AddLabel(beadID, "build-failure")
		if retry.retrying && retry.currentStep == "review" {
			if retry.handleStepFailure("build failed — cannot hand off to review") {
				elapsed := time.Since(startedAt)
				WizardWriteResult(wizardName, beadID, "retry_failure", branchName, commitSHA, elapsed, accMetrics, deps, log)
				return nil
			}
		}
	}

	// 15. If we didn't hand off, reopen the bead so it doesn't stay orphaned.
	if !handoffDone {
		deps.UpdateBead(beadID, map[string]interface{}{"status": "open"})
		log("apprentice mode — bead reopened")
	}

	// 16. Write result
	elapsed := time.Since(startedAt)
	WizardWriteResult(wizardName, beadID, result, branchName, commitSHA, elapsed, accMetrics, deps, log)

	log("done (%.0fs total)", elapsed.Seconds())
	return nil
}

// submitApprenticeBundleFunc is the package-level seam for
// pkg/apprentice.Submit. Production code calls the upstream function; tests
// replace it with a capture stub to assert on Options without needing a real
// dolt store for the default GetBead/SetMetadata/AddComment callbacks.
var submitApprenticeBundleFunc = apprentice.Submit

// deliverApprenticeWork runs the apprentice's post-build delivery step. It
// consults the tower's configured apprentice transport and either submits a
// git bundle via pkg/apprentice.Submit (transport=bundle) or pushes the feat
// branch to the tower's remote (transport=push). Prior to this, the
// apprentice-mode exit silently returned — the wizard never learned about
// the work. Both branches return an error on failure so the caller can fail
// the attempt rather than masquerade as a successful no-op.
//
// apprenticeIdx and attemptID are resolved by the caller (from
// SPIRE_APPRENTICE_IDX / SPIRE_ATTEMPT_ID) and threaded in here so this
// function has no hidden env coupling — tests can drive it directly without
// setting process env.
func deliverApprenticeWork(wc *spgit.WorktreeContext, beadID string, apprenticeIdx int, attemptID string, deps *Deps, logf func(string, ...interface{})) error {
	transport := config.ApprenticeTransportBundle
	if deps.ActiveTowerConfig != nil {
		if tower, err := deps.ActiveTowerConfig(); err == nil && tower != nil {
			transport = tower.Apprentice.EffectiveTransport()
		}
	}
	switch transport {
	case config.ApprenticeTransportBundle:
		if deps.NewBundleStore == nil {
			return fmt.Errorf("bundle transport configured but no BundleStore factory wired")
		}
		bstore, err := deps.NewBundleStore()
		if err != nil {
			return fmt.Errorf("open bundle store: %w", err)
		}
		logf("apprentice mode — submitting bundle for %s (idx %d, base %s)", beadID, apprenticeIdx, wc.BaseBranch)
		return submitApprenticeBundleFunc(context.Background(), apprentice.Options{
			BeadID:        beadID,
			AttemptID:     attemptID,
			ApprenticeIdx: apprenticeIdx,
			BaseBranch:    wc.BaseBranch,
			StartSHA:      wc.StartSHA,
			WorktreeDir:   wc.Dir,
			Store:         bstore,
		})
	case config.ApprenticeTransportPush:
		remote := "origin"
		logf("apprentice mode — pushing %s to %s", wc.Branch, remote)
		return wc.Push(remote)
	default:
		return fmt.Errorf("unknown apprentice transport %q", transport)
	}
}

// cmdBuildFix handles the --build-fix apprentice mode. The executor writes
// .build-error.log to the staging worktree and spawns the apprentice with
// --build-fix --apprentice --worktree-dir <path>. This function reads the
// error log, invokes Claude to fix the build errors, verifies the build,
// and commits the fix directly in the staging worktree.
func cmdBuildFix(beadID, wizardName, worktreeDir string, startedAt time.Time,
	deps *Deps, log func(string, ...interface{})) error {

	if worktreeDir == "" {
		return fmt.Errorf("--build-fix requires --worktree-dir")
	}

	log("build-fix mode: working in staging worktree %s", worktreeDir)

	// Read the build error log written by the executor.
	errFile := filepath.Join(worktreeDir, ".build-error.log")
	buildErrBytes, err := os.ReadFile(errFile)
	if err != nil {
		return fmt.Errorf("read .build-error.log: %w", err)
	}
	buildErr := string(buildErrBytes)
	log("build error:\n%s", buildErr)

	// Load repo config from the worktree (it's a copy of the repo).
	repoCfg, err := repoconfig.Load(worktreeDir)
	if err != nil {
		log("warning: could not load spire.yaml: %s (using defaults)", err)
		repoCfg = &repoconfig.RepoConfig{}
	}

	model := repoCfg.Agent.Model
	if model == "" {
		model = "claude-sonnet-4-6"
	}
	maxTurns := wizardConfiguredMaxTurns(repoCfg)
	buildFixTimeout := "5m"

	// Resolve the build command for verification after fix.
	buildCmd := repoCfg.Runtime.Build

	// Capture session baseline BEFORE Claude runs. If Claude commits during
	// the run, StartSHA..HEAD will detect the new commit. Capturing after
	// Claude runs would set StartSHA == HEAD, hiding Claude's commit.
	wc, wcErr := spgit.ResumeWorktreeContext(worktreeDir, "", "", "", log)
	if wcErr != nil {
		return fmt.Errorf("resume worktree context: %w", wcErr)
	}

	// Build the prompt.
	prompt := wizardBuildBuildFixPrompt(wizardName, beadID, buildErr, buildCmd, repoCfg)
	promptPath := filepath.Join(worktreeDir, ".spire-prompt.txt")
	if err := os.WriteFile(promptPath, []byte(prompt), 0644); err != nil {
		return fmt.Errorf("write build-fix prompt: %w", err)
	}

	// Run Claude to fix the build errors.
	claudeStartedAt := time.Now()
	log("starting build-fix phase (timeout: %s)", buildFixTimeout)
	buildFixMetrics, runErr := WizardRunClaude(worktreeDir, promptPath, model, buildFixTimeout, maxTurns,
		WizardAgentResultDir(deps, wizardName), "build-fix")
	if runErr != nil {
		log("claude build-fix failed: %s", runErr)
	}
	log("build-fix finished (%.0fs)", time.Since(claudeStartedAt).Seconds())

	// Clean up the prompt file.
	os.Remove(promptPath)

	commitSHA, committed := WizardCommit(wc, beadID, "fix build errors", log)

	if committed {
		log("build-fix committed: %s", commitSHA)
	} else {
		log("build-fix: no changes to commit")
	}

	// Write result for the executor to read.
	elapsed := time.Since(startedAt)
	result := "success"
	if !committed {
		result = "no_changes"
	}
	WizardWriteResult(wizardName, beadID, result, "", commitSHA, elapsed, buildFixMetrics, deps, log)

	log("build-fix done (%.0fs total)", elapsed.Seconds())
	return nil
}

// wizardBuildBuildFixPrompt builds a focused prompt for fixing build errors
// in a staging worktree.
func wizardBuildBuildFixPrompt(wizardName, beadID, buildErr, buildCmd string, cfg *repoconfig.RepoConfig) string {
	contextPaths := cfg.Context
	if len(contextPaths) == 0 {
		contextPaths = []string{"CLAUDE.md"}
	}
	var contextBlock strings.Builder
	for _, p := range contextPaths {
		fmt.Fprintf(&contextBlock, "- %s\n", p)
	}

	buildCmdStr := buildCmd
	if buildCmdStr == "" {
		buildCmdStr = "go build ./..."
	}

	return fmt.Sprintf(`You are Spire build-fix apprentice %s.

You are working in a staging worktree where multiple branches have been merged together.
The merged code fails to build. Your ONLY job is to fix the build errors.

## Build error
`+"```"+`
%s
`+"```"+`

## Instructions
1. Read the build error output above carefully.
2. Identify the source files causing the errors.
3. Fix the compilation errors. Common causes after merging:
   - Duplicate function/type definitions
   - Missing imports
   - Signature mismatches (a function was changed in one branch but callers were in another)
   - Missing or conflicting type definitions
4. Run the build command to verify your fix: %s
5. COMMIT your changes with message: fix(%s): resolve build errors in staging worktree

## Rules
- Fix ONLY build errors. Do NOT refactor, improve, or change any other code.
- Do NOT revert changes from other branches. Reconcile conflicts by making both sides work together.
- If a function was added by one branch and a caller was added by another, make them compatible.
- Read the repo context if you need to understand conventions:
%s
`, wizardName, buildErr, buildCmdStr, beadID, contextBlock.String())
}

// ResolveRepo finds the local repo path, remote URL, and base branch
// for a bead by matching its ID prefix against registered repos.
func ResolveRepo(beadID string, deps *Deps) (repoPath, repoURL, baseBranch string, err error) {
	cfg, err := deps.LoadConfig()
	if err != nil {
		return "", "", "", err
	}

	// Extract prefix from bead ID (e.g. "spi-abc" -> "spi")
	prefix := ""
	if idx := strings.Index(beadID, "-"); idx > 0 {
		prefix = beadID[:idx]
	}

	// Look up in local config first (has the local path)
	if inst, ok := cfg.Instances[prefix]; ok {
		repoPath = inst.Path
	}

	// Query repos table for URL and branch
	database, _ := deps.ResolveDatabase(cfg)
	if database != "" && prefix != "" {
		sql := fmt.Sprintf("SELECT repo_url, branch FROM `%s`.repos WHERE prefix = '%s'",
			database, deps.SQLEscape(prefix))
		if out, err := deps.RawDoltQuery(sql); err == nil {
			rows := deps.ParseDoltRows(out, []string{"repo_url", "branch"})
			if len(rows) > 0 {
				repoURL = rows[0]["repo_url"]
				baseBranch = rows[0]["branch"]
			}
		}
	}

	if repoPath == "" {
		return "", "", "", fmt.Errorf("no local repo registered for prefix %q (bead %s)", prefix, beadID)
	}
	if baseBranch == "" {
		baseBranch = repoconfig.DefaultBranchBase
		log.Printf("[resolve] base branch not configured for prefix %q — defaulting to %q%s", prefix, baseBranch, runtime.LogFields(runtime.RunContextFromEnv()))
	}
	return repoPath, repoURL, baseBranch, nil
}

// ResolveBranchForBead resolves the branch name for a bead by loading
// spire.yaml from the given repoPath (or cwd if empty). Falls back to
// "feat/<beadID>" if the config cannot be loaded.
func ResolveBranchForBead(beadID, repoPath string) string {
	dir := repoPath
	if dir == "" {
		dir = "."
	}
	cfg, err := repoconfig.Load(dir)
	if err != nil || cfg == nil {
		return "feat/" + beadID
	}
	return cfg.ResolveBranch(beadID)
}

// WizardCreateWorktree creates a git worktree for the wizard to work in.
// On first run it creates a new branch from baseBranch. On --review-fix
// the branch already exists (committed by the previous run), so it checks
// out the existing branch instead of trying to create it again.
//
// Returns a WorktreeContext that must be used for all subsequent git operations.
func WizardCreateWorktree(repoPath, beadID, wizardName, baseBranch, branchName string, deps *Deps, log func(string, ...any)) (*spgit.WorktreeContext, error) {
	worktreeBase := filepath.Join(os.TempDir(), "spire-wizard", wizardName)
	worktreeDir := filepath.Join(worktreeBase, beadID)
	rc := &spgit.RepoContext{Dir: repoPath, BaseBranch: baseBranch, Log: log}

	// Clean up any stale worktree at this path
	if _, err := os.Stat(worktreeDir); err == nil {
		rc.ForceRemoveWorktree(worktreeDir)
		os.RemoveAll(worktreeDir)
	}

	if err := os.MkdirAll(filepath.Dir(worktreeDir), 0755); err != nil {
		return nil, err
	}

	// Try creating worktree with new branch from base
	wc, err := rc.CreateWorktreeNewBranch(worktreeDir, branchName, baseBranch)
	if err != nil {
		// Branch may already exist (--review-fix path). Fetch and check out the existing branch.
		rc.Fetch("origin", branchName)
		wc, err = rc.CreateWorktree(worktreeDir, branchName)
		if err != nil {
			return nil, fmt.Errorf("git worktree add: %w", err)
		}
	}

	// Configure git user in worktree to the archmage identity so all commits
	// are attributed to the archmage on GitHub. The wizard name goes in
	// Co-Authored-By for traceability. Uses WorktreeContext.ConfigureUser which
	// scopes settings with --worktree so they don't pollute the main repo's config.
	archName, archEmail := wizardName, wizardName+"@spire.local" // fallback
	if tower, tErr := deps.ActiveTowerConfig(); tErr == nil && tower != nil {
		if tower.Archmage.Name != "" {
			archName = tower.Archmage.Name
		}
		if tower.Archmage.Email != "" {
			archEmail = tower.Archmage.Email
		}
	}
	wc.ConfigureUser(archName, archEmail)

	// Remove .beads/ from the worktree so the apprentice's test runs
	// and Claude's exploratory commands don't create real beads in the
	// production database. The apprentice doesn't need store access.
	os.RemoveAll(filepath.Join(wc.Dir, ".beads"))

	return wc, nil
}

// WizardExtractTitle extracts the title from bd show --json output.
// The output is a JSON array of bead objects.
func WizardExtractTitle(beadJSON string) string {
	var parsed []struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal([]byte(beadJSON), &parsed); err == nil && len(parsed) > 0 {
		return parsed[0].Title
	}
	return ""
}

// --- Prompt builders ---

// WizardBuildDesignPrompt builds the design phase prompt for the wizard.
func WizardBuildDesignPrompt(wizardName, beadID string, cfg *repoconfig.RepoConfig,
	focusContext, beadJSON string) string {

	contextPaths := cfg.Context
	if len(contextPaths) == 0 {
		contextPaths = []string{"CLAUDE.md", "SPIRE.md"}
	}
	var contextBlock strings.Builder
	for _, p := range contextPaths {
		fmt.Fprintf(&contextBlock, "- %s\n", p)
	}

	graphSuffix := promptctx.BuildPromptSuffix(beadID, promptctx.StoreDeps(), false)

	return fmt.Sprintf(`You are Spire autonomous wizard %s — DESIGN PHASE.

Task: bead %s

Read the task description and the repo context. Explore the relevant code.
Write a concise implementation plan. Cover:
- What files to change and why
- Key design decisions
- Edge cases or risks
- Rough ordering of changes

Do NOT write any code. Do NOT modify any files. Output your plan to stdout.

Repo context paths:
%s
Focus context:
%s

Bead JSON:
%s

%s`, wizardName, beadID, contextBlock.String(), focusContext, beadJSON, graphSuffix)
}

// WizardBuildImplementPrompt builds the implement phase prompt for the wizard.
func WizardBuildImplementPrompt(wizardName, beadID, branchName, baseBranch, model string,
	maxTurns int, timeout string, cfg *repoconfig.RepoConfig,
	focusContext, beadJSON, designPlan, reviewFeedback string) string {

	contextPaths := cfg.Context
	if len(contextPaths) == 0 {
		contextPaths = []string{"CLAUDE.md", "SPIRE.md"}
	}
	var contextBlock strings.Builder
	for _, p := range contextPaths {
		fmt.Fprintf(&contextBlock, "- %s\n", p)
	}

	optionalCmd := func(cmd string) string {
		if cmd == "" {
			return "(none)"
		}
		return cmd
	}

	var extra strings.Builder
	if designPlan != "" {
		fmt.Fprintf(&extra, "\nDesign plan (from design phase):\n%s\n", designPlan)
	}
	if reviewFeedback != "" {
		fmt.Fprintf(&extra, "\nAddress the following review feedback:\n%s\n", reviewFeedback)
	}

	graphSuffix := promptctx.BuildPromptSuffix(beadID, promptctx.StoreDeps(), false)

	return fmt.Sprintf(`You are Spire apprentice %s — IMPLEMENT PHASE.

You are working in an isolated git worktree. Other agents may be working on related tasks in parallel — you cannot see their changes.

## Task
- bead: %s
- base branch: %s
- feature branch: %s
- target model: %s
- max turns: %d
- hard timeout: %s

## Before making changes
1. Read the focus context below carefully — it contains the FULL task description.
2. Read the repo context paths below. If a path is a directory, inspect only relevant files.
3. Pay attention to "do not touch" constraints in the task description — other agents handle those files.

## Repo context paths
%s

## Validation commands
- install: %s
- lint: %s
- build: %s
- test: %s

## Rules — READ THESE

1. Work ONLY on the task described below. Do not add features, refactor unrelated code, or "improve" things outside your task scope.
2. Do NOT create a PR, merge, push, or touch other branches. The orchestrator handles that.
3. Do NOT modify files listed as "do_not_touch" in the task description — another agent handles those.
4. If the task description says to create types/interfaces that other tasks will use, make them complete and well-documented.
5. COMMIT YOUR WORK before running validation. Your code MUST build — the orchestrator will verify this and send you back to fix build errors if it doesn't. If tests fail on code you wrote, try to fix it. If tests fail on code you didn't write, IGNORE IT and commit anyway.
6. If you CANNOT complete the full task as described, commit what you have and report what's missing as a comment on the bead. Partial work committed is ALWAYS better than no work committed.
7. Do NOT revert your changes. Do NOT undo work you've done. If something isn't working, commit what you have — the reviewer will sort it out.

%s
## Focus context (FULL task description)
%s

## Bead JSON
%s

%s`, wizardName, beadID, baseBranch, branchName, model, maxTurns, timeout,
		contextBlock.String(),
		optionalCmd(cfg.Runtime.Install),
		optionalCmd(cfg.Runtime.Lint),
		optionalCmd(cfg.Runtime.Build),
		optionalCmd(cfg.Runtime.Test),
		extra.String(),
		focusContext, beadJSON, graphSuffix)
}

// WizardBuildCustomPrompt builds a prompt for a wizard.run step that uses an
// inline prompt from with.prompt in the formula. The inline prompt becomes the
// task-specific instructions, wrapped with standard Spire system context
// (identity, focus context, bead JSON, repo config).
func WizardBuildCustomPrompt(wizardName, beadID string, cfg *repoconfig.RepoConfig,
	focusContext, beadJSON, customPrompt string) string {

	optionalCmd := func(cmd string) string {
		if cmd == "" {
			return "(none)"
		}
		return cmd
	}

	contextPaths := cfg.Context
	if len(contextPaths) == 0 {
		contextPaths = []string{"CLAUDE.md", "SPIRE.md"}
	}
	var contextBlock strings.Builder
	for _, p := range contextPaths {
		fmt.Fprintf(&contextBlock, "- %s\n", p)
	}

	return fmt.Sprintf(`You are %s, a Spire agent working on bead %s.

## Context

%s

## Bead

%s

## Repo

- Build: %s
- Test: %s

## Repo context paths
%s
## Your task

%s
`, wizardName, beadID,
		focusContext,
		beadJSON,
		optionalCmd(cfg.Runtime.Build),
		optionalCmd(cfg.Runtime.Test),
		contextBlock.String(),
		customPrompt)
}

// WizardBuildClaudeArgs builds the common claude CLI arguments.
// maxTurns is passed as --max-turns to limit agent iterations.
// Timeout enforcement is handled by the caller via context.WithTimeout.
//
// Output format is stream-json so each turn (system init, tool_use,
// tool_result, assistant/user, final result) lands on disk for
// post-mortem. --include-partial-messages adds assistant deltas;
// --verbose is required for stream-json to actually emit intermediate
// events (omitting it silently degrades to result-only output).
func WizardBuildClaudeArgs(prompt, model string, maxTurns int) []string {
	args := []string{
		"--dangerously-skip-permissions",
		"-p", prompt,
		"--model", model,
		"--output-format", "stream-json",
		"--include-partial-messages",
		"--verbose",
	}
	// 0 means unlimited — omit the flag so Claude has no turn ceiling.
	// The timeout is the real gate.
	if maxTurns > 0 {
		args = append(args, "--max-turns", fmt.Sprintf("%d", maxTurns))
	}
	return args
}

// wizardConfiguredMaxTurns returns the configured Claude turn cap for wizard
// subprocesses. Zero is intentional and means timeout-gated/unlimited: do not
// synthesize a default cap here.
func wizardConfiguredMaxTurns(cfg *repoconfig.RepoConfig) int {
	if cfg == nil {
		return 0
	}
	return cfg.Agent.MaxTurns
}

// sanitizeClaudeLogLabel normalizes a semantic label into a filesystem-safe
// token. Mirrors pkg/executor/claude_runner.go so the board inspector's
// glob + sort treats wizard and executor logs consistently.
func sanitizeClaudeLogLabel(s string) string {
	r := strings.NewReplacer("/", "-", " ", "-", ":", "-")
	out := r.Replace(s)
	if out == "" {
		return "claude"
	}
	return out
}

// providerStream bundles the per-invocation stdout transcript and
// stderr sidecar produced by openProviderStreamLog.
type providerStream struct {
	stdoutPath string
	stderrPath string
	stdout     *os.File
	stderr     *os.File
}

// openClaudeProviderStream is the best-effort caller shim used by the
// Claude invocation helpers. It opens a transcript pair under
// <agentResultDir>/claude/, logs the stdout path to stderr so an
// operator can tail it, and writes the Claude-specific header preamble.
// Returns nil (tee disabled) when agentResultDir is empty or the
// transcript cannot be opened — a broken log dir must never block the
// claude invocation.
func openClaudeProviderStream(agentResultDir, label, worktreeDir string, args []string) *providerStream {
	if agentResultDir == "" {
		return nil
	}
	stream, err := openProviderStreamLog(agentResultDir, "claude", label)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: claude transcript capture: %v%s\n", err, runtime.LogFields(runtime.RunContextFromEnv()))
		return nil
	}
	fmt.Fprintf(os.Stderr, "[claude] invocation [%s] logging to %s%s\n", label, stream.stdoutPath, runtime.LogFields(runtime.RunContextFromEnv()))
	writeClaudeLogHeader(stream.stdout, label, worktreeDir, args)
	return stream
}

// openProviderStreamLog opens a pair of per-invocation transcript files
// for an AI-provider CLI spawn:
//
//   - stdout JSONL transcript: <agentResultDir>/<provider>/<label>-<ts>.jsonl
//   - stderr sidecar:          <agentResultDir>/<provider>/<label>-<ts>.stderr.log
//
// Both files are opened O_CREATE|O_WRONLY|O_APPEND with 0644. The
// containing directory is created with 0755.
//
// The provider must be non-empty — transcripts keyed under "" would
// collide with other providers' discovery globs. An empty
// agentResultDir is also rejected so callers opt into capture
// explicitly.
//
// If opening the stderr sidecar fails after stdout was opened, stdout
// is closed before the error is returned so no file handle leaks.
//
// Timestamp format matches pkg/executor/claude_runner.go so downstream
// readers (pkg/board/inspector.go, cmd/spire/logs.go) can treat
// wizard-produced and executor-produced transcripts uniformly.
func openProviderStreamLog(agentResultDir, provider, label string) (*providerStream, error) {
	if agentResultDir == "" {
		return nil, fmt.Errorf("openProviderStreamLog: empty agentResultDir")
	}
	if provider == "" {
		return nil, fmt.Errorf("openProviderStreamLog: empty provider")
	}
	dir := filepath.Join(agentResultDir, provider)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	ts := time.Now().UTC().Format("20060102-150405")
	baseStem := fmt.Sprintf("%s-%s", sanitizeClaudeLogLabel(label), ts)
	// Two spawns inside the same second (e.g. transient-cutoff retry with a
	// near-zero backoff in tests, or fast back-to-back invocations in
	// production) would collide on the second-resolution timestamp and
	// O_APPEND into the prior attempt's transcript. Probe with O_EXCL and
	// add a numeric suffix on collision so each attempt gets its own file.
	stem := baseStem
	var stdoutPath, stderrPath string
	var stdout *os.File
	for i := 1; ; i++ {
		stdoutPath = filepath.Join(dir, stem+".jsonl")
		f, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND|os.O_EXCL, 0o644)
		if err == nil {
			stdout = f
			stderrPath = filepath.Join(dir, stem+".stderr.log")
			break
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("open stdout transcript %s: %w", stdoutPath, err)
		}
		stem = fmt.Sprintf("%s-%d", baseStem, i+1)
	}
	stderr, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		stdout.Close()
		return nil, fmt.Errorf("open stderr sidecar %s: %w", stderrPath, err)
	}
	return &providerStream{
		stdoutPath: stdoutPath,
		stderrPath: stderrPath,
		stdout:     stdout,
		stderr:     stderr,
	}, nil
}

// WizardAgentResultDir returns the per-wizard directory under which
// claude stream logs live (<DoltGlobalDir>/wizards/<wizardName>). Empty
// if deps or DoltGlobalDir are nil.
func WizardAgentResultDir(deps *Deps, wizardName string) string {
	if deps == nil || deps.DoltGlobalDir == nil {
		return ""
	}
	return filepath.Join(deps.DoltGlobalDir(), "wizards", wizardName)
}

// WizardRunClaude invokes the claude CLI in print mode. Output is teed
// to stderr and, when agentResultDir is non-empty, to a per-invocation
// transcript pair under <agentResultDir>/claude/: a JSONL stdout
// transcript and a .stderr.log sidecar. Returns token usage metrics
// parsed from the stream's final result event. timeout enforces a hard
// process-level deadline via context.
//
// When the wizard's cached AuthContext is configured for subscription and
// the api-key slot is populated, a 429 from claude triggers an in-memory
// subscription→api-key swap and a single retry (spi-mdxtww). The swap is
// sticky for subsequent calls in the same wizard process.
//
// On a non-zero exit that looks like a transport-layer stream interruption
// (no `result` event, no 429, no `max_turns`; see agent.TransientStreamCutoff),
// the spawn is retried up to maxTransientRetries times with a
// transientRetryBackoff schedule between attempts. Each attempt opens its
// own timestamped JSONL transcript so the post-mortem trail is preserved.
func WizardRunClaude(worktreeDir, promptPath, model, timeout string, maxTurns int, agentResultDir, label string) (ClaudeMetrics, error) {
	promptBytes, err := os.ReadFile(promptPath)
	if err != nil {
		return ClaudeMetrics{}, fmt.Errorf("read prompt: %w", err)
	}

	// Tool metrics are now collected via the OTel pipeline (daemon OTLP receiver).
	// The OTEL_EXPORTER_OTLP_ENDPOINT env var is injected at spawn time.

	args := WizardBuildClaudeArgs(string(promptBytes), model, maxTurns)

	dur, parseErr := time.ParseDuration(timeout)
	if parseErr != nil {
		dur = 15 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()

	out, runErr := runWizardClaudeWithRetry(ctx, args, worktreeDir, agentResultDir, label)
	_, metrics := parseClaudeResultJSON(out)
	metrics.MaxTurns = maxTurns
	metrics.AuthProfileFinal = wizardPromotionSlot()
	return metrics, runErr
}

// WizardRunClaudeCapture invokes the claude CLI and captures the text result.
// Returns the result text extracted from the stream's final result event,
// token usage metrics, and any execution error. Falls back to raw output if
// parsing fails. timeout enforces a hard process-level deadline via context.
// When agentResultDir is non-empty, the full stream is also teed to
// <agentResultDir>/claude/<label>-<ts>.jsonl, with a sidecar
// <label>-<ts>.stderr.log for stderr.
//
// Same 429 auto-promote and transient-cutoff retry behavior as WizardRunClaude.
func WizardRunClaudeCapture(worktreeDir, promptPath, model, timeout string, maxTurns int, agentResultDir, label string) (string, ClaudeMetrics, error) {
	promptBytes, err := os.ReadFile(promptPath)
	if err != nil {
		return "", ClaudeMetrics{}, fmt.Errorf("read prompt: %w", err)
	}

	// Tool metrics are now collected via the OTel pipeline (daemon OTLP receiver).

	args := WizardBuildClaudeArgs(string(promptBytes), model, maxTurns)

	dur, parseErr := time.ParseDuration(timeout)
	if parseErr != nil {
		dur = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()

	out, runErr := runWizardClaudeWithRetry(ctx, args, worktreeDir, agentResultDir, label)
	resultText, metrics := parseClaudeResultJSON(out)
	metrics.MaxTurns = maxTurns
	metrics.AuthProfileFinal = wizardPromotionSlot()
	// Fall back to raw output if parsing didn't find a result event
	if resultText == "" {
		resultText = string(out)
	}

	return resultText, metrics, runErr
}

// wizardClaudeCLIRunner is the injected claude-invocation shim. Defaults
// to agent.DefaultClaudeCLIRunner; tests override it to stub the CLI.
var wizardClaudeCLIRunner = agent.DefaultClaudeCLIRunner

// runWizardClaudeInvoke centralizes the auth-aware claude invocation used
// by WizardRunClaude and WizardRunClaudeCapture. It pulls the current
// AuthContext from the wizard-scoped cache, runs the CLI, and on a 429
// with auto-promote preconditions met, swaps to the api-key slot and
// retries once. Promotes are recorded in the package-level cache so
// future calls in the same wizard process see the sticky slot and
// WizardWriteResult surfaces auth_profile_final.
func runWizardClaudeInvoke(ctx context.Context, args []string, dir string, stdoutTee, stderrTee io.Writer, label string) agent.ClaudeInvokeResult {
	auth := currentWizardAuth()
	beadID := os.Getenv("SPIRE_BEAD_ID")
	res := agent.InvokeClaudeWithAuth(agent.ClaudeInvokeParams{
		Ctx:     ctx,
		Args:    args,
		Dir:     dir,
		BaseEnv: os.Environ(),
		Auth:    auth,
		Stdout:  stdoutTee,
		Stderr:  stderrTee,
		BeadID:  beadID,
		Step:    label,
		Log: func(format string, a ...interface{}) {
			fmt.Fprintf(os.Stderr, "[wizard] "+format+"%s\n",
				append(a, runtime.LogFields(runtime.RunContextFromEnv()))...)
		},
		Runner: wizardClaudeCLIRunner,
	})
	if res.Promoted && auth != nil {
		recordWizardPromotion(auth.SlotName())
	}
	return res
}

// maxTransientRetries caps how many additional spawn attempts we'll make
// after a transient stream cutoff is detected (so total attempts =
// 1 + maxTransientRetries). Tuned from production observations on
// spi-skfsia and siblings: a single retry usually clears the network
// blip; two is enough to ride through the rare double-cut.
const maxTransientRetries = 2

// transientRetryBackoffs are the per-attempt sleep windows between
// retries. attempt index 0 is the wait BEFORE the second spawn,
// index 1 is the wait BEFORE the third spawn. Length must equal
// maxTransientRetries.
var transientRetryBackoffs = []time.Duration{
	5 * time.Second,
	30 * time.Second,
}

// transientRetryBackoff returns the sleep before the (1+i)-th retry. For
// indexes outside the configured schedule it returns the last entry — a
// safety net that should never fire because the caller bounds attempts by
// maxTransientRetries.
func transientRetryBackoff(i int) time.Duration {
	if i < 0 {
		i = 0
	}
	if i >= len(transientRetryBackoffs) {
		return transientRetryBackoffs[len(transientRetryBackoffs)-1]
	}
	return transientRetryBackoffs[i]
}

// runWizardClaudeWithRetry orchestrates one or more claude CLI invocations
// for a single phase prompt. Each attempt opens its own timestamped JSONL
// transcript via openClaudeProviderStream so per-attempt post-mortems are
// preserved. After every attempt, the outcome is classified in this order:
//
//  1. Is429Response — the inner 429 auto-promote already retried once
//     inside InvokeClaudeWithAuth; treat its final result as authoritative,
//     no outer retry.
//  2. IsMaxTurns — genuine budget exhaustion, no retry.
//  3. TransientStreamCutoff with attempts remaining — sleep the configured
//     backoff and respawn with the same args/worktree/auth.
//  4. anything else (success or non-transient error) — return as-is.
//
// Returns the stdout bytes and exit error of the FINAL attempt.
func runWizardClaudeWithRetry(ctx context.Context, args []string, worktreeDir, agentResultDir, label string) ([]byte, error) {
	beadID := os.Getenv("SPIRE_BEAD_ID")
	for attempt := 0; ; attempt++ {
		out, runErr := runWizardClaudeOnce(ctx, args, worktreeDir, agentResultDir, label)

		if runErr == nil {
			return out, nil
		}
		if agent.Is429Response(out, runErr) {
			return out, runErr
		}
		if agent.IsMaxTurns(out) {
			return out, runErr
		}
		if !agent.TransientStreamCutoff(out, runErr) {
			return out, runErr
		}
		if attempt >= maxTransientRetries {
			fmt.Fprintf(os.Stderr,
				"[claude] transient stream cut on bead %s, retries exhausted (%d/%d)%s\n",
				beadID, attempt+1, maxTransientRetries+1,
				runtime.LogFields(runtime.RunContextFromEnv()))
			return out, runErr
		}
		backoff := transientRetryBackoff(attempt)
		fmt.Fprintf(os.Stderr,
			"[claude] transient stream cut on bead %s, retrying (attempt %d/%d) after %s%s\n",
			beadID, attempt+2, maxTransientRetries+1, backoff,
			runtime.LogFields(runtime.RunContextFromEnv()))
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return out, ctx.Err()
		}
	}
}

// runWizardClaudeOnce performs a single claude CLI spawn and returns the
// captured stdout plus the spawn error. A fresh transcript pair is opened
// per call so retries don't clobber the prior attempt's JSONL — the
// timestamp in openProviderStreamLog's filename guarantees uniqueness.
func runWizardClaudeOnce(ctx context.Context, args []string, worktreeDir, agentResultDir, label string) ([]byte, error) {
	stream := openClaudeProviderStream(agentResultDir, label, worktreeDir, args)
	if stream != nil {
		defer stream.stdout.Close()
		defer stream.stderr.Close()
	}
	var buf bytes.Buffer
	stdoutTee := io.Writer(&buf)
	if stream != nil {
		stdoutTee = io.MultiWriter(&buf, stream.stdout)
	}
	var stderrTee io.Writer = os.Stderr
	if stream != nil {
		stderrTee = io.MultiWriter(os.Stderr, stream.stderr)
	}
	started := time.Now()
	res := runWizardClaudeInvoke(ctx, args, worktreeDir, stdoutTee, stderrTee, label)
	if stream != nil {
		fmt.Fprintf(stream.stdout, "\n=== end (err=%v, duration=%s) ===\n", res.Err, time.Since(started))
	}
	return buf.Bytes(), res.Err
}

// writeClaudeLogHeader writes an identifying preamble to the claude
// log file so an operator tailing it mid-run sees what invocation they
// are watching. Mirrors the executor's header format.
func writeClaudeLogHeader(f *os.File, label, worktreeDir string, args []string) {
	fmt.Fprintf(f, "=== claude invocation ===\n")
	fmt.Fprintf(f, "label: %s\n", label)
	fmt.Fprintf(f, "dir:   %s\n", worktreeDir)
	fmt.Fprintf(f, "time:  %s\n", time.Now().UTC().Format(time.RFC3339))
	// Skip args: the prompt (-p) is enormous and not useful in the header.
	_ = args
	fmt.Fprintln(f)
	fmt.Fprintf(f, "=== stream ===\n")
}

// WizardRunCmd runs a shell command in the given directory.
func WizardRunCmd(dir, command string) error {
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// WizardRunCmdCapture runs a shell command and returns combined stdout+stderr.
func WizardRunCmdCapture(dir, command string) (string, error) {
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// WizardValidate runs lint, build, and test commands from spire.yaml.
func WizardValidate(dir string, cfg *repoconfig.RepoConfig, log func(string, ...interface{})) bool {
	if cfg.Runtime.Lint != "" {
		log("validating: lint")
		if err := WizardRunCmd(dir, cfg.Runtime.Lint); err != nil {
			log("lint failed: %s", err)
			return false
		}
	}
	if cfg.Runtime.Build != "" {
		log("validating: build")
		if err := WizardRunCmd(dir, cfg.Runtime.Build); err != nil {
			log("build failed: %s", err)
			return false
		}
	}
	if cfg.Runtime.Test != "" {
		log("validating: test")
		if err := WizardRunCmd(dir, cfg.Runtime.Test); err != nil {
			log("test failed: %s", err)
			return false
		}
	}
	return true
}

// DefaultMaxBuildFixRounds is the default number of build-fix attempts before giving up.
const DefaultMaxBuildFixRounds = 2

// BuildRunFunc runs a build command in a directory and returns output + error.
type BuildRunFunc func(dir, cmd string) (string, error)

// AgentRunFunc invokes a Claude agent and returns metrics. The
// agentResultDir + label arguments select where the per-invocation
// stream log is written; empty agentResultDir disables the log tee.
type AgentRunFunc func(dir, promptPath, model, timeout string, maxTurns int, agentResultDir, label string) (ClaudeMetrics, error)

// WizardBuildGate runs the build command and, on failure, enters a fix loop:
// invoke Claude with the build error, re-commit, re-build, up to maxRounds.
// Returns true if the build passes (immediately or after fixes). When
// agentResultDir is non-empty, each fix invocation's stream is captured
// to <agentResultDir>/claude/build-gate-fix-r<N>-<ts>.jsonl (plus a
// matching .stderr.log sidecar).
func WizardBuildGate(wc *spgit.WorktreeContext, beadID, beadTitle, worktreeDir, model string,
	cfg *repoconfig.RepoConfig, accMetrics *ClaudeMetrics, agentResultDir string, log func(string, ...interface{})) bool {
	return wizardBuildGateImpl(wc, beadID, beadTitle, worktreeDir, model, cfg, accMetrics, agentResultDir, log,
		WizardRunCmdCapture, WizardRunClaude)
}

// wizardBuildGateImpl is the testable implementation of WizardBuildGate.
func wizardBuildGateImpl(wc *spgit.WorktreeContext, beadID, beadTitle, worktreeDir, model string,
	cfg *repoconfig.RepoConfig, accMetrics *ClaudeMetrics, agentResultDir string, log func(string, ...interface{}),
	runBuild BuildRunFunc, runAgent AgentRunFunc) bool {

	buildCmd := cfg.Runtime.Build
	if buildCmd == "" {
		log("build-gate: no build command configured — skipping")
		return true
	}

	// Initial build check.
	log("build-gate: running build")
	buildOut, buildErr := runBuild(worktreeDir, buildCmd)
	if buildErr == nil {
		log("build-gate: build passed")
		return true
	}
	log("build-gate: build failed:\n%s", buildOut)

	maxRounds := DefaultMaxBuildFixRounds
	if model == "" {
		model = "claude-sonnet-4-6"
	}
	maxTurns := wizardConfiguredMaxTurns(cfg)
	buildFixTimeout := "5m"

	for round := 1; round <= maxRounds; round++ {
		log("build-gate: fix round %d/%d", round, maxRounds)

		// Build a focused prompt with the error output.
		prompt := wizardBuildGateFixPrompt(beadID, buildCmd, buildOut, cfg)
		promptPath := filepath.Join(worktreeDir, ".spire-build-fix-prompt.txt")
		if err := os.WriteFile(promptPath, []byte(prompt), 0644); err != nil {
			log("build-gate: failed to write prompt: %s", err)
			return false
		}

		// Invoke Claude to fix. Label includes the round so each attempt
		// writes to its own file (not strictly necessary — timestamps
		// already disambiguate — but makes post-mortem scanning easier).
		fixLabel := fmt.Sprintf("build-gate-fix-r%d", round)
		fixMetrics, runErr := runAgent(worktreeDir, promptPath, model, buildFixTimeout, maxTurns, agentResultDir, fixLabel)
		if accMetrics != nil {
			*accMetrics = accMetrics.Add(fixMetrics)
		}
		os.Remove(promptPath)
		if runErr != nil {
			log("build-gate: claude fix failed: %s", runErr)
		}

		// Commit whatever Claude produced.
		commitSHA, committed := WizardCommit(wc, beadID, beadTitle, log)
		if committed {
			log("build-gate: fix committed: %s", commitSHA)
		}

		// Re-run build.
		log("build-gate: re-running build")
		buildOut, buildErr = runBuild(worktreeDir, buildCmd)
		if buildErr == nil {
			log("build-gate: build passed after fix round %d", round)
			return true
		}
		log("build-gate: still failing:\n%s", buildOut)
	}

	log("build-gate: exhausted %d fix rounds — build still fails", maxRounds)
	return false
}

// wizardBuildGateFixPrompt builds a focused prompt for fixing build errors
// in the apprentice's own worktree (not staging).
func wizardBuildGateFixPrompt(beadID, buildCmd, buildErr string, cfg *repoconfig.RepoConfig) string {
	contextPaths := cfg.Context
	if len(contextPaths) == 0 {
		contextPaths = []string{"CLAUDE.md"}
	}
	var contextBlock strings.Builder
	for _, p := range contextPaths {
		fmt.Fprintf(&contextBlock, "- %s\n", p)
	}

	buildCmdStr := buildCmd
	if buildCmdStr == "" {
		buildCmdStr = "go build ./..."
	}

	return fmt.Sprintf(`You are a Spire build-fix agent.

Your code does not build. Fix the build errors below.

## Build command
%s

## Build error
`+"```"+`
%s
`+"```"+`

## Instructions
1. Read the build error output above carefully.
2. Identify the source files causing the errors.
3. Fix the compilation errors.
4. Run the build command to verify your fix: %s
5. COMMIT your changes with message: fix(%s): resolve build errors

## Rules
- Fix ONLY build errors. Do NOT refactor, improve, or change any other code.
- Do NOT revert prior work — fix forward.
- Do NOT create new files unless absolutely necessary to resolve the error.
- Keep changes minimal — the smallest diff that makes the build pass.

## Repo context paths
%s`, buildCmdStr, buildErr, buildCmdStr, beadID, contextBlock.String())
}

// reviewFixFailureGuard returns a non-nil error when a review-fix
// apprentice's claude subprocess exited non-zero (signal kill, timeout,
// API error, etc.) AND nothing was staged. A no-op fix must NOT be
// treated as success: the graph interpreter would fire the formula's
// `resets` directive, re-run sage review against unchanged code, and
// potentially flip an earlier request_changes into an approve — making
// the whole review loop a no-op. Returning an error routes the step to
// failed, which (per max_review_rounds + steps.arbiter.when) escalates
// to the arbiter instead of looping.
//
// A successful subprocess that produced zero changes is a legitimate
// edge case (e.g. the issue was already fixed before the wizard ran)
// and is NOT caught here.
//
// Callers must NOT write result.json when this returns an error: the
// executor's wizard.run dispatcher trusts result.json over the
// subprocess exit status (pkg/executor/graph_actions.go wizardRunSpawn),
// so writing a result would downgrade this failure to "completed" and
// the resets would still fire.
func reviewFixFailureGuard(reviewFix, committed bool, subprocessErr error) error {
	if !reviewFix || committed || subprocessErr == nil {
		return nil
	}
	return fmt.Errorf("review-fix apprentice failed with no staged changes: %w", subprocessErr)
}

// WizardCommit commits any changes on the branch.
// Apprentices never push feature branches to origin — the wizard merges
// locally from the worktree (local mode) or shared PVC (k8s mode).
// Only the final main merge touches origin.
func WizardCommit(wc *spgit.WorktreeContext, beadID, beadTitle string, log func(string, ...interface{})) (commitSHA string, committed bool) {
	hasUncommitted := wc.HasUncommittedChanges()

	// Use session-scoped commit detection (StartSHA..HEAD) when available,
	// falling back to BaseBranch..HEAD for legacy callers.
	hasNewCommits, err := wc.HasNewCommitsSinceStart()
	if err != nil {
		// Never convert comparison errors into "Claude already committed."
		// A comparison error means we cannot determine commit state — report
		// it as no commits, not as success.
		log("warning: could not check for new commits: %s — treating as no commits", err)
		hasNewCommits = false
	}

	if !hasUncommitted && !hasNewCommits {
		log("no changes to commit and no new commits on branch")
		return "", false
	}

	// If Claude already committed, report success.
	if !hasUncommitted && hasNewCommits {
		sha, _ := wc.HeadSHA()
		log("Claude already committed on branch %s", wc.Branch)
		recordCommitMetadata(beadID, sha, log)
		return sha, true
	}

	// Commit (stages all, removes prompt files)
	title := beadTitle
	if title == "" {
		title = "implement task"
	}
	if len(title) > 0 {
		title = strings.ToLower(title[:1]) + title[1:]
	}
	title = strings.TrimRight(title, ".")
	msg := fmt.Sprintf("feat(%s): %s", beadID, title)

	sha, err := wc.Commit(msg, ".spire-prompt.txt", ".spire-design-prompt.txt")
	if err != nil {
		log("git commit failed: %s", err)
		return "", false
	}
	if sha == "" {
		// Fallback: Claude may have committed real changes while leftover
		// prompt files (now cleaned) were the only uncommitted content.
		// Re-check using session-scoped detection after the clean attempt.
		if fallback, ferr := wc.HasNewCommitsSinceStart(); ferr == nil && fallback {
			fsha, herr := wc.HeadSHA()
			if herr != nil {
				log("nothing staged and could not read HEAD: %s", herr)
				return "", false
			}
			log("nothing staged, but Claude committed on branch %s — using existing commit", wc.Branch)
			recordCommitMetadata(beadID, fsha, log)
			return fsha, true
		}
		log("nothing staged after git add")
		return "", false
	}

	recordCommitMetadata(beadID, sha, log)
	return sha, true
}

// recordCommitMetadata appends a commit SHA to the bead's metadata.commits
// list. Best-effort: logs a warning on failure but never blocks the commit flow.
func recordCommitMetadata(beadID, sha string, log func(string, ...interface{})) {
	if beadID == "" || sha == "" {
		return
	}
	if err := store.AppendBeadMetadataList(beadID, "commits", sha); err != nil {
		log("warning: failed to record commit metadata: %v", err)
	}
}

// wizardUpdateBead adds a comment to the bead. Labels are managed by WizardReviewHandoff.
func wizardUpdateBead(beadID, wizardName, branchName, commitSHA string, committed, testsPassed bool, deps *Deps, log func(string, ...interface{})) {
	if !committed {
		note := fmt.Sprintf("Wizard %s finished without changes", wizardName)
		deps.AddComment(beadID, note)
		return
	}

	note := fmt.Sprintf("Wizard %s committed branch %s", wizardName, branchName)
	if commitSHA != "" {
		note += fmt.Sprintf(" @ %s", commitSHA[:min(len(commitSHA), 8)])
	}
	if !testsPassed {
		note += " (tests failed)"
	}
	deps.AddComment(beadID, note)
}

// WizardWriteResult writes a result JSON file for observability.
// Includes token usage metrics when available (non-zero).
func WizardWriteResult(wizardName, beadID, result, branchName, commitSHA string,
	elapsed time.Duration, metrics ClaudeMetrics, deps *Deps, log func(string, ...interface{})) {

	resultDir := filepath.Join(deps.DoltGlobalDir(), "wizards", wizardName)
	os.MkdirAll(resultDir, 0755)

	data := map[string]interface{}{
		"wizard":             wizardName,
		"bead_id":            beadID,
		"result":             result,
		"branch":             branchName,
		"commit":             commitSHA,
		"elapsed_s":          int(elapsed.Seconds()),
		"completed":          time.Now().UTC().Format(time.RFC3339),
		"context_tokens_in":  metrics.InputTokens,
		"context_tokens_out": metrics.OutputTokens,
		"total_tokens":       metrics.TotalTokens,
		"turns":              metrics.Turns,
		"max_turns":          metrics.MaxTurns,
		"stop_reason":        metrics.StopReason,
		"subtype":            metrics.Subtype,
		"is_error":           metrics.IsError,
		"terminal_reason":    metrics.TerminalReason,
		"api_error_status":   metrics.APIErrorStatus,
		"cache_read_tokens":  metrics.CacheReadTokens,
		"cache_write_tokens": metrics.CacheWriteTokens,
		"cost_usd":           metrics.CostUSD,
	}
	if len(metrics.ToolCalls) > 0 {
		data["tool_calls"] = metrics.ToolCalls
	}
	// Prefer metrics.AuthProfileFinal when callers set it explicitly; fall
	// back to the wizard's package-level cache so even a result produced
	// without the 429-aware Claude helpers (e.g. legacy sage review path)
	// still surfaces any promote that happened earlier in the run.
	finalSlot := metrics.AuthProfileFinal
	if finalSlot == "" {
		finalSlot = wizardPromotionSlot()
	}
	if finalSlot != "" {
		data["auth_profile_final"] = finalSlot
	}
	out, _ := json.MarshalIndent(data, "", "  ")
	resultPath := filepath.Join(resultDir, "result.json")
	if err := os.WriteFile(resultPath, append(out, '\n'), 0644); err != nil {
		log("warning: could not write result: %s", err)
	}
}

// WizardCleanup removes the git worktree.
// WizardCleanup is a legacy wrapper kept for any call sites that haven't
// migrated to WorktreeContext.Cleanup() yet.
func WizardCleanup(worktreeDir, repoPath string) {
	wc := spgit.WorktreeContext{Dir: worktreeDir, RepoPath: repoPath}
	wc.Cleanup()
}

// CaptureWizardFocus runs `spire focus <bead-id>` and captures stdout.
func CaptureWizardFocus(beadID string) (string, error) {
	cmd := exec.Command(os.Args[0], "focus", beadID)
	cmd.Env = os.Environ()
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// --- Feedback collection ---

// WizardCollectFeedback collects review feedback messages addressed to this wizard for a bead.
func WizardCollectFeedback(beadID, wizardName string, deps *Deps) string {
	messages, err := deps.ListBeads(beads.IssueFilter{
		IDPrefix: "spi-",
		Labels:   []string{"msg", "to:" + wizardName, "ref:" + beadID},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: collect feedback: %s%s\n", err, runtime.LogFields(runtime.RunContextFromEnv()))
		return ""
	}

	var parts []string
	for _, m := range messages {
		parts = append(parts, m.Description)
		// Close consumed message
		deps.CloseBead(m.ID)
	}
	return strings.Join(parts, "\n---\n")
}

// WizardCollectReviewHistory collects structured review history from review-round beads,
// falling back to message-based feedback if no review beads exist.
func WizardCollectReviewHistory(beadID, wizardName string, deps *Deps) string {
	reviewBeads, err := deps.GetReviewBeads(beadID)
	if err == nil && len(reviewBeads) > 0 {
		var buf strings.Builder
		buf.WriteString("## Prior Review Rounds\n\n")
		for _, rb := range reviewBeads {
			roundNum := deps.ReviewRoundNumber(rb)
			sage := deps.HasLabel(rb, "sage:")
			buf.WriteString(fmt.Sprintf("### Round %d (sage: %s, status: %s)\n", roundNum, sage, rb.Status))
			if rb.Description != "" {
				buf.WriteString(rb.Description)
				buf.WriteString("\n")
			}
			buf.WriteString("\n")
		}
		// Also collect any message-based feedback (in case of hybrid state)
		msgFeedback := WizardCollectFeedback(beadID, wizardName, deps)
		if msgFeedback != "" {
			buf.WriteString("## Latest Feedback Message\n")
			buf.WriteString(msgFeedback)
		}
		return buf.String()
	}
	// Fall back to message-based feedback
	return WizardCollectFeedback(beadID, wizardName, deps)
}

// --- Review handoff ---

// WizardReviewHandoff spawns a reviewer process for a bead.
// On spawn failure, the steward's detectReviewReady() will detect the bead
// needs review via its closed implement step bead and re-route on the next cycle.
//
// Registry lifecycle: backend.Spawn is the sole creator of the reviewer's
// registry entry — see pkg/agent/README.md "Registry lifecycle". This function
// must not pre-register the reviewer; doing so would dual-write with the
// backend's Add and risk skew if the spawn fails partway through the contract
// population below.
func WizardReviewHandoff(beadID, wizardName, branchName string, deps *Deps, log func(string, ...interface{})) {
	deps.AddLabel(beadID, "feat-branch:"+branchName)

	reviewerName := wizardName + "-review"

	// Resolve tower identity and repo context so the runtime-contract
	// fields on SpawnConfig are populated (required by cluster backends
	// at buildSubstratePod — see pkg/agent/backend_k8s.go
	// ErrIdentityRequired/ErrWorkspaceRequired). A best-effort lookup:
	// missing tower config (local-dev, unit tests) leaves TowerName
	// empty and PopulateRuntimeContract fills workspace defaults so
	// ProcessBackend still works unchanged.
	var towerName string
	if tower, tErr := deps.ActiveTowerConfig(); tErr == nil && tower != nil {
		towerName = tower.Name
	}
	repoPath, repoURL, baseBranch, rErr := deps.ResolveRepo(beadID)
	if rErr != nil {
		log("resolve repo for review handoff: %s — continuing with empty repo identity", rErr)
	}
	if bb := findBaseBranchInParentChain(beadID, deps); bb != "" {
		baseBranch = bb
	}

	sc := SpawnConfig{
		Name:       reviewerName,
		BeadID:     beadID,
		Role:       RoleSage,
		Tower:      towerName,
		InstanceID: config.InstanceID(),
	}

	// Sage review is a same-owner read of the implement workspace, so
	// HandoffBorrowed is the correct delivery semantic — matches the
	// executor's wizardRunSpawn default for the sage-review flow.
	sc, contractErr := executor.PopulateRuntimeContract(sc, executor.RuntimeContractInputs{
		TowerName:   towerName,
		RepoURL:     repoURL,
		RepoPath:    repoPath,
		BaseBranch:  baseBranch,
		RunStep:     "review",
		Backend:     agent.ResolveBackendName(repoPath),
		HandoffMode: executor.HandoffBorrowed,
		Log:         log,
	})
	if contractErr != nil {
		log("failed to populate runtime contract for review handoff: %s", contractErr)
		deps.RegistryRemove(reviewerName)
		deps.AddComment(beadID, fmt.Sprintf("Review handoff runtime contract: %s", contractErr))
		return
	}

	// Spawn reviewer
	backend := deps.ResolveBackend("")
	handle, spawnErr := backend.Spawn(sc)
	if spawnErr != nil {
		log("failed to spawn reviewer: %s — steward will detect via review beads", spawnErr)
		// Remove the dead registry entry; the steward's detectReviewReady()
		// will detect the bead needs review via its closed implement step bead.
		deps.RegistryRemove(reviewerName)
		deps.AddComment(beadID, fmt.Sprintf("Local review spawn failed: %s — steward will re-route", spawnErr))
		return
	}

	// Update registry with the identifier now that spawn succeeded.
	if id := handle.Identifier(); id != "" {
		if pid, err := strconv.Atoi(id); err == nil {
			deps.RegistryUpdate(reviewerName, func(w *Entry) {
				w.PID = pid
			})
		}
	}

	log("review handoff complete, spawned %s (%s)", reviewerName, handle.Identifier())
	// Self-unregister happens via defer in CmdWizardRun
}

// findBaseBranchInParentChain walks up the bead's parent chain looking for a
// base-branch: label. Returns the branch name from the first bead that has one,
// or "" if none in the chain do. This lets child tasks inherit the base branch
// from their epic without needing the label copied to every child.
func findBaseBranchInParentChain(beadID string, deps *Deps) string {
	visited := make(map[string]bool)
	current := beadID
	for current != "" && !visited[current] {
		visited[current] = true
		bead, err := deps.GetBead(current)
		if err != nil {
			break
		}
		if bb := deps.HasLabel(bead, "base-branch:"); bb != "" {
			return bb
		}
		current = bead.Parent
	}
	return ""
}
