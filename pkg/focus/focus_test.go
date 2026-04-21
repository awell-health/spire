package focus

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/executor"
	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// Fixed timestamps used across fixture issues so RFC3339 formatting is
// deterministic in snapshot assertions.
var (
	tsCreated = time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	tsUpdated = time.Date(2026, 4, 2, 11, 30, 0, 0, time.UTC)
	tsClosed  = time.Date(2026, 4, 3, 12, 45, 0, 0, time.UTC)
)

func mustMarshal(t *testing.T, v interface{}) string {
	t.Helper()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(data)
}

func mustUnmarshal(t *testing.T, raw string) map[string]interface{} {
	t.Helper()
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func sampleIssue() *beads.Issue {
	return &beads.Issue{
		ID:                 "spi-test",
		Title:              "Test bead",
		Description:        "A bead we use in tests.",
		AcceptanceCriteria: "1. It works.",
		Status:             beads.StatusInProgress,
		Priority:           2,
		IssueType:          beads.TypeTask,
		Assignee:           "agent",
		Owner:              "jbb@jbb.dev",
		CreatedBy:          "JB",
		CreatedAt:          tsCreated,
		UpdatedAt:          tsUpdated,
		Labels:             []string{"feat-branch:feat/spi-test", "workflow-step"},
	}
}

func baseDeps(issue *beads.Issue) Deps {
	return Deps{
		GetIssue: func(id string) (*beads.Issue, error) {
			if id == issue.ID {
				return issue, nil
			}
			return nil, beadsNotFound(id)
		},
	}
}

type notFoundErr struct{ id string }

func (e notFoundErr) Error() string { return "bead " + e.id + " not found" }

func beadsNotFound(id string) error { return notFoundErr{id: id} }

// --- FocusContext shape tests ---

func TestBuild_MinimalBead(t *testing.T) {
	issue := sampleIssue()
	fc, err := BuildWithDeps(context.Background(), issue.ID, baseDeps(issue))
	if err != nil {
		t.Fatalf("BuildWithDeps: %v", err)
	}

	if fc.Bead.ID != issue.ID {
		t.Errorf("bead.id = %q, want %q", fc.Bead.ID, issue.ID)
	}
	if fc.Bead.Type != "task" {
		t.Errorf("bead.type = %q, want task", fc.Bead.Type)
	}
	if fc.Bead.Status != "in_progress" {
		t.Errorf("bead.status = %q, want in_progress", fc.Bead.Status)
	}
	if fc.Bead.Assignee != "agent" {
		t.Errorf("bead.assignee = %q, want agent", fc.Bead.Assignee)
	}
	if fc.Bead.Owner != "jbb@jbb.dev" {
		t.Errorf("bead.owner = %q", fc.Bead.Owner)
	}
	if fc.Bead.CreatedBy != "JB" {
		t.Errorf("bead.created_by = %q", fc.Bead.CreatedBy)
	}
	if fc.Bead.CreatedAt != "2026-04-01T10:00:00Z" {
		t.Errorf("bead.created_at = %q (not RFC3339 UTC)", fc.Bead.CreatedAt)
	}
	if fc.Bead.UpdatedAt != "2026-04-02T11:30:00Z" {
		t.Errorf("bead.updated_at = %q", fc.Bead.UpdatedAt)
	}
	if fc.Description != issue.Description {
		t.Errorf("description mismatch")
	}
	if fc.AcceptanceCriteria != issue.AcceptanceCriteria {
		t.Errorf("acceptance_criteria mismatch")
	}
}

func TestBuild_NoAcceptanceNoParent_MarshalsCleanly(t *testing.T) {
	issue := sampleIssue()
	issue.AcceptanceCriteria = ""

	fc, err := BuildWithDeps(context.Background(), issue.ID, baseDeps(issue))
	if err != nil {
		t.Fatalf("BuildWithDeps: %v", err)
	}

	raw := mustMarshal(t, fc)
	obj := mustUnmarshal(t, raw)

	if _, ok := obj["acceptance_criteria"]; ok {
		t.Errorf("acceptance_criteria should be absent, not empty string; got: %s", raw)
	}
	if _, ok := obj["parent"]; ok {
		t.Errorf("parent should be absent (null-ish), got key present in: %s", raw)
	}
	if _, ok := obj["comments"]; ok {
		t.Errorf("comments should be absent when empty, got key present in: %s", raw)
	}
	if _, ok := obj["deps"]; ok {
		t.Errorf("deps should be absent when empty, got key present in: %s", raw)
	}
	if _, ok := obj["thread"]; ok {
		t.Errorf("thread should be absent when empty, got key present in: %s", raw)
	}
	if _, ok := obj["workspace"]; ok {
		t.Errorf("workspace should be absent when no graph state, got: %s", raw)
	}
	if _, ok := obj["formula"]; ok {
		t.Errorf("formula should be absent when no resolver, got: %s", raw)
	}

	bead, ok := obj["bead"].(map[string]interface{})
	if !ok {
		t.Fatalf("bead missing or not an object: %s", raw)
	}
	if bead["id"] != "spi-test" {
		t.Errorf("bead.id = %v", bead["id"])
	}
}

func TestBuild_ComputesClosedAt(t *testing.T) {
	issue := sampleIssue()
	issue.ClosedAt = &tsClosed

	fc, err := BuildWithDeps(context.Background(), issue.ID, baseDeps(issue))
	if err != nil {
		t.Fatalf("BuildWithDeps: %v", err)
	}
	if fc.Bead.ClosedAt != "2026-04-03T12:45:00Z" {
		t.Errorf("bead.closed_at = %q, want 2026-04-03T12:45:00Z", fc.Bead.ClosedAt)
	}
}

// --- Comments ---

func TestBuild_PopulatesComments(t *testing.T) {
	issue := sampleIssue()
	commentTime := time.Date(2026, 4, 4, 9, 0, 0, 0, time.UTC)
	deps := baseDeps(issue)
	deps.GetComments = func(id string) ([]*beads.Comment, error) {
		return []*beads.Comment{
			{ID: "c1", IssueID: id, Author: "spire", Text: "plan goes here", CreatedAt: commentTime},
			{ID: "c2", IssueID: id, Author: "", Text: "anonymous note", CreatedAt: time.Time{}},
			nil, // should be tolerated
		}, nil
	}

	fc, err := BuildWithDeps(context.Background(), issue.ID, deps)
	if err != nil {
		t.Fatalf("BuildWithDeps: %v", err)
	}

	if len(fc.Comments) != 2 {
		t.Fatalf("len(comments) = %d, want 2 (nil filtered)", len(fc.Comments))
	}
	c0 := fc.Comments[0]
	if c0.ID != "c1" || c0.Author != "spire" || c0.Body != "plan goes here" {
		t.Errorf("comment 0: %+v", c0)
	}
	if c0.CreatedAt != "2026-04-04T09:00:00Z" {
		t.Errorf("comment 0 created_at = %q", c0.CreatedAt)
	}
	if fc.Comments[1].CreatedAt != "" {
		t.Errorf("comment 1 should have empty created_at for zero time, got %q", fc.Comments[1].CreatedAt)
	}

	// JSON should include comments with the documented keys.
	raw := mustMarshal(t, fc)
	if !strings.Contains(raw, "\"author\": \"spire\"") {
		t.Errorf("JSON missing author: %s", raw)
	}
	if !strings.Contains(raw, "\"body\": \"plan goes here\"") {
		t.Errorf("JSON missing body: %s", raw)
	}
}

// --- Deps / parent / thread ---

func TestBuild_GroupsDepsAndExtractsParent(t *testing.T) {
	issue := sampleIssue()
	parent := &beads.Issue{
		ID:        "spi-parent",
		Title:     "Parent epic",
		Status:    beads.StatusOpen,
		IssueType: beads.TypeEpic,
	}
	sibling := store.Bead{
		ID:     "spi-sib",
		Title:  "Sibling task",
		Status: "open",
		Type:   "task",
	}

	deps := Deps{
		GetIssue: func(id string) (*beads.Issue, error) {
			switch id {
			case issue.ID:
				return issue, nil
			case parent.ID:
				return parent, nil
			}
			return nil, beadsNotFound(id)
		},
		GetDepsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			return []*beads.IssueWithDependencyMetadata{
				{
					Issue:          beads.Issue{ID: parent.ID, Title: parent.Title, Status: parent.Status, IssueType: parent.IssueType},
					DependencyType: beads.DepParentChild,
				},
				{
					Issue:          beads.Issue{ID: "spi-blocker", Title: "Blocks me", Status: beads.StatusOpen, IssueType: beads.TypeTask},
					DependencyType: beads.DepBlocks,
				},
				{
					Issue:          beads.Issue{ID: "spi-design", Title: "Design note", Status: beads.StatusClosed, IssueType: beads.IssueType("design")},
					DependencyType: beads.DepDiscoveredFrom,
				},
				nil,
			}, nil
		},
		GetChildren: func(parentID string) ([]store.Bead, error) {
			if parentID != parent.ID {
				t.Errorf("GetChildren called with %q, expected parent id", parentID)
			}
			return []store.Bead{
				sibling,
				{ID: issue.ID, Title: "self", Status: "in_progress", Type: "task"}, // must be filtered out
			}, nil
		},
	}

	fc, err := BuildWithDeps(context.Background(), issue.ID, deps)
	if err != nil {
		t.Fatalf("BuildWithDeps: %v", err)
	}

	if fc.Parent == nil {
		t.Fatal("expected parent to be populated")
	}
	if fc.Parent.ID != parent.ID {
		t.Errorf("parent.id = %q", fc.Parent.ID)
	}
	if fc.Parent.Type != "epic" {
		t.Errorf("parent.type = %q, want epic", fc.Parent.Type)
	}
	if fc.Parent.Title != parent.Title {
		t.Errorf("parent.title = %q", fc.Parent.Title)
	}

	if len(fc.Deps) != 2 {
		t.Fatalf("len(deps) = %d, want 2 (parent-child excluded, nil filtered)", len(fc.Deps))
	}
	depTypes := map[string]FocusDep{}
	for _, d := range fc.Deps {
		depTypes[d.DepType] = d
	}
	if _, ok := depTypes["parent-child"]; ok {
		t.Errorf("parent-child should not appear in deps: %+v", fc.Deps)
	}
	if d, ok := depTypes["blocks"]; !ok || d.ID != "spi-blocker" {
		t.Errorf("missing blocks dep: %+v", fc.Deps)
	}
	if d, ok := depTypes["discovered-from"]; !ok || d.ID != "spi-design" || d.Type != "design" {
		t.Errorf("missing discovered-from dep: %+v", fc.Deps)
	}

	if len(fc.Thread) != 1 {
		t.Fatalf("len(thread) = %d, want 1 (self filtered)", len(fc.Thread))
	}
	if fc.Thread[0].ID != sibling.ID {
		t.Errorf("thread[0].id = %q", fc.Thread[0].ID)
	}
}

