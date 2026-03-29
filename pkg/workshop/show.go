package workshop

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/awell-health/spire/cmd/spire/embedded"
	"github.com/awell-health/spire/pkg/formula"
)

// Show loads a formula by name and returns a human-readable rendering
// with header info and phase/step diagram.
func Show(name string) (string, error) {
	data, source, err := loadRawFormula(name)
	if err != nil {
		return "", err
	}

	var hdr formulaHeader
	if err := toml.Unmarshal(data, &hdr); err != nil {
		return "", fmt.Errorf("parse formula header: %w", err)
	}

	switch hdr.Version {
	case 2:
		f, err := formula.ParseFormulaV2(data)
		if err != nil {
			return "", fmt.Errorf("parse v2 formula: %w", err)
		}
		return renderV2(f, source), nil
	case 3:
		f, err := formula.ParseFormulaStepGraph(data)
		if err != nil {
			return "", fmt.Errorf("parse v3 formula: %w", err)
		}
		return renderV3(f, source), nil
	default:
		return "", fmt.Errorf("unsupported formula version %d", hdr.Version)
	}
}

// renderV2 produces a human-readable display of a v2 phase-pipeline formula.
func renderV2(f *formula.FormulaV2, source string) string {
	var b strings.Builder

	// Header
	fmt.Fprintf(&b, "%s (v%d)", f.Name, f.Version)
	if f.Description != "" {
		fmt.Fprintf(&b, " — %s", f.Description)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "Source: %s\n", source)

	// Pipeline diagram
	phases := f.EnabledPhases()
	if len(phases) > 0 {
		b.WriteString("\nPipeline:\n")
		fmt.Fprintf(&b, "  %s\n", strings.Join(phases, " → "))
	}

	// Per-phase details
	for _, phaseName := range phases {
		pc := f.Phases[phaseName]
		b.WriteString("\n")
		fmt.Fprintf(&b, "  [%s]\n", phaseName)

		if role := pc.GetRole(); role != "apprentice" || pc.Role != "" {
			fmt.Fprintf(&b, "    role:     %s\n", pc.GetRole())
		}
		if pc.Model != "" {
			fmt.Fprintf(&b, "    model:    %s\n", pc.Model)
		}
		if pc.Timeout != "" {
			fmt.Fprintf(&b, "    timeout:  %s\n", pc.Timeout)
		}
		if pc.MaxTurns > 0 {
			fmt.Fprintf(&b, "    turns:    %d\n", pc.MaxTurns)
		}
		if dispatch := pc.GetDispatch(); dispatch != "direct" {
			fmt.Fprintf(&b, "    dispatch: %s\n", dispatch)
		}
		if pc.StagingBranch != "" {
			fmt.Fprintf(&b, "    staging:  %s\n", pc.StagingBranch)
		}
		if pc.MergeStrategy != "" {
			fmt.Fprintf(&b, "    strategy: %s\n", pc.MergeStrategy)
		}
		if pc.Worktree {
			fmt.Fprintf(&b, "    worktree: true\n")
		}
		if pc.Apprentice {
			fmt.Fprintf(&b, "    apprentice: true\n")
		}
		if pc.Auto {
			fmt.Fprintf(&b, "    auto:     true\n")
		}
		if pc.VerdictOnly {
			fmt.Fprintf(&b, "    verdict_only: true\n")
		}
		if pc.Judgment {
			fmt.Fprintf(&b, "    judgment:  true\n")
		}
		if pc.Behavior != "" {
			fmt.Fprintf(&b, "    behavior: %s\n", pc.Behavior)
		}
		if pc.Build != "" {
			fmt.Fprintf(&b, "    build:    %s\n", pc.Build)
		}
		if pc.Test != "" {
			fmt.Fprintf(&b, "    test:     %s\n", pc.Test)
		}
		if pc.RevisionPolicy != nil {
			fmt.Fprintf(&b, "    revision_policy:\n")
			fmt.Fprintf(&b, "      max_rounds:    %d\n", pc.RevisionPolicy.MaxRounds)
			if pc.RevisionPolicy.ArbiterModel != "" {
				fmt.Fprintf(&b, "      arbiter_model: %s\n", pc.RevisionPolicy.ArbiterModel)
			}
		}
		if len(pc.Context) > 0 {
			fmt.Fprintf(&b, "    context:  %s\n", strings.Join(pc.Context, ", "))
		}
	}

	// Variables
	if len(f.Vars) > 0 {
		b.WriteString("\nVariables:\n")
		names := make([]string, 0, len(f.Vars))
		for n := range f.Vars {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			v := f.Vars[n]
			req := ""
			if v.Required {
				req = " (required)"
			}
			fmt.Fprintf(&b, "  %s%s — %s\n", n, req, v.Description)
			if v.Default != "" {
				fmt.Fprintf(&b, "    default: %s\n", v.Default)
			}
		}
	}

	return b.String()
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

	// Determine order: entry first, then by depth (BFS-ish)
	entry := formula.EntryStep(f)
	ordered := topologicalOrder(f, entry)

	b.WriteString("\nSteps:\n")
	for _, name := range ordered {
		step := f.Steps[name]
		markers := ""
		if name == entry {
			markers += " [entry]"
		}
		if step.Terminal {
			markers += " [terminal]"
		}
		fmt.Fprintf(&b, "\n  %s%s\n", name, markers)
		if step.Role != "" {
			fmt.Fprintf(&b, "    role:      %s\n", step.Role)
		}
		if step.Title != "" {
			fmt.Fprintf(&b, "    title:     %s\n", step.Title)
		}
		if step.Model != "" {
			fmt.Fprintf(&b, "    model:     %s\n", step.Model)
		}
		if step.Timeout != "" {
			fmt.Fprintf(&b, "    timeout:   %s\n", step.Timeout)
		}
		if step.VerdictOnly {
			fmt.Fprintf(&b, "    verdict_only: true\n")
		}
		if len(step.Needs) > 0 {
			fmt.Fprintf(&b, "    needs:     %s\n", strings.Join(step.Needs, ", "))
		}
		if step.Condition != "" {
			fmt.Fprintf(&b, "    condition: %s\n", step.Condition)
		}
	}

	// Variables
	if len(f.Vars) > 0 {
		b.WriteString("\nVariables:\n")
		names := make([]string, 0, len(f.Vars))
		for n := range f.Vars {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			v := f.Vars[n]
			req := ""
			if v.Required {
				req = " (required)"
			}
			fmt.Fprintf(&b, "  %s%s — %s\n", n, req, v.Description)
			if v.Default != "" {
				fmt.Fprintf(&b, "    default: %s\n", v.Default)
			}
		}
	}

	return b.String()
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
