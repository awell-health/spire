package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/awell-health/spire/pkg/cleric"
	pkgclose "github.com/awell-health/spire/pkg/close"
	"github.com/awell-health/spire/pkg/reset"
	"github.com/awell-health/spire/pkg/store"
	"github.com/awell-health/spire/pkg/summon"
)

// This file ships the v1 cleric action catalog endpoints (spi-wrjiw6):
//
//   - GET  /api/v1/actions
//   - POST /api/v1/beads/{id}/resummon
//   - POST /api/v1/beads/{id}/dismiss
//   - POST /api/v1/beads/{id}/update_status
//   - POST /api/v1/beads/{id}/reset_hard
//   - POST /api/v1/recoveries/{id}/comment_request
//
// Each handler follows the spi-kntoe1 pattern shipped by handleBeadReset:
// JSON body, archmage identity stamping via appendArchmageLabels, idempotent
// re-fire (returns 200 with current state when already-in-target-state),
// and 4xx for validation errors.

// --------------------------------------------------------------------------
// Seams (mirror the existing reset / summon / ready pattern so handler tests
// can stub the side effects without booting a real Dolt store).
// --------------------------------------------------------------------------

var (
	actionsStoreEnsureFunc = func(dir string) error {
		_, err := store.Ensure(dir)
		return err
	}
	actionsGetBeadFunc    = store.GetBead
	actionsUpdateBeadFunc = store.UpdateBead
	actionsAddLabelFunc   = store.AddLabel
	actionsAddCommentFunc = store.AddCommentReturning
	actionsAddCommentAsFunc = store.AddCommentAsReturning

	// resummonRunFunc routes to summon.Run so the existing wizard-spawn
	// flow handles the actual transition + spawn. summon.Run accepts
	// hooked beads and transitions them to in_progress, which is what
	// resummon needs.
	resummonRunFunc = summon.Run

	// dismissCloseFunc closes the bead through the canonical lifecycle
	// (workflow-step children + label cleanup + cascade). Wired to
	// pkg/close.RunLifecycle in production; tests swap it for a recorder.
	dismissCloseFunc = pkgclose.RunLifecycle

	// dismissResetHardFunc cleans worktree+branch+graph state by
	// delegating to the existing soft-reset surface with Hard=true.
	// Reused for reset_hard so both endpoints share the same destructive
	// core (and therefore stamp the same audit footprint).
	dismissResetHardFunc = func(ctx context.Context, beadID string) error {
		_, err := reset.ResetBead(ctx, reset.Opts{BeadID: beadID, Hard: true})
		return err
	}
)

// --------------------------------------------------------------------------
// GET /api/v1/actions — manifest
// --------------------------------------------------------------------------

// handleActionsManifest answers GET /api/v1/actions with the v1 cleric
// action catalog. Read-only. Auth is whatever bearerAuth already enforces;
// the manifest itself is not sensitive (it's the public list of verbs the
// desktop's HITL dropdown renders).
func (s *Server) handleActionsManifest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"actions": cleric.V1Actions,
	})
}

// --------------------------------------------------------------------------
// POST /api/v1/beads/{id}/resummon
// --------------------------------------------------------------------------

