package store

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/steveyegge/beads"
)

// PrefixFromID extracts the repo prefix from a bead ID (e.g. "oo" from "oo-b9u").
// For hierarchical IDs like "spi-a3f8.1", returns "spi".
func PrefixFromID(id string) string {
	if i := strings.Index(id, "-"); i > 0 {
		return id[:i]
	}
	return ""
}

// CreateBead creates a new bead and returns its ID. Routes through the
// gateway HTTPS API when the active tower is in gateway mode; otherwise
// writes directly to the local Dolt-backed store.
func CreateBead(opts CreateOpts) (string, error) {
	if t, ok := isGatewayMode(); ok {
		return createBeadGateway(t, opts)
	}
	return createBeadDirect(opts)
}

func createBeadDirect(opts CreateOpts) (string, error) {
	s, ctx, err := getStore()
	if err != nil {
		return "", err
	}
	issue := &beads.Issue{
		Title:       opts.Title,
		Description: opts.Description,
		Priority:    opts.Priority,
		Status:      beads.StatusOpen,
		IssueType:   opts.Type,
		Labels:      opts.Labels,
		Ephemeral:   opts.Ephemeral,
	}
	if opts.Prefix != "" {
		issue.PrefixOverride = opts.Prefix
	}
	if len(opts.Metadata) > 0 {
		raw, err := json.Marshal(opts.Metadata)
		if err != nil {
			return "", fmt.Errorf("marshal bead metadata: %w", err)
		}
		issue.Metadata = json.RawMessage(raw)
	}
	if err := s.CreateIssue(ctx, issue, Actor()); err != nil {
		return "", fmt.Errorf("create bead: %w", err)
	}
	// Stamp the canonical filed_at so lifecycle analytics can distinguish
	// queue time from execution time. Best-effort: CreateIssue already
	// committed, and the SQL upsert is idempotent.
	stampFiledBestEffort(issue.ID, string(opts.Type))
	// CreateIssue populates issue.ID
	if opts.Parent != "" {
		dep := &beads.Dependency{
			IssueID:     issue.ID,
			DependsOnID: opts.Parent,
			Type:        beads.DepParentChild,
		}
		if err := s.AddDependency(ctx, dep, Actor()); err != nil {
			return issue.ID, fmt.Errorf("add parent dep for %s: %w", issue.ID, err)
		}
	}
	return issue.ID, nil
}

// AddDep adds a blocking dependency: issueID depends on dependsOnID.
func AddDep(issueID, dependsOnID string) error {
	return AddDepTyped(issueID, dependsOnID, string(beads.DepBlocks))
}

// AddDepTyped adds a dependency with a specific type.
// depType should be one of the beads.Dep* constants (e.g. "discovered-from", "related", "blocks").
//
// Gateway mode: no client method yet — fails closed with ErrGatewayUnsupported.
func AddDepTyped(issueID, dependsOnID, depType string) error {
	if _, ok := isGatewayMode(); ok {
		return gatewayUnsupportedErr("AddDepTyped")
	}
	s, ctx, err := getStore()
	if err != nil {
		return err
	}
	dep := &beads.Dependency{
		IssueID:     issueID,
		DependsOnID: dependsOnID,
		Type:        beads.DependencyType(depType),
	}
	return s.AddDependency(ctx, dep, Actor())
}

// RemoveDep removes a dependency between two beads.
//
// Gateway mode: no client method yet — fails closed with ErrGatewayUnsupported.
func RemoveDep(issueID, dependsOnID string) error {
	if _, ok := isGatewayMode(); ok {
		return gatewayUnsupportedErr("RemoveDep")
	}
	s, ctx, err := getStore()
	if err != nil {
		return err
	}
	return s.RemoveDependency(ctx, issueID, dependsOnID, Actor())
}

// CloseBead closes a bead.
//
// Gateway mode: routes to UpdateBead({"status":"closed"}) so the gateway's
// PATCH /api/v1/beads/{id} endpoint handles the close server-side.
func CloseBead(id string) error {
	if _, ok := isGatewayMode(); ok {
		return UpdateBead(id, map[string]interface{}{"status": "closed"})
	}
	s, ctx, err := getStore()
	if err != nil {
		return err
	}
	if err := s.CloseIssue(ctx, id, "", Actor(), ""); err != nil {
		return err
	}
	stampStatusTransitionBestEffort(id, "closed")
	return nil
}

// DeleteBead permanently deletes a bead and its associated data.
//
// Gateway mode: no client method yet — fails closed with ErrGatewayUnsupported.
// Hard delete is intentionally not exposed through the gateway today; callers
// should prefer CloseBead.
func DeleteBead(id string) error {
	if _, ok := isGatewayMode(); ok {
		return gatewayUnsupportedErr("DeleteBead")
	}
	s, ctx, err := getStore()
	if err != nil {
		return err
	}
	return s.DeleteIssue(ctx, id)
}

