// Package workshop provides formula authoring tools: dry-run simulation,
// publishing, and (in other files) browsing, validation, and composition.
package workshop

import (
	"fmt"
	"sort"
	"strings"

	"github.com/awell-health/spire/pkg/formula"
)

// BeadInfo is a minimal bead representation for dry-run context.
// Mirrors formula.BeadInfo but lives in workshop to avoid coupling.
type BeadInfo struct {
	ID     string
	Type   string
	Labels []string
	Title  string
}

// DryRunResult is the structured output of a formula dry-run.
type DryRunResult struct {
	Formula       string            `json:"formula"`
	Version       int               `json:"version"`
	EnabledPhases []string          `json:"enabled_phases"`
	Phases        []PhaseSimulation `json:"phases"`
	Errors        []string          `json:"errors,omitempty"`
}

// PhaseSimulation describes what a single phase would do.
type PhaseSimulation struct {
	Name     string `json:"name"`
	Role     string `json:"role"`
	Behavior string `json:"behavior,omitempty"`
	Model    string `json:"model,omitempty"`
	Timeout  string `json:"timeout,omitempty"`
	Dispatch string `json:"dispatch"`
	MaxTurns int    `json:"max_turns,omitempty"`
	Worktree bool   `json:"worktree,omitempty"`
	Strategy string `json:"strategy,omitempty"`
	Auto     bool   `json:"auto,omitempty"`
	// Review-specific
	RevisionPolicy *formula.RevisionPolicy `json:"revision_policy,omitempty"`
	VerdictOnly    bool                    `json:"verdict_only,omitempty"`
	// Wave-specific
	StagingBranch     string `json:"staging_branch,omitempty"`
	Build             string `json:"build,omitempty"`
	Test              string `json:"test,omitempty"`
	MaxBuildFixRounds int    `json:"max_build_fix_rounds,omitempty"`
	// Computed
	Description string `json:"description"`
}

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

// DryRun simulates a FormulaV2 phase pipeline. No side effects.
// If beadID is non-empty and loadBead is provided, resolves bead-specific
// context (type, labels, repo) to show what would happen for that bead.
func DryRun(f *formula.FormulaV2, beadID string, loadBead func(string) (BeadInfo, error)) (*DryRunResult, error) {
	if f == nil {
		return nil, fmt.Errorf("formula is nil")
	}

	result := &DryRunResult{
		Formula:       f.Name,
		Version:       f.Version,
		EnabledPhases: f.EnabledPhases(),
	}

	var bead *BeadInfo
	if beadID != "" && loadBead != nil {
		b, err := loadBead(beadID)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("load bead %s: %s", beadID, err))
		} else {
			bead = &b
		}
	}

	for _, phase := range result.EnabledPhases {
		pc := f.Phases[phase]
		sim := buildPhaseSimulation(phase, pc, beadID, bead)
		result.Phases = append(result.Phases, sim)
	}

	return result, nil
}

// buildPhaseSimulation constructs a PhaseSimulation from a PhaseConfig.
func buildPhaseSimulation(phase string, pc formula.PhaseConfig, beadID string, bead *BeadInfo) PhaseSimulation {
	sim := PhaseSimulation{
		Name:           phase,
		Role:           pc.GetRole(),
		Behavior:       pc.GetBehavior(),
		Model:          pc.Model,
		Timeout:        pc.Timeout,
		Dispatch:       pc.GetDispatch(),
		MaxTurns:       pc.GetMaxTurns(),
		Worktree:       pc.Worktree,
		Strategy:       pc.GetMergeStrategy(),
		Auto:           pc.Auto,
		RevisionPolicy: pc.RevisionPolicy,
		VerdictOnly:    pc.VerdictOnly,
		StagingBranch:  pc.StagingBranch,
		Build:          pc.Build,
		Test:           pc.Test,
	}

	if pc.MaxBuildFixRounds > 0 {
		sim.MaxBuildFixRounds = pc.GetMaxBuildFixRounds()
	}

	// Substitute {bead-id} in staging branch pattern
	if sim.StagingBranch != "" && beadID != "" {
		sim.StagingBranch = strings.ReplaceAll(sim.StagingBranch, "{bead-id}", beadID)
	}

	sim.Description = describePhase(sim)
	return sim
}

// describePhase generates a human-readable description of what a phase would do.
func describePhase(sim PhaseSimulation) string {
	// Behavior overrides take precedence
	switch sim.Behavior {
	case "validate-design":
		return "Wizard validates linked design bead is closed and substantive"
	case "epic-plan":
		return "Wizard invokes Claude to break epic into child tasks"
	case "task-plan":
		return "Wizard invokes Claude to produce a focused implementation plan"
	case "sage-review":
		return "Sage reviews staging branch diff, returns verdict (approve/request_changes)"
	case "merge-to-main":
		return "Rebase staging onto main, push, delete branches, close bead"
	case "deploy":
		return "Execute deploy command after merge"
	}

	// Role-based descriptions
	switch sim.Role {
	case "skip":
		return "Phase skipped"
	case "human":
		return "Blocks until human transitions phase"
	case "sage":
		if sim.VerdictOnly {
			return "Sage reviews diff and returns verdict only (no edits)"
		}
		return "Sage reviews diff and may suggest changes"
	case "apprentice":
		switch sim.Dispatch {
		case "wave":
			return "Parallel apprentice wave dispatch with staging branch merges"
		case "sequential":
			return "Sequential apprentice dispatch (one at a time)"
		default:
			return "Single apprentice implements in worktree"
		}
	case "wizard":
		return "Wizard invokes Claude for planning/validation"
	case "arbiter":
		return "Arbiter makes final merge/discard decision"
	}

	return fmt.Sprintf("Phase %q with role %q", sim.Name, sim.Role)
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
	}
	return succs
}
