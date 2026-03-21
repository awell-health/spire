package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/repoconfig"
)

// runReviewMode handles standalone task review (artificer --mode=review).
// One-shot: review the bead's branch, merge or send feedback, exit.
func runReviewMode(beadID, model string, maxRounds int, commsDir, workspaceDir, stateDir string) {
	log.Printf("[review] starting standalone review for %s (model=%s)", beadID, model)

	// Ensure directories exist.
	for _, d := range []string{commsDir, workspaceDir, stateDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			log.Fatalf("failed to create directory %s: %v", d, err)
		}
	}

	// Initialize beads state.
	if err := initBeadsState(stateDir); err != nil {
		log.Fatalf("beads init failed: %v", err)
	}
	os.Setenv("BEADS_DIR", filepath.Join(stateDir, ".beads"))

	// Initialize workspace.
	repoCfg, err := initWorkspace(workspaceDir, stateDir)
	if err != nil {
		log.Fatalf("workspace init failed: %v", err)
	}

	// Load the bead.
	bead, err := loadBead(beadID)
	if err != nil {
		log.Fatalf("failed to load bead %s: %v", beadID, err)
	}
	log.Printf("[review] bead: %s — %s", bead.ID, bead.Title)

	// Resolve the feature branch from labels.
	branch := beadLabel(bead, "feat-branch:")
	if branch == "" {
		// Fallback to default pattern.
		branch = resolveBranch(beadID, repoCfg.Branch.Pattern)
	}

	// Fetch and verify branch exists.
	if err := gitFetch(workspaceDir); err != nil {
		log.Fatalf("git fetch failed: %v", err)
	}
	if !branchExists(workspaceDir, branch) {
		log.Fatalf("branch %s does not exist", branch)
	}

	// Run the review.
	result := reviewSingleBranch(workspaceDir, stateDir, bead, branch, model, maxRounds, repoCfg)

	log.Printf("[review] completed: %s", result)

	// Push bead state.
	bd("dolt", "push") //nolint:errcheck
}

// reviewSingleBranch reviews a single branch and handles the outcome.
// Used by both epic mode (for each child) and standalone review mode.
// Returns a string describing the outcome.
func reviewSingleBranch(workspaceDir, stateDir string, bead *Bead, branch, model string, maxRounds int, cfg *repoconfig.RepoConfig) string {
	base := resolveTargetBranch(bead, nil, cfg)

	// Checkout branch and run tests.
	if err := gitCheckout(workspaceDir, branch); err != nil {
		log.Printf("[review] failed to checkout %s: %v", branch, err)
		return "checkout_failed"
	}

	testResult := runTests(workspaceDir, branch, cfg)
	if !testResult.Passed {
		log.Printf("[review] tests failed on %s during %s", bead.ID, testResult.Stage)
		sendTestFailure(*bead, testResult) //nolint:errcheck
		return "test_failure"
	}

	// Get diff for review.
	diff, err := gitDiff(workspaceDir, base, branch)
	if err != nil {
		log.Printf("[review] failed to get diff for %s: %v", bead.ID, err)
		return "diff_failed"
	}

	filesChanged, linesAdded, linesRemoved, _ := gitDiffStats(workspaceDir, base, branch)
	reviewStart := time.Now()

	// Call Opus review.
	review, usage, err := callOpusReview(model, bead.Description, diff, *bead, testResult.Output, 0)
	if err != nil {
		log.Printf("[review] review call failed for %s: %v", bead.ID, err)
		return "review_failed"
	}

	log.Printf("[review] %s verdict: %s — %s", bead.ID, review.Verdict, review.Summary)
	recordRun(*bead, "", model, "success", review, usage, [3]int{filesChanged, linesAdded, linesRemoved}, reviewStart) //nolint:errcheck

	switch review.Verdict {
	case "approve":
		return handleStandaloneApproval(workspaceDir, bead, branch, review, cfg)

	case "request_changes":
		return handleStandaloneRequestChanges(bead, review)

	case "reject":
		return handleStandaloneRejection(bead, review)
	}

	return "unknown_verdict"
}

