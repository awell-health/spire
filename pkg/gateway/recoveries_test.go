package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/cleric"
	"github.com/awell-health/spire/pkg/executor"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// recoveryStubs records every side effect the recoveries handlers can
// produce so tests assert on labels, comments, status flips, and graph
// state writes without booting a real Dolt or graph store.
type recoveryStubs struct {
	listFilters       []beads.IssueFilter
	getBeadCalls      []string
	updateBeadCalls   []map[string]interface{}
	addLabelCalls     []labelCall
	addCommentCalls   []addCommentCall
	addCommentAsCalls []addCommentAsCall
	setMetadataCalls  []setMetadataCall
	graphLoads        []string
	graphSaves        []graphSaveCall
	unhookStepBead    []string
}

type labelCall struct{ id, label string }
type setMetadataCall struct {
	id   string
	meta map[string]string
}
type graphSaveCall struct {
	agent string
	state *executor.GraphState
}

// recoveryEnv aggregates the stubbed bead universe and graph state for a
// single test. Tests configure beads (id → bead), deps, dependents, and
// graph states before invoking the handler. Mutations are reflected back
// so subsequent reads in the same test see the post-write state.
type recoveryEnv struct {
	beads      map[string]store.Bead
	deps       map[string][]*beads.IssueWithDependencyMetadata
	dependents map[string][]*beads.IssueWithDependencyMetadata
	graph      map[string]*executor.GraphState
	listError  error
	getBeadErr error
	calls      *recoveryStubs
	now        time.Time
}

func newRecoveryEnv() *recoveryEnv {
	return &recoveryEnv{
		beads:      map[string]store.Bead{},
		deps:       map[string][]*beads.IssueWithDependencyMetadata{},
		dependents: map[string][]*beads.IssueWithDependencyMetadata{},
		graph:      map[string]*executor.GraphState{},
		calls:      &recoveryStubs{},
		now:        time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC),
	}
}

func withRecoveryStubs(t *testing.T, env *recoveryEnv) {
	t.Helper()
	prevEnsure := recoveriesStoreEnsureFunc
	prevGet := recoveriesGetBeadFunc
	prevUpdate := recoveriesUpdateBeadFunc
	prevAddComment := recoveriesAddCommentFunc
	prevAddCommentAs := recoveriesAddCommentAsFunc
	prevAddLabel := recoveriesAddLabelFunc
	prevList := recoveriesListBeadsFunc
	prevDeps := recoveriesGetDepsFunc
	prevDependents := recoveriesGetDependentsFunc
	prevSetMeta := recoveriesSetMetadataFunc
	prevGraphLoad := recoveriesGraphStateLoadFunc
	prevGraphSave := recoveriesGraphStateSaveFunc
	prevNow := recoveriesNowFunc
	prevSanitize := recoveriesSanitizeAgentName
	prevUnhook := recoveriesUnhookStepBeadFunc

	recoveriesStoreEnsureFunc = func(string) error { return nil }
	recoveriesGetBeadFunc = func(id string) (store.Bead, error) {
		env.calls.getBeadCalls = append(env.calls.getBeadCalls, id)
		if env.getBeadErr != nil {
			return store.Bead{}, env.getBeadErr
		}
		b, ok := env.beads[id]
		if !ok {
			return store.Bead{}, errors.New("bead " + id + " not found")
		}
		// Return a copy so handlers can't mutate the env directly.
		copy := b
		copy.ID = id
		return copy, nil
	}
	recoveriesUpdateBeadFunc = func(id string, updates map[string]interface{}) error {
		merged := map[string]interface{}{"__id": id}
		for k, v := range updates {
			merged[k] = v
		}
		env.calls.updateBeadCalls = append(env.calls.updateBeadCalls, merged)
		// Reflect status updates onto the env so the response handler
		// re-fetches the post-transition state.
		if v, ok := updates["status"].(string); ok {
			b := env.beads[id]
			b.Status = v
			env.beads[id] = b
		}
		return nil
	}
	recoveriesAddCommentFunc = func(id, text string) (string, error) {
		env.calls.addCommentCalls = append(env.calls.addCommentCalls, addCommentCall{id: id, text: text})
		return "c-r-1", nil
	}
	recoveriesAddCommentAsFunc = func(id, author, text string) (string, error) {
		env.calls.addCommentAsCalls = append(env.calls.addCommentAsCalls, addCommentAsCall{id: id, author: author, text: text})
		return "c-r-as-1", nil
	}
	recoveriesAddLabelFunc = func(id, label string) error {
		env.calls.addLabelCalls = append(env.calls.addLabelCalls, labelCall{id: id, label: label})
		b := env.beads[id]
		b.Labels = append(b.Labels, label)
		env.beads[id] = b
		return nil
	}
	recoveriesListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		env.calls.listFilters = append(env.calls.listFilters, filter)
		if env.listError != nil {
			return nil, env.listError
		}
		var out []store.Bead
		for id, b := range env.beads {
			if filter.IssueType != nil && string(*filter.IssueType) != b.Type {
				continue
			}
			if filter.Status != nil && string(*filter.Status) != b.Status {
				continue
			}
			b.ID = id
			out = append(out, b)
		}
		return out, nil
	}
	recoveriesGetDepsFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return env.deps[id], nil
	}
	recoveriesGetDependentsFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return env.dependents[id], nil
	}
	recoveriesSetMetadataFunc = func(id string, m map[string]string) error {
		env.calls.setMetadataCalls = append(env.calls.setMetadataCalls, setMetadataCall{id: id, meta: m})
		b := env.beads[id]
		if b.Metadata == nil {
			b.Metadata = map[string]string{}
		}
		for k, v := range m {
			b.Metadata[k] = v
		}
		env.beads[id] = b
		return nil
	}
	recoveriesGraphStateLoadFunc = func(agentName string) (*executor.GraphState, error) {
		env.calls.graphLoads = append(env.calls.graphLoads, agentName)
		return env.graph[agentName], nil
	}
	recoveriesGraphStateSaveFunc = func(agentName string, gs *executor.GraphState) error {
		env.calls.graphSaves = append(env.calls.graphSaves, graphSaveCall{agent: agentName, state: gs})
		env.graph[agentName] = gs
		return nil
	}
	recoveriesNowFunc = func() time.Time { return env.now }
	recoveriesSanitizeAgentName = sanitizeAgentNameLocal
	recoveriesUnhookStepBeadFunc = func(stepID string) error {
		env.calls.unhookStepBead = append(env.calls.unhookStepBead, stepID)
		return nil
	}

	t.Cleanup(func() {
		recoveriesStoreEnsureFunc = prevEnsure
		recoveriesGetBeadFunc = prevGet
		recoveriesUpdateBeadFunc = prevUpdate
		recoveriesAddCommentFunc = prevAddComment
		recoveriesAddCommentAsFunc = prevAddCommentAs
		recoveriesAddLabelFunc = prevAddLabel
		recoveriesListBeadsFunc = prevList
		recoveriesGetDepsFunc = prevDeps
		recoveriesGetDependentsFunc = prevDependents
		recoveriesSetMetadataFunc = prevSetMeta
		recoveriesGraphStateLoadFunc = prevGraphLoad
		recoveriesGraphStateSaveFunc = prevGraphSave
		recoveriesNowFunc = prevNow
		recoveriesSanitizeAgentName = prevSanitize
		recoveriesUnhookStepBeadFunc = prevUnhook
	})
}

