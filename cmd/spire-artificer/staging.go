package main

import (
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"github.com/awell-health/spire/pkg/repoconfig"
)

// StagingBranch tracks the staging branch state during a merge cycle.
type StagingBranch struct {
	Name       string   // "staging/{epic-id}" or "staging/{bead-id}"
	TargetBase string   // resolved target branch
	MergedIDs  []string // successfully merged bead IDs
}

// autoResolvableLockFiles are files where we can safely accept --theirs on conflict.
var autoResolvableLockFiles = map[string]bool{
	"package-lock.json": true,
	"pnpm-lock.yaml":    true,
	"go.sum":            true,
	"yarn.lock":         true,
}

// processStagingMerge replaces the old direct-to-base merge flow.
// It creates a staging branch, batches approved merges, runs tests, then
// fast-forwards the target (or creates a PR if merge-mode is "pr").
func processStagingMerge(dir string, states map[string]*ChildState, cfg *repoconfig.RepoConfig, epicID string, epic *Bead) error {
	// Collect approved children in the merge queue.
	var ready []string
	for id, cs := range states {
		if cs.InMergeQueue && cs.Verdict == "approved" {
			ready = append(ready, id)
		}
	}
	if len(ready) == 0 {
		return nil
	}

	// Sort by dependency order.
	ordered, err := getDependencyOrder(epicID, ready)
	if err != nil {
		log.Printf("[staging] warning: could not resolve dependencies, using original order: %v", err)
		ordered = ready
	}

	base := resolveTargetBranch(epic, nil, cfg)
	mergeMode := resolveMergeMode(epic, nil)

	staging := &StagingBranch{
		Name:       fmt.Sprintf("staging/%s", epicID),
		TargetBase: base,
	}

	// Step 1: Create staging branch from target.
	log.Printf("[staging] creating %s from origin/%s", staging.Name, base)
	if err := gitCmd(dir, "fetch", "origin", base); err != nil {
		return fmt.Errorf("fetch base: %w", err)
	}
	if err := gitCmd(dir, "checkout", "-B", staging.Name, "origin/"+base); err != nil {
		return fmt.Errorf("create staging branch: %w", err)
	}

	// Step 2: Batch merge each approved child.
	for _, childID := range ordered {
		cs := states[childID]
		branch := cs.Branch
		mergeMsg := fmt.Sprintf("Merge %s into staging", childID)

		log.Printf("[staging] merging %s (%s)", childID, branch)

		autoResolved, mergeErr := gitMergeWithAutoResolve(dir, branch, mergeMsg)
		if mergeErr != nil {
			// Merge failed — mark child, skip, continue with remaining.
			log.Printf("[staging] merge conflict on %s (could not auto-resolve): %v", childID, mergeErr)
			cs.InMergeQueue = false
			cs.Verdict = "request_changes"
			bd("update", childID, "--add-label", "escalation:merge-conflict") //nolint:errcheck
			spireSend(resolveWizardAgent(Bead{ID: childID}),
				fmt.Sprintf("Merge conflict on %s — please rebase on %s and push again", branch, base),
				childID, 1) //nolint:errcheck
			continue
		}
		if autoResolved {
			log.Printf("[staging] auto-resolved lock file conflicts for %s", childID)
		}

		staging.MergedIDs = append(staging.MergedIDs, childID)
	}

	if len(staging.MergedIDs) == 0 {
		// Nothing merged — clean up staging branch.
		gitCmd(dir, "checkout", base) //nolint:errcheck
		gitCmd(dir, "branch", "-D", staging.Name) //nolint:errcheck
		return nil
	}

	// Step 3: Run tests on staging.
	log.Printf("[staging] running tests on %s (%d children merged)", staging.Name, len(staging.MergedIDs))
	testResult := runTests(dir, staging.Name, cfg)
	if !testResult.Passed {
		log.Printf("[staging] tests failed during %s — rolling back last merge", testResult.Stage)
		return handleStagingTestFailure(dir, staging, states, testResult, cfg, epicID, epic)
	}

	// Step 4: Handle merge mode.
	switch mergeMode {
	case "pr":
		log.Printf("[staging] creating consolidated PR from %s → %s", staging.Name, base)
		if err := createConsolidatedPR(dir, staging, states, cfg, epicID); err != nil {
			return fmt.Errorf("create consolidated PR: %w", err)
		}
	default: // "merge"
		log.Printf("[staging] fast-forwarding %s → %s", staging.Name, base)
		if err := fastForwardTarget(dir, staging); err != nil {
			return fmt.Errorf("fast-forward: %w", err)
		}
	}

	// Step 5: Mark merged children.
	for _, childID := range staging.MergedIDs {
		cs := states[childID]
		cs.Verdict = "merged"
		cs.InMergeQueue = false

		// Close individual PRs if they exist.
		if cs.PRNumber > 0 {
			if err := mergePR(dir, cs.PRNumber); err != nil {
				log.Printf("[staging] warning: could not close PR #%d for %s: %v", cs.PRNumber, childID, err)
			}
		}

		// Close molecule steps.
		closeMoleculeStep(childID, "review")
		closeMoleculeStep(childID, "merge")

		// Close the bead.
		bd("close", childID) //nolint:errcheck
		spireSend("steward",
			fmt.Sprintf("Merged %s into %s", childID, staging.TargetBase),
			childID, 2) //nolint:errcheck

		log.Printf("[staging] merged %s", childID)
	}

	// Step 6: Cleanup staging branch.
	cleanupStagingBranch(dir, staging)

	// Delete merged feature branches.
	for _, childID := range staging.MergedIDs {
		cs := states[childID]
		gitCmd(dir, "push", "origin", "--delete", cs.Branch) //nolint:errcheck
	}

	return nil
}

