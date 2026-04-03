package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/executor"
	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/observability"
	"github.com/spf13/cobra"
	"github.com/steveyegge/beads"
)

var focusCmd = &cobra.Command{
	Use:   "focus <bead-id>",
	Short: "Assemble read-only context for a task",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmdFocus(args)
	},
}

func cmdFocus(args []string) error {
	if err := requireDolt(); err != nil {
		return err
	}

	if len(args) < 1 {
		return fmt.Errorf("usage: spire focus <bead-id>")
	}
	id := args[0]

	// 1. Fetch the target bead
	target, err := storeGetBead(id)
	if err != nil {
		return fmt.Errorf("focus %s: %w", id, err)
	}

	// 2. Resolve formula (v3 only).
	anyFormula, _, _ := ResolveFormulaAny(target)
	var graph *formula.FormulaStepGraph
	if anyFormula != nil {
		graph, _ = anyFormula.(*formula.FormulaStepGraph)
	}

	return focusV3(target, graph)
}

// focusV3 renders the v3 step-graph based focus output.
func focusV3(target Bead, graph *formula.FormulaStepGraph) error {
	id := target.ID

	// --- Load graph state ---
	wizardName := resolveWizardName(id)
	gs, _ := executor.LoadGraphState(wizardName, configDir)

	// --- Determine interrupted status ---
	interrupted := isInterruptedBead(target.Labels)
	interruptLabel := ""
	if interrupted {
		interruptLabel = getInterruptLabel(target.Labels)
	}

	// --- Header ---
	fmt.Printf("--- Task %s ---\n", target.ID)
	fmt.Printf("Title: %s\n", target.Title)
	fmt.Printf("Status: %s\n", target.Status)
	fmt.Printf("Priority: P%d\n", target.Priority)
	if graph != nil {
		fmt.Printf("Formula: %s\n", graph.Name)
	}
	if target.Description != "" {
		fmt.Printf("Description: %s\n", target.Description)
	}
	if interrupted {
		fmt.Printf("!! INTERRUPTED: %s\n", interruptLabel)
	}
	fmt.Println()

	// --- Step Graph ---
	if graph != nil {
		fmt.Printf("--- Step Graph ---\n")
		ordered := topoSortSteps(graph)
		for _, stepName := range ordered {
			stepCfg := graph.Steps[stepName]
			renderStepLine(stepName, stepCfg, gs)
		}
		fmt.Println()
	}

	// --- Workspace section ---
	if gs != nil && len(gs.Workspaces) > 0 {
		fmt.Printf("--- Workspace ---\n")
		for name, ws := range gs.Workspaces {
			wsStatus := ws.Status
			if wsStatus == "" {
				wsStatus = "pending"
			}
			fmt.Printf("  %s: %s (%s, %s)\n", name, ws.Branch, ws.Kind, wsStatus)
		}
		fmt.Println()
	}

	// --- Failure Context (interrupted beads only) ---
	if interrupted && gs != nil {
		failedStep, failedState := findFailedStep(gs)
		if failedStep != "" {
			fmt.Printf("--- Failure Context ---\n")
			fmt.Printf("  Step: %s\n", failedStep)
			stepCfg, ok := graph.Steps[failedStep]
			if ok {
				if stepCfg.Action != "" {
					fmt.Printf("  Action: %s\n", stepCfg.Action)
				}
				if stepCfg.Flow != "" {
					fmt.Printf("  Flow: %s\n", stepCfg.Flow)
				}
				if stepCfg.Workspace != "" {
					fmt.Printf("  Workspace: %s\n", stepCfg.Workspace)
				}
			}
			if errMsg, ok := failedState.Outputs["error"]; ok {
				fmt.Printf("  Error: %s\n", errMsg)
			}
			fmt.Println()
		}

		// --- Recovery Options ---
		fmt.Printf("--- Recovery Options ---\n")
		if failedStep != "" {
			fmt.Printf("  spire reset --to %s %s   # retry from failed step\n", failedStep, id)
		}
		fmt.Printf("  spire reset --hard %s          # full restart\n", id)
		fmt.Printf("  spire resummon %s               # restart from scratch\n", id)
		fmt.Println()
	}

	// Shared tail sections
	focusTail(target, id)
	return nil
}

