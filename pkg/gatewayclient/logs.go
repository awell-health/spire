package gatewayclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// LogArtifactRecord is the JSON projection of one manifest row served on
// the gateway's bead-logs list endpoint. Mirrors pkg/gateway's logArtifactRow
// shape; field names match the wire format so the same struct decodes
// the gateway response unchanged.
type LogArtifactRecord struct {
	ID               string            `json:"id"`
	BeadID           string            `json:"bead_id"`
	AttemptID        string            `json:"attempt_id"`
	RunID            string            `json:"run_id"`
	AgentName        string            `json:"agent_name"`
	Role             string            `json:"role"`
	Phase            string            `json:"phase"`
	Provider         string            `json:"provider,omitempty"`
	Stream           string            `json:"stream"`
	Sequence         int               `json:"sequence"`
	ByteSize         int64             `json:"byte_size"`
	Checksum         string            `json:"checksum,omitempty"`
	Status           string            `json:"status"`
	StartedAt        string            `json:"started_at,omitempty"`
	EndedAt          string            `json:"ended_at,omitempty"`
	CreatedAt        string            `json:"created_at"`
	UpdatedAt        string            `json:"updated_at"`
	RedactionVersion int               `json:"redaction_version"`
	Visibility       string            `json:"visibility"`
	Summary          string            `json:"summary,omitempty"`
	Tail             string            `json:"tail,omitempty"`
	Links            LogArtifactLinks  `json:"links"`
}

// LogArtifactLinks bundles the per-artifact sub-route URLs returned by
// the gateway. Clients follow these rather than re-deriving paths from
// the bead/artifact IDs so the gateway stays free to evolve URL shape.
type LogArtifactLinks struct {
	Raw    string `json:"raw"`
	Pretty string `json:"pretty,omitempty"`
}

// LogsListResponse is the envelope for GET /api/v1/beads/{id}/logs.
// NextCursor is empty when no more rows are available; the cursor is
// opaque base64 — clients pass it back verbatim on the next call.
type LogsListResponse struct {
	Artifacts  []LogArtifactRecord `json:"artifacts"`
	NextCursor string              `json:"next_cursor,omitempty"`
}

// ListBeadLogs calls GET /api/v1/beads/{id}/logs and returns the
// envelope. cursor / limit are optional; pass "" / 0 to use the
// gateway defaults. Returns ErrNotFound when the bead is missing on
// the server side.
func (c *Client) ListBeadLogs(ctx context.Context, beadID, cursor string, limit int) (LogsListResponse, error) {
	q := url.Values{}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/api/v1/beads/" + beadID + "/logs"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	var out LogsListResponse
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &out); err != nil {
		return LogsListResponse{}, err
	}
	return out, nil
}

// ListAllBeadLogs walks the cursor pagination until the gateway
// returns an empty NextCursor, accumulating every artifact for a bead.
// Useful for callers (board inspector, `spire logs pretty`) that need
// the full set rather than a single page. Each round-trip uses the
// gateway's default limit; a concrete maximum is enforced by the
// gateway-side maxLogsListLimit clamp so callers can't accidentally
// blow out the response budget.
func (c *Client) ListAllBeadLogs(ctx context.Context, beadID string) ([]LogArtifactRecord, error) {
	var out []LogArtifactRecord
	cursor := ""
	for {
		page, err := c.ListBeadLogs(ctx, beadID, cursor, 0)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Artifacts...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return out, nil
}

// FetchBeadLogRaw streams the artifact bytes from
// GET /api/v1/beads/{id}/logs/{artifact_id}/raw.
//
// Set asEngineer to true for engineer-scope reads (raw bytes pass
// through the gateway unredacted for engineer_only artifacts); the
// default scope (desktop) is the right answer for the inspector and
// `spire logs pretty`. The gateway honors the X-Spire-Scope header.
//
// Returns ErrNotFound when the bead, artifact, or bytes are unknown
// to the gateway. Other non-2xx statuses surface as *HTTPError so
// callers can pattern-match on the body (the bead-logs handlers
// emit JSON error envelopes).
func (c *Client) FetchBeadLogRaw(ctx context.Context, beadID, artifactID string, asEngineer bool) ([]byte, error) {
	if beadID == "" || artifactID == "" {
		return nil, errors.New("gatewayclient: bead and artifact IDs are required")
	}
	path := fmt.Sprintf("/api/v1/beads/%s/logs/%s/raw", beadID, artifactID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("gatewayclient: build request: %w", err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	c.setIdentityHeaders(req)
	if asEngineer {
		req.Header.Set("X-Spire-Scope", "engineer")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gatewayclient: GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			return nil, ErrUnauthorized
		case http.StatusNotFound, http.StatusGone:
			return nil, ErrNotFound
		}
		return nil, &HTTPError{Status: resp.StatusCode, Body: string(bytes.TrimSpace(bodyBytes))}
	}
	return io.ReadAll(resp.Body)
}
