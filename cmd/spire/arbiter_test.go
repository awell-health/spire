package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// arbiterTestHarness captures the side effects runArbiterDecide produces so
// tests can assert on review-round metadata writes, comments, and attempt
// closures without a live store.
type arbiterTestHarness struct {
	bead                Bead
	beadErr             error
	review              *Bead
	reviewErr           error
	metaWrittenOnID     string
	metadata            map[string]string
	closedReviewID      string
	closeReviewErr      error
	comments            []string
	commentTargets      []string
	activeAttempt       *Bead
	closedAttemptID     string
	closedAttemptResult string
}

func newArbiterHarness(t *testing.T, bead Bead) (*arbiterTestHarness, func()) {
	t.Helper()

	h := &arbiterTestHarness{
		bead:     bead,
		metadata: map[string]string{},
		// Default: a single open review-round exists for the task. Tests that
		// need a closed/missing/different round override after construction.
		review: &Bead{
			ID:     bead.ID + ".review",
			Title:  "review-round-1",
			Status: "in_progress",
			Labels: []string{"review-round", "round:1"},
			Parent: bead.ID,
		},
	}

	origGetBead := arbiterGetBeadFunc
	origSetMeta := arbiterSetMetadataMapFunc
	origAddComment := arbiterAddCommentFunc
	origGetAttempt := arbiterGetActiveAttemptFunc
	origCloseAttempt := arbiterCloseAttemptBeadFunc
	origMostRecent := arbiterMostRecentReviewFunc
	origCloseBead := arbiterCloseBeadFunc
	origNow := arbiterNowFunc

	arbiterGetBeadFunc = func(id string) (Bead, error) {
		if h.beadErr != nil {
			return Bead{}, h.beadErr
		}
		return h.bead, nil
	}
	arbiterSetMetadataMapFunc = func(id string, m map[string]string) error {
		h.metaWrittenOnID = id
		for k, v := range m {
			h.metadata[k] = v
		}
		return nil
	}
	arbiterAddCommentFunc = func(id, text string) error {
		h.comments = append(h.comments, text)
		h.commentTargets = append(h.commentTargets, id)
		return nil
	}
	arbiterGetActiveAttemptFunc = func(parentID string) (*Bead, error) {
		return h.activeAttempt, nil
	}
	arbiterCloseAttemptBeadFunc = func(attemptID, result string) error {
		h.closedAttemptID = attemptID
		h.closedAttemptResult = result
		return nil
	}
	arbiterMostRecentReviewFunc = func(parentID string) (*Bead, error) {
		if h.reviewErr != nil {
			return nil, h.reviewErr
		}
		return h.review, nil
	}
	arbiterCloseBeadFunc = func(id string) error {
		h.closedReviewID = id
		return h.closeReviewErr
	}
	arbiterNowFunc = func() time.Time {
		return time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	}

	cleanup := func() {
		arbiterGetBeadFunc = origGetBead
		arbiterSetMetadataMapFunc = origSetMeta
		arbiterAddCommentFunc = origAddComment
		arbiterGetActiveAttemptFunc = origGetAttempt
		arbiterCloseAttemptBeadFunc = origCloseAttempt
		arbiterMostRecentReviewFunc = origMostRecent
		arbiterCloseBeadFunc = origCloseBead
		arbiterNowFunc = origNow
	}
	return h, cleanup
}

// newDecideCmd builds a fresh cobra.Command that mirrors arbiterDecideCmd's
// flag shape. Using a fresh command avoids leaking SetArgs / parsed-flag
// state across tests that drive the command directly. For tests that need
// MarkFlagRequired enforcement, use executeArbiterDecide instead.
func newDecideCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:  "decide <bead>",
		Args: cobra.ExactArgs(1),
		RunE: runArbiterDecide,
	}
	cmd.Flags().String("verdict", "", "")
	cmd.Flags().String("note", "", "")
	return cmd
}

// executeArbiterDecide drives the real arbiterDecideCmd through rootCmd so
// cobra's flag validation (notably MarkFlagRequired) fires. Running from
// arbiterCmd.Execute directly bypasses root-level parsing and skips the
// required-flag check — rootCmd is the load-bearing entry point here.
// Stdout/stderr from cobra are swallowed so tests don't print help text.
func executeArbiterDecide(t *testing.T, args []string) error {
	t.Helper()

	origStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	defer func() {
		os.Stdout = origStdout
	}()

	var errBuf bytes.Buffer
	rootCmd.SetOut(&errBuf)
	rootCmd.SetErr(&errBuf)

	fullArgs := append([]string{"arbiter", "decide"}, args...)
	rootCmd.SetArgs(fullArgs)
	err := rootCmd.Execute()

	w.Close()
	_, _ = io.Copy(io.Discard, r)

	// Reset flag values so the next Execute doesn't inherit --verdict.
	_ = arbiterDecideCmd.Flags().Set("verdict", "")
	_ = arbiterDecideCmd.Flags().Set("note", "")

	return err
}

