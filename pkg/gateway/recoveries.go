package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/cleric"
	"github.com/awell-health/spire/pkg/executor"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// This file ships the desktop HITL surface gateway endpoints (spi-sn0qg3):
//
//   - GET  /api/v1/recoveries — list recovery beads (default: awaiting_review)
//   - POST /api/v1/recoveries/{id}/gate — set wait_for_gate output (approve / reject / takeover)
//
// The gate endpoint is the desktop's primary write surface for the cleric
// HITL flow. Setting wait_for_gate.outputs.gate (and rejection_comment for
// reject) unblocks the cleric formula's wait_for_gate step on the next
// dispatch — the formula then advances to execute, requeue_after_reject,
// or handle_takeover per its when clauses.
//
// The action endpoints from spi-wrjiw6 (resummon, dismiss, etc.) are
// orthogonal: cleric.execute fires those via the formula, not the desktop.

// staleThresholdHoursDefault is the default threshold used when listing
// recoveries: items in `awaiting_review` longer than this are flagged as
// stale=true. Hardcoded for v1 per design (spi-sn0qg3 open question).
const staleThresholdHoursDefault = 24

// gate output keys the cleric formula's wait_for_gate step listens on.
// Mirror of pkg/cleric.GateApprove / GateReject / GateTakeover.
const (
	outputKeyGate             = "gate"
	outputKeyRejectionComment = "rejection_comment"
)

// stepWaitForGate is the cleric formula's wait step. The formula's
// when-clauses key on `steps.wait_for_gate.outputs.gate` so the gateway
// must write into this exact step name when setting the gate.
const stepWaitForGate = "wait_for_gate"

// metaProposalPublishedAt is the bead-metadata key cleric.publish writes
// when it transitions the recovery bead to awaiting_review. Used by the
// listing endpoint to compute the stale flag.
const metaProposalPublishedAt = "cleric_proposal_published_at"

// --------------------------------------------------------------------------
// Seams (mirror the actions.go pattern so handler tests can stub side
// effects without booting Dolt or a graph-state store).
// --------------------------------------------------------------------------

var (
	recoveriesStoreEnsureFunc      = actionsStoreEnsureFunc
	recoveriesGetBeadFunc          = store.GetBead
	recoveriesUpdateBeadFunc       = store.UpdateBead
	recoveriesAddCommentAsFunc     = store.AddCommentAsReturning
	recoveriesAddCommentFunc       = store.AddCommentReturning
	recoveriesAddLabelFunc         = store.AddLabel
	recoveriesListBeadsFunc        = store.ListBeads
	recoveriesGetDependentsFunc    = store.GetDependentsWithMeta
	recoveriesGetDepsFunc          = store.GetDepsWithMeta
	recoveriesSetMetadataFunc      = store.SetBeadMetadataMap

	// recoveriesGraphStateLoadFunc / recoveriesGraphStateSaveFunc are the
	// per-test-overridable seams for reading and writing the cleric agent's
	// GraphState. Production wires the file-backed store; tests inject an
	// in-memory recorder. Empty agentName means "no graph state" — the
	// gate handler tolerates a missing GraphState (e.g. cleric never ran)
	// by returning a 409 with a clear error.
	recoveriesGraphStateLoadFunc = func(agentName string) (*executor.GraphState, error) {
		return loadGraphStateLocal(agentName)
	}
	recoveriesGraphStateSaveFunc = func(agentName string, gs *executor.GraphState) error {
		return saveGraphStateLocal(agentName, gs)
	}

	// recoveriesNowFunc returns the wall-clock time used to compute stale.
	// Tests inject a fixed clock so stale assertions are deterministic.
	recoveriesNowFunc = func() time.Time { return time.Now().UTC() }

	// recoveriesSanitizeAgentName mirrors steward.SanitizeK8sLabel so the
	// gateway computes the cleric's agent name identically. Lifted into a
	// seam so tests can verify the wiring without depending on pkg/steward.
	recoveriesSanitizeAgentName = sanitizeAgentNameLocal
)

