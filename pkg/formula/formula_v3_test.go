package formula

import "testing"

func TestLoadEmbeddedStepGraph_AllV3Formulas(t *testing.T) {
	names := []string{
		"review-phase",
		"epic-implement-phase",
		"spire-agent-work-v3",
		"spire-bugfix-v3",
		"spire-epic-v3",
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
	g, err := LoadStepGraphByName("spire-agent-work-v3")
	if err != nil {
		t.Fatalf("LoadStepGraphByName: %v", err)
	}
	if g.Name != "spire-agent-work-v3" {
		t.Errorf("expected spire-agent-work-v3, got %s", g.Name)
	}
}

func TestResolveAny_DefaultV3(t *testing.T) {
	bead := BeadInfo{ID: "spi-test", Type: "task", Labels: nil}
	f, v, err := ResolveAny(bead)
	if err != nil {
		t.Fatalf("ResolveAny: %v", err)
	}
	if v != 3 {
		t.Errorf("expected version 3, got %d", v)
	}
	fv3, ok := f.(*FormulaStepGraph)
	if !ok {
		t.Fatalf("expected *FormulaStepGraph, got %T", f)
	}
	if fv3.Name != "spire-agent-work-v3" {
		t.Errorf("expected spire-agent-work-v3, got %s", fv3.Name)
	}
}

func TestResolveAny_V2Label(t *testing.T) {
	bead := BeadInfo{ID: "spi-test", Type: "task", Labels: []string{"formula-version:2"}}
	f, v, err := ResolveAny(bead)
	if err != nil {
		t.Fatalf("ResolveAny: %v", err)
	}
	if v != 2 {
		t.Errorf("expected version 2, got %d", v)
	}
	fv2, ok := f.(*FormulaV2)
	if !ok {
		t.Fatalf("expected *FormulaV2, got %T", f)
	}
	if fv2.Name != "spire-agent-work" {
		t.Errorf("expected spire-agent-work, got %s", fv2.Name)
	}
}

func TestResolveAny_ExplicitV3Formula(t *testing.T) {
	bead := BeadInfo{ID: "spi-test", Type: "task", Labels: []string{"formula:spire-epic-v3"}}
	f, v, err := ResolveAny(bead)
	if err != nil {
		t.Fatalf("ResolveAny: %v", err)
	}
	if v != 3 {
		t.Errorf("expected version 3, got %d", v)
	}
	fv3, ok := f.(*FormulaStepGraph)
	if !ok {
		t.Fatalf("expected *FormulaStepGraph, got %T", f)
	}
	if fv3.Name != "spire-epic-v3" {
		t.Errorf("expected spire-epic-v3, got %s", fv3.Name)
	}
}

func TestResolveAny_ExplicitV2Formula(t *testing.T) {
	bead := BeadInfo{ID: "spi-test", Type: "task", Labels: []string{"formula:spire-agent-work"}}
	f, v, err := ResolveAny(bead)
	if err != nil {
		t.Fatalf("ResolveAny: %v", err)
	}
	if v != 2 {
		t.Errorf("expected version 2, got %d", v)
	}
	fv2, ok := f.(*FormulaV2)
	if !ok {
		t.Fatalf("expected *FormulaV2, got %T", f)
	}
	if fv2.Name != "spire-agent-work" {
		t.Errorf("expected spire-agent-work, got %s", fv2.Name)
	}
}

func TestResolveV3_ByType(t *testing.T) {
	tests := []struct {
		beadType     string
		expectedName string
	}{
		{"task", "spire-agent-work-v3"},
		{"bug", "spire-bugfix-v3"},
		{"epic", "spire-epic-v3"},
		{"chore", "spire-agent-work-v3"},
		{"feature", "spire-agent-work-v3"},
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

func TestWantsV2(t *testing.T) {
	if WantsV2(BeadInfo{Labels: nil}) {
		t.Error("nil labels should not want v2")
	}
	if WantsV2(BeadInfo{Labels: []string{"something-else"}}) {
		t.Error("unrelated label should not want v2")
	}
	if !WantsV2(BeadInfo{Labels: []string{"formula-version:2"}}) {
		t.Error("formula-version:2 label should want v2")
	}
	if WantsV2(BeadInfo{Labels: []string{"formula-version:3"}}) {
		t.Error("formula-version:3 label should not want v2")
	}
}

// TestV3FormulaGraph_AgentWork validates the structure of the spire-agent-work-v3 formula.
func TestV3FormulaGraph_AgentWork(t *testing.T) {
	g, err := LoadEmbeddedStepGraph("spire-agent-work-v3")
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

// TestV3FormulaGraph_Epic validates the structure of the spire-epic-v3 formula.
func TestV3FormulaGraph_Epic(t *testing.T) {
	g, err := LoadEmbeddedStepGraph("spire-epic-v3")
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
