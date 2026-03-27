package main

import (
	"fmt"
	"strings"

	"github.com/steveyegge/beads"
)

// behaviorValidateDesign checks that the bead has a linked, closed, substantive
// design bead (discovered-from dep of type "design"). Blocks with needs-design
// label and archmage notification if not.
func (e *formulaExecutor) behaviorValidateDesign() error {
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

	storeRemoveLabel(e.beadID, "needs-design")
	e.log("design validated: %d design bead(s) linked and closed", len(designBeads))
	storeAddComment(e.beadID, fmt.Sprintf("Wizard: design validated — %d design bead(s) linked and closed. Advancing to plan.", len(designBeads)))
	return nil
}

// behaviorValidateSpec checks that the bead has a linked, closed spec bead.
// The required dep issue type comes from pc.SpecType (default: "spec").
func (e *formulaExecutor) behaviorValidateSpec(pc PhaseConfig) error {
	specType := pc.SpecType
	if specType == "" {
		specType = "spec"
	}

	deps, err := storeGetDepsWithMeta(e.beadID)
	if err != nil {
		return fmt.Errorf("get deps: %w", err)
	}

	var specBeads []Bead
	for _, dep := range deps {
		if dep.DependencyType != beads.DepDiscoveredFrom {
			continue
		}
		if string(dep.IssueType) != specType {
			continue
		}
		specBeads = append(specBeads, Bead{
			ID:          dep.ID,
			Title:       dep.Title,
			Description: dep.Description,
			Status:      string(dep.Status),
			Priority:    dep.Priority,
			Type:        string(dep.IssueType),
			Labels:      dep.Labels,
		})
	}

	if len(specBeads) == 0 {
		e.log("no linked %s bead found — marking as needs-spec", specType)
		storeAddLabel(e.beadID, "needs-spec")
		storeAddComment(e.beadID, fmt.Sprintf("Wizard: no %s bead linked. Create one and link with discovered-from dep.", specType))
		wizardMessageArchmage(e.agentName, e.beadID,
			fmt.Sprintf("Bead %s needs a %s bead. No discovered-from dep of type %s found.", e.beadID, specType, specType))
		return fmt.Errorf("bead %s has no linked %s bead", e.beadID, specType)
	}

	for _, sb := range specBeads {
		if sb.Status != "closed" {
			storeAddLabel(e.beadID, "needs-spec")
			storeAddComment(e.beadID, fmt.Sprintf("Wizard: %s bead %s is still open.", specType, sb.ID))
			wizardMessageArchmage(e.agentName, e.beadID,
				fmt.Sprintf("Bead %s blocked: %s bead %s still open.", e.beadID, specType, sb.ID))
			return fmt.Errorf("%s bead %s not yet closed", specType, sb.ID)
		}
	}

	for _, sb := range specBeads {
		comments, _ := storeGetComments(sb.ID)
		if len(comments) == 0 && sb.Description == "" {
			storeAddLabel(e.beadID, "needs-spec")
			storeAddComment(e.beadID, fmt.Sprintf("Wizard: %s bead %s has no content.", specType, sb.ID))
			wizardMessageArchmage(e.agentName, e.beadID,
				fmt.Sprintf("Bead %s blocked: %s bead %s has no content.", e.beadID, specType, sb.ID))
			return fmt.Errorf("%s bead %s has no content", specType, sb.ID)
		}
	}

	storeRemoveLabel(e.beadID, "needs-spec")
	e.log("%s validated: %d bead(s) linked and closed", specType, len(specBeads))
	storeAddComment(e.beadID, fmt.Sprintf("Wizard: %s validated — %d bead(s) linked and closed.", specType, len(specBeads)))
	return nil
}

// behaviorValidateApproval checks for an "approved" or "approved-by:*" label on the bead.
// Blocks with needs-approval label and archmage notification if missing.
func (e *formulaExecutor) behaviorValidateApproval(pc PhaseConfig) error {
	bead, err := storeGetBead(e.beadID)
	if err != nil {
		return fmt.Errorf("get bead: %w", err)
	}

	for _, l := range bead.Labels {
		if l == "approved" || strings.HasPrefix(l, "approved-by:") {
			storeRemoveLabel(e.beadID, "needs-approval")
			e.log("approval found: %s", l)
			storeAddComment(e.beadID, fmt.Sprintf("Wizard: approval validated (%s).", l))
			return nil
		}
	}

	e.log("no approval label found — marking as needs-approval")
	storeAddLabel(e.beadID, "needs-approval")
	storeAddComment(e.beadID, "Wizard: approval required. Add label `approved` or `approved-by:<name>` to proceed.")
	wizardMessageArchmage(e.agentName, e.beadID,
		fmt.Sprintf("Bead %s needs approval. Add label `approved` to proceed.", e.beadID))
	return fmt.Errorf("bead %s has no approval label", e.beadID)
}

// behaviorValidateGate checks for a "gate-passed" label on the bead.
// Blocks with needs-gate label and archmage notification if missing.
func (e *formulaExecutor) behaviorValidateGate(pc PhaseConfig) error {
	bead, err := storeGetBead(e.beadID)
	if err != nil {
		return fmt.Errorf("get bead: %w", err)
	}

	for _, l := range bead.Labels {
		if l == "gate-passed" {
			storeRemoveLabel(e.beadID, "needs-gate")
			e.log("gate passed")
			storeAddComment(e.beadID, "Wizard: gate validated (gate-passed label found).")
			return nil
		}
	}

	e.log("gate not passed — marking as needs-gate")
	storeAddLabel(e.beadID, "needs-gate")
	storeAddComment(e.beadID, "Wizard: gate check required. Add label `gate-passed` when the external gate clears.")
	wizardMessageArchmage(e.agentName, e.beadID,
		fmt.Sprintf("Bead %s is blocked on an external gate. Add label `gate-passed` to proceed.", e.beadID))
	return fmt.Errorf("bead %s has not passed gate check", e.beadID)
}