// handleStagingTestFailure rolls back the last merge, escalates, and continues
// with remaining children.
func handleStagingTestFailure(dir string, staging *StagingBranch, states map[string]*ChildState, testResult *TestResult, cfg *repoconfig.RepoConfig, epicID string, epic *Bead) error {
	if len(staging.MergedIDs) == 0 {
		return nil
	}

	// Identify the last merged child as the likely culprit.
	failedID := staging.MergedIDs[len(staging.MergedIDs)-1]
	staging.MergedIDs = staging.MergedIDs[:len(staging.MergedIDs)-1]

	// Revert the last merge.
	log.Printf("[staging] reverting merge of %s", failedID)
	if err := gitCmd(dir, "reset", "--hard", "HEAD~1"); err != nil {
		log.Printf("[staging] warning: failed to revert: %v", err)
	}

	// Escalate.
	cs := states[failedID]
	cs.InMergeQueue = false
	cs.Verdict = "request_changes"
	bd("update", failedID, "--add-label", "escalation:staging-failure") //nolint:errcheck
	bdComment(failedID, fmt.Sprintf("Staging tests failed during %s:\n%s",
		testResult.Stage, truncate(testResult.Output, 2000))) //nolint:errcheck
	spireSend("steward",
		fmt.Sprintf("P0: staging test failure for %s during %s. Branch preserved.", failedID, testResult.Stage),
		failedID, 0) //nolint:errcheck

	// If there are remaining merged children, re-test and proceed.
	if len(staging.MergedIDs) > 0 {
		log.Printf("[staging] re-testing with %d remaining children after removing %s", len(staging.MergedIDs), failedID)
		reTestResult := runTests(dir, staging.Name, cfg)
		if reTestResult.Passed {
			base := resolveTargetBranch(epic, nil, cfg)
			mergeMode := resolveMergeMode(epic, nil)
			switch mergeMode {
			case "pr":
				if err := createConsolidatedPR(dir, staging, states, cfg, epicID); err != nil {
					return fmt.Errorf("create consolidated PR after rollback: %w", err)
				}
			default:
				if err := fastForwardTarget(dir, staging); err != nil {
					return fmt.Errorf("fast-forward after rollback: %w", err)
				}
			}
			_ = base

			// Mark remaining as merged.
			for _, childID := range staging.MergedIDs {
				cs := states[childID]
				cs.Verdict = "merged"
				cs.InMergeQueue = false
				if cs.PRNumber > 0 {
					mergePR(dir, cs.PRNumber) //nolint:errcheck
				}
				closeMoleculeStep(childID, "review")
				closeMoleculeStep(childID, "merge")
				bd("close", childID)                                                                    //nolint:errcheck
				spireSend("steward", fmt.Sprintf("Merged %s into %s", childID, staging.TargetBase), childID, 2) //nolint:errcheck
			}
			cleanupStagingBranch(dir, staging)
		} else {
			log.Printf("[staging] tests still failing after rollback — keeping staging branch for investigation")
		}
	}

	return nil
}

// fastForwardTarget pushes the staging branch as the target base.
func fastForwardTarget(dir string, staging *StagingBranch) error {
	// Push staging as target: git push origin staging/{id}:{target}
	refspec := fmt.Sprintf("%s:%s", staging.Name, staging.TargetBase)
	if err := gitCmd(dir, "push", "origin", refspec); err != nil {
		// Target may have moved ahead — try rebasing.
		log.Printf("[staging] fast-forward failed, attempting rebase")
		if err := gitCmd(dir, "fetch", "origin", staging.TargetBase); err != nil {
			return fmt.Errorf("fetch for rebase: %w", err)
		}
		if err := gitCmd(dir, "rebase", "origin/"+staging.TargetBase); err != nil {
			return fmt.Errorf("rebase staging: %w", err)
		}
		if err := gitCmd(dir, "push", "origin", refspec); err != nil {
			return fmt.Errorf("push after rebase: %w", err)
		}
	}
	return nil
}