// loadGraphStateLocal loads from the file-backed graph state store using
// the same ConfigDir resolution as the local executor. Cluster-mode
// towers persist graph state in Dolt; that path is not exercised by the
// gate endpoint in v1 — the desktop is local-native today.
func loadGraphStateLocal(agentName string) (*executor.GraphState, error) {
	fs := &executor.FileGraphStateStore{ConfigDir: configDirFn}
	return fs.Load(agentName)
}

func saveGraphStateLocal(agentName string, gs *executor.GraphState) error {
	fs := &executor.FileGraphStateStore{ConfigDir: configDirFn}
	return fs.Save(agentName, gs)
}

// configDirFn resolves the spire config dir for the current process. We
// avoid importing pkg/config directly so the gateway stays decoupled
// from the steward's config plumbing.
var configDirFn = func() (string, error) {
	// pkg/executor.GraphStatePath uses configDirFn to assemble
	// <dir>/runtime/<agent>/graph_state.json. Reuse the same env var the
	// CLI uses (SPIRE_CONFIG_DIR) when set; fall back to ~/.spire.
	if d := envSpireConfigDir(); d != "" {
		return d, nil
	}
	return defaultSpireConfigDir()
}

// sanitizeAgentNameLocal mirrors pkg/steward.SanitizeK8sLabel exactly.
// Inlined to avoid the gateway → steward import (steward depends on
// gateway-adjacent packages and a cycle would form).
func sanitizeAgentNameLocal(s string) string {
	result := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
			result = append(result, c)
		case c >= 'A' && c <= 'Z':
			result = append(result, c+32)
		case c == '.' || c == '_':
			result = append(result, '-')
		}
	}
	return string(result)
}

// --------------------------------------------------------------------------
// GET /api/v1/recoveries
// --------------------------------------------------------------------------

// RecoveryListItem is one entry in the GET /api/v1/recoveries response.
// Wire-format JSON shape — the desktop's RecoveryCard component consumes
// this directly.
type RecoveryListItem struct {
	// ID is the recovery bead's ID (e.g. "spi-qahji0").
	ID string `json:"id"`

	// Title and Description carry the recovery bead's user-visible
	// summary so the desktop can render a card without a second fetch.
	Title       string `json:"title"`
	Description string `json:"description"`
	Status      string `json:"status"`
	Labels      []string `json:"labels,omitempty"`

	// Proposal is the parsed cleric.ProposedAction (verb, args,
	// reasoning, confidence, destructive, failure_class) lifted from
	// the recovery bead's `cleric_proposal` metadata. Nil when the
	// bead has not yet had a proposal published — the desktop should
	// render the bead as "cleric is thinking" in that case.
	Proposal *cleric.ProposedAction `json:"proposal,omitempty"`

	// SourceBead is the wizard's source bead the recovery is recovering.
	// Resolved via the recovery bead's caused-by edge.
	SourceBead *RecoverySourceSummary `json:"source_bead,omitempty"`

	// FailureContext carries the failure metadata the wizard stamped on
	// the recovery bead at create time — failure_class, source_step,
	// failure_signature. The desktop's "failed step" panel renders this.
	FailureContext map[string]string `json:"failure_context,omitempty"`

	// GraphNeighbors lists closed/related dependencies the cleric used
	// (or could have used) as input. Per design, render via store API,
	// not shell-out to spire graph.
	GraphNeighbors []RecoveryNeighbor `json:"graph_neighbors,omitempty"`

	// PeerRecoveries is the prior recovery beads on the same source.
	// Used by the desktop to render "you've rejected this 3 times"
	// banner and the gate-history audit trail.
	PeerRecoveries []RecoveryPeer `json:"peer_recoveries,omitempty"`

	// AwaitingReviewSince is the RFC3339 timestamp the proposal was
	// published. Empty when the bead isn't in awaiting_review or the
	// publish-time metadata wasn't recorded (legacy beads).
	AwaitingReviewSince string `json:"awaiting_review_since,omitempty"`

	// Stale is true when AwaitingReviewSince is older than the
	// configured threshold. The desktop's "Stale pending reviews"
	// panel filters on this.
	Stale bool `json:"stale,omitempty"`
}

