package executor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/awell-health/spire/pkg/agent"
	spgit "github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/steveyegge/beads"
)

// ErrDuplicateApprentice signals that a live apprentice already owns a child
// bead, so the dispatch seam refused to spawn a second one. The wave/sequential/
// direct dispatch paths surface this as an ActionResult.Error so the formula
// step parks (hooked) instead of silently advancing the epic over an in-flight
// child — see spi sleep-duplicate-dispatch fix.
//
// Background: when a laptop sleeps mid-epic, agent processes die, the daemon
// orphan-sweep reopens the epic, and a fresh wizard re-dispatches. ComputeWaves
// re-includes still-in_progress children, and the dispatch path had no
// idempotency guard, so a second apprentice was spawned alongside any survivor.
// This sentinel + liveChildSet close that gap as defense-in-depth.
var ErrDuplicateApprentice = errors.New("dispatch: live apprentice already exists for child")

// liveChildSet snapshots the backend's live agents keyed by bead ID. One List()
// call per wave; consulted per child before Spawn.
//
// Keys strictly on Alive==true: a dead/orphaned registry entry (the common
// post-sleep state, before OrphanSweep cleans it) must NOT suppress a needed
// re-spawn, or the child would strand. Liveness is the backend's PID-based,
// zombie-safe probe (pkg/process.ProcessAlive via ProcessBackend.List).
//
// excludeName is the dispatching wizard's own agent name, which is dropped from
// the set. This matters for the direct-dispatch path, where the apprentice is
// spawned under the wizard's OWN bead ID — without the exclusion the wizard's
// own live registry entry would match and the guard would refuse every direct
// dispatch. Wave/sequential children carry distinct bead IDs, so the exclusion
// is a harmless no-op there.
//
// Fails OPEN: a nil spawner or a List() error returns an empty set so a
// transient registry-read blip never blocks legitimate dispatch (mirrors
// OrphanSweep's "transient IsAlive errors fail open" stance).
func liveChildSet(sp agent.Backend, excludeName string) map[string]bool {
	live := map[string]bool{}
	if sp == nil {
		return live
	}
	infos, err := sp.List()
	if err != nil {
		return live
	}
	for _, in := range infos {
		if in.Name == excludeName {
			continue
		}
		if in.Alive && in.BeadID != "" {
			live[in.BeadID] = true
		}
	}
	return live
}

// conflictResolver returns a closure that resolves merge conflicts using a
// given turn budget. When maxTurns is 0, the --max-turns flag is omitted.
func (e *Executor) conflictResolver(maxTurns int) func(string, string) error {
	return func(repoPath, childBranch string) error {
		return e.resolveConflictsWithBudget(repoPath, childBranch, maxTurns)
	}
}

// resolveMergeConflicts is the in-wizard entry point for the Claude-driven
// merge-conflict resolver. It runs the resolver on a provisioned workspace
// and returns when the conflict markers are cleared and the merge commit
// lands. The wizard's recovery-dispatch path calls this via the
// RepairMode=MergeConflictResolution branch; legacy merge flows still call
// resolveConflictsWithBudget directly through the conflictResolver closure.
//
// maxTurns is the Claude CLI turn budget; pass 0 to omit --max-turns and
// defer to the CLI's own ceiling.
func (e *Executor) resolveMergeConflicts(repoPath, childBranch string, maxTurns int) error {
	return e.resolveConflictsWithBudget(repoPath, childBranch, maxTurns)
}

