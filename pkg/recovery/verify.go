package recovery

import (
	"fmt"
	"strings"
)

// Verify checks whether the interrupted state has been successfully cleared
// after a recovery action. Returns a clean result if no interrupted:* labels
// remain and no open alerts are linked.
func Verify(beadID string, deps *Deps) (*VerifyResult, error) {
	bead, err := deps.GetBead(beadID)
	if err != nil {
		return nil, err
	}

	result := &VerifyResult{Clean: true, Healthy: true, Status: VerifyHealthy}

	// Check for remaining interrupted:* labels.
	for _, l := range bead.Labels {
		if strings.HasPrefix(l, "interrupted:") {
			result.InterruptLabels = append(result.InterruptLabels, l)
			result.Clean = false
			result.Healthy = false
			result.Status = VerifyDegraded
		}
		if l == "needs-human" {
			result.NeedsHuman = true
			result.Clean = false
			result.Healthy = false
			result.Status = VerifyDegraded
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
						result.Healthy = false
						result.Status = VerifyDegraded
						break
					}
				}
			}
		}
	}

	if !result.Healthy {
		result.Reason = fmt.Sprintf("%d interrupt labels, %d open alerts", len(result.InterruptLabels), result.AlertsOpen)
	}

	return result, nil
}

// CheckSourceHealth returns whether the source bead is in a workable state.
// It is deliberately mechanical: no agent reasoning, no side effects.
// Healthy requires: no interrupted:* labels on source bead, no open
// recovery-for or caused-by dependents (other than selfBeadID) with
// status open or in_progress.
func CheckSourceHealth(deps RecoveryDeps, sourceBead, selfBeadID string) VerifyResult {
	result := VerifyResult{Healthy: true, Status: VerifyHealthy}

	// 1. Fetch source bead; if not found, return unknown.
	bead, err := deps.GetBead(sourceBead)
	if err != nil {
		result.Healthy = false
		result.Status = VerifyUnknown
		result.Reason = fmt.Sprintf("cannot fetch source bead %s: %s", sourceBead, err)
		return result
	}

	// 2. Check for interrupted:* labels.
	for _, l := range bead.Labels {
		if strings.HasPrefix(l, "interrupted:") {
			result.InterruptLabels = append(result.InterruptLabels, l)
			result.Checks = append(result.Checks, fmt.Sprintf("interrupted label: %s", l))
		}
	}
	if len(result.InterruptLabels) > 0 {
		result.Healthy = false
		result.Status = VerifyDegraded
		result.Reason = fmt.Sprintf("source bead has %d interrupted labels", len(result.InterruptLabels))
	}

	// 3. Check dependents for open recovery siblings (excluding self).
	dependents, err := deps.GetDependentsWithMeta(sourceBead)
	if err == nil {
		for _, dep := range dependents {
			if dep.ID == selfBeadID {
				continue
			}
			if !isRecoveryLink(dep.DependencyType) {
				continue
			}
			if dep.Status == "open" || dep.Status == "in_progress" {
				result.Healthy = false
				result.Status = VerifyDegraded
				check := fmt.Sprintf("open recovery sibling: %s (status=%s)", dep.ID, dep.Status)
				result.Checks = append(result.Checks, check)
				if result.Reason == "" {
					result.Reason = check
				}
			}
		}
	}

	// 4. All checks pass if still healthy.
	if result.Healthy {
		result.Checks = append(result.Checks, "no interrupted labels", "no open recovery siblings")
	}

	return result
}