func TestBuild_NoParent_NoThread(t *testing.T) {
	issue := sampleIssue()
	deps := baseDeps(issue)
	deps.GetDepsWithMeta = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return []*beads.IssueWithDependencyMetadata{
			{
				Issue:          beads.Issue{ID: "spi-blocker", Title: "Blocks me", Status: beads.StatusOpen, IssueType: beads.TypeTask},
				DependencyType: beads.DepBlocks,
			},
		}, nil
	}
	childCalled := false
	deps.GetChildren = func(parentID string) ([]store.Bead, error) {
		childCalled = true
		return nil, nil
	}

	fc, err := BuildWithDeps(context.Background(), issue.ID, deps)
	if err != nil {
		t.Fatalf("BuildWithDeps: %v", err)
	}
	if fc.Parent != nil {
		t.Errorf("parent should be nil, got %+v", fc.Parent)
	}
	if fc.Thread != nil {
		t.Errorf("thread should be nil, got %+v", fc.Thread)
	}
	if childCalled {
		t.Errorf("GetChildren should not be called when there is no parent")
	}
}

// --- Workspace / formula ---

func TestBuild_PopulatesFormulaAndWorkspace(t *testing.T) {
	issue := sampleIssue()
	deps := baseDeps(issue)
	deps.ResolveFormula = func(i *beads.Issue) (*formula.FormulaStepGraph, error) {
		return &formula.FormulaStepGraph{
			Name:  "task-default",
			Entry: "implement",
			Steps: map[string]formula.StepConfig{
				"implement": {},
				"review":    {Needs: []string{"implement"}},
				"merge":     {Needs: []string{"review"}},
			},
		}, nil
	}
	deps.LoadGraphState = func(beadID string) (*executor.GraphState, error) {
		return &executor.GraphState{
			BeadID:     beadID,
			ActiveStep: "implement",
			Steps: map[string]executor.StepState{
				"implement": {Status: "active", StartedAt: "2026-04-01T10:00:00Z"},
				"review":    {Status: "pending"},
				"merge":     {Status: "pending"},
			},
			Workspaces: map[string]executor.WorkspaceState{
				"feature": {
					Name:       "feature",
					Kind:       "owned_worktree",
					Branch:     "feat/spi-test",
					BaseBranch: "main",
					Status:     "active",
					Ownership:  "owned",
					Scope:      "run",
				},
			},
		}, nil
	}

	fc, err := BuildWithDeps(context.Background(), issue.ID, deps)
	if err != nil {
		t.Fatalf("BuildWithDeps: %v", err)
	}

	if fc.Formula == nil {
		t.Fatal("formula should be populated")
	}
	if fc.Formula.Name != "task-default" {
		t.Errorf("formula.name = %q", fc.Formula.Name)
	}
	if fc.Formula.Entry != "implement" {
		t.Errorf("formula.entry = %q", fc.Formula.Entry)
	}
	wantSteps := []string{"implement", "merge", "review"} // sorted alphabetically
	if len(fc.Formula.Steps) != len(wantSteps) {
		t.Fatalf("formula.steps = %v, want %v", fc.Formula.Steps, wantSteps)
	}
	for i, name := range wantSteps {
		if fc.Formula.Steps[i] != name {
			t.Errorf("formula.steps[%d] = %q, want %q (expected alphabetical sort)", i, fc.Formula.Steps[i], name)
		}
	}

	if fc.Workspace == nil {
		t.Fatal("workspace should be populated")
	}
	if fc.Workspace.ActiveStep != "implement" {
		t.Errorf("workspace.active_step = %q", fc.Workspace.ActiveStep)
	}
	if len(fc.Workspace.Steps) != 3 {
		t.Errorf("workspace.steps len = %d", len(fc.Workspace.Steps))
	}
	if fc.Workspace.Steps["implement"].Status != "active" {
		t.Errorf("workspace.steps[implement].status = %q", fc.Workspace.Steps["implement"].Status)
	}
	if fc.Workspace.Workspaces["feature"].Branch != "feat/spi-test" {
		t.Errorf("workspace.workspaces[feature].branch = %q", fc.Workspace.Workspaces["feature"].Branch)
	}
}