// handleBeadResummon re-summons the wizard for a hooked bead. If the bead
// is already in_progress (live wizard owner presumed), returns 200 with
// the current state — idempotent. If the bead is in any other status,
// returns 4xx with a clear message so the human knows resummon doesn't
// apply. Mirrors the design's "Bead should already be in `hooked` state
// (otherwise the wizard is presumed alive)" rule from spi-1s5w0o.
func (s *Server) handleBeadResummon(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bead ID required"})
		return
	}
	if err := actionsStoreEnsureFunc(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	bead, err := actionsGetBeadFunc(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}

	switch bead.Status {
	case "hooked":
		// Proceed.
	case "in_progress":
		// Idempotent: a wizard is already running. Return current state
		// rather than spawning a second one. The cleric's verify is
		// implicit (next wizard run), so re-firing should be safe.
		stampActionsArchmage(r, bead.ID, &bead)
		writeJSON(w, http.StatusOK, bead)
		return
	case "closed":
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("bead %s is closed — nothing to resummon", id)})
		return
	default:
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": fmt.Sprintf("bead %s is %q, not hooked — wizard is presumed alive (resummon requires hooked)", id, bead.Status),
		})
		return
	}

	// summon.Run handles the hooked → in_progress transition + wizard
	// spawn. Errors map through statusForSummonError for parity with the
	// /summon endpoint.
	if _, err := resummonRunFunc(id, ""); err != nil {
		writeJSON(w, statusForSummonError(err), map[string]string{"error": err.Error()})
		return
	}

	// Re-fetch so the response reflects the post-transition state and
	// any labels (including the archmage stamp) are current.
	updated, err := actionsGetBeadFunc(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	stampActionsArchmage(r, updated.ID, &updated)
	writeJSON(w, http.StatusOK, updated)
}

// --------------------------------------------------------------------------
// POST /api/v1/beads/{id}/dismiss
// --------------------------------------------------------------------------

// handleBeadDismiss cancels the bead's work entirely: cleans worktree +
// branch + graph state, then closes the bead with a `closed_reason:dismissed`
// label so downstream observability can distinguish dismissal from a clean
// merge close.
//
// Idempotent: if the bead is already closed, returns 200 with the current
// state and stamps the archmage label so re-fires are still attributed.
func (s *Server) handleBeadDismiss(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bead ID required"})
		return
	}
	if err := actionsStoreEnsureFunc(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	bead, err := actionsGetBeadFunc(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}

	if bead.Status == "closed" {
		// Already dismissed. Stamp identity (idempotent) and return.
		stampActionsArchmage(r, bead.ID, &bead)
		writeJSON(w, http.StatusOK, bead)
		return
	}

	// Step 1: clean worktree + branch + graph state by running a hard reset.
	// This goes through pkg/reset → cmd/spire's runResetCore so the same
	// kill-wizard / strip-labels / unhook / worktree-teardown flow runs as
	// the CLI's `spire reset --hard`.
	if err := dismissResetHardFunc(r.Context(), id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": fmt.Sprintf("dismiss %s: clean worktree/branch: %v", id, err),
		})
		return
	}

	// Step 2: stamp the dismissal reason BEFORE closing so the label
	// survives close-cascades.
	if err := actionsAddLabelFunc(id, "closed_reason:dismissed"); err != nil {
		// Non-fatal: the worktree is gone; the close still has to fire.
		s.log.Printf("[gateway] dismiss %s: stamp closed_reason label: %v", id, err)
	}

	// Step 3: run the close lifecycle. RunLifecycle handles workflow-step
	// children, alert cascade, and the actual close. Idempotent on
	// already-closed parents.
	if err := dismissCloseFunc(id); err != nil {
		writeJSON(w, statusForCloseError(err), map[string]string{"error": err.Error()})
		return
	}

	// Step 4: re-read so the response reflects closed status + the new
	// labels.
	updated, err := actionsGetBeadFunc(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	stampActionsArchmage(r, updated.ID, &updated)
	writeJSON(w, http.StatusOK, updated)
}

// --------------------------------------------------------------------------
// POST /api/v1/beads/{id}/update_status
// --------------------------------------------------------------------------

// updateStatusBody is the request body for handleBeadUpdateStatus.
type updateStatusBody struct {
	To string `json:"to"`
}