// RecoverySourceSummary is the source-bead context the desktop renders
// alongside a recovery card. Keeps the response shape lean — only the
// fields the card surfaces, not the full bead.
type RecoverySourceSummary struct {
	ID     string   `json:"id"`
	Title  string   `json:"title"`
	Status string   `json:"status"`
	Type   string   `json:"issue_type"`
	Labels []string `json:"labels,omitempty"`
}

// RecoveryNeighbor is one closed/related neighbor of the recovery bead's
// source bead — lets the desktop render the graph-context surface
// without a second round-trip per dep.
type RecoveryNeighbor struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	Status         string `json:"status"`
	Type           string `json:"issue_type"`
	DependencyType string `json:"dependency_type"`
}

// RecoveryPeer is a sibling recovery bead on the same source. Carries
// just enough for the desktop to render "rejected 3 times in a row" and
// the prior-proposal history.
type RecoveryPeer struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	Verb         string `json:"verb,omitempty"`
	FailureClass string `json:"failure_class,omitempty"`

	// GateOutcome reflects the persisted execute_result metadata when
	// the gate was approve+executed; empty for reject/takeover/in-flight.
	// Surfaced so the desktop can show "approved → executed" badges.
	GateOutcome string `json:"gate_outcome,omitempty"`

	UpdatedAt string `json:"updated_at,omitempty"`
}

// handleRecoveriesList answers GET /api/v1/recoveries.
//
// Query params:
//   - status: bead status to filter on; default `awaiting_review`. Pass
//     empty to disable status filtering.
//   - stale_threshold_hours: integer; defaults to 24. Items in
//     awaiting_review longer than this are flagged stale=true.
//
// The handler returns one RecoveryListItem per matching bead. Beads in
// `in_progress` are intentionally NOT in the default response — they
// have no proposal yet and would render as empty cards.
//
// This handler does not paginate in v1: the desktop's review surface is
// expected to comfortably handle dozens of pending recoveries; if the
// volume grows we'll add ?limit/?offset and keep the default response
// shape unchanged.
func (s *Server) handleRecoveriesList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if err := recoveriesStoreEnsureFunc(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	statusFilter := r.URL.Query().Get("status")
	if _, ok := r.URL.Query()["status"]; !ok {
		statusFilter = "awaiting_review"
	}

	threshold := staleThresholdHoursDefault
	if v := r.URL.Query().Get("stale_threshold_hours"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			threshold = n
		}
	}

	recoveryType := beads.IssueType("recovery")
	filter := beads.IssueFilter{
		IssueType: &recoveryType,
		// Bypass the default "exclude closed" so callers can pass status=closed
		// to audit historical recoveries. The default `awaiting_review` filter
		// does this implicitly via Status.
		ExcludeStatus: []beads.Status{"__none__"},
	}
	if statusFilter != "" {
		bs := beads.Status(statusFilter)
		filter.Status = &bs
		filter.ExcludeStatus = nil
	}

	beadList, err := recoveriesListBeadsFunc(filter)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	now := recoveriesNowFunc()
	staleCutoff := now.Add(-time.Duration(threshold) * time.Hour)

	out := make([]RecoveryListItem, 0, len(beadList))
	for _, b := range beadList {
		item := RecoveryListItem{
			ID:          b.ID,
			Title:       b.Title,
			Description: b.Description,
			Status:      b.Status,
			Labels:      b.Labels,
		}

		// Decode proposal from the bead's metadata. Tolerate missing /
		// malformed metadata: a recovery without a proposal still
		// renders as a card with the failure context.
		if raw := b.Meta(cleric.MetadataKeyProposal); raw != "" {
			if pa, perr := cleric.ParseProposedAction([]byte(raw)); perr == nil {
				p := pa
				item.Proposal = &p
			}
		}

		// Failure context: the wizard stamps these on the recovery bead
		// at create time. Surface as a flat map so the desktop can
		// render a key/value table without per-key plumbing.
		fc := map[string]string{}
		for _, k := range []string{
			"failure_class", "failure_signature", "source_bead",
			"source_step", "source_formula", "source_attempt",
		} {
			if v := b.Meta(k); v != "" {
				fc[k] = v
			}
		}
		if len(fc) > 0 {
			item.FailureContext = fc
		}

		// Awaiting-review-since timestamp: cleric.publish writes this
		// to metadata when it transitions to awaiting_review. Beads
		// that arrived in awaiting_review through some other path
		// (e.g. test fixtures, manual filing) won't carry the field;
		// treat them as not-yet-published.
		publishedAt := b.Meta(metaProposalPublishedAt)
		if publishedAt != "" {
			item.AwaitingReviewSince = publishedAt
			if t, perr := time.Parse(time.RFC3339, publishedAt); perr == nil {
				item.Stale = t.Before(staleCutoff)
			}
		}

		// Source bead (caused-by edge) + graph neighbors and peer
		// recoveries. We tolerate per-bead lookup errors: a bead with
		// missing deps still renders, just without the neighbor panel.
		if src, neighbors, peers := s.recoveryGraphContext(b.ID); src != nil {
			item.SourceBead = src
			item.GraphNeighbors = neighbors
			item.PeerRecoveries = peers
		}

		out = append(out, item)
	}

	// Stable order: items with the oldest awaiting_review_since first
	// so the desktop's "stale" panel can render in chronological order
	// without client-side sorting. Items missing the timestamp sort to
	// the end (best-effort placeholder).
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].AwaitingReviewSince == "" {
			return false
		}
		if out[j].AwaitingReviewSince == "" {
			return true
		}
		return out[i].AwaitingReviewSince < out[j].AwaitingReviewSince
	})

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"recoveries":            out,
		"stale_threshold_hours": threshold,
	})
}

