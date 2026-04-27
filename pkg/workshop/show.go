package workshop

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/formula/embedded"
)

// Show loads a formula by name and returns a human-readable rendering
// with header info and step-graph DAG diagram.
func Show(name string) (string, error) {
	data, source, err := loadRawFormula(name)
	if err != nil {
		return "", err
	}

	f, err := formula.ParseFormulaStepGraph(data)
	if err != nil {
		return "", fmt.Errorf("parse v3 formula: %w", err)
	}
	return renderV3(f, source), nil
}

// renderV3 produces a human-readable display of a v3 step-graph formula.
func renderV3(f *formula.FormulaStepGraph, source string) string {
	var b strings.Builder

	// Header
	fmt.Fprintf(&b, "%s (v%d)", f.Name, f.Version)
	if f.Description != "" {
		fmt.Fprintf(&b, " — %s", f.Description)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "Source: %s\n", source)

	// Workspace declarations
	renderV3Workspaces(&b, f.Workspaces)

	// ASCII DAG rendering
	renderDAG(&b, f)

	// Detailed step listing
	entry := formula.EntryStep(f)
	ordered := topologicalOrder(f, entry)

	b.WriteString("\nSteps:\n")
	for _, name := range ordered {
		step := f.Steps[name]
		renderV3Step(&b, name, step, entry)
	}

	// Variables
	renderV3Vars(&b, f.Vars)

	return b.String()
}

// renderDAG produces an ASCII tree-style rendering of the step graph.
// It walks the graph from the entry step using buildSuccessorMap, rendering
// box-drawing characters for the tree structure:
//
//	plan [entry]
//	 └─ implement
//	     └─ review (-> subgraph-review)
//	         ├─ merge [when: outcome == merge]
//	         │   └─ close [terminal]
//	         └─ discard [terminal, when: outcome == discard]
func renderDAG(b *strings.Builder, f *formula.FormulaStepGraph) {
	entry := formula.EntryStep(f)
	if entry == "" {
		return
	}

	successors := buildSuccessorMap(f)

	b.WriteString("\nGraph:\n")

	// Track visited to detect back-edges (resets/cycles)
	visited := make(map[string]bool)
	dagRenderNode(b, f, entry, "  ", true, true, visited, successors)
}

// dagAnnotation builds the annotation string for a DAG node.
func dagAnnotation(f *formula.FormulaStepGraph, name string, step formula.StepConfig) string {
	var annotations []string

	if name == formula.EntryStep(f) {
		annotations = append(annotations, "entry")
	}
	if step.Terminal {
		annotations = append(annotations, "terminal")
	}
	if step.When != nil {
		annotations = append(annotations, "when: "+renderWhenPredicate(step.When))
	} else if step.Condition != "" {
		annotations = append(annotations, "when: "+step.Condition)
	}

	if len(annotations) == 0 {
		return ""
	}
	return " [" + strings.Join(annotations, ", ") + "]"
}

// dagRenderNode recursively renders a step and its successors as a tree.
func dagRenderNode(b *strings.Builder, f *formula.FormulaStepGraph, name string, prefix string, isLast bool, isRoot bool, visited map[string]bool, successors map[string][]string) {
	step := f.Steps[name]

	// Build the connector
	var connector string
	if isRoot {
		connector = ""
	} else if isLast {
		connector = "└─ "
	} else {
		connector = "├─ "
	}

	// Nested graph reference
	graphRef := ""
	if step.Graph != "" {
		graphRef = fmt.Sprintf(" (-> %s)", step.Graph)
	}

	annotationStr := dagAnnotation(f, name, step)

	fmt.Fprintf(b, "%s%s%s%s%s\n", prefix, connector, name, graphRef, annotationStr)

	// Cycle detection: if we already visited this node, don't recurse
	if visited[name] {
		return
	}
	visited[name] = true

	// Determine child prefix for continuation lines
	var childPrefix string
	if isRoot {
		childPrefix = prefix
	} else if isLast {
		childPrefix = prefix + "    "
	} else {
		childPrefix = prefix + "│   "
	}

	succs := successors[name]

	// Separate forward edges from back-edges (cycles/resets)
	var forwardSuccs []string
	var resetTargets []string
	for _, s := range succs {
		if visited[s] {
			resetTargets = append(resetTargets, s)
		} else {
			forwardSuccs = append(forwardSuccs, s)
		}
	}

	// Total children = forward successors + optional reset line
	totalChildren := len(forwardSuccs)
	if len(resetTargets) > 0 {
		totalChildren++
	}

	// Render forward successors first
	childIdx := 0
	for _, s := range forwardSuccs {
		childIdx++
		isLastChild := childIdx == totalChildren
		dagRenderNode(b, f, s, childPrefix, isLastChild, false, visited, successors)
	}

	// Render reset back-edges as a note at the end
	if len(resetTargets) > 0 {
		fmt.Fprintf(b, "%s└─ (resets: %s)\n", childPrefix, strings.Join(resetTargets, ", "))
	}

	delete(visited, name)
}

