// Package gateway: Workshop endpoints. These four routes expose the formula
// catalog that pkg/workshop already manages so the spire-desktop app can
// render its Workshop view. Handlers are thin pass-throughs to pkg/workshop +
// pkg/formula — no new business logic. The wire types match the TS contract
// in docs/design/workshop-desktop.md §2.1 (camelCase TS → snake_case JSON).
package gateway

import (
	"net/http"
	"sort"
	"strings"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/workshop"
)

// --------------------------------------------------------------------------
// Wire types — JSON shapes the desktop consumes. Naming convention: the
// `Wire` suffix distinguishes them from the pkg/workshop and pkg/formula
// types we project from. JSON tags match docs/design/workshop-desktop.md
// §2.1 verbatim. Optional fields use ,omitempty + pointer/zero-value
// semantics.
// --------------------------------------------------------------------------

// FormulaInfoWire is the catalog-row projection of a formula. Returned as
// an array from GET /api/v1/workshop/formulas. Mirrors the TS FormulaInfo
// interface in the design doc.
type FormulaInfoWire struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Source      string   `json:"source"`
	Category    string   `json:"category"`
	DefaultFor  []string `json:"default_for"`
	Version     int      `json:"version"`
	StepCount   int      `json:"step_count"`
	AuthoredBy  string   `json:"authored_by,omitempty"`
}

// FormulaDetailWire is the full per-formula payload returned from
// GET /api/v1/workshop/formulas/{name}. Embeds FormulaInfoWire and adds the
// graph-shape fields the desktop needs to render the canvas.
type FormulaDetailWire struct {
	FormulaInfoWire
	Entry      string           `json:"entry"`
	Vars       []VarWire        `json:"vars"`
	Workspaces []WorkspaceWire  `json:"workspaces"`
	Steps      []StepWire       `json:"steps"`
	Edges      []EdgeWire       `json:"edges"`
	Paths      [][]string       `json:"paths"`
	Outputs    []OutputWire     `json:"outputs"`
	Issues     []workshop.Issue `json:"issues"`
	Stats      *StatsWire       `json:"stats,omitempty"`
}

// StepWire is one step card in the rendered DAG.
type StepWire struct {
	Name        string            `json:"name"`
	Kind        string            `json:"kind"`
	Action      string            `json:"action"`
	Title       string            `json:"title"`
	Needs       []string          `json:"needs"`
	Terminal    bool              `json:"terminal,omitempty"`
	Workspace   string            `json:"workspace,omitempty"`
	Graph       string            `json:"graph,omitempty"`
	Flow        string            `json:"flow,omitempty"`
	Role        string            `json:"role,omitempty"`
	Model       string            `json:"model,omitempty"`
	Timeout     string            `json:"timeout,omitempty"`
	With        map[string]string `json:"with,omitempty"`
	When        string            `json:"when,omitempty"`
	Produces    []string          `json:"produces,omitempty"`
	Resets      []string          `json:"resets,omitempty"`
	VerdictOnly bool              `json:"verdict_only,omitempty"`
}

// EdgeWire is one edge in the explicit edge list. Kind is required and
// distinguishes the three edge categories (`needs` | `guard` | `reset`).
type EdgeWire struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
	When string `json:"when,omitempty"`
}

// VarWire is one declared formula variable.
type VarWire struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Required    bool     `json:"required,omitempty"`
	Default     string   `json:"default,omitempty"`
	Values      []string `json:"values,omitempty"`
	Description string   `json:"description,omitempty"`
}

// WorkspaceWire is one declared workspace.
type WorkspaceWire struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Branch    string `json:"branch"`
	Base      string `json:"base"`
	Scope     string `json:"scope,omitempty"`
	Ownership string `json:"ownership,omitempty"`
	Cleanup   string `json:"cleanup,omitempty"`
}

// OutputWire is one declared graph output (terminal-step output channel).
type OutputWire struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Values      []string `json:"values,omitempty"`
	Description string   `json:"description,omitempty"`
}

