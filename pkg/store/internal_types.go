package store

import (
	"fmt"

	"github.com/steveyegge/beads"
)

// InternalTypes are bead types used for internal bookkeeping, not user-facing work.
var InternalTypes = map[string]bool{
	"message": true,
	"step":    true,
	"attempt": true,
	"review":  true,
}

// IsWorkBead returns true if the bead represents user-facing work (not internal bookkeeping)
// and is a top-level bead (not a child of another bead).
func IsWorkBead(b Bead) bool {
	return !InternalTypes[b.Type] && b.Parent == ""
}

// IsInternalBead returns true if the bead type is an internal bookkeeping type.
func IsInternalBead(b Bead) bool {
	return InternalTypes[b.Type]
}

// migrationMapping defines label→type conversions for existing beads.
var migrationMapping = []struct {
	Label      string
	TargetType string
}{
	{"msg", "message"},
	{"workflow-step", "step"},
	{"attempt", "attempt"},
	{"review-round", "review"},
}

// MigrateInternalTypes converts existing label-identified beads to proper types.
// It is idempotent — safe to run on every startup. Labels are NOT removed.
func MigrateInternalTypes() error {
	s, ctx, err := getStore()
	if err != nil {
		return fmt.Errorf("migrate internal types: %w", err)
	}

	for _, m := range migrationMapping {
		issues, err := s.SearchIssues(ctx, "", beads.IssueFilter{
			Labels: []string{m.Label},
		})
		if err != nil {
			return fmt.Errorf("migrate internal types: search label %q: %w", m.Label, err)
		}

		migrated := 0
		for _, issue := range issues {
			if string(issue.IssueType) == m.TargetType {
				continue // already correct type
			}
			if err := s.UpdateIssue(ctx, issue.ID, map[string]interface{}{
				"issue_type": m.TargetType,
			}, Actor()); err != nil {
				return fmt.Errorf("migrate internal types: update %s to type %q: %w", issue.ID, m.TargetType, err)
			}
			migrated++
		}

		if migrated > 0 {
			fmt.Printf("  migrated %d beads: %s → %s\n", migrated, m.Label, m.TargetType)
		}
	}

	return nil
}
