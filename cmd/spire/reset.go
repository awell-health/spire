package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func cmdReset(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: spire reset <bead-id> [--hard]")
	}

	var beadID string
	var hard bool
	for _, arg := range args {
		switch arg {
		case "--hard":
			hard = true
		default:
			if strings.HasPrefix(arg, "-") {
				return fmt.Errorf("unknown flag %q\nusage: spire reset <bead-id> [--hard]", arg)
			}
			beadID = arg
		}
	}

	if beadID == "" {
		return fmt.Errorf("usage: spire reset <bead-id> [--hard]")
	}

	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	reg := loadWizardRegistry()

	// Find wizard entry for this bead (includes dead entries).
	var wizard *localWizard
	for i := range reg.Wizards {
		if reg.Wizards[i].BeadID == beadID {
			wizard = &reg.Wizards[i]
			break
		}
	}

	var wizardName string
	var worktreePath string

	if wizard != nil {
		wizardName = wizard.Name
		worktreePath = wizard.Worktree

		// Kill process if alive.
		if wizard.PID > 0 && processAlive(wizard.PID) {
			if proc, err := os.FindProcess(wizard.PID); err == nil {
				proc.Signal(syscall.SIGTERM)
				// Wait up to 5s for the process to exit.
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					time.Sleep(200 * time.Millisecond)
					if !processAlive(wizard.PID) {
						break
					}
				}
				// Force kill if still alive.
				if processAlive(wizard.PID) {
					proc.Signal(syscall.SIGKILL)
				}
			}
			fmt.Printf("  %s↓ %s killed (pid %d)%s\n", dim, wizardName, wizard.PID, reset)
		}

		// Remove this bead's entry from the registry.
		var remaining []localWizard
		for _, w := range reg.Wizards {
			if w.BeadID != beadID {
				remaining = append(remaining, w)
			}
		}
		reg.Wizards = remaining
		saveWizardRegistry(reg)

		// Delete executor state file.
		statePath := executorStatePath(wizardName)
		if err := os.Remove(statePath); err == nil {
			fmt.Printf("  %s✗ state file removed%s\n", dim, reset)
		}
	} else {
		// No registry entry — derive the wizard name from the bead ID.
		wizardName = "wizard-" + beadID
	}

	// Clean all executor-related labels from the bead.
	bead, err := storeGetBead(beadID)
	if err != nil {
		return fmt.Errorf("get bead %s: %w", beadID, err)
	}

	for _, label := range bead.Labels {
		if isResetLabel(label) {
			storeRemoveLabel(beadID, label) // best-effort
		}
	}

	// Set bead status to open.
	if err := storeUpdateBead(beadID, map[string]interface{}{"status": "open"}); err != nil {
		fmt.Printf("  %s(note: could not reopen %s: %s)%s\n", dim, beadID, err, reset)
	} else {
		fmt.Printf("  %s↺ %s reopened%s\n", yellow, beadID, reset)
	}

	// --hard: also remove worktree and feature branch.
	if hard {
		if worktreePath == "" {
			worktreePath = filepath.Join(os.TempDir(), "spire-wizard", wizardName, beadID)
		}
		if err := os.RemoveAll(worktreePath); err == nil {
			fmt.Printf("  %s✗ worktree removed: %s%s\n", dim, worktreePath, reset)
		} else if !os.IsNotExist(err) {
			fmt.Printf("  %s(note: could not remove worktree %s: %s)%s\n", dim, worktreePath, err, reset)
		}

		// Delete feature branch(es) recorded in labels.
		for _, label := range bead.Labels {
			if strings.HasPrefix(label, "feat-branch:") {
				branch := strings.TrimPrefix(label, "feat-branch:")
				if branch != "" {
					resetDeleteBranch(branch)
				}
			}
		}
	}

	fmt.Printf("%s reset — ready for re-summon\n", beadID)
	return nil
}

// isResetLabel returns true if a label should be removed during reset.
func isResetLabel(label string) bool {
	if strings.HasPrefix(label, "phase:") {
		return true
	}
	if strings.HasPrefix(label, "owner:") {
		return true
	}
	if strings.HasPrefix(label, "review-round:") {
		return true
	}
	if strings.HasPrefix(label, "feat-branch:") {
		return true
	}
	switch label {
	case "test-failure", "needs-human", "review-approved":
		return true
	}
	return false
}

// resetDeleteBranch deletes a local git branch (best-effort, prints result).
func resetDeleteBranch(branch string) {
	cwd, _ := os.Getwd()
	rc := &RepoContext{Dir: cwd}
	if err := rc.ForceDeleteBranch(branch); err != nil {
		fmt.Printf("  %s(note: could not delete branch %s: %s)%s\n", dim, branch, err, reset)
	} else {
		fmt.Printf("  %s✗ branch deleted: %s%s\n", dim, branch, reset)
	}
}