// focusTail renders shared context sections: recovery work,
// related deps, messages, thread context, comments, and agent runs.
func focusTail(target Bead, id string) {
	// Recovery work section for interrupted beads.
	if isInterruptedBead(target.Labels) {
		dependents, dErr := storeGetDependentsWithMetaFunc(id)
		if dErr == nil {
			if section := formatRecoverySection(dependents); section != "" {
				fmt.Print(section)
			}
		}
	}

	// Related beads (from dependency graph)
	relDeps, relErr := storeGetDepsWithMeta(id)
	if relErr != nil {
		fmt.Fprintf(os.Stderr, "warning: could not load related deps for %s: %v\n", id, relErr)
	}
	for _, dep := range relDeps {
		if dep.DependencyType == beads.DepParentChild {
			continue // parent-child shown elsewhere
		}
		fmt.Printf("--- %s: %s ---\n", dep.DependencyType, dep.ID)
		fmt.Printf("Title: %s\n", dep.Title)
		fmt.Printf("Status: %s\n", dep.Status)
		if dep.Description != "" {
			fmt.Printf("Description: %s\n", dep.Description)
		}
		fmt.Println()
	}

	// Messages referencing this bead
	referrers, _ := storeListBeads(beads.IssueFilter{IDPrefix: "spi-", Labels: []string{"msg", "ref:" + id}, Status: statusPtr(beads.StatusOpen)})
	for _, m := range referrers {
		from := hasLabel(m, "from:")
		fmt.Printf("--- Referenced by %s ---\n", m.ID)
		if from != "" {
			fmt.Printf("From: %s\n", from)
		}
		fmt.Printf("Subject: %s\n", m.Title)
		fmt.Println()
	}

	// Thread context (parent + siblings)
	if target.Parent != "" {
		parentBead, parentErr := storeGetBead(target.Parent)
		if parentErr == nil {
			fmt.Printf("--- Thread (parent: %s) ---\n", parentBead.ID)
			fmt.Printf("Subject: %s\n", parentBead.Title)

			siblings, _ := storeGetChildren(target.Parent)
			for _, s := range siblings {
				if s.ID == target.ID {
					continue
				}
				from := hasLabel(s, "from:")
				fmt.Printf("  %s [%s]: %s\n", s.ID, from, s.Title)
			}
			fmt.Println()
		}
	}

	// Comments
	comments, commErr := storeGetComments(id)
	if commErr == nil && len(comments) > 0 {
		fmt.Printf("--- Comments (%d) ---\n", len(comments))
		for _, c := range comments {
			if c.Author != "" {
				fmt.Printf("[%s]: %s\n", c.Author, c.Text)
			} else {
				fmt.Println(c.Text)
			}
		}
		fmt.Println()
	}

	// Agent runs
	runs, runErr := observability.RunsForBead(id)
	if runErr == nil && len(runs) > 0 {
		fmt.Printf("--- Agent Runs (%d) ---\n", len(runs))
		for _, r := range runs {
			model := observability.ToString(r["model"])
			role := observability.ToString(r["role"])
			result := observability.ToString(r["result"])
			dur := observability.ToInt(r["duration_seconds"])
			started := observability.ToString(r["started_at"])
			fmt.Printf("  %-20s %-10s %-10s %4ds  %s\n", model, role, result, dur, started)
		}
		fmt.Println()
	}
}

// --- Step graph rendering helpers ---

// renderStepLine prints a single step line for the focus step graph display.
func renderStepLine(name string, stepCfg formula.StepConfig, gs *executor.GraphState) {
	status := "pending"
	indicator := "."
	detail := ""

	if gs != nil {
		if ss, ok := gs.Steps[name]; ok {
			status = ss.Status
			if status == "" {
				status = "pending"
			}
			detail = formatStepDetail(ss)
		}
	}

	prefix := "  "
	switch status {
	case "completed":
		indicator = "\xe2\x9c\x93" // check mark
	case "active":
		indicator = "\xe2\x97\x8f" // filled circle
		prefix = "\xe2\x86\x92 "   // arrow prefix for active
	case "failed":
		indicator = "\xe2\x9c\x97" // X mark
	case "skipped":
		indicator = "-"
	default: // pending
		indicator = "."
	}

	condition := formatStepCondition(stepCfg)

	line := fmt.Sprintf("%s%-14s %s %-10s", prefix, name, indicator, status)
	if detail != "" {
		line += "  " + detail
	}
	if condition != "" {
		line += "  " + condition
	}
	fmt.Println(line)
}

// formatStepDetail returns duration and completed count for a step.
func formatStepDetail(ss executor.StepState) string {
	var parts []string
	if ss.StartedAt != "" && ss.CompletedAt != "" {
		dur := parseDuration(ss.StartedAt, ss.CompletedAt)
		if dur != "" {
			parts = append(parts, "("+dur+")")
		}
	}
	if ss.CompletedCount > 1 {
		parts = append(parts, fmt.Sprintf("x%d", ss.CompletedCount))
	}
	return strings.Join(parts, " ")
}