// StatsWire is the per-formula run statistics strip. Optional in v1 — we
// always omit it (no run aggregation backend yet). Defined so the contract
// is stable when stats land later.
type StatsWire struct {
	Runs         int     `json:"runs"`
	Success      float64 `json:"success"`
	AvgCost      float64 `json:"avg_cost"`
	P50Duration  string  `json:"p50_duration"`
}

// --------------------------------------------------------------------------
// Handlers
// --------------------------------------------------------------------------

// handleWorkshopFormulas serves GET /api/v1/workshop/formulas.
//
// Query params (both optional):
//
//	?source=embedded|custom — restrict to embedded or on-disk formulas
//	?category=task|bug|...|subgraph|custom — restrict to a derived category
//
// Returns a JSON array of FormulaInfoWire objects sorted by name.
func (s *Server) handleWorkshopFormulas(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	infos, err := workshop.ListFormulas()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	sourceFilter := r.URL.Query().Get("source")
	categoryFilter := r.URL.Query().Get("category")

	out := make([]FormulaInfoWire, 0, len(infos))
	for _, info := range infos {
		if sourceFilter != "" && info.Source != sourceFilter {
			continue
		}
		category, defaultFor := deriveCategoryAndDefaultFor(info.Name)
		if categoryFilter != "" && category != categoryFilter {
			continue
		}
		out = append(out, FormulaInfoWire{
			Name:        info.Name,
			Description: info.Description,
			Source:      info.Source,
			Category:    category,
			DefaultFor:  defaultFor,
			Version:     info.Version,
			StepCount:   len(info.Phases),
		})
	}

	writeJSON(w, http.StatusOK, out)
}

// handleWorkshopFormulaByName routes /api/v1/workshop/formulas/{name},
// /{name}/source, and /{name}/validate. Method-checks happen inside the
// per-resource handler so the routing layer stays simple.
func (s *Server) handleWorkshopFormulaByName(w http.ResponseWriter, r *http.Request) {
	rest := pathSuffix(r.URL.Path, "/api/v1/workshop/formulas/")
	if rest == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "formula name required"})
		return
	}

	// Split on the first slash to isolate the sub-resource (if any). Names
	// themselves never contain slashes, so this is safe.
	name, sub := rest, ""
	if idx := strings.Index(rest, "/"); idx >= 0 {
		name = rest[:idx]
		sub = rest[idx+1:]
	}
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "formula name required"})
		return
	}

	switch sub {
	case "":
		s.handleWorkshopDetail(w, r, name)
	case "source":
		s.handleWorkshopSource(w, r, name)
	case "validate":
		s.handleWorkshopValidate(w, r, name)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown sub-resource"})
	}
}

// handleWorkshopDetail serves GET /api/v1/workshop/formulas/{name}. Loads
// the step graph, materializes edges from needs+when+resets, runs a
// DryRunStepGraph for the path enumeration, and pulls validation issues so
// the desktop can display all of it in a single fetch.
func (s *Server) handleWorkshopDetail(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	graph, err := formula.LoadStepGraphByName(name)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "formula not found"})
		return
	}

	// Resolve source by reading the raw bytes the same way LoadRawFormula
	// does. This lets the FormulaInfoWire row carry the right source label
	// even though LoadStepGraphByName strips it.
	rawData, source, err := workshop.LoadRawFormula(name)
	if err != nil {
		// Should not happen if LoadStepGraphByName succeeded, but bail
		// safely rather than emit a half-populated payload.
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	category, defaultFor := deriveCategoryAndDefaultFor(name)

	detail := FormulaDetailWire{
		FormulaInfoWire: FormulaInfoWire{
			Name:        graph.Name,
			Description: graph.Description,
			Source:      source,
			Category:    category,
			DefaultFor:  defaultFor,
			Version:     graph.Version,
			StepCount:   len(graph.Steps),
		},
		Entry:      formula.EntryStep(graph),
		Vars:       projectVars(graph),
		Workspaces: projectWorkspaces(graph),
		Steps:      projectSteps(graph),
		Edges:      materializeEdges(graph),
		Outputs:    parseOutputs(rawData),
	}

	// Issues: surface workshop validation to the desktop.
	issues, err := workshop.Validate(name)
	if err != nil {
		// Validation requires loading the formula bytes; if it fails after
		// LoadStepGraphByName succeeded, treat as soft error and emit empty.
		issues = nil
	}
	if issues == nil {
		issues = []workshop.Issue{}
	}
	detail.Issues = issues

	// Paths: derived via DFS in DryRunStepGraph. We only consume the Paths
	// field; the rest of the simulation overlaps with what we already
	// materialized above.
	if sim, err := workshop.DryRunStepGraph(graph); err == nil && sim != nil {
		detail.Paths = sim.Paths
	}
	if detail.Paths == nil {
		detail.Paths = [][]string{}
	}

	writeJSON(w, http.StatusOK, detail)
}

