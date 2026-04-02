package recovery

import "strings"

// Verify checks whether the interrupted state has been successfully cleared
// after a recovery action. Returns a clean result if no interrupted:* labels
// remain and no open alerts are linked.
func Verify(beadID string, deps *Deps) (*VerifyResult, error) {
	bead, err := deps.GetBead(beadID)
	if err != nil {
		return nil, err
	}

	result := &VerifyResult{Clean: true}

	// Check for remaining interrupted:* labels.
	for _, l := range bead.Labels {
		if strings.HasPrefix(l, "interrupted:") {
			result.InterruptLabels = append(result.InterruptLabels, l)
			result.Clean = false
		}
		if l == "needs-human" {
			result.NeedsHuman = true
			result.Clean = false
		}
	}

	// Count open alert beads still linked.
	if deps.GetDependentsWithMeta != nil {
		dependents, err := deps.GetDependentsWithMeta(beadID)
		if err == nil {
			for _, dep := range dependents {
				if dep.DependencyType != "caused-by" && dep.DependencyType != "related" {
					continue
				}
				if dep.Status == "closed" {
					continue
				}
				for _, l := range dep.Labels {
					if l == "alert" || strings.HasPrefix(l, "alert:") {
						result.AlertsOpen++
						result.Clean = false
						break
					}
				}
			}
		}
	}

	return result, nil
}
