package cleric

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// fakeStore is an in-memory test seam for the cleric handlers. Each
// field is a per-bead map mirroring the pkg/store API the handlers
// touch. Tests construct a fake, populate beads, run a handler, and
// then assert directly on the fake's state.
type fakeStore struct {
	beadsByID  map[string]store.Bead
	depsByID   map[string][]*beads.IssueWithDependencyMetadata
	depsRev    map[string][]*beads.IssueWithDependencyMetadata
	comments   map[string][]string
	gateway    GatewayClient
	clockNow   time.Time
	addLabelFn func(id, label string) error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		beadsByID: map[string]store.Bead{},
		depsByID:  map[string][]*beads.IssueWithDependencyMetadata{},
		depsRev:   map[string][]*beads.IssueWithDependencyMetadata{},
		comments:  map[string][]string{},
		clockNow:  time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC),
	}
}

func (fs *fakeStore) toDeps() Deps {
	d := Deps{
		GetBead: func(id string) (store.Bead, error) {
			return fs.beadsByID[id], nil
		},
		SetBeadMetadata: func(id string, m map[string]string) error {
			b := fs.beadsByID[id]
			if b.Metadata == nil {
				b.Metadata = map[string]string{}
			}
			for k, v := range m {
				b.Metadata[k] = v
			}
			fs.beadsByID[id] = b
			return nil
		},
		UpdateBead: func(id string, updates map[string]interface{}) error {
			b := fs.beadsByID[id]
			for k, v := range updates {
				if k == "status" {
					if s, ok := v.(string); ok {
						b.Status = s
					}
				}
			}
			fs.beadsByID[id] = b
			return nil
		},
		AddLabel: func(id, label string) error {
			if fs.addLabelFn != nil {
				return fs.addLabelFn(id, label)
			}
			b := fs.beadsByID[id]
			b.Labels = append(b.Labels, label)
			fs.beadsByID[id] = b
			return nil
		},
		AddComment: func(id, text string) error {
			fs.comments[id] = append(fs.comments[id], text)
			return nil
		},
		CloseBead: func(id string) error {
			b := fs.beadsByID[id]
			b.Status = StatusClosed
			fs.beadsByID[id] = b
			return nil
		},
		GetDepsWithMeta: func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
			return fs.depsByID[id], nil
		},
		Gateway: fs.gateway,
		Now:     func() time.Time { return fs.clockNow },
	}
	return d
}

// linkCausedBy adds a caused-by edge from rec → src and the reverse for
// dependents-style queries.
func (fs *fakeStore) linkCausedBy(rec, src string) {
	srcBead, ok := fs.beadsByID[src]
	if !ok {
		srcBead = store.Bead{ID: src}
	}
	fs.depsByID[rec] = append(fs.depsByID[rec], &beads.IssueWithDependencyMetadata{
		Issue: beads.Issue{
			ID:        src,
			Title:     srcBead.Title,
			IssueType: beads.IssueType(srcBead.Type),
			Status:    beads.Status(srcBead.Status),
		},
		DependencyType: beads.DependencyType(store.DepCausedBy),
	})
	fs.depsRev[src] = append(fs.depsRev[src], &beads.IssueWithDependencyMetadata{
		Issue: beads.Issue{
			ID:        rec,
			IssueType: beads.IssueType("recovery"),
			Status:    beads.Status(fs.beadsByID[rec].Status),
		},
		DependencyType: beads.DependencyType(store.DepCausedBy),
	})
}

// ---

func TestPublish_PersistsProposalAndTransitions(t *testing.T) {
	fs := newFakeStore()
	fs.beadsByID["spi-rec"] = store.Bead{ID: "spi-rec", Status: StatusInProgress}
	fs.beadsByID["spi-src"] = store.Bead{ID: "spi-src", Status: "hooked"}
	fs.linkCausedBy("spi-rec", "spi-src")

	stdout := `{"verb":"resummon","reasoning":"transient build flake","failure_class":"build-error"}`
	res := Publish("spi-rec", stdout, fs.toDeps())
	if res.Err != nil {
		t.Fatalf("publish err: %v", res.Err)
	}
	if got := res.Outputs["status"]; got != "awaiting_review" {
		t.Errorf("outputs.status = %q, want awaiting_review", got)
	}
	if rec := fs.beadsByID["spi-rec"]; rec.Status != StatusAwaitingReview {
		t.Errorf("recovery status = %q, want %q", rec.Status, StatusAwaitingReview)
	}
	if rec := fs.beadsByID["spi-rec"]; rec.Metadata[MetadataKeyProposal] == "" {
		t.Error("proposal metadata not persisted")
	}
	if comments := fs.comments["spi-rec"]; len(comments) == 0 {
		t.Error("no audit comment on recovery")
	}
}

