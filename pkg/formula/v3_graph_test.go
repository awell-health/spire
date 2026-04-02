package formula

import "testing"

// --- NextSteps tests using real embedded formulas ---

func TestNextSteps_AgentWork_EntryPoint(t *testing.T) {
	g, err := LoadEmbeddedStepGraph("spire-agent-work-v3")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// Empty completed — should return the entry step "plan".
	next, err := NextSteps(g, map[string]bool{}, map[string]string{})
	if err != nil {
		t.Fatalf("NextSteps: %v", err)
	}
	if len(next) != 1 || next[0] != "plan" {
		t.Fatalf("expected [plan], got %v", next)
	}
}

func TestNextSteps_AgentWork_AfterPlan(t *testing.T) {
	g, err := LoadEmbeddedStepGraph("spire-agent-work-v3")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	completed := map[string]bool{"plan": true}
	ctx := map[string]string{}
	next, err := NextSteps(g, completed, ctx)
	if err != nil {
		t.Fatalf("NextSteps: %v", err)
	}
	if len(next) != 1 || next[0] != "implement" {
		t.Fatalf("expected [implement], got %v", next)
	}
}

func TestNextSteps_AgentWork_AfterReview_Merge(t *testing.T) {
	g, err := LoadEmbeddedStepGraph("spire-agent-work-v3")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	completed := map[string]bool{"plan": true, "implement": true, "review": true}
	ctx := map[string]string{"steps.review.outputs.outcome": "merge"}
	next, err := NextSteps(g, completed, ctx)
	if err != nil {
		t.Fatalf("NextSteps: %v", err)
	}
	if len(next) != 1 || next[0] != "merge" {
		t.Fatalf("expected [merge], got %v", next)
	}
}

func TestNextSteps_AgentWork_AfterReview_Discard(t *testing.T) {
	g, err := LoadEmbeddedStepGraph("spire-agent-work-v3")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	completed := map[string]bool{"plan": true, "implement": true, "review": true}
	ctx := map[string]string{"steps.review.outputs.outcome": "discard"}
	next, err := NextSteps(g, completed, ctx)
	if err != nil {
		t.Fatalf("NextSteps: %v", err)
	}
	if len(next) != 1 || next[0] != "discard" {
		t.Fatalf("expected [discard], got %v", next)
	}
}

func TestNextSteps_Epic_FullSequence(t *testing.T) {
	g, err := LoadEmbeddedStepGraph("spire-epic-v3")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// Walk through each step of the epic formula.
	steps := []struct {
		completed map[string]bool
		ctx       map[string]string
		wantStep  string
	}{
		{
			completed: map[string]bool{},
			ctx:       map[string]string{},
			wantStep:  "design-check",
		},
		{
			completed: map[string]bool{"design-check": true},
			ctx:       map[string]string{},
			wantStep:  "plan",
		},
		{
			completed: map[string]bool{"design-check": true, "plan": true},
			ctx:       map[string]string{},
			wantStep:  "materialize",
		},
		{
			completed: map[string]bool{"design-check": true, "plan": true, "materialize": true},
			ctx:       map[string]string{},
			wantStep:  "implement",
		},
		{
			completed: map[string]bool{"design-check": true, "plan": true, "materialize": true, "implement": true},
			ctx:       map[string]string{},
			wantStep:  "review",
		},
		{
			completed: map[string]bool{"design-check": true, "plan": true, "materialize": true, "implement": true, "review": true},
			ctx:       map[string]string{"steps.review.outputs.outcome": "merge"},
			wantStep:  "merge",
		},
		{
			completed: map[string]bool{"design-check": true, "plan": true, "materialize": true, "implement": true, "review": true, "merge": true},
			ctx:       map[string]string{"steps.review.outputs.outcome": "merge"},
			wantStep:  "close",
		},
	}

	for i, tt := range steps {
		next, err := NextSteps(g, tt.completed, tt.ctx)
		if err != nil {
			t.Fatalf("step %d: NextSteps: %v", i, err)
		}
		if len(next) != 1 || next[0] != tt.wantStep {
			t.Errorf("step %d: expected [%s], got %v", i, tt.wantStep, next)
		}
	}

	// Also test the discard branch.
	completed := map[string]bool{"design-check": true, "plan": true, "materialize": true, "implement": true, "review": true}
	ctx := map[string]string{"steps.review.outputs.outcome": "discard"}
	next, err := NextSteps(g, completed, ctx)
	if err != nil {
		t.Fatalf("discard: NextSteps: %v", err)
	}
	if len(next) != 1 || next[0] != "discard" {
		t.Fatalf("discard: expected [discard], got %v", next)
	}
}

