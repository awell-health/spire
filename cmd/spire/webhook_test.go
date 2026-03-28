package main

import (
	"testing"

	"github.com/awell-health/spire/pkg/integration"
)

// --- resolveWebhookRecipient tests ---

// TestWebhookRecipient_AttemptAgent verifies that the attempt bead's agent:
// label takes priority over the epic's owner: label.
func TestWebhookRecipient_AttemptAgent(t *testing.T) {
	attempt := &Bead{
		ID:     "spi-epic.1",
		Title:  "attempt: wizard-impl",
		Status: "in_progress",
		Labels: []string{"attempt", "agent:wizard-impl"},
	}
	epicBead := Bead{
		ID:     "spi-epic",
		Labels: []string{"owner:old-owner"},
	}

	got := integration.ResolveWebhookRecipient(attempt, epicBead)
	if got != "wizard-impl" {
		t.Errorf("ResolveWebhookRecipient = %q, want wizard-impl", got)
	}
}

// TestWebhookRecipient_AttemptAgentOwnerMissing verifies routing works when
// owner: label is absent and only the attempt bead carries agent info.
func TestWebhookRecipient_AttemptAgentOwnerMissing(t *testing.T) {
	attempt := &Bead{
		ID:     "spi-epic.1",
		Title:  "attempt: wizard-impl",
		Status: "in_progress",
		Labels: []string{"attempt", "agent:wizard-impl"},
	}
	epicBead := Bead{
		ID:     "spi-epic",
		Labels: nil,
	}

	got := integration.ResolveWebhookRecipient(attempt, epicBead)
	if got != "wizard-impl" {
		t.Errorf("ResolveWebhookRecipient = %q, want wizard-impl", got)
	}
}

// TestWebhookRecipient_AttemptAgentOwnerStale verifies that a stale owner:
// label is overridden when an active attempt bead has an agent: label.
func TestWebhookRecipient_AttemptAgentOwnerStale(t *testing.T) {
	attempt := &Bead{
		ID:     "spi-epic.2",
		Title:  "attempt: wizard-new",
		Status: "in_progress",
		Labels: []string{"attempt", "agent:wizard-new"},
	}
	epicBead := Bead{
		ID:     "spi-epic",
		Labels: []string{"owner:wizard-stale"},
	}

	got := integration.ResolveWebhookRecipient(attempt, epicBead)
	if got != "wizard-new" {
		t.Errorf("ResolveWebhookRecipient = %q, want wizard-new (not stale owner)", got)
	}
}

// TestWebhookRecipient_NoAttemptFallsBackToOwner verifies that when there is
// no active attempt, the owner: label is used as the fallback recipient.
func TestWebhookRecipient_NoAttemptFallsBackToOwner(t *testing.T) {
	epicBead := Bead{
		ID:     "spi-epic",
		Labels: []string{"owner:wizard-owner"},
	}

	got := integration.ResolveWebhookRecipient(nil, epicBead)
	if got != "wizard-owner" {
		t.Errorf("ResolveWebhookRecipient = %q, want wizard-owner", got)
	}
}

// TestWebhookRecipient_NoAttemptNoOwner verifies empty string is returned when
// neither attempt nor owner: label is present (no notification sent).
func TestWebhookRecipient_NoAttemptNoOwner(t *testing.T) {
	epicBead := Bead{
		ID:     "spi-epic",
		Labels: nil,
	}

	got := integration.ResolveWebhookRecipient(nil, epicBead)
	if got != "" {
		t.Errorf("ResolveWebhookRecipient = %q, want empty string", got)
	}
}

// TestWebhookRecipient_AttemptWithoutAgentLabel verifies fallback to owner:
// when the attempt bead exists but has no agent: label.
func TestWebhookRecipient_AttemptWithoutAgentLabel(t *testing.T) {
	attempt := &Bead{
		ID:     "spi-epic.1",
		Title:  "attempt: unlabelled",
		Status: "in_progress",
		Labels: []string{"attempt"}, // no agent: label
	}
	epicBead := Bead{
		ID:     "spi-epic",
		Labels: []string{"owner:wizard-fallback"},
	}

	got := integration.ResolveWebhookRecipient(attempt, epicBead)
	if got != "wizard-fallback" {
		t.Errorf("ResolveWebhookRecipient = %q, want wizard-fallback", got)
	}
}
