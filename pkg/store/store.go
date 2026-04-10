// Package store provides bead persistence: types, queries, mutations, and
// bead subtype helpers (attempts, steps, review rounds). It wraps the beads
// library with Spire-specific semantics. pkg/store has no dependencies on
// other Spire packages — cross-package wiring uses callback variables
// (e.g. BeadsDirResolver) set by cmd/spire at init time.
package store

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

// Ensure opens a beads store at beadsDir if one isn't already open.
// The caller is responsible for resolving beadsDir (e.g. via resolveBeadsDir).
func Ensure(beadsDir string) (beads.Storage, error) {
	if activeStore != nil {
		return activeStore, nil
	}
	if beadsDir == "" {
		return nil, fmt.Errorf("no .beads directory found")
	}
	ctx := context.Background()
	s, err := beads.OpenFromConfig(ctx, beadsDir)
	if err != nil {
		return nil, fmt.Errorf("open beads store: %w", err)
	}
	activeStore = s
	storeCtx = ctx
	return s, nil
}

// OpenAt opens a beads store at a specific .beads directory.
// Closes any existing store first.
func OpenAt(beadsDir string) (beads.Storage, error) {
	Reset()
	ctx := context.Background()
	s, err := beads.OpenFromConfig(ctx, beadsDir)
	if err != nil {
		return nil, fmt.Errorf("open beads store at %s: %w", beadsDir, err)
	}
	activeStore = s
	storeCtx = ctx
	return s, nil
}

// Open returns a fresh, independent beads.Storage connection to the dolt
// database at beadsDir. Unlike Ensure/OpenAt it never touches the
// package-level singleton — callers own the returned connection's lifecycle.
// beadsDir must be non-empty; Open does not fall back to env vars or defaults.
func Open(beadsDir string) (beads.Storage, error) {
	if beadsDir == "" {
		return nil, fmt.Errorf("store.Open: beadsDir must not be empty")
	}
	ctx := context.Background()
	s, err := beads.OpenFromConfig(ctx, beadsDir)
	if err != nil {
		return nil, fmt.Errorf("store.Open: %w", err)
	}
	return s, nil
}

// Reset closes the active store.
func Reset() {
	if activeStore != nil {
		activeStore.Close()
		activeStore = nil
		storeCtx = nil
	}
}

// Actor returns the actor identity for store operations.
func Actor() string {
	return "spire"
}

// --- Conversion helpers ---

// IssueToBead converts a beads.Issue to the lightweight Bead type.
func IssueToBead(issue *beads.Issue) Bead {
	parent := FindParentID(issue.Dependencies)
	return Bead{
		ID:          issue.ID,
		Title:       issue.Title,
		Description: issue.Description,
		Status:      string(issue.Status),
		Priority:    issue.Priority,
		Type:        string(issue.IssueType),
		Labels:      issue.Labels,
		Parent:      parent,
		UpdatedAt:   issue.UpdatedAt.Format(time.RFC3339),
		Metadata:    metadataFromJSON(issue.Metadata),
	}
}

// IssuesToBeads converts a slice of beads.Issue to the Bead type.
func IssuesToBeads(issues []*beads.Issue) []Bead {
	result := make([]Bead, len(issues))
	for i, issue := range issues {
		result[i] = IssueToBead(issue)
	}
	return result
}

// IssueToBoardBead converts a beads.Issue to the BoardBead type.
func IssueToBoardBead(issue *beads.Issue) BoardBead {
	parent := FindParentID(issue.Dependencies)
	var deps []BoardDep
	for _, dep := range issue.Dependencies {
		deps = append(deps, BoardDep{
			IssueID:     dep.IssueID,
			DependsOnID: dep.DependsOnID,
			Type:        string(dep.Type),
		})
	}
	var closedAt string
	if issue.ClosedAt != nil {
		closedAt = issue.ClosedAt.Format(time.RFC3339)
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
		ClosedAt:     closedAt,
		Labels:       issue.Labels,
		Parent:       parent,
		Dependencies: deps,
		Metadata:     metadataFromJSON(issue.Metadata),
	}
}

// IssuesToBoardBeads converts a slice of beads.Issue to the BoardBead type.
func IssuesToBoardBeads(issues []*beads.Issue) []BoardBead {
	result := make([]BoardBead, len(issues))
	for i, issue := range issues {
		result[i] = IssueToBoardBead(issue)
	}
	return result
}

// FindParentID extracts the parent ID from a dependency list.
func FindParentID(deps []*beads.Dependency) string {
	for _, dep := range deps {
		if dep.Type == beads.DepParentChild {
			return dep.DependsOnID
		}
	}
	return ""
}

// --- Filter helpers ---

// StatusPtr returns a pointer to a beads.Status value.
func StatusPtr(s beads.Status) *beads.Status {
	return &s
}

// IssueTypePtr returns a pointer to a beads.IssueType value.
func IssueTypePtr(t beads.IssueType) *beads.IssueType {
	return &t
}

// ParseStatus converts a status string to a beads.Status.
func ParseStatus(s string) beads.Status {
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

// ParseIssueType converts a type string to a beads.IssueType.
func ParseIssueType(s string) beads.IssueType {
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
	case "recovery":
		return beads.IssueType("recovery")
	case "message":
		return beads.IssueType("message")
	case "step":
		return beads.IssueType("step")
	case "attempt":
		return beads.IssueType("attempt")
	case "review":
		return beads.IssueType("review")
	default:
		return beads.TypeTask
	}
}

// --- Local interfaces for sub-interface access ---

// ConfigDeleter provides DeleteConfig for config unset operations.
type ConfigDeleter interface {
	DeleteConfig(ctx context.Context, key string) error
}

// PendingCommitter provides CommitPending for dolt commit operations.
type PendingCommitter interface {
	CommitPending(ctx context.Context, actor string) (bool, error)
}

// --- Create options ---

// CreateOpts holds parameters for creating a bead via the store.
type CreateOpts struct {
	Title       string
	Description string
	Priority    int
	Type        beads.IssueType
	Labels      []string
	Parent      string // creates parent-child dep after create
	Prefix      string // sets Issue.PrefixOverride (the --rig equivalent)
}

// BeadsDirResolver is a function that resolves the .beads directory path.
// Set this from the main package so pkg/store can auto-initialize on first use.
var BeadsDirResolver func() string

// getStore returns the active store, auto-initializing via BeadsDirResolver
// if no store is open and a resolver has been set.
func getStore() (beads.Storage, context.Context, error) {
	if activeStore == nil && BeadsDirResolver != nil {
		if _, err := Ensure(BeadsDirResolver()); err != nil {
			return nil, nil, err
		}
	}
	if activeStore == nil {
		return nil, nil, fmt.Errorf("store not initialized — call Ensure() first")
	}
	return activeStore, storeCtx, nil
}