func TestPublish_ParseFailureReturnsError(t *testing.T) {
	fs := newFakeStore()
	fs.beadsByID["spi-rec"] = store.Bead{ID: "spi-rec"}
	res := Publish("spi-rec", "not json", fs.toDeps())
	if res.Err == nil {
		t.Fatal("expected error for unparseable stdout")
	}
	if rec := fs.beadsByID["spi-rec"]; rec.Status == StatusAwaitingReview {
		t.Error("recovery should not transition on parse failure")
	}
}

// ---

type fakeGateway struct {
	called  []ExecuteRequest
	result  ExecuteResult
	err     error
}

func (f *fakeGateway) Execute(_ context.Context, req ExecuteRequest) (ExecuteResult, error) {
	f.called = append(f.called, req)
	return f.result, f.err
}

func TestExecute_CallsGatewayAndRecordsOutcome(t *testing.T) {
	fs := newFakeStore()
	gw := &fakeGateway{result: ExecuteResult{Success: true, Message: "applied"}}
	fs.gateway = gw

	pa := ProposedAction{
		Verb:         "resummon",
		Reasoning:    "transient",
		FailureClass: "build-error",
	}
	enc, _ := pa.Marshal()
	fs.beadsByID["spi-rec"] = store.Bead{ID: "spi-rec", Metadata: map[string]string{
		MetadataKeyProposal: string(enc),
	}}
	fs.beadsByID["spi-src"] = store.Bead{ID: "spi-src"}
	fs.linkCausedBy("spi-rec", "spi-src")

	res := Execute("spi-rec", fs.toDeps())
	if res.Err != nil {
		t.Fatalf("execute err: %v", res.Err)
	}
	if len(gw.called) != 1 {
		t.Fatalf("gateway called %d times, want 1", len(gw.called))
	}
	if gw.called[0].SourceBeadID != "spi-src" {
		t.Errorf("source bead = %q, want spi-src", gw.called[0].SourceBeadID)
	}
	if got := res.Outputs["execute_success"]; got != "true" {
		t.Errorf("execute_success = %q, want true", got)
	}
	if rec := fs.beadsByID["spi-rec"]; rec.Metadata[MetadataKeyExecuteResult] == "" {
		t.Error("execute result not persisted")
	}
	// Strict success marker (spi-skfsia finding 2): execute writes
	// MetadataKeyExecuteSuccess so finish + recoveryShouldResume can
	// gate on a single, machine-checkable signal.
	if got := fs.beadsByID["spi-rec"].Metadata[MetadataKeyExecuteSuccess]; got != "true" {
		t.Errorf("MetadataKeyExecuteSuccess = %q, want \"true\" on real success", got)
	}
}

func TestExecute_GatewayUnimplementedSurfacesAsRecorded(t *testing.T) {
	fs := newFakeStore()
	gw := &fakeGateway{result: ExecuteResult{Message: "stub"}, err: ErrGatewayUnimplemented}
	fs.gateway = gw

	pa := ProposedAction{Verb: "resummon", Reasoning: "r", FailureClass: "c"}
	enc, _ := pa.Marshal()
	fs.beadsByID["spi-rec"] = store.Bead{ID: "spi-rec", Metadata: map[string]string{
		MetadataKeyProposal: string(enc),
	}}
	fs.beadsByID["spi-src"] = store.Bead{ID: "spi-src"}
	fs.linkCausedBy("spi-rec", "spi-src")

	res := Execute("spi-rec", fs.toDeps())
	if res.Err != nil {
		t.Fatalf("execute err: %v", res.Err)
	}
	if got := res.Outputs["execute_success"]; got != "false" {
		t.Errorf("execute_success = %q, want false", got)
	}
	rec := fs.beadsByID["spi-rec"]
	if got := rec.Metadata[MetadataKeyExecuteResult]; !strings.HasPrefix(got, "unimplemented:") {
		t.Errorf("execute_result = %q; want prefix unimplemented:", got)
	}
	// Strict success marker (spi-skfsia finding 2): execute MUST write
	// "false" on unimplemented so finish + recoveryShouldResume reject
	// the resume-success path.
	if got := rec.Metadata[MetadataKeyExecuteSuccess]; got != "false" {
		t.Errorf("MetadataKeyExecuteSuccess = %q, want \"false\" on unimplemented", got)
	}
}