// renderV3Workspaces renders the workspace declarations section.
func renderV3Workspaces(b *strings.Builder, workspaces map[string]formula.WorkspaceDecl) {
	if len(workspaces) == 0 {
		return
	}
	b.WriteString("\nWorkspaces:\n")
	names := make([]string, 0, len(workspaces))
	for n := range workspaces {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		ws := workspaces[name]
		fmt.Fprintf(b, "\n  %s\n", name)
		fmt.Fprintf(b, "    kind:      %s\n", ws.Kind)
		if ws.Branch != "" {
			fmt.Fprintf(b, "    branch:    %s\n", ws.Branch)
		}
		if ws.Base != "" {
			fmt.Fprintf(b, "    base:      %s\n", ws.Base)
		}
		if ws.Scope != "" {
			fmt.Fprintf(b, "    scope:     %s\n", ws.Scope)
		}
		if ws.Ownership != "" {
			fmt.Fprintf(b, "    ownership: %s\n", ws.Ownership)
		}
		if ws.Cleanup != "" {
			fmt.Fprintf(b, "    cleanup:   %s\n", ws.Cleanup)
		}
	}
}

// renderV3Vars renders variables with type field.
func renderV3Vars(b *strings.Builder, vars map[string]formula.FormulaVar) {
	if len(vars) == 0 {
		return
	}
	b.WriteString("\nVariables:\n")
	names := make([]string, 0, len(vars))
	for n := range vars {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		v := vars[n]
		req := ""
		if v.Required {
			req = " (required)"
		}
		typStr := ""
		if v.Type != "" {
			typStr = fmt.Sprintf(" [%s]", v.Type)
		}
		fmt.Fprintf(b, "  %s%s%s — %s\n", n, typStr, req, v.Description)
		if v.Default != "" {
			fmt.Fprintf(b, "    default: %s\n", v.Default)
		}
	}
}

// renderV3Step renders one step with all v3 fields.
func renderV3Step(b *strings.Builder, name string, step formula.StepConfig, entry string) {
	markers := ""
	if name == entry {
		markers += " [entry]"
	}
	if step.Terminal {
		markers += " [terminal]"
	}
	fmt.Fprintf(b, "\n  %s%s\n", name, markers)

	if step.Kind != "" {
		fmt.Fprintf(b, "    kind:      %s\n", step.Kind)
	}
	if step.Action != "" {
		fmt.Fprintf(b, "    action:    %s\n", step.Action)
	}
	if step.Flow != "" {
		fmt.Fprintf(b, "    flow:      %s\n", step.Flow)
	}
	if step.Role != "" {
		fmt.Fprintf(b, "    role:      %s\n", step.Role)
	}
	if step.Title != "" {
		fmt.Fprintf(b, "    title:     %s\n", step.Title)
	}
	if step.Model != "" {
		fmt.Fprintf(b, "    model:     %s\n", step.Model)
	}
	if step.Timeout != "" {
		fmt.Fprintf(b, "    timeout:   %s\n", step.Timeout)
	}
	if step.VerdictOnly {
		fmt.Fprintf(b, "    verdict_only: true\n")
	}
	if step.Workspace != "" {
		fmt.Fprintf(b, "    workspace: %s\n", step.Workspace)
	}
	if len(step.Needs) > 0 {
		fmt.Fprintf(b, "    needs:     %s\n", strings.Join(step.Needs, ", "))
	}
	if step.Condition != "" {
		fmt.Fprintf(b, "    condition: %s\n", step.Condition)
	}
	if step.When != nil {
		fmt.Fprintf(b, "    when:      %s\n", renderWhenPredicate(step.When))
	}
	if len(step.Produces) > 0 {
		fmt.Fprintf(b, "    produces:  %s\n", strings.Join(step.Produces, ", "))
	}
	if len(step.With) > 0 {
		keys := make([]string, 0, len(step.With))
		for k := range step.With {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(b, "    with.%-5s %s\n", k+":", step.With[k])
		}
	}
	if step.Graph != "" {
		fmt.Fprintf(b, "    graph:     %s\n", step.Graph)
	}
	if step.Retry != nil {
		fmt.Fprintf(b, "    retry:     max=%d", step.Retry.Max)
		if step.Retry.Action != "" {
			fmt.Fprintf(b, " action=%s", step.Retry.Action)
		}
		if step.Retry.Flow != "" {
			fmt.Fprintf(b, " flow=%s", step.Retry.Flow)
		}
		b.WriteString("\n")
	}
}

