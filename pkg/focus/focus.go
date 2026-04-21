// Package focus assembles the structured, machine-readable context for a
// bead that powers `spire focus --json` and downstream prompt builders.
//
// The *FocusContext returned by Build is the stable contract machine
// consumers depend on — notably `pkg/wizard.CaptureWizardFocus`, which
// will eventually import this type directly instead of scraping the
// human-readable `spire focus` stdout.
//
// The contract is deliberately scoped to high-signal sections:
// target bead core fields, description/acceptance, comments, direct
// dependencies, parent/thread summary, workspace state, and formula
// identity. Presentation-only sections (sibling agent-run listings,
// referrer dumps, recovery history, etc.) live in the text-mode
// renderer and are not part of the JSON shape.
package focus

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/awell-health/spire/pkg/executor"
	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// FocusContext is the stable machine-readable shape of a bead's focus
// context. Optional objects use pointers so consumers can distinguish
// "absent" from "empty"; slices use `omitempty` so they marshal as
// absent rather than empty arrays.
type FocusContext struct {
	Bead               FocusBead       `json:"bead"`
	Description        string          `json:"description,omitempty"`
	AcceptanceCriteria string          `json:"acceptance_criteria,omitempty"`
	Comments           []FocusComment  `json:"comments,omitempty"`
	Deps               []FocusDep      `json:"deps,omitempty"`
	Parent             *FocusBeadRef   `json:"parent,omitempty"`
	Thread             []FocusBeadRef  `json:"thread,omitempty"`
	Workspace          *FocusWorkspace `json:"workspace,omitempty"`
	Formula            *FocusFormula   `json:"formula,omitempty"`
}

// FocusBead is the target bead's core identifying fields.
type FocusBead struct {
	ID        string   `json:"id"`
	Type      string   `json:"type,omitempty"`
	Title     string   `json:"title"`
	Status    string   `json:"status,omitempty"`
	Priority  int      `json:"priority"`
	Assignee  string   `json:"assignee,omitempty"`
	Owner     string   `json:"owner,omitempty"`
	CreatedAt string   `json:"created_at,omitempty"`
	CreatedBy string   `json:"created_by,omitempty"`
	UpdatedAt string   `json:"updated_at,omitempty"`
	ClosedAt  string   `json:"closed_at,omitempty"`
	Labels    []string `json:"labels,omitempty"`
}

// FocusBeadRef is a short summary of a related bead (parent, thread sibling).
type FocusBeadRef struct {
	ID     string `json:"id"`
	Type   string `json:"type,omitempty"`
	Title  string `json:"title,omitempty"`
	Status string `json:"status,omitempty"`
}

// FocusComment is a single comment on the target bead.
type FocusComment struct {
	ID        string `json:"id,omitempty"`
	Author    string `json:"author,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	Body      string `json:"body"`
}

// FocusDep is a direct dependency of the target bead, with both the
// linked bead's identity and the relationship type.
type FocusDep struct {
	ID      string `json:"id"`
	Type    string `json:"type,omitempty"`
	Title   string `json:"title,omitempty"`
	Status  string `json:"status,omitempty"`
	DepType string `json:"dep_type"`
}

// FocusWorkspace summarizes the bead's runtime execution state.
type FocusWorkspace struct {
	ActiveStep string                            `json:"active_step,omitempty"`
	Steps      map[string]FocusStepState         `json:"steps,omitempty"`
	Workspaces map[string]FocusWorkspaceInstance `json:"workspaces,omitempty"`
}

// FocusStepState is a single step's persisted execution state.
type FocusStepState struct {
	Status         string            `json:"status"`
	StartedAt      string            `json:"started_at,omitempty"`
	CompletedAt    string            `json:"completed_at,omitempty"`
	CompletedCount int               `json:"completed_count,omitempty"`
	Outputs        map[string]string `json:"outputs,omitempty"`
}

// FocusWorkspaceInstance is a single declared workspace's runtime state.
type FocusWorkspaceInstance struct {
	Name       string `json:"name,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Branch     string `json:"branch,omitempty"`
	BaseBranch string `json:"base_branch,omitempty"`
	Status     string `json:"status,omitempty"`
	Ownership  string `json:"ownership,omitempty"`
	Scope      string `json:"scope,omitempty"`
}

// FocusFormula summarizes the resolved formula for the bead.
type FocusFormula struct {
	Name  string   `json:"name,omitempty"`
	Entry string   `json:"entry,omitempty"`
	Steps []string `json:"steps,omitempty"`
}

