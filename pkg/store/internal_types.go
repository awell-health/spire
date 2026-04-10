package store

import (
	"fmt"
	"log"

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

// labelToType maps legacy label-based identification to the new internal bead types.
var labelToType = []struct {
	label    string
	beadType beads.IssueType
}{
	{"msg", "message"},
	{"workflow-step", "step"},
	{"attempt", "attempt"},
	{"review-round", "review"},
}

// MigrateInternalTypes converts existing label-identified beads to proper types.
// It queries beads by label and updates their type field. Idempotent — skips
// beads that already have the correct type. Labels remain on the beads.
func MigrateInternalTypes() error {
	migrated := 0
	for _, lt := range labelToType {
		results, err := ListBeads(beads.IssueFilter{
			Labels: []string{lt.label},
		})
		if err != nil {
			return fmt.Errorf("migrate internal types: query label %q: %w", lt.label, err)
		}
		for _, b := range results {
			if b.Type == string(lt.beadType) {
				continue // already migrated
			}
			if err := UpdateBead(b.ID, map[string]interface{}{
				"issue_type": string(lt.beadType),
			}); err != nil {
				log.Printf("migrate internal types: update %s to type %s: %v", b.ID, lt.beadType, err)
				continue
			}
			migrated++
		}
	}
	if migrated > 0 {
		log.Printf("migrated %d beads to internal types", migrated)
	}
	return nil
}
