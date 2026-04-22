package recovery

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/awell-health/spire/pkg/store"
)

// DefaultMaxRecoveryAttempts is the number of recovery attempts before
// automatic escalation. Callers override via Deps.MaxAttempts.
const DefaultMaxRecoveryAttempts = 3

// Attempt is the recovery-attempt history record consumed by Decide. It
// aliases store.RecoveryAttempt so callers can pass the raw store rows
// without an extra projection step.
type Attempt = store.RecoveryAttempt

// DecideResult is the structured JSON response Claude returns for the
// agentic decision path. Exported so the executor-side wrapper can copy
// the fields into legacy step outputs (chosen_action, confidence, etc.).
type DecideResult struct {
	ChosenAction    string  `json:"chosen_action"`
	Confidence      float64 `json:"confidence"`
	Reasoning       string  `json:"reasoning"`
	NeedsHuman      bool    `json:"needs_human"`
	ExpectedOutcome string  `json:"expected_outcome"`
}

// Decide produces a typed RepairPlan from a diagnosis + attempt history.
// The priority order is:
//
//	(0) auto-escalate when total attempts ≥ MaxAttempts
//	(a) human guidance from bead comments
//	(a2) promoted mechanical recipe replay
//	(c) Claude-backed decision via deps.ClaudeRunner
//	(d) fallback to resummon when Claude is unavailable
//
// Claude is the agent-first default: once the attempt-budget / human /
// promoted-recipe gates are past, Decide routes through Claude. Git-state
// signals (conflicts, behind-base divergence, dirty worktree) are not
// short-circuited — they flow into the diagnosis context (ContextSummary)
// that Claude receives so the agent can reason about them explicitly.
//
// Rich context (git state, worktree state, conflicted files, human
// comments, ranked actions, learnings, wizard log tail, context summary)
// flows through Deps rather than the function signature. See Deps doc
// comments for each field.
func Decide(ctx context.Context, diagnosis Diagnosis, history []Attempt, deps Deps) (RepairPlan, error) {
	// Derive totals from history so callers don't have to double-wire
	// derived counters.
	repeatedFailures := map[string]int{}
	for _, a := range history {
		if a.Outcome == "failure" {
			repeatedFailures[a.Action]++
		}
	}
	totalAttempts := len(history)

	maxAttempts := deps.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxRecoveryAttempts
	}

	logf := deps.Logf
	if logf == nil {
		logf = func(string, ...interface{}) {}
	}

	// (0) Auto-escalate when attempt budget is exhausted.
	if totalAttempts >= maxAttempts {
		logf("recovery: decide: auto-escalate — total attempts %d >= max %d", totalAttempts, maxAttempts)
		return planForAction(
			"escalate",
			1.0,
			fmt.Sprintf("Auto-escalate: %d recovery attempts exhausted (max %d)", totalAttempts, maxAttempts),
			true,
		), nil
	}

	// (a) Human guidance in bead comments wins over everything except the
	// attempt-budget guard.
	if guided := parseHumanGuidance(deps.HumanComments, repeatedFailures); guided != "" {
		logf("recovery: decide: human guidance detected → %s", guided)
		return planForAction(
			guided,
			0.90,
			"Human guidance from bead comment",
			false,
		), nil
	}

	// (a2) Promoted mechanical recipe replay. Once a failure_signature has
	// accumulated N consecutive clean agentic recoveries (each codified as
	// a mechanical_recipe), dispatch the recipe directly. A single failure
	// demotes the chain — see MarkDemoted on failure paths.
	if sig := deps.FailureSignature; sig != "" && deps.PromotionThreshold != nil {
		threshold := deps.PromotionThreshold(sig)
		if promState, err := LookupPromotionState(sig, threshold); err != nil {
			logf("recovery: decide: promotion lookup for %s failed (continuing with agentic default): %v", sig, err)
		} else if promState.Promoted {
			action, params := recipeDispatch(promState.Recipe)
			if action != "" && repeatedFailures[action] < 2 {
				logf("recovery: decide: promoted mechanical recipe for %s → %s (count=%d threshold=%d)",
					sig, action, promState.Count, promState.Threshold)
				plan := planForAction(
					action,
					0.95,
					fmt.Sprintf("Promoted mechanical recipe for %s (%d clean outcomes ≥ threshold %d)", sig, promState.Count, promState.Threshold),
					false,
				)
				plan.Mode = RepairModeRecipe
				if len(params) > 0 {
					if plan.Params == nil {
						plan.Params = map[string]string{}
					}
					for k, v := range params {
						plan.Params[k] = v
					}
				}
				return plan, nil
			}
			if action != "" {
				logf("recovery: decide: promoted recipe %s has %d prior failures, falling through",
					action, repeatedFailures[action])
			}
		}
	}

	// (c) Claude-backed decision. Git-state signals (conflicts, behind-base
	// divergence, dirty worktree) are not preempted here — they reach Claude
	// via the diagnosis context (Deps.ContextSummary) and let the agent
	// reason about them alongside the wizard log, attempt history, and
	// learnings.
	if deps.ClaudeRunner == nil {
		// (d) No Claude runner → fallback to resummon.
		logf("recovery: decide: ClaudeRunner not available, falling back to resummon")
		return planForAction("resummon", 0.50, "Fallback: ClaudeRunner unavailable", false), nil
	}

	var stats *store.LearningStats
	if deps.LearningStats != nil {
		failureClass := string(diagnosis.FailureMode)
		if failureClass != "" {
			if s, err := deps.LearningStats(failureClass); err == nil {
				stats = s
			}
		}
	}

	prompt := buildDecidePrompt(&promptInputs{
		Diagnosis:      &diagnosis,
		RankedActions:  deps.RankedActions,
		BeadLearnings:  deps.BeadLearnings,
		CrossLearnings: deps.CrossLearnings,
		WizardLogTail:  deps.WizardLogTail,
	}, deps.TriageCount, stats)
	if deps.ContextSummary != "" {
		prompt += "\n\n## Full Recovery Context (git-aware)\n\n" + deps.ContextSummary
	}

	args := []string{
		"--dangerously-skip-permissions",
		"-p", prompt,
		"--output-format", "text",
		"--max-turns", "1",
	}

	out, err := deps.ClaudeRunner(args, "recovery-decide")
	if err != nil {
		// (d) Claude call failed → fallback to resummon.
		logf("recovery: decide: claude call failed, falling back to resummon: %v", err)
		return planForAction(
			"resummon",
			0.40,
			fmt.Sprintf("Fallback: Claude call failed: %v", err),
			false,
		), nil
	}

	var result DecideResult
	if err := parseJSONFromClaude(out, &result); err != nil {
		return RepairPlan{}, fmt.Errorf("decide: parse claude response: %w", err)
	}

	// Side-channel: hand the raw Claude response to the executor-side
	// wrapper before we mutate it (override on repeated failures, confidence
	// threshold, etc.). Transitional; disappears with the legacy output
	// surface in Chunk 6.
	if deps.CaptureDecideResult != nil {
		deps.CaptureDecideResult(result)
	}

	// Validate Claude's chosen action against repeated failures.
	if repeatedFailures[result.ChosenAction] >= 2 {
		originalAction := result.ChosenAction
		logf("recovery: decide: Claude chose %q but it has %d prior failures — overriding to escalate",
			originalAction, repeatedFailures[originalAction])
		result.ChosenAction = "escalate"
		result.Reasoning = fmt.Sprintf("Overridden: original choice %q has %d prior failures", originalAction, repeatedFailures[originalAction])
		result.NeedsHuman = true
	}

	// Apply confidence threshold.
	if result.Confidence < 0.7 {
		result.NeedsHuman = true
	}

	// Persist expected_outcome on the recovery bead so the learn step can
	// compare actual vs predicted outcome.
	if result.ExpectedOutcome != "" && deps.SetRecoveryBeadMeta != nil {
		_ = deps.SetRecoveryBeadMeta(map[string]string{
			KeyExpectedOutcome: result.ExpectedOutcome,
		})
	}

	// If needs_human, post a comment summarizing the decision.
	if result.NeedsHuman && deps.AddRecoveryBeadComment != nil {
		_ = deps.AddRecoveryBeadComment(fmt.Sprintf(
			"Recovery decide: needs human intervention.\n\nChosen action: %s\nConfidence: %.2f\nReasoning: %s\nExpected outcome: %s",
			result.ChosenAction, result.Confidence, result.Reasoning, result.ExpectedOutcome,
		))
		logf("recovery: decide needs-human (confidence=%.2f, action=%s)", result.Confidence, result.ChosenAction)
	} else {
		logf("recovery: decide chose %q (confidence=%.2f)", result.ChosenAction, result.Confidence)
	}

	plan := planForAction(result.ChosenAction, result.Confidence, result.Reasoning, result.NeedsHuman)
	return plan, nil
}

