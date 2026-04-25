package steward

// Tests for the cluster-safe review-feedback ownership lookup added by
// spi-5bzu9r.4. The shared-state surface here is the attempt bead's
// `agent:<name>` label — produced by the wizard's ensureAttemptBead path
// and visible to every steward replica via the dolt-backed bead store.
// These tests assert the cluster-native re-entry path never reaches the
// in-process registry (`registry.List`) and that local-native still falls
// back to the registry when the shared-state surface is empty.

import (
	"errors"
	"fmt"
	"testing"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/registry"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// reviewFeedbackTestCtx bundles per-test mocks for the review-feedback
// path and restores the test-replaceable function vars on cleanup.
type reviewFeedbackTestCtx struct {
	t                  *testing.T
	parentBead         store.Bead
	reviewBead         store.Bead
	attemptBeads       []store.Bead
	registryEntries    []registry.Entry
	registryListCalled int
	sentMessages       []sentMessage
}

type sentMessage struct {
	to, from, body, ref string
	priority            int
}

func newReviewFeedbackTest(t *testing.T) *reviewFeedbackTestCtx {
	t.Helper()
	ctx := &reviewFeedbackTestCtx{t: t}

	origList := ListBeadsFunc
	origGetChildren := GetChildrenFunc
	origGetActiveAttempt := GetActiveAttemptFunc
	origRegistryList := reviewRegistryListFunc
	origSend := SendMessageFunc

	t.Cleanup(func() {
		ListBeadsFunc = origList
		GetChildrenFunc = origGetChildren
		GetActiveAttemptFunc = origGetActiveAttempt
		reviewRegistryListFunc = origRegistryList
		SendMessageFunc = origSend
	})

	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		return []store.Bead{ctx.parentBead}, nil
	}
	GetChildrenFunc = func(parentID string) ([]store.Bead, error) {
		if parentID != ctx.parentBead.ID {
			return nil, nil
		}
		out := make([]store.Bead, 0, len(ctx.attemptBeads)+1)
		out = append(out, ctx.attemptBeads...)
		out = append(out, ctx.reviewBead)
		return out, nil
	}
	GetActiveAttemptFunc = func(parentID string) (*store.Bead, error) {
		// Review-feedback re-entry only fires when no active attempt exists.
		// Tests that need an active attempt set this directly.
		return nil, nil
	}
	reviewRegistryListFunc = func() ([]registry.Entry, error) {
		ctx.registryListCalled++
		return ctx.registryEntries, nil
	}
	SendMessageFunc = func(to, from, body, ref string, priority int) (string, error) {
		ctx.sentMessages = append(ctx.sentMessages, sentMessage{
			to: to, from: from, body: body, ref: ref, priority: priority,
		})
		return fmt.Sprintf("msg-%d", len(ctx.sentMessages)), nil
	}

	return ctx
}

// requestChangesParent builds an in_progress bead whose latest review-round
// child carries verdict=request_changes — the shape DetectReviewFeedback
// gates on.
func (ctx *reviewFeedbackTestCtx) requestChangesParent(beadID string) {
	ctx.parentBead = store.Bead{
		ID:       beadID,
		Title:    "task " + beadID,
		Status:   "in_progress",
		Type:     "task",
		Priority: 2,
	}
	ctx.reviewBead = store.Bead{
		ID:     beadID + ".review-1",
		Status: "closed",
		Labels: []string{"review-round", "round:1"},
		Metadata: map[string]string{
			"review_verdict": "request_changes",
		},
	}
}

func (ctx *reviewFeedbackTestCtx) addAttempt(attemptID, agentName string, n int) {
	ctx.attemptBeads = append(ctx.attemptBeads, store.Bead{
		ID:     attemptID,
		Status: "closed",
		Labels: []string{"attempt", fmt.Sprintf("attempt:%d", n), "agent:" + agentName},
	})
}

// --- lookupReviewOwner tests ---

