package main

import "github.com/awell-health/spire/pkg/board"

// Phase definitions (validPhases, isValidPhase) live in pkg/formula
// and are re-exported via formula_bridge.go.

// getPhase returns the current phase of a bead.
// Checks for an active step bead first (primary), then falls back to phase:X label.
// Returns "" if neither source indicates a phase (treated as READY by callers).
func getPhase(b Bead) string {
	return board.GetPhase(b)
}
