package main

import (
	"fmt"
	"strings"
)

// executeReview dispatches a sage for review and handles the verdict.
func (e *formulaExecutor) executeReview(phase string, pc PhaseConfig) error {
	sageName := fmt.Sprintf("%s-sage", e.agentName)
	e.log("dispatching sage %s", sageName)

	extraArgs := []string{}
	if pc.VerdictOnly {
		extraArgs = append(extraArgs, "--verdict-only")
	}

	// Pass the shared staging worktree to the sage so it reviews in the same
	// worktree used for wave merges — no separate checkout needed.
	if e.state.WorktreeDir != "" {
		extraArgs = append(extraArgs, "--worktree-dir", e.state.WorktreeDir)
	}

	handle, err := e.spawner.Spawn(SpawnConfig{
		Name:      sageName,
		BeadID:    e.beadID,
		Role:      RoleSage,
		ExtraArgs: extraArgs,
	})
	if err != nil {
		return fmt.Errorf("spawn sage: %w", err)
	}
	if err := handle.Wait(); err != nil {
		e.log("sage exited: %s — checking verdict", err)
	}

	// Read verdict from review-round child beads.
	bead, err := storeGetBead(e.beadID)
	if err != nil {
		return fmt.Errorf("get bead: %w", err)
	}

	// Check review-approved label for backwards compat (verdict-only mode still sets it).
	if containsLabel(bead, "review-approved") {
		e.log("approved")
		return nil // advance to next phase (merge)
	}

	// Check review beads for verdict
	reviews, _ := storeGetReviewBeads(e.beadID)
	lastVerdict := ""
	if len(reviews) > 0 {
		lastReview := reviews[len(reviews)-1]
		if lastReview.Status == "closed" {
			lastVerdict = reviewBeadVerdict(lastReview)
		}
	}

	if lastVerdict == "approve" {
		e.log("approved (via review bead)")
		return nil // advance to next phase (merge)
	}

	if lastVerdict == "request_changes" {
		e.state.ReviewRounds++
		e.log("request changes (round %d)", e.state.ReviewRounds)

		// Check max rounds
		revPolicy := e.formula.GetRevisionPolicy()
		if e.state.ReviewRounds >= revPolicy.MaxRounds {
			e.log("max rounds reached — escalating to arbiter")
			lastReview := &Review{Verdict: "request_changes", Summary: "Max review rounds reached"}
			return reviewEscalateToArbiter(e.beadID, sageName, lastReview, revPolicy, e.log)
		}

		// Judgment (if enabled): log agreement with sage
		if pc.Judgment {
			// Collect feedback from latest comment
			comments, _ := storeGetComments(e.beadID)
			for i := len(comments) - 1; i >= 0; i-- {
				if strings.Contains(comments[i].Text, "request_changes") || strings.Contains(comments[i].Text, "Review round") {
					break
				}
			}

			// Simple judgment: for now, always agree with sage
			// TODO: invoke Claude for judgment when session management is implemented
			e.log("judgment: agreeing with sage feedback")
			storeAddComment(e.beadID, fmt.Sprintf("Executor judgment (round %d): agree — accepting sage feedback", e.state.ReviewRounds))
		}

		// Go back to implement phase

		// Find the implement phase to re-execute
		if implPC, ok := e.formula.Phases["implement"]; ok {
			e.state.Phase = "implement"
			e.saveState()

			if implPC.GetDispatch() == "wave" {
				// For wave mode: re-running waves won't help (subtasks closed).
				// Spawn a single review-fix apprentice.
				fixName := fmt.Sprintf("%s-fix-%d", e.agentName, e.state.ReviewRounds)
				fh, ferr := e.spawner.Spawn(SpawnConfig{
					Name:      fixName,
					BeadID:    e.beadID,
					Role:      RoleApprentice,
					ExtraArgs: []string{"--review-fix", "--apprentice"},
				})
				if ferr != nil {
					return fmt.Errorf("spawn review-fix: %w", ferr)
				}
				if waitErr := fh.Wait(); waitErr != nil {
					return fmt.Errorf("review-fix apprentice failed: %w", waitErr)
				}

				// Merge fix branch into the shared staging worktree so the sage
				// reviews the updated code. The worktree persists across all phases.
				if e.state.StagingBranch != "" {
					fixBranch := fmt.Sprintf("feat/%s", e.beadID)
					e.log("merging fix branch %s into staging %s", fixBranch, e.state.StagingBranch)
					stagingWt, wtErr := e.ensureStagingWorktree()
					if wtErr != nil {
						return fmt.Errorf("ensure staging worktree for fix merge: %w", wtErr)
					}
					if mergeErr := stagingWt.MergeBranch(fixBranch, e.resolveConflicts); mergeErr != nil {
						return fmt.Errorf("merge fix branch %s into staging %s: %w", fixBranch, e.state.StagingBranch, mergeErr)
					}
				}
			} else {
				if dirErr := e.executeDirect("implement", implPC); dirErr != nil {
					return fmt.Errorf("review-fix direct failed: %w", dirErr)
				}
			}

			// Return to review
			e.state.Phase = phase
			return e.executeReview(phase, pc) // recurse for next round
		}

		return fmt.Errorf("no implement phase for review-fix cycle")
	}

	// Check if bead was closed by sage (shouldn't happen with verdict-only)
	if bead.Status == "closed" {
		e.log("bead closed by sage")
		return nil
	}

	return fmt.Errorf("no verdict found after sage review")
}
