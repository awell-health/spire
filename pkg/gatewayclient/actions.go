package gatewayclient

import (
	"context"
	"net/http"
)

// ActionManifest mirrors pkg/cleric.ActionManifest as the wire-shape the
// gateway returns from GET /api/v1/actions. We don't import pkg/cleric
// directly so this client can be consumed by callers that don't already
// pull in the cleric package.
type ActionManifest struct {
	Name         string `json:"name"`
	ArgsSchema   any    `json:"args_schema"`
	Destructive  bool   `json:"destructive"`
	EndpointPath string `json:"endpoint_path"`
	Description  string `json:"description"`
}

// ActionsResponse is the GET /api/v1/actions envelope.
type ActionsResponse struct {
	Actions []ActionManifest `json:"actions"`
}

// GetActions calls GET /api/v1/actions and returns the v1 cleric action
// catalog the desktop renders as a HITL dropdown. Read-only.
func (c *Client) GetActions(ctx context.Context) (ActionsResponse, error) {
	var out ActionsResponse
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/actions", nil, &out); err != nil {
		return ActionsResponse{}, err
	}
	return out, nil
}

// ResummonBead calls POST /api/v1/beads/{id}/resummon. Returns the
// post-action bead state. Idempotent: if the bead is already in_progress
// the gateway returns the current state without spawning a second wizard.
// Returns ErrNotFound on 404; non-2xx surfaces as *HTTPError so callers
// can pattern-match on the canonical messages (e.g. "is closed", "is not
// hooked").
func (c *Client) ResummonBead(ctx context.Context, id string) (BeadRecord, error) {
	var out BeadRecord
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/beads/"+id+"/resummon", struct{}{}, &out); err != nil {
		return BeadRecord{}, err
	}
	return out, nil
}

// DismissBead calls POST /api/v1/beads/{id}/dismiss. The gateway cleans
// the worktree, branch, and graph state; closes the bead with a
// `closed_reason:dismissed` label; and returns the post-close bead.
// Idempotent on already-closed beads.
func (c *Client) DismissBead(ctx context.Context, id string) (BeadRecord, error) {
	var out BeadRecord
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/beads/"+id+"/dismiss", struct{}{}, &out); err != nil {
		return BeadRecord{}, err
	}
	return out, nil
}

// UpdateBeadStatusOpts is the body for UpdateBeadStatus.
type UpdateBeadStatusOpts struct {
	To string `json:"to"`
}

// UpdateBeadStatus calls POST /api/v1/beads/{id}/update_status with the
// target status. The gateway enforces the server-side whitelist of valid
// {from, to} transitions; non-whitelisted moves return 400 with a list
// of allowed targets.
func (c *Client) UpdateBeadStatus(ctx context.Context, id string, opts UpdateBeadStatusOpts) (BeadRecord, error) {
	var out BeadRecord
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/beads/"+id+"/update_status", opts, &out); err != nil {
		return BeadRecord{}, err
	}
	return out, nil
}

// ResetHardBead calls POST /api/v1/beads/{id}/reset_hard. The gateway
// nukes the worktree, branch, and graph state; closes attempt/review
// beads with the reset-cycle tag (logs survive). Returns 501 in
// cluster-mode; the local-mode path is the v1 supported topology.
func (c *Client) ResetHardBead(ctx context.Context, id string) (BeadRecord, error) {
	var out BeadRecord
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/beads/"+id+"/reset_hard", struct{}{}, &out); err != nil {
		return BeadRecord{}, err
	}
	return out, nil
}

// CommentRequestOpts is the body for RecoveryCommentRequest.
type CommentRequestOpts struct {
	Question string `json:"question"`
}

// CommentRequestResponse is the envelope returned by
// POST /api/v1/recoveries/{id}/comment_request: the post-action bead
// plus the new comment's ID.
type CommentRequestResponse struct {
	ID        string     `json:"id"`
	Bead      BeadRecord `json:"bead"`
	CommentID string     `json:"comment_id"`
}

// RecoveryCommentRequest calls
// POST /api/v1/recoveries/{id}/comment_request with the given question.
// The gateway writes a labeled question comment on the recovery bead and
// stamps the `cleric-request-input` label so the steward / desktop can
// surface pending-question recoveries. The recovery bead's status is
// unchanged.
//
// Recovery-bead-only: the gateway rejects non-recovery target types with
// 400 (surfaces as *HTTPError).
func (c *Client) RecoveryCommentRequest(ctx context.Context, id string, opts CommentRequestOpts) (CommentRequestResponse, error) {
	var out CommentRequestResponse
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/recoveries/"+id+"/comment_request", opts, &out); err != nil {
		return CommentRequestResponse{}, err
	}
	return out, nil
}