// planForAction builds a RepairPlan from the classic action-string
// vocabulary. It maps the action to its canonical RepairMode and stamps
// Confidence / Reason. Callers may further mutate Params / Workspace /
// Verify as needed (e.g., the promoted-recipe path adds recipe params).
func planForAction(action string, confidence float64, reason string, needsHuman bool) RepairPlan {
	mode := actionToRepairMode(action)
	if needsHuman {
		mode = RepairModeEscalate
	}
	return RepairPlan{
		Mode:       mode,
		Action:     action,
		Confidence: confidence,
		Reason:     reason,
	}
}

// actionToRepairMode maps the historic action-string vocabulary to the
// typed RepairMode enum introduced in spi-9ql7l. The mapping is
// exhaustive for actions Decide can emit; unknown actions fall back to
// the agentic worker bucket so callers still get a dispatchable plan.
func actionToRepairMode(action string) RepairMode {
	switch action {
	case "escalate":
		return RepairModeEscalate
	case "do_nothing", "verify_clean", "verify-clean":
		return RepairModeNoop
	case "rebase-onto-base", "rebuild", "cherry-pick", "reset-hard", "reset_hard",
		"reset-to-step", "reset_to_step":
		return RepairModeMechanical
	case "resolve-conflicts", "resummon", "reset", "triage", "targeted-fix":
		return RepairModeWorker
	default:
		return RepairModeWorker
	}
}