// --- Command registration ------------------------------------------------

func TestArbiterCmdRegistered(t *testing.T) {
	// arbiterCmd must be registered on rootCmd.
	var found *cobra.Command
	for _, c := range rootCmd.Commands() {
		if c == arbiterCmd {
			found = c
			break
		}
	}
	if found == nil {
		t.Fatal("arbiterCmd is not registered on rootCmd")
	}

	// arbiterDecideCmd must be registered on arbiterCmd.
	var decide *cobra.Command
	for _, c := range arbiterCmd.Commands() {
		if c == arbiterDecideCmd {
			decide = c
			break
		}
	}
	if decide == nil {
		t.Fatal("arbiterDecideCmd is not registered on arbiterCmd")
	}
	if !strings.HasPrefix(decide.Use, "decide") {
		t.Errorf("decide.Use = %q, want prefix 'decide'", decide.Use)
	}
}

// --- Happy path: accept --------------------------------------------------

func TestArbiterDecide_HappyPath_Accept(t *testing.T) {
	bead := Bead{ID: "spi-abc", Title: "dispute"}
	h, cleanup := newArbiterHarness(t, bead)
	defer cleanup()

	cmd := newDecideCmd()
	_ = cmd.Flags().Set("verdict", "accept")
	_ = cmd.Flags().Set("note", "approved after review")
	if err := runArbiterDecide(cmd, []string{"spi-abc"}); err != nil {
		t.Fatalf("runArbiterDecide: %v", err)
	}

	// Metadata must land on the review-round, not the task bead — that is
	// the storage boundary that makes the verdict binding (sage and arbiter
	// share the same review-round; readers consult it for the final word).
	if h.metaWrittenOnID != h.review.ID {
		t.Errorf("metadata written on %q, want review-round %q (task is %q)",
			h.metaWrittenOnID, h.review.ID, bead.ID)
	}

	raw, ok := h.metadata[arbiterVerdictMetaKey]
	if !ok {
		ks := make([]string, 0, len(h.metadata))
		for k := range h.metadata {
			ks = append(ks, k)
		}
		t.Fatalf("metadata key %q not set; got keys %v", arbiterVerdictMetaKey, ks)
	}
	var payload arbiterVerdictPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("unmarshal: %v\nraw: %s", err, raw)
	}
	if payload.Source != arbiterVerdictSource {
		t.Errorf("source = %q, want %q (the marker distinguishing arbiter from sage verdicts)",
			payload.Source, arbiterVerdictSource)
	}
	if payload.Verdict != "accept" {
		t.Errorf("verdict = %q, want accept", payload.Verdict)
	}
	if payload.Note != "approved after review" {
		t.Errorf("note = %q, want 'approved after review'", payload.Note)
	}
	if payload.DecidedAt == "" {
		t.Error("decided_at empty")
	}

	// Mirrored review_verdict on the same review-round so existing readers
	// don't need to parse the JSON payload.
	if h.metadata["review_verdict"] != "accept" {
		t.Errorf("review_verdict mirror = %q, want accept", h.metadata["review_verdict"])
	}

	// Open review-round must be closed.
	if h.closedReviewID != h.review.ID {
		t.Errorf("closedReviewID = %q, want %q", h.closedReviewID, h.review.ID)
	}

	if len(h.comments) == 0 {
		t.Fatal("no comments recorded")
	}
	if !strings.Contains(h.comments[0], "accept") {
		t.Errorf("comment %q does not mention 'accept'", h.comments[0])
	}
	// The summary comment goes on the task bead so humans browsing the task
	// see "arbiter decided X" without drilling into the review-round.
	if h.commentTargets[0] != bead.ID {
		t.Errorf("comment target = %q, want task %q", h.commentTargets[0], bead.ID)
	}
}

// --- Happy path: reject --------------------------------------------------

