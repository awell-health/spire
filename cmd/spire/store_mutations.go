package main

import (
	"fmt"

	"github.com/steveyegge/beads"
)

// storeCreateBead creates a new bead and returns its ID.
func storeCreateBead(opts createOpts) (string, error) {
	store, err := ensureStore()
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
	if err := store.CreateIssue(storeCtx, issue, storeActor()); err != nil {
		return "", fmt.Errorf("create bead: %w", err)
	}
	// CreateIssue populates issue.ID
	if opts.Parent != "" {
		dep := &beads.Dependency{
			IssueID:     issue.ID,
			DependsOnID: opts.Parent,
			Type:        beads.DepParentChild,
		}
		if err := store.AddDependency(storeCtx, dep, storeActor()); err != nil {
			return issue.ID, fmt.Errorf("add parent dep for %s: %w", issue.ID, err)
		}
	}
	return issue.ID, nil
}

// storeAddDep adds a blocking dependency: issueID depends on dependsOnID.
func storeAddDep(issueID, dependsOnID string) error {
	return storeAddDepTyped(issueID, dependsOnID, string(beads.DepBlocks))
}

// storeAddDepTyped adds a dependency with a specific type.
// depType should be one of the beads.Dep* constants (e.g. "discovered-from", "related", "blocks").
func storeAddDepTyped(issueID, dependsOnID, depType string) error {
	store, err := ensureStore()
	if err != nil {
		return err
	}
	dep := &beads.Dependency{
		IssueID:     issueID,
		DependsOnID: dependsOnID,
		Type:        beads.DependencyType(depType),
	}
	return store.AddDependency(storeCtx, dep, storeActor())
}

// storeCloseBead closes a bead.
func storeCloseBead(id string) error {
	store, err := ensureStore()
	if err != nil {
		return err
	}
	return store.CloseIssue(storeCtx, id, "", storeActor(), "")
}

// storeUpdateBead updates a bead's fields.
func storeUpdateBead(id string, updates map[string]interface{}) error {
	store, err := ensureStore()
	if err != nil {
		return err
	}
	return store.UpdateIssue(storeCtx, id, updates, storeActor())
}

// storeAddLabel adds a label to a bead.
func storeAddLabel(id, label string) error {
	store, err := ensureStore()
	if err != nil {
		return err
	}
	return store.AddLabel(storeCtx, id, label, storeActor())
}

// storeRemoveLabel removes a label from a bead.
func storeRemoveLabel(id, label string) error {
	store, err := ensureStore()
	if err != nil {
		return err
	}
	return store.RemoveLabel(storeCtx, id, label, storeActor())
}

// storeSetConfig sets a config value.
func storeSetConfig(key, val string) error {
	store, err := ensureStore()
	if err != nil {
		return err
	}
	return store.SetConfig(storeCtx, key, val)
}

// storeDeleteConfig deletes a config key. Requires configDeleter sub-interface.
func storeDeleteConfig(key string) error {
	store, err := ensureStore()
	if err != nil {
		return err
	}
	cd, ok := store.(configDeleter)
	if !ok {
		return fmt.Errorf("store does not support DeleteConfig")
	}
	return cd.DeleteConfig(storeCtx, key)
}

// storeAddComment adds a comment to a bead.
func storeAddComment(id, text string) error {
	store, err := ensureStore()
	if err != nil {
		return err
	}
	_, err = store.AddIssueComment(storeCtx, id, storeActor(), text)
	return err
}

// storeCommitPending commits pending dolt changes. Requires pendingCommitter sub-interface.
func storeCommitPending(message string) error {
	store, err := ensureStore()
	if err != nil {
		return err
	}
	pc, ok := store.(pendingCommitter)
	if !ok {
		return fmt.Errorf("store does not support CommitPending")
	}
	_, err = pc.CommitPending(storeCtx, message)
	return err
}
