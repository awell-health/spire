package gatewayclient

import (
	"context"
	"net/http"
)

// DepRecord is one edge of a bead's dependency graph as returned by
// GET /api/v1/beads/{id}/deps. JSON tags match pkg/store.BoardDep so
// the dispatch layer can translate without re-marshaling.
type DepRecord struct {
	IssueID     string `json:"issue_id"`
	DependsOnID string `json:"depends_on_id"`
	Type        string `json:"type"`
}

// BlockedIssues is the response shape of GET /api/v1/beads/blocked: the
// set of open bead IDs with unresolved blockers. The gateway returns
// only IDs (full BoardBead hydration happens client-side), so the
// dispatch layer must re-fetch bead bodies if it needs more than IDs.
type BlockedIssues struct {
	Count int      `json:"count"`
	IDs   []string `json:"ids"`
}

// ListDeps calls GET /api/v1/beads/{id}/deps and returns every
// dependency edge where the bead is the dependent side (i.e. what the
// bead depends on, across every dep type — parent-child, blocks,
// discovered-from, etc.). Mirrors pkg/store.GetDepsWithMeta in purpose.
func (c *Client) ListDeps(ctx context.Context, id string) ([]DepRecord, error) {
	var out []DepRecord
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/beads/"+id+"/deps", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetBlockedIssues calls GET /api/v1/beads/blocked and returns the
// current set of blocked bead IDs. Mirrors pkg/store.GetBlockedIssues
// in purpose; the gateway endpoint accepts no filter today, so callers
// that need WorkFilter semantics must post-filter on the result.
func (c *Client) GetBlockedIssues(ctx context.Context) (BlockedIssues, error) {
	var out BlockedIssues
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/beads/blocked", nil, &out); err != nil {
		return BlockedIssues{}, err
	}
	return out, nil
}
