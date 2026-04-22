package recovery

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

func TestFormatPhaseEvent(t *testing.T) {
	cases := []struct {
		name    string
		ev      PhaseEvent
		want    []string // substrings the output must contain
		notWant []string // substrings the output must NOT contain
	}{
		{
			name: "collect_context with details",
			ev: PhaseEvent{
				Phase: "collect_context",
				Details: map[string]any{
					"class":       "merge-failure",
					"source_bead": "spi-orig",
				},
			},
			want: []string{"[collect_context]", "class=merge-failure", "source_bead=spi-orig"},
		},
		{
			name: "collect_context empty details",
			ev:   PhaseEvent{Phase: "collect_context"},
			want: []string{"[collect_context]"},
		},
		{
			name: "decide renders branch/action/confidence/reason",
			ev: PhaseEvent{
				Phase:      "decide",
				Branch:     "promoted-recipe",
				Action:     "rebase-onto-base",
				Confidence: 0.85,
				Reason:     "promoted recipe matched",
			},
			want: []string{
				"[decide]",
				"branch=promoted-recipe",
				"action=rebase-onto-base",
				"confidence=0.85",
				`reason="promoted recipe matched"`,
			},
		},
		{
			name: "execute with action and details",
			ev: PhaseEvent{
				Phase:  "execute",
				Action: "rebase-onto-base",
				Details: map[string]any{
					"mode": "mechanical",
				},
			},
			want: []string{"[execute]", "action=rebase-onto-base", "mode=mechanical"},
		},
		{
			name: "verify with verdict and kind",
			ev: PhaseEvent{
				Phase:   "verify",
				Verdict: "pass",
				Details: map[string]any{"kind": "build"},
			},
			want: []string{"[verify]", "verdict=pass", "kind=build"},
		},
		{
			name: "learn with details",
			ev: PhaseEvent{
				Phase: "learn",
				Details: map[string]any{
					"mode":    "mechanical",
					"verdict": "pass",
				},
			},
			want: []string{"[learn]", "mode=mechanical", "verdict=pass"},
		},
		{
			name: "finish renders as-is",
			ev: PhaseEvent{
				Phase:   "finish",
				Details: map[string]any{"outcome": "resume"},
			},
			want: []string{"[finish]", "outcome=resume"},
		},
		{
			name: "finish_needs_human renders with its own bracket label",
			ev: PhaseEvent{
				Phase:   "finish_needs_human",
				Details: map[string]any{"outcome": "escalate"},
			},
			want: []string{"[finish_needs_human]", "outcome=escalate"},
		},
		{
			name: "unknown phase falls through to generic shape",
			ev: PhaseEvent{
				Phase:   "retry_on_error",
				Details: map[string]any{"attempt": 2},
			},
			want: []string{"[retry_on_error]", "attempt=2"},
		},
		{
			name: "ERR trailer emitted when Err non-empty",
			ev: PhaseEvent{
				Phase: "execute",
				Err:   "boom",
			},
			want: []string{"[execute]", "ERR: boom"},
		},
		{
			name: "no ERR trailer when Err empty",
			ev: PhaseEvent{
				Phase:  "execute",
				Action: "noop",
			},
			want:    []string{"[execute]", "action=noop"},
			notWant: []string{"ERR:"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			FormatPhaseEvent(&buf, tc.ev)
			got := buf.String()
			for _, s := range tc.want {
				if !strings.Contains(got, s) {
					t.Errorf("output missing %q; got:\n%s", s, got)
				}
			}
			for _, s := range tc.notWant {
				if strings.Contains(got, s) {
					t.Errorf("output unexpectedly contains %q; got:\n%s", s, got)
				}
			}
		})
	}
}

func TestFormatPhaseEvent_NilWriter(t *testing.T) {
	// Must not panic when out is nil — defensive guard.
	FormatPhaseEvent(nil, PhaseEvent{Phase: "finish"})
}