// Deps bundles the injection points Build uses to fetch bead data.
// Callers normally leave fields unset and Build wires the defaults
// from pkg/store, pkg/executor, and pkg/formula; tests override them
// to avoid standing up a real store.
type Deps struct {
	GetIssue       func(id string) (*beads.Issue, error)
	GetComments    func(id string) ([]*beads.Comment, error)
	GetDepsWithMeta func(id string) ([]*beads.IssueWithDependencyMetadata, error)
	GetChildren    func(parentID string) ([]store.Bead, error)
	ResolveFormula func(b *beads.Issue) (*formula.FormulaStepGraph, error)
	LoadGraphState func(beadID string) (*executor.GraphState, error)
}

// defaultDeps wires production implementations from the pkg/* packages.
func defaultDeps(configDirFn func() (string, error)) Deps {
	return Deps{
		GetIssue:        store.GetIssue,
		GetComments:     store.GetComments,
		GetDepsWithMeta: store.GetDepsWithMeta,
		GetChildren:     store.GetChildren,
		ResolveFormula: func(issue *beads.Issue) (*formula.FormulaStepGraph, error) {
			return formula.ResolveV3(formula.BeadInfo{
				ID:     issue.ID,
				Type:   string(issue.IssueType),
				Labels: issue.Labels,
			})
		},
		LoadGraphState: func(beadID string) (*executor.GraphState, error) {
			return executor.LoadGraphState("wizard-"+beadID, configDirFn)
		},
	}
}

// Build assembles a FocusContext for beadID. It is a pure gather operation:
// no prints, no state mutations. Consumers (text renderer, JSON encoder)
// read the returned value.
//
// ConfigDirFn locates the Spire config dir used to find the wizard's
// graph_state.json; pass nil to skip workspace population (useful in
// tests with no on-disk state).
func Build(ctx context.Context, beadID string, configDirFn func() (string, error)) (*FocusContext, error) {
	return BuildWithDeps(ctx, beadID, defaultDeps(configDirFn))
}

// BuildWithDeps is the test-friendly variant: it takes explicit Deps so
// tests can stub the store, executor, and formula paths independently.
func BuildWithDeps(ctx context.Context, beadID string, deps Deps) (*FocusContext, error) {
	_ = ctx // reserved for future use (cancellation across store calls)
	if deps.GetIssue == nil {
		return nil, fmt.Errorf("focus.Build: GetIssue dep not set")
	}

	issue, err := deps.GetIssue(beadID)
	if err != nil {
		return nil, fmt.Errorf("focus %s: %w", beadID, err)
	}

	fc := &FocusContext{
		Bead:               buildBead(issue),
		Description:        issue.Description,
		AcceptanceCriteria: issue.AcceptanceCriteria,
	}

	if deps.GetComments != nil {
		comments, cErr := deps.GetComments(beadID)
		if cErr == nil {
			fc.Comments = buildComments(comments)
		}
	}

	var parentID string
	if deps.GetDepsWithMeta != nil {
		raw, dErr := deps.GetDepsWithMeta(beadID)
		if dErr == nil {
			fc.Deps, parentID = buildDeps(raw)
		}
	}

	if parentID != "" && deps.GetIssue != nil {
		if parent, pErr := deps.GetIssue(parentID); pErr == nil {
			ref := issueRef(parent)
			fc.Parent = &ref
			if deps.GetChildren != nil {
				siblings, _ := deps.GetChildren(parentID)
				fc.Thread = buildThread(siblings, beadID)
			}
		}
	}

	if deps.ResolveFormula != nil {
		if graph, fErr := deps.ResolveFormula(issue); fErr == nil && graph != nil {
			fc.Formula = buildFormula(graph)
		}
	}

	if deps.LoadGraphState != nil {
		if gs, gErr := deps.LoadGraphState(beadID); gErr == nil && gs != nil {
			fc.Workspace = buildWorkspace(gs)
		}
	}

	return fc, nil
}

