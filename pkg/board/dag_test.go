package board

import (
	"testing"

	"github.com/awell-health/spire/pkg/store"
)

func TestExtractReviewVerdict_FromMetadata(t *testing.T) {
	b := store.Bead{
		ID:       "spi-review-1",
		Metadata: map[string]string{"review_verdict": "approve"},
		// Description also set — metadata should take precedence.
		Description: "verdict: request_changes\n\nsome feedback",
	}
	got := extractReviewVerdict(b)
	if got != "approve" {
		t.Errorf("extractReviewVerdict() = %q, want %q (metadata should take precedence)", got, "approve")
	}
}

func TestExtractReviewVerdict_LegacyFallback(t *testing.T) {
	b := store.Bead{
		ID:          "spi-review-2",
		Description: "verdict: request_changes\n\nMissing error handling",
	}
	got := extractReviewVerdict(b)
	if got != "request_changes" {
		t.Errorf("extractReviewVerdict() = %q, want %q (legacy description parsing)", got, "request_changes")
	}
}

func TestExtractReviewVerdict_EmptyBead(t *testing.T) {
	b := store.Bead{ID: "spi-review-3"}
	got := extractReviewVerdict(b)
	if got != "" {
		t.Errorf("extractReviewVerdict() = %q, want empty for bead with no metadata or description", got)
	}
}

func TestExtractReviewVerdict_NoMatchingDescription(t *testing.T) {
	b := store.Bead{
		ID:          "spi-review-4",
		Description: "some random description without verdict prefix",
	}
	got := extractReviewVerdict(b)
	if got != "" {
		t.Errorf("extractReviewVerdict() = %q, want empty for non-verdict description", got)
	}
}

func TestExtractReviewVerdict_VerdictOnlyLine(t *testing.T) {
	b := store.Bead{
		ID:          "spi-review-5",
		Description: "verdict: approve",
	}
	got := extractReviewVerdict(b)
	if got != "approve" {
		t.Errorf("extractReviewVerdict() = %q, want %q", got, "approve")
	}
}

// TestExtractReviewVerdict_ArbiterOverridesSage verifies the arbiter_verdict
// JSON payload takes precedence over a conflicting review_verdict written by
// sage on the same review-round bead. This is the binding-verdict semantics
// the board is responsible for honoring.
func TestExtractReviewVerdict_ArbiterOverridesSage(t *testing.T) {
	b := store.Bead{
		ID: "spi-review-arb",
		Metadata: map[string]string{
			"arbiter_verdict": `{"source":"arbiter","verdict":"approve","decided_at":"2026-04-20T12:00:00Z"}`,
			"review_verdict":  "reject",
		},
		Description: "verdict: reject\n\nsage was overruled",
	}
	got := extractReviewVerdict(b)
	if got != "approve" {
		t.Errorf("extractReviewVerdict() = %q, want %q (arbiter_verdict must override sage)", got, "approve")
	}
}

// TestExtractReviewVerdict_ArbiterUnparseableFallsBack verifies that a
// malformed arbiter_verdict JSON does not silently swallow the verdict —
// readers fall back to review_verdict so the round still has a usable
// answer.
func TestExtractReviewVerdict_ArbiterUnparseableFallsBack(t *testing.T) {
	b := store.Bead{
		ID: "spi-review-bad",
		Metadata: map[string]string{
			"arbiter_verdict": "{not-json}",
			"review_verdict":  "reject",
		},
	}
	got := extractReviewVerdict(b)
	if got != "reject" {
		t.Errorf("extractReviewVerdict() = %q, want %q (fallback to review_verdict on bad JSON)", got, "reject")
	}
}
