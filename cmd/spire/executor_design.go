package main

import (
	"fmt"
	"os"

	"github.com/steveyegge/beads"
)

// wizardValidateDesign checks that the epic has a linked design bead (discovered-from dep) that is
// closed and substantive. If missing or incomplete, labels the epic "needs-design"
// and pauses. If complete, advances.
func (e *formulaExecutor) wizardValidateDesign() error {
	// Find linked design beads via discovered-from deps
	deps, err := storeGetDepsWithMeta(e.beadID)
	if err != nil {
		return fmt.Errorf("get deps: %w", err)
	}

	var designBeads []Bead
	for _, dep := range deps {
		if dep.DependencyType != beads.DepDiscoveredFrom {
			continue
		}
		if dep.IssueType != "design" {
			continue
		}
		designBeads = append(designBeads, Bead{
			ID:          dep.ID,
			Title:       dep.Title,
			Description: dep.Description,
			Status:      string(dep.Status),
			Priority:    dep.Priority,
			Type:        string(dep.IssueType),
			Labels:      dep.Labels,
		})
	}

	if len(designBeads) == 0 {
		e.log("no linked design bead found — marking as needs-design")
		storeAddLabel(e.beadID, "needs-design")
		storeAddComment(e.beadID, "Wizard: no design bead linked. Create a design bead with `spire design`, then link it: `bd dep add "+e.beadID+" <design-id> --type discovered-from`")
		wizardMessageArchmage(e.agentName, e.beadID,
			fmt.Sprintf("Epic %s needs a design bead. No discovered-from dep found. Create one with `spire design`, then link it: `bd dep add %s <design-id> --type discovered-from`", e.beadID, e.beadID))
		return fmt.Errorf("epic %s has no linked design bead — label needs-design added", e.beadID)
	}

	// Check design bead completeness
	for _, db := range designBeads {
		if db.Status != "closed" {
			e.log("design bead %s is still open — waiting for it to be closed", db.ID)
			storeAddLabel(e.beadID, "needs-design")
			storeAddComment(e.beadID, fmt.Sprintf("Wizard: design bead %s is still open. Close it when the design is settled.", db.ID))
			wizardMessageArchmage(e.agentName, e.beadID,
				fmt.Sprintf("Epic %s is blocked: design bead %s is still open. Close it when the design is settled.", e.beadID, db.ID))
			return fmt.Errorf("design bead %s not yet closed", db.ID)
		}
	}

	// Check that design bead has substance (at least one comment)
	for _, db := range designBeads {
		comments, _ := storeGetComments(db.ID)
		if len(comments) == 0 && db.Description == "" {
			e.log("design bead %s has no content — needs enrichment", db.ID)
			storeAddLabel(e.beadID, "needs-design")
			storeAddComment(e.beadID, fmt.Sprintf("Wizard: design bead %s exists but has no content. Add design decisions as comments before proceeding.", db.ID))
			wizardMessageArchmage(e.agentName, e.beadID,
				fmt.Sprintf("Epic %s is blocked: design bead %s has no content. Add design decisions as comments before proceeding.", e.beadID, db.ID))
			return fmt.Errorf("design bead %s has no content", db.ID)
		}
	}

	// Design validated — remove needs-design label if present and log
	storeRemoveLabel(e.beadID, "needs-design")
	e.log("design validated: %d design bead(s) linked and closed", len(designBeads))
	storeAddComment(e.beadID, fmt.Sprintf("Wizard: design validated — %d design bead(s) linked and closed. Advancing to plan.", len(designBeads)))
	return nil
}

// wizardMessageArchmage sends a spire message to the archmage referencing the given bead.
// Errors are logged but do not block the caller.
func wizardMessageArchmage(from, beadID, message string) {
	labels := []string{"msg", "to:archmage", "from:" + from, "ref:" + beadID}
	if _, err := storeCreateBead(createOpts{
		Title:    message,
		Priority: 1,
		Type:     beads.TypeTask,
		Prefix:   "spi",
		Labels:   labels,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: message archmage: %s\n", err)
	}
}