// renderWhenPredicate converts a structured condition to a human-readable string.
func renderWhenPredicate(when *formula.StructuredCondition) string {
	if when == nil {
		return ""
	}
	var parts []string
	for _, p := range when.All {
		parts = append(parts, fmt.Sprintf("%s %s %s", p.Left, opSymbol(p.Op), p.Right))
	}
	allStr := strings.Join(parts, " AND ")

	if len(when.Any) > 0 {
		var anyParts []string
		for _, p := range when.Any {
			anyParts = append(anyParts, fmt.Sprintf("%s %s %s", p.Left, opSymbol(p.Op), p.Right))
		}
		anyStr := strings.Join(anyParts, " OR ")
		if allStr != "" {
			return allStr + " AND (" + anyStr + ")"
		}
		return anyStr
	}
	return allStr
}

// opSymbol converts a predicate operator to its symbol form.
func opSymbol(op string) string {
	switch op {
	case "eq":
		return "=="
	case "ne":
		return "!="
	case "lt":
		return "<"
	case "gt":
		return ">"
	case "le":
		return "<="
	case "ge":
		return ">="
	default:
		return op
	}
}

// topologicalOrder returns step names in a BFS traversal from the entry point,
// falling back to alphabetical for any unreachable steps.
func topologicalOrder(f *formula.FormulaStepGraph, entry string) []string {
	visited := make(map[string]bool)
	var ordered []string

	// BFS from entry
	queue := []string{entry}
	visited[entry] = true
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		ordered = append(ordered, cur)

		// Find steps that need cur
		var dependents []string
		for name, step := range f.Steps {
			if visited[name] {
				continue
			}
			for _, need := range step.Needs {
				if need == cur {
					dependents = append(dependents, name)
					break
				}
			}
		}
		sort.Strings(dependents)
		for _, d := range dependents {
			if !visited[d] {
				visited[d] = true
				queue = append(queue, d)
			}
		}
	}

	// Pick up any orphan steps not reachable from entry
	var remaining []string
	for name := range f.Steps {
		if !visited[name] {
			remaining = append(remaining, name)
		}
	}
	sort.Strings(remaining)
	ordered = append(ordered, remaining...)

	return ordered
}

// loadRawFormula locates and reads the raw bytes of a formula by name.
// Returns the bytes, source label ("embedded" or "custom"), and any error.
// Resolution: disk paths first, then embedded fallback.
func loadRawFormula(name string) ([]byte, string, error) {
	// Try disk first
	for _, dir := range diskFormulaDirs() {
		path := filepath.Join(dir, name+".formula.toml")
		if data, err := os.ReadFile(path); err == nil {
			return data, "custom", nil
		}
	}

	// Try embedded
	filename := "formulas/" + name + ".formula.toml"
	if data, err := fs.ReadFile(embedded.Formulas, filename); err == nil {
		return data, "embedded", nil
	}

	return nil, "", fmt.Errorf("formula %q not found", name)
}

// LoadRawFormula is the exported variant of loadRawFormula. Returns the raw
// TOML bytes, source label ("embedded" or "custom"), and any error from the
// resolution. Used by the gateway's /api/v1/workshop/formulas/{name}/source
// endpoint to render the unmodified file contents to the desktop.
func LoadRawFormula(name string) ([]byte, string, error) {
	return loadRawFormula(name)
}

// RenderWhenPredicate exports renderWhenPredicate so callers outside the
// package (notably the gateway's edge materializer) can render a structured
// when condition into the same human-readable form the show command uses.
func RenderWhenPredicate(when *formula.StructuredCondition) string {
	return renderWhenPredicate(when)
}