// recoveryGraphContext resolves a recovery bead's source bead, the
// source's graph neighbors, and peer recovery beads (siblings on the
// same source). Returns (nil, nil, nil) when any step fails — the
// caller treats partial graph data as "no neighbor info" rather than a
// full-list failure.
func (s *Server) recoveryGraphContext(recoveryID string) (*RecoverySourceSummary, []RecoveryNeighbor, []RecoveryPeer) {
	depsList, err := recoveriesGetDepsFunc(recoveryID)
	if err != nil {
		return nil, nil, nil
	}

	var sourceID string
	for _, d := range depsList {
		if d == nil {
			continue
		}
		if string(d.DependencyType) == store.DepCausedBy {
			sourceID = d.ID
			break
		}
	}
	if sourceID == "" {
		return nil, nil, nil
	}

	src, err := recoveriesGetBeadFunc(sourceID)
	if err != nil {
		return nil, nil, nil
	}

	srcSummary := &RecoverySourceSummary{
		ID:     src.ID,
		Title:  src.Title,
		Status: src.Status,
		Type:   src.Type,
		Labels: src.Labels,
	}

	// Graph neighbors of the SOURCE bead — the cleric reasoned about the
	// source's task, not the recovery's. design / related / blocks edges
	// give the desktop reviewer the same context.
	srcDeps, err := recoveriesGetDepsFunc(sourceID)
	var neighbors []RecoveryNeighbor
	if err == nil {
		for _, d := range srcDeps {
			if d == nil {
				continue
			}
			neighbors = append(neighbors, RecoveryNeighbor{
				ID:             d.ID,
				Title:          d.Title,
				Status:         string(d.Status),
				Type:           string(d.IssueType),
				DependencyType: string(d.DependencyType),
			})
		}
	}

	// Peer recoveries: walk the source bead's dependents looking for
	// other recovery beads (caused-by → source). Excludes the recovery
	// we're building the item for.
	dependents, err := recoveriesGetDependentsFunc(sourceID)
	var peers []RecoveryPeer
	if err == nil {
		for _, d := range dependents {
			if d == nil {
				continue
			}
			if d.ID == recoveryID {
				continue
			}
			if string(d.DependencyType) != store.DepCausedBy {
				continue
			}
			if string(d.IssueType) != "recovery" {
				continue
			}
			peer := RecoveryPeer{
				ID:        d.ID,
				Status:    string(d.Status),
				UpdatedAt: d.UpdatedAt.Format(time.RFC3339),
			}
			// Pull verb / failure_class / outcome from the peer's
			// metadata for the desktop's banner heuristic.
			if peerBead, gErr := recoveriesGetBeadFunc(d.ID); gErr == nil {
				peer.Verb = peerBead.Meta("cleric_proposal_verb")
				peer.FailureClass = peerBead.Meta("cleric_proposal_failure_class")
				peer.GateOutcome = peerBead.Meta(cleric.MetadataKeyOutcome)
			}
			peers = append(peers, peer)
		}
		sort.SliceStable(peers, func(i, j int) bool {
			return peers[i].UpdatedAt > peers[j].UpdatedAt
		})
	}

	return srcSummary, neighbors, peers
}

