package bd

import "time"

// BeadStatus represents the status of a bead.
type BeadStatus string

const (
	StatusOpen       BeadStatus = "open"
	StatusReady      BeadStatus = "ready"
	StatusInProgress BeadStatus = "in_progress"
	StatusDeferred   BeadStatus = "deferred"
	StatusClosed     BeadStatus = "closed"
	StatusDone       BeadStatus = "done"
)

// BeadType represents the type of a bead.
type BeadType string

const (
	TypeTask    BeadType = "task"
	TypeBug     BeadType = "bug"
	TypeFeature BeadType = "feature"
	TypeEpic    BeadType = "epic"
	TypeChore   BeadType = "chore"
)

// Bead represents a beads issue from bd JSON output.
// Field names and JSON tags match the bd CLI's JSON serialization.
type Bead struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	Priority    int      `json:"priority"`
	Type        string   `json:"issue_type"`
	Owner       string   `json:"owner"`
	Parent      string   `json:"parent"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
	Labels      []string `json:"labels"`
	Children    []string `json:"children"`
	BlockedBy   []string `json:"blocked_by"`
	Blocking    []string `json:"blocking"`
}

// CreatedTime parses CreatedAt as time.Time.
func (b *Bead) CreatedTime() (time.Time, error) {
	return time.Parse(time.RFC3339, b.CreatedAt)
}

// UpdatedTime parses UpdatedAt as time.Time.
func (b *Bead) UpdatedTime() (time.Time, error) {
	return time.Parse(time.RFC3339, b.UpdatedAt)
}

// HasLabel returns true if the bead has the given label.
func (b *Bead) HasLabel(label string) bool {
	for _, l := range b.Labels {
		if l == label {
			return true
		}
	}
	return false
}

// Comment represents a comment on a bead.
type Comment struct {
	IssueID   string `json:"issue_id"`
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

// DoltRemote represents a dolt remote entry.
type DoltRemote struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}
