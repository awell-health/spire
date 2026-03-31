package store

import "testing"

func TestHasLabel(t *testing.T) {
	tests := []struct {
		name   string
		bead   Bead
		prefix string
		want   string
	}{
		{
			name:   "prefix match returns suffix",
			bead:   Bead{Labels: []string{"model:claude-opus-4", "attempt"}},
			prefix: "model:",
			want:   "claude-opus-4",
		},
		{
			name:   "no match returns empty",
			bead:   Bead{Labels: []string{"attempt", "branch:main"}},
			prefix: "model:",
			want:   "",
		},
		{
			name:   "empty labels returns empty",
			bead:   Bead{Labels: nil},
			prefix: "model:",
			want:   "",
		},
		{
			name:   "exact prefix with empty suffix",
			bead:   Bead{Labels: []string{"result:"}},
			prefix: "result:",
			want:   "",
		},
		{
			name:   "first matching prefix wins",
			bead:   Bead{Labels: []string{"model:opus", "model:sonnet"}},
			prefix: "model:",
			want:   "opus",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HasLabel(tt.bead, tt.prefix)
			if got != tt.want {
				t.Errorf("HasLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestContainsLabel(t *testing.T) {
	tests := []struct {
		name  string
		bead  Bead
		label string
		want  bool
	}{
		{"exact match", Bead{Labels: []string{"attempt", "review-round"}}, "attempt", true},
		{"no match", Bead{Labels: []string{"attempt"}}, "review-round", false},
		{"empty labels", Bead{Labels: nil}, "attempt", false},
		{"prefix is not exact", Bead{Labels: []string{"attempt:foo"}}, "attempt", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ContainsLabel(tt.bead, tt.label)
			if got != tt.want {
				t.Errorf("ContainsLabel() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBoardBead_HasLabelPrefix(t *testing.T) {
	tests := []struct {
		name   string
		bead   BoardBead
		prefix string
		want   string
	}{
		{
			name:   "prefix match returns suffix",
			bead:   BoardBead{Labels: []string{"result:success", "attempt"}},
			prefix: "result:",
			want:   "success",
		},
		{
			name:   "no match returns empty",
			bead:   BoardBead{Labels: []string{"attempt", "branch:main"}},
			prefix: "result:",
			want:   "",
		},
		{
			name:   "empty labels returns empty",
			bead:   BoardBead{Labels: nil},
			prefix: "result:",
			want:   "",
		},
		{
			name:   "first matching prefix wins",
			bead:   BoardBead{Labels: []string{"step:plan", "step:implement"}},
			prefix: "step:",
			want:   "plan",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.bead.HasLabelPrefix(tt.prefix)
			if got != tt.want {
				t.Errorf("HasLabelPrefix() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBoardBead_HasLabel(t *testing.T) {
	tests := []struct {
		name  string
		bead  BoardBead
		label string
		want  bool
	}{
		{"exact match", BoardBead{Labels: []string{"attempt", "workflow-step"}}, "attempt", true},
		{"no match", BoardBead{Labels: []string{"attempt"}}, "workflow-step", false},
		{"empty labels", BoardBead{Labels: nil}, "attempt", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.bead.HasLabel(tt.label)
			if got != tt.want {
				t.Errorf("HasLabel() = %v, want %v", got, tt.want)
			}
		})
	}
}
