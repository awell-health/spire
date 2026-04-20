package main

import (
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/store"
	"github.com/spf13/cobra"
)

// sageStubs captures calls made to the sage.go test seams so assertions can
// inspect exactly what the handler wrote and to which bead. Fields are
// populated lazily; unused fields stay at their zero values.
type sageStubs struct {
	closeReviewID       string
	closeReviewVerdict  string
	closeReviewSummary  string
	closeReviewRound    int
	closeReviewErrCount int
	closeReviewWarnCnt  int
	closeReviewFindings []store.ReviewFinding

	addedLabels   []struct{ id, label string }
	addedComments []struct{ id, text string }
}

// stubSageDeps swaps the sage.go test seams for in-memory fakes and returns
// the stubs plus a cleanup func that restores the originals. Each test that
// exercises the store path should defer the returned cleanup so swaps don't
// leak across tests in this package.
//
// By default the arbiter-bound guard returns nil (no arbiter has decided)
// so tests that don't touch arbiter state can keep their existing setup.
func stubSageDeps(t *testing.T) (*sageStubs, func()) {
	t.Helper()
	origGetChildren := sageGetChildrenFunc
	origCloseReview := sageCloseReviewFunc
	origAddLabel := sageAddLabelFunc
	origAddComment := sageAddCommentFunc
	origMostRecent := sageMostRecentReviewFunc

	s := &sageStubs{}
	sageCloseReviewFunc = func(reviewID, verdict, summary string, errorCount, warningCount, round int, findings []store.ReviewFinding) error {
		s.closeReviewID = reviewID
		s.closeReviewVerdict = verdict
		s.closeReviewSummary = summary
		s.closeReviewErrCount = errorCount
		s.closeReviewWarnCnt = warningCount
		s.closeReviewRound = round
		s.closeReviewFindings = findings
		return nil
	}
	sageAddLabelFunc = func(id, label string) error {
		s.addedLabels = append(s.addedLabels, struct{ id, label string }{id, label})
		return nil
	}
	sageAddCommentFunc = func(id, text string) error {
		s.addedComments = append(s.addedComments, struct{ id, text string }{id, text})
		return nil
	}
	sageMostRecentReviewFunc = func(parentID string) (*Bead, error) { return nil, nil }

	cleanup := func() {
		sageGetChildrenFunc = origGetChildren
		sageCloseReviewFunc = origCloseReview
		sageAddLabelFunc = origAddLabel
		sageAddCommentFunc = origAddComment
		sageMostRecentReviewFunc = origMostRecent
	}
	return s, cleanup
}

