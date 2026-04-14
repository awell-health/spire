package executor

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/awell-health/spire/pkg/store"
)

// Recovery-wizard handoff protocol
//
// This file defines the communication protocol between recovery agents and
// wizards for cooperative retry. Recovery agents set labels on a target bead
// to request a retry; wizards read those labels, perform the retry, and write
// back the result. This enables a cooperative loop where the recovery agent
// stays alive while the wizard retries from a specific step.
//
// Label conventions:
//
//   recovery:retry-from=<step>              — which step to retry from (e.g., verify-build)
//   recovery:attempt=<N>                    — current attempt number
//   recovery:recovery-bead=<id>             — link back to the recovery bead
//   recovery:status=waiting|succeeded|failed — current recovery handoff status
//   recovery:result=<json>                  — JSON-encoded RetryResult (set on completion)
//   recovery:guidance=<text>                — optional human guidance text

// RetryRequest represents a recovery agent's request for a wizard to retry
// execution from a specific step.
type RetryRequest struct {
	RecoveryBeadID string // ID of the recovery bead making the request
	TargetBeadID   string // ID of the bead to retry
	FromStep       string // which step to retry from
	AttemptNumber  int    // current attempt number
	Guidance       string // optional human guidance text
}

// RetryResult represents the outcome of a wizard retry attempt.
type RetryResult struct {
	Success     bool   `json:"success"`      // whether the retry succeeded
	FailedStep  string `json:"failed_step"`  // step that failed (empty if Success)
	Error       string `json:"error"`        // error message (empty if Success)
	StepReached string `json:"step_reached"` // furthest step reached during retry
}

// Label prefix constants for recovery handoff.
const (
	recoveryLabelRetryFrom    = "recovery:retry-from="
	recoveryLabelAttempt      = "recovery:attempt="
	recoveryLabelRecoveryBead = "recovery:recovery-bead="
	recoveryLabelStatus       = "recovery:status="
	recoveryLabelResult       = "recovery:result="
	recoveryLabelGuidance     = "recovery:guidance="
	recoveryLabelPrefix       = "recovery:"
)

// SetRetryRequest sets recovery labels on the target bead to request a retry.
// Any existing recovery request labels are cleared first to avoid stale values.
func SetRetryRequest(targetBeadID string, req RetryRequest) error {
	// Clear any existing request labels before setting new ones.
	if err := clearRecoveryLabels(targetBeadID, recoveryLabelRetryFrom, recoveryLabelAttempt,
		recoveryLabelRecoveryBead, recoveryLabelStatus, recoveryLabelGuidance); err != nil {
		return fmt.Errorf("clear existing retry request: %w", err)
	}

	labels := []string{
		recoveryLabelRetryFrom + req.FromStep,
		recoveryLabelAttempt + strconv.Itoa(req.AttemptNumber),
		recoveryLabelRecoveryBead + req.RecoveryBeadID,
		recoveryLabelStatus + "waiting",
	}
	if req.Guidance != "" {
		labels = append(labels, recoveryLabelGuidance+req.Guidance)
	}

	for _, l := range labels {
		if err := store.AddLabel(targetBeadID, l); err != nil {
			return fmt.Errorf("set retry label %s: %w", l, err)
		}
	}
	return nil
}

// GetRetryRequest reads a retry request from the target bead's labels.
// Returns (nil, false, nil) if no request is present.
func GetRetryRequest(targetBeadID string) (*RetryRequest, bool, error) {
	b, err := store.GetBead(targetBeadID)
	if err != nil {
		return nil, false, fmt.Errorf("get bead %s: %w", targetBeadID, err)
	}

	fromStep := store.HasLabel(b, recoveryLabelRetryFrom)
	if fromStep == "" {
		return nil, false, nil
	}

	attemptStr := store.HasLabel(b, recoveryLabelAttempt)
	attempt, _ := strconv.Atoi(attemptStr)

	req := &RetryRequest{
		RecoveryBeadID: store.HasLabel(b, recoveryLabelRecoveryBead),
		TargetBeadID:   targetBeadID,
		FromStep:       fromStep,
		AttemptNumber:  attempt,
		Guidance:       store.HasLabel(b, recoveryLabelGuidance),
	}
	return req, true, nil
}

// ClearRetryRequest removes all recovery: labels from the target bead.
func ClearRetryRequest(targetBeadID string) error {
	return clearAllRecoveryLabels(targetBeadID)
}

// SetRetryResult sets recovery:status and stores the result as JSON in a
// recovery:result label on the target bead.
func SetRetryResult(targetBeadID string, result RetryResult) error {
	// Clear existing status and result labels first.
	if err := clearRecoveryLabels(targetBeadID, recoveryLabelStatus, recoveryLabelResult); err != nil {
		return fmt.Errorf("clear existing retry result: %w", err)
	}

	// Determine status from result.
	status := "succeeded"
	if !result.Success {
		status = "failed"
	}

	resultJSON, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("marshal retry result: %w", err)
	}

	if err := store.AddLabel(targetBeadID, recoveryLabelStatus+status); err != nil {
		return fmt.Errorf("set recovery status label: %w", err)
	}
	if err := store.AddLabel(targetBeadID, recoveryLabelResult+string(resultJSON)); err != nil {
		return fmt.Errorf("set recovery result label: %w", err)
	}
	return nil
}

// GetRetryResult reads the retry result from the target bead's labels.
// Returns (nil, false, nil) if no result is set.
func GetRetryResult(targetBeadID string) (*RetryResult, bool, error) {
	b, err := store.GetBead(targetBeadID)
	if err != nil {
		return nil, false, fmt.Errorf("get bead %s: %w", targetBeadID, err)
	}

	resultJSON := store.HasLabel(b, recoveryLabelResult)
	if resultJSON == "" {
		return nil, false, nil
	}

	var result RetryResult
	if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
		return nil, false, fmt.Errorf("unmarshal retry result: %w", err)
	}
	return &result, true, nil
}

// ClearRetryResult clears the result and status labels from the target bead.
func ClearRetryResult(targetBeadID string) error {
	return clearRecoveryLabels(targetBeadID, recoveryLabelStatus, recoveryLabelResult)
}

// IsRetryPending checks whether there is a recovery:status=waiting label on
// the target bead.
func IsRetryPending(targetBeadID string) (bool, error) {
	b, err := store.GetBead(targetBeadID)
	if err != nil {
		return false, fmt.Errorf("get bead %s: %w", targetBeadID, err)
	}
	return store.ContainsLabel(b, recoveryLabelStatus+"waiting"), nil
}

// clearRecoveryLabels removes labels matching any of the given prefixes from
// the bead. Each prefix is matched against the bead's label list.
func clearRecoveryLabels(beadID string, prefixes ...string) error {
	b, err := store.GetBead(beadID)
	if err != nil {
		return fmt.Errorf("get bead %s: %w", beadID, err)
	}
	for _, l := range b.Labels {
		for _, p := range prefixes {
			if strings.HasPrefix(l, p) {
				if err := store.RemoveLabel(beadID, l); err != nil {
					return fmt.Errorf("remove label %s: %w", l, err)
				}
				break
			}
		}
	}
	return nil
}

// clearAllRecoveryLabels removes all labels starting with "recovery:" from the bead.
func clearAllRecoveryLabels(beadID string) error {
	return clearRecoveryLabels(beadID, recoveryLabelPrefix)
}
