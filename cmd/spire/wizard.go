package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/steveyegge/beads"
)

// cmdWizardRun is the internal entry point for a local wizard process.
// It claims a bead, creates a worktree, runs design + implement phases,
// validates, commits, pushes, updates the bead, and hands off to review.
//
// Usage: spire wizard-run <bead-id> [--name <wizard-name>] [--review-fix] [--apprentice]
func cmdWizardRun(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: spire wizard-run <bead-id> [--name <name>] [--review-fix] [--apprentice]")
	}

	// 1. Parse args
	beadID := args[0]
	wizardName := "wizard"
	reviewFix := false
	apprenticeMode := false
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
		}
	}
	if os.Getenv("SPIRE_APPRENTICE") == "1" {
		apprenticeMode = true
	}

	startedAt := time.Now()
	log := func(format string, a ...interface{}) {
		fmt.Fprintf(os.Stderr, "[%s] %s\n", wizardName, fmt.Sprintf(format, a...))
	}

	if err := requireDolt(); err != nil {
		return err
	}

	// 2. Resolve repo for this bead (prefix -> repo URL + path)
	repoPath, repoURL, baseBranch, err := wizardResolveRepo(beadID)
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

	model := repoCfg.Agent.Model
	if model == "" {
		model = "claude-sonnet-4-6"
	}
	timeout := repoCfg.Agent.Timeout
	if timeout == "" {
		timeout = "15m"
	}
	maxTurns := repoCfg.Agent.MaxTurns
	if maxTurns == 0 {
		maxTurns = 30
	}
	designTimeout := repoCfg.Agent.DesignTimeout
	if designTimeout == "" {
		designTimeout = "10m"
	}
	branchPattern := repoCfg.Branch.Pattern
	if branchPattern == "" {
		branchPattern = "feat/{bead-id}"
	}
	if repoCfg.Branch.Base != "" {
		baseBranch = repoCfg.Branch.Base
	}

	branchName := strings.ReplaceAll(branchPattern, "{bead-id}", beadID)

	// 3. Create git worktree
	worktreeDir, err := wizardCreateWorktree(repoPath, beadID, wizardName, baseBranch, branchName)
	if err != nil {
		return fmt.Errorf("create worktree: %w", err)
	}
	defer wizardCleanup(worktreeDir, repoPath)
	log("worktree: %s", worktreeDir)

	// 4. Self-register in wizards.json
	now := time.Now().UTC().Format(time.RFC3339)
	wizardRegistryAdd(localWizard{
		Name:           wizardName,
		PID:            os.Getpid(),
		BeadID:         beadID,
		Worktree:       worktreeDir,
		StartedAt:      now,
		Phase:          "init",
		PhaseStartedAt: now,
	})
	defer wizardRegistryRemove(wizardName)

	// Signal handler for clean unregister on interrupt
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		wizardRegistryRemove(wizardName)
		os.Exit(1)
	}()

	// 5. Claim the bead (skip if --review-fix — already claimed)
	if !reviewFix {
		log("claiming %s", beadID)
		os.Setenv("SPIRE_IDENTITY", wizardName)
		if err := cmdClaim([]string{beadID}); err != nil {
			return fmt.Errorf("claim: %w", err)
		}
	} else {
		os.Setenv("SPIRE_IDENTITY", wizardName)
	}

	// 6. Add owner label (deferred cleanup removes it if we exit without review handoff)
	storeAddLabel(beadID, "owner:"+wizardName)
	handoffDone := false
	defer func() {
		if !handoffDone {
			storeRemoveLabel(beadID, "owner:"+wizardName)
		}
	}()

	// 7. Set initial phase label (standalone wizard only — apprentices under executor don't set phase labels)
	if !reviewFix && !apprenticeMode {
		setPhase(beadID, "design")
	}

	// 8. Capture focus context
	log("assembling focus context")
	focusContext, err := wizardCaptureFocus(beadID)
	if err != nil {
		log("warning: focus failed: %s", err)
		focusContext = fmt.Sprintf("Bead %s — focus context unavailable", beadID)
	}

	// Get bead JSON and extract title
	beadJSON, err := wizardGetBeadJSON(beadID)
	if err != nil {
		log("warning: could not get bead JSON: %s", err)
		beadJSON = "{}"
	}
	beadTitle := wizardExtractTitle(beadJSON)

	// Install dependencies
	if repoCfg.Runtime.Install != "" {
		log("installing dependencies: %s", repoCfg.Runtime.Install)
		if err := wizardRunCmd(worktreeDir, repoCfg.Runtime.Install); err != nil {
			log("warning: install failed: %s", err)
		}
	}

	// 8-9. Phase execution
	if reviewFix {
		// --review-fix path: skip design, collect feedback, implement with feedback
		feedback := wizardCollectFeedback(beadID, wizardName)

		// Remove review-feedback label
		storeRemoveLabel(beadID, "review-feedback")

		// Transition to implement phase (standalone wizard only)
		if !apprenticeMode {
			setPhase(beadID, "implement")
		}

		// Update phase
		wizardRegistryUpdate(wizardName, func(w *localWizard) {
			w.Phase = "implement"
			w.PhaseStartedAt = time.Now().UTC().Format(time.RFC3339)
		})

		// Build implement prompt with feedback
		implPrompt := wizardBuildImplementPrompt(wizardName, beadID, branchName, baseBranch,
			model, maxTurns, timeout, repoCfg, focusContext, beadJSON, "", feedback)
		implPromptPath := filepath.Join(worktreeDir, ".spire-prompt.txt")
		if err := os.WriteFile(implPromptPath, []byte(implPrompt), 0644); err != nil {
			return fmt.Errorf("write implement prompt: %w", err)
		}

		reviewFixTimeout := designTimeout // spec: review-fix gets 10m, not 15m
		claudeStartedAt := time.Now()
		log("starting implement phase with review feedback (timeout: %s)", reviewFixTimeout)
		if err := wizardRunClaude(worktreeDir, implPromptPath, model, reviewFixTimeout, maxTurns); err != nil {
			log("claude implement failed: %s", err)
		}
		log("implement finished (%.0fs)", time.Since(claudeStartedAt).Seconds())

		// Close implement molecule step
		wizardCloseMoleculeStep(beadID, "implement")
	} else {
		// Normal path: design phase then implement phase

		// --- Design phase (skipped in apprentice mode) ---
		var designOutput string
		if !apprenticeMode {
			wizardRegistryUpdate(wizardName, func(w *localWizard) {
				w.Phase = "design"
				w.PhaseStartedAt = time.Now().UTC().Format(time.RFC3339)
			})

			designPrompt := wizardBuildDesignPrompt(wizardName, beadID, repoCfg, focusContext, beadJSON)
			designPromptPath := filepath.Join(worktreeDir, ".spire-design-prompt.txt")
			if err := os.WriteFile(designPromptPath, []byte(designPrompt), 0644); err != nil {
				return fmt.Errorf("write design prompt: %w", err)
			}

			designStartedAt := time.Now()
			log("starting design phase (timeout: %s)", designTimeout)
			designOutput, err = wizardRunClaudeCapture(worktreeDir, designPromptPath, model, designTimeout, maxTurns/2)
			if err != nil {
				log("design phase failed: %s", err)
			}
			log("design finished (%.0fs)", time.Since(designStartedAt).Seconds())

			// Write DESIGN.md
			designPath := filepath.Join(worktreeDir, "DESIGN.md")
			os.WriteFile(designPath, []byte(designOutput), 0644)

			// Post plan as bead comment
			storeAddComment(beadID, fmt.Sprintf("Design plan:\n%s", designOutput))

			// Close design molecule step
			wizardCloseMoleculeStep(beadID, "design")
		}

		// Transition to implement phase (standalone wizard only)
		if !apprenticeMode {
			setPhase(beadID, "implement")
		}

		// --- Implement phase ---
		wizardRegistryUpdate(wizardName, func(w *localWizard) {
			w.Phase = "implement"
			w.PhaseStartedAt = time.Now().UTC().Format(time.RFC3339)
		})

		implPrompt := wizardBuildImplementPrompt(wizardName, beadID, branchName, baseBranch,
			model, maxTurns, timeout, repoCfg, focusContext, beadJSON, designOutput, "")
		implPromptPath := filepath.Join(worktreeDir, ".spire-prompt.txt")
		if err := os.WriteFile(implPromptPath, []byte(implPrompt), 0644); err != nil {
			return fmt.Errorf("write implement prompt: %w", err)
		}

		claudeStartedAt := time.Now()
		log("starting implement phase (timeout: %s)", timeout)
		if err := wizardRunClaude(worktreeDir, implPromptPath, model, timeout, maxTurns); err != nil {
			log("claude implement failed: %s", err)
		}
		log("implement finished (%.0fs)", time.Since(claudeStartedAt).Seconds())

		// Close implement molecule step
		wizardCloseMoleculeStep(beadID, "implement")
	}

	// 10. Validate
	testsPassed := wizardValidate(worktreeDir, repoCfg, log)

	// 11. Commit and push
	commitSHA, pushed := wizardCommitAndPush(worktreeDir, beadID, beadTitle, branchName, log)

	// 12. Update bead (comment)
	wizardUpdateBead(beadID, wizardName, branchName, commitSHA, pushed, testsPassed, log)

	// 13. Review handoff if pushed.
	// Test failures are informational — the reviewer runs tests independently.
	// Pre-existing integration-test failures (e.g. missing .beads/ in worktree)
	// must not block the review handoff.
	//
	// handoffDone suppresses the deferred owner: removal. Set it true
	// unconditionally once we enter handoff — the function already swapped
	// owner: → implemented-by: before attempting the spawn, so the deferred
	// cleanup must not try to remove a label that no longer exists.
	if pushed {
		if !testsPassed {
			log("tests failed but branch pushed — proceeding to review")
			storeAddLabel(beadID, "test-failure")
		}
		if !apprenticeMode {
			handoffDone = true
			wizardReviewHandoff(beadID, wizardName, branchName, log)
		} else {
			handoffDone = true
			log("apprentice mode — skipping review handoff")
		}
	}

	// 14. If we didn't hand off, reopen the bead so it doesn't stay orphaned.
	if !handoffDone {
		storeUpdateBead(beadID, map[string]interface{}{"status": "open"})
		log("apprentice mode — bead reopened")
	}

	// 15. Write result
	elapsed := time.Since(startedAt)
	result := "success"
	if !pushed {
		result = "no_changes"
	}
	if !testsPassed {
		result = "test_failure"
	}
	wizardWriteResult(wizardName, beadID, result, branchName, commitSHA, elapsed, log)

	log("done (%.0fs total)", elapsed.Seconds())
	return nil
}

