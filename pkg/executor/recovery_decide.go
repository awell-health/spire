package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
)

// decideResponse is the JSON structure Claude must return from the decide step.
type decideResponse struct {
	Action     string  `json:"action"`
	Reasoning  string  `json:"reasoning"`
	Confidence float64 `json:"confidence"`
	NeedsHuman bool    `json:"needs_human"`
}

// actionRecoveryDecide is the ActionHandler for the "recovery.decide" opcode.
// It reads diagnostic context from collect_context outputs, queries learnings,
// builds a prompt for Claude, and returns a chosen recovery action.
func actionRecoveryDecide(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	return runRecoveryDecide(e, step, state)
}

func runRecoveryDecide(e *Executor, step StepConfig, state *GraphState) ActionResult {
	// 1. Extract source_bead_id and failure_class from collect_context outputs.
	ccOutputs := state.Steps["collect_context"].Outputs
	sourceBeadID := ccOutputs["source_bead_id"]
	failureClass := recovery.FailureClass(ccOutputs["failure_class"])

	// Fall back to recovery bead metadata if not in collect_context outputs.
	if sourceBeadID == "" {
		if bead, err := e.deps.GetBead(e.beadID); err == nil {
			sourceBeadID = bead.Meta(recovery.KeySourceBead)
		}
	}
	if string(failureClass) == "" {
		if bead, err := e.deps.GetBead(e.beadID); err == nil {
			failureClass = recovery.FailureClass(bead.Meta(recovery.KeyFailureClass))
		}
	}

	if sourceBeadID == "" || failureClass == "" {
		return ActionResult{Error: fmt.Errorf("decide: missing source_bead_id or failure_class from collect_context outputs and bead metadata")}
	}

	// 2. Re-run Diagnose to get current Diagnosis (ranked actions, git state, etc.).
	diag, err := recovery.Diagnose(sourceBeadID, buildRecoveryDepsFromExecutor(e))
	if err != nil {
		return ActionResult{Error: fmt.Errorf("decide: diagnose: %w", err)}
	}

	// 3. Query learnings.
	perBead, _ := recovery.GetRecoveryLearnings(sourceBeadID)
	crossBead, _ := recovery.GetCrossBeadLearnings(string(failureClass), 5)

	// 4. Build prompt and call Claude.
	prompt := buildDecidePrompt(diag, perBead, crossBead)
	raw, err := callClaudeForDecision(prompt)
	if err != nil {
		return ActionResult{Error: fmt.Errorf("decide: claude call: %w", err)}
	}

	// 5. Parse response.
	action, reasoning, confidence, needsHuman, err := parseDecideResponse(raw)
	if err != nil {
		return ActionResult{Error: fmt.Errorf("decide: parse response: %w", err)}
	}

	// 6. If low confidence or needs_human flagged, add needs-human label and signal halt.
	if needsHuman || confidence < 0.7 {
		_ = e.deps.AddLabel(state.BeadID, "needs-human")
		_ = e.deps.AddComment(state.BeadID, fmt.Sprintf(
			"Recovery agent needs human input (confidence=%.2f):\n%s", confidence, reasoning))
		return ActionResult{Outputs: map[string]string{
			"chosen_action":     "needs-human",
			"decide_reasoning":  reasoning,
			"decide_confidence": strconv.FormatFloat(confidence, 'f', 2, 64),
			"needs_human":       "true",
		}}
	}

	// 7. Append reasoning as bead comment and return.
	_ = e.deps.AddComment(state.BeadID, fmt.Sprintf(
		"Recovery decision: %s (confidence=%.2f)\n%s", action, confidence, reasoning))

	return ActionResult{Outputs: map[string]string{
		"chosen_action":     action,
		"decide_reasoning":  reasoning,
		"decide_confidence": strconv.FormatFloat(confidence, 'f', 2, 64),
		"needs_human":       "false",
	}}
}

