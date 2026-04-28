package executor

import (
	"fmt"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/cleric"
	"github.com/awell-health/spire/pkg/promptctx"
	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// wizardClericDecide invokes Claude with the cleric prompt-builder. The
// recovery bead's caused-by edge points to the source bead; the cleric
// reasons about the source bead's task, the failure step's tool-call
// detail, and the peer recovery history (related edges) to propose a
// recovery action. The agent emits a ProposedAction JSON to stdout; we
// capture stdout and return it as the step's "result" output for the
// next step (cleric.publish) to parse and persist.
//
// Cleric runtime (spi-hhkozk).
func (e *Executor) wizardClericDecide(stepName, model string, maxTurns int) ([]byte, error) {
	bead, err := e.deps.GetBead(e.beadID)
	if err != nil {
		return nil, fmt.Errorf("get recovery bead %s: %w", e.beadID, err)
	}
	if bead.Type != "recovery" {
		// Defensive: cleric formula should only run on recovery beads.
		return nil, fmt.Errorf("cleric.decide on non-recovery bead %s (type=%s)", e.beadID, bead.Type)
	}

	prompt, err := e.buildClericPrompt(bead)
	if err != nil {
		return nil, fmt.Errorf("build cleric prompt: %w", err)
	}

	resolvedModel := repoconfig.ResolveModel(model, e.repoModel())

	e.log("invoking Claude for cleric decide (max_turns=%d)", maxTurns)
	started := time.Now()
	args := []string{
		"--dangerously-skip-permissions",
		"-p", prompt,
		"--model", resolvedModel,
		"--output-format", "text",
	}
	if maxTurns > 0 {
		args = append(args, "--max-turns", fmt.Sprintf("%d", maxTurns))
	}
	out, err := e.runClaude(args, "cleric-decide")
	e.recordAgentRun(e.agentName, e.beadID, "", resolvedModel, "cleric", "decide", started, err,
		withParentRun(e.currentRunID))
	if err != nil {
		return out, fmt.Errorf("claude cleric decide: %w", err)
	}

	if strings.TrimSpace(string(out)) == "" {
		return out, fmt.Errorf("claude produced empty cleric proposal")
	}

	return out, nil
}

// buildClericPrompt assembles the cleric-decide prompt for the recovery
// bead. Composition:
//
//   - Recovery context: the bead's title + description (which carries the
//     wizard's failure diagnosis) + every comment to date.
//   - Source bead context: title + description + status + comments,
//     fetched via the caused-by edge.
//   - Peer recovery history: every prior recovery bead linked via
//     `related`, summarized for prior proposal/outcome context.
//   - Inline graph context (via promptctx.BuildPromptSuffix with cleric=true):
//     closed neighbors of the recovery bead, plus the cleric graph-walk
//     instruction.
//   - Action manifest: the verbs the cleric is allowed to propose.
//   - Task framing + JSON shape spec: instructs the cleric to emit a
//     single ProposedAction JSON object on stdout, no prose, no fences.
//
// The prompt is deliberately short. The cleric reads the graph for
// detail; the prompt's job is framing + schema.
func (e *Executor) buildClericPrompt(bead Bead) (string, error) {
	var b strings.Builder

	fmt.Fprintf(&b, `You are the Spire cleric — a one-shot recovery agent. A wizard step on a source bead failed; this recovery bead captures the failure. Read the context below and propose a recovery action as JSON.

## Recovery bead
ID: %s
Title: %s
Status: %s
Failure description:
%s
`, bead.ID, bead.Title, bead.Status, bead.Description)

	// Recovery bead comments (failure timeline).
	if comments, _ := e.deps.GetComments(bead.ID); len(comments) > 0 {
		b.WriteString("\n### Recovery bead comments\n")
		for _, c := range comments {
			fmt.Fprintf(&b, "- [%s] %s\n", c.Author, c.Text)
		}
	}

	// Source bead via caused-by edge.
	sourceID := ""
	if depList, derr := e.deps.GetDepsWithMeta(bead.ID); derr == nil {
		for _, d := range depList {
			if d == nil {
				continue
			}
			if string(d.DependencyType) == store.DepCausedBy {
				sourceID = d.ID
				break
			}
		}
	}
	if sourceID != "" {
		if src, err := e.deps.GetBead(sourceID); err == nil {
			fmt.Fprintf(&b, "\n## Source bead (the work being recovered)\nID: %s\nType: %s\nStatus: %s\nTitle: %s\nDescription:\n%s\n",
				src.ID, src.Type, src.Status, src.Title, src.Description)
			if scs, _ := e.deps.GetComments(src.ID); len(scs) > 0 {
				b.WriteString("\n### Source bead comments\n")
				for _, c := range scs {
					fmt.Fprintf(&b, "- [%s] %s\n", c.Author, c.Text)
				}
			}
		}
	}

	// Peer recovery history via `related` edges.
	if depList, derr := e.deps.GetDepsWithMeta(bead.ID); derr == nil {
		var peers []*beads.IssueWithDependencyMetadata
		for _, d := range depList {
			if d == nil {
				continue
			}
			if string(d.DependencyType) == string(beads.DepRelated) && string(d.IssueType) == "recovery" {
				peers = append(peers, d)
			}
		}
		if len(peers) > 0 {
			b.WriteString("\n## Peer recovery history (most recent first)\n")
			for _, p := range peers {
				fmt.Fprintf(&b, "- %s (status=%s): %s\n", p.ID, string(p.Status), p.Title)
				if peerBead, err := e.deps.GetBead(p.ID); err == nil {
					proposal := peerBead.Meta(cleric.MetadataKeyProposal)
					if proposal != "" {
						fmt.Fprintf(&b, "  prior proposal: %s\n", truncatePromptString(proposal, 300))
					}
					outcome := peerBead.Meta(cleric.MetadataKeyOutcome)
					if outcome != "" {
						fmt.Fprintf(&b, "  outcome: %s\n", outcome)
					}
				}
			}
		}
	}

	// Inline graph context block (closed neighbors) + cleric graph-walk
	// stop criterion. Uses the same surface as wizard/apprentice/sage so
	// the cleric inherits parity with the rest of the role registry.
	suffix := promptctx.BuildPromptSuffix(bead.ID, promptctx.StoreDeps(), true)
	b.WriteString("\n")
	b.WriteString(suffix)

	// Action manifest — the verbs the cleric may propose.
	b.WriteString("\n## Action vocabulary\nPropose exactly one of these verbs in `verb`:\n")
	for _, name := range manifestKeysSorted() {
		entry := cleric.Manifest()[name]
		fmt.Fprintf(&b, "- `%s` — %s", entry.Verb, entry.Description)
		if len(entry.ArgsSchema) > 0 {
			b.WriteString(" Args: ")
			first := true
			for argName, argSchema := range entry.ArgsSchema {
				if !first {
					b.WriteString(", ")
				}
				first = false
				if argSchema.Required {
					fmt.Fprintf(&b, "%s (required) — %s", argName, argSchema.Description)
				} else {
					fmt.Fprintf(&b, "%s — %s", argName, argSchema.Description)
				}
			}
		}
		b.WriteString("\n")
	}

	// Task framing + JSON shape.
	b.WriteString(`
## Task framing
Propose a recovery action as JSON. Do not execute. Do not write code. Output ONLY the JSON object below — no prose, no Markdown fences.

## JSON shape

` + "```" + `json
{
  "verb": "<one of the verbs above>",
  "args": { /* per-verb arg map; omit if no args */ },
  "reasoning": "<why this action; one short paragraph>",
  "confidence": 0.0,
  "destructive": false,
  "failure_class": "<short tag identifying the failure shape, e.g. step-failure:implement, merge-conflict, build-error>"
}
` + "```" + `

Constraints:
- "verb" must match one of the action-vocabulary entries verbatim.
- "reasoning" and "failure_class" are required.
- "confidence" is in [0.0, 1.0].
- The first character of your output must be '{'. The last character must be '}'.
`)

	return b.String(), nil
}

// actionClericDecide is the wizard.run flow="cleric-decide" handler.
// Spawns Claude with the cleric prompt, captures stdout, and returns it
// as the step's "result" output for cleric.publish to parse.
//
// Unlike apprentice/sage spawns, the cleric is in-process: it runs as a
// direct Claude invocation through e.runClaude rather than as a forked
// agent subprocess. The cleric pod (cluster-mode) is the same model —
// no PVC, no workspace, just a Claude call. v1 keeps both modes
// in-process so the cluster path is added in a follow-up without
// reshuffling the formula.
func actionClericDecide(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	out, err := e.wizardClericDecide(stepName, step.Model, step.MaxTurns)
	if err != nil {
		return ActionResult{
			Outputs: map[string]string{"result": "error"},
			Error:   err,
		}
	}
	return ActionResult{
		Outputs: map[string]string{
			"result": string(out),
		},
	}
}

// truncatePromptString shortens long strings for the prompt context. Single-
// line truncation with ellipsis. Used for prior-proposal summaries so the
// cleric prompt doesn't blow past Claude's context window when peer
// recoveries pile up.
func truncatePromptString(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// manifestKeysSorted returns the verb names from cleric.Manifest() in a
// stable order so the prompt is deterministic across runs.
func manifestKeysSorted() []string {
	m := cleric.Manifest()
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
