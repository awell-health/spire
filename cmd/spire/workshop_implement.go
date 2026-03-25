package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/steveyegge/beads"
)

// computeWaves takes an epic ID and returns waves — groups of subtask IDs
// that can be executed in parallel. Wave 0 has no deps, wave 1 depends
// on wave 0, etc.
func computeWaves(epicID string) ([][]string, error) {
	children, err := storeGetChildren(epicID)
	if err != nil {
		return nil, fmt.Errorf("get children: %w", err)
	}

	// Filter to open subtasks only.
	var openIDs []string
	for _, c := range children {
		if c.Status != "closed" {
			openIDs = append(openIDs, c.ID)
		}
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
	blockedBeads, _ := storeGetBlockedIssues(beads.WorkFilter{})

	// Build dep map: childID -> []blockerIDs (only blockers that are also open children).
	deps := make(map[string][]string)
	for _, bb := range blockedBeads {
		if !childSet[bb.ID] {
			continue
		}
		for _, dep := range bb.Dependencies {
			blockerID := dep.DependsOnID
			if childSet[blockerID] {
				deps[bb.ID] = append(deps[bb.ID], blockerID)
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
			for _, dep := range deps[id] {
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
			// Circular dependency or stuck — add remaining as final wave.
			for _, id := range openIDs {
				if _, done := assigned[id]; !done {
					wave = append(wave, id)
				}
			}
		}

		for _, id := range wave {
			assigned[id] = waveNum
		}
		waves = append(waves, wave)
	}

	return waves, nil
}

// workshopImplement handles the implement phase of the wizard workshop.
// It computes waves from the dependency graph, dispatches apprentices
// in parallel worktrees, and merges their work back.
func workshopImplement(state *workshopState) error {
	epicID := state.EpicID
	log := func(format string, a ...interface{}) {
		fmt.Fprintf(os.Stderr, "[workshop] "+format+"\n", a...)
	}

	waves, err := computeWaves(epicID)
	if err != nil {
		return err
	}
	if len(waves) == 0 {
		log("no open subtasks — transitioning to review")
		setPhase(epicID, "review")
		state.Phase = "review"
		return nil
	}

	log("computed %d wave(s)", len(waves))
	for i, wave := range waves {
		log("wave %d: %v", i, wave)
	}

	startWave := state.Wave

	repoPath, _, _, err := wizardResolveRepo(epicID)
	if err != nil {
		return fmt.Errorf("resolve repo: %w", err)
	}

	for waveIdx := startWave; waveIdx < len(waves); waveIdx++ {
		wave := waves[waveIdx]
		state.Wave = waveIdx
		log("=== wave %d: %d subtask(s) ===", waveIdx, len(wave))

		var wg sync.WaitGroup
		errCh := make(chan error, len(wave))

		for i, subtaskID := range wave {
			if st, ok := state.Subtasks[subtaskID]; ok && st.Status == "closed" {
				log("  %s already closed, skipping", subtaskID)
				continue
			}

			wg.Add(1)
			go func(idx int, beadID string) {
				defer wg.Done()

				apprenticeName := fmt.Sprintf("apprentice-%s-%d", epicID, idx)
				log("  dispatching %s for %s", apprenticeName, beadID)

				spireBin, _ := os.Executable()
				cmd := exec.Command(spireBin, "wizard-run", beadID, "--name", apprenticeName)
				cmd.Env = os.Environ()
				cmd.Stdout = os.Stderr
				cmd.Stderr = os.Stderr

				if err := cmd.Run(); err != nil {
					log("  %s failed: %s", apprenticeName, err)
					errCh <- fmt.Errorf("%s: %w", beadID, err)
					return
				}

				log("  %s completed", apprenticeName)
				state.Subtasks[beadID] = subtaskState{
					Status: "closed",
					Branch: fmt.Sprintf("feat/%s", beadID),
					Agent:  apprenticeName,
				}
			}(i, subtaskID)
		}

		wg.Wait()
		close(errCh)

		var errs []string
		for e := range errCh {
			errs = append(errs, e.Error())
		}

		saveWorkshopState(state)

		if len(errs) > 0 {
			log("wave %d had %d error(s): %s", waveIdx, len(errs), strings.Join(errs, "; "))
		}

		log("verifying build after wave %d", waveIdx)
		buildCmd := exec.Command("go", "build", "./cmd/spire/")
		buildCmd.Dir = repoPath
		buildCmd.Env = os.Environ()
		if out, err := buildCmd.CombinedOutput(); err != nil {
			log("build failed after wave %d: %s\n%s", waveIdx, err, string(out))
		} else {
			log("build passed after wave %d", waveIdx)
		}
	}

	log("all waves complete — transitioning to review")
	setPhase(epicID, "review")
	state.Phase = "review"
	return nil
}