func buildBead(issue *beads.Issue) FocusBead {
	b := FocusBead{
		ID:       issue.ID,
		Type:     string(issue.IssueType),
		Title:    issue.Title,
		Status:   string(issue.Status),
		Priority: issue.Priority,
		Assignee: issue.Assignee,
		Owner:    issue.Owner,
		Labels:   issue.Labels,
	}
	if !issue.CreatedAt.IsZero() {
		b.CreatedAt = issue.CreatedAt.UTC().Format(time.RFC3339)
	}
	b.CreatedBy = issue.CreatedBy
	if !issue.UpdatedAt.IsZero() {
		b.UpdatedAt = issue.UpdatedAt.UTC().Format(time.RFC3339)
	}
	if issue.ClosedAt != nil && !issue.ClosedAt.IsZero() {
		b.ClosedAt = issue.ClosedAt.UTC().Format(time.RFC3339)
	}
	return b
}

func issueRef(issue *beads.Issue) FocusBeadRef {
	return FocusBeadRef{
		ID:     issue.ID,
		Type:   string(issue.IssueType),
		Title:  issue.Title,
		Status: string(issue.Status),
	}
}

func buildComments(comments []*beads.Comment) []FocusComment {
	if len(comments) == 0 {
		return nil
	}
	out := make([]FocusComment, 0, len(comments))
	for _, c := range comments {
		if c == nil {
			continue
		}
		fc := FocusComment{
			ID:     c.ID,
			Author: c.Author,
			Body:   c.Text,
		}
		if !c.CreatedAt.IsZero() {
			fc.CreatedAt = c.CreatedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, fc)
	}
	return out
}

// buildDeps flattens raw dependencies into FocusDep entries and returns
// the parent-child dep's target ID (for loading parent/thread context).
// Parent-child deps are excluded from the returned Deps slice — parent
// information surfaces separately via the Parent field.
func buildDeps(raw []*beads.IssueWithDependencyMetadata) ([]FocusDep, string) {
	if len(raw) == 0 {
		return nil, ""
	}
	var parentID string
	out := make([]FocusDep, 0, len(raw))
	for _, dm := range raw {
		if dm == nil {
			continue
		}
		depType := string(dm.DependencyType)
		if dm.DependencyType == beads.DepParentChild {
			if parentID == "" {
				parentID = dm.ID
			}
			continue
		}
		out = append(out, FocusDep{
			ID:      dm.ID,
			Type:    string(dm.IssueType),
			Title:   dm.Title,
			Status:  string(dm.Status),
			DepType: depType,
		})
	}
	if len(out) == 0 {
		return nil, parentID
	}
	return out, parentID
}

func buildThread(siblings []store.Bead, targetID string) []FocusBeadRef {
	if len(siblings) == 0 {
		return nil
	}
	out := make([]FocusBeadRef, 0, len(siblings))
	for _, s := range siblings {
		if s.ID == targetID {
			continue
		}
		out = append(out, FocusBeadRef{
			ID:     s.ID,
			Type:   s.Type,
			Title:  s.Title,
			Status: s.Status,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func buildFormula(graph *formula.FormulaStepGraph) *FocusFormula {
	if graph == nil {
		return nil
	}
	f := &FocusFormula{
		Name:  graph.Name,
		Entry: graph.Entry,
	}
	names := make([]string, 0, len(graph.Steps))
	for name := range graph.Steps {
		names = append(names, name)
	}
	sort.Strings(names)
	f.Steps = names
	return f
}

func buildWorkspace(gs *executor.GraphState) *FocusWorkspace {
	if gs == nil {
		return nil
	}
	w := &FocusWorkspace{ActiveStep: gs.ActiveStep}
	if len(gs.Steps) > 0 {
		steps := make(map[string]FocusStepState, len(gs.Steps))
		for name, ss := range gs.Steps {
			steps[name] = FocusStepState{
				Status:         ss.Status,
				StartedAt:      ss.StartedAt,
				CompletedAt:    ss.CompletedAt,
				CompletedCount: ss.CompletedCount,
				Outputs:        ss.Outputs,
			}
		}
		w.Steps = steps
	}
	if len(gs.Workspaces) > 0 {
		wss := make(map[string]FocusWorkspaceInstance, len(gs.Workspaces))
		for name, ws := range gs.Workspaces {
			wss[name] = FocusWorkspaceInstance{
				Name:       ws.Name,
				Kind:       ws.Kind,
				Branch:     ws.Branch,
				BaseBranch: ws.BaseBranch,
				Status:     ws.Status,
				Ownership:  ws.Ownership,
				Scope:      ws.Scope,
			}
		}
		w.Workspaces = wss
	}
	if w.ActiveStep == "" && len(w.Steps) == 0 && len(w.Workspaces) == 0 {
		return nil
	}
	return w
}