func TestRenderDetails(t *testing.T) {
	cases := []struct {
		name    string
		details map[string]any
		want    string
	}{
		{"nil map", nil, ""},
		{"empty map", map[string]any{}, ""},
		{"single entry", map[string]any{"k": "v"}, "k=v"},
		{
			name: "multiple entries sorted alphabetically",
			details: map[string]any{
				"zebra": 3,
				"alpha": "a",
				"mango": true,
			},
			want: "alpha=a mango=true zebra=3",
		},
		{
			name:    "formats int and float via %v",
			details: map[string]any{"count": 42, "score": 0.5},
			want:    "count=42 score=0.5",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderDetails(tc.details); got != tc.want {
				t.Errorf("renderDetails = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEnsureRecoveryBead(t *testing.T) {
	// Swap out the package-level dep loader for the duration of the test.
	orig := depsForRecoveryCheck
	t.Cleanup(func() { depsForRecoveryCheck = orig })

	cases := []struct {
		name       string
		bead       store.Bead
		deps       []*beads.IssueWithDependencyMetadata
		depsErr    error
		wantErr    bool
		wantErrSub string
	}{
		{
			name: "accepts via recovery-bead label",
			bead: store.Bead{ID: "spi-r1", Labels: []string{"foo", "recovery-bead"}},
			// depsForRecoveryCheck should NOT be consulted on the label path.
			deps:    nil,
			wantErr: false,
		},
		{
			name: "accepts via caused-by dep edge",
			bead: store.Bead{ID: "spi-r2"},
			deps: []*beads.IssueWithDependencyMetadata{
				{DependencyType: beads.DependencyType("caused-by")},
			},
			wantErr: false,
		},
		{
			name: "accepts via recovery-for dep edge",
			bead: store.Bead{ID: "spi-r3"},
			deps: []*beads.IssueWithDependencyMetadata{
				{DependencyType: beads.DependencyType("recovery-for")},
			},
			wantErr: false,
		},
		{
			name:       "rejects when no label and no recovery dep",
			bead:       store.Bead{ID: "spi-r4", Labels: []string{"unrelated"}},
			deps:       []*beads.IssueWithDependencyMetadata{{DependencyType: beads.DependencyType("blocks")}},
			wantErr:    true,
			wantErrSub: "not a recovery bead",
		},
		{
			name:       "rejects when no label and no deps at all",
			bead:       store.Bead{ID: "spi-r5"},
			deps:       nil,
			wantErr:    true,
			wantErrSub: "not a recovery bead",
		},
		{
			name:       "propagates deps-load error with bead id wrapped",
			bead:       store.Bead{ID: "spi-r6"},
			depsErr:    errors.New("dolt down"),
			wantErr:    true,
			wantErrSub: "verify recovery bead spi-r6",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			depsForRecoveryCheck = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
				if id != tc.bead.ID {
					t.Errorf("depsForRecoveryCheck called with %q, want %q", id, tc.bead.ID)
				}
				return tc.deps, tc.depsErr
			}
			err := ensureRecoveryBead(tc.bead)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if tc.wantErrSub != "" && !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Errorf("error %q missing substring %q", err.Error(), tc.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestEnsureRecoveryBead_LabelShortCircuitsDepLoader(t *testing.T) {
	// With a recovery-bead label present, ensureRecoveryBead must
	// short-circuit BEFORE consulting depsForRecoveryCheck — the loader
	// is a network call in production and pointlessly invoking it would
	// regress the debug path.
	orig := depsForRecoveryCheck
	t.Cleanup(func() { depsForRecoveryCheck = orig })

	called := false
	depsForRecoveryCheck = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		called = true
		return nil, errors.New("should not be called")
	}

	bead := store.Bead{ID: "spi-r7", Labels: []string{"recovery-bead"}}
	if err := ensureRecoveryBead(bead); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called {
		t.Errorf("expected label short-circuit; depsForRecoveryCheck was invoked")
	}
}