func TestLookupReviewOwner_PicksLatestAttempt(t *testing.T) {
	ctx := newReviewFeedbackTest(t)
	ctx.requestChangesParent("spi-rfb-1")
	ctx.addAttempt("spi-rfb-1.attempt-1", "wizard-rfb-old", 1)
	ctx.addAttempt("spi-rfb-1.attempt-2", "wizard-rfb-new", 2)

	got, err := lookupReviewOwner("spi-rfb-1")
	if err != nil {
		t.Fatalf("lookupReviewOwner returned error: %v", err)
	}
	if got.AgentID != "wizard-rfb-new" {
		t.Errorf("AgentID = %q, want wizard-rfb-new (highest attempt:N wins)", got.AgentID)
	}
	if got.AttemptID != "spi-rfb-1.attempt-2" {
		t.Errorf("AttemptID = %q, want spi-rfb-1.attempt-2", got.AttemptID)
	}
}

func TestLookupReviewOwner_NoAttempts(t *testing.T) {
	ctx := newReviewFeedbackTest(t)
	ctx.requestChangesParent("spi-rfb-empty")

	got, err := lookupReviewOwner("spi-rfb-empty")
	if err != nil {
		t.Fatalf("lookupReviewOwner returned error: %v", err)
	}
	if !got.IsZero() {
		t.Errorf("ReviewOwnerRef = %+v, want zero (no attempt children)", got)
	}
}

func TestLookupReviewOwner_GetChildrenError(t *testing.T) {
	ctx := newReviewFeedbackTest(t)
	ctx.requestChangesParent("spi-rfb-err")
	GetChildrenFunc = func(parentID string) ([]store.Bead, error) {
		return nil, errors.New("dolt offline")
	}

	_, err := lookupReviewOwner("spi-rfb-err")
	if err == nil {
		t.Fatal("expected error when GetChildren fails")
	}
}

// --- DetectReviewFeedback cluster-mode tests ---

// TestDetectReviewFeedback_ClusterMode_UsesAttemptOwner is the headline
// regression test for the cluster-native ownership boundary. It seeds a
// bead whose only ownership signal is the attempt bead's `agent:` label
// (no registry entries) and asserts the steward routes the re-engagement
// message to that agent without ever calling registry.List.
func TestDetectReviewFeedback_ClusterMode_UsesAttemptOwner(t *testing.T) {
	ctx := newReviewFeedbackTest(t)
	ctx.requestChangesParent("spi-cluster-rfb")
	ctx.addAttempt("spi-cluster-rfb.attempt-1", "wizard-cluster-rfb", 1)
	// Deliberately leave registryEntries empty; cluster replicas don't
	// write the local wizards.json.

	DetectReviewFeedback(false, config.DeploymentModeClusterNative)

	if ctx.registryListCalled != 0 {
		t.Errorf("registry.List called %d time(s) on cluster-native path; want 0", ctx.registryListCalled)
	}
	if len(ctx.sentMessages) != 1 {
		t.Fatalf("sent %d message(s), want 1", len(ctx.sentMessages))
	}
	got := ctx.sentMessages[0]
	if got.to != "wizard-cluster-rfb" {
		t.Errorf("re-engagement message to %q, want wizard-cluster-rfb (sourced from attempt bead)", got.to)
	}
	if got.ref != "spi-cluster-rfb" {
		t.Errorf("message ref = %q, want spi-cluster-rfb", got.ref)
	}
}

// TestDetectReviewFeedback_ClusterMode_NoAttemptFailsClosed asserts the
// cluster path skips re-engagement (rather than falling through to the
// registry or routing to a fabricated owner) when no attempt bead exists.
func TestDetectReviewFeedback_ClusterMode_NoAttemptFailsClosed(t *testing.T) {
	ctx := newReviewFeedbackTest(t)
	ctx.requestChangesParent("spi-cluster-noattempt")
	ctx.registryEntries = []registry.Entry{
		{Name: "wizard-stale", BeadID: "spi-cluster-noattempt"},
	}

	DetectReviewFeedback(false, config.DeploymentModeClusterNative)

	if ctx.registryListCalled != 0 {
		t.Errorf("registry.List called %d time(s) on cluster-native path; want 0 even when attempt is missing", ctx.registryListCalled)
	}
	if len(ctx.sentMessages) != 0 {
		t.Errorf("sent %d message(s), want 0 (cluster-native must fail closed without owner)", len(ctx.sentMessages))
	}
}