// handleWorkshopSource serves GET /api/v1/workshop/formulas/{name}/source.
// Returns the raw TOML bytes the operator can copy-paste into a custom PR.
func (s *Server) handleWorkshopSource(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	data, source, err := workshop.LoadRawFormula(name)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "formula not found"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"name":   name,
		"source": source,
		"toml":   string(data),
	})
}

// handleWorkshopValidate serves GET /api/v1/workshop/formulas/{name}/validate.
// Returns the structured validation findings as { issues: [...] }.
func (s *Server) handleWorkshopValidate(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	issues, err := workshop.Validate(name)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "formula not found"})
		return
	}
	if issues == nil {
		issues = []workshop.Issue{}
	}

	writeJSON(w, http.StatusOK, map[string][]workshop.Issue{"issues": issues})
}

// --------------------------------------------------------------------------
// Projection helpers
// --------------------------------------------------------------------------

// deriveCategoryAndDefaultFor reverse-looks up DefaultV3FormulaMap to find
// the bead types this formula is the default for, and derives the category
// label the desktop renders. Rule from docs/design/workshop-desktop.md §3.1:
//
//  1. If formula name appears as a value in DefaultV3FormulaMap, the
//     matching keys form `default_for`. The category is the first matching
//     key. We sort matches so that the bead type whose name matches the
//     formula's stem (everything before "-default") comes first; this
//     matches the design doc's expectation that task-default has
//     category="task" / default_for=["task", "feature"], not the other
//     way around.
//  2. If no matches AND name starts with "subgraph-": category is
//     "subgraph", default_for is [].
//  3. Otherwise: category is "custom", default_for is [].
//
// Always returns a non-nil default_for slice so JSON encodes [] not null.
func deriveCategoryAndDefaultFor(name string) (string, []string) {
	matches := make([]string, 0)
	for beadType, formulaName := range formula.DefaultV3FormulaMap {
		if formulaName == name {
			matches = append(matches, beadType)
		}
	}
	if len(matches) == 0 {
		if strings.HasPrefix(name, "subgraph-") {
			return "subgraph", []string{}
		}
		return "custom", []string{}
	}
	stem := strings.TrimSuffix(name, "-default")
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i] == stem {
			return true
		}
		if matches[j] == stem {
			return false
		}
		return matches[i] < matches[j]
	})
	return matches[0], matches
}

// projectVars projects a graph's vars map into a sorted slice for the wire.
// Sort by name for deterministic output.
func projectVars(g *formula.FormulaStepGraph) []VarWire {
	if len(g.Vars) == 0 {
		return []VarWire{}
	}
	names := make([]string, 0, len(g.Vars))
	for n := range g.Vars {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]VarWire, 0, len(names))
	for _, n := range names {
		v := g.Vars[n]
		typ := v.Type
		if typ == "" {
			typ = "string"
		}
		out = append(out, VarWire{
			Name:        n,
			Type:        typ,
			Required:    v.Required,
			Default:     v.Default,
			Description: v.Description,
		})
	}
	return out
}

// projectWorkspaces projects a graph's workspaces map into a sorted slice
// for the wire.
func projectWorkspaces(g *formula.FormulaStepGraph) []WorkspaceWire {
	if len(g.Workspaces) == 0 {
		return []WorkspaceWire{}
	}
	names := make([]string, 0, len(g.Workspaces))
	for n := range g.Workspaces {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]WorkspaceWire, 0, len(names))
	for _, n := range names {
		ws := g.Workspaces[n]
		out = append(out, WorkspaceWire{
			Name:      n,
			Kind:      ws.Kind,
			Branch:    ws.Branch,
			Base:      ws.Base,
			Scope:     ws.Scope,
			Ownership: ws.Ownership,
			Cleanup:   ws.Cleanup,
		})
	}
	return out
}

