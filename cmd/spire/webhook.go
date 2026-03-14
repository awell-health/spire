package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

// linearEvent represents the relevant fields from a Linear webhook payload.
type linearEvent struct {
	Action string `json:"action"`
	Type   string `json:"type"`
	Data   struct {
		ID         string `json:"id"`
		Identifier string `json:"identifier"`
		Title      string `json:"title"`
		Priority   int    `json:"priority"`
		Labels     []struct {
			Name string `json:"name"`
		} `json:"labels"`
		Assignee *struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"assignee"`
	} `json:"data"`
}

// labelRigMap maps exact Linear label names to rig prefixes.
var labelRigMap = map[string]string{
	"Workstream: Platform": "awp",
}

// labelPrefixRigMap maps Linear label prefixes to rig prefixes.
var labelPrefixRigMap = map[string]string{
	"Panels": "pan",
	"Grove":  "gro",
}

// linearToBeadsPriority converts Linear priority (0-4) to beads priority (0-4).
// Linear: 0=none, 1=urgent, 2=high, 3=medium, 4=low
// Beads:  0=P0,   1=P1,     2=P2,   3=P3,      4=P4
func linearToBeadsPriority(linearPri int) int {
	switch linearPri {
	case 1:
		return 0
	case 2:
		return 1
	case 3:
		return 2
	case 4:
		return 3
	default:
		return 3 // no priority -> P3
	}
}

// mapLabelsToRig determines the rig prefix from Linear issue labels.
// Returns the rig prefix and true if a match is found, or "" and false.
func mapLabelsToRig(labels []string) (string, bool) {
	// Exact match first
	for _, label := range labels {
		if rig, ok := labelRigMap[label]; ok {
			return rig, true
		}
	}
	// Prefix match
	for _, label := range labels {
		for prefix, rig := range labelPrefixRigMap {
			if strings.HasPrefix(label, prefix) {
				return rig, true
			}
		}
	}
	return "", false
}

// parseWebhookPayload parses a Linear webhook JSON payload from a bead description.
func parseWebhookPayload(description string) (linearEvent, error) {
	var event linearEvent
	if err := json.Unmarshal([]byte(description), &event); err != nil {
		return event, fmt.Errorf("parse webhook payload: %w", err)
	}
	if event.Data.Identifier == "" {
		return event, fmt.Errorf("parse webhook payload: missing identifier")
	}
	return event, nil
}

// processWebhookEvent processes a single webhook event bead.
// Returns an error only if the event should be retried (not closed).
func processWebhookEvent(eventBead Bead) error {
	// Extract event type and linear identifier from labels
	eventType := hasLabel(eventBead, "event:")
	linearID := hasLabel(eventBead, "linear:")

	if linearID == "" {
		log.Printf("[daemon] event %s: no linear: label, skipping", eventBead.ID)
		return nil // close it, don't retry
	}

	// Parse the payload from description
	event, err := parseWebhookPayload(eventBead.Description)
	if err != nil {
		log.Printf("[daemon] event %s: %s, skipping", eventBead.ID, err)
		return nil // close it, malformed payload
	}

	// Extract Linear labels as strings
	var linearLabels []string
	for _, l := range event.Data.Labels {
		linearLabels = append(linearLabels, l.Name)
	}

	// Map to rig prefix
	rig, found := mapLabelsToRig(linearLabels)
	if !found {
		log.Printf("[daemon] event %s: no rig match for labels %v, skipping", eventBead.ID, linearLabels)
		return nil // close it, no rig match
	}

	// Ensure epic bead exists
	epicID, err := ensureEpicBead(rig, event)
	if err != nil {
		return fmt.Errorf("ensure epic bead: %w", err) // retry
	}

	// Notify owner if claimed
	err = notifyOwnerIfClaimed(epicID, linearID, eventType)
	if err != nil {
		// Non-fatal: notification failure should not prevent closing the event
		log.Printf("[daemon] event %s: notification failed: %s", eventBead.ID, err)
	}

	return nil
}

// ensureEpicBead finds or creates an epic bead for the given Linear issue.
// Returns the bead ID.
func ensureEpicBead(rig string, event linearEvent) (string, error) {
	identifier := event.Data.Identifier
	title := event.Data.Title
	priority := linearToBeadsPriority(event.Data.Priority)

	// Look for existing epic with this linear identifier
	var existing []Bead
	err := bdJSON(&existing, "list", fmt.Sprintf("--rig=%s", rig), "--label", fmt.Sprintf("linear:%s", identifier), "--type", "epic")
	if err != nil {
		return "", fmt.Errorf("search for epic linear:%s: %w", identifier, err)
	}

	if len(existing) > 0 {
		epicBead := existing[0]
		// Update title/priority if changed
		needsUpdate := false
		var updateArgs []string
		updateArgs = append(updateArgs, "update", epicBead.ID)

		if epicBead.Title != title {
			updateArgs = append(updateArgs, "--title", title)
			needsUpdate = true
		}
		if epicBead.Priority != priority {
			updateArgs = append(updateArgs, "-p", fmt.Sprintf("%d", priority))
			needsUpdate = true
		}

		if needsUpdate {
			_, err := bd(updateArgs...)
			if err != nil {
				return "", fmt.Errorf("update epic %s: %w", epicBead.ID, err)
			}
			log.Printf("[daemon] updated epic %s (%s): title/priority synced", epicBead.ID, identifier)
		}

		return epicBead.ID, nil
	}

	// Create new epic bead
	id, err := bdSilent(
		"create",
		fmt.Sprintf("--rig=%s", rig),
		"--type=epic",
		"--title", title,
		"-p", fmt.Sprintf("%d", priority),
		"--labels", fmt.Sprintf("linear:%s", identifier),
	)
	if err != nil {
		return "", fmt.Errorf("create epic for %s: %w", identifier, err)
	}

	log.Printf("[daemon] created epic %s for %s in rig %s", id, identifier, rig)
	return id, nil
}

// notifyOwnerIfClaimed sends spire mail to the epic owner if someone has claimed it.
func notifyOwnerIfClaimed(epicID, linearID, eventType string) error {
	// Fetch the epic bead to check for owner
	out, err := bd("show", epicID, "--json")
	if err != nil {
		return fmt.Errorf("show epic %s: %w", epicID, err)
	}

	epicBead, err := parseBead([]byte(out))
	if err != nil {
		return fmt.Errorf("parse epic %s: %w", epicID, err)
	}

	// Check for owner — look for the owner field or a claimed-by label
	owner := hasLabel(epicBead, "owner:")
	if owner == "" {
		// No owner, no notification
		return nil
	}

	// Send notification via spire send
	msg := fmt.Sprintf("%s updated (%s)", linearID, eventType)
	return cmdSend([]string{"--as", "spi", owner, msg, "--ref", epicID})
}