func TestBuild_WorkspaceAbsentWhenNoGraphState(t *testing.T) {
	issue := sampleIssue()
	deps := baseDeps(issue)
	deps.LoadGraphState = func(beadID string) (*executor.GraphState, error) {
		return nil, nil // no persisted state
	}

	fc, err := BuildWithDeps(context.Background(), issue.ID, deps)
	if err != nil {
		t.Fatalf("BuildWithDeps: %v", err)
	}
	if fc.Workspace != nil {
		t.Errorf("workspace should be nil, got %+v", fc.Workspace)
	}

	raw := mustMarshal(t, fc)
	obj := mustUnmarshal(t, raw)
	if _, ok := obj["workspace"]; ok {
		t.Errorf("workspace key should be absent when nil: %s", raw)
	}
}

func TestBuild_RequiresGetIssue(t *testing.T) {
	_, err := BuildWithDeps(context.Background(), "spi-x", Deps{})
	if err == nil {
		t.Fatal("expected error when GetIssue dep is nil")
	}
	if !strings.Contains(err.Error(), "GetIssue") {
		t.Errorf("error message should mention missing dep: %v", err)
	}
}

func TestBuild_PropagatesGetIssueError(t *testing.T) {
	_, err := BuildWithDeps(context.Background(), "spi-missing", Deps{
		GetIssue: func(id string) (*beads.Issue, error) {
			return nil, beadsNotFound(id)
		},
	})
	if err == nil {
		t.Fatal("expected error from missing bead")
	}
}

