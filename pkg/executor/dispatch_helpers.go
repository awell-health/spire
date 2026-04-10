package executor

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	spgit "github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/steveyegge/beads"
)

// conflictResolver returns a closure that resolves merge conflicts using a
// given turn budget. When maxTurns is 0, the --max-turns flag is omitted.
func (e *Executor) conflictResolver(maxTurns int) func(string, string) error {
	return func(repoPath, childBranch string) error {
		return e.resolveConflictsWithBudget(repoPath, childBranch, maxTurns)
	}
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