// projectSteps projects a graph's steps map into a sorted slice for the
// wire. Each entry preserves the StepConfig fields the desktop renders.
func projectSteps(g *formula.FormulaStepGraph) []StepWire {
	if len(g.Steps) == 0 {
		return []StepWire{}
	}
	names := make([]string, 0, len(g.Steps))
	for n := range g.Steps {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]StepWire, 0, len(names))
	for _, n := range names {
		sc := g.Steps[n]
		when := ""
		if sc.When != nil {
			when = workshop.RenderWhenPredicate(sc.When)
		} else if sc.Condition != "" {
			when = sc.Condition
		}
		needs := sc.Needs
		if needs == nil {
			needs = []string{}
		}
		out = append(out, StepWire{
			Name:        n,
			Kind:        sc.Kind,
			Action:      sc.Action,
			Title:       sc.Title,
			Needs:       needs,
			Terminal:    sc.Terminal,
			Workspace:   sc.Workspace,
			Graph:       sc.Graph,
			Flow:        sc.Flow,
			Role:        sc.Role,
			Model:       sc.Model,
			Timeout:     sc.Timeout,
			With:        sc.With,
			When:        when,
			Produces:    sc.Produces,
			Resets:      sc.Resets,
			VerdictOnly: sc.VerdictOnly,
		})
	}
	return out
}

// materializeEdges flattens needs[], when, and resets[] into an explicit
// edge list with kind ∈ {needs, guard, reset}. Mirrors the workshop's
// buildSuccessorMap topology, but keeps the edge kind so the desktop never
// has to recompute it. Sort order: by (from, to, kind) for determinism.
func materializeEdges(g *formula.FormulaStepGraph) []EdgeWire {
	edges := make([]EdgeWire, 0)

	// Forward edges: every step's needs[] becomes one or more incoming
	// edges. If the step has a when/condition, those incoming edges are
	// "guard" edges and carry the rendered when string; otherwise they
	// are plain "needs" edges.
	for stepName, step := range g.Steps {
		when := ""
		kind := "needs"
		if step.When != nil {
			when = workshop.RenderWhenPredicate(step.When)
			kind = "guard"
		} else if step.Condition != "" {
			when = step.Condition
			kind = "guard"
		}
		for _, need := range step.Needs {
			edges = append(edges, EdgeWire{
				From: need,
				To:   stepName,
				Kind: kind,
				When: when,
			})
		}
		// Reset back-edges: step X resets [Y...] declares X→Y reset edges.
		for _, target := range step.Resets {
			edges = append(edges, EdgeWire{
				From: stepName,
				To:   target,
				Kind: "reset",
			})
		}
	}

	sort.SliceStable(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		if edges[i].To != edges[j].To {
			return edges[i].To < edges[j].To
		}
		return edges[i].Kind < edges[j].Kind
	})
	return edges
}

// formulaWithOutputs is a minimal TOML projection used to extract
// [outputs.*] declarations. FormulaStepGraph itself doesn't store outputs
// (the runtime parses but ignores them), so we re-parse the raw bytes.
type formulaWithOutputs struct {
	Outputs map[string]formula.OutputDecl `toml:"outputs"`
}

// parseOutputs re-parses the raw TOML bytes to extract [outputs.*]
// declarations and projects them into wire-typed slices sorted by name.
// Returns an empty slice (never nil) so JSON encodes [] not null.
func parseOutputs(data []byte) []OutputWire {
	out := make([]OutputWire, 0)
	if len(data) == 0 {
		return out
	}
	var parsed formulaWithOutputs
	if err := toml.Unmarshal(data, &parsed); err != nil {
		return out
	}
	if len(parsed.Outputs) == 0 {
		return out
	}
	names := make([]string, 0, len(parsed.Outputs))
	for n := range parsed.Outputs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		o := parsed.Outputs[n]
		out = append(out, OutputWire{
			Name:        n,
			Type:        o.Type,
			Values:      o.Values,
			Description: o.Description,
		})
	}
	return out
}
