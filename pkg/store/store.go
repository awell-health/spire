// Package store provides bead persistence: types, queries, mutations, and
// bead subtype helpers (attempts, steps, review rounds). It wraps the beads
// library with Spire-specific semantics.
//
// Public bead/message/dep functions dispatch on the active tower's mode
// (see pkg/config.TowerConfig.IsGateway): gateway-mode towers route through
// pkg/gatewayclient over HTTPS; direct-mode towers use the embedded Dolt
// path. The mode branch is centralized in dispatch.go; every public entry
// point (GetBead, ListBeads, CreateBead, UpdateBead, ListMessages,
// SendMessage, MarkMessageRead, ListDeps, GetBlockedIssues) preserves its
// pre-dispatch signature so cmd/spire callers are unchanged.
//
// pkg/store depends on pkg/config (tower resolution, keychain) and
// pkg/gatewayclient (HTTPS transport) for the dispatch surface; legacy
// cross-package wiring still uses callback variables like BeadsDirResolver
// that cmd/spire populates at init time.
package store

import (
	"context"
	"fmt"
	"log"
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

// --- Dependency population ---

// depBatchFetcher is satisfied by dolt stores that support bulk dependency
// fetching (type-asserted from beads.Storage).
type depBatchFetcher interface {
	GetDependencyRecordsForIssues(ctx context.Context, issueIDs []string) (map[string][]*beads.Dependency, error)
}

// PopulateDependencies batch-fetches dependency records and sets
// issue.Dependencies on each issue. This ensures FindParentID can derive the
// Parent field correctly. No-op when the store doesn't support bulk dependency
// queries or when issues is empty.
func PopulateDependencies(ctx context.Context, s beads.Storage, issues []*beads.Issue) {
	if len(issues) == 0 {
		return
	}
	dqs, ok := s.(depBatchFetcher)
	if !ok {
		return
	}
	ids := make([]string, len(issues))
	for i, issue := range issues {
		ids[i] = issue.ID
	}
	allDeps, err := dqs.GetDependencyRecordsForIssues(ctx, ids)
	if err != nil {
		log.Printf("[store] PopulateDependencies: %v", err)
		return // best-effort: fall back to empty deps
	}
	for _, issue := range issues {
		issue.Dependencies = allDeps[issue.ID]
	}
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

// StatusHooked is the "hooked" bead status — a step parked waiting for a
// condition (human approval, external event, error recovery). Defined here
// because the beads library has it in internal/types but does not re-export it.
const StatusHooked beads.Status = "hooked"

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
	case "hooked":
		return StatusHooked
	default:
		return beads.StatusOpen
	}
}

// ParseIssueType converts a type string to a beads.IssueType. Returns an error
// for unknown strings so CLI callers can surface typos (e.g. "taks") instead of
// silently downgrading to task. Internal types (message, step, attempt, review)
// parse successfully here — IsInternalType is the gate for the human-facing CLI.
func ParseIssueType(s string) (beads.IssueType, error) {
	switch strings.ToLower(s) {
	case "bug":
		return beads.TypeBug, nil
	case "feature":
		return beads.TypeFeature, nil
	case "task":
		return beads.TypeTask, nil
	case "epic":
		return beads.TypeEpic, nil
	case "chore":
		return beads.TypeChore, nil
	case "design":
		return beads.IssueType("design"), nil
	case "recovery":
		return beads.IssueType("recovery"), nil
	case "message":
		return beads.IssueType("message"), nil
	case "step":
		return beads.IssueType("step"), nil
	case "attempt":
		return beads.IssueType("attempt"), nil
	case "review":
		return beads.IssueType("review"), nil
	default:
		return "", fmt.Errorf("unknown issue type %q (valid: task, bug, feature, epic, chore, design, recovery)", s)
	}
}

// ParseIssueTypeOrTask is a lenient wrapper around ParseIssueType for non-CLI
// callers (Linear webhook intake, daemon paths, executor wiring) where silently
// downgrading an unknown type preserves resilience. Logs a warning so typos are
// not invisible. Use the strict ParseIssueType at human-facing boundaries.
func ParseIssueTypeOrTask(s string) beads.IssueType {
	t, err := ParseIssueType(s)
	if err != nil {
		log.Printf("[store] ParseIssueTypeOrTask: %v — falling back to task", err)
		return beads.TypeTask
	}
	return t
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

	// Ephemeral, when true, routes the bead to the wisps table at create time.
	// Wisps are cluster-only and not git-synced. Flipping Ephemeral via
	// UpdateBead after create does NOT move the row between tables — this
	// field must be set on the create call for the routing to work.
	Ephemeral bool
	// Metadata is encoded to JSON and persisted on the bead's metadata column.
	// A nil/empty map leaves the column unset.
	Metadata map[string]string

	// Author is the actor used for the underlying bd CreateIssue call. Empty
	// falls back to Actor() ("spire"). Set this when a gateway request
	// carries a per-call archmage identity so the dolt commit author
	// reflects the calling desktop, not the cluster's "spire" default.
	Author string
}

// BeadsDirResolver is a function that resolves the .beads directory path.
// Set this from the main package so pkg/store can auto-initialize on first use.
var BeadsDirResolver func() string

// getStore returns the active store, auto-initializing via BeadsDirResolver
// if no store is open and a resolver has been set.
//
// Under TowerModeGateway, getStore fails closed before touching local Dolt:
// dispatch entries route through gatewayclient, and any code path that
// slips past dispatch lands here and returns a wrapped ErrGatewayUnsupported
// rather than silently mutating the laptop's database. This is the
// belt-and-suspenders backstop for the structural transport split.
func getStore() (beads.Storage, context.Context, error) {
	if t, ok := isGatewayMode(); ok && t != nil {
		return nil, nil, fmt.Errorf("getStore called under gateway-mode tower %q (missing dispatch entry): %w", t.Name, ErrGatewayUnsupported)
	}
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
