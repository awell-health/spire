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
	"syscall"
	"time"

	spgit "github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/steveyegge/beads"
)

// ClaudeMetrics captures token usage and cost from a Claude CLI invocation.
type ClaudeMetrics struct {
	InputTokens  int
	OutputTokens int
	TotalTokens  int
	Turns        int
	CostUSD      float64
}

// Add returns the sum of two ClaudeMetrics values.
func (m ClaudeMetrics) Add(other ClaudeMetrics) ClaudeMetrics {
	return ClaudeMetrics{
		InputTokens:  m.InputTokens + other.InputTokens,
		OutputTokens: m.OutputTokens + other.OutputTokens,
		TotalTokens:  m.TotalTokens + other.TotalTokens,
		Turns:        m.Turns + other.Turns,
		CostUSD:      m.CostUSD + other.CostUSD,
	}
}

// parseClaudeResultJSON scans Claude CLI JSON output for the result event
// and extracts the text result and usage metrics. Returns zero metrics on
// any parse failure (best effort, never errors).
func parseClaudeResultJSON(output []byte) (resultText string, metrics ClaudeMetrics) {
	lines := bytes.Split(output, []byte("\n"))
	// Scan in reverse — the result event is typically the last line.
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		if !bytes.Contains(line, []byte(`"type"`)) {
			continue
		}
		var evt struct {
			Type         string  `json:"type"`
			Result       string  `json:"result"`
			NumTurns     int     `json:"num_turns"`
			TotalCostUSD float64 `json:"total_cost_usd"`
			Usage        struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(line, &evt); err != nil {
			continue
		}
		if evt.Type == "result" {
			resultText = evt.Result
			metrics = ClaudeMetrics{
				InputTokens:  evt.Usage.InputTokens,
				OutputTokens: evt.Usage.OutputTokens,
				TotalTokens:  evt.Usage.InputTokens + evt.Usage.OutputTokens,
				Turns:        evt.NumTurns,
				CostUSD:      evt.TotalCostUSD,
			}
			return
		}
	}
	return
}

