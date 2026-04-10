package wizard

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/awell-health/spire/pkg/repoconfig"
)

// EpicReview handles the review phase of the wizard epic orchestration.
// It dispatches a sage (reviewer), waits for the verdict, and makes
// judgment calls on review feedback.
func EpicReview(state *EpicState, spawner Backend, deps *Deps) error {
	epicID := state.EpicID
	log := func(format string, a ...interface{}) {
		fmt.Fprintf(os.Stderr, "[wizard-epic] "+format+"\n", a...)
	}

	// 1. Dispatch a sage (reviewer)
	sageName := fmt.Sprintf("sage-%s", epicID)
	log("dispatching sage %s for review", sageName)

	handle, spawnErr := spawner.Spawn(SpawnConfig{
		Name:   sageName,
		BeadID: epicID,
		Role:   RoleSage,
	})
	if spawnErr != nil {
		return fmt.Errorf("spawn sage: %w", spawnErr)
	}

	// Run the sage synchronously — it reviews and posts a verdict message
	if err := handle.Wait(); err != nil {
		// The sage may have posted a verdict even if it exited non-zero
		// (e.g., it requested changes and spawned a wizard-run which we don't want)
		log("sage exited: %s — checking for verdict", err)
	}

	// 2. Read the verdict from review-round child beads.
	// The sage (wizard-review) creates and closes review-round beads with verdicts.
	bead, err := deps.GetBead(epicID)
	if err != nil {
		return fmt.Errorf("get bead: %w", err)
	}

	// Also check review-approved label for backwards compat (verdict-only mode still sets it).
	if deps.ContainsLabel(bead, "review-approved") {
		log("review approved — transitioning to merge")
		state.Phase = "merge"
		return nil
	}

	// Check review beads for verdict
	reviews, _ := deps.GetReviewBeads(epicID)
	lastVerdict := ""
	if len(reviews) > 0 {
		lastReview := reviews[len(reviews)-1]
		if lastReview.Status == "closed" {
			lastVerdict = deps.ReviewBeadVerdict(lastReview)
		}
	}

	if lastVerdict == "approve" {
		log("review approved (via review bead) — transitioning to merge")
		state.Phase = "merge"
		return nil
	}

	if lastVerdict == "request_changes" {
		// Request changes — the wizard makes a judgment call
		log("review requested changes (round %d)", state.ReviewRounds+1)
		state.ReviewRounds++

		// Default revision policy (v2 formula loading removed).
		revPolicy := RevisionPolicy{MaxRounds: 3, ArbiterModel: repoconfig.DefaultReviewModel}

		// Check if we've hit max rounds
		if state.ReviewRounds >= revPolicy.MaxRounds {
			log("max review rounds (%d) reached — escalating to arbiter", revPolicy.MaxRounds)
			// Use existing arbiter escalation
			lastReview := &Review{
				Verdict: "request_changes",
				Summary: "Max review rounds reached",
			}
			return ReviewEscalateToArbiter(epicID, sageName, lastReview, revPolicy, deps, log)
		}

		// Collect the sage's feedback from comments
		comments, _ := deps.GetComments(epicID)
		var feedback string
		for i := len(comments) - 1; i >= 0; i-- {
			if strings.Contains(comments[i].Text, "Review round") || strings.Contains(comments[i].Text, "request_changes") {
				feedback = comments[i].Text
				break
			}
		}

		// Consult Claude for judgment
		judgmentPrompt := fmt.Sprintf(`You are a wizard — a per-epic orchestrator reviewing feedback from a sage (code reviewer).

Epic: %s
Review round: %d
Sage feedback:
%s

Based on this feedback, decide:
(a) AGREE — the sage is right, the implementation needs fixing
(b) DISAGREE — the sage is wrong or nitpicking, proceed to merge
(c) PARTIAL — some points are valid, others are not

Respond with ONLY a JSON object:
{"decision": "agree|disagree|partial", "reason": "brief explanation"}`, epicID, state.ReviewRounds, feedback)

		judgment, err := EpicConsultClaude(state, judgmentPrompt)
		if err != nil {
			log("claude judgment failed: %s — defaulting to agree", err)
			judgment = `{"decision": "agree", "reason": "judgment unavailable"}`
		}

		// Parse judgment
		var decision struct {
			Decision string `json:"decision"`
			Reason   string `json:"reason"`
		}
		json.Unmarshal([]byte(judgment), &decision)

		log("judgment: %s — %s", decision.Decision, decision.Reason)
		deps.AddComment(epicID, fmt.Sprintf("Wizard judgment (round %d): %s — %s", state.ReviewRounds, decision.Decision, decision.Reason))

		switch decision.Decision {
		case "disagree":
			// Override the sage — proceed to merge
			log("overriding sage — transitioning to merge")
			deps.AddLabel(epicID, "review-approved")
			state.Phase = "merge"
			return nil

		default: // "agree", "partial", or unknown — re-implement with feedback
			log("accepting feedback — dispatching review-fix")

			// Spawn wizard-run --review-fix directly (subtasks are already closed,
			// so the wave system would produce zero waves)
			fixName := fmt.Sprintf("apprentice-%s-fix-%d", epicID, state.ReviewRounds)
			fixHandle, fixErr := spawner.Spawn(SpawnConfig{
				Name:      fixName,
				BeadID:    epicID,
				Role:      RoleApprentice,
				ExtraArgs: []string{"--review-fix"},
			})
			if fixErr != nil {
				log("review-fix spawn failed: %s", fixErr)
			} else if err := fixHandle.Wait(); err != nil {
				log("review-fix failed: %s", err)
			}

			// After fix, go back to review
			state.Phase = "review"
			return nil
		}
	}

	// No clear verdict in review beads — the sage may have merged directly
	// Check if the bead is already closed
	if bead.Status == "closed" {
		log("epic already closed (sage may have merged directly)")
		state.Phase = "done"
		return nil
	}

	// Check current phase — sage may have transitioned
	currentPhase := deps.GetPhase(bead)
	if currentPhase != "" && currentPhase != "review" {
		log("phase changed to %s (sage transitioned)", currentPhase)
		state.Phase = currentPhase
		return nil
	}

	log("no verdict found — check manually")
	return fmt.Errorf("review completed without clear verdict")
}
