package integration

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// LinearEvent represents the relevant fields from a Linear webhook payload.
type LinearEvent struct {
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

// LabelRigMap maps exact Linear label names to rig prefixes.
// Configure via bd config set linear.label-map 'Label Name=prefix,Other=pfx'
var LabelRigMap = map[string]string{}

// LabelPrefixRigMap maps Linear label prefixes to rig prefixes.
// Configure via bd config set linear.label-prefix-map 'Prefix=pfx,Other=pfx'
var LabelPrefixRigMap = map[string]string{}

var labelMapsOnce sync.Once

// LoadLabelMaps loads label-to-rig mapping configuration from the store.
func LoadLabelMaps() {
	labelMapsOnce.Do(func() {
		if StoreGetConfig == nil {
			return
		}
		if out, _ := StoreGetConfig("linear.label-map"); out != "" {
			for _, pair := range strings.Split(out, ",") {
				parts := strings.SplitN(pair, "=", 2)
				if len(parts) == 2 {
					LabelRigMap[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
				}
			}
		}
		if out, _ := StoreGetConfig("linear.label-prefix-map"); out != "" {
			for _, pair := range strings.Split(out, ",") {
				parts := strings.SplitN(pair, "=", 2)
				if len(parts) == 2 {
					LabelPrefixRigMap[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
				}
			}
		}
	})
}

// ResetLabelMaps resets the label maps so they can be reloaded.
// Useful for testing.
func ResetLabelMaps() {
	labelMapsOnce = sync.Once{}
	LabelRigMap = map[string]string{}
	LabelPrefixRigMap = map[string]string{}
}

// LinearToBeadsPriority converts Linear priority (0-4) to beads priority (0-4).
// Linear: 0=none, 1=urgent, 2=high, 3=medium, 4=low
// Beads:  0=P0,   1=P1,     2=P2,   3=P3,      4=P4
func LinearToBeadsPriority(linearPri int) int {
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

// MapLabelsToRig determines the rig prefix from Linear issue labels.
// Returns the rig prefix and true if a match is found, or "" and false.
func MapLabelsToRig(labels []string) (string, bool) {
	LoadLabelMaps()

	// Exact match first
	for _, label := range labels {
		if rig, ok := LabelRigMap[label]; ok {
			return rig, true
		}
	}
	// Prefix match
	for _, label := range labels {
		for prefix, rig := range LabelPrefixRigMap {
			if strings.HasPrefix(label, prefix) {
				return rig, true
			}
		}
	}
	return "", false
}

// ParseWebhookPayload parses a Linear webhook JSON payload from a bead description.
func ParseWebhookPayload(description string) (LinearEvent, error) {
	var event LinearEvent
	if err := json.Unmarshal([]byte(description), &event); err != nil {
		return event, fmt.Errorf("parse webhook payload: %w", err)
	}
	if event.Data.Identifier == "" {
		return event, fmt.Errorf("parse webhook payload: missing identifier")
	}
	return event, nil
}

// ProcessWebhookEvent processes a single webhook event bead.
// Returns an error only if the event should be retried (not closed).
func ProcessWebhookEvent(eventBead Bead) error {
	LoadLabelMaps()

	// Extract event type and linear identifier from labels
	eventType := HasLabel(eventBead, "event:")
	linearID := HasLabel(eventBead, "linear:")

	if linearID == "" {
		log.Printf("[daemon] event %s: no linear: label, skipping", eventBead.ID)
		return nil // close it, don't retry
	}

	// Parse the payload from description
	event, err := ParseWebhookPayload(eventBead.Description)
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
	rig, found := MapLabelsToRig(linearLabels)
	if !found {
		log.Printf("[daemon] event %s: no rig match for labels %v, skipping", eventBead.ID, linearLabels)
		return nil // close it, no rig match
	}

	// Ensure epic bead exists
	epicID, err := EnsureEpicBead(rig, event)
	if err != nil {
		return fmt.Errorf("ensure epic bead: %w", err) // retry
	}

	// Notify owner if claimed
	err = NotifyOwnerIfClaimed(epicID, linearID, eventType)
	if err != nil {
		// Non-fatal: notification failure should not prevent closing the event
		log.Printf("[daemon] event %s: notification failed: %s", eventBead.ID, err)
	}

	return nil
}

// EnsureEpicBead finds or creates an epic bead for the given Linear issue.
// Returns the bead ID.
func EnsureEpicBead(rig string, event LinearEvent) (string, error) {
	identifier := event.Data.Identifier
	title := event.Data.Title
	priority := LinearToBeadsPriority(event.Data.Priority)

	// Look for existing epic with this linear identifier
	existing, err := StoreListBeads(beads.IssueFilter{IDPrefix: rig + "-", Labels: []string{"linear:" + identifier}, IssueType: IssueTypePtr(beads.TypeEpic)})
	if err != nil {
		return "", fmt.Errorf("search for epic linear:%s: %w", identifier, err)
	}

	if len(existing) > 0 {
		epicBead := existing[0]
		// Update title/priority if changed
		updates := map[string]interface{}{}

		if epicBead.Title != title {
			updates["title"] = title
		}
		if epicBead.Priority != priority {
			updates["priority"] = priority
		}

		if len(updates) > 0 {
			if err := StoreUpdateBead(epicBead.ID, updates); err != nil {
				return "", fmt.Errorf("update epic %s: %w", epicBead.ID, err)
			}
			log.Printf("[daemon] updated epic %s (%s): title/priority synced", epicBead.ID, identifier)
		}

		return epicBead.ID, nil
	}

	// Create new epic bead
	id, err := StoreCreateBead(store.CreateOpts{Title: title, Priority: priority, Type: beads.TypeEpic, Labels: []string{"linear:" + identifier}, Prefix: rig})
	if err != nil {
		return "", fmt.Errorf("create epic for %s: %w", identifier, err)
	}

	log.Printf("[daemon] created epic %s for %s in rig %s", id, identifier, rig)
	return id, nil
}

// WebhookQueueRow represents a row from the webhook_queue table.
type WebhookQueueRow struct {
	ID        string `json:"id"`
	EventType string `json:"event_type"`
	LinearID  string `json:"linear_id"`
	Payload   string `json:"payload"`
}

// ProcessWebhookQueue reads unprocessed rows from webhook_queue,
// creates webhook event beads from them, processes them, and marks them done.
// Returns (processed count, error count).
//
// gateway-mode: no-op. The webhook_queue table lives in the cluster's Dolt
// database for cluster-as-truth deployments; the cluster-side daemon
// processes it. This call's UPDATE webhook_queue SET processed = 1 would
// otherwise mutate the laptop's local Dolt directly (DoltSQL is a callback
// that resolves to local Dolt SQL), bypassing the gateway. Returning
// (0, 0) leaves cluster-side processing as the sole writer.
func ProcessWebhookQueue() (int, int) {
	if err := config.EnsureNotGatewayResolved("integration.ProcessWebhookQueue"); err != nil {
		return 0, 0
	}
	// Query unprocessed queue rows
	out, err := DoltSQL(
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
		Rows []WebhookQueueRow `json:"rows"`
	}
	if err := json.Unmarshal([]byte(out), &wrapper); err != nil {
		// Try parsing as a plain array (fallback)
		var rows []WebhookQueueRow
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
		eventID, createErr := StoreCreateBead(store.CreateOpts{
			Title:       fmt.Sprintf("%s: %s", row.EventType, row.LinearID),
			Description: row.Payload,
			Priority:    3,
			Type:        beads.TypeTask,
			Labels:      []string{"webhook", fmt.Sprintf("event:%s", row.EventType), fmt.Sprintf("linear:%s", row.LinearID)},
			Prefix:      "spi",
		})
		if createErr != nil {
			log.Printf("[daemon] queue row %s: create bead failed: %s", row.ID, createErr)
			errors++
			continue
		}

		// Fetch the created bead for processing
		eventBead, fetchErr := StoreGetBead(eventID)
		if fetchErr != nil {
			log.Printf("[daemon] queue row %s: get bead %s failed: %s", row.ID, eventID, fetchErr)
			errors++
			continue
		}

		// Process the event (existing logic)
		procErr := ProcessWebhookEvent(eventBead)
		if procErr != nil {
			log.Printf("[daemon] queue row %s: process error (will retry): %s", row.ID, procErr)
			errors++
			continue
		}

		// Close the event bead
		StoreCloseBead(eventID)

		// Mark queue row as processed
		_, markErr := DoltSQL(
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

// ResolveWebhookRecipient returns the agent that should receive a webhook notification
// for the given epic. The active attempt bead's agent: label is authoritative; the
// epic's owner: label is used as a fallback for backwards compatibility.
func ResolveWebhookRecipient(attempt *Bead, epicBead Bead) string {
	if attempt != nil {
		if agent := HasLabel(*attempt, "agent:"); agent != "" {
			return agent
		}
	}
	return HasLabel(epicBead, "owner:")
}

// NotifyOwnerIfClaimed sends spire mail to the active attempt's agent if the epic
// has an active attempt bead. Falls back to the owner: label if no attempt exists.
func NotifyOwnerIfClaimed(epicID, linearID, eventType string) error {
	// Check for an active attempt child bead -- the attempt bead's agent: label
	// is authoritative for who is currently working on this epic.
	attempt, err := StoreGetActiveAttempt(epicID)
	if err != nil {
		// Log the invariant violation but don't fail -- fall back to owner: label.
		log.Printf("[daemon] notifyOwnerIfClaimed %s: active attempt query error: %s", epicID, err)
	}

	// Fetch the epic bead for the owner: fallback if needed.
	epicBead, fetchErr := StoreGetBead(epicID)
	if fetchErr != nil {
		return fmt.Errorf("get epic %s: %w", epicID, fetchErr)
	}

	recipient := ResolveWebhookRecipient(attempt, epicBead)
	if recipient == "" {
		// No active attempt agent and no owner label -- nothing to notify.
		return nil
	}

	// Send notification via spire send
	msg := fmt.Sprintf("%s updated (%s)", linearID, eventType)
	return CmdSendFunc([]string{"--as", "spi", recipient, msg, "--ref", epicID})
}