func buildDecidePrompt(diag *recovery.Diagnosis, perBead []store.Bead, crossBead []store.Bead) string {
	var sb strings.Builder

	sb.WriteString("## Failure Diagnosis\n\n")
	sb.WriteString(fmt.Sprintf("Bead: %s — %s\n", diag.BeadID, diag.Title))
	sb.WriteString(fmt.Sprintf("Failure class: %s\n", diag.FailureMode))
	sb.WriteString(fmt.Sprintf("Interrupted label: %s\n", diag.InterruptLabel))
	sb.WriteString(fmt.Sprintf("Attempt count: %d\n", diag.AttemptCount))
	if diag.Git != nil {
		sb.WriteString(fmt.Sprintf("Git state: branch_exists=%v worktree_exists=%v\n",
			diag.Git.BranchExists, diag.Git.WorktreeExists))
	}
	sb.WriteString(fmt.Sprintf("Wizard running: %v\n", diag.WizardRunning))
	if len(diag.AlertBeads) > 0 {
		sb.WriteString(fmt.Sprintf("Alert beads: %d open\n", len(diag.AlertBeads)))
	}

	sb.WriteString("\n## Ranked Recovery Actions\n\n")
	for i, a := range diag.Actions {
		sb.WriteString(fmt.Sprintf("%d. %s — %s", i+1, a.Name, a.Description))
		if a.Destructive {
			sb.WriteString(" [DESTRUCTIVE]")
		}
		if a.Warning != "" {
			sb.WriteString(fmt.Sprintf(" WARNING: %s", a.Warning))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("\n## Prior Learnings (this bead)\n\n")
	if len(perBead) == 0 {
		sb.WriteString("None.\n")
	} else {
		for _, b := range perBead {
			sb.WriteString(fmt.Sprintf("- [%s] resolution=%s outcome=%s: %s\n",
				b.Meta(recovery.KeyLearningKey),
				b.Meta(recovery.KeyResolutionKind),
				b.Meta(recovery.KeyVerificationStatus),
				b.Meta(recovery.KeyLearningSummary)))
		}
	}

	sb.WriteString("\n## Similar Incidents (cross-bead, lower weight)\n\n")
	if len(crossBead) == 0 {
		sb.WriteString("None.\n")
	} else {
		for _, b := range crossBead {
			sb.WriteString(fmt.Sprintf("- [%s/%s] resolution=%s outcome=%s: %s\n",
				b.Meta(recovery.KeySourceBead),
				b.Meta(recovery.KeyLearningKey),
				b.Meta(recovery.KeyResolutionKind),
				b.Meta(recovery.KeyVerificationStatus),
				b.Meta(recovery.KeyLearningSummary)))
		}
	}

	sb.WriteString(`
## Your Task

Choose ONE recovery action from the ranked list above.
- Use prior learnings to inform your decision — per-bead learnings outweigh cross-bead.
- "do-nothing" is valid ONLY if the source bead is already clean (no active failure indicators).
- Prefer lower-ranked (less destructive) actions unless learnings show they failed before.
- If you are uncertain, set needs_human=true rather than guessing.

Respond with ONLY this JSON (no prose):
{
  "action": "<action-name>",
  "reasoning": "<1-2 sentences explaining your choice>",
  "confidence": 0.0,
  "needs_human": false
}

Valid action names: resummon, reset, reset-to-step, verify-clean, annotate-resolution, escalate, do-nothing
Confidence < 0.7 will automatically trigger needs_human=true.
`)
	return sb.String()
}

func callClaudeForDecision(prompt string) (string, error) {
	client := anthropic.NewClient() // reads ANTHROPIC_API_KEY from env
	msg, err := client.Messages.New(context.Background(), anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeOpus4_6,
		MaxTokens: int64(512),
		System: []anthropic.TextBlockParam{
			{Text: "You are a recovery agent for the Spire work coordination system. " +
				"You reason about which bounded recovery action to take based on diagnostic evidence. " +
				"You always respond in valid JSON."},
		},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})
	if err != nil {
		return "", err
	}
	if len(msg.Content) == 0 {
		return "", fmt.Errorf("empty response from Claude")
	}
	return msg.Content[0].Text, nil
}

func parseDecideResponse(raw string) (action, reasoning string, confidence float64, needsHuman bool, err error) {
	// Strip markdown code fences if present.
	raw = strings.TrimSpace(raw)
	if idx := strings.Index(raw, "{"); idx > 0 {
		raw = raw[idx:]
	}
	if idx := strings.LastIndex(raw, "}"); idx >= 0 && idx < len(raw)-1 {
		raw = raw[:idx+1]
	}
	var r decideResponse
	if err = json.Unmarshal([]byte(raw), &r); err != nil {
		return "", "", 0, false, fmt.Errorf("parse decide JSON: %w (raw: %s)", err, raw)
	}
	return r.Action, r.Reasoning, r.Confidence, r.NeedsHuman, nil
}

// buildRecoveryDepsFromExecutor creates a recovery.Deps from executor Deps,
// mapping store.Bead → recovery.DepBead and *beads.IssueWithDependencyMetadata
// → recovery.DepDependent.
func buildRecoveryDepsFromExecutor(e *Executor) *recovery.Deps {
	return &recovery.Deps{
		GetBead: func(id string) (recovery.DepBead, error) {
			b, err := e.deps.GetBead(id)
			if err != nil {
				return recovery.DepBead{}, err
			}
			return recovery.DepBead{
				ID:     b.ID,
				Title:  b.Title,
				Status: b.Status,
				Labels: b.Labels,
				Parent: b.Parent,
			}, nil
		},
		GetChildren: func(parentID string) ([]recovery.DepBead, error) {
			children, err := e.deps.GetChildren(parentID)
			if err != nil {
				return nil, err
			}
			result := make([]recovery.DepBead, len(children))
			for i, c := range children {
				result[i] = recovery.DepBead{
					ID:     c.ID,
					Title:  c.Title,
					Status: c.Status,
					Labels: c.Labels,
					Parent: c.Parent,
				}
			}
			return result, nil
		},
		GetDependentsWithMeta: func(id string) ([]recovery.DepDependent, error) {
			if e.deps.GetDependentsWithMeta == nil {
				return nil, nil
			}
			deps, err := e.deps.GetDependentsWithMeta(id)
			if err != nil {
				return nil, err
			}
			result := make([]recovery.DepDependent, len(deps))
			for i, d := range deps {
				result[i] = recovery.DepDependent{
					ID:             d.ID,
					Title:          d.Title,
					Status:         string(d.Status),
					Labels:         d.Labels,
					DependencyType: string(d.DependencyType),
				}
			}
			return result, nil
		},
		AddComment: e.deps.AddComment,
	}
}