// resolveConflictsWithBudget invokes Claude to resolve merge conflicts.
// If maxTurns > 0, --max-turns is passed to the Claude CLI; otherwise the flag
// is omitted (letting the CLI's own default or timeout govern).
func (e *Executor) resolveConflictsWithBudget(repoPath, childBranch string, maxTurns int) error {
	wc := &spgit.WorktreeContext{Dir: repoPath}

	// Get the list of conflicted files
	conflicted, err := wc.ConflictedFiles()
	if err != nil {
		return fmt.Errorf("list conflicts: %w", err)
	}
	if len(conflicted) == 0 {
		return fmt.Errorf("no conflicted files found")
	}
	conflictedFiles := strings.Join(conflicted, "\n")

	// Build a prompt with the conflicts
	prompt := fmt.Sprintf(`You are resolving merge conflicts for branch %s being merged into the staging branch.

The following files have conflicts. For each file, read it, resolve the conflict markers (<<<<<<< ======= >>>>>>>), and write the resolved version. Keep both sides' changes where they don't contradict. When they do contradict, prefer the incoming branch (%s) since it has the newer implementation.

Conflicted files:
%s

After resolving all conflicts, stage them with git add.
Do NOT commit — the merge commit will be created automatically.`,
		childBranch, childBranch, conflictedFiles)

	// Invoke Claude to resolve
	args := buildConflictResolverArgs(prompt, repoconfig.ResolveModel("", e.repoModel()), maxTurns)
	cmd := exec.Command("claude", args...)
	cmd.Dir = repoPath
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("claude resolve: %w", err)
	}

	// Verify all conflicts are resolved (no more conflict markers)
	status := wc.StatusPorcelain()
	if strings.Contains(status, "UU ") {
		return fmt.Errorf("conflicts still unresolved after Claude")
	}

	// Complete the merge
	if commitErr := wc.CommitMerge(); commitErr != nil {
		return fmt.Errorf("commit merge: %w", commitErr)
	}

	e.log("  conflicts resolved by Claude")
	return nil
}

// buildConflictResolverArgs constructs the Claude CLI args for conflict
// resolution. If maxTurns > 0, --max-turns is included; otherwise it is
// omitted so the executor does not invent a turn budget.
func buildConflictResolverArgs(prompt, model string, maxTurns int) []string {
	args := []string{
		"--dangerously-skip-permissions",
		"-p", prompt,
		"--model", model,
	}
	if maxTurns > 0 {
		args = append(args, "--max-turns", fmt.Sprintf("%d", maxTurns))
	}
	return args
}

// ComputeWaves topologically sorts open children of epicID into dependency waves.
func ComputeWaves(epicID string, deps *Deps) ([][]string, error) {
	children, err := deps.GetChildren(epicID)
	if err != nil {
		return nil, fmt.Errorf("get children: %w", err)
	}

	// Filter to open subtasks only — exclude internal DAG beads.
	var openIDs []string
	for _, c := range children {
		if c.Status == "closed" {
			continue
		}
		if deps.IsAttemptBead(c) || deps.IsStepBead(c) || deps.IsReviewRoundBead(c) {
			continue
		}
		openIDs = append(openIDs, c.ID)
	}

	if len(openIDs) == 0 {
		return nil, nil
	}

	// Build a set of open child IDs for fast lookup.
	childSet := make(map[string]bool)
	for _, id := range openIDs {
		childSet[id] = true
	}

	// Get blocked issues to determine dependencies.
	blockedBeads, _ := deps.GetBlockedIssues(beads.WorkFilter{})

	// Build dep map: childID -> []blockerIDs (only blockers that are also open children).
	depMap := make(map[string][]string)
	for _, bb := range blockedBeads {
		if !childSet[bb.ID] {
			continue
		}
		for _, dep := range bb.Dependencies {
			blockerID := dep.DependsOnID
			if childSet[blockerID] {
				depMap[bb.ID] = append(depMap[bb.ID], blockerID)
			}
		}
	}

	// Topological sort into waves.
	assigned := make(map[string]int) // ID -> wave number
	var waves [][]string

	for len(assigned) < len(openIDs) {
		var wave []string
		waveNum := len(waves)

		for _, id := range openIDs {
			if _, done := assigned[id]; done {
				continue
			}
			ready := true
			for _, dep := range depMap[id] {
				if _, done := assigned[dep]; !done {
					ready = false
					break
				}
			}
			if ready {
				wave = append(wave, id)
			}
		}

		if len(wave) == 0 {
			// Dependency cycle detected — fail closed.
			var stuck []string
			for _, id := range openIDs {
				if _, done := assigned[id]; !done {
					stuck = append(stuck, id)
				}
			}
			return nil, fmt.Errorf("dependency cycle detected among beads: %v", stuck)
		}

		for _, id := range wave {
			assigned[id] = waveNum
		}
		waves = append(waves, wave)
	}

	return waves, nil
}
