package workshop

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/awell-health/spire/pkg/formula"
)

// GraphBuilder accumulates v3 step-graph formula configuration.
// Parallel to FormulaBuilder (v2) but constructs FormulaStepGraph.
type GraphBuilder struct {
	name        string
	description string
	stepOrder   []string
	stepConfigs map[string]formula.StepConfig
	workspaces  map[string]formula.WorkspaceDecl
	vars        map[string]formula.FormulaVar
}

// NewGraphBuilder creates a new GraphBuilder with the given formula name.
func NewGraphBuilder(name string) *GraphBuilder {
	return &GraphBuilder{
		name:        name,
		stepConfigs: make(map[string]formula.StepConfig),
		workspaces:  make(map[string]formula.WorkspaceDecl),
		vars:        make(map[string]formula.FormulaVar),
	}
}

// SetDescription sets the formula description.
func (gb *GraphBuilder) SetDescription(desc string) {
	gb.description = desc
}

// AddStep validates the step kind and appends the step to the ordered list.
// Returns error if the step name is duplicate or the kind is invalid.
func (gb *GraphBuilder) AddStep(name string, cfg formula.StepConfig) error {
	if _, exists := gb.stepConfigs[name]; exists {
		return fmt.Errorf("step %q already exists", name)
	}
	if cfg.Kind != "" && !formula.ValidStepKind(cfg.Kind) {
		return fmt.Errorf("step %q: invalid kind %q", name, cfg.Kind)
	}
	gb.stepOrder = append(gb.stepOrder, name)
	gb.stepConfigs[name] = cfg
	return nil
}

// AddWorkspace validates the workspace kind and adds the declaration.
func (gb *GraphBuilder) AddWorkspace(name string, ws formula.WorkspaceDecl) error {
	if ws.Kind == "" {
		return fmt.Errorf("workspace %q: kind is required", name)
	}
	formula.DefaultWorkspaceDecl(&ws)
	gb.workspaces[name] = ws
	return nil
}

// AddVar adds or overwrites a typed formula variable.
func (gb *GraphBuilder) AddVar(name string, v formula.FormulaVar) {
	gb.vars[name] = v
}

// Build constructs a FormulaStepGraph and validates it via formula.ValidateGraph.
func (gb *GraphBuilder) Build() (*formula.FormulaStepGraph, error) {
	if len(gb.stepOrder) == 0 {
		return nil, fmt.Errorf("no steps defined")
	}

	steps := make(map[string]formula.StepConfig, len(gb.stepOrder))
	for _, name := range gb.stepOrder {
		steps[name] = gb.stepConfigs[name]
	}

	var workspaces map[string]formula.WorkspaceDecl
	if len(gb.workspaces) > 0 {
		workspaces = make(map[string]formula.WorkspaceDecl, len(gb.workspaces))
		for k, v := range gb.workspaces {
			workspaces[k] = v
		}
	}

	var vars map[string]formula.FormulaVar
	if len(gb.vars) > 0 {
		vars = make(map[string]formula.FormulaVar, len(gb.vars))
		for k, v := range gb.vars {
			vars[k] = v
		}
	}

	g := &formula.FormulaStepGraph{
		Name:        gb.name,
		Description: gb.description,
		Version:     3,
		Steps:       steps,
		Workspaces:  workspaces,
		Vars:        vars,
	}

	if err := formula.ValidateGraph(g); err != nil {
		return nil, fmt.Errorf("graph validation: %w", err)
	}

	return g, nil
}

// MarshalTOML serializes the graph to ordered TOML bytes.
func (gb *GraphBuilder) MarshalTOML() ([]byte, error) {
	g, err := gb.Build()
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer

	// Header
	fmt.Fprintf(&buf, "name = %q\n", g.Name)
	if g.Description != "" {
		fmt.Fprintf(&buf, "description = %q\n", g.Description)
	}
	fmt.Fprintf(&buf, "version = %d\n", g.Version)

	// Vars in sorted order
	if len(g.Vars) > 0 {
		varNames := make([]string, 0, len(g.Vars))
		for n := range g.Vars {
			varNames = append(varNames, n)
		}
		sort.Strings(varNames)
		for _, name := range varNames {
			v := g.Vars[name]
			fmt.Fprintf(&buf, "\n[vars.%s]\n", name)
			writeGraphVar(&buf, v)
		}
	}

	// Workspaces in sorted order
	if len(g.Workspaces) > 0 {
		wsNames := make([]string, 0, len(g.Workspaces))
		for n := range g.Workspaces {
			wsNames = append(wsNames, n)
		}
		sort.Strings(wsNames)
		for _, name := range wsNames {
			ws := g.Workspaces[name]
			fmt.Fprintf(&buf, "\n[workspaces.%s]\n", name)
			writeWorkspaceDecl(&buf, ws)
		}
	}

	// Steps in topological order
	entry := formula.EntryStep(g)
	ordered := topologicalOrder(g, entry)
	for _, name := range ordered {
		sc := g.Steps[name]
		fmt.Fprintf(&buf, "\n[steps.%s]\n", name)
		writeStepConfig(&buf, name, sc)
	}

	return buf.Bytes(), nil
}