// handleBeadUpdateStatus transitions a bead's status. Server-side
// whitelist enforced via cleric.IsValidStatusTransition — non-whitelisted
// {from, to} pairs return 400 with the canonical message naming the
// allowed transitions, so the desktop can render a useful error.
//
// Idempotent: from == to returns 200 with current state (no UpdateBead
// call).
func (s *Server) handleBeadUpdateStatus(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bead ID required"})
		return
	}

	var body updateStatusBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	body.To = strings.TrimSpace(body.To)
	if body.To == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "field \"to\" is required"})
		return
	}

	if err := actionsStoreEnsureFunc(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	bead, err := actionsGetBeadFunc(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}

	if !cleric.IsValidStatusTransition(bead.Status, body.To) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("invalid status transition %q → %q (allowed from %q: %s)",
				bead.Status, body.To, bead.Status, allowedToList(bead.Status)),
		})
		return
	}

	// Idempotent self-transition: skip the UpdateBead call but still
	// stamp the archmage label so the audit trail records the attempt.
	if bead.Status == body.To {
		stampActionsArchmage(r, bead.ID, &bead)
		writeJSON(w, http.StatusOK, bead)
		return
	}

	if err := actionsUpdateBeadFunc(id, map[string]interface{}{"status": body.To}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	updated, err := actionsGetBeadFunc(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	stampActionsArchmage(r, updated.ID, &updated)
	writeJSON(w, http.StatusOK, updated)
}

// allowedToList renders the whitelist's allowed targets for `from` as a
// comma-separated string for the 400 response. Empty when no transitions
// are allowed.
func allowedToList(from string) string {
	allowed, ok := cleric.UpdateStatusTransitions[from]
	if !ok {
		return "<none>"
	}
	keys := make([]string, 0, len(allowed))
	for k := range allowed {
		keys = append(keys, k)
	}
	// Stable ordering for the error message.
	sortStringsAscending(keys)
	return strings.Join(keys, ", ")
}

// sortStringsAscending is a thin wrapper so we don't pull sort into the
// top-level imports just for this one call site. Stable, ascending —
// mutates in place.
func sortStringsAscending(xs []string) {
	// Insertion sort: the lists are tiny (<10 items each).
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}

// --------------------------------------------------------------------------
// POST /api/v1/beads/{id}/reset_hard
// --------------------------------------------------------------------------

// handleBeadResetHard runs the destructive reset path — worktree, branch,
// graph state, all gone. attempt/review beads are closed with a reset-cycle
// tag so logs survive (per existing CLI semantics in cmd/spire/reset.go).
//
// This is a thin wrapper over the existing reset surface with Hard=true.
// We expose it as a separate endpoint so the cleric manifest has one
// verb-per-action and the desktop dropdown can render reset_hard
// alongside resummon / dismiss without parsing query args.
//
// Idempotent: if the bead's worktree/branch/graph state are already gone,
// the underlying reset is a no-op.
func (s *Server) handleBeadResetHard(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bead ID required"})
		return
	}

	// Cluster-mode short-circuit, mirroring handleBeadReset. The cluster
	// gateway can't reach the wizard pod directly to clean a worktree
	// volume.
	mode, err := resetTowerModeFunc()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if mode == "gateway" {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "reset not supported in cluster mode yet — see follow-up bead",
		})
		return
	}

	if err := actionsStoreEnsureFunc(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	if _, err := actionsGetBeadFunc(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}

	if err := dismissResetHardFunc(r.Context(), id); err != nil {
		writeJSON(w, statusForResetError(err), map[string]string{"error": err.Error()})
		return
	}

	updated, err := actionsGetBeadFunc(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	stampActionsArchmage(r, updated.ID, &updated)
	writeJSON(w, http.StatusOK, updated)
}

// --------------------------------------------------------------------------
// POST /api/v1/recoveries/{id}/comment_request
// --------------------------------------------------------------------------

// commentRequestBody is the request body for handleRecoveryCommentRequest.
type commentRequestBody struct {
	Question string `json:"question"`
}

// handleRecoveryCommentRequest writes a labeled question comment on a
// recovery bead. The cleric uses this when it doesn't know what to do —
// rather than guessing an action, it asks the human for input. The
// recovery bead's status is unchanged (stays `awaiting_review`); the
// human responds via comment reply, and the next cleric round reads the
// reply via the existing graph-context plumbing.
//
// The comment is stamped with the `cleric-request-input` label so the
// next cleric prompt-builder can extract it as additional structured
// input. Per design (spi-wrjiw6 open question), human replies get the
// `cleric-request-reply` label — that's a desktop-side concern, not
// this handler's.
//
// Recovery-bead-only: this verb has no meaning on a source bead, so the
// handler rejects non-recovery target types with 400.
//
// Idempotent: re-firing with the same question writes a second comment
// (intentional — the cleric may want to ask multiple things across
// multiple rounds). Callers wanting strict idempotency should dedupe at
// the source.
func (s *Server) handleRecoveryCommentRequest(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "recovery ID required"})
		return
	}

	var body commentRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	body.Question = strings.TrimSpace(body.Question)
	if body.Question == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "field \"question\" is required and must be non-empty"})
		return
	}

	if err := actionsStoreEnsureFunc(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	bead, err := actionsGetBeadFunc(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}

	if bead.Type != "recovery" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("bead %s is type %q, not %q — comment_request is recovery-bead-only", id, bead.Type, "recovery"),
		})
		return
	}

	// Compose the comment text with the structured prefix so the next
	// cleric prompt-builder can extract it as an "additional input"
	// section.
	text := "[cleric-request-input] " + body.Question

	// Use the calling archmage as the comment author so the audit trail
	// attributes the question. Cleric agents authenticate as the
	// tower's archmage by default; humans firing directly via desktop
	// stamp themselves.
	var author string
	if ident, ok := IdentityFromContext(r.Context()); ok {
		author = ident.AuthorString()
	}

	var (
		commentID string
		cerr      error
	)
	if author != "" {
		commentID, cerr = actionsAddCommentAsFunc(id, author, text)
	} else {
		commentID, cerr = actionsAddCommentFunc(id, text)
	}
	if cerr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": cerr.Error()})
		return
	}

	// Stamp the recovery bead itself with the `cleric-request-input`
	// label so the steward / desktop can quickly find pending-question
	// recoveries without scanning every comment.
	if err := actionsAddLabelFunc(id, "cleric-request-input"); err != nil {
		// Non-fatal: the comment is the durable signal; the label is a
		// convenience index.
		s.log.Printf("[gateway] comment_request %s: stamp label: %v", id, err)
	}

	updated, err := actionsGetBeadFunc(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	stampActionsArchmage(r, updated.ID, &updated)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":         id,
		"bead":       updated,
		"comment_id": commentID,
	})
}

