package main

import (
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
)

// Review is the structured output from a code review.
type Review struct {
	Verdict string        `json:"verdict"` // "approve", "request_changes"
	Summary string        `json:"summary"`
	Issues  []ReviewIssue `json:"issues,omitempty"`
}

// ReviewIssue represents a single issue found during review.
type ReviewIssue struct {
	File     string `json:"file"`
	Line     int    `json:"line,omitempty"`
	Severity string `json:"severity"` // "error", "warning"
	Message  string `json:"message"`
}

func cmdWizardReview(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: spire wizard-review <bead-id> --name <name> [--verdict-only]")
	}

	beadID := args[0]
	reviewerName := "reviewer"
	verdictOnly := false
	for i := 1; i < len(args); i++ {
		if args[i] == "--name" && i+1 < len(args) {
			i++
			reviewerName = args[i]
		} else if args[i] == "--verdict-only" {
			verdictOnly = true
		}
	}
	if os.Getenv("SPIRE_VERDICT_ONLY") == "1" {
		verdictOnly = true
	}

	log := func(format string, a ...interface{}) {
		fmt.Fprintf(os.Stderr, "[%s] %s\n", reviewerName, fmt.Sprintf(format, a...))
	}

	// Self-register in the wizard registry.
	now := time.Now().UTC().Format(time.RFC3339)
	wizardRegistryAdd(localWizard{
		Name:           reviewerName,
		PID:            os.Getpid(),
		BeadID:         beadID,
		StartedAt:      now,
		Phase:          "review",
		PhaseStartedAt: now,
	})
	defer wizardRegistryRemove(reviewerName)

	// Signal handler for cleanup. os.Exit skips defers, so we must
	// replicate the registry cleanup here.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		wizardRegistryRemove(reviewerName)
		os.Exit(1)
	}()

	if err := requireDolt(); err != nil {
		return err
	}

	// 1. Resolve repo
	repoPath, _, baseBranch, err := wizardResolveRepo(beadID)
	if err != nil {
		return fmt.Errorf("resolve repo: %w", err)
	}

	// 2. Get bead and resolve branch from labels
	bead, err := storeGetBead(beadID)
	if err != nil {
		return fmt.Errorf("get bead: %w", err)
	}

	branch := hasLabel(bead, "feat-branch:")
	if branch == "" {
		branch = fmt.Sprintf("feat/%s", beadID)
	}
	log("reviewing %s branch %s", beadID, branch)

	// 3. Create own worktree (before doing work, so failures don't leak the worktree)
	worktreeDir, err := reviewCreateWorktree(repoPath, beadID, reviewerName, baseBranch, branch)
	if err != nil {
		return fmt.Errorf("create worktree: %w", err)
	}
	defer reviewCleanupWorktree(worktreeDir, repoPath)
	log("worktree: %s", worktreeDir)

	// 4. Get diff
	diff, err := reviewGetDiff(worktreeDir, baseBranch)
	if err != nil {
		return fmt.Errorf("get diff: %w", err)
	}
	if diff == "" {
		log("no diff found, nothing to review")
		return nil
	}

	// 5. Run tests in worktree
	repoCfg, _ := repoconfig.Load(repoPath)
	testOutput := ""
	if repoCfg != nil && repoCfg.Runtime.Test != "" {
		log("running tests")
		testOut, testErr := reviewRunTests(worktreeDir, repoCfg)
		testOutput = testOut
		if testErr != nil {
			log("tests failed: %s", testErr)
		}
	}

	// 7. Get current review round from existing review child beads
	existingReviews, _ := storeGetReviewBeads(beadID)
	round := len(existingReviews) + 1
	log("review round: %d", round)

	// 7b. Load formula for revision policy
	var revPolicy RevisionPolicy
	if formula, fErr := LoadFormulaByName("spire-agent-work"); fErr == nil {
		revPolicy = formula.GetRevisionPolicy()
	}
	if revPolicy.MaxRounds == 0 {
		revPolicy = RevisionPolicy{MaxRounds: 3, ArbiterModel: "claude-opus-4-6"}
	}

	// 7c. Create review-round bead before dispatching review
	reviewBeadID, rbErr := storeCreateReviewBead(beadID, reviewerName, round)
	if rbErr != nil {
		log("warning: create review bead: %s", rbErr)
	}

	// 8. Run Opus review
	log("running Opus review")
	review, err := reviewRunOpus(bead.Title, bead.Description, diff, testOutput, round)
	if err != nil {
		// Close review bead on failure
		if reviewBeadID != "" {
			storeCloseReviewBead(reviewBeadID, "error", err.Error())
		}
		return fmt.Errorf("opus review: %w", err)
	}
	log("verdict: %s — %s", review.Verdict, review.Summary)

	// Close review-round bead with verdict
	if reviewBeadID != "" {
		var summaryBuf strings.Builder
		summaryBuf.WriteString(review.Summary)
		for _, issue := range review.Issues {
			summaryBuf.WriteString(fmt.Sprintf("\n- [%s] %s", issue.Severity, issue.Message))
			if issue.File != "" {
				summaryBuf.WriteString(fmt.Sprintf(" (%s", issue.File))
				if issue.Line > 0 {
					summaryBuf.WriteString(fmt.Sprintf(":%d", issue.Line))
				}
				summaryBuf.WriteString(")")
			}
		}
		storeCloseReviewBead(reviewBeadID, review.Verdict, summaryBuf.String())
	}

	// 9. Handle verdict
	switch review.Verdict {
	case "approve":
		if verdictOnly {
			// Verdict-only mode: post comment, exit. Don't merge or close.
			// Review bead already closed with verdict above.
			storeAddLabel(beadID, "review-approved")
			storeAddComment(beadID, fmt.Sprintf("Review approved by %s (verdict-only)", reviewerName))
			log("approved (verdict-only) — exiting")
		} else {
			if err := reviewHandleApproval(beadID, reviewerName, branch, baseBranch, repoPath, log); err != nil {
				return fmt.Errorf("approval: %w", err)
			}
		}
	case "request_changes":
		if verdictOnly {
			// Review bead already closed with verdict above.
			var comment strings.Builder
			comment.WriteString(fmt.Sprintf("Review round %d: request_changes — %s", round, review.Summary))
			for _, issue := range review.Issues {
				comment.WriteString(fmt.Sprintf("\n- [%s] %s", issue.Severity, issue.Message))
				if issue.File != "" {
					comment.WriteString(fmt.Sprintf(" (%s", issue.File))
					if issue.Line > 0 {
						comment.WriteString(fmt.Sprintf(":%d", issue.Line))
					}
					comment.WriteString(")")
				}
			}
			storeAddComment(beadID, comment.String())
			log("request_changes (verdict-only) — exiting")
		} else {
			if err := reviewHandleRequestChanges(beadID, reviewerName, review, round, revPolicy, log); err != nil {
				return err
			}
		}
	default:
		log("unexpected verdict: %s, treating as request_changes", review.Verdict)
		if !verdictOnly {
			if err := reviewHandleRequestChanges(beadID, reviewerName, review, round, revPolicy, log); err != nil {
				return err
			}
		}
	}

	return nil
}