// ---

func TestTakeover_LabelsSourceAndClosesRecovery(t *testing.T) {
	fs := newFakeStore()
	fs.beadsByID["spi-rec"] = store.Bead{ID: "spi-rec", Status: StatusAwaitingReview}
	fs.beadsByID["spi-src"] = store.Bead{ID: "spi-src", Status: "hooked"}
	fs.linkCausedBy("spi-rec", "spi-src")

	res := Takeover("spi-rec", fs.toDeps())
	if res.Err != nil {
		t.Fatalf("takeover err: %v", res.Err)
	}
	src := fs.beadsByID["spi-src"]
	if !containsString(src.Labels, LabelNeedsManual) {
		t.Errorf("source labels = %v, want %s", src.Labels, LabelNeedsManual)
	}
	if src.Status != "hooked" {
		t.Errorf("source status changed to %q; takeover must NOT touch source status", src.Status)
	}
	if rec := fs.beadsByID["spi-rec"]; rec.Status != StatusClosed {
		t.Errorf("recovery status = %q, want %q", rec.Status, StatusClosed)
	}
}

func TestFinish_PersistsOutcomeAndCloses(t *testing.T) {
	fs := newFakeStore()
	fs.beadsByID["spi-rec"] = store.Bead{
		ID: "spi-rec",
		Metadata: map[string]string{
			"cleric_proposal_verb":          "resummon",
			"cleric_proposal_failure_class": "build-error",
			MetadataKeyExecuteResult:        "applied",
			// Strict success marker (spi-skfsia finding 2):
			// finish only stamps approve+executed when execute
			// recorded a real success.
			MetadataKeyExecuteSuccess: "true",
		},
	}
	res := Finish("spi-rec", fs.toDeps())
	if res.Err != nil {
		t.Fatalf("finish err: %v", res.Err)
	}
	rec := fs.beadsByID["spi-rec"]
	if rec.Status != StatusClosed {
		t.Errorf("recovery status = %q, want closed", rec.Status)
	}
	if got := rec.Metadata[MetadataKeyOutcome]; got != "approve+executed" {
		t.Errorf("outcome = %q, want approve+executed", got)
	}
}

// TestFinish_RefusesApproveExecutedOnFailedExecution pins the strict
// success contract (spi-skfsia finding 2): cleric.execute reporting
// gateway-unimplemented or gateway-error must NOT result in
// cleric_outcome=approve+executed. The recovery still closes (humans
// audit failures from the bead's metadata) but the outcome is
// approve+failed so steward.recoveryShouldResume refuses to re-summon.
func TestFinish_RefusesApproveExecutedOnFailedExecution(t *testing.T) {
	fs := newFakeStore()
	fs.beadsByID["spi-rec"] = store.Bead{
		ID: "spi-rec",
		Metadata: map[string]string{
			"cleric_proposal_verb":          "resummon",
			"cleric_proposal_failure_class": "build-error",
			MetadataKeyExecuteResult:        "unimplemented: stub gateway",
			MetadataKeyExecuteSuccess:       "false",
		},
	}
	res := Finish("spi-rec", fs.toDeps())
	if res.Err != nil {
		t.Fatalf("finish err: %v", res.Err)
	}
	rec := fs.beadsByID["spi-rec"]
	if rec.Status != StatusClosed {
		t.Errorf("recovery status = %q, want closed (finish always closes)", rec.Status)
	}
	if got := rec.Metadata[MetadataKeyOutcome]; got != "approve+failed" {
		t.Errorf("outcome = %q, want approve+failed (execute did not succeed)", got)
	}
}