// CmdWizardRun is the internal entry point for a local wizard process.
// It claims a bead, creates a worktree, runs design + implement phases,
// validates, commits, updates the bead, and hands off to review.
//
// Usage: spire wizard-run <bead-id> [--name <wizard-name>] [--review-fix] [--apprentice] [--build-fix] [--worktree-dir <path>]
func CmdWizardRun(args []string, deps *Deps) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: spire wizard-run <bead-id> [--name <name>] [--review-fix] [--apprentice] [--build-fix] [--worktree-dir <path>] [--start-ref <ref>]")
	}

	// 1. Parse args
	beadID := args[0]
	wizardName := "wizard"
	reviewFix := false
	apprenticeMode := false
	buildFixMode := false
	worktreeDirOverride := ""
	startRef := ""
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
		}
	}
	if os.Getenv("SPIRE_APPRENTICE") == "1" {
		apprenticeMode = true
	}

	startedAt := time.Now()
	log := func(format string, a ...interface{}) {
		fmt.Fprintf(os.Stderr, "[%s] %s\n", wizardName, fmt.Sprintf(format, a...))
	}

	// --- Build-fix mode: early return path ---
	// The executor spawns the apprentice with --build-fix --apprentice --worktree-dir <path>.
	// The apprentice works directly in the existing staging worktree to fix build errors.
	if buildFixMode {
		return cmdBuildFix(beadID, wizardName, worktreeDirOverride, startedAt, deps, log)
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
	maxTurns := repoCfg.Agent.MaxTurns
	if maxTurns == 0 {
		maxTurns = 75
	}
	designTimeout := repoconfig.ResolveDesignTimeout(repoCfg.Agent.DesignTimeout)
	branchPattern := repoconfig.ResolveBranchPattern(repoCfg.Branch.Pattern)
	if repoCfg.Branch.Base != "" && baseBranch == "" {
		baseBranch = repoCfg.Branch.Base
	}

	// Bead-level base-branch override (from spire file --branch) takes
	// precedence over repo config and tower defaults.
	if bead, berr := deps.GetBead(beadID); berr == nil {
		if bb := deps.HasLabel(bead, "base-branch:"); bb != "" {
			log("using bead base-branch override: %s (was: %s)", bb, baseBranch)
			baseBranch = bb
		}
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

	// 4. Self-register in wizards.json
	now := time.Now().UTC().Format(time.RFC3339)
	deps.RegistryAdd(Entry{
		Name:           wizardName,
		PID:            os.Getpid(),
		BeadID:         beadID,
		Worktree:       worktreeDir,
		StartedAt:      now,
		Phase:          "init",
		PhaseStartedAt: now,
	})
	defer deps.RegistryRemove(wizardName)

	// Signal handler for clean unregister on interrupt
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		deps.RegistryRemove(wizardName)
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

	// 8-9. Phase execution
	var accMetrics ClaudeMetrics
	if reviewFix {
		// --review-fix path: skip design, collect feedback, implement with feedback
		feedback := WizardCollectReviewHistory(beadID, wizardName, deps)

		// Update phase
		deps.RegistryUpdate(wizardName, func(w *Entry) {
			w.Phase = "implement"
			w.PhaseStartedAt = time.Now().UTC().Format(time.RFC3339)
		})

		// Build implement prompt with feedback
		implPrompt := WizardBuildImplementPrompt(wizardName, beadID, branchName, baseBranch,
			model, maxTurns, timeout, repoCfg, focusContext, beadJSON, "", feedback)
		implPromptPath := filepath.Join(worktreeDir, ".spire-prompt.txt")
		if err := os.WriteFile(implPromptPath, []byte(implPrompt), 0644); err != nil {
			return fmt.Errorf("write implement prompt: %w", err)
		}

		reviewFixTimeout := designTimeout // spec: review-fix gets 10m, not 15m
		claudeStartedAt := time.Now()
		log("starting implement phase with review feedback (timeout: %s)", reviewFixTimeout)
		metrics, runErr := WizardRunClaude(worktreeDir, implPromptPath, model, reviewFixTimeout, maxTurns)
		if runErr != nil {
			log("claude implement failed: %s", runErr)
		}
		accMetrics = accMetrics.Add(metrics)
		log("implement finished (%.0fs)", time.Since(claudeStartedAt).Seconds())

		// Close implement molecule step
		deps.CloseMoleculeStep(beadID, "implement")
	} else {
		// Normal path: design phase then implement phase

		// --- Design phase (skipped in apprentice mode) ---
		var designOutput string
		if !apprenticeMode {
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
			designOutput, designMetrics, err = WizardRunClaudeCapture(worktreeDir, designPromptPath, model, designTimeout, maxTurns/2)
			if err != nil {
				log("design phase failed: %s", err)
			}
			accMetrics = accMetrics.Add(designMetrics)
			log("design finished (%.0fs)", time.Since(designStartedAt).Seconds())

			// Write DESIGN.md
			designPath := filepath.Join(worktreeDir, "DESIGN.md")
			os.WriteFile(designPath, []byte(designOutput), 0644)

			// Post plan as bead comment
			deps.AddComment(beadID, fmt.Sprintf("Design plan:\n%s", designOutput))

			// Close design molecule step
			deps.CloseMoleculeStep(beadID, "design")
		}

		// --- Implement phase ---
		deps.RegistryUpdate(wizardName, func(w *Entry) {
			w.Phase = "implement"
			w.PhaseStartedAt = time.Now().UTC().Format(time.RFC3339)
		})

		implPrompt := WizardBuildImplementPrompt(wizardName, beadID, branchName, baseBranch,
			model, maxTurns, timeout, repoCfg, focusContext, beadJSON, designOutput, "")
		implPromptPath := filepath.Join(worktreeDir, ".spire-prompt.txt")
		if err := os.WriteFile(implPromptPath, []byte(implPrompt), 0644); err != nil {
			return fmt.Errorf("write implement prompt: %w", err)
		}

		claudeStartedAt := time.Now()
		log("starting implement phase (timeout: %s)", timeout)
		implMetrics, runErr := WizardRunClaude(worktreeDir, implPromptPath, model, timeout, maxTurns)
		if runErr != nil {
			log("claude implement failed: %s", runErr)
		}
		accMetrics = accMetrics.Add(implMetrics)
		log("implement finished (%.0fs)", time.Since(claudeStartedAt).Seconds())

		// Close implement molecule step
		deps.CloseMoleculeStep(beadID, "implement")
	}

	// 10. Commit
	commitSHA, committed := WizardCommit(wc, beadID, beadTitle, log)

	// 11. Build gate — the apprentice can't go home until the build passes.
	// On failure, invokes Claude to fix build errors, up to N rounds.
	buildPassed := WizardBuildGate(wc, beadID, beadTitle, worktreeDir, model, repoCfg, &accMetrics, log)

	// 12. Run tests (informational only — does not gate completion).
	testsPassed := true
	if repoCfg.Runtime.Test != "" {
		log("validating: test")
		if err := WizardRunCmd(worktreeDir, repoCfg.Runtime.Test); err != nil {
			log("test failed: %s (informational)", err)
			testsPassed = false
		}
	}

	// 13. Update bead (comment)
	wizardUpdateBead(beadID, wizardName, branchName, commitSHA, committed, testsPassed, deps, log)

	// 14. Review handoff if committed and build passes.
	if committed && buildPassed {
		if !testsPassed {
			log("tests failed but build passed — proceeding")
			deps.AddLabel(beadID, "test-failure")
		}
		if !apprenticeMode {
			handoffDone = true
			WizardReviewHandoff(beadID, wizardName, branchName, deps, log)
		} else {
			handoffDone = true
			log("apprentice mode — skipping review handoff")
		}
	} else if !buildPassed {
		log("build failed — apprentice cannot hand off")
		deps.AddLabel(beadID, "build-failure")
	}

	// 15. If we didn't hand off, reopen the bead so it doesn't stay orphaned.
	if !handoffDone {
		deps.UpdateBead(beadID, map[string]interface{}{"status": "open"})
		log("apprentice mode — bead reopened")
	}

	// 16. Write result
	elapsed := time.Since(startedAt)
	result := "success"
	if !committed {
		result = "no_changes"
	}
	if !buildPassed {
		result = "build_failure"
	} else if !testsPassed {
		result = "test_failure"
	}
	WizardWriteResult(wizardName, beadID, result, branchName, commitSHA, elapsed, accMetrics, deps, log)

	log("done (%.0fs total)", elapsed.Seconds())
	return nil
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
	maxTurns := repoCfg.Agent.MaxTurns
	if maxTurns == 0 {
		maxTurns = 30 // build fixes should be quick
	}
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
	buildFixMetrics, runErr := WizardRunClaude(worktreeDir, promptPath, model, buildFixTimeout, maxTurns)
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
		baseBranch = "main"
		log.Printf("[resolve] base branch not configured for prefix %q — defaulting to %q", prefix, baseBranch)
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
`, wizardName, beadID, contextBlock.String(), focusContext, beadJSON)
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
`, wizardName, beadID, baseBranch, branchName, model, maxTurns, timeout,
		contextBlock.String(),
		optionalCmd(cfg.Runtime.Install),
		optionalCmd(cfg.Runtime.Lint),
		optionalCmd(cfg.Runtime.Build),
		optionalCmd(cfg.Runtime.Test),
		extra.String(),
		focusContext, beadJSON)
}