// --- DetectReviewFeedback local-mode tests ---

// TestDetectReviewFeedback_LocalMode_PrefersAttemptOwner verifies the
// shared-state surface still wins on local-native; the registry is the
// fallback path, not the primary.
func TestDetectReviewFeedback_LocalMode_PrefersAttemptOwner(t *testing.T) {
	ctx := newReviewFeedbackTest(t)
	ctx.requestChangesParent("spi-local-rfb")
	ctx.addAttempt("spi-local-rfb.attempt-1", "wizard-from-attempt", 1)
	ctx.registryEntries = []registry.Entry{
		{Name: "wizard-from-registry", BeadID: "spi-local-rfb"},
	}

	DetectReviewFeedback(false, config.DeploymentModeLocalNative)

	if len(ctx.sentMessages) != 1 {
		t.Fatalf("sent %d message(s), want 1", len(ctx.sentMessages))
	}
	if got := ctx.sentMessages[0].to; got != "wizard-from-attempt" {
		t.Errorf("owner = %q, want wizard-from-attempt (shared state wins on local-native too)", got)
	}
	if ctx.registryListCalled != 0 {
		t.Errorf("registry.List called %d time(s); want 0 when attempt-bead lookup succeeds", ctx.registryListCalled)
	}
}

// TestDetectReviewFeedback_LocalMode_FallsBackToRegistry verifies the
// existing local-native fallback still works when no attempt bead is
// present (e.g., legacy beads from before the attempt-bead migration).
func TestDetectReviewFeedback_LocalMode_FallsBackToRegistry(t *testing.T) {
	ctx := newReviewFeedbackTest(t)
	ctx.requestChangesParent("spi-local-fallback")
	ctx.registryEntries = []registry.Entry{
		{Name: "wizard-from-registry", BeadID: "spi-local-fallback"},
	}

	DetectReviewFeedback(false, config.DeploymentModeLocalNative)

	if ctx.registryListCalled == 0 {
		t.Error("expected registry.List to be called on local-native fallback path")
	}
	if len(ctx.sentMessages) != 1 {
		t.Fatalf("sent %d message(s), want 1", len(ctx.sentMessages))
	}
	if got := ctx.sentMessages[0].to; got != "wizard-from-registry" {
		t.Errorf("owner = %q, want wizard-from-registry (registry fallback)", got)
	}
}

// TestDetectReviewFeedback_LocalMode_GenericOwnerWhenAllEmpty preserves
// the historical behavior of routing to a synthetic "wizard" target when
// no surface yields an owner. This is the legacy fast-path the old code
// produced when both the attempt and registry came up empty.
func TestDetectReviewFeedback_LocalMode_GenericOwnerWhenAllEmpty(t *testing.T) {
	ctx := newReviewFeedbackTest(t)
	ctx.requestChangesParent("spi-local-empty")

	DetectReviewFeedback(false, config.DeploymentModeLocalNative)

	if len(ctx.sentMessages) != 1 {
		t.Fatalf("sent %d message(s), want 1 (local-native preserves generic-owner fallback)", len(ctx.sentMessages))
	}
	if got := ctx.sentMessages[0].to; got != "wizard" {
		t.Errorf("owner = %q, want generic %q", got, "wizard")
	}
}

// TestDetectReviewFeedback_DryRunSuppressesSend covers the dry-run guard
// against a regression where the new lookup ordering accidentally
// re-emitted in dry-run.
func TestDetectReviewFeedback_DryRunSuppressesSend(t *testing.T) {
	ctx := newReviewFeedbackTest(t)
	ctx.requestChangesParent("spi-dryrun")
	ctx.addAttempt("spi-dryrun.attempt-1", "wizard-dryrun", 1)

	DetectReviewFeedback(true, config.DeploymentModeClusterNative)

	if len(ctx.sentMessages) != 0 {
		t.Errorf("dry-run sent %d message(s), want 0", len(ctx.sentMessages))
	}
}