func TestArbiterDecide_HappyPath_Reject(t *testing.T) {
	bead := Bead{ID: "spi-rej", Title: "dispute"}
	h, cleanup := newArbiterHarness(t, bead)
	defer cleanup()

	// Open attempt should be closed as part of the binding verdict.
	h.activeAttempt = &Bead{ID: "spi-rej.attempt", Title: "attempt: wizard-x"}

	cmd := newDecideCmd()
	_ = cmd.Flags().Set("verdict", "reject")
	if err := runArbiterDecide(cmd, []string{"spi-rej"}); err != nil {
		t.Fatalf("runArbiterDecide: %v", err)
	}

	raw, ok := h.metadata[arbiterVerdictMetaKey]
	if !ok {
		t.Fatalf("metadata key %q not set", arbiterVerdictMetaKey)
	}
	var payload arbiterVerdictPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Verdict != "reject" {
		t.Errorf("verdict = %q, want reject", payload.Verdict)
	}
	if payload.Source != arbiterVerdictSource {
		t.Errorf("source = %q, want %q", payload.Source, arbiterVerdictSource)
	}

	// Verdict goes on the review-round, not the task — keep the boundaries
	// from drifting back as we extend the harness.
	if h.metaWrittenOnID != h.review.ID {
		t.Errorf("metadata written on %q, want review-round %q", h.metaWrittenOnID, h.review.ID)
	}

	if h.closedAttemptID != "spi-rej.attempt" {
		t.Errorf("closedAttemptID = %q, want spi-rej.attempt", h.closedAttemptID)
	}
	if h.closedAttemptResult != arbiterAttemptResult {
		t.Errorf("closedAttemptResult = %q, want %q", h.closedAttemptResult, arbiterAttemptResult)
	}
}

// --- Already-closed review-round: arbiter still binds, doesn't re-close ---

func TestArbiterDecide_ClosedReview_StillWritesNoCloseReplay(t *testing.T) {
	// A sage may have closed the round before the arbiter ran. The arbiter
	// must still write the binding verdict on that closed round (so readers
	// switch to it) but must NOT re-close the bead.
	bead := Bead{ID: "spi-late", Title: "dispute"}
	h, cleanup := newArbiterHarness(t, bead)
	defer cleanup()
	h.review = &Bead{
		ID:     "spi-late.review",
		Title:  "review-round-1",
		Status: "closed",
		Labels: []string{"review-round", "round:1"},
		Parent: bead.ID,
	}

	cmd := newDecideCmd()
	_ = cmd.Flags().Set("verdict", "reject")
	if err := runArbiterDecide(cmd, []string{"spi-late"}); err != nil {
		t.Fatalf("runArbiterDecide: %v", err)
	}

	if _, ok := h.metadata[arbiterVerdictMetaKey]; !ok {
		t.Errorf("arbiter_verdict not written on already-closed review-round")
	}
	if h.metaWrittenOnID != h.review.ID {
		t.Errorf("metaWrittenOnID = %q, want %q", h.metaWrittenOnID, h.review.ID)
	}
	if h.closedReviewID != "" {
		t.Errorf("closeBead called on already-closed review-round (%q); should be a no-op",
			h.closedReviewID)
	}
}

// --- No review-round: arbiter has nothing to bind ---

func TestArbiterDecide_NoReviewRound_Errors(t *testing.T) {
	bead := Bead{ID: "spi-noreview", Title: "dispute"}
	h, cleanup := newArbiterHarness(t, bead)
	defer cleanup()
	h.review = nil // task has no review-round child yet

	cmd := newDecideCmd()
	_ = cmd.Flags().Set("verdict", "accept")
	err := runArbiterDecide(cmd, []string{"spi-noreview"})
	if err == nil {
		t.Fatal("expected error when no review-round exists, got nil")
	}
	if !strings.Contains(err.Error(), "no review-round") {
		t.Errorf("error %q should mention 'no review-round'", err.Error())
	}
	if _, wrote := h.metadata[arbiterVerdictMetaKey]; wrote {
		t.Error("arbiter_verdict written despite missing review-round")
	}
}

// --- Invalid verdict -----------------------------------------------------

func TestArbiterDecide_InvalidVerdict(t *testing.T) {
	bead := Bead{ID: "spi-bad", Title: "dispute"}
	h, cleanup := newArbiterHarness(t, bead)
	defer cleanup()

	cmd := newDecideCmd()
	_ = cmd.Flags().Set("verdict", "bogus")
	err := runArbiterDecide(cmd, []string{"spi-bad"})
	if err == nil {
		t.Fatal("expected error for invalid verdict, got nil")
	}
	if !strings.Contains(err.Error(), "bogus") && !strings.Contains(err.Error(), "invalid") {
		t.Errorf("error = %q, want to mention bogus verdict or 'invalid'", err.Error())
	}
	if _, wrote := h.metadata[arbiterVerdictMetaKey]; wrote {
		t.Error("verdict metadata written despite invalid input")
	}
}

// --- Missing verdict flag ------------------------------------------------

func TestArbiterDecide_MissingVerdictFlag(t *testing.T) {
	bead := Bead{ID: "spi-mv", Title: "dispute"}
	_, cleanup := newArbiterHarness(t, bead)
	defer cleanup()

	err := executeArbiterDecide(t, []string{"spi-mv"})
	if err == nil {
		t.Fatal("expected error when --verdict flag missing, got nil")
	}
	if !strings.Contains(err.Error(), "verdict") {
		t.Errorf("error = %q, want to mention 'verdict'", err.Error())
	}
}