// writeGraphVar writes a FormulaVar with type field.
func writeGraphVar(buf *bytes.Buffer, v formula.FormulaVar) {
	if v.Type != "" {
		fmt.Fprintf(buf, "type = %q\n", v.Type)
	}
	if v.Description != "" {
		fmt.Fprintf(buf, "description = %q\n", v.Description)
	}
	if v.Required {
		fmt.Fprintf(buf, "required = true\n")
	}
	if v.Default != "" {
		fmt.Fprintf(buf, "default = %q\n", v.Default)
	}
}

// writeWorkspaceDecl writes workspace declaration fields.
func writeWorkspaceDecl(buf *bytes.Buffer, ws formula.WorkspaceDecl) {
	fmt.Fprintf(buf, "kind = %q\n", ws.Kind)
	if ws.Branch != "" {
		fmt.Fprintf(buf, "branch = %q\n", ws.Branch)
	}
	if ws.Base != "" {
		fmt.Fprintf(buf, "base = %q\n", ws.Base)
	}
	if ws.Scope != "" {
		fmt.Fprintf(buf, "scope = %q\n", ws.Scope)
	}
	if ws.Ownership != "" {
		fmt.Fprintf(buf, "ownership = %q\n", ws.Ownership)
	}
	if ws.Cleanup != "" {
		fmt.Fprintf(buf, "cleanup = %q\n", ws.Cleanup)
	}
}

// writeStepConfig writes a v3 StepConfig to TOML.
func writeStepConfig(buf *bytes.Buffer, name string, sc formula.StepConfig) {
	if sc.Kind != "" {
		fmt.Fprintf(buf, "kind = %q\n", sc.Kind)
	}
	if sc.Action != "" {
		fmt.Fprintf(buf, "action = %q\n", sc.Action)
	}
	if sc.Role != "" {
		fmt.Fprintf(buf, "role = %q\n", sc.Role)
	}
	if sc.Title != "" {
		fmt.Fprintf(buf, "title = %q\n", sc.Title)
	}
	if sc.Flow != "" {
		fmt.Fprintf(buf, "flow = %q\n", sc.Flow)
	}
	if sc.Model != "" {
		fmt.Fprintf(buf, "model = %q\n", sc.Model)
	}
	if sc.Timeout != "" {
		fmt.Fprintf(buf, "timeout = %q\n", sc.Timeout)
	}
	if sc.Workspace != "" {
		fmt.Fprintf(buf, "workspace = %q\n", sc.Workspace)
	}
	if len(sc.Needs) > 0 {
		fmt.Fprintf(buf, "needs = [%s]\n", formatStringSlice(sc.Needs))
	}
	if sc.Terminal {
		fmt.Fprintf(buf, "terminal = true\n")
	}
	if sc.VerdictOnly {
		fmt.Fprintf(buf, "verdict_only = true\n")
	}
	if sc.Condition != "" {
		fmt.Fprintf(buf, "condition = %q\n", sc.Condition)
	}
	if len(sc.Produces) > 0 {
		fmt.Fprintf(buf, "produces = [%s]\n", formatStringSlice(sc.Produces))
	}
	if sc.Graph != "" {
		fmt.Fprintf(buf, "graph = %q\n", sc.Graph)
	}
	if sc.Retry != nil {
		fmt.Fprintf(buf, "\n[steps.%s.retry]\n", name)
		fmt.Fprintf(buf, "max = %d\n", sc.Retry.Max)
		if sc.Retry.Action != "" {
			fmt.Fprintf(buf, "action = %q\n", sc.Retry.Action)
		}
		if sc.Retry.Flow != "" {
			fmt.Fprintf(buf, "flow = %q\n", sc.Retry.Flow)
		}
	}
	if len(sc.With) > 0 {
		fmt.Fprintf(buf, "\n[steps.%s.with]\n", name)
		keys := make([]string, 0, len(sc.With))
		for k := range sc.With {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(buf, "%s = %q\n", k, sc.With[k])
		}
	}
	if sc.When != nil {
		writeWhenCondition(buf, name, sc.When)
	}
}

// writeWhenCondition writes a structured condition as TOML.
func writeWhenCondition(buf *bytes.Buffer, stepName string, when *formula.StructuredCondition) {
	if when == nil {
		return
	}
	for i, p := range when.All {
		fmt.Fprintf(buf, "\n[[steps.%s.when.all]]\n", stepName)
		fmt.Fprintf(buf, "left = %q\n", p.Left)
		fmt.Fprintf(buf, "op = %q\n", p.Op)
		fmt.Fprintf(buf, "right = %q\n", p.Right)
		_ = i
	}
	for i, p := range when.Any {
		fmt.Fprintf(buf, "\n[[steps.%s.when.any]]\n", stepName)
		fmt.Fprintf(buf, "left = %q\n", p.Left)
		fmt.Fprintf(buf, "op = %q\n", p.Op)
		fmt.Fprintf(buf, "right = %q\n", p.Right)
		_ = i
	}
}

// formatStringSliceQuoted wraps each element in quotes and joins with commas.
func formatStringSliceQuoted(ss []string) string {
	quoted := make([]string, len(ss))
	for i, s := range ss {
		quoted[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(quoted, ", ")
}
