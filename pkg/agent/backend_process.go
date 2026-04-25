package agent

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/awell-health/spire/pkg/dolt"
	"github.com/awell-health/spire/pkg/runtime"
)

// ProcessBackend implements Backend for local process execution.
// It wraps processSpawner for Spawn and absorbs process-specific tracking
// (wizard registry, PID files, log files) into the unified backend interface.
type ProcessBackend struct {
	spawner *ProcessSpawner
}

// NewProcessBackend creates a new process backend.
func NewProcessBackend() *ProcessBackend {
	return &ProcessBackend{spawner: &ProcessSpawner{}}
}

func newProcessBackend() *ProcessBackend {
	return NewProcessBackend()
}

// Spawn delegates to processSpawner.Spawn and registers the agent in the
// wizard registry with the PID from the returned handle.
//
// ProcessBackend.Spawn is the SOLE seam that creates wizard registry entries
// for spawned agents — see pkg/agent/README.md "Registry lifecycle". Wizard /
// handoff code must not pre-register or self-register; the child runtime
// stamps Phase via registry.Update from pkg/executor/graph_interpreter.go and
// pkg/wizard/wizard*.go after this Add lands.
func (b *ProcessBackend) Spawn(cfg SpawnConfig) (Handle, error) {
	handle, err := b.spawner.Spawn(cfg)
	if err != nil {
		return nil, err
	}

	// Register in wizard registry with PID from handle.
	pid, _ := strconv.Atoi(handle.Identifier())
	entry := Entry{
		Name:       cfg.Name,
		PID:        pid,
		BeadID:     cfg.BeadID,
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
		Tower:      cfg.Tower,
		InstanceID: cfg.InstanceID,
	}
	if err := RegistryAdd(entry); err != nil {
		return handle, fmt.Errorf("[processBackend] registry add for %s: %w%s", cfg.Name, err, runtime.LogFields(cfg.Run))
	}

	return handle, nil
}

// List reads the wizard registry and returns Info for each entry,
// checking liveness via ProcessAlive.
func (b *ProcessBackend) List() ([]Info, error) {
	reg := LoadRegistry()
	infos := make([]Info, 0, len(reg.Wizards))

	for _, w := range reg.Wizards {
		alive := w.PID > 0 && dolt.ProcessAlive(w.PID)

		var startedAt time.Time
		if w.StartedAt != "" {
			if t, err := time.Parse(time.RFC3339, w.StartedAt); err == nil {
				startedAt = t
			}
		}

		infos = append(infos, Info{
			Name:       w.Name,
			BeadID:     w.BeadID,
			Phase:      w.Phase,
			Alive:      alive,
			Identifier: strconv.Itoa(w.PID),
			StartedAt:  startedAt,
			Tower:      w.Tower,
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
func (b *ProcessBackend) Logs(name string) (io.ReadCloser, error) {
	dir := filepath.Join(dolt.GlobalDir(), "wizards")
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
func (b *ProcessBackend) Kill(name string) error {
	reg := LoadRegistry()

	// Find the wizard entry.
	var found *Entry
	for i := range reg.Wizards {
		if reg.Wizards[i].Name == name {
			found = &reg.Wizards[i]
			break
		}
	}
	if found == nil {
		return fmt.Errorf("agent %q not found in registry", name)
	}

	if found.InstanceID != "" && found.InstanceID != CallerInstanceID {
		// No SpawnConfig in scope here — the kill path is invoked by the
		// steward/CLI long after the spawn boundary. Fall back to the
		// callee's own env so the log line still carries the canonical
		// identity set for whichever tower/bead the caller is bound to.
		log.Printf("warning: killing agent %s owned by instance %s%s", name, found.InstanceID, runtime.LogFields(runtime.RunContextFromEnv()))
	}

	pid := found.PID
	if pid > 0 && dolt.ProcessAlive(pid) {
		proc, _ := os.FindProcess(pid)
		if err := proc.Signal(syscall.SIGTERM); err != nil {
			return fmt.Errorf("kill agent %s (pid %d): %w", name, pid, err)
		}
	}

	// Clear PID file via the injected callback (if set).
	if ClearPIDFunc != nil {
		ClearPIDFunc(name)
	}

	// Remove from registry.
	if err := RegistryRemove(name); err != nil {
		return fmt.Errorf("registry remove %s: %w", name, err)
	}

	return nil
}

// CallerInstanceID is set by the caller (e.g., steward or cmd/spire) to
// identify this Spire instance. Used to distinguish same-instance kills
// from cross-instance kills in log output.
var CallerInstanceID string

// ClearPIDFunc is set by cmd/spire to clear wizard PID files.
// pkg/agent does not import steward_local — this callback bridges the gap.
var ClearPIDFunc func(name string)
