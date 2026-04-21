package executor

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/awell-health/spire/pkg/recovery"
)

// runVerifyPlan is the cleric-side entry point for the verify step's
// VerifyKind dispatch (design spi-h32xj §5). It sets a RetryRequest on the
// target bead carrying plan.Verify, then polls the target for the wizard's
// reported verdict. All Kind variants share the cooperative retry protocol:
// the wizard branches on VerifyPlan.Kind to decide between skipping to a
// named step (rerun-step), executing a narrow command in its worktree
// (narrow-check), or running a captured recipe postcondition
// (recipe-postcondition, chunk-7 stub).
//
// mappedStep is the wizard-phase fallback used when plan.Verify.StepName is
// empty — this preserves backward compat with pre-chunk-5 decide outputs
// that did not populate VerifyPlan at all.
func runVerifyPlan(
	e *Executor,
	plan recovery.RepairPlan,
	sourceBeadID, mappedStep string,
	attemptNumber int,
	guidance string,
	state *GraphState,
) (recovery.VerifyVerdict, *RetryResult, error) {
	verify := plan.Verify
	if verify.Kind == "" {
		verify.Kind = recovery.VerifyKindRerunStep
	}
	if verify.Kind == recovery.VerifyKindRerunStep && verify.StepName == "" {
		verify.StepName = mappedStep
	}

	fromStep := verify.StepName
	if fromStep == "" {
		fromStep = mappedStep
	}

	req := RetryRequest{
		RecoveryBeadID: e.beadID,
		TargetBeadID:   sourceBeadID,
		FromStep:       fromStep,
		AttemptNumber:  attemptNumber,
		Guidance:       guidance,
		VerifyPlan:     &verify,
	}

	if err := SetRetryRequest(sourceBeadID, req); err != nil {
		return recovery.VerifyVerdictFail, nil, fmt.Errorf("set retry request: %w", err)
	}
	e.log("recovery: verify: retry request set on %s (kind=%s, step=%s, attempt=%d)",
		sourceBeadID, verify.Kind, fromStep, attemptNumber)

	return pollRetryResult(e, sourceBeadID, state)
}

// pollRetryResult waits for the target bead's wizard to publish a
// RetryResult label, or for the default verify timeout to elapse. Returns
// the verdict and the full RetryResult for the caller to populate step
// outputs. On non-pass outcomes the recovery context is rebuilt so the
// next decide iteration sees fresh diagnostics.
func pollRetryResult(e *Executor, sourceBeadID string, state *GraphState) (recovery.VerifyVerdict, *RetryResult, error) {
	pollInterval := time.Duration(DefaultVerifyPollInterval) * time.Second
	timeout := time.Duration(DefaultVerifyTimeout) * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = ClearRetryRequest(sourceBeadID)
			e.log("recovery: verify: polling timed out after %s", timeout)
			return recovery.VerifyVerdictTimeout, nil, nil
		case <-ticker.C:
		}

		result, found, err := GetRetryResult(sourceBeadID)
		if err != nil {
			e.log("recovery: verify: poll error: %v", err)
			continue
		}
		if !found {
			continue
		}

		verdict := resolveVerifyVerdict(result)

		if verdict == recovery.VerifyVerdictPass {
			_ = ClearRetryRequest(sourceBeadID)
			_ = ClearRetryResult(sourceBeadID)
			e.log("recovery: verify: retry passed (step_reached=%s)", result.StepReached)
			return verdict, result, nil
		}

		_ = ClearRetryResult(sourceBeadID)
		e.log("recovery: verify: retry %s (failed_step=%s, error=%s)",
			verdict, result.FailedStep, result.Error)
		rebuildRecoveryContext(e, state)
		return verdict, result, nil
	}
}

// resolveVerifyVerdict returns the canonical VerifyVerdict from a
// RetryResult. Chunk-5-aware wizards populate Verdict directly; older
// wizards only set the legacy Success bool, so fall back to that when
// Verdict is empty.
func resolveVerifyVerdict(result *RetryResult) recovery.VerifyVerdict {
	if result == nil {
		return recovery.VerifyVerdictFail
	}
	if result.Verdict != "" {
		return result.Verdict
	}
	if result.Success {
		return recovery.VerifyVerdictPass
	}
	return recovery.VerifyVerdictFail
}

// rebuildRecoveryContext re-runs BuildRecoveryContext and stores the
// refreshed snapshot in state.Vars so the next decide iteration starts
// from up-to-date git / attempt-history diagnostics.
func rebuildRecoveryContext(e *Executor, state *GraphState) {
	if state == nil {
		return
	}
	repoPath := e.effectiveRepoPath()
	var db *sql.DB
	if e.deps != nil && e.deps.DoltDB != nil {
		db = e.deps.DoltDB()
	}
	fresh, err := BuildRecoveryContext(db, repoPath, e.beadID)
	if err != nil {
		return
	}
	freshJSON, err := json.Marshal(fresh)
	if err != nil {
		return
	}
	if state.Vars == nil {
		state.Vars = make(map[string]string)
	}
	state.Vars["full_recovery_context"] = string(freshJSON)
}

// verdictToDecision translates a VerifyVerdict to the cleric's terminal
// Decision: pass → resume, anything else → escalate. This is the glue
// between the per-plan verdict and the steward-visible outcome consumed
// by the hooked parent.
func verdictToDecision(v recovery.VerifyVerdict) recovery.Decision {
	if v == recovery.VerifyVerdictPass {
		return recovery.DecisionResume
	}
	return recovery.DecisionEscalate
}