// --------------------------------------------------------------------------
// /api/v1/recoveries/{id} dispatcher
// --------------------------------------------------------------------------

// handleRecoveryByID routes the recovery-scoped sub-routes. The only verb
// here today is comment_request; future cleric-side actions can land
// alongside it.
func (s *Server) handleRecoveryByID(w http.ResponseWriter, r *http.Request) {
	rest := pathSuffix(r.URL.Path, "/api/v1/recoveries/")
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "recovery ID required"})
		return
	}
	switch action {
	case "comment_request":
		s.handleRecoveryCommentRequest(w, r, id)
	case "gate":
		s.handleRecoveryGate(w, r, id)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// --------------------------------------------------------------------------
// stampActionsArchmage stamps the calling archmage's labels onto bead so
// the audit trail attributes the action. Reuses the existing
// appendArchmageLabels seam so behaviour matches createBead /
// handleBeadReset.
//
// Failures from the underlying AddLabel are non-fatal: the action itself
// has already succeeded, and a partial-stamp shouldn't undo a good
// result. This mirrors the same trade-off in handleBeadReset.
// --------------------------------------------------------------------------

func stampActionsArchmage(r *http.Request, id string, bead *store.Bead) {
	if bead == nil {
		return
	}
	ident, ok := IdentityFromContext(r.Context())
	if !ok || ident.Name == "" {
		return
	}
	stamped := appendArchmageLabels(bead.Labels, ident)
	for _, l := range stamped {
		if !containsLabel(bead.Labels, l) {
			if err := actionsAddLabelFunc(id, l); err != nil {
				// Best-effort audit; the action has succeeded.
				continue
			}
			bead.Labels = append(bead.Labels, l)
		}
	}
}