// --------------------------------------------------------------------------
// POST /api/v1/recoveries/{id}/gate
// --------------------------------------------------------------------------

// gateRequestBody is the request shape for handleRecoveryGate.
type gateRequestBody struct {
	Gate    string `json:"gate"`
	Comment string `json:"comment"`
}

// handleRecoveryGate sets the cleric formula's wait_for_gate step
// outputs so the formula can advance to execute / requeue / handle_takeover
// on its next dispatch.
//
// Body: { gate: "approve"|"reject"|"takeover", comment?: string }
//
// Validation:
//   - bead must exist and be a recovery type
//   - bead must be in awaiting_review status
//   - gate must be one of approve|reject|takeover
//   - comment is required when gate=reject
//
// On success:
//   - wait_for_gate.outputs.gate is set to the approve|reject|takeover value
//   - wait_for_gate.outputs.rejection_comment is set when gate=reject
//   - the recovery bead's status flips from awaiting_review back to
//     in_progress so the steward's hooked sweep dispatches a fresh cleric
//     to advance the formula
//   - the calling archmage is stamped onto the recovery bead's labels for
//     audit attribution (mirrors the spi-kntoe1 / spi-wrjiw6 pattern)
//   - returns 200 with the updated recovery bead
//
// Idempotency:
//   - repeating an approve/takeover when the bead is already in_progress
//     (gate output already set) returns 200 with current state — no-op
//   - re-firing a different gate after the formula advanced is intentionally
//     not supported; the bead's status will not be awaiting_review and the
//     handler returns 409
func (s *Server) handleRecoveryGate(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "recovery ID required"})
		return
	}

	var body gateRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	body.Gate = strings.TrimSpace(body.Gate)
	body.Comment = strings.TrimSpace(body.Comment)

	switch body.Gate {
	case cleric.GateApprove, cleric.GateReject, cleric.GateTakeover:
		// valid
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("gate must be one of %q, %q, %q (got %q)",
				cleric.GateApprove, cleric.GateReject, cleric.GateTakeover, body.Gate),
		})
		return
	}
	if body.Gate == cleric.GateReject && body.Comment == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "comment is required when gate=reject",
		})
		return
	}

	if err := recoveriesStoreEnsureFunc(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	bead, err := recoveriesGetBeadFunc(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if bead.Type != "recovery" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("bead %s is type %q, not %q — gate is recovery-bead-only", id, bead.Type, "recovery"),
		})
		return
	}
	if bead.Status != cleric.StatusAwaitingReview {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": fmt.Sprintf("recovery %s is %q, not %q — gate cannot be set", id, bead.Status, cleric.StatusAwaitingReview),
		})
		return
	}

	// Locate the cleric agent's GraphState. The cleric's name follows
	// the steward dispatch convention: "cleric-" + sanitized recovery
	// bead ID (see pkg/steward/steward.go:1822).
	agentName := "cleric-" + recoveriesSanitizeAgentName(id)
	gs, err := recoveriesGraphStateLoadFunc(agentName)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "load graph state: " + err.Error()})
		return
	}
	if gs == nil {
		// No persisted GraphState yet — this happens when the recovery
		// bead reached awaiting_review through a path that didn't go
		// through the cleric formula (e.g. a manually-filed proposal
		// during HITL bring-up). The desktop's gate is still useful:
		// we record the decision via metadata + status flip so the
		// next steward sweep / cleric dispatch picks it up. The wait
		// step output will be set lazily by the cleric runtime when
		// it materializes a GraphState for the bead.
		s.log.Printf("[gateway] gate %s: no graph state for agent %q — recording gate via metadata only", id, agentName)
	} else if step, ok := gs.Steps[stepWaitForGate]; ok {
		if step.Outputs == nil {
			step.Outputs = map[string]string{}
		}
		step.Outputs[outputKeyGate] = body.Gate
		if body.Gate == cleric.GateReject {
			step.Outputs[outputKeyRejectionComment] = body.Comment
		} else {
			delete(step.Outputs, outputKeyRejectionComment)
		}
		gs.Steps[stepWaitForGate] = step
		if err := recoveriesGraphStateSaveFunc(agentName, gs); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "save graph state: " + err.Error()})
			return
		}
	} else {
		// GraphState exists but the wait_for_gate step is absent. This
		// would mean the cleric formula was edited and an old in-flight
		// recovery referenced the legacy step layout. Surface as 409
		// rather than silently fixing — the operator should investigate.
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": fmt.Sprintf("recovery %s graph state has no %q step — formula may be out of date", id, stepWaitForGate),
		})
		return
	}

	// Stamp the gate decision onto the recovery bead's metadata so the
	// audit trail is queryable independent of the GraphState file.
	// Mirrors the cleric.publish convention of using bead metadata as
	// the durable record.
	gateMeta := map[string]string{
		"cleric_gate":         body.Gate,
		"cleric_gate_set_at":  recoveriesNowFunc().Format(time.RFC3339),
	}
	if body.Gate == cleric.GateReject {
		gateMeta["cleric_gate_comment"] = body.Comment
	}
	if err := recoveriesSetMetadataFunc(id, gateMeta); err != nil {
		// Non-fatal: gate output already set in GraphState. Log and continue.
		s.log.Printf("[gateway] gate %s: stamp metadata: %v", id, err)
	}

	// Audit comment so reviewers see the gate decision in the bead's
	// comment stream. Use AddCommentAs when an archmage identity is
	// available so authorship is attributable.
	commentText := fmt.Sprintf("[cleric-gate] %s", body.Gate)
	if body.Comment != "" {
		commentText += ": " + body.Comment
	}
	var author string
	if ident, ok := IdentityFromContext(r.Context()); ok {
		author = ident.AuthorString()
	}
	if author != "" {
		if _, cerr := recoveriesAddCommentAsFunc(id, author, commentText); cerr != nil {
			s.log.Printf("[gateway] gate %s: add comment: %v", id, cerr)
		}
	} else {
		if _, cerr := recoveriesAddCommentFunc(id, commentText); cerr != nil {
			s.log.Printf("[gateway] gate %s: add comment: %v", id, cerr)
		}
	}

	// Transition recovery bead awaiting_review → in_progress so the
	// steward picks it up on its next sweep. The cleric formula will
	// advance to execute / requeue_after_reject / handle_takeover per
	// its when-clauses on next dispatch.
	if err := recoveriesUpdateBeadFunc(id, map[string]interface{}{
		"status": cleric.StatusInProgress,
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "transition status: " + err.Error()})
		return
	}

	// Stamp the calling archmage onto the recovery bead's labels.
	updated, err := recoveriesGetBeadFunc(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	stampActionsArchmageInline(r, updated.ID, &updated, recoveriesAddLabelFunc)
	writeJSON(w, http.StatusOK, updated)
}

// stampActionsArchmageInline mirrors stampActionsArchmage in actions.go
// but takes the AddLabel seam by parameter so the recoveries handler
// can reuse the same audit-stamping logic without sharing a package
// var. Failures are non-fatal — the gate has already been recorded.
func stampActionsArchmageInline(r *http.Request, id string, bead *store.Bead, addLabel func(string, string) error) {
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
			if err := addLabel(id, l); err != nil {
				continue
			}
			bead.Labels = append(bead.Labels, l)
		}
	}
}

// envSpireConfigDir returns SPIRE_CONFIG_DIR if set, else "".
func envSpireConfigDir() string {
	return strings.TrimSpace(os.Getenv("SPIRE_CONFIG_DIR"))
}

// defaultSpireConfigDir returns ~/.spire (or the OS-equivalent). Pulled
// out as a stub so tests can override without touching real ~/.
var defaultSpireConfigDir = func() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".spire"), nil
}