// --- Worktree helpers ---

func reviewCreateWorktree(repoPath, beadID, reviewerName, baseBranch, branch string) (string, error) {
	worktreeDir := filepath.Join(os.TempDir(), "spire-review", reviewerName, beadID)

	// Clean up stale worktree
	if _, err := os.Stat(worktreeDir); err == nil {
		exec.Command("git", "-C", repoPath, "worktree", "remove", "--force", worktreeDir).Run()
		os.RemoveAll(worktreeDir)
	}

	os.MkdirAll(filepath.Dir(worktreeDir), 0755)

	// Fetch the branch
	exec.Command("git", "-C", repoPath, "fetch", "origin", branch).Run()

	// Create worktree from the branch (not creating new branch)
	cmd := exec.Command("git", "-C", repoPath, "worktree", "add", worktreeDir, "origin/"+branch)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Try with local branch name
		cmd2 := exec.Command("git", "-C", repoPath, "worktree", "add", worktreeDir, branch)
		if out2, err2 := cmd2.CombinedOutput(); err2 != nil {
			return "", fmt.Errorf("git worktree add: %s\n%s\n%s", err, string(out), string(out2))
		}
	}

	return worktreeDir, nil
}

func reviewCleanupWorktree(worktreeDir, repoPath string) {
	exec.Command("git", "-C", repoPath, "worktree", "remove", "--force", worktreeDir).Run()
	os.RemoveAll(worktreeDir)
}