// --- Stability: top-level JSON keys ---

// TestBuild_JSONShape_TopLevelKeys locks in the documented top-level keys
// of the JSON shape. If you add a new section, extend the expected set here
// and bump the contract note in focus.go.
func TestBuild_JSONShape_TopLevelKeys(t *testing.T) {
	issue := sampleIssue()
	deps := baseDeps(issue)
	deps.GetComments = func(id string) ([]*beads.Comment, error) {
		return []*beads.Comment{{ID: "c", IssueID: id, Author: "a", Text: "t", CreatedAt: tsCreated}}, nil
	}
	deps.GetDepsWithMeta = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return []*beads.IssueWithDependencyMetadata{
			{
				Issue:          beads.Issue{ID: "spi-p", Title: "p", Status: beads.StatusOpen, IssueType: beads.TypeEpic},
				DependencyType: beads.DepParentChild,
			},
			{
				Issue:          beads.Issue{ID: "spi-b", Title: "b", Status: beads.StatusOpen, IssueType: beads.TypeTask},
				DependencyType: beads.DepBlocks,
			},
		}, nil
	}
	deps.GetIssue = func(id string) (*beads.Issue, error) {
		if id == "spi-p" {
			return &beads.Issue{ID: "spi-p", Title: "parent", Status: beads.StatusOpen, IssueType: beads.TypeEpic}, nil
		}
		return issue, nil
	}
	deps.GetChildren = func(parentID string) ([]store.Bead, error) {
		return []store.Bead{{ID: "spi-sib", Title: "sib", Status: "open", Type: "task"}}, nil
	}
	deps.ResolveFormula = func(i *beads.Issue) (*formula.FormulaStepGraph, error) {
		return &formula.FormulaStepGraph{Name: "task-default", Entry: "implement", Steps: map[string]formula.StepConfig{"implement": {}}}, nil
	}
	deps.LoadGraphState = func(beadID string) (*executor.GraphState, error) {
		return &executor.GraphState{BeadID: beadID, ActiveStep: "implement", Steps: map[string]executor.StepState{"implement": {Status: "active"}}}, nil
	}

	fc, err := BuildWithDeps(context.Background(), issue.ID, deps)
	if err != nil {
		t.Fatalf("BuildWithDeps: %v", err)
	}
	obj := mustUnmarshal(t, mustMarshal(t, fc))

	wantKeys := []string{
		"bead",
		"description",
		"acceptance_criteria",
		"comments",
		"deps",
		"parent",
		"thread",
		"workspace",
		"formula",
	}
	for _, k := range wantKeys {
		if _, ok := obj[k]; !ok {
			t.Errorf("expected top-level key %q to be present, got keys: %v", k, keysOf(obj))
		}
	}
}

func keysOf(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
