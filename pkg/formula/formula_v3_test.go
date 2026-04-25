package formula

import (
	"errors"
	"testing"

	"github.com/awell-health/spire/pkg/formula/embedded"
)

func TestLoadEmbeddedStepGraph_AllV3Formulas(t *testing.T) {
	names := []string{
		"subgraph-review",
		"subgraph-implement",
		"task-default",
		"bug-default",
		"epic-default",
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			g, err := LoadEmbeddedStepGraph(name)
			if err != nil {
				t.Fatalf("LoadEmbeddedStepGraph(%q): %v", name, err)
			}
			if g.Version != 3 {
				t.Errorf("expected version 3, got %d", g.Version)
			}
			if g.Name != name {
				t.Errorf("expected name %q, got %q", name, g.Name)
			}
			if len(g.Steps) == 0 {
				t.Error("expected at least one step")
			}
		})
	}
}

func TestLoadStepGraphByName_FallsBackToEmbedded(t *testing.T) {
	// LoadStepGraphByName should find embedded formulas.
	g, err := LoadStepGraphByName("task-default")
	if err != nil {
		t.Fatalf("LoadStepGraphByName: %v", err)
	}
	if g.Name != "task-default" {
		t.Errorf("expected task-default, got %s", g.Name)
	}
}

func TestResolveV3_Default(t *testing.T) {
	bead := BeadInfo{ID: "spi-test", Type: "task", Labels: nil}
	g, err := ResolveV3(bead)
	if err != nil {
		t.Fatalf("ResolveV3: %v", err)
	}
	if g.Name != "task-default" {
		t.Errorf("expected task-default, got %s", g.Name)
	}
}

func TestResolveV3_V2LabelIgnored(t *testing.T) {
	bead := BeadInfo{ID: "spi-test", Type: "task", Labels: []string{"formula-version:2"}}
	g, err := ResolveV3(bead)
	if err != nil {
		t.Fatalf("ResolveV3: %v", err)
	}
	if g.Name != "task-default" {
		t.Errorf("expected task-default, got %s", g.Name)
	}
}

func TestResolveV3_ExplicitFormula(t *testing.T) {
	bead := BeadInfo{ID: "spi-test", Type: "task", Labels: []string{"formula:epic-default"}}
	g, err := ResolveV3(bead)
	if err != nil {
		t.Fatalf("ResolveV3: %v", err)
	}
	if g.Name != "epic-default" {
		t.Errorf("expected epic-default, got %s", g.Name)
	}
}

func TestResolveV3_ByType(t *testing.T) {
	tests := []struct {
		beadType     string
		expectedName string
	}{
		{"task", "task-default"},
		{"bug", "bug-default"},
		{"epic", "epic-default"},
		{"chore", "chore-default"},
		{"feature", "task-default"},
	}

	for _, tt := range tests {
		t.Run(tt.beadType, func(t *testing.T) {
			bead := BeadInfo{ID: "spi-test", Type: tt.beadType}
			g, err := ResolveV3(bead)
			if err != nil {
				t.Fatalf("ResolveV3: %v", err)
			}
			if g.Name != tt.expectedName {
				t.Errorf("expected %s, got %s", tt.expectedName, g.Name)
			}
		})
	}
}

// TestV3FormulaGraph_AgentWork validates the structure of the task-default formula.
func TestV3FormulaGraph_AgentWork(t *testing.T) {
	g, err := LoadEmbeddedStepGraph("task-default")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// Check entry step.
	if g.Entry != "plan" {
		t.Errorf("expected entry=plan, got %s", g.Entry)
	}

	// Check step flow.
	expectedSteps := []string{"plan", "implement", "review", "merge", "close", "discard"}
	for _, name := range expectedSteps {
		if _, ok := g.Steps[name]; !ok {
			t.Errorf("missing step %q", name)
		}
	}

	// Check terminal steps.
	terminals := 0
	for name, step := range g.Steps {
		if step.Terminal {
			terminals++
			if name != "close" && name != "discard" {
				t.Errorf("unexpected terminal step: %s", name)
			}
		}
	}
	if terminals != 2 {
		t.Errorf("expected 2 terminal steps, got %d", terminals)
	}

	// Check workspace.
	if _, ok := g.Workspaces["feature"]; !ok {
		t.Error("missing workspace 'feature'")
	}
}