// --- Diff + test helpers ---

func reviewGetDiff(worktreeDir, baseBranch string) (string, error) {
	// Fetch base for comparison
	exec.Command("git", "-C", worktreeDir, "fetch", "origin", baseBranch).Run()

	cmd := exec.Command("git", "-C", worktreeDir, "diff", "origin/"+baseBranch+"...HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git diff: %w", err)
	}
	return string(out), nil
}

func reviewRunTests(worktreeDir string, cfg *repoconfig.RepoConfig) (string, error) {
	if cfg.Runtime.Test == "" {
		return "", nil
	}
	cmd := exec.Command("sh", "-c", cfg.Runtime.Test)
	cmd.Dir = worktreeDir
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// --- Opus review ---

func reviewRunOpus(title, spec, diff, testOutput string, round int) (*Review, error) {
	systemPrompt := `You are a senior staff engineer performing code review. You review diffs against specifications.

Your job is to determine: does this implementation satisfy the specification?

Evaluate:
1. Correctness: Does the code do what the spec says?
2. Completeness: Are all requirements from the spec addressed?
3. Quality: Is the code clean, well-tested, and maintainable?
4. Edge cases: Are error paths and edge cases handled?

Respond ONLY with a JSON object:
{
  "verdict": "approve" | "request_changes",
  "summary": "1-3 sentence summary",
  "issues": [{"file": "path", "line": 42, "severity": "error|warning", "message": "description"}]
}

Verdicts:
- "approve": Implementation satisfies the spec. Minor style issues are OK.
- "request_changes": Implementation has fixable issues. List them.`

	var userPrompt strings.Builder
	userPrompt.WriteString("## Task\n")
	userPrompt.WriteString(title)
	userPrompt.WriteString("\n\n")

	if spec != "" {
		userPrompt.WriteString("## Specification\n")
		userPrompt.WriteString(spec)
		userPrompt.WriteString("\n\n")
	}

	userPrompt.WriteString("## Diff\n```diff\n")
	if len(diff) > 500000 {
		userPrompt.WriteString(diff[:500000])
		userPrompt.WriteString("\n... (truncated)\n")
	} else {
		userPrompt.WriteString(diff)
	}
	userPrompt.WriteString("\n```\n\n")

	if testOutput != "" {
		userPrompt.WriteString("## Test Results\n```\n")
		if len(testOutput) > 50000 {
			userPrompt.WriteString(testOutput[:50000])
			userPrompt.WriteString("\n... (truncated)\n")
		} else {
			userPrompt.WriteString(testOutput)
		}
		userPrompt.WriteString("\n```\n\n")
	}

	if round > 1 {
		userPrompt.WriteString(fmt.Sprintf("## Review Context\nThis is review round %d. Focus on whether previously flagged issues have been addressed.\n", round))
	}

	// Build full prompt for claude CLI
	fullPrompt := fmt.Sprintf("System: %s\n\n%s", systemPrompt, userPrompt.String())

	// Run claude with Opus model
	cmd := exec.Command("claude", "--dangerously-skip-permissions", "-p", fullPrompt, "--model", "claude-opus-4-6", "--output-format", "text")
	cmd.Env = os.Environ()
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("claude review: %w", err)
	}

	return parseReviewOutput(string(out))
}

func parseReviewOutput(text string) (*Review, error) {
	text = strings.TrimSpace(text)

	// Try direct JSON parse
	var review Review
	if err := json.Unmarshal([]byte(text), &review); err == nil {
		if review.Verdict == "approve" || review.Verdict == "request_changes" {
			return &review, nil
		}
	}

	// Try extracting from markdown code block
	if idx := strings.Index(text, "```json"); idx >= 0 {
		start := idx + len("```json")
		if end := strings.Index(text[start:], "```"); end >= 0 {
			block := strings.TrimSpace(text[start : start+end])
			if err := json.Unmarshal([]byte(block), &review); err == nil {
				if review.Verdict == "approve" || review.Verdict == "request_changes" {
					return &review, nil
				}
			}
		}
	}

	// Try finding any JSON object
	if idx := strings.Index(text, "{"); idx >= 0 {
		depth := 0
		for i := idx; i < len(text); i++ {
			switch text[i] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					block := text[idx : i+1]
					if err := json.Unmarshal([]byte(block), &review); err == nil {
						if review.Verdict == "approve" || review.Verdict == "request_changes" {
							return &review, nil
						}
					}
				}
			}
		}
	}

	// Fallback
	return &Review{
		Verdict: "request_changes",
		Summary: "Could not parse structured review response",
	}, nil
}

