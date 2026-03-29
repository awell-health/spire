package store

import "strings"

// Bead represents a beads issue — the lightweight projection used throughout spire.
type Bead struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	Priority    int      `json:"priority"`
	Type        string   `json:"issue_type"`
	Labels      []string `json:"labels"`
	Parent      string   `json:"parent"`
}

// BoardBead extends Bead with full board metadata (owner, timestamps, deps).
type BoardBead struct {
	ID              string     `json:"id"`
	Title           string     `json:"title"`
	Description     string     `json:"description"`
	Status          string     `json:"status"`
	Priority        int        `json:"priority"`
	Type            string     `json:"issue_type"`
	Owner           string     `json:"owner"`
	CreatedAt       string     `json:"created_at"`
	UpdatedAt       string     `json:"updated_at"`
	Labels          []string   `json:"labels"`
	Parent          string     `json:"parent"`
	Dependencies    []BoardDep `json:"dependencies"`
	DependencyCount int        `json:"dependency_count"`
	DependentCount  int        `json:"dependent_count"`
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
