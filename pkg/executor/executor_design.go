package executor

import (
	"fmt"
	"time"

	"github.com/steveyegge/beads"
)

// wizardValidateDesign checks that the epic has a linked design bead (discovered-from dep) that is
// closed and substantive. If missing, it auto-creates one and waits. If open or empty, it polls
// until the design bead is closed with content, then advances to the plan phase.
func (e *Executor) wizardValidateDesign() (retErr error) {
	started := time.Now()
	defer func() {
		e.recordAgentRun(e.agentName, e.beadID, "", "", "wizard", "validate-design", started, retErr)
	}()

	interval := e.designPollInterval
	if interval == 0 {
		interval = 30 * time.Second
	}

	// Track notification state so we only message/comment once per blocker.
	var (
		notifiedCreated bool
		notifiedOpen    bool
		notifiedEmpty   bool
	)

	for {
		// Find linked design beads via discovered-from deps.
		deps, err := e.deps.GetDepsWithMeta(e.beadID)
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

		// --- Case: no design bead found → create one ---
		if len(designBeads) == 0 {
			if !notifiedCreated {
				e.log("no linked design bead found — creating one automatically")

				designTitle := fmt.Sprintf("Design: %s", e.beadID)
				designID, err := e.deps.CreateBead(CreateOpts{
					Title:    designTitle,
					Priority: 1,
					Type:     e.deps.ParseIssueType("design"),
					Prefix:   prefixFromBeadID(e.beadID),
				})
				if err != nil {
					return fmt.Errorf("create design bead: %w", err)
				}

				// Link: epic discovered-from design bead.
				if err := e.deps.AddDepTyped(e.beadID, designID, "discovered-from"); err != nil {
					return fmt.Errorf("link design bead: %w", err)
				}

				// Comment on the epic.
				e.deps.AddComment(e.beadID, fmt.Sprintf(
					"Wizard: auto-created design bead %s. Waiting for archmage to fill in the design and close it.", designID))

				// Comment on the design bead.
				e.deps.AddComment(designID, fmt.Sprintf(
					"This design bead was auto-created for epic %s. Please add design decisions and close when ready.", e.beadID))

				// Label epic needs-human.
				e.deps.AddLabel(e.beadID, "needs-human")

				// Message archmage once.
				MessageArchmage(e.agentName, e.beadID,
					fmt.Sprintf("Epic %s needs design. Auto-created design bead %s — please fill it in and close it.", e.beadID, designID),
					e.deps)

				notifiedCreated = true
			} else {
				e.log("still waiting for design bead to appear in deps")
			}

			time.Sleep(interval)
			continue
		}

		// --- Case: design bead(s) found but open → wait ---
		allClosed := true
		for _, db := range designBeads {
			if db.Status != "closed" {
				allClosed = false
				if !notifiedOpen {
					e.log("design bead %s is still open — waiting for it to be closed", db.ID)
					e.deps.AddLabel(e.beadID, "needs-human")
					e.deps.AddComment(e.beadID, fmt.Sprintf(
						"Wizard: design bead %s is still open. Waiting for it to be closed.", db.ID))
					MessageArchmage(e.agentName, e.beadID,
						fmt.Sprintf("Epic %s is waiting: design bead %s is still open. Close it when the design is settled.", e.beadID, db.ID),
						e.deps)
					notifiedOpen = true
				} else {
					e.log("still waiting for design bead %s to close", db.ID)
				}
				break
			}
		}
		if !allClosed {
			time.Sleep(interval)
			continue
		}

		// --- Case: design bead(s) closed but empty → wait ---
		allHaveContent := true
		for _, db := range designBeads {
			comments, _ := e.deps.GetComments(db.ID)
			if len(comments) == 0 && db.Description == "" {
				allHaveContent = false
				if !notifiedEmpty {
					e.log("design bead %s has no content — waiting for enrichment", db.ID)
					e.deps.AddLabel(e.beadID, "needs-human")
					e.deps.AddComment(e.beadID, fmt.Sprintf(
						"Wizard: design bead %s exists but has no content. Waiting for design decisions to be added.", db.ID))
					MessageArchmage(e.agentName, e.beadID,
						fmt.Sprintf("Epic %s is waiting: design bead %s has no content. Add design decisions as comments before proceeding.", e.beadID, db.ID),
						e.deps)
					notifiedEmpty = true
				} else {
					e.log("still waiting for design bead %s to get content", db.ID)
				}
				break
			}
		}
		if !allHaveContent {
			time.Sleep(interval)
			continue
		}

		// --- Happy path: all design beads closed with content → advance ---
		e.deps.RemoveLabel(e.beadID, "needs-human")
		e.deps.RemoveLabel(e.beadID, "needs-design")
		e.log("design validated: %d design bead(s) linked and closed", len(designBeads))
		e.deps.AddComment(e.beadID, fmt.Sprintf("Wizard: design validated — %d design bead(s) linked and closed. Advancing to plan.", len(designBeads)))
		return nil
	}
}
