package formula

import (
	"strings"
	"testing"
)

func TestEvalCondition(t *testing.T) {
	tests := []struct {
		name    string
		expr    string
		ctx     map[string]string
		want    bool
		wantErr bool
	}{
		{"empty condition", "", nil, true, false},
		{"simple eq true", "verdict == approve", map[string]string{"verdict": "approve"}, true, false},
		{"simple eq false", "verdict == approve", map[string]string{"verdict": "request_changes"}, false, false},
		{"and true", "verdict == request_changes && round < max_rounds",
			map[string]string{"verdict": "request_changes", "round": "1", "max_rounds": "3"}, true, false},
		{"and false at boundary", "verdict == request_changes && round < max_rounds",
			map[string]string{"verdict": "request_changes", "round": "3", "max_rounds": "3"}, false, false},
		{"or first branch", "verdict == approve || arbiter_decision == merge || arbiter_decision == split",
			map[string]string{"verdict": "approve"}, true, false},
		{"or second branch", "verdict == approve || arbiter_decision == merge || arbiter_decision == split",
			map[string]string{"verdict": "request_changes", "arbiter_decision": "merge"}, true, false},
		{"or third branch", "verdict == approve || arbiter_decision == merge || arbiter_decision == split",
			map[string]string{"verdict": "request_changes", "arbiter_decision": "split"}, true, false},
		{"or none match", "verdict == approve || arbiter_decision == merge",
			map[string]string{"verdict": "request_changes", "arbiter_decision": "discard"}, false, false},
		{"discard eq", "arbiter_decision == discard",
			map[string]string{"arbiter_decision": "discard"}, true, false},
		{"missing field", "verdict == approve", map[string]string{}, false, false},
		{"not equal true", "verdict != approve", map[string]string{"verdict": "request_changes"}, true, false},
		{"not equal false", "verdict != approve", map[string]string{"verdict": "approve"}, false, false},
		{"greater than", "round > max_rounds", map[string]string{"round": "4", "max_rounds": "3"}, true, false},
		{"greater eq true", "round >= max_rounds", map[string]string{"round": "3", "max_rounds": "3"}, true, false},
		{"greater eq false", "round >= max_rounds", map[string]string{"round": "2", "max_rounds": "3"}, false, false},
		{"less eq true", "round <= max_rounds", map[string]string{"round": "3", "max_rounds": "3"}, true, false},
		{"less eq false", "round <= max_rounds", map[string]string{"round": "4", "max_rounds": "3"}, false, false},
		{"non-numeric error", "round < max_rounds", map[string]string{"round": "abc", "max_rounds": "3"}, false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := EvalCondition(tt.expr, tt.ctx)
			if (err != nil) != tt.wantErr {
				t.Fatalf("EvalCondition() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("EvalCondition() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEvalPredicate(t *testing.T) {
	tests := []struct {
		name    string
		pred    Predicate
		ctx     map[string]string
		want    bool
		wantErr bool
	}{
		{
			name: "eq string match",
			pred: Predicate{Left: "verdict", Op: "eq", Right: "approve"},
			ctx:  map[string]string{"verdict": "approve"},
			want: true,
		},
		{
			name: "eq string no match",
			pred: Predicate{Left: "verdict", Op: "eq", Right: "approve"},
			ctx:  map[string]string{"verdict": "reject"},
			want: false,
		},
		{
			name: "ne true",
			pred: Predicate{Left: "verdict", Op: "ne", Right: "approve"},
			ctx:  map[string]string{"verdict": "reject"},
			want: true,
		},
		{
			name: "ne false",
			pred: Predicate{Left: "verdict", Op: "ne", Right: "approve"},
			ctx:  map[string]string{"verdict": "approve"},
			want: false,
		},
		{
			name: "lt numeric",
			pred: Predicate{Left: "round", Op: "lt", Right: "3"},
			ctx:  map[string]string{"round": "1"},
			want: true,
		},
		{
			name: "gt numeric",
			pred: Predicate{Left: "round", Op: "gt", Right: "3"},
			ctx:  map[string]string{"round": "5"},
			want: true,
		},
		{
			name: "le equal",
			pred: Predicate{Left: "round", Op: "le", Right: "3"},
			ctx:  map[string]string{"round": "3"},
			want: true,
		},
		{
			name: "ge less",
			pred: Predicate{Left: "round", Op: "ge", Right: "3"},
			ctx:  map[string]string{"round": "2"},
			want: false,
		},
		{
			name: "dotted path resolution",
			pred: Predicate{Left: "steps.review.outputs.verdict", Op: "eq", Right: "approve"},
			ctx:  map[string]string{"steps.review.outputs.verdict": "approve"},
			want: true,
		},
		{
			name: "right side context resolution",
			pred: Predicate{Left: "round", Op: "lt", Right: "vars.max_rounds"},
			ctx:  map[string]string{"round": "1", "vars.max_rounds": "3"},
			want: true,
		},
		{
			name: "missing dotted field returns false",
			pred: Predicate{Left: "steps.review.outputs.verdict", Op: "eq", Right: "approve"},
			ctx:  map[string]string{},
			want: false,
		},
		{
			name:    "non-numeric left errors on numeric op",
			pred:    Predicate{Left: "round", Op: "lt", Right: "3"},
			ctx:     map[string]string{"round": "abc"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := EvalPredicate(tt.pred, tt.ctx)
			if (err != nil) != tt.wantErr {
				t.Fatalf("EvalPredicate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("EvalPredicate() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEvalStructuredCondition(t *testing.T) {
	tests := []struct {
		name    string
		cond    *StructuredCondition
		ctx     map[string]string
		want    bool
		wantErr bool
	}{
		{
			name: "nil condition is true",
			cond: nil,
			ctx:  map[string]string{},
			want: true,
		},
		{
			name: "empty all and any is true",
			cond: &StructuredCondition{},
			ctx:  map[string]string{},
			want: true,
		},
		{
			name: "all-only all pass",
			cond: &StructuredCondition{
				All: []Predicate{
					{Left: "verdict", Op: "eq", Right: "approve"},
					{Left: "round", Op: "lt", Right: "3"},
				},
			},
			ctx:  map[string]string{"verdict": "approve", "round": "1"},
			want: true,
		},
		{
			name: "all-only one fails",
			cond: &StructuredCondition{
				All: []Predicate{
					{Left: "verdict", Op: "eq", Right: "approve"},
					{Left: "round", Op: "lt", Right: "3"},
				},
			},
			ctx:  map[string]string{"verdict": "reject", "round": "1"},
			want: false,
		},
		{
			name: "any-only one matches",
			cond: &StructuredCondition{
				Any: []Predicate{
					{Left: "verdict", Op: "eq", Right: "approve"},
					{Left: "verdict", Op: "eq", Right: "merge"},
				},
			},
			ctx:  map[string]string{"verdict": "merge"},
			want: true,
		},
		{
			name: "any-only none match",
			cond: &StructuredCondition{
				Any: []Predicate{
					{Left: "verdict", Op: "eq", Right: "approve"},
					{Left: "verdict", Op: "eq", Right: "merge"},
				},
			},
			ctx:  map[string]string{"verdict": "reject"},
			want: false,
		},
		{
			name: "both all and any pass",
			cond: &StructuredCondition{
				All: []Predicate{
					{Left: "round", Op: "ge", Right: "3"},
				},
				Any: []Predicate{
					{Left: "verdict", Op: "eq", Right: "approve"},
					{Left: "verdict", Op: "eq", Right: "merge"},
				},
			},
			ctx:  map[string]string{"round": "3", "verdict": "merge"},
			want: true,
		},
		{
			name: "all passes any fails",
			cond: &StructuredCondition{
				All: []Predicate{
					{Left: "round", Op: "ge", Right: "3"},
				},
				Any: []Predicate{
					{Left: "verdict", Op: "eq", Right: "approve"},
				},
			},
			ctx:  map[string]string{"round": "3", "verdict": "reject"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := EvalStructuredCondition(tt.cond, tt.ctx)
			if (err != nil) != tt.wantErr {
				t.Fatalf("EvalStructuredCondition() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("EvalStructuredCondition() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEvalStepCondition(t *testing.T) {
	tests := []struct {
		name    string
		step    StepConfig
		ctx     map[string]string
		want    bool
		wantErr bool
	}{
		{
			name: "when-only routes to structured",
			step: StepConfig{
				When: &StructuredCondition{
					All: []Predicate{{Left: "verdict", Op: "eq", Right: "approve"}},
				},
			},
			ctx:  map[string]string{"verdict": "approve"},
			want: true,
		},
		{
			name: "condition-only routes to string",
			step: StepConfig{
				Condition: "verdict == approve",
			},
			ctx:  map[string]string{"verdict": "approve"},
			want: true,
		},
		{
			name: "both set returns error",
			step: StepConfig{
				When: &StructuredCondition{
					All: []Predicate{{Left: "verdict", Op: "eq", Right: "approve"}},
				},
				Condition: "verdict == approve",
			},
			ctx:     map[string]string{"verdict": "approve"},
			wantErr: true,
		},
		{
			name: "neither returns true",
			step: StepConfig{},
			ctx:  map[string]string{},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := EvalStepCondition(tt.step, tt.ctx)
			if (err != nil) != tt.wantErr {
				t.Fatalf("EvalStepCondition() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Fatalf("EvalStepCondition() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEvalStepCondition_BothSetError(t *testing.T) {
	step := StepConfig{
		When:      &StructuredCondition{All: []Predicate{{Left: "x", Op: "eq", Right: "y"}}},
		Condition: "x == y",
	}
	_, err := EvalStepCondition(step, map[string]string{"x": "y"})
	if err == nil {
		t.Fatal("expected error when both when and condition are set")
	}
	if !strings.Contains(err.Error(), "both when and condition") {
		t.Fatalf("unexpected error: %v", err)
	}
}
