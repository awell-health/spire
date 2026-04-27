package gatewayclient

import (
	"context"
	"net/http"
	"net/url"
)

// BeadRecord is the lightweight bead projection the gateway returns from
// /api/v1/beads and /api/v1/beads/{id}. JSON tags match pkg/store.Bead
// so the dispatch layer can reuse the shape 1:1 without re-marshaling.
type BeadRecord struct {
	ID          string            `json:"id"`
	Title       string            `json:"title"`
	Description string            `json:"description"`
	Status      string            `json:"status"`
	Priority    int               `json:"priority"`
	Type        string            `json:"issue_type"`
	Labels      []string          `json:"labels"`
	Parent      string            `json:"parent"`
	UpdatedAt   string            `json:"updated_at"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// ListBeadsFilter is the subset of pkg/store filter fields the gateway's
// GET /api/v1/beads query-string surface accepts. Empty strings are
// omitted so the server default (include every status) applies.
type ListBeadsFilter struct {
	Status string // "open", "ready", "in_progress", "closed", ...
	Label  string // comma-separated labels (AND)
	Prefix string // repo prefix, e.g. "spi"
	Type   string // issue type, e.g. "task", "bug", "epic"
}

// CreateBeadInput matches the POST /api/v1/beads request body. Fields
// correspond to pkg/store.CreateOpts; Prefix pins the repo prefix when
// the tower hosts multiple.
type CreateBeadInput struct {
	Title       string   `json:"title"`
	Type        string   `json:"type,omitempty"`
	Priority    int      `json:"priority,omitempty"`
	Description string   `json:"description,omitempty"`
	Labels      []string `json:"labels,omitempty"`
	Parent      string   `json:"parent,omitempty"`
	Prefix      string   `json:"prefix,omitempty"`
}

// ListBeads calls GET /api/v1/beads with the given filter encoded as
// query params. Mirrors pkg/store.ListBeads in purpose; the returned
// slice may be empty (never nil on success).
func (c *Client) ListBeads(ctx context.Context, filter ListBeadsFilter) ([]BeadRecord, error) {
	q := url.Values{}
	if filter.Status != "" {
		q.Set("status", filter.Status)
	}
	if filter.Label != "" {
		q.Set("label", filter.Label)
	}
	if filter.Prefix != "" {
		q.Set("prefix", filter.Prefix)
	}
	if filter.Type != "" {
		q.Set("type", filter.Type)
	}
	path := "/api/v1/beads"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	var out []BeadRecord
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetBead calls GET /api/v1/beads/{id}. Returns ErrNotFound if the
// gateway responds 404.
func (c *Client) GetBead(ctx context.Context, id string) (BeadRecord, error) {
	var out BeadRecord
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/beads/"+id, nil, &out); err != nil {
		return BeadRecord{}, err
	}
	return out, nil
}

// CreateBead calls POST /api/v1/beads with the given input and returns
// the new bead's ID. Mirrors pkg/store.CreateBead(CreateOpts) (string, error).
func (c *Client) CreateBead(ctx context.Context, in CreateBeadInput) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/beads", in, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// UpdateBead calls PATCH /api/v1/beads/{id} with the given field updates.
// Mirrors pkg/store.UpdateBead(id, updates); the response body is
// discarded because pkg/store.UpdateBead returns only an error.
func (c *Client) UpdateBead(ctx context.Context, id string, updates map[string]any) error {
	return c.doJSON(ctx, http.MethodPatch, "/api/v1/beads/"+id, updates, nil)
}

// CloseBead calls POST /api/v1/beads/{id}/close. Runs the full close
// lifecycle (workflow-step children + label cleanup + caused-by alert
// cascade + parent close) server-side. Returns ErrNotFound if the gateway
// responds 404. The response body is discarded — the lifecycle either
// succeeded or returned an error, the caller doesn't need the post-close
// shape today.
func (c *Client) CloseBead(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodPost, "/api/v1/beads/"+id+"/close", nil, nil)
}

// ResetBeadOpts mirrors the gateway's resetBeadRequest body and the CLI's
// `spire reset` flag set. All fields are optional; the zero value means
// "soft reset, no target step" (the default v0.48 behaviour). To and
// Hard are mutually exclusive — the gateway rejects the combination
// with 400, matching the CLI. Force/Set require To.
type ResetBeadOpts struct {
	To    string            `json:"to,omitempty"`
	Force bool              `json:"force,omitempty"`
	Set   map[string]string `json:"set,omitempty"`
	Hard  bool              `json:"hard,omitempty"`
}

// ResetBead calls POST /api/v1/beads/{id}/reset with the given options
// and returns the post-reset bead so callers can re-render without a
// follow-up GET. Mirrors `spire reset <id> [--to ...] [--force]
// [--set ...] [--hard]` exactly. Returns ErrNotFound on 404; non-2xx
// statuses (400 for validation errors, 409 for state conflicts, 501 for
// cluster mode) surface as *HTTPError with the gateway's error message
// in the body so callers can pattern-match on the canonical strings.
func (c *Client) ResetBead(ctx context.Context, id string, opts ResetBeadOpts) (BeadRecord, error) {
	var out BeadRecord
	// Always send a body so the gateway parses the typed fields, even
	// when all fields are zero-valued — keeps the wire shape uniform
	// across CLI and desktop callers and simplifies server-side
	// observability (no "empty body" branch to special-case).
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/beads/"+id+"/reset", opts, &out); err != nil {
		return BeadRecord{}, err
	}
	return out, nil
}
