package formula

import "testing"

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
