package executor

import (
	"fmt"
	"testing"

	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
)

func TestBuildSeedMetadata(t *testing.T) {
	tests := []struct {
		name        string
		parentID    string
		failureType string
		nodeCtx     string
		want        recovery.RecoveryMetadata
	}{
		{
			name:        "empty nodeCtx",
			parentID:    "spi-abc",
			failureType: "merge-failure",
			nodeCtx:     "",
			want: recovery.RecoveryMetadata{
				FailureClass:     "merge-failure",
				SourceBead:       "spi-abc",
				SourceStep:       "",
				SourceFlow:       "",
				FailureSignature: "merge-failure",
			},
		},
		{
			name:        "nodeCtx with step and flow",
			parentID:    "spi-def",
			failureType: "step-failure",
			nodeCtx:     "step=implement action=wizard.run flow=implement workspace=feature",
			want: recovery.RecoveryMetadata{
				FailureClass:     "step-failure",
				SourceBead:       "spi-def",
				SourceStep:       "implement",
				SourceFlow:       "implement",
				FailureSignature: "step-failure:implement",
			},
		},
		{
			name:        "nodeCtx step only no flow",
			parentID:    "spi-ghi",
			failureType: "build-failure",
			nodeCtx:     "step=review",
			want: recovery.RecoveryMetadata{
				FailureClass:     "build-failure",
				SourceBead:       "spi-ghi",
				SourceStep:       "review",
				SourceFlow:       "",
				FailureSignature: "build-failure:review",
			},
		},
		{
			name:        "nodeCtx flow only no step",
			parentID:    "spi-jkl",
			failureType: "repo-resolution",
			nodeCtx:     "action=wizard.run flow=implement",
			want: recovery.RecoveryMetadata{
				FailureClass:     "repo-resolution",
				SourceBead:       "spi-jkl",
				SourceStep:       "",
				SourceFlow:       "implement",
				FailureSignature: "repo-resolution",
			},
		},
		{
			name:        "step appears after other fields with flow",
			parentID:    "spi-mno",
			failureType: "step-failure",
			nodeCtx:     "action=wizard.run step=merge flow=review workspace=feature",
			want: recovery.RecoveryMetadata{
				FailureClass:     "step-failure",
				SourceBead:       "spi-mno",
				SourceStep:       "merge",
				SourceFlow:       "review",
				FailureSignature: "step-failure:merge",
			},
		},
		{
			name:        "flow is task-plan",
			parentID:    "spi-pqr",
			failureType: "step-failure",
			nodeCtx:     "step=verify-build flow=task-plan",
			want: recovery.RecoveryMetadata{
				FailureClass:     "step-failure",
				SourceBead:       "spi-pqr",
				SourceStep:       "verify-build",
				SourceFlow:       "task-plan",
				FailureSignature: "step-failure:verify-build",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildSeedMetadata(tt.parentID, tt.failureType, tt.nodeCtx)
			if got.FailureClass != tt.want.FailureClass {
				t.Errorf("FailureClass = %q, want %q", got.FailureClass, tt.want.FailureClass)
			}
			if got.SourceBead != tt.want.SourceBead {
				t.Errorf("SourceBead = %q, want %q", got.SourceBead, tt.want.SourceBead)
			}
			if got.SourceStep != tt.want.SourceStep {
				t.Errorf("SourceStep = %q, want %q", got.SourceStep, tt.want.SourceStep)
			}
			if got.SourceFlow != tt.want.SourceFlow {
				t.Errorf("SourceFlow = %q, want %q", got.SourceFlow, tt.want.SourceFlow)
			}
			if got.FailureSignature != tt.want.FailureSignature {
				t.Errorf("FailureSignature = %q, want %q", got.FailureSignature, tt.want.FailureSignature)
			}
			if got.SourceFormula != "" {
				t.Errorf("SourceFormula = %q, want empty (not yet wired)", got.SourceFormula)
			}
		})
	}
}

func TestSeedRecoveryMetadata_EmptyRecoveryID(t *testing.T) {
	// seedRecoveryMetadata with empty recoveryID should return immediately
	// without calling store.SetBeadMetadataMap (which would fail without a db).
	// If it doesn't guard, this test panics or errors.
	seedRecoveryMetadata("", "spi-parent", "merge-failure", "step=implement")
}

