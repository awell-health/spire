package store

import (
	"fmt"

	"github.com/steveyegge/beads"
)

// CreateBead creates a new bead and returns its ID.
func CreateBead(opts CreateOpts) (string, error) {
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
	}
	if opts.Prefix != "" {
		issue.PrefixOverride = opts.Prefix
	}
	if err := s.CreateIssue(ctx, issue, Actor()); err != nil {
		return "", fmt.Errorf("create bead: %w", err)
	}
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
func AddDepTyped(issueID, dependsOnID, depType string) error {
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
func RemoveDep(issueID, dependsOnID string) error {
	s, ctx, err := getStore()
	if err != nil {
		return err
	}
	return s.RemoveDependency(ctx, issueID, dependsOnID, Actor())
}

// CloseBead closes a bead.
func CloseBead(id string) error {
	s, ctx, err := getStore()
	if err != nil {
		return err
	}
	return s.CloseIssue(ctx, id, "", Actor(), "")
}

// DeleteBead permanently deletes a bead and its associated data.
func DeleteBead(id string) error {
	s, ctx, err := getStore()
	if err != nil {
		return err
	}
	return s.DeleteIssue(ctx, id)
}

// UpdateBead updates a bead's fields.
func UpdateBead(id string, updates map[string]interface{}) error {
	s, ctx, err := getStore()
	if err != nil {
		return err
	}
	return s.UpdateIssue(ctx, id, updates, Actor())
}

// AddLabel adds a label to a bead.
func AddLabel(id, label string) error {
	s, ctx, err := getStore()
	if err != nil {
		return err
	}
	return s.AddLabel(ctx, id, label, Actor())
}

// RemoveLabel removes a label from a bead.
func RemoveLabel(id, label string) error {
	s, ctx, err := getStore()
	if err != nil {
		return err
	}
	return s.RemoveLabel(ctx, id, label, Actor())
}

// SetConfig sets a config value.
func SetConfig(key, val string) error {
	s, ctx, err := getStore()
	if err != nil {
		return err
	}
	return s.SetConfig(ctx, key, val)
}

// DeleteConfig deletes a config key. Requires ConfigDeleter sub-interface.
func DeleteConfig(key string) error {
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
func AddComment(id, text string) error {
	s, ctx, err := getStore()
	if err != nil {
		return err
	}
	_, err = s.AddIssueComment(ctx, id, Actor(), text)
	return err
}

// CommitPending commits pending dolt changes. Requires PendingCommitter sub-interface.
func CommitPending(message string) error {
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