// wizardResolveRepo finds the local repo path, remote URL, and base branch
// for a bead by matching its ID prefix against registered repos.
func wizardResolveRepo(beadID string) (repoPath, repoURL, baseBranch string, err error) {
	cfg, err := loadConfig()
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
	database, _ := resolveDatabase(cfg)
	if database != "" && prefix != "" {
		sql := fmt.Sprintf("SELECT repo_url, branch FROM `%s`.repos WHERE prefix = '%s'",
			database, sqlEscape(prefix))
		if out, err := rawDoltQuery(sql); err == nil {
			rows := parseDoltRows(out, []string{"repo_url", "branch"})
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
	}
	return repoPath, repoURL, baseBranch, nil
}

// wizardCreateWorktree creates a git worktree for the wizard to work in.
// On first run it creates a new branch from baseBranch. On --review-fix
// the branch already exists (pushed by the previous run), so it checks
// out the existing branch instead of trying to create it again.
func wizardCreateWorktree(repoPath, beadID, wizardName, baseBranch, branchName string) (string, error) {
	worktreeBase := filepath.Join(os.TempDir(), "spire-wizard", wizardName)
	worktreeDir := filepath.Join(worktreeBase, beadID)

	// Clean up any stale worktree at this path
	if _, err := os.Stat(worktreeDir); err == nil {
		exec.Command("git", "-C", repoPath, "worktree", "remove", "--force", worktreeDir).Run()
		os.RemoveAll(worktreeDir)
	}

	if err := os.MkdirAll(filepath.Dir(worktreeDir), 0755); err != nil {
		return "", err
	}

	// Try creating worktree with new branch from base
	cmd := exec.Command("git", "-C", repoPath, "worktree", "add", "-b", branchName, worktreeDir, baseBranch)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Branch may already exist (--review-fix path). Fetch and check out the existing branch.
		exec.Command("git", "-C", repoPath, "fetch", "origin", branchName).Run()
		cmd2 := exec.Command("git", "-C", repoPath, "worktree", "add", worktreeDir, branchName)
		if out2, err2 := cmd2.CombinedOutput(); err2 != nil {
			return "", fmt.Errorf("git worktree add: %s\n%s\n(retry with existing branch): %s\n%s", err, string(out), err2, string(out2))
		}
	}

	// Configure git user in worktree
	exec.Command("git", "-C", worktreeDir, "config", "user.name", wizardName).Run()
	exec.Command("git", "-C", worktreeDir, "config", "user.email", wizardName+"@spire.local").Run()

	return worktreeDir, nil
}