// sampleProposalJSON returns a minimum-valid ProposedAction JSON string
// suitable for stamping onto a recovery bead's metadata.
func sampleProposalJSON(t *testing.T, verb, failureClass string, args map[string]string) string {
	t.Helper()
	pa := cleric.ProposedAction{
		Verb:         verb,
		Args:         args,
		Reasoning:    "stub reasoning",
		FailureClass: failureClass,
	}
	b, err := pa.Marshal()
	if err != nil {
		t.Fatalf("marshal sample proposal: %v", err)
	}
	return string(b)
}

// --------------------------------------------------------------------------
// GET /api/v1/recoveries
// --------------------------------------------------------------------------

func TestHandleRecoveriesList_RejectsNonGET(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	withRecoveryStubs(t, newRecoveryEnv())
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		req := httptest.NewRequest(method, "/api/v1/recoveries", nil)
		rec := httptest.NewRecorder()
		s.handleRecoveriesList(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want 405", method, rec.Code)
		}
	}
}

// TestHandleRecoveriesList_DefaultsToAwaitingReview pins the default
// status filter — the desktop's main review surface only ever wants
// awaiting_review beads, so the handler must default to it without the
// caller specifying the param.
func TestHandleRecoveriesList_DefaultsToAwaitingReview(t *testing.T) {
	env := newRecoveryEnv()
	env.beads["spi-r1"] = store.Bead{
		Title:  "recovery 1",
		Status: "awaiting_review",
		Type:   "recovery",
	}
	env.beads["spi-r2"] = store.Bead{
		Title:  "recovery 2",
		Status: "in_progress",
		Type:   "recovery",
	}
	withRecoveryStubs(t, env)

	s := newTestServer(&fakeTrigger{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/recoveries", nil)
	rec := httptest.NewRecorder()
	s.handleRecoveriesList(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}

	var out struct {
		Recoveries          []RecoveryListItem `json:"recoveries"`
		StaleThresholdHours int                `json:"stale_threshold_hours"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Recoveries) != 1 {
		t.Fatalf("got %d recoveries, want 1 (filter should drop in_progress)", len(out.Recoveries))
	}
	if out.Recoveries[0].ID != "spi-r1" {
		t.Errorf("Recoveries[0].ID = %q, want spi-r1", out.Recoveries[0].ID)
	}
	if out.StaleThresholdHours != staleThresholdHoursDefault {
		t.Errorf("StaleThresholdHours = %d, want %d", out.StaleThresholdHours, staleThresholdHoursDefault)
	}

	// Filter must include status awaiting_review and issue type recovery.
	if len(env.calls.listFilters) != 1 {
		t.Fatalf("expected one ListBeads call, got %d", len(env.calls.listFilters))
	}
	f := env.calls.listFilters[0]
	if f.Status == nil || string(*f.Status) != "awaiting_review" {
		t.Errorf("status filter = %v, want awaiting_review", f.Status)
	}
	if f.IssueType == nil || string(*f.IssueType) != "recovery" {
		t.Errorf("type filter = %v, want recovery", f.IssueType)
	}
}

// TestHandleRecoveriesList_DecodesProposalAndStaleFlag verifies that the
// handler decodes the cleric_proposal metadata and computes stale=true
// when awaiting_review_since is older than the threshold.
func TestHandleRecoveriesList_DecodesProposalAndStaleFlag(t *testing.T) {
	env := newRecoveryEnv()
	env.now = time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	twentyFiveHoursAgo := env.now.Add(-25 * time.Hour).Format(time.RFC3339)
	env.beads["spi-r-stale"] = store.Bead{
		Title:  "stale review",
		Status: "awaiting_review",
		Type:   "recovery",
		Metadata: map[string]string{
			cleric.MetadataKeyProposal:           sampleProposalJSON(t, "resummon", "step-failure:review", nil),
			"cleric_proposal_published_at":       twentyFiveHoursAgo,
			"cleric_proposal_verb":               "resummon",
			"cleric_proposal_failure_class":      "step-failure:review",
			"failure_class":                      "step-failure",
		},
	}
	twoHoursAgo := env.now.Add(-2 * time.Hour).Format(time.RFC3339)
	env.beads["spi-r-fresh"] = store.Bead{
		Title:  "fresh review",
		Status: "awaiting_review",
		Type:   "recovery",
		Metadata: map[string]string{
			cleric.MetadataKeyProposal:           sampleProposalJSON(t, "dismiss", "compile-error", nil),
			"cleric_proposal_published_at":       twoHoursAgo,
		},
	}
	withRecoveryStubs(t, env)

	s := newTestServer(&fakeTrigger{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/recoveries", nil)
	rec := httptest.NewRecorder()
	s.handleRecoveriesList(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}

	var out struct {
		Recoveries []RecoveryListItem `json:"recoveries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Recoveries) != 2 {
		t.Fatalf("got %d items, want 2", len(out.Recoveries))
	}

	// Verify the stale and fresh items by ID.
	byID := map[string]RecoveryListItem{}
	for _, r := range out.Recoveries {
		byID[r.ID] = r
	}
	stale := byID["spi-r-stale"]
	if stale.Proposal == nil {
		t.Fatalf("stale item has nil proposal")
	}
	if stale.Proposal.Verb != "resummon" {
		t.Errorf("stale.Proposal.Verb = %q, want resummon", stale.Proposal.Verb)
	}
	if !stale.Stale {
		t.Errorf("stale flag = false, want true (25h > 24h threshold)")
	}
	if stale.AwaitingReviewSince != twentyFiveHoursAgo {
		t.Errorf("AwaitingReviewSince = %q, want %q", stale.AwaitingReviewSince, twentyFiveHoursAgo)
	}

	fresh := byID["spi-r-fresh"]
	if fresh.Stale {
		t.Errorf("fresh stale = true, want false (2h < 24h)")
	}
	if fresh.Proposal == nil || fresh.Proposal.Verb != "dismiss" {
		t.Errorf("fresh.Proposal = %+v, want verb=dismiss", fresh.Proposal)
	}
}

// TestHandleRecoveriesList_StaleThresholdQueryParam verifies the
// stale_threshold_hours query parameter overrides the default.
func TestHandleRecoveriesList_StaleThresholdQueryParam(t *testing.T) {
	env := newRecoveryEnv()
	env.now = time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	twoHoursAgo := env.now.Add(-2 * time.Hour).Format(time.RFC3339)
	env.beads["spi-r1"] = store.Bead{
		Title:  "two hours old",
		Status: "awaiting_review",
		Type:   "recovery",
		Metadata: map[string]string{
			cleric.MetadataKeyProposal:           sampleProposalJSON(t, "resummon", "step-failure:review", nil),
			"cleric_proposal_published_at":       twoHoursAgo,
		},
	}
	withRecoveryStubs(t, env)

	// With threshold=1h, the 2h-old item should be stale.
	s := newTestServer(&fakeTrigger{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/recoveries?stale_threshold_hours=1", nil)
	rec := httptest.NewRecorder()
	s.handleRecoveriesList(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var out struct {
		Recoveries          []RecoveryListItem `json:"recoveries"`
		StaleThresholdHours int                `json:"stale_threshold_hours"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.StaleThresholdHours != 1 {
		t.Errorf("StaleThresholdHours echoed = %d, want 1", out.StaleThresholdHours)
	}
	if len(out.Recoveries) != 1 || !out.Recoveries[0].Stale {
		t.Errorf("recoveries[0].Stale = %v, want true (2h > 1h threshold)", out.Recoveries[0].Stale)
	}
}

// TestHandleRecoveriesList_GraphContext verifies the source bead, graph
// neighbors, and peer recoveries appear in the response when the deps
// graph is populated.
func TestHandleRecoveriesList_GraphContext(t *testing.T) {
	env := newRecoveryEnv()
	env.beads["spi-r1"] = store.Bead{
		Title:  "current recovery",
		Status: "awaiting_review",
		Type:   "recovery",
		Metadata: map[string]string{
			cleric.MetadataKeyProposal: sampleProposalJSON(t, "resummon", "step-failure:review", nil),
		},
	}
	env.beads["spi-source"] = store.Bead{
		Title:  "the failing task",
		Status: "hooked",
		Type:   "task",
		Labels: []string{"feat-branch:foo"},
	}
	env.beads["spi-design"] = store.Bead{
		Title:  "design",
		Status: "closed",
		Type:   "design",
	}
	env.beads["spi-r-prior"] = store.Bead{
		Title:  "prior recovery",
		Status: "closed",
		Type:   "recovery",
		Metadata: map[string]string{
			"cleric_proposal_verb":          "reset --hard",
			"cleric_proposal_failure_class": "step-failure:review",
			// Rejected peers are short-circuited before cleric.finish,
			// so MetadataKeyOutcome is empty; the gate decision lives
			// on MetadataKeyGate (set by the gateway's gate handler).
			cleric.MetadataKeyGate: cleric.GateReject,
		},
	}

	// recovery → source via caused-by
	env.deps["spi-r1"] = []*beads.IssueWithDependencyMetadata{
		{
			Issue:          beads.Issue{ID: "spi-source", Title: "the failing task"},
			DependencyType: "caused-by",
		},
	}
	// source → design via discovered-from
	env.deps["spi-source"] = []*beads.IssueWithDependencyMetadata{
		{
			Issue:          beads.Issue{ID: "spi-design", Title: "design", Status: "closed", IssueType: "design"},
			DependencyType: "discovered-from",
		},
	}
	// source has two recoveries; the prior one is on dependents of source.
	env.dependents["spi-source"] = []*beads.IssueWithDependencyMetadata{
		{
			Issue:          beads.Issue{ID: "spi-r1", Title: "current recovery", IssueType: "recovery", Status: "awaiting_review"},
			DependencyType: "caused-by",
		},
		{
			Issue:          beads.Issue{ID: "spi-r-prior", Title: "prior recovery", IssueType: "recovery", Status: "closed"},
			DependencyType: "caused-by",
		},
	}
	withRecoveryStubs(t, env)

	s := newTestServer(&fakeTrigger{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/recoveries", nil)
	rec := httptest.NewRecorder()
	s.handleRecoveriesList(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	var out struct {
		Recoveries []RecoveryListItem `json:"recoveries"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Recoveries) != 1 {
		t.Fatalf("got %d items, want 1", len(out.Recoveries))
	}
	item := out.Recoveries[0]
	if item.SourceBead == nil {
		t.Fatalf("SourceBead is nil")
	}
	if item.SourceBead.ID != "spi-source" {
		t.Errorf("SourceBead.ID = %q, want spi-source", item.SourceBead.ID)
	}
	if item.SourceBead.Title != "the failing task" {
		t.Errorf("SourceBead.Title = %q, want the failing task", item.SourceBead.Title)
	}

	if len(item.GraphNeighbors) != 1 || item.GraphNeighbors[0].ID != "spi-design" {
		t.Errorf("GraphNeighbors = %+v, want one entry for spi-design", item.GraphNeighbors)
	}

	// Peer recoveries: should NOT include spi-r1 itself, only the prior one.
	if len(item.PeerRecoveries) != 1 {
		t.Fatalf("got %d peers, want 1 (excluding self)", len(item.PeerRecoveries))
	}
	peer := item.PeerRecoveries[0]
	if peer.ID != "spi-r-prior" {
		t.Errorf("peer.ID = %q, want spi-r-prior", peer.ID)
	}
	if peer.Verb != "reset --hard" {
		t.Errorf("peer.Verb = %q, want reset --hard", peer.Verb)
	}
	// The desktop banner heuristic ("you've rejected this 3 times")
	// keys on peer.Gate. Verify the gate decision surfaces from the
	// MetadataKeyGate metadata the gate handler writes.
	if peer.Gate != cleric.GateReject {
		t.Errorf("peer.Gate = %q, want %q", peer.Gate, cleric.GateReject)
	}
	if peer.GateOutcome != "" {
		t.Errorf("peer.GateOutcome = %q, want empty (rejected peers never reach cleric.finish)", peer.GateOutcome)
	}
}

// --------------------------------------------------------------------------
// POST /api/v1/recoveries/{id}/gate
// --------------------------------------------------------------------------

func TestHandleRecoveryGate_RejectsNonPOST(t *testing.T) {
	env := newRecoveryEnv()
	withRecoveryStubs(t, env)
	s := newTestServer(&fakeTrigger{})
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, "/api/v1/recoveries/spi-r1/gate", nil)
		rec := httptest.NewRecorder()
		s.handleRecoveryGate(rec, req, "spi-r1")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want 405", method, rec.Code)
		}
	}
}

func TestHandleRecoveryGate_RejectsEmptyID(t *testing.T) {
	env := newRecoveryEnv()
	withRecoveryStubs(t, env)
	s := newTestServer(&fakeTrigger{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/recoveries//gate", strings.NewReader(`{"gate":"approve"}`))
	rec := httptest.NewRecorder()
	s.handleRecoveryGate(rec, req, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleRecoveryGate_RejectsInvalidGate(t *testing.T) {
	env := newRecoveryEnv()
	env.beads["spi-r1"] = store.Bead{Type: "recovery", Status: "awaiting_review"}
	withRecoveryStubs(t, env)
	s := newTestServer(&fakeTrigger{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/recoveries/spi-r1/gate",
		strings.NewReader(`{"gate":"yolo"}`))
	rec := httptest.NewRecorder()
	s.handleRecoveryGate(rec, req, "spi-r1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "gate must be one of") {
		t.Errorf("error body = %q, want mention of valid gates", rec.Body.String())
	}
}

func TestHandleRecoveryGate_RequiresCommentOnReject(t *testing.T) {
	env := newRecoveryEnv()
	env.beads["spi-r1"] = store.Bead{Type: "recovery", Status: "awaiting_review"}
	withRecoveryStubs(t, env)
	s := newTestServer(&fakeTrigger{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/recoveries/spi-r1/gate",
		strings.NewReader(`{"gate":"reject"}`))
	rec := httptest.NewRecorder()
	s.handleRecoveryGate(rec, req, "spi-r1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "comment is required") {
		t.Errorf("error body = %q, want mention of required comment", rec.Body.String())
	}
}

func TestHandleRecoveryGate_RejectsNonRecoveryBead(t *testing.T) {
	env := newRecoveryEnv()
	env.beads["spi-task"] = store.Bead{Type: "task", Status: "awaiting_review"}
	withRecoveryStubs(t, env)
	s := newTestServer(&fakeTrigger{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/recoveries/spi-task/gate",
		strings.NewReader(`{"gate":"approve"}`))
	rec := httptest.NewRecorder()
	s.handleRecoveryGate(rec, req, "spi-task")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "recovery-bead-only") {
		t.Errorf("body = %q, want mention of recovery-bead-only", rec.Body.String())
	}
}

func TestHandleRecoveryGate_RejectsNonAwaitingReviewStatus(t *testing.T) {
	env := newRecoveryEnv()
	env.beads["spi-r1"] = store.Bead{Type: "recovery", Status: "in_progress"}
	withRecoveryStubs(t, env)
	s := newTestServer(&fakeTrigger{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/recoveries/spi-r1/gate",
		strings.NewReader(`{"gate":"approve"}`))
	rec := httptest.NewRecorder()
	s.handleRecoveryGate(rec, req, "spi-r1")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body=%q)", rec.Code, rec.Body.String())
	}
}

// TestHandleRecoveryGate_ApproveHappyPath verifies the canonical approve
// flow: GraphState's wait_for_gate.outputs.gate is set to "approve",
// recovery bead status flips to in_progress, and the response surfaces
// the post-state.
func TestHandleRecoveryGate_ApproveHappyPath(t *testing.T) {
	env := newRecoveryEnv()
	env.beads["spi-r1"] = store.Bead{Type: "recovery", Status: "awaiting_review"}
	env.graph["cleric-spi-r1"] = &executor.GraphState{
		BeadID: "spi-r1",
		Steps: map[string]executor.StepState{
			"decide":         {Status: "completed"},
			"publish":        {Status: "completed"},
			stepWaitForGate:  {Status: "active", Outputs: map[string]string{}},
		},
	}
	withRecoveryStubs(t, env)
	s := newTestServer(&fakeTrigger{})

	body := bytes.NewReader([]byte(`{"gate":"approve"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/recoveries/spi-r1/gate", body)
	rec := httptest.NewRecorder()
	s.handleRecoveryGate(rec, req, "spi-r1")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}

	// GraphState was loaded once and saved once.
	if len(env.calls.graphLoads) != 1 || env.calls.graphLoads[0] != "cleric-spi-r1" {
		t.Errorf("graphLoads = %v, want [cleric-spi-r1]", env.calls.graphLoads)
	}
	if len(env.calls.graphSaves) != 1 {
		t.Fatalf("graphSaves = %d, want 1", len(env.calls.graphSaves))
	}
	saved := env.calls.graphSaves[0].state
	if saved.Steps[stepWaitForGate].Outputs["gate"] != "approve" {
		t.Errorf("wait_for_gate.outputs.gate = %q, want approve",
			saved.Steps[stepWaitForGate].Outputs["gate"])
	}
	if _, present := saved.Steps[stepWaitForGate].Outputs["rejection_comment"]; present {
		t.Errorf("rejection_comment present on approve — want absent")
	}

	// Status flipped to in_progress.
	if len(env.calls.updateBeadCalls) != 1 {
		t.Fatalf("updateBead calls = %d, want 1", len(env.calls.updateBeadCalls))
	}
	upd := env.calls.updateBeadCalls[0]
	if upd["status"] != "in_progress" {
		t.Errorf("update status = %v, want in_progress", upd["status"])
	}

	// gate metadata was stamped.
	if len(env.calls.setMetadataCalls) != 1 {
		t.Fatalf("setMetadata calls = %d, want 1", len(env.calls.setMetadataCalls))
	}
	if env.calls.setMetadataCalls[0].meta[cleric.MetadataKeyGate] != cleric.GateApprove {
		t.Errorf("%s metadata = %q, want %q",
			cleric.MetadataKeyGate,
			env.calls.setMetadataCalls[0].meta[cleric.MetadataKeyGate],
			cleric.GateApprove)
	}
}

// TestHandleRecoveryGate_RejectStoresComment verifies the rejection_comment
// output is set when gate=reject + comment is supplied.
func TestHandleRecoveryGate_RejectStoresComment(t *testing.T) {
	env := newRecoveryEnv()
	env.beads["spi-r1"] = store.Bead{Type: "recovery", Status: "awaiting_review"}
	env.graph["cleric-spi-r1"] = &executor.GraphState{
		BeadID: "spi-r1",
		Steps: map[string]executor.StepState{
			stepWaitForGate: {Status: "active"},
		},
	}
	withRecoveryStubs(t, env)
	s := newTestServer(&fakeTrigger{})

	body := bytes.NewReader([]byte(`{"gate":"reject","comment":"try a different approach"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/recoveries/spi-r1/gate", body)
	rec := httptest.NewRecorder()
	s.handleRecoveryGate(rec, req, "spi-r1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}

	if len(env.calls.graphSaves) != 1 {
		t.Fatalf("graphSaves = %d, want 1", len(env.calls.graphSaves))
	}
	saved := env.calls.graphSaves[0].state.Steps[stepWaitForGate].Outputs
	if saved["gate"] != "reject" {
		t.Errorf("gate = %q, want reject", saved["gate"])
	}
	if saved["rejection_comment"] != "try a different approach" {
		t.Errorf("rejection_comment = %q, want %q", saved["rejection_comment"], "try a different approach")
	}

	// Metadata records the comment too.
	if env.calls.setMetadataCalls[0].meta[cleric.MetadataKeyGateComment] != "try a different approach" {
		t.Errorf("metadata %s = %q, want %q",
			cleric.MetadataKeyGateComment,
			env.calls.setMetadataCalls[0].meta[cleric.MetadataKeyGateComment],
			"try a different approach")
	}
}

// TestHandleRecoveryGate_TakeoverHappyPath confirms the takeover flow
// sets the gate output and the bead status flips to in_progress (the
// formula's handle_takeover step then closes the bead).
func TestHandleRecoveryGate_TakeoverHappyPath(t *testing.T) {
	env := newRecoveryEnv()
	env.beads["spi-r1"] = store.Bead{Type: "recovery", Status: "awaiting_review"}
	env.graph["cleric-spi-r1"] = &executor.GraphState{
		BeadID: "spi-r1",
		Steps: map[string]executor.StepState{
			stepWaitForGate: {Status: "active"},
		},
	}
	withRecoveryStubs(t, env)
	s := newTestServer(&fakeTrigger{})

	body := bytes.NewReader([]byte(`{"gate":"takeover"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/recoveries/spi-r1/gate", body)
	rec := httptest.NewRecorder()
	s.handleRecoveryGate(rec, req, "spi-r1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}

	saved := env.calls.graphSaves[0].state.Steps[stepWaitForGate].Outputs
	if saved["gate"] != "takeover" {
		t.Errorf("gate = %q, want takeover", saved["gate"])
	}
}

// TestHandleRecoveryGate_NoGraphState_Returns409 covers the case where
// a recovery bead reaches awaiting_review without a persisted GraphState.
// The cleric formula is the only path to awaiting_review — without graph
// state the human's gate decision has nowhere to land because no executor
// path rehydrates the metadata into wait_for_gate.outputs. Returning 409
// keeps the operator in the loop rather than silently writing metadata
// the formula will ignore on the next dispatch (spi-skfsia finding 3).
func TestHandleRecoveryGate_NoGraphState_Returns409(t *testing.T) {
	env := newRecoveryEnv()
	env.beads["spi-r1"] = store.Bead{Type: "recovery", Status: "awaiting_review"}
	// Note: env.graph["cleric-spi-r1"] is not set
	withRecoveryStubs(t, env)
	s := newTestServer(&fakeTrigger{})

	body := bytes.NewReader([]byte(`{"gate":"approve"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/recoveries/spi-r1/gate", body)
	rec := httptest.NewRecorder()
	s.handleRecoveryGate(rec, req, "spi-r1")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body=%q)", rec.Code, rec.Body.String())
	}

	// No graph save, no metadata write, no status flip.
	if len(env.calls.graphSaves) != 0 {
		t.Errorf("graphSaves = %d, want 0 (no GraphState present)", len(env.calls.graphSaves))
	}
	if len(env.calls.setMetadataCalls) != 0 {
		t.Errorf("setMetadata called %d times; want 0 — handler must refuse on missing graph state", len(env.calls.setMetadataCalls))
	}
	if len(env.calls.updateBeadCalls) != 0 {
		t.Errorf("updateBead called %d times; want 0 — bead status must not flip on 409", len(env.calls.updateBeadCalls))
	}
}

// TestHandleRecoveryGate_GraphStateMissingStep returns 409 when the
// loaded GraphState exists but has no wait_for_gate step (formula
// drift). The 409 keeps the operator in the loop rather than silently
// fixing.
func TestHandleRecoveryGate_GraphStateMissingStep(t *testing.T) {
	env := newRecoveryEnv()
	env.beads["spi-r1"] = store.Bead{Type: "recovery", Status: "awaiting_review"}
	env.graph["cleric-spi-r1"] = &executor.GraphState{
		BeadID: "spi-r1",
		Steps: map[string]executor.StepState{
			"decide": {Status: "completed"},
		},
	}
	withRecoveryStubs(t, env)
	s := newTestServer(&fakeTrigger{})

	body := bytes.NewReader([]byte(`{"gate":"approve"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/recoveries/spi-r1/gate", body)
	rec := httptest.NewRecorder()
	s.handleRecoveryGate(rec, req, "spi-r1")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body=%q)", rec.Code, rec.Body.String())
	}
}

// TestHandleRecoveryGate_StampsArchmage verifies the calling archmage's
// labels are stamped onto the recovery bead so the audit trail has a
// stable attribution. Mirrors the spi-kntoe1 / spi-wrjiw6 stamping
// pattern shipped in actions.go.
func TestHandleRecoveryGate_StampsArchmage(t *testing.T) {
	env := newRecoveryEnv()
	env.beads["spi-r1"] = store.Bead{Type: "recovery", Status: "awaiting_review"}
	env.graph["cleric-spi-r1"] = &executor.GraphState{
		BeadID: "spi-r1",
		Steps: map[string]executor.StepState{
			stepWaitForGate: {Status: "active"},
		},
	}
	withRecoveryStubs(t, env)
	s := newTestServer(&fakeTrigger{})

	body := bytes.NewReader([]byte(`{"gate":"approve"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/recoveries/spi-r1/gate", body)
	req = withRequestIdentity(req, "Alice", "alice@example.com")
	rec := httptest.NewRecorder()
	s.handleRecoveryGate(rec, req, "spi-r1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}

	// At least one archmage:Alice label should have been added.
	var stamped bool
	for _, lc := range env.calls.addLabelCalls {
		if lc.id == "spi-r1" && strings.Contains(lc.label, "archmage:") {
			stamped = true
			break
		}
	}
	if !stamped {
		t.Errorf("archmage label not stamped on recovery; addLabelCalls=%+v", env.calls.addLabelCalls)
	}

	// Audit comment should attribute to the archmage when identity is set.
	if len(env.calls.addCommentAsCalls) == 0 {
		t.Errorf("expected AddCommentAs to be invoked when identity is present")
	}
}

// TestHandleRecoveryByID_RoutesGate verifies the dispatcher in actions.go
// routes /gate to handleRecoveryGate.
func TestHandleRecoveryByID_RoutesGate(t *testing.T) {
	env := newRecoveryEnv()
	env.beads["spi-r1"] = store.Bead{Type: "recovery", Status: "awaiting_review"}
	env.graph["cleric-spi-r1"] = &executor.GraphState{
		BeadID: "spi-r1",
		Steps: map[string]executor.StepState{
			stepWaitForGate: {Status: "active"},
		},
	}
	withRecoveryStubs(t, env)
	// The dispatcher also touches actionsStoreEnsureFunc / actionsAddLabelFunc
	// / actionsAddCommentAsFunc when it routes to comment_request — those
	// aren't on this code path but withActionStubs is required to keep the
	// shared seams from panicking.
	withActionStubs(t, store.Bead{Status: "awaiting_review", Type: "recovery"}, nil)
	s := newTestServer(&fakeTrigger{})

	body := bytes.NewReader([]byte(`{"gate":"approve"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/recoveries/spi-r1/gate", body)
	rec := httptest.NewRecorder()
	s.handleRecoveryByID(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}

	if len(env.calls.graphSaves) == 0 {
		t.Errorf("expected graph save on gate route — dispatcher did not invoke handler")
	}
}

func TestHandleRecoveryByID_UnknownAction(t *testing.T) {
	env := newRecoveryEnv()
	env.beads["spi-r1"] = store.Bead{Type: "recovery", Status: "awaiting_review"}
	withRecoveryStubs(t, env)
	withActionStubs(t, store.Bead{}, nil)
	s := newTestServer(&fakeTrigger{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/recoveries/spi-r1/foobar", nil)
	rec := httptest.NewRecorder()
	s.handleRecoveryByID(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestHandleRecoveryGate_ParkedApproveAdvancesToExecute pins the
// regression for spi-skfsia finding 1: the gate handler must reset the
// wait_for_gate step status from `hooked` (the realistic post-park
// state) back to `pending` so the interpreter's hooked-filter
// (graph_interpreter.go:147) lets the formula re-evaluate the wait step
// and advance to execute on the next dispatch. Pre-fix the step would
// remain parked because its outputs were updated but its status stayed
// hooked.
func TestHandleRecoveryGate_ParkedApproveAdvancesToExecute(t *testing.T) {
	env := newRecoveryEnv()
	env.beads["spi-r1"] = store.Bead{Type: "recovery", Status: "awaiting_review"}
	env.graph["cleric-spi-r1"] = &executor.GraphState{
		BeadID: "spi-r1",
		Steps: map[string]executor.StepState{
			"decide":        {Status: "completed"},
			"publish":       {Status: "completed"},
			stepWaitForGate: {Status: "hooked", Outputs: map[string]string{}},
		},
		StepBeadIDs: map[string]string{
			stepWaitForGate: "spi-r1-wait",
		},
	}
	withRecoveryStubs(t, env)
	s := newTestServer(&fakeTrigger{})

	body := bytes.NewReader([]byte(`{"gate":"approve"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/recoveries/spi-r1/gate", body)
	rec := httptest.NewRecorder()
	s.handleRecoveryGate(rec, req, "spi-r1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}

	if len(env.calls.graphSaves) != 1 {
		t.Fatalf("graphSaves = %d, want 1", len(env.calls.graphSaves))
	}
	saved := env.calls.graphSaves[0].state.Steps[stepWaitForGate]
	if saved.Status != "pending" {
		t.Errorf("wait_for_gate.Status = %q, want pending (must reset from hooked so interpreter re-evaluates)", saved.Status)
	}
	if saved.Outputs["gate"] != "approve" {
		t.Errorf("wait_for_gate.outputs.gate = %q, want approve", saved.Outputs["gate"])
	}

	// Step bead must be unhooked too so the interpreter's reactivation
	// logic doesn't refuse to advance.
	if len(env.calls.unhookStepBead) != 1 || env.calls.unhookStepBead[0] != "spi-r1-wait" {
		t.Errorf("unhookStepBead calls = %v, want [spi-r1-wait]", env.calls.unhookStepBead)
	}
}

// TestHandleRecoveryGate_ParkedRejectResetsAndPreservesComment pins the
// reject path: from a parked wait_for_gate state, gate=reject must
// reset wait_for_gate's step status and the prior decide/publish steps
// to `pending` so a fresh cleric round starts, and the rejection_comment
// must land in both the step outputs (where cleric.reject reads it) and
// the bead metadata (where the audit/listing surfaces read it).
func TestHandleRecoveryGate_ParkedRejectResetsAndPreservesComment(t *testing.T) {
	env := newRecoveryEnv()
	env.beads["spi-r1"] = store.Bead{Type: "recovery", Status: "awaiting_review"}
	env.graph["cleric-spi-r1"] = &executor.GraphState{
		BeadID: "spi-r1",
		Steps: map[string]executor.StepState{
			"decide":        {Status: "completed", Outputs: map[string]string{"result": "stale"}},
			"publish":       {Status: "completed", Outputs: map[string]string{"status": "awaiting_review"}},
			stepWaitForGate: {Status: "hooked", Outputs: map[string]string{}},
		},
	}
	withRecoveryStubs(t, env)
	s := newTestServer(&fakeTrigger{})

	body := bytes.NewReader([]byte(`{"gate":"reject","comment":"try a different approach"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/recoveries/spi-r1/gate", body)
	rec := httptest.NewRecorder()
	s.handleRecoveryGate(rec, req, "spi-r1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}

	saved := env.calls.graphSaves[0].state
	wfg := saved.Steps[stepWaitForGate]
	if wfg.Status != "pending" {
		t.Errorf("wait_for_gate.Status = %q, want pending", wfg.Status)
	}
	if wfg.Outputs["gate"] != "reject" {
		t.Errorf("wait_for_gate.outputs.gate = %q, want reject", wfg.Outputs["gate"])
	}
	if wfg.Outputs["rejection_comment"] != "try a different approach" {
		t.Errorf("wait_for_gate.outputs.rejection_comment = %q, want %q",
			wfg.Outputs["rejection_comment"], "try a different approach")
	}
	if dec := saved.Steps["decide"]; dec.Status != "pending" || dec.Outputs != nil {
		t.Errorf("decide reset to wrong state: status=%q outputs=%v; want pending/nil", dec.Status, dec.Outputs)
	}
	if pub := saved.Steps["publish"]; pub.Status != "pending" || pub.Outputs != nil {
		t.Errorf("publish reset to wrong state: status=%q outputs=%v; want pending/nil", pub.Status, pub.Outputs)
	}

	// Metadata records the comment too.
	if env.calls.setMetadataCalls[0].meta[cleric.MetadataKeyGateComment] != "try a different approach" {
		t.Errorf("metadata %s = %q; want %q",
			cleric.MetadataKeyGateComment,
			env.calls.setMetadataCalls[0].meta[cleric.MetadataKeyGateComment],
			"try a different approach")
	}
}

// TestHandleRecoveryGate_ParkedTakeoverUnparks pins the takeover-from-
// parked-state path: the gate handler resets the wait_for_gate step
// status to `pending` so the formula advances to handle_takeover (which
// labels source needs-manual + closes recovery). Without the reset the
// step stays hooked and the formula never reaches the takeover branch.
func TestHandleRecoveryGate_ParkedTakeoverUnparks(t *testing.T) {
	env := newRecoveryEnv()
	env.beads["spi-r1"] = store.Bead{Type: "recovery", Status: "awaiting_review"}
	env.graph["cleric-spi-r1"] = &executor.GraphState{
		BeadID: "spi-r1",
		Steps: map[string]executor.StepState{
			stepWaitForGate: {Status: "hooked"},
		},
	}
	withRecoveryStubs(t, env)
	s := newTestServer(&fakeTrigger{})

	body := bytes.NewReader([]byte(`{"gate":"takeover"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/recoveries/spi-r1/gate", body)
	rec := httptest.NewRecorder()
	s.handleRecoveryGate(rec, req, "spi-r1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}

	saved := env.calls.graphSaves[0].state.Steps[stepWaitForGate]
	if saved.Status != "pending" {
		t.Errorf("wait_for_gate.Status = %q, want pending", saved.Status)
	}
	if saved.Outputs["gate"] != "takeover" {
		t.Errorf("wait_for_gate.outputs.gate = %q, want takeover", saved.Outputs["gate"])
	}
	// rejection_comment must NOT be present on takeover.
	if _, ok := saved.Outputs["rejection_comment"]; ok {
		t.Errorf("rejection_comment present on takeover; want absent")
	}
}

// TestSanitizeAgentNameLocal pins the parity with steward.SanitizeK8sLabel
// since the gateway's gate routing computes the cleric agent name. A
// regression here would silently break gate writes.
func TestSanitizeAgentNameLocal(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{in: "spi-abc123", want: "spi-abc123"},
		{in: "spi-A1B2", want: "spi-a1b2"},
		{in: "spi.r_1", want: "spi-r-1"},
		{in: "Spi-R/1", want: "spi-r1"},
	}
	for _, c := range cases {
		got := sanitizeAgentNameLocal(c.in)
		if got != c.want {
			t.Errorf("sanitizeAgentNameLocal(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

