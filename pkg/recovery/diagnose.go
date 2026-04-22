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

	// 3. Count attempts and get latest result.
	attemptCount, lastResult := countAttempts(beadID, deps)

	// Parse v3 step context from the attempt result if available.
	var stepContext *StepContext
	if lastResult != "" {
		stepContext = parseStepContext(lastResult)
	}

	// 4. Find alert beads and recovery bead via dependents (single query).
	alertBeads, recoveryBead := findLinkedBeads(beadID, deps)

	// For hooked beads without an interrupted:* label, require failure evidence
	// (a recovery bead or alert beads). Approval/design gates are hooked but
	// have no failure artifacts — those are not recoverable.
	if interruptLabel == "" && bead.Status == "hooked" {
		if recoveryBead != nil {
			// Recovery bead exists — real failure. Use alert label for classification if available.
			interruptLabel = "interrupted:hooked"
			for _, a := range alertBeads {
				if strings.HasPrefix(a.Label, "alert:") {
					interruptLabel = "interrupted:" + strings.TrimPrefix(a.Label, "alert:")
					break
				}
			}
		} else if len(alertBeads) > 0 {
			// Alert beads but no recovery bead — still failure evidence.
			if strings.HasPrefix(alertBeads[0].Label, "alert:") {
				interruptLabel = "interrupted:" + strings.TrimPrefix(alertBeads[0].Label, "alert:")
			} else {
				interruptLabel = "interrupted:hooked"
			}
		} else {
			// No failure evidence — this is an approval gate or design wait, not a failure.
			return nil, fmt.Errorf("bead %s is hooked but has no failure evidence (no recovery or alert beads) — likely an approval gate, not recoverable", beadID)
		}
	}

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

	// 9. Resource-scoped enrichment. Wizard-failure classes leave this nil
	// so existing callers see no shape change. Missing operator stamps are
	// tolerated — extractResourceContext returns (nil, false) and diagnose
	// proceeds as normal.
	var resourceCtx *ResourceContext
	if fc.IsResourceScoped() {
		if rc, ok := extractResourceContext(bead, deps); ok {
			resourceCtx = rc
		}
	}

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
		ResourceContext:   resourceCtx,
	}, nil
}

// Operator-stamped wisp metadata keys consumed by extractResourceContext.
// Documented in pkg/recovery/README.md's "Resource-scoped recoveries"
// section so operator-side code can target them.
const (
	metaKeySourceResourceURI = "source-resource-uri"
	metaKeyTerminationLog    = "termination-log"
	metaKeyConditionSnapshot = "condition-snapshot"
)

// extractResourceContext reads operator-stamped wisp metadata and resolves
// the wisp's single caused-by target to assemble a ResourceContext for
// resource-scoped recoveries. Returns (nil, false) when no fields are
// populated so the caller can leave Diagnosis.ResourceContext nil.
//
// Missing individual metadata keys are tolerated — the renderer fills in
// "<not provided>". Multiple caused-by targets are unexpected; the first
// is used and a warning is logged (diagnose never errors on absent or
// surprising operator stamps).
func extractResourceContext(bead DepBead, deps *Deps) (*ResourceContext, bool) {
	rc := &ResourceContext{}
	if bead.Metadata != nil {
		rc.SourceResourceURI = bead.Metadata[metaKeySourceResourceURI]
		rc.TerminationLog = bead.Metadata[metaKeyTerminationLog]
		rc.ConditionSnapshot = bead.Metadata[metaKeyConditionSnapshot]
	}

	if deps != nil && deps.GetDepsWithMeta != nil {
		if targets, err := deps.GetDepsWithMeta(bead.ID); err == nil {
			var pinnedIDs []string
			for _, t := range targets {
				if t.DependencyType == "caused-by" {
					pinnedIDs = append(pinnedIDs, t.ID)
				}
			}
			if len(pinnedIDs) > 1 {
				log.Printf("[recovery] warning: wisp %s has %d caused-by targets; using first (%s)",
					bead.ID, len(pinnedIDs), pinnedIDs[0])
			}
			if len(pinnedIDs) > 0 {
				rc.PinnedIdentityBeadID = pinnedIDs[0]
				if deps.GetBead != nil {
					if pinned, err := deps.GetBead(pinnedIDs[0]); err == nil {
						rc.PinnedIdentityDescription = pinned.Description
					}
				}
			}
		}
	}

	populated := rc.SourceResourceURI != "" ||
		rc.ConditionSnapshot != "" ||
		rc.TerminationLog != "" ||
		rc.PinnedIdentityBeadID != "" ||
		rc.PinnedIdentityDescription != ""
	if !populated {
		return nil, false
	}
	return rc, true
}

// FormatResourceContext renders a ResourceContext into a prompt-ready
// markdown block suitable for inclusion in the cleric's decide prompt
// alongside the existing git-state / log-tail sections. Missing fields
// render as "<not provided>" so downstream agents can still reason about
// what stamps are absent.
//
// Returns "" for a nil ResourceContext so callers can unconditionally
// concatenate the result.
func FormatResourceContext(rc *ResourceContext) string {
	if rc == nil {
		return ""
	}
	orDefault := func(s string) string {
		if s == "" {
			return "<not provided>"
		}
		return s
	}
	var b strings.Builder
	b.WriteString("### Resource context\n")
	b.WriteString("- URI: ")
	b.WriteString(orDefault(rc.SourceResourceURI))
	b.WriteString("\n- Conditions: ")
	b.WriteString(orDefault(rc.ConditionSnapshot))
	b.WriteString("\n- Termination log tail: ")
	b.WriteString(orDefault(rc.TerminationLog))
	b.WriteString("\n\n### Identity\n")
	b.WriteString("- Bead: ")
	b.WriteString(orDefault(rc.PinnedIdentityBeadID))
	b.WriteString("\n- Description: ")
	b.WriteString(orDefault(rc.PinnedIdentityDescription))
	b.WriteString("\n")
	return b.String()
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
		// Check for recovery beads: legacy recovery-for edges OR
		// current caused-by edges with recovery-bead label.
		if dep.Status != "closed" && recoveryRef == nil {
			if dep.DependencyType == "recovery-for" {
				recoveryRef = &RecoveryRef{ID: dep.ID, Title: dep.Title}
				continue
			}
			if dep.DependencyType == "caused-by" {
				for _, l := range dep.Labels {
					if l == "recovery-bead" {
						recoveryRef = &RecoveryRef{ID: dep.ID, Title: dep.Title}
						break
					}
				}
				if recoveryRef != nil {
					continue
				}
			}
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
