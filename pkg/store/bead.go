package store

import "strings"

// Bead represents a beads issue — the lightweight projection used throughout spire.
type Bead struct {
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

// Meta returns the metadata value for key, or "" if not set.
func (b Bead) Meta(key string) string {
	if b.Metadata == nil {
		return ""
	}
	return b.Metadata[key]
}

// IsActive reports whether the bead is currently being worked on or queued
// in the open backlog. Mirrors lifecycle.IsActive — defined here so pkg/store
// callers can use a named predicate without inverting the package dependency
// (pkg/lifecycle imports pkg/store, so pkg/store cannot import pkg/lifecycle).
// Behavior must stay in lockstep with pkg/lifecycle's predicate body.
func (b Bead) IsActive() bool {
	return b.Status == "in_progress" || b.Status == "open"
}

// BoardBead extends Bead with full board metadata (owner, timestamps, deps).
type BoardBead struct {
	ID              string            `json:"id"`
	Title           string            `json:"title"`
	Description     string            `json:"description"`
	Status          string            `json:"status"`
	Priority        int               `json:"priority"`
	Type            string            `json:"issue_type"`
	Owner           string            `json:"owner"`
	CreatedAt       string            `json:"created_at"`
	UpdatedAt       string            `json:"updated_at"`
	ClosedAt        string            `json:"closed_at,omitempty"`
	Labels          []string          `json:"labels"`
	Parent          string            `json:"parent"`
	Dependencies    []BoardDep        `json:"dependencies"`
	DependencyCount int               `json:"dependency_count"`
	DependentCount  int               `json:"dependent_count"`
	Metadata        map[string]string `json:"metadata,omitempty"`
}

// Meta returns the metadata value for key, or "" if not set.
func (b BoardBead) Meta(key string) string {
	if b.Metadata == nil {
		return ""
	}
	return b.Metadata[key]
}

// BoardDep represents a dependency edge on a board bead.
type BoardDep struct {
	IssueID     string `json:"issue_id"`
	DependsOnID string `json:"depends_on_id"`
	Type        string `json:"type"`
}

// HasLabel checks if a bead has a label with the given prefix, returning the suffix.
func HasLabel(b Bead, prefix string) string {
	for _, l := range b.Labels {
		if strings.HasPrefix(l, prefix) {
			return l[len(prefix):]
		}
	}
	return ""
}

// ContainsLabel checks if a bead has an exact label match.
func ContainsLabel(b Bead, label string) bool {
	for _, l := range b.Labels {
		if l == label {
			return true
		}
	}
	return false
}

// HasLabel checks if a BoardBead has an exact label match.
func (b BoardBead) HasLabel(label string) bool {
	for _, l := range b.Labels {
		if l == label {
			return true
		}
	}
	return false
}

// HasLabelPrefix checks if a BoardBead has a label with the given prefix, returning the suffix.
func (b BoardBead) HasLabelPrefix(prefix string) string {
	for _, l := range b.Labels {
		if strings.HasPrefix(l, prefix) {
			return l[len(prefix):]
		}
	}
	return ""
}