// createConsolidatedPR creates a single PR from the staging branch to the target,
// listing all included children in the body.
func createConsolidatedPR(dir string, staging *StagingBranch, states map[string]*ChildState, cfg *repoconfig.RepoConfig, epicID string) error {
	// Push staging branch to remote.
	if err := gitCmd(dir, "push", "-u", "origin", staging.Name); err != nil {
		return fmt.Errorf("push staging: %w", err)
	}

	title := fmt.Sprintf("feat(%s): merge %d children", epicID, len(staging.MergedIDs))
	if len(title) > 72 {
		title = title[:69] + "..."
	}

	var body strings.Builder
	body.WriteString("## Summary\n\n")
	body.WriteString(fmt.Sprintf("Consolidated merge of %d children from epic `%s`.\n\n", len(staging.MergedIDs), epicID))
	body.WriteString("### Included\n\n")
	for _, childID := range staging.MergedIDs {
		cs := states[childID]
		body.WriteString(fmt.Sprintf("- `%s` (branch: `%s`)\n", childID, cs.Branch))
	}
	body.WriteString("\n---\nReviewed by the Artificer (Opus)\n")

	args := []string{
		"pr", "create",
		"--head", staging.Name,
		"--base", staging.TargetBase,
		"--title", title,
		"--body", body.String(),
	}

	labels := append([]string{}, cfg.PR.Labels...)
	labels = append(labels, "artificer-approved")
	for _, l := range labels {
		args = append(args, "--label", l)
	}
	for _, r := range cfg.PR.Reviewers {
		args = append(args, "--reviewer", r)
	}

	out, err := ghOutput(dir, args...)
	if err != nil {
		return fmt.Errorf("gh pr create: %w", err)
	}

	prNum := extractPRNumber(out)
	log.Printf("[staging] created consolidated PR #%d: %s", prNum, out)
	return nil
}

// cleanupStagingBranch deletes the staging branch locally and on the remote.
func cleanupStagingBranch(dir string, staging *StagingBranch) {
	gitCmd(dir, "checkout", staging.TargetBase) //nolint:errcheck
	gitCmd(dir, "branch", "-D", staging.Name) //nolint:errcheck
	gitCmd(dir, "push", "origin", "--delete", staging.Name) //nolint:errcheck
}

// gitMergeWithAutoResolve attempts a merge and auto-resolves lock file conflicts.
// Returns (autoResolved, error). If non-trivial conflicts remain, it aborts and returns an error.
func gitMergeWithAutoResolve(dir, branch, message string) (bool, error) {
	// Attempt normal merge.
	if err := gitMerge(dir, branch, message); err == nil {
		return false, nil
	}

	// Merge failed — check if all conflicts are auto-resolvable.
	conflicted, err := gitOutput(dir, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		gitCmd(dir, "merge", "--abort") //nolint:errcheck
		return false, fmt.Errorf("could not list conflicts: %w", err)
	}

	files := strings.Split(strings.TrimSpace(conflicted), "\n")
	for _, f := range files {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		base := filepath.Base(f)
		if !autoResolvableLockFiles[base] {
			gitCmd(dir, "merge", "--abort") //nolint:errcheck
			return false, fmt.Errorf("non-trivial conflict in %s", f)
		}
	}

	// All conflicts are in lock files — accept theirs.
	for _, f := range files {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if err := gitCmd(dir, "checkout", "--theirs", f); err != nil {
			gitCmd(dir, "merge", "--abort") //nolint:errcheck
			return false, fmt.Errorf("auto-resolve %s: %w", f, err)
		}
		if err := gitCmd(dir, "add", f); err != nil {
			gitCmd(dir, "merge", "--abort") //nolint:errcheck
			return false, fmt.Errorf("stage %s: %w", f, err)
		}
	}

	// Complete the merge.
	if err := gitCmd(dir, "commit", "--no-edit"); err != nil {
		gitCmd(dir, "merge", "--abort") //nolint:errcheck
		return false, fmt.Errorf("complete merge: %w", err)
	}

	return true, nil
}

// closeMoleculeStep closes a workflow molecule step for a bead.
func closeMoleculeStep(beadID, stepName string) {
	// Query for workflow molecule.
	var beads []Bead
	if err := bdJSON(&beads, "list", "--label", "workflow:"+beadID, "--status=open"); err != nil {
		return
	}

	for _, b := range beads {
		// Find step children matching the step name.
		var children []Bead
		if err := bdJSON(&children, "children", b.ID); err != nil {
			continue
		}
		for _, child := range children {
			if strings.HasPrefix(strings.ToLower(child.Title), stepName) {
				bd("close", child.ID) //nolint:errcheck
				log.Printf("[staging] closed molecule step %s (%s) for %s", stepName, child.ID, beadID)
			}
		}
	}
}