// parseHumanGuidance scans human comments for action keywords and returns
// the matching recovery action name, or "" if no guidance is detected.
// Avoids suggesting actions that have repeatedly failed.
//
// Two-gate acceptance:
//   - Gate A (imperative opener): the comment's first non-whitespace token
//     (lowercased, leading markdown/quote punctuation stripped, trailing
//     punctuation stripped) must be in imperativeOpeners. This keeps system
//     failure-report text ("recovery action \"rebase-onto-base\" failed",
//     "Cleric execute errored — scheduling retry: rebase conflict ...") from
//     being mistaken for a human imperative.
//   - Gate B (keyword match): the comment must still contain an action
//     keyword from guidanceMap; that match determines which action to return.
//
// False-negatives on phrasings like "Please rebase..." / "Let's rebuild..."
// are a deliberate tradeoff — tighter signal kills self-amplification loops.
func parseHumanGuidance(comments []string, repeatedFailures map[string]int) string {
	guidanceMap := map[string]string{
		"rebase":            "rebase-onto-base",
		"try rebase":        "rebase-onto-base",
		"rebase onto main":  "rebase-onto-base",
		"rebase onto base":  "rebase-onto-base",
		"cherry-pick":       "cherry-pick",
		"cherry pick":       "cherry-pick",
		"resolve conflicts": "resolve-conflicts",
		"resolve conflict":  "resolve-conflicts",
		"rebuild":           "rebuild",
		"try rebuild":       "rebuild",
		"resummon":          "resummon",
		"re-summon":         "resummon",
		"try again":         "resummon",
		"reset":             "reset-to-step",
		"reset to step":     "reset-to-step",
		"escalate":          "escalate",
		"fix":               "targeted-fix",
		"targeted fix":      "targeted-fix",
	}

	// Scan comments from most recent to oldest.
	for i := len(comments) - 1; i >= 0; i-- {
		if !hasImperativeOpener(comments[i]) {
			continue
		}
		lower := strings.ToLower(strings.TrimSpace(comments[i]))
		for keyword, action := range guidanceMap {
			if strings.Contains(lower, keyword) {
				// Don't follow guidance toward an action that keeps failing.
				if repeatedFailures != nil && repeatedFailures[action] >= 2 {
					continue
				}
				return action
			}
		}
	}
	return ""
}

