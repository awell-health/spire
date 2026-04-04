package executor

import (
	"fmt"
	"strings"

	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
)

// actionRecoveryCollectContext is the ActionHandler for "recovery.collect_context".
// It mechanically assembles diagnosis, ranked actions, and prior learnings for
// a recovery bead, then writes the formatted context as a bead comment and
// stashes JSON in state.Vars for in-process access by the decide step.
func actionRecoveryCollectContext(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
	// 1. Get recovery bead and extract source bead ID from metadata.
	recoveryBead, err := e.deps.GetBead(e.beadID)
	if err != nil {
		return ActionResult{Error: fmt.Errorf("get recovery bead %s: %w", e.beadID, err)}
	}

	sourceBeadID := recoveryBead.Meta(recovery.KeySourceBead)
	if sourceBeadID == "" {
		return ActionResult{Error: fmt.Errorf("recovery bead %s has no source_bead metadata", e.beadID)}
	}

	// Read failure classification from recovery bead metadata (set by escalation).
	failureClass := recoveryBead.Meta(recovery.KeyFailureClass)
	failureSig := recoveryBead.Meta(recovery.KeyFailureSignature)

	// 2. Build recovery.Deps bridge from executor deps and diagnose the source bead.
	rdeps := buildExecutorRecoveryDeps(e)
	diag, diagErr := recovery.Diagnose(sourceBeadID, rdeps)
	if diagErr != nil {
		// Diagnosis failure is non-fatal — the source bead may already be resolved.
		// Build a minimal diagnosis stub so the decide step still has context.
		e.log("recovery: diagnose %s failed (non-fatal): %s", sourceBeadID, diagErr)
		diag = &recovery.Diagnosis{
			BeadID:      sourceBeadID,
			FailureMode: recovery.FailureClass(failureClass),
		}
	}

	// If failure class wasn't in metadata, take it from diagnosis.
	if failureClass == "" {
		failureClass = string(diag.FailureMode)
	}

	// 3. Per-bead learnings: closed recovery beads for the same source + failure class.
	reusableTrue := true
	perBeadLearnings, perErr := store.ListClosedRecoveryBeads(store.RecoveryLookupFilter{
		SourceBead:   sourceBeadID,
		FailureClass: failureClass,
		Reusable:     &reusableTrue,
		Limit:        10,
	})
	if perErr != nil {
		e.log("recovery: per-bead learnings query failed: %s", perErr)
	}

	// 4. Cross-bead learnings: reusable learnings for the same failure class, any source.
	crossBeadLearnings, crossErr := store.ListClosedRecoveryBeads(store.RecoveryLookupFilter{
		FailureClass: failureClass,
		Reusable:     &reusableTrue,
		Limit:        5,
	})
	if crossErr != nil {
		e.log("recovery: cross-bead learnings query failed: %s", crossErr)
	}

	// Filter out per-bead entries from cross-bead to avoid double-counting.
	crossBeadLearnings = filterOutSourceBead(crossBeadLearnings, sourceBeadID)

	// 5. Assemble RecoveryContext.
	rc := &RecoveryContext{
		SourceBeadID:       sourceBeadID,
		FailureClass:       failureClass,
		FailureSig:         failureSig,
		Diagnosis:          *diag,
		RankedActions:      diag.Actions,
		PerBeadLearnings:   perBeadLearnings,
		CrossBeadLearnings: crossBeadLearnings,
	}

	// 6. Write bead comment with recovery context markdown.
	md := "## Recovery Context\n\n" + rc.ToMarkdown()
	if e.deps.AddComment != nil {
		if err := e.deps.AddComment(e.beadID, md); err != nil {
			e.log("recovery: write context comment: %s", err)
		}
	}

	// 7. Stash JSON in state.Vars for in-process access by the decide step.
	if rcJSON, jsonErr := rc.ToJSON(); jsonErr == nil {
		if state.Vars == nil {
			state.Vars = make(map[string]string)
		}
		state.Vars["recovery_context"] = string(rcJSON)
	}

	// 8. Determine verification_status for downstream routing.
	// If diagnosis failed due to "no interrupted:* label" the source is clean.
	verificationStatus := "dirty"
	if diagErr != nil && strings.Contains(diagErr.Error(), "no interrupted") {
		verificationStatus = "clean"
	} else if diagErr != nil && strings.Contains(diagErr.Error(), "already closed") {
		verificationStatus = "clean"
	}

	e.log("recovery: collected context for %s (class=%s, verification=%s, per-bead=%d, cross-bead=%d)",
		sourceBeadID, failureClass, verificationStatus, len(perBeadLearnings), len(crossBeadLearnings))

	return ActionResult{Outputs: map[string]string{
		"status":              "collected",
		"failure_class":       failureClass,
		"source_bead":         sourceBeadID,
		"verification_status": verificationStatus,
	}}
}