// wizardCaptureFocus runs `spire focus <bead-id>` and captures stdout.
func wizardCaptureFocus(beadID string) (string, error) {
	cmd := exec.Command(os.Args[0], "focus", beadID)
	cmd.Env = os.Environ()
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// wizardGetBeadJSON runs `bd show <bead-id> --json` and captures stdout.
func wizardGetBeadJSON(beadID string) (string, error) {
	out, err := bd("show", beadID, "--json")
	if err != nil {
		return "", err
	}
	return out, nil
}

// wizardExtractTitle extracts the title from bd show --json output.
// The output is a JSON array of bead objects.
func wizardExtractTitle(beadJSON string) string {
	var parsed []struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal([]byte(beadJSON), &parsed); err == nil && len(parsed) > 0 {
		return parsed[0].Title
	}
	return ""
}

// --- Prompt builders ---

func wizardBuildDesignPrompt(wizardName, beadID string, cfg *repoconfig.RepoConfig,
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

func wizardBuildImplementPrompt(wizardName, beadID, branchName, baseBranch, model string,
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

	return fmt.Sprintf(`You are Spire autonomous wizard %s — IMPLEMENT PHASE.

Task:
- bead: %s
- base branch: %s
- feature branch: %s
- target model: %s
- max turns: %d
- hard timeout: %s

Before making changes:
1. Read the focus context below.
2. Read the repo context paths below. If a path is a directory, inspect only the relevant files.

Repo context paths:
%s
Validation commands:
- install: %s
- lint: %s
- build: %s
- test: %s

Constraints:
- Do not create a PR.
- Do not run git commit or git push — the wrapper handles that.
- Focus on implementing the task described in the focus context.
%s
Focus context:
%s

Bead JSON:
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

// wizardBuildClaudeArgs builds the common claude CLI arguments.
// maxTurns is passed as --max-turns to limit agent iterations.
// Timeout enforcement is handled by the caller via context.WithTimeout.
func wizardBuildClaudeArgs(prompt, model string, maxTurns int) []string {
	args := []string{
		"--dangerously-skip-permissions",
		"-p", prompt,
		"--model", model,
		"--max-turns", fmt.Sprintf("%d", maxTurns),
	}
	return args
}

// wizardRunClaude invokes the claude CLI in print mode (output goes to stderr).
// timeout enforces a hard process-level deadline via context.
func wizardRunClaude(worktreeDir, promptPath, model, timeout string, maxTurns int) error {
	promptBytes, err := os.ReadFile(promptPath)
	if err != nil {
		return fmt.Errorf("read prompt: %w", err)
	}

	args := wizardBuildClaudeArgs(string(promptBytes), model, maxTurns)

	dur, parseErr := time.ParseDuration(timeout)
	if parseErr != nil {
		dur = 15 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), dur)
	defer cancel()

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = worktreeDir
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stderr // wizard output goes to stderr (stdout is for JSON results)
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// wizardRunClaudeCapture invokes the claude CLI and captures stdout.
// timeout enforces a hard process-level deadline via context.
func wizardRunClaudeCapture(worktreeDir, promptPath, model, timeout string, maxTurns int) (string, error) {
	promptBytes, err := os.ReadFile(promptPath)
	if err != nil {
		return "", fmt.Errorf("read prompt: %w", err)
	}

	args := wizardBuildClaudeArgs(string(promptBytes), model, maxTurns)

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

	out, err := cmd.Output()
	return string(out), err
}

// wizardRunCmd runs a shell command in the given directory.
func wizardRunCmd(dir, command string) error {
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// wizardValidate runs lint, build, and test commands from spire.yaml.
func wizardValidate(dir string, cfg *repoconfig.RepoConfig, log func(string, ...interface{})) bool {
	if cfg.Runtime.Lint != "" {
		log("validating: lint")
		if err := wizardRunCmd(dir, cfg.Runtime.Lint); err != nil {
			log("lint failed: %s", err)
			return false
		}
	}
	if cfg.Runtime.Build != "" {
		log("validating: build")
		if err := wizardRunCmd(dir, cfg.Runtime.Build); err != nil {
			log("build failed: %s", err)
			return false
		}
	}
	if cfg.Runtime.Test != "" {
		log("validating: test")
		if err := wizardRunCmd(dir, cfg.Runtime.Test); err != nil {
			log("test failed: %s", err)
			return false
		}
	}
	return true
}

// wizardCommitAndPush commits any changes and pushes the branch.
func wizardCommitAndPush(dir, beadID, beadTitle, branchName string, log func(string, ...interface{})) (commitSHA string, pushed bool) {
	// Check for changes
	statusCmd := exec.Command("git", "-C", dir, "status", "--porcelain")
	statusOut, _ := statusCmd.Output()
	if len(strings.TrimSpace(string(statusOut))) == 0 {
		log("no changes to commit")
		return "", false
	}

	// Remove prompt files before staging
	os.Remove(filepath.Join(dir, ".spire-prompt.txt"))
	os.Remove(filepath.Join(dir, ".spire-design-prompt.txt"))

	// Stage all
	if err := exec.Command("git", "-C", dir, "add", "-A").Run(); err != nil {
		log("git add failed: %s", err)
		return "", false
	}

	// Check if there's anything staged
	diffCmd := exec.Command("git", "-C", dir, "diff", "--cached", "--quiet")
	if diffCmd.Run() == nil {
		log("nothing staged after git add")
		return "", false
	}

	// Commit
	title := beadTitle
	if title == "" {
		title = "implement task"
	}
	// Lowercase first char, strip trailing period.
	if len(title) > 0 {
		title = strings.ToLower(title[:1]) + title[1:]
	}
	title = strings.TrimRight(title, ".")
	msg := fmt.Sprintf("feat(%s): %s", beadID, title)
	commitCmd := exec.Command("git", "-C", dir, "commit", "-m", msg)
	if out, err := commitCmd.CombinedOutput(); err != nil {
		log("git commit failed: %s\n%s", err, string(out))
		return "", false
	}

	// Get commit SHA
	shaCmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	shaOut, _ := shaCmd.Output()
	commitSHA = strings.TrimSpace(string(shaOut))

	// Push
	log("pushing branch %s", branchName)
	pushCmd := exec.Command("git", "-C", dir, "push", "-u", "origin", branchName)
	pushCmd.Env = os.Environ()
	if out, err := pushCmd.CombinedOutput(); err != nil {
		log("git push failed: %s\n%s", err, string(out))
		return commitSHA, false
	}

	return commitSHA, true
}

// wizardUpdateBead adds a comment to the bead. Labels are managed by wizardReviewHandoff.
func wizardUpdateBead(beadID, wizardName, branchName, commitSHA string, pushed, testsPassed bool, log func(string, ...interface{})) {
	if !pushed {
		note := fmt.Sprintf("Wizard %s finished without changes", wizardName)
		storeAddComment(beadID, note)
		return
	}

	note := fmt.Sprintf("Wizard %s pushed branch %s", wizardName, branchName)
	if commitSHA != "" {
		note += fmt.Sprintf(" @ %s", commitSHA[:min(len(commitSHA), 8)])
	}
	if !testsPassed {
		note += " (tests failed)"
	}
	storeAddComment(beadID, note)
}

// wizardWriteResult writes a result JSON file for observability.
func wizardWriteResult(wizardName, beadID, result, branchName, commitSHA string,
	elapsed time.Duration, log func(string, ...interface{})) {

	resultDir := filepath.Join(doltGlobalDir(), "wizards", wizardName)
	os.MkdirAll(resultDir, 0755)

	data := map[string]interface{}{
		"wizard":    wizardName,
		"bead_id":   beadID,
		"result":    result,
		"branch":    branchName,
		"commit":    commitSHA,
		"elapsed_s": int(elapsed.Seconds()),
		"completed": time.Now().UTC().Format(time.RFC3339),
	}
	out, _ := json.MarshalIndent(data, "", "  ")
	resultPath := filepath.Join(resultDir, "result.json")
	if err := os.WriteFile(resultPath, append(out, '\n'), 0644); err != nil {
		log("warning: could not write result: %s", err)
	}
}

// wizardCleanup removes the git worktree.
func wizardCleanup(worktreeDir, repoPath string) {
	exec.Command("git", "-C", repoPath, "worktree", "remove", "--force", worktreeDir).Run()
	os.RemoveAll(worktreeDir)
}

// --- Molecule helpers ---

// wizardFindMoleculeSteps finds the workflow molecule for a bead and returns
// step name -> step bead ID mapping.
func wizardFindMoleculeSteps(beadID string) (string, map[string]string, error) {
	// Find molecule by workflow:<beadID> label
	mols, err := storeListBeads(beads.IssueFilter{
		IDPrefix: "spi-",
		Labels:   []string{"workflow:" + beadID},
	})
	if err != nil || len(mols) == 0 {
		return "", nil, fmt.Errorf("no molecule found for %s", beadID)
	}
	molID := mols[0].ID

	// Get children (the molecule steps)
	children, err := storeGetChildren(molID)
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

// wizardCloseMoleculeStep closes a named step in the bead's workflow molecule.
func wizardCloseMoleculeStep(beadID, stepName string) {
	_, steps, err := wizardFindMoleculeSteps(beadID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: molecule step %s: %s\n", stepName, err)
		return
	}
	stepID, ok := steps[stepName]
	if !ok {
		fmt.Fprintf(os.Stderr, "warning: molecule step %s not found for %s\n", stepName, beadID)
		return
	}
	if err := storeCloseBead(stepID); err != nil {
		fmt.Fprintf(os.Stderr, "warning: close molecule step %s (%s): %s\n", stepName, stepID, err)
	}
}

// --- Feedback collection ---

// wizardCollectFeedback collects review feedback messages addressed to this wizard for a bead.
func wizardCollectFeedback(beadID, wizardName string) string {
	messages, err := storeListBeads(beads.IssueFilter{
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
		storeCloseBead(m.ID)
	}
	return strings.Join(parts, "\n---\n")
}

// --- Review handoff ---

// wizardReviewHandoff swaps labels and spawns a reviewer process.
// On spawn failure, review-ready and implemented-by stay in place so the
// steward's detectReviewReady() can re-route the bead on the next cycle.
func wizardReviewHandoff(beadID, wizardName, branchName string, log func(string, ...interface{})) {
	// Swap owner -> implemented-by
	storeRemoveLabel(beadID, "owner:"+wizardName)
	storeAddLabel(beadID, "implemented-by:"+wizardName)

	// Add review labels
	storeAddLabel(beadID, "review-ready")
	storeAddLabel(beadID, "feat-branch:"+branchName)

	// Transition to review phase
	setPhase(beadID, "review")

	// Register reviewer in wizard registry
	reviewerName := wizardName + "-review"
	wizardRegistryAdd(localWizard{
		Name:           reviewerName,
		PID:            0, // will be set by the reviewer process
		BeadID:         beadID,
		StartedAt:      time.Now().UTC().Format(time.RFC3339),
		Phase:          "review",
		PhaseStartedAt: time.Now().UTC().Format(time.RFC3339),
	})

	// Spawn reviewer
	backend := ResolveBackend("")
	handle, spawnErr := backend.Spawn(SpawnConfig{
		Name:   reviewerName,
		BeadID: beadID,
		Role:   RoleSage,
	})
	if spawnErr != nil {
		log("failed to spawn reviewer: %s — leaving review-ready for steward", spawnErr)
		// Remove the dead registry entry but keep review-ready and
		// implemented-by so the steward's detectReviewReady() can re-route
		// the bead to a review pod (k8s) or a future local retry.
		wizardRegistryRemove(reviewerName)
		storeAddComment(beadID, fmt.Sprintf("Local review spawn failed: %s — bead left review-ready for steward", spawnErr))
		return
	}

	// Update registry with the identifier now that spawn succeeded.
	if id := handle.Identifier(); id != "" {
		if pid, err := strconv.Atoi(id); err == nil {
			wizardRegistryUpdate(reviewerName, func(w *localWizard) {
				w.PID = pid
			})
		}
	}

	log("review handoff complete, spawned %s (%s)", reviewerName, handle.Identifier())
	// Self-unregister happens via defer in cmdWizardRun
}
