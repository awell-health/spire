// Package workshop provides formula authoring tools: dry-run simulation,
// publishing, and (in other files) browsing, validation, and composition.
package workshop

import (
	"fmt"
	"sort"

	"github.com/awell-health/spire/pkg/formula"
)

// StepGraphSimulation describes a step-graph dry-run (v3 formulas).
type StepGraphSimulation struct {
	Formula    string                `json:"formula"`
	Version    int                   `json:"version"`
	Entry      string                `json:"entry"`
	Steps      []StepSimulation      `json:"steps"`
	Workspaces []WorkspaceSimulation `json:"workspaces,omitempty"`
	VarTypes   map[string]string     `json:"var_types,omitempty"`
	Paths      [][]string            `json:"paths"`
	Errors     []string              `json:"errors,omitempty"`
}

// WorkspaceSimulation describes a declared workspace in a v3 formula dry-run.
type WorkspaceSimulation struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Branch    string `json:"branch,omitempty"`
	Base      string `json:"base,omitempty"`
	Scope     string `json:"scope,omitempty"`
	Ownership string `json:"ownership,omitempty"`
	Cleanup   string `json:"cleanup,omitempty"`
}

// StepSimulation describes what a single step in a step graph would do.
type StepSimulation struct {
	Name        string            `json:"name"`
	Role        string            `json:"role,omitempty"`
	Title       string            `json:"title,omitempty"`
	Needs       []string          `json:"needs,omitempty"`
	Condition   string            `json:"condition,omitempty"`
	Terminal    bool              `json:"terminal"`
	Timeout     string            `json:"timeout,omitempty"`
	Model       string            `json:"model,omitempty"`
	Kind        string            `json:"kind,omitempty"`
	Action      string            `json:"action,omitempty"`
	Flow        string            `json:"flow,omitempty"`
	Workspace   string            `json:"workspace,omitempty"`
	Produces    []string          `json:"produces,omitempty"`
	With        map[string]string `json:"with,omitempty"`
	When        string            `json:"when,omitempty"`
	Graph       string            `json:"graph,omitempty"`
	Retry       string            `json:"retry,omitempty"`
	Description string            `json:"description"`
}

// DryRunStepGraph simulates a FormulaStepGraph by walking all reachable
// paths from entry to terminal steps. Returns every possible execution
// path with the conditions that would activate each.
func DryRunStepGraph(g *formula.FormulaStepGraph) (*StepGraphSimulation, error) {
	if g == nil {
		return nil, fmt.Errorf("step graph is nil")
	}

	result := &StepGraphSimulation{
		Formula: g.Name,
		Version: g.Version,
	}

	// Validate the graph
	if err := formula.ValidateGraph(g); err != nil {
		result.Errors = append(result.Errors, err.Error())
		// Still populate steps even if validation fails
	}

	// Find entry step
	result.Entry = formula.EntryStep(g)

	// Build step simulations (sorted by name for determinism)
	stepNames := make([]string, 0, len(g.Steps))
	for name := range g.Steps {
		stepNames = append(stepNames, name)
	}
	sort.Strings(stepNames)

	for _, name := range stepNames {
		sc := g.Steps[name]
		sim := StepSimulation{
			Name:      name,
			Role:      sc.Role,
			Title:     sc.Title,
			Needs:     sc.Needs,
			Condition: sc.Condition,
			Terminal:  sc.Terminal,
			Timeout:   sc.Timeout,
			Model:     sc.Model,
			Kind:      sc.Kind,
			Action:    sc.Action,
			Flow:      sc.Flow,
			Workspace: sc.Workspace,
			Produces:  sc.Produces,
			With:      sc.With,
			Graph:     sc.Graph,
		}
		if sc.When != nil {
			sim.When = renderWhenPredicate(sc.When)
		}
		if sc.Retry != nil {
			sim.Retry = fmt.Sprintf("max=%d", sc.Retry.Max)
			if sc.Retry.Action != "" {
				sim.Retry += fmt.Sprintf(" action=%s", sc.Retry.Action)
			}
			if sc.Retry.Flow != "" {
				sim.Retry += fmt.Sprintf(" flow=%s", sc.Retry.Flow)
			}
		}
		sim.Description = describeStep(sim)
		result.Steps = append(result.Steps, sim)
	}

	// Populate workspace simulations
	if len(g.Workspaces) > 0 {
		wsNames := make([]string, 0, len(g.Workspaces))
		for n := range g.Workspaces {
			wsNames = append(wsNames, n)
		}
		sort.Strings(wsNames)
		for _, name := range wsNames {
			ws := g.Workspaces[name]
			result.Workspaces = append(result.Workspaces, WorkspaceSimulation{
				Name:      name,
				Kind:      ws.Kind,
				Branch:    ws.Branch,
				Base:      ws.Base,
				Scope:     ws.Scope,
				Ownership: ws.Ownership,
				Cleanup:   ws.Cleanup,
			})
		}
	}

	// Populate var types
	if len(g.Vars) > 0 {
		result.VarTypes = make(map[string]string, len(g.Vars))
		for name, v := range g.Vars {
			t := v.Type
			if t == "" {
				t = "string"
			}
			result.VarTypes[name] = t
		}
	}

	// Enumerate all paths from entry to terminal steps using DFS
	if result.Entry != "" {
		successors := buildSuccessorMap(g)
		var paths [][]string
		var dfs func(current string, path []string, visited map[string]bool)
		dfs = func(current string, path []string, visited map[string]bool) {
			path = append(path, current)
			if formula.IsTerminal(g, current) {
				cp := make([]string, len(path))
				copy(cp, path)
				paths = append(paths, cp)
				return
			}
			succs := successors[current]
			if len(succs) == 0 {
				// Dead end (non-terminal with no successors) — still record path
				cp := make([]string, len(path))
				copy(cp, path)
				paths = append(paths, cp)
				return
			}
			for _, next := range succs {
				if visited[next] {
					// Avoid infinite loops in cyclic graphs (e.g., fix -> sage-review)
					// Record path up to cycle point
					cp := append(append([]string(nil), path...), next)
					paths = append(paths, cp)
					continue
				}
				visited[next] = true
				dfs(next, path, visited)
				delete(visited, next)
			}
		}

		visited := map[string]bool{result.Entry: true}
		dfs(result.Entry, nil, visited)
		result.Paths = paths
	}

	return result, nil
}