// buildExecutorRecoveryDeps bridges executor.Deps to recovery.Deps so the
// recovery.Diagnose function can be called from within an action handler.
// Git checks and registry lookups are left nil (optional in Diagnose).
func buildExecutorRecoveryDeps(e *Executor) *recovery.Deps {
	return &recovery.Deps{
		GetBead: func(id string) (recovery.DepBead, error) {
			b, err := e.deps.GetBead(id)
			if err != nil {
				return recovery.DepBead{}, err
			}
			return recovery.DepBead{
				ID:     b.ID,
				Title:  b.Title,
				Status: b.Status,
				Labels: b.Labels,
				Parent: b.Parent,
			}, nil
		},
		GetChildren: func(parentID string) ([]recovery.DepBead, error) {
			children, err := e.deps.GetChildren(parentID)
			if err != nil {
				return nil, err
			}
			result := make([]recovery.DepBead, len(children))
			for i, c := range children {
				result[i] = recovery.DepBead{
					ID:     c.ID,
					Title:  c.Title,
					Status: c.Status,
					Labels: c.Labels,
					Parent: c.Parent,
				}
			}
			return result, nil
		},
		GetDependentsWithMeta: func(id string) ([]recovery.DepDependent, error) {
			if e.deps.GetDependentsWithMeta == nil {
				return nil, nil
			}
			deps, err := e.deps.GetDependentsWithMeta(id)
			if err != nil {
				return nil, err
			}
			result := make([]recovery.DepDependent, len(deps))
			for i, d := range deps {
				result[i] = recovery.DepDependent{
					ID:             d.ID,
					Title:          d.Title,
					Status:         string(d.Status),
					Labels:         d.Labels,
					DependencyType: string(d.DependencyType),
				}
			}
			return result, nil
		},
		AddComment: e.deps.AddComment,
		CloseBead:  e.deps.CloseBead,
		ResolveRepo: func(beadID string) (string, string, error) {
			if e.deps.ResolveRepo == nil {
				return "", "", fmt.Errorf("ResolveRepo not available")
			}
			repoPath, _, baseBranch, err := e.deps.ResolveRepo(beadID)
			return repoPath, baseBranch, err
		},
		ListRecoveryLearnings: func(filter store.RecoveryLookupFilter) ([]store.RecoveryLearning, error) {
			return store.ListClosedRecoveryBeads(filter)
		},
		// Git checks and registry lookup left nil — Diagnose handles nil gracefully.
	}
}

// filterOutSourceBead removes entries whose SourceBead matches the given ID,
// preventing double-counting between per-bead and cross-bead learning sets.
func filterOutSourceBead(learnings []store.RecoveryLearning, sourceBeadID string) []store.RecoveryLearning {
	if sourceBeadID == "" {
		return learnings
	}
	var filtered []store.RecoveryLearning
	for _, l := range learnings {
		if !strings.EqualFold(l.SourceBead, sourceBeadID) {
			filtered = append(filtered, l)
		}
	}
	return filtered
}