// UpdateBead updates a bead's fields. Routes through the gateway HTTPS
// API when the active tower is in gateway mode; otherwise writes directly
// to the local Dolt-backed store.
func UpdateBead(id string, updates map[string]interface{}) error {
	if t, ok := isGatewayMode(); ok {
		return updateBeadGateway(t, id, updates)
	}
	return updateBeadDirect(id, updates)
}

func updateBeadDirect(id string, updates map[string]interface{}) error {
	s, ctx, err := getStore()
	if err != nil {
		return err
	}
	if err := s.UpdateIssue(ctx, id, updates, Actor()); err != nil {
		return err
	}
	// Stamp the matching lifecycle transition when status moves. The SQL
	// upsert is idempotent (first-wins for ready/started, last-wins for
	// closed) so calling this on every update is safe.
	if v, ok := updates["status"]; ok {
		if s, ok := v.(string); ok {
			stampStatusTransitionBestEffort(id, s)
		}
	}
	return nil
}

// AddLabel adds a label to a bead.
//
// Gateway mode: no client method yet — fails closed with ErrGatewayUnsupported.
func AddLabel(id, label string) error {
	if _, ok := isGatewayMode(); ok {
		return gatewayUnsupportedErr("AddLabel")
	}
	s, ctx, err := getStore()
	if err != nil {
		return err
	}
	return s.AddLabel(ctx, id, label, Actor())
}

// RemoveLabel removes a label from a bead.
//
// Gateway mode: no client method yet — fails closed with ErrGatewayUnsupported.
func RemoveLabel(id, label string) error {
	if _, ok := isGatewayMode(); ok {
		return gatewayUnsupportedErr("RemoveLabel")
	}
	s, ctx, err := getStore()
	if err != nil {
		return err
	}
	return s.RemoveLabel(ctx, id, label, Actor())
}

// SetConfig sets a config value.
//
// Gateway mode: no client method yet — fails closed with ErrGatewayUnsupported.
func SetConfig(key, val string) error {
	if _, ok := isGatewayMode(); ok {
		return gatewayUnsupportedErr("SetConfig")
	}
	s, ctx, err := getStore()
	if err != nil {
		return err
	}
	return s.SetConfig(ctx, key, val)
}

// DeleteConfig deletes a config key. Requires ConfigDeleter sub-interface.
//
// Gateway mode: no client method yet — fails closed with ErrGatewayUnsupported.
func DeleteConfig(key string) error {
	if _, ok := isGatewayMode(); ok {
		return gatewayUnsupportedErr("DeleteConfig")
	}
	s, _, err := getStore()
	if err != nil {
		return err
	}
	cd, ok := s.(ConfigDeleter)
	if !ok {
		return fmt.Errorf("store does not support DeleteConfig")
	}
	return cd.DeleteConfig(storeCtx, key)
}

// AddComment adds a comment to a bead.
//
// Gateway mode: no client method yet — fails closed with ErrGatewayUnsupported.
func AddComment(id, text string) error {
	if _, ok := isGatewayMode(); ok {
		return gatewayUnsupportedErr("AddComment")
	}
	s, ctx, err := getStore()
	if err != nil {
		return err
	}
	_, err = s.AddIssueComment(ctx, id, Actor(), text)
	return err
}

// AddCommentReturning adds a comment and returns its ID.
//
// Gateway mode: no client method yet — fails closed with ErrGatewayUnsupported.
func AddCommentReturning(id, text string) (string, error) {
	if _, ok := isGatewayMode(); ok {
		return "", gatewayUnsupportedErr("AddCommentReturning")
	}
	s, ctx, err := getStore()
	if err != nil {
		return "", err
	}
	c, err := s.AddIssueComment(ctx, id, Actor(), text)
	if err != nil {
		return "", err
	}
	if c == nil {
		return "", nil
	}
	return c.ID, nil
}

// AddCommentAs adds a comment authored by the given actor.
//
// Unlike AddComment, which defaults to Actor()="spire" for agent-issued
// comments, AddCommentAs lets the caller supply an explicit author string
// (e.g. "Name <email>" for archmage-authored comments from the CLI).
//
// Gateway mode: no client method yet — fails closed with ErrGatewayUnsupported.
func AddCommentAs(id, author, text string) error {
	if _, ok := isGatewayMode(); ok {
		return gatewayUnsupportedErr("AddCommentAs")
	}
	s, ctx, err := getStore()
	if err != nil {
		return err
	}
	_, err = s.AddIssueComment(ctx, id, author, text)
	return err
}

// CommitPending commits pending dolt changes. Requires PendingCommitter sub-interface.
//
// Gateway mode: dolt commits are server-owned in cluster-as-truth deployments;
// this CLI/steward call is meaningless against a gateway tower. Fails closed
// so steward bubbles the error up rather than silently no-oping.
func CommitPending(message string) error {
	if _, ok := isGatewayMode(); ok {
		return gatewayUnsupportedErr("CommitPending")
	}
	s, _, err := getStore()
	if err != nil {
		return err
	}
	pc, ok := s.(PendingCommitter)
	if !ok {
		return fmt.Errorf("store does not support CommitPending")
	}
	_, err = pc.CommitPending(storeCtx, message)
	return err
}