// --- Verdict handlers ---

// reviewHandleApproval handles an approved verdict: transitions to merge phase,
// executes the terminal merge step, and closes the bead.
//
// Returns an error if the merge fails; the bead is left open so a human can diagnose.
func reviewHandleApproval(beadID, reviewerName, branch, baseBranch, repoPath string, log func(string, ...interface{})) error {
	log("approved — closing review step")

	// Add review-approved (still needed for executor/workshop reads until those are migrated)
	storeAddLabel(beadID, "review-approved")

	// Close review molecule step
	wizardCloseMoleculeStep(beadID, "review")
	storeAddComment(beadID, fmt.Sprintf("Review approved by %s", reviewerName))

	// Resolve build command from bead's formula
	bead, _ := storeGetBead(beadID)
	buildCmd := resolveBeadBuildCmd(bead)

	// Terminal merge: rebase → build verify → ff-only merge → push → delete branch → close bead.
	// DAG invariant enforced: branch is deleted before bead is closed.
	log("executing terminal merge")
	if err := terminalMerge(beadID, branch, baseBranch, repoPath, buildCmd, log); err != nil {
		log("merge failed: %s — bead left at review-approved", err)
		storeAddComment(beadID, fmt.Sprintf("Auto-merge failed: %s", err))
		escalateHumanFailure(beadID, reviewerName, "merge-failure", err.Error())
		return err
	}

	// Close merge molecule step after successful merge
	wizardCloseMoleculeStep(beadID, "merge")
	storeRemoveLabel(beadID, "test-failure")
	log("done — merged and closed")
	return nil
}

// reviewMerge creates a PR for the feature branch and squash-merges it.
func reviewMerge(beadID, beadTitle, branch, baseBranch, repoPath string, log func(string, ...interface{})) error {
	// Determine commit type from bead type
	bead, _ := storeGetBead(beadID)
	commitType := "feat"
	switch bead.Type {
	case "bug":
		commitType = "fix"
	case "chore":
		commitType = "chore"
	}

	prTitle := fmt.Sprintf("%s(%s): %s", commitType, beadID, beadTitle)
	if len(prTitle) > 72 {
		prTitle = prTitle[:72]
	}

	prBody := fmt.Sprintf("## Summary\nAuto-generated by Spire wizard for bead `%s`.\n\nBead: %s — %s", beadID, beadID, beadTitle)

	// Create PR via gh CLI
	log("creating PR: %s → %s", branch, baseBranch)
	createCmd := exec.Command("gh", "pr", "create",
		"--title", prTitle,
		"--body", prBody,
		"--base", baseBranch,
		"--head", branch,
	)
	createCmd.Dir = repoPath
	createCmd.Env = os.Environ()
	createOut, err := createCmd.CombinedOutput()
	if err != nil {
		outStr := strings.TrimSpace(string(createOut))
		// If PR already exists, that's fine — just merge it.
		if !strings.Contains(outStr, "already exists") {
			return fmt.Errorf("gh pr create: %s — %s", err, outStr)
		}
		log("PR already exists, proceeding to merge")
	} else {
		prURL := strings.TrimSpace(string(createOut))
		log("created PR: %s", prURL)
		storeAddComment(beadID, fmt.Sprintf("PR created: %s", prURL))
	}

	// Squash-merge via gh CLI
	log("merging PR (squash)")
	mergeCmd := exec.Command("gh", "pr", "merge", branch,
		"--squash",
		"--delete-branch",
		"--subject", prTitle,
	)
	mergeCmd.Dir = repoPath
	mergeCmd.Env = os.Environ()
	mergeOut, err := mergeCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh pr merge: %s — %s", err, strings.TrimSpace(string(mergeOut)))
	}
	log("merged successfully")
	storeAddComment(beadID, fmt.Sprintf("Merged %s into %s (squash)", branch, baseBranch))

	return nil
}