// TestV3FormulaGraph_Epic validates the structure of the epic-default formula.
func TestV3FormulaGraph_Epic(t *testing.T) {
	g, err := LoadEmbeddedStepGraph("epic-default")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if g.Entry != "design-check" {
		t.Errorf("expected entry=design-check, got %s", g.Entry)
	}

	// Check that design-check step uses check.design-linked action.
	dc, ok := g.Steps["design-check"]
	if !ok {
		t.Fatal("missing design-check step")
	}
	if dc.Action != "check.design-linked" {
		t.Errorf("design-check action: expected check.design-linked, got %s", dc.Action)
	}

	// Check that materialize uses beads.materialize_plan.
	mat, ok := g.Steps["materialize"]
	if !ok {
		t.Fatal("missing materialize step")
	}
	if mat.Action != "beads.materialize_plan" {
		t.Errorf("materialize action: expected beads.materialize_plan, got %s", mat.Action)
	}

	// Check workspace.
	if _, ok := g.Workspaces["staging"]; !ok {
		t.Error("missing workspace 'staging'")
	}
}

// validTowerTOML returns a valid v3 formula TOML string for use in tower
// fetcher tests. It reads the embedded task-default formula.
func validTowerTOML(t *testing.T) string {
	t.Helper()
	data, err := embedded.Formulas.ReadFile("formulas/task-default.formula.toml")
	if err != nil {
		t.Fatalf("read embedded formula: %v", err)
	}
	return string(data)
}

// setTowerFetcher installs a TowerFetcher for the duration of a test.
func setTowerFetcher(t *testing.T, fn func(string) (string, error)) {
	t.Helper()
	prev := TowerFetcher
	TowerFetcher = fn
	t.Cleanup(func() { TowerFetcher = prev })
}

func TestLoadStepGraphByNameWithSource_TowerWins(t *testing.T) {
	toml := validTowerTOML(t)
	setTowerFetcher(t, func(name string) (string, error) {
		if name == "task-default" {
			return toml, nil
		}
		return "", errors.New("not found")
	})

	g, source, err := LoadStepGraphByNameWithSource("task-default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if source != "tower" {
		t.Errorf("expected source=tower, got %q", source)
	}
	if g.Name != "task-default" {
		t.Errorf("expected name=task-default, got %q", g.Name)
	}
}

func TestLoadStepGraphByNameWithSource_FallsToEmbedded_NoTower(t *testing.T) {
	// TowerFetcher is nil — should fall through to embedded.
	setTowerFetcher(t, nil)

	g, source, err := LoadStepGraphByNameWithSource("task-default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if source != "embedded" {
		t.Errorf("expected source=embedded, got %q", source)
	}
	if g.Name != "task-default" {
		t.Errorf("expected name=task-default, got %q", g.Name)
	}
}

func TestLoadStepGraphByNameWithSource_TowerError_FallsThrough(t *testing.T) {
	// TowerFetcher returns error (dolt unreachable) — should fall through.
	setTowerFetcher(t, func(name string) (string, error) {
		return "", errors.New("connection refused")
	})

	g, source, err := LoadStepGraphByNameWithSource("task-default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if source != "embedded" {
		t.Errorf("expected source=embedded, got %q", source)
	}
	if g.Name != "task-default" {
		t.Errorf("expected name=task-default, got %q", g.Name)
	}
}

func TestLoadStepGraphByNameWithSource_MalformedTower_FallsThrough(t *testing.T) {
	// Tower returns invalid TOML — should log warning and fall through.
	setTowerFetcher(t, func(name string) (string, error) {
		return "this is not valid TOML {{{{", nil
	})

	g, source, err := LoadStepGraphByNameWithSource("task-default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if source != "embedded" {
		t.Errorf("expected source=embedded after malformed tower, got %q", source)
	}
	if g.Name != "task-default" {
		t.Errorf("expected name=task-default, got %q", g.Name)
	}
}

func TestLoadStepGraphByNameWithSource_TowerMiss_FallsThrough(t *testing.T) {
	// Tower returns empty content — should fall through to embedded.
	setTowerFetcher(t, func(name string) (string, error) {
		return "", nil // empty = not found
	})

	g, source, err := LoadStepGraphByNameWithSource("task-default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if source != "embedded" {
		t.Errorf("expected source=embedded, got %q", source)
	}
	if g.Name != "task-default" {
		t.Errorf("expected name=task-default, got %q", g.Name)
	}
}

func TestLoadStepGraphByNameWithSource_NotFoundAnywhere(t *testing.T) {
	setTowerFetcher(t, func(name string) (string, error) {
		return "", errors.New("not found")
	})

	_, _, err := LoadStepGraphByNameWithSource("nonexistent-formula-xyz")
	if err == nil {
		t.Fatal("expected error for nonexistent formula")
	}
}
