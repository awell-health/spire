package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/cleric"
	"github.com/awell-health/spire/pkg/store"
	"github.com/awell-health/spire/pkg/summon"
)

// actionStubCalls aggregates the side effects each test wants to assert
// on. We use a single struct rather than per-handler call structs so the
// test stubs can intercept everything from one place.
type actionStubCalls struct {
	getBead          int
	updateBead       []map[string]interface{}
	addLabels        []string
	addComment       []addCommentCall
	addCommentAs     []addCommentAsCall
	resummonRuns     []string
	closeRuns        []string
	resetHardRuns    []string
}

type addCommentCall struct {
	id, text string
}
type addCommentAsCall struct {
	id, author, text string
}

// withActionStubs swaps the package-level seams used by the new action
// endpoints. Each test passes a `bead` value (cloned per get) and a
// `getErr` to drive the GetBead branch.
//
// The returned actionStubCalls handle records every observed side
// effect so tests can assert on labels, comments, and dispatched runs
// without booting a real Dolt store.
func withActionStubs(t *testing.T, bead store.Bead, getErr error) *actionStubCalls {
	t.Helper()
	prevEnsure := actionsStoreEnsureFunc
	prevGet := actionsGetBeadFunc
	prevUpdate := actionsUpdateBeadFunc
	prevAddLabel := actionsAddLabelFunc
	prevAddComment := actionsAddCommentFunc
	prevAddCommentAs := actionsAddCommentAsFunc
	prevResummon := resummonRunFunc
	prevClose := dismissCloseFunc
	prevResetHard := dismissResetHardFunc

	calls := &actionStubCalls{}
	current := bead

	actionsStoreEnsureFunc = func(string) error { return nil }
	actionsGetBeadFunc = func(id string) (store.Bead, error) {
		calls.getBead++
		if getErr != nil {
			return store.Bead{}, getErr
		}
		b := current
		b.ID = id
		return b, nil
	}
	actionsUpdateBeadFunc = func(id string, updates map[string]interface{}) error {
		merged := make(map[string]interface{}, len(updates)+1)
		merged["__id"] = id
		for k, v := range updates {
			merged[k] = v
		}
		calls.updateBead = append(calls.updateBead, merged)
		// Reflect status updates back so subsequent GetBead calls return
		// the post-transition state.
		if v, ok := updates["status"].(string); ok {
			current.Status = v
		}
		return nil
	}
	actionsAddLabelFunc = func(id, label string) error {
		calls.addLabels = append(calls.addLabels, label)
		current.Labels = append(current.Labels, label)
		return nil
	}
	actionsAddCommentFunc = func(id, text string) (string, error) {
		calls.addComment = append(calls.addComment, addCommentCall{id: id, text: text})
		return "c-stub-1", nil
	}
	actionsAddCommentAsFunc = func(id, author, text string) (string, error) {
		calls.addCommentAs = append(calls.addCommentAs, addCommentAsCall{id: id, author: author, text: text})
		return "c-stub-2", nil
	}
	resummonRunFunc = func(id, dispatch string) (summon.Result, error) {
		calls.resummonRuns = append(calls.resummonRuns, id)
		// Reflect the hooked → in_progress transition that summon.Run does.
		if current.Status == "hooked" {
			current.Status = "in_progress"
		}
		return summon.Result{WizardName: "wizard-" + id}, nil
	}
	dismissCloseFunc = func(id string) error {
		calls.closeRuns = append(calls.closeRuns, id)
		current.Status = "closed"
		return nil
	}
	dismissResetHardFunc = func(_ context.Context, id string) error {
		calls.resetHardRuns = append(calls.resetHardRuns, id)
		return nil
	}

	t.Cleanup(func() {
		actionsStoreEnsureFunc = prevEnsure
		actionsGetBeadFunc = prevGet
		actionsUpdateBeadFunc = prevUpdate
		actionsAddLabelFunc = prevAddLabel
		actionsAddCommentFunc = prevAddComment
		actionsAddCommentAsFunc = prevAddCommentAs
		resummonRunFunc = prevResummon
		dismissCloseFunc = prevClose
		dismissResetHardFunc = prevResetHard
	})
	return calls
}