// TestSuppressRecoveryEscalation_BoundedRetry is the spi-9eopwy regression
// test for the cleric-loop bug. The escalation-suppression branch
// originally added a comment and returned, with no upper bound on retries.
// A broken cleric (e.g. one whose stdout fails to parse on every run)
// kept the recovery bead open forever; the steward then dispatched a
// fresh cleric on every tick — burning an agent slot indefinitely.
//
// The fix counts suppressions on a `cleric-retry:N` label and, on the
// MaxClericEscalationRetries-th failure, closes the recovery bead and
// labels it `needs-human` so the steward stops re-dispatching.
func TestSuppressRecoveryEscalation_BoundedRetry(t *testing.T) {
	beadID := "spi-rec"
	state := &fakeRecoveryBeadState{
		bead: Bead{ID: beadID, Type: "recovery"},
	}

	deps := &Deps{
		GetBead:     func(id string) (Bead, error) { return state.get(id), nil },
		AddComment:  func(id, text string) error { state.recordComment(id, text); return nil },
		AddLabel:    func(id, label string) error { state.addLabel(id, label); return nil },
		RemoveLabel: func(id, label string) error { state.removeLabel(id, label); return nil },
		CloseBead:   func(id string) error { state.close(id); return nil },
	}

	// First two failures: bead stays open, retry label increments.
	for i := 1; i <= MaxClericEscalationRetries-1; i++ {
		if !suppressRecoveryEscalation(beadID, "step-failure", "parser bug", deps) {
			t.Fatalf("attempt %d: suppress returned false; want true on a recovery bead", i)
		}
		if state.closed {
			t.Fatalf("attempt %d: bead closed prematurely (count=%d, max=%d)", i, i, MaxClericEscalationRetries)
		}
		gotLabel := store.HasLabel(state.bead, LabelClericRetry)
		var got int
		fmt.Sscanf(gotLabel, "%d", &got)
		if got != i {
			t.Errorf("attempt %d: cleric-retry = %q (parsed %d), want %d", i, gotLabel, got, i)
		}
	}

	// Final failure: bead must close + carry needs-human label.
	if !suppressRecoveryEscalation(beadID, "step-failure", "parser bug", deps) {
		t.Fatalf("final attempt: suppress returned false; want true on a recovery bead")
	}
	if !state.closed {
		t.Errorf("bead should be closed after %d suppressed failures", MaxClericEscalationRetries)
	}
	if !state.hasLabel("needs-human") {
		t.Errorf("bead should carry needs-human label after exhaustion; labels=%v", state.bead.Labels)
	}
}

// TestSuppressRecoveryEscalation_NotARecoveryBead verifies the helper
// is a no-op on non-recovery beads — escalation paths must continue to
// run their alert/comment/archmage logic in that case.
func TestSuppressRecoveryEscalation_NotARecoveryBead(t *testing.T) {
	state := &fakeRecoveryBeadState{
		bead: Bead{ID: "spi-task", Type: "task"},
	}
	deps := &Deps{
		GetBead:    func(id string) (Bead, error) { return state.get(id), nil },
		AddComment: func(id, text string) error { state.recordComment(id, text); return nil },
		AddLabel:   func(id, label string) error { state.addLabel(id, label); return nil },
		CloseBead:  func(id string) error { state.close(id); return nil },
	}
	if suppressRecoveryEscalation("spi-task", "build-failure", "x", deps) {
		t.Fatal("suppress returned true on a non-recovery bead; the escalation must continue")
	}
	if state.closed {
		t.Error("non-recovery bead must not be closed by the suppressor")
	}
}

// fakeRecoveryBeadState is a minimal in-memory bead model for the
// retry-counter test. It captures label add/remove and close calls.
type fakeRecoveryBeadState struct {
	bead     Bead
	comments []string
	closed   bool
}

func (s *fakeRecoveryBeadState) get(id string) Bead {
	if s.bead.ID == id {
		return s.bead
	}
	return Bead{}
}

func (s *fakeRecoveryBeadState) recordComment(_id, text string) {
	s.comments = append(s.comments, text)
}

func (s *fakeRecoveryBeadState) addLabel(_id, label string) {
	for _, l := range s.bead.Labels {
		if l == label {
			return
		}
	}
	s.bead.Labels = append(s.bead.Labels, label)
}

func (s *fakeRecoveryBeadState) removeLabel(_id, label string) {
	out := s.bead.Labels[:0]
	for _, l := range s.bead.Labels {
		if l != label {
			out = append(out, l)
		}
	}
	s.bead.Labels = out
}

func (s *fakeRecoveryBeadState) hasLabel(label string) bool {
	for _, l := range s.bead.Labels {
		if l == label {
			return true
		}
	}
	return false
}

func (s *fakeRecoveryBeadState) close(_id string) {
	s.closed = true
}

// TestMessageArchmage_DerivesPrefix verifies that MessageArchmage creates
// the message bead using the source bead's prefix, not a hardcoded one.
func TestMessageArchmage_DerivesPrefix(t *testing.T) {
	tests := []struct {
		name       string
		beadID     string
		wantPrefix string
	}{
		{"spi prefix", "spi-abc123", "spi"},
		{"spd prefix", "spd-ac5", "spd"},
		{"web prefix", "web-xyz", "web"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPrefix string
			deps := &Deps{
				CreateBead: func(opts CreateOpts) (string, error) {
					gotPrefix = opts.Prefix
					return "msg-001", nil
				},
				AddDepTyped: func(issueID, dependsOnID, depType string) error { return nil },
			}
			MessageArchmage("test-agent", tt.beadID, "test message", deps)
			if gotPrefix != tt.wantPrefix {
				t.Errorf("CreateBead prefix = %q, want %q", gotPrefix, tt.wantPrefix)
			}
		})
	}
}
