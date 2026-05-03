package gateway

import "github.com/awell-health/spire/pkg/store"

// LegacyStatusFor maps a Landing-3 bead status back to its pre-Landing-3
// equivalent so the desktop, board, and any other API consumer that still
// speaks the older taxonomy keeps rendering the right column for one
// release. The mapping mirrors the design's open-question resolution in
// spi-sqqero: review/changes/merge-pending all collapse onto in_progress
// (the single legacy bucket for "actor is doing work"), and awaiting_human
// collapses onto open (the legacy bucket for "waiting on a human").
//
// TODO(spi-a76fxv): remove this shim — and the legacy_status field on the
// gateway bead responses — in the release after Landing 3 lands. By then
// every consumer should be reading the canonical `status` field directly.
func LegacyStatusFor(status string) string {
	switch status {
	case "awaiting_review", "needs_changes", "merge_pending":
		return "in_progress"
	case "awaiting_human":
		return "open"
	default:
		return status
	}
}

// beadResponse is the JSON shape the gateway emits for a single bead. It
// embeds store.Bead so every existing field keeps its name and tag, and
// adds the legacy_status shim alongside.
type beadResponse struct {
	store.Bead
	LegacyStatus string `json:"legacy_status"`
}

// wrapBead returns the response shape for a single bead.
func wrapBead(b store.Bead) beadResponse {
	return beadResponse{Bead: b, LegacyStatus: LegacyStatusFor(b.Status)}
}

// wrapBeads returns the response shape for a slice of beads.
func wrapBeads(bs []store.Bead) []beadResponse {
	out := make([]beadResponse, len(bs))
	for i, b := range bs {
		out[i] = wrapBead(b)
	}
	return out
}

// wrapBeadPtr returns the response shape for a *store.Bead, preserving nil.
func wrapBeadPtr(b *store.Bead) *beadResponse {
	if b == nil {
		return nil
	}
	r := wrapBead(*b)
	return &r
}