func reviewHandleRequestChanges(beadID, reviewerName string, review *Review, round int, revPolicy RevisionPolicy, log func(string, ...interface{})) error {
	log("requesting changes (round %d)", round)

	// Review bead already closed with verdict above; no label writes needed.

	// Post comment
	var comment strings.Builder
	comment.WriteString(fmt.Sprintf("Review round %d: request_changes — %s", round, review.Summary))
	if len(review.Issues) > 0 {
		comment.WriteString("\n\nIssues:")
		for _, issue := range review.Issues {
			comment.WriteString(fmt.Sprintf("\n- [%s] %s", issue.Severity, issue.Message))
			if issue.File != "" {
				comment.WriteString(fmt.Sprintf(" (%s", issue.File))
				if issue.Line > 0 {
					comment.WriteString(fmt.Sprintf(":%d", issue.Line))
				}
				comment.WriteString(")")
			}
		}
	}
	storeAddComment(beadID, comment.String())

	// Check if we've reached max review rounds — escalate to arbiter
	if round >= revPolicy.MaxRounds {
		log("max review rounds (%d) reached — escalating to arbiter", revPolicy.MaxRounds)
		return reviewEscalateToArbiter(beadID, reviewerName, review, revPolicy, log)
	}

	wizardName := "wizard-" + beadID

	// Send feedback message
	feedbackText := review.Summary
	if len(review.Issues) > 0 {
		var buf strings.Builder
		buf.WriteString(review.Summary)
		for _, issue := range review.Issues {
			buf.WriteString(fmt.Sprintf("\n- [%s] %s", issue.Severity, issue.Message))
			if issue.File != "" {
				buf.WriteString(fmt.Sprintf(" (%s:%d)", issue.File, issue.Line))
			}
		}
		feedbackText = buf.String()
	}

	// Send via spire send
	spireBin, _ := os.Executable()
	sendCmd := exec.Command(spireBin, "send", wizardName, feedbackText, "--ref", beadID, "--as", reviewerName)
	sendCmd.Env = os.Environ()
	sendCmd.Stderr = os.Stderr
	sendCmd.Run()

	// Register re-engaged wizard
	reengageNow := time.Now().UTC().Format(time.RFC3339)
	wizardRegistryAdd(localWizard{
		Name:           wizardName,
		PID:            0,
		BeadID:         beadID,
		StartedAt:      reengageNow,
		Phase:          "review-fix",
		PhaseStartedAt: reengageNow,
	})

	// Spawn wizard-run --review-fix
	log("spawning %s --review-fix", wizardName)
	logDir := filepath.Join(doltGlobalDir(), "wizards")
	backend := ResolveBackend("")
	handle, spawnErr := backend.Spawn(SpawnConfig{
		Name:      wizardName,
		BeadID:    beadID,
		Role:      RoleApprentice,
		ExtraArgs: []string{"--review-fix"},
		LogPath:   filepath.Join(logDir, wizardName+"-fix.log"),
	})
	if spawnErr != nil {
		log("failed to spawn wizard: %s", spawnErr)
	} else if id := handle.Identifier(); id != "" {
		if pid, convErr := strconv.Atoi(id); convErr == nil {
			wizardRegistryUpdate(wizardName, func(w *localWizard) {
				w.PID = pid
			})
		}
	}

	log("done — re-engaged %s for round %d", wizardName, round)
	return nil
}

