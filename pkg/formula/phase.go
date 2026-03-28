// Package formula provides formula parsing, resolution, and phase definitions
// for the Spire universal phase pipeline. It is self-contained: no imports
// of pkg/store or pkg/config.
package formula

// ValidPhases lists the 5 universal phases in pipeline order.
var ValidPhases = []string{"design", "plan", "implement", "review", "merge"}

// IsValidPhase checks if a phase name is one of the 5 universal phases.
func IsValidPhase(phase string) bool {
	for _, p := range ValidPhases {
		if p == phase {
			return true
		}
	}
	return false
}
