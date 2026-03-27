package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/beads"
)

var (
	activeStore beads.Storage
	storeCtx    context.Context
)

// ensureStore opens a beads store if one isn't already open.
// Uses BEADS_DIR env var or auto-discovers .beads/ directory.
func ensureStore() (beads.Storage, error) {
	if activeStore != nil {
		return activeStore, nil
	}
	beadsDir := resolveBeadsDir()
	if beadsDir == "" {
		return nil, fmt.Errorf("no .beads directory found")
	}
	ctx := context.Background()
	store, err := beads.OpenFromConfig(ctx, beadsDir)
	if err != nil {
		return nil, fmt.Errorf("open beads store: %w", err)
	}
	activeStore = store
	storeCtx = ctx
	return store, nil
}

// openStoreAt opens a beads store at a specific .beads directory.
// Closes any existing store first.
func openStoreAt(beadsDir string) (beads.Storage, error) {
	resetStore()
	ctx := context.Background()
	store, err := beads.OpenFromConfig(ctx, beadsDir)
	if err != nil {
		return nil, fmt.Errorf("open beads store at %s: %w", beadsDir, err)
	}
	activeStore = store
	storeCtx = ctx
	return store, nil
}

// resetStore closes the active store.
func resetStore() {
	if activeStore != nil {
		activeStore.Close()
		activeStore = nil
		storeCtx = nil
	}
}

// storeActor returns the actor identity for store operations.
func storeActor() string {
	return "spire"
}

// --- Conversion helpers ---

// issueToBead converts a beads.Issue to spire's lightweight Bead type.
func issueToBead(issue *beads.Issue) Bead {
	parent := findParentID(issue.Dependencies)
	return Bead{
		ID:          issue.ID,
		Title:       issue.Title,
		Description: issue.Description,
		Status:      string(issue.Status),
		Priority:    issue.Priority,
		Type:        string(issue.IssueType),
		Labels:      issue.Labels,
		Parent:      parent,
	}
}

// issuesToBeads converts a slice of beads.Issue to spire's Bead type.
func issuesToBeads(issues []*beads.Issue) []Bead {
	result := make([]Bead, len(issues))
	for i, issue := range issues {
		result[i] = issueToBead(issue)
	}
	return result
}

// issueToBoardBead converts a beads.Issue to spire's BoardBead type.
func issueToBoardBead(issue *beads.Issue) BoardBead {
	parent := findParentID(issue.Dependencies)
	var deps []BoardDep
	for _, dep := range issue.Dependencies {
		deps = append(deps, BoardDep{
			IssueID:     dep.IssueID,
			DependsOnID: dep.DependsOnID,
			Type:        string(dep.Type),
		})
	}
	return BoardBead{
		ID:           issue.ID,
		Title:        issue.Title,
		Description:  issue.Description,
		Status:       string(issue.Status),
		Priority:     issue.Priority,
		Type:         string(issue.IssueType),
		Owner:        issue.Owner,
		CreatedAt:    issue.CreatedAt.Format(time.RFC3339),
		UpdatedAt:    issue.UpdatedAt.Format(time.RFC3339),
		Labels:       issue.Labels,
		Parent:       parent,
		Dependencies: deps,
	}
}

// issuesToBoardBeads converts a slice of beads.Issue to spire's BoardBead type.
func issuesToBoardBeads(issues []*beads.Issue) []BoardBead {
	result := make([]BoardBead, len(issues))
	for i, issue := range issues {
		result[i] = issueToBoardBead(issue)
	}
	return result
}

// findParentID extracts the parent ID from a dependency list.
func findParentID(deps []*beads.Dependency) string {
	for _, dep := range deps {
		if dep.Type == beads.DepParentChild {
			return dep.DependsOnID
		}
	}
	return ""
}

// --- Filter helpers ---

// statusPtr returns a pointer to a beads.Status value.
func statusPtr(s beads.Status) *beads.Status {
	return &s
}

// issueTypePtr returns a pointer to a beads.IssueType value.
func issueTypePtr(t beads.IssueType) *beads.IssueType {
	return &t
}

// parseStatus converts a status string to a beads.Status.
func parseStatus(s string) beads.Status {
	switch strings.ToLower(s) {
	case "open":
		return beads.StatusOpen
	case "in_progress":
		return beads.StatusInProgress
	case "blocked":
		return beads.StatusBlocked
	case "deferred":
		return beads.StatusDeferred
	case "closed":
		return beads.StatusClosed
	default:
		return beads.StatusOpen
	}
}

// parseIssueType converts a type string to a beads.IssueType.
func parseIssueType(s string) beads.IssueType {
	switch strings.ToLower(s) {
	case "bug":
		return beads.TypeBug
	case "feature":
		return beads.TypeFeature
	case "task":
		return beads.TypeTask
	case "epic":
		return beads.TypeEpic
	case "chore":
		return beads.TypeChore
	case "design":
		return beads.IssueType("design")
	default:
		return beads.TypeTask
	}
}

// --- Local interfaces for sub-interface access ---

// configDeleter provides DeleteConfig for config unset operations.
type configDeleter interface {
	DeleteConfig(ctx context.Context, key string) error
}

// pendingCommitter provides CommitPending for dolt commit operations.
type pendingCommitter interface {
	CommitPending(ctx context.Context, actor string) (bool, error)
}

// --- Create options ---

// createOpts holds parameters for creating a bead via the store.
type createOpts struct {
	Title       string
	Description string
	Priority    int
	Type        beads.IssueType
	Labels      []string
	Parent      string // creates parent-child dep after create
	Prefix      string // sets Issue.PrefixOverride (the --rig equivalent)
}

