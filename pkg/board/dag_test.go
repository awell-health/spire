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