// handleStandaloneApproval merges an approved standalone task via staging branch.
func handleStandaloneApproval(workspaceDir string, bead *Bead, branch string, review *Review, cfg *repoconfig.RepoConfig) string {
	base := resolveTargetBranch(bead, nil, cfg)
	mergeMode := resolveMergeMode(bead, nil)

	// Create a PR first.
	prNum, err := createOrUpdatePR(workspaceDir, *bead, branch, review, cfg)
	if err != nil {
		log.Printf("[review] failed to create PR for %s: %v", bead.ID, err)
	}

	// Use staging branch pattern (same as epic flow, just one child).
	staging := &StagingBranch{
		Name:       fmt.Sprintf("staging/%s", bead.ID),
		TargetBase: base,
	}

	// Create staging from target.
	if err := gitCmd(workspaceDir, "fetch", "origin", base); err != nil {
		log.Printf("[review] fetch base failed: %v", err)
		return "merge_failed"
	}
	if err := gitCmd(workspaceDir, "checkout", "-B", staging.Name, "origin/"+base); err != nil {
		log.Printf("[review] create staging failed: %v", err)
		return "merge_failed"
	}

	// Merge the branch.
	mergeMsg := fmt.Sprintf("Merge %s: %s", bead.ID, bead.Title)
	autoResolved, mergeErr := gitMergeWithAutoResolve(workspaceDir, branch, mergeMsg)
	if mergeErr != nil {
		log.Printf("[review] merge failed for %s: %v", bead.ID, mergeErr)
		spireSend(resolveWizardAgent(*bead),
			fmt.Sprintf("Merge conflict on %s — please rebase on %s and push again", branch, base),
			bead.ID, 1) //nolint:errcheck
		gitCmd(workspaceDir, "checkout", base) //nolint:errcheck
		gitCmd(workspaceDir, "branch", "-D", staging.Name) //nolint:errcheck
		return "merge_conflict"
	}
	if autoResolved {
		log.Printf("[review] auto-resolved lock file conflicts for %s", bead.ID)
	}
	staging.MergedIDs = []string{bead.ID}

	// Run tests on staging.
	testResult := runTests(workspaceDir, staging.Name, cfg)
	if !testResult.Passed {
		log.Printf("[review] staging tests failed for %s during %s", bead.ID, testResult.Stage)
		bd("update", bead.ID, "--add-label", "escalation:staging-failure") //nolint:errcheck
		bdComment(bead.ID, fmt.Sprintf("Staging tests failed during %s:\n%s",
			testResult.Stage, truncate(testResult.Output, 2000))) //nolint:errcheck
		spireSend("steward",
			fmt.Sprintf("P0: staging test failure for %s during %s", bead.ID, testResult.Stage),
			bead.ID, 0) //nolint:errcheck
		return "staging_test_failure"
	}

	// Push to target.
	switch mergeMode {
	case "pr":
		// Push staging and create consolidated PR.
		if err := gitCmd(workspaceDir, "push", "-u", "origin", staging.Name); err != nil {
			log.Printf("[review] push staging failed: %v", err)
			return "push_failed"
		}
		var body strings.Builder
		body.WriteString("## Summary\n\n")
		body.WriteString(review.Summary)
		body.WriteString(fmt.Sprintf("\n\n**Bead**: `%s`\n", bead.ID))
		body.WriteString("\n---\nReviewed by the Artificer (Opus)\n")

		title := fmt.Sprintf("feat(%s): %s", bead.ID, bead.Title)
		if len(title) > 72 {
			title = title[:69] + "..."
		}
		ghOutput(workspaceDir, "pr", "create",
			"--head", staging.Name, "--base", base,
			"--title", title, "--body", body.String(),
			"--label", "artificer-approved") //nolint:errcheck
		log.Printf("[review] created PR for %s → %s (human review requested)", staging.Name, base)
	default:
		if err := fastForwardTarget(workspaceDir, staging); err != nil {
			log.Printf("[review] fast-forward failed: %v", err)
			return "push_failed"
		}
	}

	// Close PR if it exists.
	if prNum > 0 {
		mergePR(workspaceDir, prNum) //nolint:errcheck
	}

	// Cleanup.
	closeMoleculeStep(bead.ID, "review")
	closeMoleculeStep(bead.ID, "merge")
	bd("close", bead.ID) //nolint:errcheck
	spireSend("steward", fmt.Sprintf("Merged standalone %s into %s", bead.ID, base), bead.ID, 2) //nolint:errcheck

	// Remove review labels.
	bd("update", bead.ID, "--remove-label", "review-ready") //nolint:errcheck
	bd("update", bead.ID, "--remove-label", "review-assigned") //nolint:errcheck

	cleanupStagingBranch(workspaceDir, staging)
	gitCmd(workspaceDir, "push", "origin", "--delete", branch) //nolint:errcheck

	return "merged"
}

// handleStandaloneRequestChanges sends feedback to the wizard and updates labels.
func handleStandaloneRequestChanges(bead *Bead, review *Review) string {
	log.Printf("[review] requesting changes on %s", bead.ID)

	// Send review feedback to wizard.
	sendReviewToWizard(*bead, review) //nolint:errcheck

	// Update labels: remove review-ready, add review-feedback.
	bd("update", bead.ID, "--remove-label", "review-ready") //nolint:errcheck
	bd("update", bead.ID, "--remove-label", "review-assigned") //nolint:errcheck
	bd("update", bead.ID, "--add-label", "review-feedback") //nolint:errcheck

	bdComment(bead.ID, fmt.Sprintf("Review: request_changes — %s", review.Summary)) //nolint:errcheck

	return "request_changes"
}

// handleStandaloneRejection escalates to steward and closes the bead.
func handleStandaloneRejection(bead *Bead, review *Review) string {
	log.Printf("[review] rejecting %s", bead.ID)

	reportToSteward(*bead, review) //nolint:errcheck

	bd("update", bead.ID, "--remove-label", "review-ready") //nolint:errcheck
	bd("update", bead.ID, "--remove-label", "review-assigned") //nolint:errcheck
	bd("update", bead.ID, "--add-label", "rejected") //nolint:errcheck
	bd("close", bead.ID, "--reason", "Rejected during review") //nolint:errcheck

	return "rejected"
}