func TestNextSteps_Review_ConditionalBranching(t *testing.T) {
	g, err := LoadEmbeddedStepGraph("review-phase")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	t.Run("approve goes to merge", func(t *testing.T) {
		completed := map[string]bool{"sage-review": true}
		ctx := map[string]string{"verdict": "approve"}
		next, err := NextSteps(g, completed, ctx)
		if err != nil {
			t.Fatalf("NextSteps: %v", err)
		}
		if len(next) != 1 || next[0] != "merge" {
			t.Errorf("expected [merge], got %v", next)
		}
	})

	t.Run("request_changes round<max goes to fix", func(t *testing.T) {
		completed := map[string]bool{"sage-review": true}
		ctx := map[string]string{"verdict": "request_changes", "round": "1", "max_rounds": "3"}
		next, err := NextSteps(g, completed, ctx)
		if err != nil {
			t.Fatalf("NextSteps: %v", err)
		}
		if len(next) != 1 || next[0] != "fix" {
			t.Errorf("expected [fix], got %v", next)
		}
	})

	t.Run("request_changes round>=max goes to arbiter", func(t *testing.T) {
		completed := map[string]bool{"sage-review": true}
		ctx := map[string]string{"verdict": "request_changes", "round": "3", "max_rounds": "3"}
		next, err := NextSteps(g, completed, ctx)
		if err != nil {
			t.Fatalf("NextSteps: %v", err)
		}
		if len(next) != 1 || next[0] != "arbiter" {
			t.Errorf("expected [arbiter], got %v", next)
		}
	})

	t.Run("arbiter merge goes to merge terminal", func(t *testing.T) {
		completed := map[string]bool{"sage-review": true, "arbiter": true}
		ctx := map[string]string{
			"verdict":          "request_changes",
			"round":            "3",
			"max_rounds":       "3",
			"arbiter_decision": "merge",
		}
		next, err := NextSteps(g, completed, ctx)
		if err != nil {
			t.Fatalf("NextSteps: %v", err)
		}
		if len(next) != 1 || next[0] != "merge" {
			t.Errorf("expected [merge], got %v", next)
		}
	})

	t.Run("arbiter discard goes to discard terminal", func(t *testing.T) {
		completed := map[string]bool{"sage-review": true, "arbiter": true}
		ctx := map[string]string{
			"verdict":          "request_changes",
			"round":            "3",
			"max_rounds":       "3",
			"arbiter_decision": "discard",
		}
		next, err := NextSteps(g, completed, ctx)
		if err != nil {
			t.Fatalf("NextSteps: %v", err)
		}
		if len(next) != 1 || next[0] != "discard" {
			t.Errorf("expected [discard], got %v", next)
		}
	})
}

func TestValidateGraph_AllEmbeddedV3(t *testing.T) {
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
				t.Fatalf("load %q: %v", name, err)
			}
			// LoadEmbeddedStepGraph already calls ParseFormulaStepGraph which
			// calls ValidateGraph internally, but we test explicitly to be sure.
			if err := ValidateGraph(g); err != nil {
				t.Fatalf("validate %q: %v", name, err)
			}
		})
	}
}
