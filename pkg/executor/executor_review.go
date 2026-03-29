package executor

import (
	"fmt"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/agent"
)

// executeReview dispatches a sage for review and handles the verdict.
func (e *Executor) executeReview(phase string, pc PhaseConfig) error {
	sageName := fmt.Sprintf("%s-sage", e.agentName)
	e.log("dispatching sage %s", sageName)

	extraArgs := []string{}
	if pc.VerdictOnly {
		extraArgs = append(extraArgs, "--verdict-only")
	}

	// Pass the shared staging worktree to the sage so it reviews in the same
	// worktree used for wave merges.
	if e.state.WorktreeDir != "" {
		extraArgs = append(extraArgs, "--worktree-dir", e.state.WorktreeDir)
	}

	started := time.Now()
	handle, err := e.deps.Spawner.Spawn(agent.SpawnConfig{
		Name:      sageName,
		BeadID:    e.beadID,
		Role:      agent.RoleSage,
		ExtraArgs: extraArgs,
	})
	if err != nil {
		return fmt.Errorf("spawn sage: %w", err)
	}
	waitErr := handle.Wait()
	e.recordAgentRun(sageName, e.beadID, "", pc.Model, "sage", started, waitErr)
	if waitErr != nil {
		e.log("sage exited: %s — checking verdict", waitErr)
	}

	// Read verdict from review-round child beads.
	bead, err := e.deps.GetBead(e.beadID)
	if err != nil {
		return fmt.Errorf("get bead: %w", err)
	}

	// Check review-approved label for backwards compat.
	if e.deps.ContainsLabel(bead, "review-approved") {
		e.log("approved")
		return nil
	}

	// Check review beads for verdict
	reviews, _ := e.deps.GetReviewBeads(e.beadID)
	lastVerdict := ""
	if len(reviews) > 0 {
		lastReview := reviews[len(reviews)-1]
		if lastReview.Status == "closed" {
			lastVerdict = e.deps.ReviewBeadVerdict(lastReview)
		}
	}

	if lastVerdict == "approve" {
		e.log("approved (via review bead)")
		return nil
	}

	if lastVerdict == "request_changes" {
		e.state.ReviewRounds++
		e.log("request changes (round %d)", e.state.ReviewRounds)

		// Check max rounds
		revPolicy := e.formula.GetRevisionPolicy()
		if e.state.ReviewRounds >= revPolicy.MaxRounds {
			e.log("max rounds reached — escalating to arbiter")
			lastReview := &Review{Verdict: "request_changes", Summary: "Max review rounds reached"}
			return e.deps.ReviewEscalateToArbiter(e.beadID, sageName, lastReview, revPolicy, e.log)
		}

		// Judgment (if enabled)
		if pc.Judgment {
			comments, _ := e.deps.GetComments(e.beadID)
			for i := len(comments) - 1; i >= 0; i-- {
				if strings.Contains(comments[i].Text, "request_changes") || strings.Contains(comments[i].Text, "Review round") {
					break
				}
			}

			e.log("judgment: agreeing with sage feedback")
			e.deps.AddComment(e.beadID, fmt.Sprintf("Executor judgment (round %d): agree — accepting sage feedback", e.state.ReviewRounds))
		}

		// Go back to implement phase
		if implPC, ok := e.formula.Phases["implement"]; ok {
			e.state.Phase = "implement"
			e.saveState()

			if implPC.GetDispatch() == "wave" {
				// Spawn a single review-fix apprentice.
				fixName := fmt.Sprintf("%s-fix-%d", e.agentName, e.state.ReviewRounds)
				fixArgs := []string{"--review-fix", "--apprentice"}
				if e.state.WorktreeDir != "" {
					fixArgs = append(fixArgs, "--worktree-dir", e.state.WorktreeDir)
				}
				fixStarted := time.Now()
				fh, ferr := e.deps.Spawner.Spawn(agent.SpawnConfig{
					Name:      fixName,
					BeadID:    e.beadID,
					Role:      agent.RoleApprentice,
					ExtraArgs: fixArgs,
				})
				if ferr != nil {
					return fmt.Errorf("spawn review-fix: %w", ferr)
				}
				fixWaitErr := fh.Wait()
				e.recordAgentRun(fixName, e.beadID, "", implPC.Model, "apprentice", fixStarted, fixWaitErr)
				if fixWaitErr != nil {
					return fmt.Errorf("review-fix apprentice failed: %w", fixWaitErr)
				}

				// Merge fix branch into the shared staging worktree.
				if e.state.StagingBranch != "" {
					fixBranch := e.resolveBranch(e.beadID)
					e.log("merging fix branch %s into staging %s", fixBranch, e.state.StagingBranch)
					stagingWt, wtErr := e.ensureStagingWorktree()
					if wtErr != nil {
						EscalateHumanFailure(e.beadID, e.agentName, "review-fix-merge-conflict",
							fmt.Sprintf("ensure staging worktree for fix merge: %s", wtErr), e.deps)
						return fmt.Errorf("ensure staging worktree for fix merge: %w", wtErr)
					}
					if mergeErr := stagingWt.MergeBranch(fixBranch, e.resolveConflicts); mergeErr != nil {
						EscalateHumanFailure(e.beadID, e.agentName, "review-fix-merge-conflict",
							fmt.Sprintf("merge fix branch %s into staging %s: %s", fixBranch, e.state.StagingBranch, mergeErr), e.deps)
						return fmt.Errorf("merge fix branch %s into staging %s: %w", fixBranch, e.state.StagingBranch, mergeErr)
					}
				}
			} else {
				if dirErr := e.executeDirect("implement", implPC); dirErr != nil {
					return fmt.Errorf("review-fix direct failed: %w", dirErr)
				}
				// Note: executeDirect now handles merging feat/<bead-id> into
				// the staging worktree internally, so no additional merge is
				// needed here.
			}

			// Return to review
			e.state.Phase = phase
			return e.executeReview(phase, pc) // recurse for next round
		}

		return fmt.Errorf("no implement phase for review-fix cycle")
	}

	// Check if bead was closed by sage
	if bead.Status == "closed" {
		e.log("bead closed by sage")
		return nil
	}

	return fmt.Errorf("no verdict found after sage review")
}
