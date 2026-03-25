package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// processBackend implements AgentBackend for local process execution.
// It wraps processSpawner for Spawn and absorbs process-specific tracking
// (wizard registry, PID files, log files) into the unified backend interface.
type processBackend struct {
	spawner *processSpawner
}

func newProcessBackend() *processBackend {
	return &processBackend{spawner: &processSpawner{}}
}

// Spawn delegates to processSpawner.Spawn and registers the agent in the
// wizard registry with the PID from the returned handle.
func (b *processBackend) Spawn(cfg SpawnConfig) (AgentHandle, error) {
	handle, err := b.spawner.Spawn(cfg)
	if err != nil {
		return nil, err
	}

	// Register in wizard registry with PID from handle.
	pid, _ := strconv.Atoi(handle.Identifier())
	entry := localWizard{
		Name:      cfg.Name,
		PID:       pid,
		BeadID:    cfg.BeadID,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := wizardRegistryAdd(entry); err != nil {
		// Non-fatal: log and continue. The agent is running regardless.
		fmt.Fprintf(os.Stderr, "[processBackend] warning: registry add for %s: %v\n", cfg.Name, err)
	}

	return handle, nil
}

// List reads the wizard registry and returns AgentInfo for each entry,
// checking liveness via processAlive.
func (b *processBackend) List() ([]AgentInfo, error) {
	reg := loadWizardRegistry()
	infos := make([]AgentInfo, 0, len(reg.Wizards))

	for _, w := range reg.Wizards {
		alive := w.PID > 0 && processAlive(w.PID)

		var startedAt time.Time
		if w.StartedAt != "" {
			if t, err := time.Parse(time.RFC3339, w.StartedAt); err == nil {
				startedAt = t
			}
		}

		infos = append(infos, AgentInfo{
			Name:       w.Name,
			BeadID:     w.BeadID,
			Phase:      w.Phase,
			Alive:      alive,
			Identifier: strconv.Itoa(w.PID),
			StartedAt:  startedAt,
		})
	}

	return infos, nil
}

// Logs returns an io.ReadCloser for the named agent's log file.
// It tries multiple naming conventions used across the codebase:
//
//	<name>.log, <name>-fix.log, wizard-<name>.log
//
// Returns os.ErrNotExist if no log file is found.
func (b *processBackend) Logs(name string) (io.ReadCloser, error) {
	dir := filepath.Join(doltGlobalDir(), "wizards")
	candidates := []string{
		filepath.Join(dir, name+".log"),
		filepath.Join(dir, name+"-fix.log"),
		filepath.Join(dir, "wizard-"+name+".log"),
	}

	for _, path := range candidates {
		f, err := os.Open(path)
		if err == nil {
			return f, nil
		}
	}

	return nil, os.ErrNotExist
}

// Kill looks up the named agent in the wizard registry, sends SIGTERM
// if alive, clears its PID file, and removes it from the registry.
func (b *processBackend) Kill(name string) error {
	reg := loadWizardRegistry()

	// Find the wizard entry.
	var found *localWizard
	for i := range reg.Wizards {
		if reg.Wizards[i].Name == name {
			found = &reg.Wizards[i]
			break
		}
	}
	if found == nil {
		return fmt.Errorf("agent %q not found in registry", name)
	}

	pid := found.PID
	if pid > 0 && processAlive(pid) {
		proc, _ := os.FindProcess(pid)
		if err := proc.Signal(syscall.SIGTERM); err != nil {
			return fmt.Errorf("kill agent %s (pid %d): %w", name, pid, err)
		}
	}

	// Clear PID file.
	clearWizardPID(name)

	// Remove from registry.
	if err := wizardRegistryRemove(name); err != nil {
		return fmt.Errorf("registry remove %s: %w", name, err)
	}

	return nil
}