// describeStep generates a human-readable description of what a step would do.
func describeStep(sim StepSimulation) string {
	switch sim.Action {
	case "wizard.run":
		desc := "Run wizard"
		if sim.Flow != "" {
			desc += " " + sim.Flow + " flow"
		}
		if sim.Workspace != "" {
			desc += " in " + sim.Workspace
		}
		return desc
	case "dispatch.children":
		strategy := sim.With["strategy"]
		if strategy == "" {
			strategy = "default"
		}
		return fmt.Sprintf("Dispatch children via %s strategy", strategy)
	case "graph.run":
		if sim.Graph != "" {
			return fmt.Sprintf("Execute nested graph %s", sim.Graph)
		}
		return "Execute nested graph"
	case "git.merge_to_main":
		if sim.Workspace != "" {
			return fmt.Sprintf("Merge %s to main", sim.Workspace)
		}
		return "Merge staging to main"
	case "bead.finish":
		status := sim.With["status"]
		if status != "" {
			return fmt.Sprintf("Finalize bead (status=%s)", status)
		}
		return "Finalize bead"
	case "verify.run":
		if sim.Workspace != "" {
			return fmt.Sprintf("Run verification in %s", sim.Workspace)
		}
		return "Run verification"
	case "check.design-linked":
		return "Verify linked design bead"
	case "beads.materialize_plan":
		return "Materialize plan into child beads"
	}

	// Fallback: action-based if present
	if sim.Action != "" {
		return fmt.Sprintf("Execute %s (%s)", sim.Action, sim.Kind)
	}

	// Legacy role-based fallback
	switch sim.Role {
	case "sage":
		if sim.Title != "" {
			return fmt.Sprintf("Sage reviews: %s", sim.Title)
		}
		return "Sage reviews diff and returns verdict"
	case "apprentice":
		if sim.Title != "" {
			return fmt.Sprintf("Apprentice implements: %s", sim.Title)
		}
		return "Apprentice implements in worktree"
	case "arbiter":
		return "Arbiter makes final decision"
	case "executor":
		return "Executor runs step"
	}

	return fmt.Sprintf("Step %q", sim.Name)
}

// buildSuccessorMap builds a map from step name to its successors.
// A step S2 is a successor of S1 if S1 appears in S2's Needs list.
// Reset edges are also included: if step X declares resets = ["Y", "Z"],
// then Y and Z are successors of X (back-edges enabling review loops).
func buildSuccessorMap(g *formula.FormulaStepGraph) map[string][]string {
	succs := make(map[string][]string)

	// Collect all step names sorted for determinism
	stepNames := make([]string, 0, len(g.Steps))
	for name := range g.Steps {
		stepNames = append(stepNames, name)
	}
	sort.Strings(stepNames)

	for _, name := range stepNames {
		step := g.Steps[name]
		for _, need := range step.Needs {
			succs[need] = append(succs[need], name)
		}
		// Reset edges: step X resets Y means X -> Y (back-edge).
		for _, target := range step.Resets {
			succs[name] = append(succs[name], target)
		}
	}
	return succs
}