// withRequestIdentity attaches an ArchmageIdentity to the request's
// context so handlers' archmage-stamping branches fire.
func withRequestIdentity(req *http.Request, name, email string) *http.Request {
	id := ArchmageIdentity{Name: name, Email: email, Source: "header"}
	return req.WithContext(WithIdentity(req.Context(), id))
}

// --------------------------------------------------------------------------
// /api/v1/actions
// --------------------------------------------------------------------------

func TestHandleActionsManifest_RejectsNonGET(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		req := httptest.NewRequest(method, "/api/v1/actions", nil)
		rec := httptest.NewRecorder()
		s.handleActionsManifest(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want 405", method, rec.Code)
		}
	}
}

func TestHandleActionsManifest_HappyPath(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/actions", nil)
	rec := httptest.NewRecorder()
	s.handleActionsManifest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	var got struct {
		Actions []cleric.ActionManifest `json:"actions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.Actions) != len(cleric.V1Actions) {
		t.Fatalf("got %d actions, want %d", len(got.Actions), len(cleric.V1Actions))
	}
	wantNames := map[string]bool{
		"resummon": true, "dismiss": true, "update_status": true,
		"comment_request": true, "reset_hard": true,
	}
	for _, a := range got.Actions {
		if !wantNames[a.Name] {
			t.Errorf("unexpected action name %q in manifest", a.Name)
		}
	}
}

// --------------------------------------------------------------------------
// /api/v1/beads/{id}/resummon
// --------------------------------------------------------------------------

func TestHandleBeadResummon_RejectsNonPOST(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	withActionStubs(t, store.Bead{Status: "hooked"}, nil)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, "/api/v1/beads/spi-abc/resummon", nil)
		rec := httptest.NewRecorder()
		s.handleBeadResummon(rec, req, "spi-abc")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want 405", method, rec.Code)
		}
	}
}

func TestHandleBeadResummon_RejectsEmptyID(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	withActionStubs(t, store.Bead{}, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/beads//resummon", nil)
	rec := httptest.NewRecorder()
	s.handleBeadResummon(rec, req, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleBeadResummon_NotFoundReturns404(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	withActionStubs(t, store.Bead{}, errors.New("bead spi-x not found"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/beads/spi-x/resummon", nil)
	rec := httptest.NewRecorder()
	s.handleBeadResummon(rec, req, "spi-x")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%q)", rec.Code, rec.Body.String())
	}
}

// TestHandleBeadResummon_HookedHappyPath verifies the canonical path:
// hooked bead → summon.Run fires → 200 with post-state.
func TestHandleBeadResummon_HookedHappyPath(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	calls := withActionStubs(t, store.Bead{Status: "hooked"}, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/beads/spi-abc/resummon", nil)
	rec := httptest.NewRecorder()
	s.handleBeadResummon(rec, req, "spi-abc")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	if len(calls.resummonRuns) != 1 || calls.resummonRuns[0] != "spi-abc" {
		t.Errorf("resummonRunFunc invocations = %v, want [spi-abc]", calls.resummonRuns)
	}
	var got store.Bead
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != "in_progress" {
		t.Errorf("post-resummon status = %q, want in_progress", got.Status)
	}
}

// TestHandleBeadResummon_IdempotentOnInProgress verifies that a re-fire
// when the bead is already in_progress returns 200 without spawning a
// second wizard.
func TestHandleBeadResummon_IdempotentOnInProgress(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	calls := withActionStubs(t, store.Bead{Status: "in_progress"}, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/beads/spi-abc/resummon", nil)
	rec := httptest.NewRecorder()
	s.handleBeadResummon(rec, req, "spi-abc")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	if len(calls.resummonRuns) != 0 {
		t.Errorf("resummonRunFunc was invoked %d times, want 0 (idempotent re-fire on in_progress)", len(calls.resummonRuns))
	}
}

// TestHandleBeadResummon_RejectsClosed pins the "closed → no resummon"
// rule. The 4xx body must mention "closed" so callers can pattern-match.
func TestHandleBeadResummon_RejectsClosed(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	withActionStubs(t, store.Bead{Status: "closed"}, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/beads/spi-abc/resummon", nil)
	rec := httptest.NewRecorder()
	s.handleBeadResummon(rec, req, "spi-abc")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "closed") {
		t.Errorf("body = %q, want mention of closed", rec.Body.String())
	}
}

// TestHandleBeadResummon_RejectsOpenWithConflict pins the "wizard
// presumed alive" rule for non-hooked / non-in_progress states. Returns
// 409 with a message about "not hooked" so the desktop knows the
// resummon doesn't apply.
func TestHandleBeadResummon_RejectsOpenWithConflict(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	withActionStubs(t, store.Bead{Status: "open"}, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/beads/spi-abc/resummon", nil)
	rec := httptest.NewRecorder()
	s.handleBeadResummon(rec, req, "spi-abc")

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body=%q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "hooked") {
		t.Errorf("body = %q, want mention of hooked", rec.Body.String())
	}
}

// TestHandleBeadResummon_StampsArchmage verifies the archmage labels are
// stamped on the post-resummon bead when the request supplies an
// identity. This is the audit trail for "who triggered this".
func TestHandleBeadResummon_StampsArchmage(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	calls := withActionStubs(t, store.Bead{Status: "hooked"}, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/beads/spi-abc/resummon", nil)
	req = withRequestIdentity(req, "alice", "alice@example.com")
	rec := httptest.NewRecorder()
	s.handleBeadResummon(rec, req, "spi-abc")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	wantLabels := map[string]bool{
		"archmage:alice":                   false,
		"archmage-email:alice@example.com": false,
	}
	for _, l := range calls.addLabels {
		if _, ok := wantLabels[l]; ok {
			wantLabels[l] = true
		}
	}
	for label, seen := range wantLabels {
		if !seen {
			t.Errorf("archmage label %q was not stamped (got %v)", label, calls.addLabels)
		}
	}
}

// --------------------------------------------------------------------------
// /api/v1/beads/{id}/dismiss
// --------------------------------------------------------------------------

func TestHandleBeadDismiss_RejectsNonPOST(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	withActionStubs(t, store.Bead{Status: "in_progress"}, nil)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, "/api/v1/beads/spi-abc/dismiss", nil)
		rec := httptest.NewRecorder()
		s.handleBeadDismiss(rec, req, "spi-abc")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want 405", method, rec.Code)
		}
	}
}

func TestHandleBeadDismiss_HappyPath(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	calls := withActionStubs(t, store.Bead{Status: "in_progress"}, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/beads/spi-abc/dismiss", nil)
	rec := httptest.NewRecorder()
	s.handleBeadDismiss(rec, req, "spi-abc")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	if len(calls.resetHardRuns) != 1 || calls.resetHardRuns[0] != "spi-abc" {
		t.Errorf("dismissResetHardFunc runs = %v, want [spi-abc]", calls.resetHardRuns)
	}
	if len(calls.closeRuns) != 1 || calls.closeRuns[0] != "spi-abc" {
		t.Errorf("dismissCloseFunc runs = %v, want [spi-abc]", calls.closeRuns)
	}
	// closed_reason label must be stamped before the close lifecycle so
	// downstream observability can distinguish dismissal.
	foundCloseReason := false
	for _, l := range calls.addLabels {
		if l == "closed_reason:dismissed" {
			foundCloseReason = true
			break
		}
	}
	if !foundCloseReason {
		t.Errorf("closed_reason:dismissed label not stamped (got %v)", calls.addLabels)
	}
}

func TestHandleBeadDismiss_IdempotentOnClosed(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	calls := withActionStubs(t, store.Bead{Status: "closed"}, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/beads/spi-abc/dismiss", nil)
	rec := httptest.NewRecorder()
	s.handleBeadDismiss(rec, req, "spi-abc")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	if len(calls.resetHardRuns) != 0 {
		t.Errorf("dismissResetHardFunc was invoked %d times on already-closed bead", len(calls.resetHardRuns))
	}
	if len(calls.closeRuns) != 0 {
		t.Errorf("dismissCloseFunc was invoked %d times on already-closed bead", len(calls.closeRuns))
	}
}

func TestHandleBeadDismiss_NotFoundReturns404(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	withActionStubs(t, store.Bead{}, errors.New("bead spi-x not found"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/beads/spi-x/dismiss", nil)
	rec := httptest.NewRecorder()
	s.handleBeadDismiss(rec, req, "spi-x")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// --------------------------------------------------------------------------
// /api/v1/beads/{id}/update_status
// --------------------------------------------------------------------------

func TestHandleBeadUpdateStatus_RejectsNonPOST(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	withActionStubs(t, store.Bead{Status: "hooked"}, nil)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, "/api/v1/beads/spi-abc/update_status", strings.NewReader(`{"to":"open"}`))
		rec := httptest.NewRecorder()
		s.handleBeadUpdateStatus(rec, req, "spi-abc")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want 405", method, rec.Code)
		}
	}
}

func TestHandleBeadUpdateStatus_MissingToReturns400(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	withActionStubs(t, store.Bead{Status: "hooked"}, nil)

	body := strings.NewReader(`{}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/beads/spi-abc/update_status", body)
	req.ContentLength = int64(body.Len())
	rec := httptest.NewRecorder()
	s.handleBeadUpdateStatus(rec, req, "spi-abc")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "required") {
		t.Errorf("body = %q, want mention of required", rec.Body.String())
	}
}

