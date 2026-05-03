package clericexec

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/cleric"
	"github.com/awell-health/spire/pkg/summon"
)

// recordedCall captures arguments to a stubbed seam so tests assert on
// behavior without booting the underlying packages.
type recordedCall struct {
	verb       string
	beadID     string
	field      string
	value      string
	commentTo  string
	commentTxt string
}

// fakeClient builds an InProcClient where every seam records the call
// rather than reaching into pkg/store / pkg/summon / pkg/reset.
func fakeClient() (*InProcClient, *[]recordedCall) {
	calls := &[]recordedCall{}
	return &InProcClient{
		ResummonFunc: func(beadID, _ string) (summon.Result, error) {
			*calls = append(*calls, recordedCall{verb: "resummon", beadID: beadID})
			return summon.Result{WizardName: "wizard-" + beadID}, nil
		},
		DismissCloseFunc: func(id string) error {
			*calls = append(*calls, recordedCall{verb: "dismiss/close", beadID: id})
			return nil
		},
		DismissResetFunc: func(_ context.Context, beadID string) error {
			*calls = append(*calls, recordedCall{verb: "dismiss/reset", beadID: beadID})
			return nil
		},
		ResetHardFunc: func(_ context.Context, beadID string) error {
			*calls = append(*calls, recordedCall{verb: "reset_hard", beadID: beadID})
			return nil
		},
		AddLabelFunc: func(id, label string) error {
			*calls = append(*calls, recordedCall{verb: "label", beadID: id, value: label})
			return nil
		},
		UpdateBeadFunc: func(id string, updates map[string]interface{}) error {
			for k, v := range updates {
				*calls = append(*calls, recordedCall{verb: "update", beadID: id, field: k, value: stringify(v)})
			}
			return nil
		},
		AddCommentFunc: func(id, text string) (string, error) {
			*calls = append(*calls, recordedCall{verb: "comment", commentTo: id, commentTxt: text})
			return "c1", nil
		},
	}, calls
}