// formatStepCondition returns a summary of the step's condition/when clause.
func formatStepCondition(stepCfg formula.StepConfig) string {
	if stepCfg.Terminal {
		cond := "[terminal"
		if stepCfg.When != nil || stepCfg.Condition != "" {
			cond += ", when: " + conditionSummary(stepCfg)
		}
		cond += "]"
		return cond
	}
	if stepCfg.When != nil || stepCfg.Condition != "" {
		return "[when: " + conditionSummary(stepCfg) + "]"
	}
	return ""
}

// conditionSummary returns a short human-readable summary of a step's condition.
func conditionSummary(stepCfg formula.StepConfig) string {
	if stepCfg.When != nil {
		var parts []string
		for _, p := range stepCfg.When.All {
			parts = append(parts, fmt.Sprintf("%s %s %s", shortKey(p.Left), p.Op, p.Right))
		}
		for _, p := range stepCfg.When.Any {
			parts = append(parts, fmt.Sprintf("%s %s %s", shortKey(p.Left), p.Op, p.Right))
		}
		return strings.Join(parts, " && ")
	}
	if stepCfg.Condition != "" {
		return stepCfg.Condition
	}
	return ""
}

// shortKey trims a dotted key to the last two segments for readability.
// e.g. "steps.review.outputs.outcome" -> "outcome"
func shortKey(key string) string {
	parts := strings.Split(key, ".")
	if len(parts) > 1 {
		return parts[len(parts)-1]
	}
	return key
}

// topoSortSteps returns step names in topological order (dependencies before dependents).
func topoSortSteps(graph *formula.FormulaStepGraph) []string {
	// Build in-degree map and adjacency list.
	inDegree := make(map[string]int, len(graph.Steps))
	dependents := make(map[string][]string, len(graph.Steps))
	for name := range graph.Steps {
		inDegree[name] = 0
	}
	for name, step := range graph.Steps {
		for _, need := range step.Needs {
			dependents[need] = append(dependents[need], name)
			inDegree[name]++
		}
	}

	// Kahn's algorithm with lexicographic tie-breaking for determinism.
	var queue []string
	for name, deg := range inDegree {
		if deg == 0 {
			queue = append(queue, name)
		}
	}
	sort.Strings(queue)

	var result []string
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		result = append(result, node)

		var next []string
		for _, dep := range dependents[node] {
			inDegree[dep]--
			if inDegree[dep] == 0 {
				next = append(next, dep)
			}
		}
		sort.Strings(next)
		queue = append(queue, next...)
	}

	return result
}

// parseDuration parses RFC3339 timestamps and returns a human-readable duration.
func parseDuration(startStr, endStr string) string {
	start, err := time.Parse(time.RFC3339, startStr)
	if err != nil {
		return ""
	}
	end, err := time.Parse(time.RFC3339, endStr)
	if err != nil {
		return ""
	}
	d := end.Sub(start)
	if d < time.Second {
		return "<1s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

// findFailedStep finds the first failed step in the graph state.
func findFailedStep(gs *executor.GraphState) (string, executor.StepState) {
	for name, ss := range gs.Steps {
		if ss.Status == "failed" {
			return name, ss
		}
	}
	return "", executor.StepState{}
}

// resolveWizardName determines the wizard name for a bead.
// Checks the wizard registry first, falls back to the convention.
func resolveWizardName(beadID string) string {
	reg := loadWizardRegistry()
	wiz := findLiveWizardForBead(reg, beadID)
	if wiz != nil {
		return wiz.Name
	}
	return "wizard-" + beadID
}

// getInterruptLabel returns the value after "interrupted:" from the bead's labels.
func getInterruptLabel(labels []string) string {
	for _, l := range labels {
		if strings.HasPrefix(l, "interrupted:") {
			return l[len("interrupted:"):]
		}
	}
	return ""
}

// isInterruptedBead returns true if any label starts with "interrupted:".
func isInterruptedBead(labels []string) bool {
	for _, l := range labels {
		if strings.HasPrefix(l, "interrupted:") {
			return true
		}
	}
	return false
}

// formatRecoverySection returns the formatted recovery work section for display,
// or "" if no open recovery-for dependent exists.
func formatRecoverySection(dependents []*beads.IssueWithDependencyMetadata) string {
	for _, dep := range dependents {
		if string(dep.DependencyType) != "recovery-for" {
			continue
		}
		if string(dep.Status) == "closed" {
			continue
		}
		return fmt.Sprintf("--- Recovery Work ---\n  %s  %s  (%s)\n\n", dep.ID, dep.Title, dep.Status)
	}
	return ""
}
