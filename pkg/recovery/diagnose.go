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

	// Accept hooked status (new model) or interrupted:* label (legacy).
	if interruptLabel == "" && bead.Status != "hooked" {
		return nil, fmt.Errorf("bead %s has no interrupted:* label and is not hooked — not in interrupted state", beadID)
	}
	// Synthesize a label for downstream code when only status is set.
	if interruptLabel == "" && bead.Status == "hooked" {
		interruptLabel = "interrupted:hooked"
	}

	// 3. Count attempts and get latest result.
	attemptCount, lastResult := countAttempts(beadID, deps)

	// Parse v3 step context from the attempt result if available.
	var stepContext *StepContext
	if lastResult != "" {
		stepContext = parseStepContext(lastResult)
	}

	// 4. Find alert beads and recovery bead via dependents (single query).
	alertBeads, recoveryBead := findLinkedBeads(beadID, deps)

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
		StepContext:       stepContext,
		Runtime:           runtime,
		Git:               gitState,
		AlertBeads:        alertBeads,
		RecoveryBead:      recoveryBead,
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

// findLinkedBeads finds alert beads and the first open recovery bead linked to
// the parent via dependents. Uses a single GetDependentsWithMeta call.
func findLinkedBeads(parentID string, deps *Deps) ([]AlertInfo, *RecoveryRef) {
	if deps.GetDependentsWithMeta == nil {
		return nil, nil
	}
	dependents, err := deps.GetDependentsWithMeta(parentID)
	if err != nil {
		return nil, nil
	}

	var alerts []AlertInfo
	var recoveryRef *RecoveryRef
	for _, dep := range dependents {
		// Check for recovery-for dependent (first open one wins).
		if dep.DependencyType == "recovery-for" && dep.Status != "closed" && recoveryRef == nil {
			recoveryRef = &RecoveryRef{ID: dep.ID, Title: dep.Title}
			continue
		}

		// Check for alert beads.
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
	return alerts, recoveryRef
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

// parseStepContext extracts v3 node-scoped step context from an attempt result string.
// Expected format: "failure: step <name> action=<action> flow=<flow> workspace=<ws>: <error>"
func parseStepContext(result string) *StepContext {
	if !strings.Contains(result, "step ") {
		return nil
	}
	idx := strings.Index(result, "step ")
	if idx < 0 {
		return nil
	}
	rest := result[idx+5:] // after "step "

	sc := &StepContext{}
	// Parse step name (first token before space or colon).
	parts := strings.Fields(rest)
	if len(parts) == 0 {
		return nil
	}
	sc.StepName = strings.TrimSuffix(parts[0], ":")

	// Parse key=value pairs. Trim trailing colons that appear when the
	// key=value token immediately precedes the ": <error>" separator.
	for _, p := range parts[1:] {
		if strings.HasPrefix(p, "action=") {
			sc.Action = strings.TrimSuffix(strings.TrimPrefix(p, "action="), ":")
		} else if strings.HasPrefix(p, "flow=") {
			sc.Flow = strings.TrimSuffix(strings.TrimPrefix(p, "flow="), ":")
		} else if strings.HasPrefix(p, "workspace=") {
			sc.Workspace = strings.TrimSuffix(strings.TrimPrefix(p, "workspace="), ":")
		}
	}

	if sc.StepName == "" {
		return nil
	}
	return sc
}
