package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// workshopReview handles the review phase of the wizard workshop.
// It dispatches a sage (reviewer), waits for the verdict, and makes
// judgment calls on review feedback.
func workshopReview(state *workshopState, spawner AgentSpawner) error {
	epicID := state.EpicID
	log := func(format string, a ...interface{}) {
		fmt.Fprintf(os.Stderr, "[workshop] "+format+"\n", a...)
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

	// 2. Read the verdict from the bead state
	// The sage (wizard-review) updates bead labels directly:
	// - review-approved → approved
	// - review-feedback → request changes
	bead, err := storeGetBead(epicID)
	if err != nil {
		return fmt.Errorf("get bead: %w", err)
	}

	if containsLabel(bead, "review-approved") {
		// Approved — transition to merge
		log("review approved — transitioning to merge")
		setPhase(epicID, "merge")
		state.Phase = "merge"
		return nil
	}

	if containsLabel(bead, "review-feedback") {
		// Request changes — the wizard makes a judgment call
		log("review requested changes (round %d)", state.ReviewRounds+1)
		state.ReviewRounds++

		// Load revision policy from formula
		var revPolicy RevisionPolicy
		formulaPath, fErr := FindFormula("spire-agent-work")
		if fErr == nil {
			if formula, pErr := LoadFormulaV2(formulaPath); pErr == nil {
				revPolicy = formula.GetRevisionPolicy()
			}
		}
		if revPolicy.MaxRounds == 0 {
			revPolicy = RevisionPolicy{MaxRounds: 3, ArbiterModel: "claude-opus-4-6"}
		}

		// Check if we've hit max rounds
		if state.ReviewRounds >= revPolicy.MaxRounds {
			log("max review rounds (%d) reached — escalating to arbiter", revPolicy.MaxRounds)
			// Use existing arbiter escalation
			lastReview := &Review{
				Verdict: "request_changes",
				Summary: "Max review rounds reached",
			}
			return reviewEscalateToArbiter(epicID, sageName, lastReview, revPolicy, log)
		}

		// Collect the sage's feedback from comments
		comments, _ := storeGetComments(epicID)
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

		judgment, err := workshopConsultClaude(state, judgmentPrompt)
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
		storeAddComment(epicID, fmt.Sprintf("Wizard judgment (round %d): %s — %s", state.ReviewRounds, decision.Decision, decision.Reason))

		switch decision.Decision {
		case "disagree":
			// Override the sage — proceed to merge
			log("overriding sage — transitioning to merge")
			storeRemoveLabel(epicID, "review-feedback")
			storeAddLabel(epicID, "review-approved")
			setPhase(epicID, "merge")
			state.Phase = "merge"
			return nil

		default: // "agree", "partial", or unknown — re-implement with feedback
			log("accepting feedback — dispatching review-fix")
			storeRemoveLabel(epicID, "review-feedback")
			setPhase(epicID, "implement")

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
			setPhase(epicID, "review")
			state.Phase = "review"
			return nil
		}
	}

	// No clear verdict in labels — the sage may have merged directly
	// Check if the bead is already closed
	if bead.Status == "closed" {
		log("epic already closed (sage may have merged directly)")
		state.Phase = "done"
		return nil
	}

	// Check current phase — sage may have transitioned
	currentPhase := getPhase(bead)
	if currentPhase != "" && currentPhase != "review" {
		log("phase changed to %s (sage transitioned)", currentPhase)
		state.Phase = currentPhase
		return nil
	}

	log("no verdict found — check manually")
	return fmt.Errorf("review completed without clear verdict")
}