// WizardBuildClaudeArgs builds the common claude CLI arguments.
// maxTurns is passed as --max-turns to limit agent iterations.
// Timeout enforcement is handled by the caller via context.WithTimeout.
func WizardBuildClaudeArgs(prompt, model string, maxTurns int) []string {
	args := []string{
		"--dangerously-skip-permissions",
		"-p", prompt,
		"--model", model,
		"--output-format", "json",
	}
	// 0 means unlimited — omit the flag so Claude has no turn ceiling.
	// The timeout is the real gate.
	if maxTurns > 0 {
		args = append(args, "--max-turns", fmt.Sprintf("%d", maxTurns))
	}
	return args
}

// WizardRunClaude invokes the claude CLI in print mode (output teed to stderr).
// Returns token usage metrics parsed from the JSON result event.
// timeout enforces a hard process-level deadline via context.
func WizardRunClaude(worktreeDir, promptPath, model, timeout string, maxTurns int) (ClaudeMetrics, error) {
	promptBytes, err := os.ReadFile(promptPath)
	if err != nil {
		return ClaudeMetrics{}, fmt.Errorf("read prompt: %w", err)
	}

	args := WizardBuildClaudeArgs(string(promptBytes), model, maxTurns)

	dur, parseErr := time.ParseDuration(timeout)
	if parseErr != nil {
		dur = 15 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = worktreeDir
	cmd.Env = os.Environ()
	var buf bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stderr, &buf)
	cmd.Stderr = os.Stderr

	runErr := cmd.Run()
	_, metrics := parseClaudeResultJSON(buf.Bytes())
	return metrics, runErr
}

