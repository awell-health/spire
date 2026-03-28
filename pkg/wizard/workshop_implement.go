package wizard

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// WorkshopImplement handles the implement phase of the wizard workshop.
// It computes waves from the dependency graph, dispatches apprentices
// in parallel worktrees, and merges their work back.
func WorkshopImplement(state *WorkshopState, spawner Backend, deps *Deps) error {
	epicID := state.EpicID
	log := func(format string, a ...interface{}) {
		fmt.Fprintf(os.Stderr, "[workshop] "+format+"\n", a...)
	}

	waves, err := deps.ComputeWaves(epicID)
	if err != nil {
		return err
	}
	if len(waves) == 0 {
		log("no open subtasks — transitioning to review")
		state.Phase = "review"
		return nil
	}

	log("computed %d wave(s)", len(waves))
	for i, wave := range waves {
		log("wave %d: %v", i, wave)
	}

	startWave := state.Wave

	repoPath, _, _, err := deps.ResolveRepo(epicID)
	if err != nil {
		return fmt.Errorf("resolve repo: %w", err)
	}

	for waveIdx := startWave; waveIdx < len(waves); waveIdx++ {
		wave := waves[waveIdx]
		state.Wave = waveIdx
		log("=== wave %d: %d subtask(s) ===", waveIdx, len(wave))

		type apprenticeResult struct {
			BeadID string
			Agent  string
			Err    error
		}

		var wg sync.WaitGroup
		resultCh := make(chan apprenticeResult, len(wave))

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

				handle, err := spawner.Spawn(SpawnConfig{
					Name:   apprenticeName,
					BeadID: beadID,
					Role:   RoleApprentice,
				})
				if err != nil {
					log("  %s spawn failed: %s", apprenticeName, err)
					resultCh <- apprenticeResult{BeadID: beadID, Agent: apprenticeName, Err: err}
					return
				}
				if err := handle.Wait(); err != nil {
					log("  %s failed: %s", apprenticeName, err)
					resultCh <- apprenticeResult{BeadID: beadID, Agent: apprenticeName, Err: err}
					return
				}

				log("  %s completed", apprenticeName)
				resultCh <- apprenticeResult{BeadID: beadID, Agent: apprenticeName}
			}(i, subtaskID)
		}

		wg.Wait()
		close(resultCh)

		// Collect results — single-threaded write to state.Subtasks (no race)
		var errs []string
		for r := range resultCh {
			if r.Err != nil {
				errs = append(errs, fmt.Sprintf("%s: %s", r.BeadID, r.Err))
				continue
			}
			state.Subtasks[r.BeadID] = SubtaskState{
				Status: "closed",
				Branch: deps.ResolveBranch(r.BeadID, repoPath),
				Agent:  r.Agent,
			}
		}

		SaveWorkshopState(state, deps)

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
	state.Phase = "review"
	return nil
}