// imperativeOpeners is the set of first tokens accepted as evidence that a
// comment is a human imperative (not a system failure report).
var imperativeOpeners = map[string]bool{
	"try":         true,
	"retry":       true,
	"redo":        true,
	"rebase":      true,
	"resolve":     true,
	"fix":         true,
	"cherry":      true,
	"cherry-pick": true,
	"revert":      true,
	"rebuild":     true,
	"resummon":    true,
	"re-summon":   true,
	"reset":       true,
	"escalate":    true,
	"targeted":    true,
	"merge":       true,
	"apply":       true,
}

// hasImperativeOpener reports whether the comment's first non-whitespace
// token is an accepted imperative. The check normalizes leading markdown
// bullets / blockquote markers / quote chars and trailing punctuation, and
// is case-insensitive.
func hasImperativeOpener(comment string) bool {
	// Strip leading markdown/quote/list decoration that agents or humans may
	// add ahead of an imperative ("- try ...", "> rebase ...", "\"try ...\"").
	trim := strings.TrimLeft(comment, " \t\r\n-*>#`\"'")
	if trim == "" {
		return false
	}
	// First whitespace-delimited token.
	end := strings.IndexFunc(trim, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	var tok string
	if end == -1 {
		tok = trim
	} else {
		tok = trim[:end]
	}
	// Drop trailing punctuation so "rebase." / "fix:" / "try," match.
	tok = strings.TrimRight(tok, ".,:;!?)\"'`")
	return imperativeOpeners[strings.ToLower(tok)]
}

// recipeDispatch extracts the executable action + params from a recipe
// for use by the decide step. Only builtin recipes are directly dispatchable
// today — sequence recipes would require planner support and are captured
// here for forward compatibility but treated as non-dispatchable (returns "").
func recipeDispatch(r *MechanicalRecipe) (action string, params map[string]string) {
	if r == nil {
		return "", nil
	}
	if r.Kind != RecipeKindBuiltin || r.Action == "" {
		return "", nil
	}
	return r.Action, r.Params
}

// promptInputs bundles the data buildDecidePrompt consumes. Kept as a
// struct (vs. many positional args) so the signature stays stable as
// prompt sections are added.
type promptInputs struct {
	Diagnosis      *Diagnosis
	RankedActions  []RecoveryAction
	BeadLearnings  []store.RecoveryLearning
	CrossLearnings []store.RecoveryLearning
	WizardLogTail  string
}

// buildDecidePrompt constructs the Claude prompt for the decide step.
// triageCount is the number of triage attempts already made on this
// recovery bead.
func buildDecidePrompt(cc *promptInputs, triageCount int, stats *store.LearningStats) string {
	var b strings.Builder
	b.WriteString("You are a cleric agent for Spire, an AI agent coordination system.\n\n")
	b.WriteString("A bead (work item) has been interrupted and needs recovery. Analyze the diagnosis and choose the best recovery action.\n\n")

	// Diagnosis context.
	if cc.Diagnosis != nil {
		diagJSON, _ := json.MarshalIndent(cc.Diagnosis, "", "  ")
		b.WriteString("## Diagnosis\n```json\n")
		b.Write(diagJSON)
		b.WriteString("\n```\n\n")
	}

	// Wizard log output.
	if cc.WizardLogTail != "" {
		b.WriteString("## Wizard Log Output (last ~100 lines)\n")
		b.WriteString("This is the actual output from the wizard that failed. Use it to understand WHY the failure occurred.\n\n")
		b.WriteString("```\n")
		b.WriteString(cc.WizardLogTail)
		if !strings.HasSuffix(cc.WizardLogTail, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("```\n\n")
	}

	// Ranked actions.
	if len(cc.RankedActions) > 0 {
		b.WriteString("## Available Actions (mechanically ranked)\n```json\n")
		actJSON, _ := json.MarshalIndent(cc.RankedActions, "", "  ")
		b.Write(actJSON)
		b.WriteString("\n```\n\n")
	}

	// Historical outcome statistics.
	if stats != nil && stats.TotalRecoveries > 0 {
		b.WriteString("## Historical Outcome Statistics\n\n")
		b.WriteString(fmt.Sprintf("Based on %d prior recoveries for failure class `%s`:\n\n",
			stats.TotalRecoveries, stats.FailureClass))
		b.WriteString("| Action | Attempts | Success Rate | Clean | Dirty | Relapsed |\n")
		b.WriteString("|--------|----------|-------------|-------|-------|----------|\n")
		for _, as := range stats.ActionStats {
			b.WriteString(fmt.Sprintf("| %s | %d | %.0f%% | %d | %d | %d |\n",
				as.ResolutionKind, as.Total, as.SuccessRate*100,
				as.CleanCount, as.DirtyCount, as.RelapsedCount))
		}
		if stats.PredictionAccuracy > 0 {
			b.WriteString(fmt.Sprintf("\nDecide agent prediction accuracy: %.0f%% (when expected_outcome was set).\n",
				stats.PredictionAccuracy*100))
		}
		b.WriteString("\nWeight your action choice by historical success rates. Prefer actions with >70% success rate for this failure class unless the specific circumstances call for a different approach.\n\n")
	}

	// Bead learnings.
	if len(cc.BeadLearnings) > 0 {
		b.WriteString("## Prior experience with this exact bead\n```json\n")
		blJSON, _ := json.MarshalIndent(cc.BeadLearnings, "", "  ")
		b.Write(blJSON)
		b.WriteString("\n```\n\n")
	}

	// Cross-bead learnings.
	if len(cc.CrossLearnings) > 0 {
		b.WriteString("## Similar incidents across the system (lower weight)\n```json\n")
		clJSON, _ := json.MarshalIndent(cc.CrossLearnings, "", "  ")
		b.Write(clJSON)
		b.WriteString("\n```\n\n")
	}

	b.WriteString("## Instructions\n")
	b.WriteString("Choose a recovery action. Output ONLY a JSON object with these fields:\n")
	b.WriteString("- `chosen_action`: one of \"reset\", \"resummon\", \"do_nothing\", \"escalate\", \"reset_to_step\", \"verify_clean\", \"triage\"\n")
	b.WriteString("- `confidence`: 0.0 to 1.0 — how confident you are this action will resolve the issue\n")
	b.WriteString("- `reasoning`: brief explanation of why you chose this action\n")
	b.WriteString("- `needs_human`: set to true if confidence < 0.7\n")
	b.WriteString("- `expected_outcome`: what you expect to happen after this action is taken. Include: what should succeed, how to verify it worked, and under what conditions this action would be the wrong choice.\n\n")

	// Log-referencing reasoning requirements.
	if cc.WizardLogTail != "" {
		b.WriteString("### CRITICAL: Log-Based Reasoning\n")
		b.WriteString("Wizard log output is available above. Your reasoning MUST reference specific errors or output lines from the log.\n")
		b.WriteString("Distinguish between:\n")
		b.WriteString("- **Infrastructure failures** (missing commands, env setup, dependency install) that resummon/reset may fix\n")
		b.WriteString("- **Code-level failures** (test assertions, missing env vars, type errors) that require code changes and resummon CANNOT fix\n\n")
	}

	// Triage guidance.
	b.WriteString("### Triage Action\n")
	b.WriteString("Choose `triage` when:\n")
	b.WriteString("- Failure class is `step-failure` at implement or review-fix steps\n")
	b.WriteString("- Test output shows clear code-level failures (assertion errors, type errors, compilation errors)\n")
	b.WriteString("- The worktree still exists (see diagnosis git state)\n")
	b.WriteString("- This is the first or second triage attempt (max 2)\n\n")
	b.WriteString("Do NOT choose `triage` for:\n")
	b.WriteString("- Infrastructure failures (missing commands, env setup, dependency install)\n")
	b.WriteString("- When the worktree has been cleaned up\n")
	b.WriteString("- When triage has already been tried twice\n\n")

	// Triage budget context.
	triageRemaining := 2 - triageCount
	if triageRemaining < 0 {
		triageRemaining = 0
	}
	b.WriteString(fmt.Sprintf("**Triage budget:** %d of 2 attempts used, %d remaining.\n", triageCount, triageRemaining))
	if triageCount >= 2 {
		b.WriteString("Triage budget is exhausted — do NOT choose `triage`.\n")
	}
	b.WriteString("\n")

	// Worktree existence from diagnosis.
	if cc.Diagnosis != nil && cc.Diagnosis.Git != nil {
		if cc.Diagnosis.Git.WorktreeExists {
			b.WriteString("**Worktree exists:** yes (triage is possible)\n\n")
		} else {
			b.WriteString("**Worktree exists:** no (triage is NOT possible — worktree was cleaned up)\n\n")
		}
	}

	// Relapse awareness.
	b.WriteString("### Relapse Awareness\n")
	b.WriteString("If prior learnings show outcome=\"relapsed\" for an action on this bead, do NOT choose that action again unless you can explain from the log why this time is different.\n")
	b.WriteString("A \"relapsed\" outcome means a prior recovery with that action appeared to succeed but the bead failed again with the same failure class within 24 hours.\n\n")

	b.WriteString("### Action Guide\n")
	b.WriteString("- `resummon`: Soft reset — clears interrupt labels, sets bead to open. Wizard resumes on the SAME branch with existing code. Use for transient/infrastructure failures.\n")
	b.WriteString("- `reset-hard`: Destructive reset — kills wizard, deletes worktree, branches, graph state, and internal DAG beads. Fresh start from scratch. Use when code is fundamentally broken and resuming would repeat the same mistakes.\n")
	b.WriteString("- `reset_to_step`: Rewinds to a specific step. Use for targeted re-execution of a single phase.\n")
	b.WriteString("- `escalate`: Marks for human intervention. Use when confidence is low or the problem is outside agent capability.\n")
	b.WriteString("- `do_nothing`: Valid if the source bead already appears clean.\n\n")
	b.WriteString("Output ONLY the JSON object, no markdown fences, no explanation outside the JSON.\n")

	return b.String()
}

// parseJSONFromClaude extracts a JSON object from Claude's output, handling
// potential markdown fences and surrounding text.
func parseJSONFromClaude(out []byte, v interface{}) error {
	text := strings.TrimSpace(string(out))

	// Strip markdown fences if present.
	if strings.HasPrefix(text, "```") {
		lines := strings.Split(text, "\n")
		var jsonLines []string
		inBlock := false
		for _, line := range lines {
			if strings.HasPrefix(line, "```") {
				inBlock = !inBlock
				continue
			}
			if inBlock {
				jsonLines = append(jsonLines, line)
			}
		}
		text = strings.Join(jsonLines, "\n")
	}

	// Find JSON object boundaries.
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start >= 0 && end > start {
		text = text[start : end+1]
	}

	return json.Unmarshal([]byte(text), v)
}

// ctx is accepted for future cancellation/tracing hooks. Today Decide
// doesn't plumb it through the Claude runner, but keeping it in the
// signature matches the design §6 contract and avoids future breaking
// changes when we wire context.DeadlineExceeded through ClaudeRunner.
var _ = context.TODO
