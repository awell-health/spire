package wizard

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/beads"
)

func TestImplementResult_ClassifiesClaudeFailure(t *testing.T) {
	failure := newImplementClaudeFailure(errors.New("claude cli: exit status 1"), ClaudeMetrics{
		IsError:        true,
		Subtype:        "error_max_turns",
		TerminalReason: "max_turns",
		StopReason:     "tool_use",
		Turns:          151,
	})
	if failure == nil {
		t.Fatal("expected implement failure metadata")
	}

	tests := []struct {
		name        string
		committed   bool
		buildPassed bool
		testsPassed bool
		failure     *implementClaudeFailure
		want        string
	}{
		{"no commit after claude failure", false, true, true, failure, "implement_failure"},
		{"commit after claude failure is salvage", true, true, true, failure, "partial"},
		{"build failure wins", true, false, true, failure, "build_failure"},
		{"clean no changes remains no_changes", false, true, true, nil, "no_changes"},
		{"clean commit success", true, true, true, nil, "success"},
		{"clean commit with test failure", true, true, false, nil, "test_failure"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := implementResult(tt.committed, tt.buildPassed, tt.testsPassed, tt.failure)
			if got != tt.want {
				t.Fatalf("implementResult() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewImplementClaudeFailure_DetectsStructuredAbnormalTermination(t *testing.T) {
	tests := []struct {
		name    string
		metrics ClaudeMetrics
		want    bool
	}{
		{"clean end turn", ClaudeMetrics{StopReason: "end_turn", Subtype: "success"}, false},
		{"is_error", ClaudeMetrics{IsError: true}, true},
		{"terminal reason max turns", ClaudeMetrics{TerminalReason: "max_turns"}, true},
		{"stop reason max turns", ClaudeMetrics{StopReason: "max_turns"}, true},
		{"error subtype", ClaudeMetrics{Subtype: "error_max_turns"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := newImplementClaudeFailure(nil, tt.metrics) != nil
			if got != tt.want {
				t.Fatalf("newImplementClaudeFailure() present = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestAnnotateImplementClaudeFailureLabelsAndComments(t *testing.T) {
	failure := newImplementClaudeFailure(errors.New("claude cli: exit status 1"), ClaudeMetrics{
		IsError:        true,
		APIErrorStatus: 429,
		StopReason:     "stop_sequence",
		Turns:          51,
	})

	var labels []string
	var comments []string
	deps := &Deps{
		AddLabel: func(_, label string) error {
			labels = append(labels, label)
			return nil
		},
		GetComments: func(_ string) ([]*beads.Comment, error) {
			return nil, nil
		},
		AddComment: func(_, text string) error {
			comments = append(comments, text)
			return nil
		},
	}

	annotateImplementClaudeFailure("spi-test", "partial", true, true, failure, deps, func(string, ...interface{}) {})

	if len(labels) != 1 || labels[0] != "implement:partial-api-429" {
		t.Fatalf("labels = %v, want [implement:partial-api-429]", labels)
	}
	if len(comments) != 1 {
		t.Fatalf("comments = %d, want 1", len(comments))
	}
	for _, want := range []string{"result=partial", "reason=api-429", "api_error_status=429"} {
		if !strings.Contains(comments[0], want) {
			t.Fatalf("comment missing %q:\n%s", want, comments[0])
		}
	}
}
