package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
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

// webhookQueueRow represents a row from the webhook_queue table.
type webhookQueueRow struct {
	ID        string `json:"id"`
	EventType string `json:"event_type"`
	LinearID  string `json:"linear_id"`
	Payload   string `json:"payload"`
}

// doltSQL runs a SQL query against the Dolt server and returns the output.
// Uses dolt CLI with connection parameters from environment.
func doltSQL(query string, jsonOutput bool) (string, error) {
	host := os.Getenv("BEADS_DOLT_SERVER_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	port := os.Getenv("BEADS_DOLT_SERVER_PORT")
	if port == "" {
		port = "3307"
	}

	args := []string{
		"--host", host,
		"--port", port,
		"--user", "root",
		"--no-tls",
		"--use-db", "spi",
		"sql", "-q", query,
	}
	if jsonOutput {
		args = append(args, "-r", "json")
	}

	cmd := exec.Command("dolt", args...)
	cmd.Env = append(os.Environ(), "DOLT_CLI_PASSWORD=")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("dolt sql: %s\n%s", err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

// processWebhookQueue reads unprocessed rows from webhook_queue,
// creates webhook event beads from them, processes them, and marks them done.
// Returns (processed count, error count).
func processWebhookQueue() (int, int) {
	// Query unprocessed queue rows
	out, err := doltSQL(
		"SELECT id, event_type, linear_id, payload FROM webhook_queue WHERE processed = 0",
		true,
	)
	if err != nil {
		// Table may not exist yet -- not an error
		if !strings.Contains(err.Error(), "webhook_queue") {
			log.Printf("[daemon] query webhook_queue: %s", err)
		}
		return 0, 0
	}

	if strings.TrimSpace(out) == "" {
		return 0, 0
	}

	// dolt sql -r json wraps results in {"rows": [...]}
	var wrapper struct {
		Rows []webhookQueueRow `json:"rows"`
	}
	if err := json.Unmarshal([]byte(out), &wrapper); err != nil {
		// Try parsing as a plain array (fallback)
		var rows []webhookQueueRow
		if err2 := json.Unmarshal([]byte(out), &rows); err2 != nil {
			log.Printf("[daemon] parse webhook_queue rows: %s (wrapper: %s)", err2, err)
			return 0, 0
		}
		wrapper.Rows = rows
	}

	if len(wrapper.Rows) == 0 {
		return 0, 0
	}

	log.Printf("[daemon] found %d unprocessed queue rows", len(wrapper.Rows))

	processed := 0
	errors := 0

	for _, row := range wrapper.Rows {
		// Create a webhook event bead from the queue row
		eventID, createErr := bdSilent(
			"create",
			"--rig=spi",
			"--type=task",
			"-p", "3",
			"--title", fmt.Sprintf("%s: %s", row.EventType, row.LinearID),
			"--labels", fmt.Sprintf("webhook,event:%s,linear:%s", row.EventType, row.LinearID),
			"--description", row.Payload,
		)
		if createErr != nil {
			log.Printf("[daemon] queue row %s: create bead failed: %s", row.ID, createErr)
			errors++
			continue
		}

		// Fetch the created bead for processing
		showOut, showErr := bd("show", eventID, "--json")
		if showErr != nil {
			log.Printf("[daemon] queue row %s: show bead %s failed: %s", row.ID, eventID, showErr)
			errors++
			continue
		}

		eventBead, parseErr := parseBead([]byte(showOut))
		if parseErr != nil {
			log.Printf("[daemon] queue row %s: parse bead %s failed: %s", row.ID, eventID, parseErr)
			errors++
			continue
		}

		// Process the event (existing logic)
		procErr := processWebhookEvent(eventBead)
		if procErr != nil {
			log.Printf("[daemon] queue row %s: process error (will retry): %s", row.ID, procErr)
			errors++
			continue
		}

		// Close the event bead
		bd("close", eventID)

		// Mark queue row as processed
		_, markErr := doltSQL(
			fmt.Sprintf("UPDATE webhook_queue SET processed = 1 WHERE id = '%s'", row.ID),
			false,
		)
		if markErr != nil {
			log.Printf("[daemon] queue row %s: mark processed failed: %s", row.ID, markErr)
			// Don't count as error -- the bead was created and processed
		}

		processed++
	}

	return processed, errors
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