func TestHandleBeadUpdateStatus_InvalidJSONReturns400(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	withActionStubs(t, store.Bead{Status: "hooked"}, nil)

	body := strings.NewReader(`{not valid`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/beads/spi-abc/update_status", body)
	req.ContentLength = int64(body.Len())
	rec := httptest.NewRecorder()
	s.handleBeadUpdateStatus(rec, req, "spi-abc")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%q)", rec.Code, rec.Body.String())
	}
}

func TestHandleBeadUpdateStatus_RejectsNonWhitelistedTransition(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	calls := withActionStubs(t, store.Bead{Status: "closed"}, nil)

	body := strings.NewReader(`{"to":"open"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/beads/spi-abc/update_status", body)
	req.ContentLength = int64(body.Len())
	rec := httptest.NewRecorder()
	s.handleBeadUpdateStatus(rec, req, "spi-abc")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%q)", rec.Code, rec.Body.String())
	}
	// Body must name the source status so the desktop can build a useful
	// error.
	if !strings.Contains(rec.Body.String(), "closed") {
		t.Errorf("body = %q, want mention of closed source status", rec.Body.String())
	}
	if len(calls.updateBead) != 0 {
		t.Errorf("UpdateBead was called %d times despite a rejected transition", len(calls.updateBead))
	}
}

func TestHandleBeadUpdateStatus_HappyPathHookedToOpen(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	calls := withActionStubs(t, store.Bead{Status: "hooked"}, nil)

	body := strings.NewReader(`{"to":"open"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/beads/spi-abc/update_status", body)
	req.ContentLength = int64(body.Len())
	rec := httptest.NewRecorder()
	s.handleBeadUpdateStatus(rec, req, "spi-abc")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	if len(calls.updateBead) != 1 {
		t.Fatalf("UpdateBead calls = %d, want 1", len(calls.updateBead))
	}
	if got := calls.updateBead[0]["status"]; got != "open" {
		t.Errorf("UpdateBead status arg = %v, want open", got)
	}
}

func TestHandleBeadUpdateStatus_IdempotentSelfTransition(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	calls := withActionStubs(t, store.Bead{Status: "open"}, nil)

	body := strings.NewReader(`{"to":"open"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/beads/spi-abc/update_status", body)
	req.ContentLength = int64(body.Len())
	rec := httptest.NewRecorder()
	s.handleBeadUpdateStatus(rec, req, "spi-abc")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	if len(calls.updateBead) != 0 {
		t.Errorf("UpdateBead was called %d times on a self-transition; expected 0", len(calls.updateBead))
	}
}

// --------------------------------------------------------------------------
// /api/v1/beads/{id}/reset_hard
// --------------------------------------------------------------------------

func TestHandleBeadResetHard_RejectsNonPOST(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	withActionStubs(t, store.Bead{Status: "hooked"}, nil)
	prevMode := resetTowerModeFunc
	resetTowerModeFunc = func() (string, error) { return "", nil }
	t.Cleanup(func() { resetTowerModeFunc = prevMode })

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, "/api/v1/beads/spi-abc/reset_hard", nil)
		rec := httptest.NewRecorder()
		s.handleBeadResetHard(rec, req, "spi-abc")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want 405", method, rec.Code)
		}
	}
}

func TestHandleBeadResetHard_ClusterModeReturns501(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	withActionStubs(t, store.Bead{Status: "in_progress"}, nil)
	prevMode := resetTowerModeFunc
	resetTowerModeFunc = func() (string, error) { return "gateway", nil }
	t.Cleanup(func() { resetTowerModeFunc = prevMode })

	req := httptest.NewRequest(http.MethodPost, "/api/v1/beads/spi-abc/reset_hard", nil)
	rec := httptest.NewRecorder()
	s.handleBeadResetHard(rec, req, "spi-abc")

	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 (body=%q)", rec.Code, rec.Body.String())
	}
}

func TestHandleBeadResetHard_HappyPath(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	calls := withActionStubs(t, store.Bead{Status: "in_progress"}, nil)
	prevMode := resetTowerModeFunc
	resetTowerModeFunc = func() (string, error) { return "", nil }
	t.Cleanup(func() { resetTowerModeFunc = prevMode })

	req := httptest.NewRequest(http.MethodPost, "/api/v1/beads/spi-abc/reset_hard", nil)
	rec := httptest.NewRecorder()
	s.handleBeadResetHard(rec, req, "spi-abc")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	if len(calls.resetHardRuns) != 1 || calls.resetHardRuns[0] != "spi-abc" {
		t.Errorf("resetHard runs = %v, want [spi-abc]", calls.resetHardRuns)
	}
}

func TestHandleBeadResetHard_IdempotentRefire(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	calls := withActionStubs(t, store.Bead{Status: "in_progress"}, nil)
	prevMode := resetTowerModeFunc
	resetTowerModeFunc = func() (string, error) { return "", nil }
	t.Cleanup(func() { resetTowerModeFunc = prevMode })

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/beads/spi-abc/reset_hard", nil)
		rec := httptest.NewRecorder()
		s.handleBeadResetHard(rec, req, "spi-abc")
		if rec.Code != http.StatusOK {
			t.Fatalf("call %d: status = %d, want 200", i, rec.Code)
		}
	}
	if len(calls.resetHardRuns) != 2 {
		t.Errorf("resetHard runs = %d, want 2 (each call dispatches; underlying primitive is idempotent on already-clean state)", len(calls.resetHardRuns))
	}
}

// --------------------------------------------------------------------------
// /api/v1/recoveries/{id}/comment_request
// --------------------------------------------------------------------------

func TestHandleRecoveryCommentRequest_RejectsNonPOST(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	withActionStubs(t, store.Bead{Type: "recovery", Status: "awaiting_review"}, nil)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, "/api/v1/recoveries/spi-abc/comment_request", strings.NewReader(`{"question":"x"}`))
		rec := httptest.NewRecorder()
		s.handleRecoveryCommentRequest(rec, req, "spi-abc")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want 405", method, rec.Code)
		}
	}
}

func TestHandleRecoveryCommentRequest_MissingQuestionReturns400(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	withActionStubs(t, store.Bead{Type: "recovery"}, nil)

	body := strings.NewReader(`{}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/recoveries/spi-abc/comment_request", body)
	req.ContentLength = int64(body.Len())
	rec := httptest.NewRecorder()
	s.handleRecoveryCommentRequest(rec, req, "spi-abc")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "required") {
		t.Errorf("body = %q, want mention of required", rec.Body.String())
	}
}

func TestHandleRecoveryCommentRequest_RejectsNonRecoveryType(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	calls := withActionStubs(t, store.Bead{Type: "task", Status: "in_progress"}, nil)

	body := strings.NewReader(`{"question":"what now?"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/recoveries/spi-abc/comment_request", body)
	req.ContentLength = int64(body.Len())
	rec := httptest.NewRecorder()
	s.handleRecoveryCommentRequest(rec, req, "spi-abc")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "recovery-bead-only") {
		t.Errorf("body = %q, want mention of recovery-bead-only", rec.Body.String())
	}
	if len(calls.addComment) != 0 || len(calls.addCommentAs) != 0 {
		t.Errorf("comment was written despite a non-recovery target type")
	}
}

func TestHandleRecoveryCommentRequest_HappyPathWritesLabeledComment(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	calls := withActionStubs(t, store.Bead{Type: "recovery", Status: "awaiting_review"}, nil)

	body := strings.NewReader(`{"question":"do I close or escalate?"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/recoveries/spi-abc/comment_request", body)
	req.ContentLength = int64(body.Len())
	rec := httptest.NewRecorder()
	s.handleRecoveryCommentRequest(rec, req, "spi-abc")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	// The comment text must carry the structured prefix so the next
	// cleric prompt-builder can extract it.
	if len(calls.addComment) != 1 {
		t.Fatalf("addComment calls = %d, want 1", len(calls.addComment))
	}
	if !strings.HasPrefix(calls.addComment[0].text, "[cleric-request-input]") {
		t.Errorf("comment text = %q, want [cleric-request-input] prefix", calls.addComment[0].text)
	}
	if !strings.Contains(calls.addComment[0].text, "do I close or escalate?") {
		t.Errorf("comment text = %q, want question round-tripped", calls.addComment[0].text)
	}
	// The label index must be stamped so the desktop can find pending
	// requests without scanning every comment.
	foundLabel := false
	for _, l := range calls.addLabels {
		if l == "cleric-request-input" {
			foundLabel = true
			break
		}
	}
	if !foundLabel {
		t.Errorf("cleric-request-input label not stamped (got %v)", calls.addLabels)
	}
	// Status must NOT change (the recovery stays in awaiting_review).
	if len(calls.updateBead) != 0 {
		t.Errorf("UpdateBead was called %d times; comment_request must not transition status", len(calls.updateBead))
	}
}

func TestHandleRecoveryCommentRequest_StampsArchmageAuthor(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	calls := withActionStubs(t, store.Bead{Type: "recovery", Status: "awaiting_review"}, nil)

	body := strings.NewReader(`{"question":"x"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/recoveries/spi-abc/comment_request", body)
	req = withRequestIdentity(req, "alice", "alice@example.com")
	req.ContentLength = int64(body.Len())
	rec := httptest.NewRecorder()
	s.handleRecoveryCommentRequest(rec, req, "spi-abc")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	if len(calls.addCommentAs) != 1 {
		t.Fatalf("addCommentAs calls = %d, want 1 (with archmage identity)", len(calls.addCommentAs))
	}
	if calls.addCommentAs[0].author != "alice <alice@example.com>" {
		t.Errorf("comment author = %q, want \"alice <alice@example.com>\"", calls.addCommentAs[0].author)
	}
}

// TestHandleRecoveryByID_DispatchesCommentRequest pins the URL routing.
func TestHandleRecoveryByID_DispatchesCommentRequest(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	calls := withActionStubs(t, store.Bead{Type: "recovery", Status: "awaiting_review"}, nil)

	body := strings.NewReader(`{"question":"halp"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/recoveries/spi-abc/comment_request", body)
	req.ContentLength = int64(body.Len())
	rec := httptest.NewRecorder()
	s.handleRecoveryByID(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	if len(calls.addComment) != 1 {
		t.Errorf("dispatch did not reach handleRecoveryCommentRequest (addComment calls = %d)", len(calls.addComment))
	}
}

func TestHandleRecoveryByID_UnknownActionReturns404(t *testing.T) {
	s := newTestServer(&fakeTrigger{})
	withActionStubs(t, store.Bead{Type: "recovery"}, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/recoveries/spi-abc/bogus", nil)
	rec := httptest.NewRecorder()
	s.handleRecoveryByID(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body=%q)", rec.Code, rec.Body.String())
	}
}
