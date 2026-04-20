package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// stubSageDeps swaps the sage.go test seams for in-memory fakes and returns
// a cleanup func that restores the originals. Each test that exercises the
// store path should defer the returned cleanup so swaps don't leak across
// tests in this package.
func stubSageDeps(t *testing.T) func() {
	t.Helper()
	origGetChildren := sageGetChildrenFunc
	origSetMeta := sageSetMetadataMapFunc
	origCloseAttempt := sageCloseAttemptFunc
	return func() {
		sageGetChildrenFunc = origGetChildren
		sageSetMetadataMapFunc = origSetMeta
		sageCloseAttemptFunc = origCloseAttempt
	}
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

// TestSageAccept_HappyPath verifies the verdict=accept path writes the
// right metadata and closes the current review-round child.
func TestSageAccept_HappyPath(t *testing.T) {
	cleanup := stubSageDeps(t)
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

	var setMetaID string
	var setMetaMap map[string]string
	sageSetMetadataMapFunc = func(id string, m map[string]string) error {
		setMetaID = id
		setMetaMap = m
		return nil
	}

	var closedID, closedResult string
	sageCloseAttemptFunc = func(id, result string) error {
		closedID = id
		closedResult = result
		return nil
	}

	if err := cmdSageAccept("spi-p", ""); err != nil {
		t.Fatalf("cmdSageAccept: %v", err)
	}
	if setMetaID != "spi-p" {
		t.Errorf("setMetaID = %q, want spi-p", setMetaID)
	}
	if setMetaMap["review_verdict"] != "accept" {
		t.Errorf("review_verdict = %q, want accept", setMetaMap["review_verdict"])
	}
	if _, ok := setMetaMap["review_comment"]; ok {
		t.Errorf("review_comment should not be set when no comment given; got %q", setMetaMap["review_comment"])
	}
	if closedID != "spi-p.1" {
		t.Errorf("closed attempt = %q, want spi-p.1", closedID)
	}
	if closedResult != "accept" {
		t.Errorf("close result = %q, want accept", closedResult)
	}
}

// TestSageAccept_WithComment verifies the optional comment lands in
// review_comment metadata alongside the verdict.
func TestSageAccept_WithComment(t *testing.T) {
	cleanup := stubSageDeps(t)
	defer cleanup()

	sageGetChildrenFunc = func(parentID string) ([]Bead, error) {
		return []Bead{
			{
				ID:     "spi-p.1",
				Title:  "review-round-1",
				Status: "in_progress",
				Labels: []string{"review-round"},
				Parent: parentID,
			},
		}, nil
	}
	var metaMap map[string]string
	sageSetMetadataMapFunc = func(id string, m map[string]string) error {
		metaMap = m
		return nil
	}
	sageCloseAttemptFunc = func(id, result string) error { return nil }

	if err := cmdSageAccept("spi-p", "LGTM, nice work"); err != nil {
		t.Fatalf("cmdSageAccept: %v", err)
	}
	if metaMap["review_verdict"] != "accept" {
		t.Errorf("verdict = %q, want accept", metaMap["review_verdict"])
	}
	if metaMap["review_comment"] != "LGTM, nice work" {
		t.Errorf("review_comment = %q, want 'LGTM, nice work'", metaMap["review_comment"])
	}
}

// TestSageReject_HappyPath verifies verdict=reject + feedback metadata and
// that the review-round child is closed with a reject result.
func TestSageReject_HappyPath(t *testing.T) {
	cleanup := stubSageDeps(t)
	defer cleanup()

	sageGetChildrenFunc = func(parentID string) ([]Bead, error) {
		return []Bead{
			{
				ID:     "spi-p.1",
				Title:  "review-round-1",
				Status: "in_progress",
				Labels: []string{"review-round"},
				Parent: parentID,
			},
		}, nil
	}

	var metaMap map[string]string
	sageSetMetadataMapFunc = func(id string, m map[string]string) error {
		metaMap = m
		return nil
	}

	var closedID, closedResult string
	sageCloseAttemptFunc = func(id, result string) error {
		closedID = id
		closedResult = result
		return nil
	}

	if err := cmdSageReject("spi-p", "missing error handling in foo.go"); err != nil {
		t.Fatalf("cmdSageReject: %v", err)
	}
	if metaMap["review_verdict"] != "reject" {
		t.Errorf("verdict = %q, want reject", metaMap["review_verdict"])
	}
	if metaMap["review_feedback"] != "missing error handling in foo.go" {
		t.Errorf("feedback = %q, want 'missing error handling in foo.go'", metaMap["review_feedback"])
	}
	if closedID != "spi-p.1" {
		t.Errorf("closed = %q, want spi-p.1", closedID)
	}
	if closedResult != "reject" {
		t.Errorf("close result = %q, want reject", closedResult)
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

// TestSageReject_NoOpenReview verifies the command errors when the task bead
// has no open review-round child. Closed rounds and non-review children
// must not satisfy the check.
func TestSageReject_NoOpenReview(t *testing.T) {
	cleanup := stubSageDeps(t)
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
	sageSetMetadataMapFunc = func(id string, m map[string]string) error {
		t.Fatalf("setMetadata should not be called when no open review round; got id=%q", id)
		return nil
	}
	sageCloseAttemptFunc = func(id, result string) error {
		t.Fatalf("closeAttempt should not be called when no open review round; got id=%q", id)
		return nil
	}

	err := cmdSageReject("spi-p", "whatever")
	if err == nil {
		t.Fatal("expected error when bead has no open review-round child")
	}
	if !strings.Contains(err.Error(), "no open review-round") {
		t.Errorf("error %q should mention 'no open review-round'", err.Error())
	}
}