// TestSageCmdRegistered verifies sageCmd is wired onto rootCmd and carries
// both subcommands. This is the smoke test that proves init() ran.
func TestSageCmdRegistered(t *testing.T) {
	found := false
	for _, c := range rootCmd.Commands() {
		if c == sageCmd {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("sageCmd not registered on rootCmd")
	}

	var haveAccept, haveReject bool
	for _, c := range sageCmd.Commands() {
		switch c.Name() {
		case "accept":
			haveAccept = true
		case "reject":
			haveReject = true
		}
	}
	if !haveAccept {
		t.Error("sageCmd missing 'accept' subcommand")
	}
	if !haveReject {
		t.Error("sageCmd missing 'reject' subcommand")
	}
}

// TestSageAccept_HappyPath verifies that CLI "accept" translates to the
// canonical "approve" verdict on the open review-round bead, the parent
// gets the review-approved label (so DetectMergeReady picks it up), and no
// parallel verdict metadata lands on the parent.
func TestSageAccept_HappyPath(t *testing.T) {
	s, cleanup := stubSageDeps(t)
	defer cleanup()

	sageGetChildrenFunc = func(parentID string) ([]Bead, error) {
		return []Bead{
			{
				ID:     "spi-p.1",
				Title:  "review-round-1",
				Status: "in_progress",
				Labels: []string{"review-round", "sage:sage-1", "round:1"},
				Parent: parentID,
			},
		}, nil
	}

	if err := cmdSageAccept("spi-p", ""); err != nil {
		t.Fatalf("cmdSageAccept: %v", err)
	}

	if s.closeReviewID != "spi-p.1" {
		t.Errorf("closed review id = %q, want spi-p.1", s.closeReviewID)
	}
	if s.closeReviewVerdict != "approve" {
		t.Errorf("verdict = %q, want approve (canonical review-round verdict)", s.closeReviewVerdict)
	}
	if s.closeReviewRound != 1 {
		t.Errorf("round = %d, want 1", s.closeReviewRound)
	}

	haveApproved := false
	for _, l := range s.addedLabels {
		if l.id == "spi-p" && l.label == "review-approved" {
			haveApproved = true
		}
	}
	if !haveApproved {
		t.Errorf("expected review-approved label on parent spi-p, got %+v", s.addedLabels)
	}

	// No parent-bead verdict metadata should have been written — the
	// review-round bead is the single source of truth.
	for _, c := range s.addedComments {
		if c.id != "spi-p" {
			t.Errorf("unexpected comment target %q; sage should only comment on the parent", c.id)
		}
		if strings.Contains(c.text, "review_verdict") {
			t.Errorf("comment should not leak parent metadata verbiage: %q", c.text)
		}
	}
}

// TestSageAccept_WithComment verifies the optional comment is appended to
// the summary passed to CloseReviewBead and surfaces in the parent comment
// trail — on the review-round bead, not a parent metadata field.
func TestSageAccept_WithComment(t *testing.T) {
	s, cleanup := stubSageDeps(t)
	defer cleanup()

	sageGetChildrenFunc = func(parentID string) ([]Bead, error) {
		return []Bead{
			{
				ID:     "spi-p.1",
				Title:  "review-round-1",
				Status: "in_progress",
				Labels: []string{"review-round", "round:1"},
				Parent: parentID,
			},
		}, nil
	}

	if err := cmdSageAccept("spi-p", "LGTM, nice work"); err != nil {
		t.Fatalf("cmdSageAccept: %v", err)
	}
	if s.closeReviewVerdict != "approve" {
		t.Errorf("verdict = %q, want approve", s.closeReviewVerdict)
	}
	if !strings.Contains(s.closeReviewSummary, "LGTM, nice work") {
		t.Errorf("summary should carry the operator comment, got %q", s.closeReviewSummary)
	}
	if len(s.addedComments) != 1 {
		t.Fatalf("want 1 parent comment, got %d (%+v)", len(s.addedComments), s.addedComments)
	}
	if !strings.Contains(s.addedComments[0].text, "LGTM, nice work") {
		t.Errorf("parent comment = %q; want it to include the operator comment", s.addedComments[0].text)
	}
}

// TestSageReject_HappyPath verifies verdict translation for reject and that
// the feedback lands on the review-round bead summary; no parent-bead
// metadata path is used.
func TestSageReject_HappyPath(t *testing.T) {
	s, cleanup := stubSageDeps(t)
	defer cleanup()

	sageGetChildrenFunc = func(parentID string) ([]Bead, error) {
		return []Bead{
			{
				ID:     "spi-p.1",
				Title:  "review-round-1",
				Status: "in_progress",
				Labels: []string{"review-round", "round:1"},
				Parent: parentID,
			},
		}, nil
	}

	if err := cmdSageReject("spi-p", "missing error handling in foo.go"); err != nil {
		t.Fatalf("cmdSageReject: %v", err)
	}
	if s.closeReviewID != "spi-p.1" {
		t.Errorf("closed review id = %q, want spi-p.1", s.closeReviewID)
	}
	if s.closeReviewVerdict != "request_changes" {
		t.Errorf("verdict = %q, want request_changes (canonical review-round verdict)", s.closeReviewVerdict)
	}
	if s.closeReviewSummary != "missing error handling in foo.go" {
		t.Errorf("summary = %q, want the feedback text verbatim", s.closeReviewSummary)
	}
	if s.closeReviewRound != 1 {
		t.Errorf("round = %d, want 1", s.closeReviewRound)
	}
	// Reject must not add review-approved — that only fires on accept.
	for _, l := range s.addedLabels {
		if l.label == "review-approved" {
			t.Errorf("reject should not add review-approved label, got %+v", s.addedLabels)
		}
	}
	if len(s.addedComments) != 1 || s.addedComments[0].id != "spi-p" {
		t.Fatalf("want 1 parent comment on spi-p, got %+v", s.addedComments)
	}
	if !strings.Contains(s.addedComments[0].text, "request_changes") {
		t.Errorf("parent comment should mention request_changes, got %q", s.addedComments[0].text)
	}
	if !strings.Contains(s.addedComments[0].text, "missing error handling in foo.go") {
		t.Errorf("parent comment should include feedback text, got %q", s.addedComments[0].text)
	}
}

// TestSageReject_MissingFeedback verifies cobra.MarkFlagRequired is wired on
// the feedback flag. We check the annotation directly (avoids mutating the
// global sageRejectCmd) and also drive a minimal command with the same
// configuration to confirm the cobra validator produces a feedback-mentioning
// error.
func TestSageReject_MissingFeedback(t *testing.T) {
	fl := sageRejectCmd.Flag("feedback")
	if fl == nil {
		t.Fatal("feedback flag not defined on sageRejectCmd")
	}
	if _, ok := fl.Annotations[cobra.BashCompOneRequiredFlag]; !ok {
		t.Errorf("feedback flag missing required annotation; got %v", fl.Annotations)
	}

	fresh := &cobra.Command{
		Use:           "reject <bead>",
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE:          func(cmd *cobra.Command, args []string) error { return nil },
	}
	fresh.Flags().String("feedback", "", "Feedback text")
	if err := fresh.MarkFlagRequired("feedback"); err != nil {
		t.Fatalf("MarkFlagRequired: %v", err)
	}
	fresh.SetArgs([]string{"spi-x"})
	err := fresh.Execute()
	if err == nil {
		t.Fatal("expected error for missing --feedback flag")
	}
	if !strings.Contains(err.Error(), "feedback") {
		t.Errorf("error %q should mention 'feedback'", err.Error())
	}
}

// arbiterBoundReview returns a review-round bead carrying an
// arbiter_verdict payload, used to seed the arbiter-decided state.
func arbiterBoundReview(parent, id string, status string) *Bead {
	return &Bead{
		ID:     id,
		Title:  "review-round-1",
		Status: status,
		Labels: []string{"review-round", "round:1"},
		Parent: parent,
		Metadata: map[string]string{
			arbiterVerdictMetaKey: `{"source":"arbiter","verdict":"reject","decided_at":"2026-04-20T12:00:00Z"}`,
			"review_verdict":      "reject",
		},
	}
}

// TestSageAccept_RefusesAfterArbiter verifies sage accept refuses to write
// when the most recent review-round (open or closed) carries an arbiter
// verdict. This is the binding-verdict guarantee.
func TestSageAccept_RefusesAfterArbiter(t *testing.T) {
	for _, status := range []string{"in_progress", "closed"} {
		t.Run(status, func(t *testing.T) {
			_, cleanup := stubSageDeps(t)
			defer cleanup()

			sageMostRecentReviewFunc = func(parentID string) (*Bead, error) {
				return arbiterBoundReview(parentID, "spi-p.1", status), nil
			}
			sageCloseReviewFunc = func(reviewID, verdict, summary string, errorCount, warningCount, round int, findings []store.ReviewFinding) error {
				t.Fatalf("CloseReviewBead must not be called after arbiter decided; got id=%q", reviewID)
				return nil
			}
			sageGetChildrenFunc = func(parentID string) ([]Bead, error) {
				t.Fatal("getChildren must not run — guard runs first")
				return nil, nil
			}

			err := cmdSageAccept("spi-p", "looks fine")
			if err == nil {
				t.Fatal("expected error after arbiter decision, got nil")
			}
			if !strings.Contains(err.Error(), "arbiter") {
				t.Errorf("error %q should mention 'arbiter'", err.Error())
			}
			if !strings.Contains(err.Error(), "not accepted") {
				t.Errorf("error %q should mention 'not accepted'", err.Error())
			}
		})
	}
}

// TestSageReject_RefusesAfterArbiter mirrors the accept guard for reject.
func TestSageReject_RefusesAfterArbiter(t *testing.T) {
	_, cleanup := stubSageDeps(t)
	defer cleanup()

	sageMostRecentReviewFunc = func(parentID string) (*Bead, error) {
		return arbiterBoundReview(parentID, "spi-p.1", "closed"), nil
	}
	sageCloseReviewFunc = func(reviewID, verdict, summary string, errorCount, warningCount, round int, findings []store.ReviewFinding) error {
		t.Fatalf("CloseReviewBead must not be called after arbiter decided; got id=%q", reviewID)
		return nil
	}
	sageGetChildrenFunc = func(parentID string) ([]Bead, error) {
		t.Fatal("getChildren must not run — guard runs first")
		return nil, nil
	}

	err := cmdSageReject("spi-p", "more issues")
	if err == nil {
		t.Fatal("expected error after arbiter decision, got nil")
	}
	if !strings.Contains(err.Error(), "arbiter") {
		t.Errorf("error %q should mention 'arbiter'", err.Error())
	}
}

// TestSageReject_NoOpenReview verifies the command errors when the task bead
// has no open review-round child. Closed rounds and non-review children
// must not satisfy the check, and no verdict write must occur.
func TestSageReject_NoOpenReview(t *testing.T) {
	s, cleanup := stubSageDeps(t)
	defer cleanup()

	sageGetChildrenFunc = func(parentID string) ([]Bead, error) {
		return []Bead{
			{
				ID:     "spi-p.1",
				Title:  "review-round-1",
				Status: "closed",
				Labels: []string{"review-round", "round:1"},
				Parent: parentID,
			},
			{
				ID:     "spi-p.2",
				Title:  "attempt: wizard-x",
				Status: "in_progress",
				Labels: []string{"attempt"},
				Parent: parentID,
			},
		}, nil
	}
	sageCloseReviewFunc = func(reviewID, verdict, summary string, errorCount, warningCount, round int, findings []store.ReviewFinding) error {
		t.Fatalf("CloseReviewBead should not be called when no open review round; got id=%q", reviewID)
		return nil
	}

	err := cmdSageReject("spi-p", "whatever")
	if err == nil {
		t.Fatal("expected error when bead has no open review-round child")
	}
	if !strings.Contains(err.Error(), "no open review-round") {
		t.Errorf("error %q should mention 'no open review-round'", err.Error())
	}
	if len(s.addedLabels)+len(s.addedComments) != 0 {
		t.Errorf("failure path must not write to parent; got labels=%+v comments=%+v",
			s.addedLabels, s.addedComments)
	}
}

// TestSageVerdict_NoParentMetadataWrite guards against the spi-o475n
// split-brain regression: both verdicts must route through CloseReviewBead
// on the review-round bead and must NOT write a review_verdict field to the
// parent. We verify by ensuring only the review-round id is passed to
// sageCloseReviewFunc and that no label/comment carries the
// "review_verdict=" form that would indicate a metadata path.
func TestSageVerdict_NoParentMetadataWrite(t *testing.T) {
	s, cleanup := stubSageDeps(t)
	defer cleanup()

	sageGetChildrenFunc = func(parentID string) ([]Bead, error) {
		return []Bead{
			{
				ID:     "spi-p.1",
				Title:  "review-round-1",
				Status: "in_progress",
				Labels: []string{"review-round", "round:1"},
				Parent: parentID,
			},
		}, nil
	}

	if err := cmdSageAccept("spi-p", "ok"); err != nil {
		t.Fatalf("cmdSageAccept: %v", err)
	}
	if s.closeReviewID == "spi-p" {
		t.Error("verdict write targeted parent bead; must target review-round bead")
	}
	for _, l := range s.addedLabels {
		if strings.HasPrefix(l.label, "review_verdict") {
			t.Errorf("parent got review_verdict label %q; verdict must live on review-round bead only", l.label)
		}
	}

	*s = sageStubs{}
	if err := cmdSageReject("spi-p", "fix this"); err != nil {
		t.Fatalf("cmdSageReject: %v", err)
	}
	if s.closeReviewID == "spi-p" {
		t.Error("reject verdict write targeted parent bead; must target review-round bead")
	}
	if s.closeReviewVerdict != "request_changes" {
		t.Errorf("canonical verdict = %q, want request_changes", s.closeReviewVerdict)
	}
}
