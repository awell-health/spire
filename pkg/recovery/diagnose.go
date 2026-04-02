package recovery

import (
	"fmt"
	"log"
	"strings"
)

// Diagnose gathers runtime state, inspects bead relationships, classifies the
// failure mode, and builds a ranked list of recovery actions for an interrupted
// parent bead.
func Diagnose(beadID string, deps *Deps) (*Diagnosis, error) {
	// 1. Fetch the bead.
	bead, err := deps.GetBead(beadID)
	if err != nil {
		return nil, fmt.Errorf("get bead %s: %w", beadID, err)
	}

	if bead.Status == "closed" {
		return nil, fmt.Errorf("bead %s is already closed", beadID)
	}

	// 2. Extract interrupted:* and phase:* labels.
	var interruptLabel string
	var phaseLabel string
	for _, l := range bead.Labels {
		if strings.HasPrefix(l, "interrupted:") {
			if interruptLabel != "" {
				log.Printf("[recovery] warning: multiple interrupted:* labels on %s, using first: %s", beadID, interruptLabel)
			} else {
				interruptLabel = l
			}
		}
		if strings.HasPrefix(l, "phase:") {
			phaseLabel = strings.TrimPrefix(l, "phase:")
		}
	}

	if interruptLabel == "" {
		return nil, fmt.Errorf("bead %s has no interrupted:* label — not in interrupted state", beadID)
	}

	// 3. Count attempts and get latest result.
	attemptCount, lastResult := countAttempts(beadID, deps)

	// 4. Find alert beads via dependents.
	alertBeads := findAlertBeads(beadID, deps)

	// 5. Load executor state (best-effort).
	var runtime *RuntimeState
	wizardName := "wizard-" + beadID

	// Try to derive agent name from latest attempt's agent:* label.
	if agentName := findAgentName(beadID, deps); agentName != "" {
		wizardName = agentName
	}

	if deps.LoadExecutorState != nil {
		rt, err := deps.LoadExecutorState(wizardName)
		if err == nil {
			runtime = rt
		}
	}

	// 6. Check git state.
	var gitState *GitState
	if deps.ResolveRepo != nil {
		gitState = checkGitState(beadID, bead, runtime, deps)
	}

	// 7. Check wizard registry.
	var wizardRunning bool
	if deps.LookupRegistry != nil {
		name, _, alive, err := deps.LookupRegistry(beadID)
		if err == nil {
			wizardRunning = alive
			if name != "" {
				wizardName = name
			}
		}
	}

	// 8. Classify and build actions.
	fc := classifyInterruptLabel(interruptLabel)
	actions := buildActions(fc, beadID, attemptCount, gitState)

	return &Diagnosis{
		BeadID:            beadID,
		Title:             bead.Title,
		Status:            bead.Status,
		FailureMode:       fc,
		InterruptLabel:    interruptLabel,
		Phase:             phaseLabel,
		AttemptCount:      attemptCount,
		LastAttemptResult: lastResult,
		Runtime:           runtime,
		Git:               gitState,
		AlertBeads:        alertBeads,
		WizardRunning:     wizardRunning,
		WizardName:        wizardName,
		Actions:           actions,
	}, nil
}

// countAttempts counts attempt beads for the parent and returns the latest
// attempt's result label (if closed).
func countAttempts(parentID string, deps *Deps) (int, string) {
	children, err := deps.GetChildren(parentID)
	if err != nil {
		return 0, ""
	}

	var count int
	var latestResult string
	for _, child := range children {
		isAttempt := false
		for _, l := range child.Labels {
			if l == "attempt" {
				isAttempt = true
				break
			}
		}
		if !isAttempt {
			continue
		}
		count++
		// Check for result:* label on closed attempts.
		if child.Status == "closed" {
			for _, l := range child.Labels {
				if strings.HasPrefix(l, "result:") {
					latestResult = strings.TrimPrefix(l, "result:")
				}
			}
		}
	}
	return count, latestResult
}

// findAlertBeads finds alert beads linked to the parent via caused-by or related deps.
func findAlertBeads(parentID string, deps *Deps) []AlertInfo {
	if deps.GetDependentsWithMeta == nil {
		return nil
	}
	dependents, err := deps.GetDependentsWithMeta(parentID)
	if err != nil {
		return nil
	}

	var alerts []AlertInfo
	for _, dep := range dependents {
		if dep.DependencyType != "caused-by" && dep.DependencyType != "related" {
			continue
		}
		if dep.Status == "closed" {
			continue
		}
		for _, l := range dep.Labels {
			if l == "alert" || strings.HasPrefix(l, "alert:") {
				alerts = append(alerts, AlertInfo{
					ID:    dep.ID,
					Label: l,
				})
				break
			}
		}
	}
	return alerts
}

// findAgentName extracts the agent name from the latest attempt's agent:* label.
func findAgentName(parentID string, deps *Deps) string {
	children, err := deps.GetChildren(parentID)
	if err != nil {
		return ""
	}

	var agentName string
	for _, child := range children {
		isAttempt := false
		for _, l := range child.Labels {
			if l == "attempt" {
				isAttempt = true
			}
			if strings.HasPrefix(l, "agent:") {
				agentName = strings.TrimPrefix(l, "agent:")
			}
		}
		if isAttempt && agentName != "" {
			// Keep going — we want the last (most recent) attempt's agent name.
			// Children are not guaranteed sorted, but the latest agent name
			// is usually the same across attempts.
		}
	}
	return agentName
}

// checkGitState checks branch and worktree existence for the bead's feature branch.
func checkGitState(beadID string, bead DepBead, runtime *RuntimeState, deps *Deps) *GitState {
	// Derive branch name from labels or runtime state.
	var branchName string
	for _, l := range bead.Labels {
		if strings.HasPrefix(l, "feat-branch:") {
			branchName = strings.TrimPrefix(l, "feat-branch:")
			break
		}
	}
	if branchName == "" && runtime != nil && runtime.StagingBranch != "" {
		branchName = runtime.StagingBranch
	}
	if branchName == "" {
		branchName = "feat/" + beadID
	}

	repoPath, _, err := deps.ResolveRepo(beadID)
	if err != nil {
		return &GitState{BranchName: branchName}
	}

	gs := &GitState{BranchName: branchName}

	if deps.CheckBranchExists != nil {
		gs.BranchExists = deps.CheckBranchExists(repoPath, branchName)
	}

	// Derive worktree dir from runtime state or convention.
	var worktreeDir string
	if runtime != nil && runtime.WorktreeDir != "" {
		worktreeDir = runtime.WorktreeDir
	}

	if worktreeDir != "" {
		if deps.CheckWorktreeExists != nil {
			gs.WorktreeExists = deps.CheckWorktreeExists(worktreeDir)
		}
		if gs.WorktreeExists && deps.CheckWorktreeDirty != nil {
			gs.WorktreeDirty = deps.CheckWorktreeDirty(worktreeDir)
		}
	}

	return gs
}
