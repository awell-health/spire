package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/repoconfig"
)

// cmdWizardRun is the internal entry point for a local wizard process.
// It claims a bead, creates a worktree, runs claude, validates, commits,
// pushes, and updates the bead. One-shot: exits when done.
//
// Usage: spire wizard-run <bead-id> [--name <wizard-name>]
func cmdWizardRun(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: spire wizard-run <bead-id> [--name <name>]")
	}

	beadID := args[0]
	wizardName := "wizard"
	for i := 1; i < len(args); i++ {
		if args[i] == "--name" && i+1 < len(args) {
			i++
			wizardName = args[i]
		}
	}

	startedAt := time.Now()
	log := func(format string, a ...interface{}) {
		fmt.Fprintf(os.Stderr, "[%s] %s\n", wizardName, fmt.Sprintf(format, a...))
	}

	if err := requireDolt(); err != nil {
		return err
	}

	// 1. Resolve repo for this bead (prefix → repo URL + path)
	repoPath, repoURL, baseBranch, err := wizardResolveRepo(beadID)
	if err != nil {
		return fmt.Errorf("resolve repo: %w", err)
	}
	log("repo: %s (base: %s)", repoURL, baseBranch)

	// 2. Load repo config (spire.yaml)
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

	// 4. Claim the bead
	log("claiming %s", beadID)
	os.Setenv("SPIRE_IDENTITY", wizardName)
	if err := cmdClaim([]string{beadID}); err != nil {
		return fmt.Errorf("claim: %w", err)
	}

	// 5. Capture focus context
	log("assembling focus context")
	focusContext, err := wizardCaptureFocus(beadID)
	if err != nil {
		log("warning: focus failed: %s", err)
		focusContext = fmt.Sprintf("Bead %s — focus context unavailable", beadID)
	}

	// 6. Get bead JSON and extract title
	beadJSON, err := wizardGetBeadJSON(beadID)
	if err != nil {
		log("warning: could not get bead JSON: %s", err)
		beadJSON = "{}"
	}
	beadTitle := wizardExtractTitle(beadJSON)

	// 7. Build prompt
	prompt := wizardBuildPrompt(wizardName, beadID, branchName, baseBranch,
		model, maxTurns, timeout, repoCfg, focusContext, beadJSON)

	// 8. Write prompt to file in worktree
	promptPath := filepath.Join(worktreeDir, ".spire-prompt.txt")
	if err := os.WriteFile(promptPath, []byte(prompt), 0644); err != nil {
		return fmt.Errorf("write prompt: %w", err)
	}

	// 9. Install dependencies
	if repoCfg.Runtime.Install != "" {
		log("installing dependencies: %s", repoCfg.Runtime.Install)
		if err := wizardRunCmd(worktreeDir, repoCfg.Runtime.Install); err != nil {
			log("warning: install failed: %s", err)
		}
	}

	// 10. Run Claude
	claudeStartedAt := time.Now()
	log("starting claude (model: %s, timeout: %s)", model, timeout)
	if err := wizardRunClaude(worktreeDir, promptPath, model, timeout); err != nil {
		log("claude failed: %s", err)
		// Continue — we still want to commit+push any partial work
	}
	log("claude finished (%.0fs)", time.Since(claudeStartedAt).Seconds())

	// 11. Validate
	testsPassed := wizardValidate(worktreeDir, repoCfg, log)

	// 12. Commit and push
	commitSHA, pushed := wizardCommitAndPush(worktreeDir, beadID, beadTitle, branchName, log)

	// 13. Update bead state
	wizardUpdateBead(beadID, wizardName, branchName, commitSHA, pushed, testsPassed, log)

	// 14. Write result
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

	// Extract prefix from bead ID (e.g. "spi-abc" → "spi")
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

	// Create worktree with new branch from base
	cmd := exec.Command("git", "-C", repoPath, "worktree", "add", "-b", branchName, worktreeDir, baseBranch)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git worktree add: %s\n%s", err, string(out))
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
	var beads []struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal([]byte(beadJSON), &beads); err == nil && len(beads) > 0 {
		return beads[0].Title
	}
	return ""
}

func wizardBuildPrompt(wizardName, beadID, branchName, baseBranch, model string,
	maxTurns int, timeout string, cfg *repoconfig.RepoConfig,
	focusContext, beadJSON string) string {

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

	return fmt.Sprintf(`You are Spire autonomous wizard %s.

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
		focusContext, beadJSON)
}

// wizardRunClaude invokes the claude CLI in print mode.
func wizardRunClaude(worktreeDir, promptPath, model, timeout string) error {
	// Read prompt from file
	promptBytes, err := os.ReadFile(promptPath)
	if err != nil {
		return fmt.Errorf("read prompt: %w", err)
	}

	args := []string{
		"--dangerously-skip-permissions",
		"-p", string(promptBytes),
		"--model", model,
	}

	cmd := exec.Command("claude", args...)
	cmd.Dir = worktreeDir
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stderr // wizard output goes to stderr (stdout is for JSON results)
	cmd.Stderr = os.Stderr

	return cmd.Run()
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

	// Remove prompt file before staging
	os.Remove(filepath.Join(dir, ".spire-prompt.txt"))

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

// wizardUpdateBead adds a comment and updates labels on the bead.
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

	if pushed && testsPassed {
		storeAddLabel(beadID, "review-ready")
		storeAddLabel(beadID, "feat-branch:"+branchName)
		log("marked review-ready")
	}
}

// wizardWriteResult writes a result JSON file for observability.
func wizardWriteResult(wizardName, beadID, result, branchName, commitSHA string,
	elapsed time.Duration, log func(string, ...interface{})) {

	resultDir := filepath.Join(doltGlobalDir(), "wizards", wizardName)
	os.MkdirAll(resultDir, 0755)

	data := map[string]interface{}{
		"wizard":     wizardName,
		"bead_id":    beadID,
		"result":     result,
		"branch":     branchName,
		"commit":     commitSHA,
		"elapsed_s":  int(elapsed.Seconds()),
		"completed":  time.Now().UTC().Format(time.RFC3339),
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