// reviewEscalateToArbiter runs the arbiter model to make a final decision.
// Arbiter outcomes: merge (force-approve), discard (close as wontfix), split (child beads).
func reviewEscalateToArbiter(beadID, reviewerName string, lastReview *Review, policy RevisionPolicy, log func(string, ...interface{})) error {
	log("running arbiter (%s)", policy.ArbiterModel)

	// Build arbiter prompt
	bead, err := storeGetBead(beadID)
	if err != nil {
		return fmt.Errorf("arbiter: get bead: %w", err)
	}

	// Collect structured review history from review-round beads
	reviewBeads, _ := storeGetReviewBeads(beadID)
	var reviewHistory strings.Builder
	for _, rb := range reviewBeads {
		roundNum := reviewRoundNumber(rb)
		sage := hasLabel(rb, "sage:")
		reviewHistory.WriteString(fmt.Sprintf("### Round %d (sage: %s, status: %s)\n", roundNum, sage, rb.Status))
		if rb.Description != "" {
			reviewHistory.WriteString(rb.Description)
		}
		reviewHistory.WriteString("\n---\n")
	}
	// Fall back to comment archaeology if no review beads found
	if len(reviewBeads) == 0 {
		comments, _ := storeGetComments(beadID)
		for _, c := range comments {
			if strings.Contains(c.Text, "Review round") || strings.Contains(c.Text, "review") {
				reviewHistory.WriteString(c.Text)
				reviewHistory.WriteString("\n---\n")
			}
		}
	}

	prompt := fmt.Sprintf(`You are an arbiter — a senior technical decision-maker.

A code review has gone through %d rounds without resolution. You must make a final call.

## Task
Title: %s
Description: %s

## Last Review Summary
%s

## Review History
%s

## Your Decision

You MUST respond with ONLY a JSON object (no markdown, no explanation):
{
  "decision": "merge" | "discard" | "split",
  "reason": "1-2 sentence justification",
  "split_tasks": [{"title": "task title", "description": "what to do"}]  // only if decision=split
}

Decision meanings:
- "merge": Force-approve. The implementation is good enough. Minor remaining issues are acceptable.
- "discard": Close as wontfix. The approach is fundamentally wrong or the task is no longer needed.
- "split": The remaining issues are real but independent. Create child beads for each and close this bead.
`, policy.MaxRounds, bead.Title, bead.Description, lastReview.Summary, reviewHistory.String())

	// Run arbiter via claude CLI
	cmd := exec.Command("claude", "--dangerously-skip-permissions", "-p", prompt, "--model", policy.ArbiterModel, "--output-format", "text")
	cmd.Env = os.Environ()
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		log("arbiter failed: %s — escalating to archmage", err)
		storeAddComment(beadID, fmt.Sprintf("Arbiter failed: %s — needs human resolution", err))
		escalateHumanFailure(beadID, reviewerName, "arbiter-failure", err.Error())
		return nil
	}

	// Parse arbiter response
	var decision struct {
		Decision   string `json:"decision"`
		Reason     string `json:"reason"`
		SplitTasks []struct {
			Title       string `json:"title"`
			Description string `json:"description"`
		} `json:"split_tasks"`
	}

	outStr := strings.TrimSpace(string(out))
	if err := json.Unmarshal([]byte(outStr), &decision); err != nil {
		// Try extracting JSON from the response
		if idx := strings.Index(outStr, "{"); idx >= 0 {
			if end := strings.LastIndex(outStr, "}"); end > idx {
				json.Unmarshal([]byte(outStr[idx:end+1]), &decision)
			}
		}
	}

	log("arbiter decision: %s — %s", decision.Decision, decision.Reason)
	storeAddComment(beadID, fmt.Sprintf("Arbiter decision: %s — %s", decision.Decision, decision.Reason))
	storeAddLabel(beadID, "arbiter:"+decision.Decision)

	switch decision.Decision {
	case "merge":
		// Force-approve: same terminal path as sage approve.
		log("arbiter: force-approve, proceeding to merge")
		branch := hasLabel(bead, "feat-branch:")
		if branch == "" {
			branch = fmt.Sprintf("feat/%s", beadID)
		}
		repoPath, _, baseBranch, err := wizardResolveRepo(beadID)
		if err != nil {
			escalateHumanFailure(beadID, reviewerName, "repo-resolution",
				fmt.Sprintf("arbiter merge: %s", err.Error()))
			return nil
		}
		reviewHandleApproval(beadID, reviewerName, branch, baseBranch, repoPath, log)
		return nil

	case "split":
		// Merge approved work, create child beads for remaining issues, close original.
		// terminalSplit checks reviewHandleApproval error and aborts child-bead creation
		// if the merge fails — preventing child beads from being orphaned from unmerged code.
		var tasks []SplitTask
		for _, t := range decision.SplitTasks {
			tasks = append(tasks, SplitTask{Title: t.Title, Description: t.Description})
		}
		return terminalSplit(beadID, reviewerName, tasks, log)

	default: // "discard" or unknown
		// Delete branches and close as wontfix.
		return terminalDiscard(beadID, log)
	}
}