// TestFinish_RefusesApproveExecutedWithoutMarker covers the missing-
// marker case: an old recovery bead that never had MetadataKeyExecuteSuccess
// written (e.g. a legacy code path or partially-migrated state) must
// also be treated as not-successful — the absence of the strict marker
// is read as "execute did not succeed" rather than as "default to true".
func TestFinish_RefusesApproveExecutedWithoutMarker(t *testing.T) {
	fs := newFakeStore()
	fs.beadsByID["spi-rec"] = store.Bead{
		ID: "spi-rec",
		Metadata: map[string]string{
			"cleric_proposal_verb":          "resummon",
			"cleric_proposal_failure_class": "build-error",
			MetadataKeyExecuteResult:        "applied",
			// No MetadataKeyExecuteSuccess.
		},
	}
	res := Finish("spi-rec", fs.toDeps())
	if res.Err != nil {
		t.Fatalf("finish err: %v", res.Err)
	}
	rec := fs.beadsByID["spi-rec"]
	if got := rec.Metadata[MetadataKeyOutcome]; got != "approve+failed" {
		t.Errorf("outcome = %q, want approve+failed (no success marker)", got)
	}
}

// ---

func TestHasOpenRecovery_DetectsOpenRecovery(t *testing.T) {
	fs := newFakeStore()
	fs.beadsByID["spi-rec"] = store.Bead{ID: "spi-rec", Type: "recovery", Status: StatusInProgress}
	fs.beadsByID["spi-src"] = store.Bead{ID: "spi-src"}
	fs.linkCausedBy("spi-rec", "spi-src")

	has, recoveryID, err := HasOpenRecovery("spi-src", func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return fs.depsRev[id], nil
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !has || recoveryID != "spi-rec" {
		t.Errorf("has=%v id=%q; want has=true id=spi-rec", has, recoveryID)
	}
}

func TestHasOpenRecovery_IgnoresClosedRecovery(t *testing.T) {
	fs := newFakeStore()
	fs.beadsByID["spi-rec"] = store.Bead{ID: "spi-rec", Type: "recovery", Status: StatusClosed}
	fs.beadsByID["spi-src"] = store.Bead{ID: "spi-src"}
	fs.linkCausedBy("spi-rec", "spi-src")
	// Manually set the dependent's status so the recovery looks closed.
	fs.depsRev["spi-src"][0].Status = beads.Status(StatusClosed)

	has, _, err := HasOpenRecovery("spi-src", func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return fs.depsRev[id], nil
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if has {
		t.Error("should not flag closed recovery as open")
	}
}

func TestHasOpenRecovery_IgnoresNonRecoveryCausedBy(t *testing.T) {
	// A bug bead linked via caused-by should NOT block summon.
	fs := newFakeStore()
	fs.beadsByID["spi-bug"] = store.Bead{ID: "spi-bug", Type: "bug", Status: StatusInProgress}
	fs.beadsByID["spi-src"] = store.Bead{ID: "spi-src"}
	fs.depsRev["spi-src"] = []*beads.IssueWithDependencyMetadata{
		{
			Issue: beads.Issue{
				ID:        "spi-bug",
				IssueType: beads.IssueType("bug"),
				Status:    beads.Status(StatusInProgress),
			},
			DependencyType: beads.DependencyType(store.DepCausedBy),
		},
	}
	has, _, err := HasOpenRecovery("spi-src", func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return fs.depsRev[id], nil
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if has {
		t.Error("non-recovery caused-by edges should not block summon")
	}
}

// --- Auto-approve fast-path (spi-kl8x5y) ---

// stubLearningStore returns the configured promoted/demoted answers for
// any (failure_class, action) lookup. Tests inject this into Deps.Learning
// to exercise the auto-approve fast-path without touching SQL.
type stubLearningStore struct {
	promoted bool
	demoted  []store.DemotedClericPair
}

func (s stubLearningStore) LastNFinalizedOutcomes(_, _ string, _ int) ([]store.ClericOutcome, error) {
	if !s.promoted {
		return nil, nil
	}
	tru := true
	row := store.ClericOutcome{
		Gate:                    "approve",
		WizardPostActionSuccess: sql.NullBool{Bool: tru, Valid: true},
		Finalized:               true,
	}
	return []store.ClericOutcome{row, row, row}, nil
}

func (s stubLearningStore) ListDemotedPairs(_ int) ([]store.DemotedClericPair, error) {
	return s.demoted, nil
}

func TestPublish_AutoApprovedSkipsAwaitingReview(t *testing.T) {
	fs := newFakeStore()
	fs.beadsByID["spi-rec"] = store.Bead{ID: "spi-rec", Status: StatusInProgress}
	fs.beadsByID["spi-src"] = store.Bead{ID: "spi-src", Status: "hooked"}
	fs.linkCausedBy("spi-rec", "spi-src")

	deps := fs.toDeps()
	deps.Learning = stubLearningStore{promoted: true}

	stdout := `{"verb":"resummon","reasoning":"transient","failure_class":"build-error"}`
	res := Publish("spi-rec", stdout, deps)
	if res.Err != nil {
		t.Fatalf("publish err: %v", res.Err)
	}
	if got := res.Outputs["status"]; got != "auto_approved" {
		t.Errorf("status = %q, want auto_approved", got)
	}
	if got := res.Outputs["auto_approved"]; got != "true" {
		t.Errorf("auto_approved = %q, want true", got)
	}
	if got := res.Outputs["gate"]; got != GateApprove {
		t.Errorf("gate = %q, want %q", got, GateApprove)
	}
	if got := res.Outputs["rejection_comment"]; got == "" {
		t.Error("rejection_comment must be non-empty so wait_for_gate's produces clears")
	}
	rec := fs.beadsByID["spi-rec"]
	if rec.Status == StatusAwaitingReview {
		t.Error("auto-approved recovery bead must NOT enter awaiting_review")
	}
	if !containsString(rec.Labels, LabelAutoApproved) {
		t.Errorf("recovery labels = %v, want %s", rec.Labels, LabelAutoApproved)
	}
}

func TestPublish_NotPromotedFollowsNormalPath(t *testing.T) {
	fs := newFakeStore()
	fs.beadsByID["spi-rec"] = store.Bead{ID: "spi-rec", Status: StatusInProgress}
	fs.beadsByID["spi-src"] = store.Bead{ID: "spi-src", Status: "hooked"}
	fs.linkCausedBy("spi-rec", "spi-src")

	deps := fs.toDeps()
	deps.Learning = stubLearningStore{promoted: false}

	stdout := `{"verb":"resummon","reasoning":"transient","failure_class":"build-error"}`
	res := Publish("spi-rec", stdout, deps)
	if res.Err != nil {
		t.Fatalf("publish err: %v", res.Err)
	}
	if got := res.Outputs["status"]; got != "awaiting_review" {
		t.Errorf("status = %q, want awaiting_review (not promoted)", got)
	}
	if got := res.Outputs["auto_approved"]; got != "" {
		t.Errorf("auto_approved = %q, want empty on normal path", got)
	}
	rec := fs.beadsByID["spi-rec"]
	if rec.Status != StatusAwaitingReview {
		t.Errorf("recovery status = %q, want %s", rec.Status, StatusAwaitingReview)
	}
	if containsString(rec.Labels, LabelAutoApproved) {
		t.Errorf("normal-path recovery should not carry %s label", LabelAutoApproved)
	}
}

// --- Outcome recording (spi-kl8x5y) ---

func TestFinish_RecordsPendingOutcome(t *testing.T) {
	fs := newFakeStore()
	fs.beadsByID["spi-rec"] = store.Bead{
		ID: "spi-rec",
		Metadata: map[string]string{
			"cleric_proposal_verb":          "resummon",
			"cleric_proposal_failure_class": "step-failure:implement",
			MetadataKeyExecuteResult:        "applied",
			MetadataKeyExecuteSuccess:       "true",
			MetadataKeyProposal:             `{"verb":"resummon","reasoning":"r","failure_class":"step-failure:implement"}`,
			"source_step":                   "implement",
		},
	}
	fs.beadsByID["spi-src"] = store.Bead{ID: "spi-src"}
	fs.linkCausedBy("spi-rec", "spi-src")

	var recorded []store.ClericOutcome
	deps := fs.toDeps()
	deps.RecordOutcome = func(o store.ClericOutcome) error {
		recorded = append(recorded, o)
		return nil
	}

	res := Finish("spi-rec", deps)
	if res.Err != nil {
		t.Fatalf("finish err: %v", res.Err)
	}
	if len(recorded) != 1 {
		t.Fatalf("recorded outcomes = %d, want 1", len(recorded))
	}
	got := recorded[0]
	if got.RecoveryBeadID != "spi-rec" {
		t.Errorf("recovery_bead_id = %q, want spi-rec", got.RecoveryBeadID)
	}
	if got.SourceBeadID != "spi-src" {
		t.Errorf("source_bead_id = %q, want spi-src", got.SourceBeadID)
	}
	if got.FailureClass != "step-failure:implement" {
		t.Errorf("failure_class = %q", got.FailureClass)
	}
	if got.Action != "resummon" {
		t.Errorf("action = %q, want resummon", got.Action)
	}
	if got.Gate != GateApprove {
		t.Errorf("gate = %q, want approve", got.Gate)
	}
	if got.TargetStep != "implement" {
		t.Errorf("target_step = %q, want implement (from source_step metadata)", got.TargetStep)
	}
	if got.Finalized {
		t.Error("approve outcome must be Finalized=false (wizard observer fills success later)")
	}
}

func TestTakeover_RecordsFinalizedOutcome(t *testing.T) {
	fs := newFakeStore()
	fs.beadsByID["spi-rec"] = store.Bead{
		ID: "spi-rec", Status: StatusAwaitingReview,
		Metadata: map[string]string{
			"cleric_proposal_verb":          "resummon",
			"cleric_proposal_failure_class": "step-failure:implement",
		},
	}
	fs.beadsByID["spi-src"] = store.Bead{ID: "spi-src", Status: "hooked"}
	fs.linkCausedBy("spi-rec", "spi-src")

	var recorded []store.ClericOutcome
	deps := fs.toDeps()
	deps.RecordOutcome = func(o store.ClericOutcome) error {
		recorded = append(recorded, o)
		return nil
	}

	res := Takeover("spi-rec", deps)
	if res.Err != nil {
		t.Fatalf("takeover err: %v", res.Err)
	}
	if len(recorded) != 1 {
		t.Fatalf("recorded outcomes = %d, want 1", len(recorded))
	}
	got := recorded[0]
	if got.Gate != GateTakeover {
		t.Errorf("gate = %q, want takeover", got.Gate)
	}
	if !got.Finalized {
		t.Error("takeover outcome must be Finalized=true")
	}
}

func TestReject_RecordsFinalizedRejectOutcome(t *testing.T) {
	fs := newFakeStore()
	fs.beadsByID["spi-rec"] = store.Bead{
		ID: "spi-rec", Status: StatusAwaitingReview,
		Metadata: map[string]string{
			"cleric_proposal_verb":          "resummon",
			"cleric_proposal_failure_class": "step-failure:implement",
		},
	}
	fs.beadsByID["spi-src"] = store.Bead{ID: "spi-src", Status: "hooked"}
	fs.linkCausedBy("spi-rec", "spi-src")

	var recorded []store.ClericOutcome
	deps := fs.toDeps()
	deps.RecordOutcome = func(o store.ClericOutcome) error {
		recorded = append(recorded, o)
		return nil
	}

	res := Reject("spi-rec", deps)
	if res.Err != nil {
		t.Fatalf("reject err: %v", res.Err)
	}
	if len(recorded) != 1 {
		t.Fatalf("recorded outcomes = %d, want 1", len(recorded))
	}
	got := recorded[0]
	if got.Gate != GateReject {
		t.Errorf("gate = %q, want reject", got.Gate)
	}
	if !got.Finalized {
		t.Error("reject outcome must be Finalized=true")
	}
	if got.RecoveryBeadID != "spi-rec" || got.SourceBeadID != "spi-src" {
		t.Errorf("recovery=%q source=%q", got.RecoveryBeadID, got.SourceBeadID)
	}
	// Reject must NOT close the bead — the formula's resets directive
	// drives the requeue.
	if rec := fs.beadsByID["spi-rec"]; rec.Status == StatusClosed {
		t.Error("cleric.reject must not close the recovery bead — formula handles requeue")
	}
}

// helpers

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