func stringify(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func TestExecute_Resummon_DispatchesToSummonRun(t *testing.T) {
	c, calls := fakeClient()
	res, err := c.Execute(context.Background(), cleric.ExecuteRequest{
		RecoveryBeadID: "spi-rec",
		SourceBeadID:   "spi-src",
		Proposal:       cleric.ProposedAction{Verb: "resummon"},
	})
	if err != nil {
		t.Fatalf("execute err: %v", err)
	}
	if !res.Success {
		t.Errorf("Success = false; want true")
	}
	if len(*calls) != 1 || (*calls)[0].verb != "resummon" || (*calls)[0].beadID != "spi-src" {
		t.Errorf("calls = %+v, want [{resummon spi-src}]", *calls)
	}
}

func TestExecute_Dismiss_RunsResetThenCloseAndStampsLabel(t *testing.T) {
	c, calls := fakeClient()
	res, err := c.Execute(context.Background(), cleric.ExecuteRequest{
		RecoveryBeadID: "spi-rec",
		SourceBeadID:   "spi-src",
		Proposal:       cleric.ProposedAction{Verb: "dismiss"},
	})
	if err != nil {
		t.Fatalf("execute err: %v", err)
	}
	if !res.Success {
		t.Errorf("Success = false; want true")
	}
	// Order: reset, label, close.
	verbs := make([]string, len(*calls))
	for i, c := range *calls {
		verbs[i] = c.verb
	}
	if len(verbs) < 3 || verbs[0] != "dismiss/reset" || verbs[1] != "label" || verbs[2] != "dismiss/close" {
		t.Errorf("verbs = %v; want [dismiss/reset label dismiss/close ...]", verbs)
	}
}

func TestExecute_UnknownVerb_SurfacesUnimplemented(t *testing.T) {
	c, _ := fakeClient()
	res, err := c.Execute(context.Background(), cleric.ExecuteRequest{
		RecoveryBeadID: "spi-rec",
		SourceBeadID:   "spi-src",
		Proposal:       cleric.ProposedAction{Verb: "totally-new-verb"},
	})
	if err == nil {
		t.Fatalf("expected error for unknown verb")
	}
	if !errors.Is(err, cleric.ErrGatewayUnimplemented) {
		t.Errorf("err = %v, want wraps ErrGatewayUnimplemented", err)
	}
	if res.Success {
		t.Errorf("Success = true; want false on unimplemented")
	}
}

func TestExecute_MissingSource_Errors(t *testing.T) {
	c, _ := fakeClient()
	_, err := c.Execute(context.Background(), cleric.ExecuteRequest{
		RecoveryBeadID: "spi-rec",
		Proposal:       cleric.ProposedAction{Verb: "resummon"},
	})
	if err == nil {
		t.Fatal("expected error for missing source bead")
	}
	if !strings.Contains(err.Error(), "source bead") {
		t.Errorf("err = %v, want mention of source bead", err)
	}
}

func TestExecute_CommentRequest_WritesToRecoveryNotSource(t *testing.T) {
	c, calls := fakeClient()
	res, err := c.Execute(context.Background(), cleric.ExecuteRequest{
		RecoveryBeadID: "spi-rec",
		SourceBeadID:   "spi-src",
		Proposal: cleric.ProposedAction{
			Verb: "comment-request-input",
			Args: map[string]string{"question": "what next?"},
		},
	})
	if err != nil {
		t.Fatalf("execute err: %v", err)
	}
	if !res.Success {
		t.Errorf("Success = false; want true")
	}
	if len(*calls) != 1 {
		t.Fatalf("calls = %d; want 1", len(*calls))
	}
	got := (*calls)[0]
	if got.verb != "comment" {
		t.Fatalf("verb = %q, want comment", got.verb)
	}
	// The comment must land on the recovery bead — not the source.
	if got.commentTo != "spi-rec" {
		t.Errorf("comment target = %q, want spi-rec", got.commentTo)
	}
	if !strings.Contains(got.commentTxt, "what next?") {
		t.Errorf("comment text = %q; want to contain question", got.commentTxt)
	}
}

// TestExecute_CommentRequest_AppendsContext is the spi-9eopwy
// follow-on: when the cleric emits both `question` and `context`, the
// comment body must surface both so the human reviewer has the
// background needed to answer.
func TestExecute_CommentRequest_AppendsContext(t *testing.T) {
	c, calls := fakeClient()
	_, err := c.Execute(context.Background(), cleric.ExecuteRequest{
		RecoveryBeadID: "spi-rec",
		SourceBeadID:   "spi-src",
		Proposal: cleric.ProposedAction{
			Verb: "comment-request-input",
			Args: map[string]string{
				"question": "Should we dismiss?",
				"context":  "Source bead is now closed and the work is obsolete.",
			},
		},
	})
	if err != nil {
		t.Fatalf("execute err: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("calls = %d; want 1", len(*calls))
	}
	got := (*calls)[0].commentTxt
	if !strings.Contains(got, "Should we dismiss?") {
		t.Errorf("comment %q missing question", got)
	}
	if !strings.Contains(got, "Source bead is now closed") {
		t.Errorf("comment %q missing context body", got)
	}
}

func TestExecute_UpdateStatus_AppliesStatus(t *testing.T) {
	c, calls := fakeClient()
	_, err := c.Execute(context.Background(), cleric.ExecuteRequest{
		RecoveryBeadID: "spi-rec",
		SourceBeadID:   "spi-src",
		Proposal: cleric.ProposedAction{
			Verb: "update_status",
			Args: map[string]string{"to": "open"},
		},
	})
	if err != nil {
		t.Fatalf("execute err: %v", err)
	}
	if len(*calls) != 1 || (*calls)[0].field != "status" || (*calls)[0].value != "open" {
		t.Errorf("calls = %+v; want one update {status, open}", *calls)
	}
}