// WizardRunClaudeCapture invokes the claude CLI and captures the text result.
// Returns the result text extracted from the JSON result event, token usage
// metrics, and any execution error. Falls back to raw output if JSON parsing fails.
// timeout enforces a hard process-level deadline via context.
func WizardRunClaudeCapture(worktreeDir, promptPath, model, timeout string, maxTurns int) (string, ClaudeMetrics, error) {
	promptBytes, err := os.ReadFile(promptPath)
	if err != nil {
		return "", ClaudeMetrics{}, fmt.Errorf("read prompt: %w", err)
	}

	args := WizardBuildClaudeArgs(string(promptBytes), model, maxTurns)

	dur, parseErr := time.ParseDuration(timeout)
	if parseErr != nil {
		dur = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = worktreeDir
	cmd.Env = os.Environ()
	cmd.Stderr = os.Stderr

	out, runErr := cmd.Output()
	resultText, metrics := parseClaudeResultJSON(out)
	// Fall back to raw output if parsing didn't find a result event
	if resultText == "" {
		resultText = string(out)
	}
	return resultText, metrics, runErr
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

// WizardBuildGate runs the build command and, on failure, enters a fix loop:
// invoke Claude with the build error, re-commit, re-build, up to maxRounds.
// Returns true if the build passes (immediately or after fixes).
func WizardBuildGate(wc *spgit.WorktreeContext, beadID, beadTitle, worktreeDir, model string,
	cfg *repoconfig.RepoConfig, accMetrics *ClaudeMetrics, log func(string, ...interface{})) bool {

	buildCmd := cfg.Runtime.Build
	if buildCmd == "" {
		log("build-gate: no build command configured — skipping")
		return true
	}

	// Initial build check.
	log("build-gate: running build")
	buildOut, buildErr := WizardRunCmdCapture(worktreeDir, buildCmd)
	if buildErr == nil {
		log("build-gate: build passed")
		return true
	}
	log("build-gate: build failed:\n%s", buildOut)

	maxRounds := DefaultMaxBuildFixRounds
	if model == "" {
		model = "claude-sonnet-4-6"
	}
	maxTurns := cfg.Agent.MaxTurns
	if maxTurns == 0 {
		maxTurns = 30
	}
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

		// Invoke Claude to fix.
		fixMetrics, runErr := WizardRunClaude(worktreeDir, promptPath, model, buildFixTimeout, maxTurns)
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
		buildOut, buildErr = WizardRunCmdCapture(worktreeDir, buildCmd)
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
			return fsha, true
		}
		log("nothing staged after git add")
		return "", false
	}

	return sha, true
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
		"cost_usd":           metrics.CostUSD,
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

// --- Molecule helpers ---

// FindMoleculeSteps finds the workflow molecule for a bead and returns
// step name -> step bead ID mapping.
func FindMoleculeSteps(beadID string, deps *Deps) (string, map[string]string, error) {
	// Find molecule by workflow:<beadID> label
	mols, err := deps.ListBeads(beads.IssueFilter{
		IDPrefix: "spi-",
		Labels:   []string{"workflow:" + beadID},
	})
	if err != nil || len(mols) == 0 {
		return "", nil, fmt.Errorf("no molecule found for %s", beadID)
	}
	molID := mols[0].ID

	// Get children (the molecule steps)
	children, err := deps.GetChildren(molID)
	if err != nil {
		return molID, nil, err
	}

	// Match by title prefix
	steps := make(map[string]string)
	for _, c := range children {
		lower := strings.ToLower(c.Title)
		switch {
		case strings.HasPrefix(lower, "design"):
			steps["design"] = c.ID
		case strings.HasPrefix(lower, "implement"):
			steps["implement"] = c.ID
		case strings.HasPrefix(lower, "review"):
			steps["review"] = c.ID
		case strings.HasPrefix(lower, "merge"):
			steps["merge"] = c.ID
		}
	}
	return molID, steps, nil
}

// CloseMoleculeStep closes a named step in the bead's workflow molecule.
func CloseMoleculeStep(beadID, stepName string, deps *Deps) {
	_, steps, err := FindMoleculeSteps(beadID, deps)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: molecule step %s: %s\n", stepName, err)
		return
	}
	stepID, ok := steps[stepName]
	if !ok {
		fmt.Fprintf(os.Stderr, "warning: molecule step %s not found for %s\n", stepName, beadID)
		return
	}
	if err := deps.CloseBead(stepID); err != nil {
		fmt.Fprintf(os.Stderr, "warning: close molecule step %s (%s): %s\n", stepName, stepID, err)
	}
}

// --- Feedback collection ---

// WizardCollectFeedback collects review feedback messages addressed to this wizard for a bead.
func WizardCollectFeedback(beadID, wizardName string, deps *Deps) string {
	messages, err := deps.ListBeads(beads.IssueFilter{
		IDPrefix: "spi-",
		Labels:   []string{"msg", "to:" + wizardName, "ref:" + beadID},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: collect feedback: %s\n", err)
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
func WizardReviewHandoff(beadID, wizardName, branchName string, deps *Deps, log func(string, ...interface{})) {
	deps.AddLabel(beadID, "feat-branch:"+branchName)

	// Transition to review phase
	// Register reviewer in wizard registry
	reviewerName := wizardName + "-review"
	deps.RegistryAdd(Entry{
		Name:           reviewerName,
		PID:            0, // will be set by the reviewer process
		BeadID:         beadID,
		StartedAt:      time.Now().UTC().Format(time.RFC3339),
		Phase:          "review",
		PhaseStartedAt: time.Now().UTC().Format(time.RFC3339),
	})

	// Spawn reviewer
	backend := deps.ResolveBackend("")
	handle, spawnErr := backend.Spawn(SpawnConfig{
		Name:   reviewerName,
		BeadID: beadID,
		Role:   RoleSage,
	})
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
