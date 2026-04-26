package agent

import (
	"context"
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
//
// The Entry's PGID is captured after the child has started (Setpgid only
// takes effect at exec, so syscall.Getpgid must run after cmd.Start
// returns). On detached spawns the leader's PID equals its PGID; we ask
// the kernel anyway so attached spawns also record an accurate group.
// The PGID is what TerminateBead signals to reap the whole subtree —
// without it, detached children that reparent to PID 1 would survive
// reset (the spi-w65pr1 bug).
func (b *ProcessBackend) Spawn(cfg SpawnConfig) (Handle, error) {
	handle, err := b.spawner.Spawn(cfg)
	if err != nil {
		return nil, err
	}

	// Register in wizard registry with PID from handle.
	pid, _ := strconv.Atoi(handle.Identifier())
	pgid, _ := pgidOf(pid)
	entry := Entry{
		Name:       cfg.Name,
		PID:        pid,
		PGID:       pgid,
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

// terminateBeadGracePeriod is how long TerminateBead waits between
// SIGTERM and SIGKILL. Matches the legacy 5s grace the inline reset
// code used (cmd/spire/reset.go pre-spi-w65pr1). Exposed as a package
// variable so tests can shrink it without affecting production.
var terminateBeadGracePeriod = 5 * time.Second

// TerminateBead reaps every process the local backend spawned for the
// given bead. For each registry entry whose BeadID matches:
//
//  1. SIGTERM the entry's recorded PGID (or PID as a fallback when
//     the row predates spi-w65pr1 and has no PGID).
//  2. Wait up to terminateBeadGracePeriod for the group to exit.
//  3. SIGKILL whatever is still alive.
//  4. Drop the registry entry.
//
// Signalling -PGID instead of the leader PID is the key behaviour
// change vs. the prior cmd/spire reset path — child apprentices /
// claude / codex subprocesses survive parent exit by detaching with
// Setpgid, and reparent to PID 1 if the parent dies first. They keep
// their original PGID, so kill(-pgid, ...) still reaches them.
//
// After the per-entry pass, TerminateBead does a final liveness check
// on every PGID it signalled. If any group still has a member, it
// returns a non-nil error so reset can fail closed and surface a
// manual-cleanup message (the "warn/fail closed" requirement from the
// bug report).
//
// Idempotent: callable when no entries exist (returns nil), and
// tolerates ESRCH at every signal/probe step (group already gone =
// success).
func (b *ProcessBackend) TerminateBead(ctx context.Context, beadID string) error {
	if beadID == "" {
		return fmt.Errorf("[processBackend] TerminateBead: beadID required")
	}

	entries, err := EntriesForBead(beadID)
	if err != nil {
		return fmt.Errorf("[processBackend] TerminateBead: registry read: %w", err)
	}
	if len(entries) == 0 {
		return nil
	}

	type target struct {
		name string
		pid  int
		pgid int
	}
	var targets []target
	for _, e := range entries {
		targets = append(targets, target{name: e.Name, pid: e.PID, pgid: e.PGID})
	}

	for _, t := range targets {
		signalTerminate(t.pid, t.pgid)
	}

	deadline := time.Now().Add(terminateBeadGracePeriod)
	for time.Now().Before(deadline) {
		if !anyTargetAlive(targets, func(t target) (int, int) { return t.pid, t.pgid }) {
			break
		}
		select {
		case <-ctx.Done():
			break
		case <-time.After(200 * time.Millisecond):
		}
		if ctx.Err() != nil {
			break
		}
	}

	for _, t := range targets {
		if isAlive(t.pid, t.pgid) {
			signalKill(t.pid, t.pgid)
		}
	}

	if err := RegistryRemoveBead(beadID); err != nil {
		return fmt.Errorf("[processBackend] TerminateBead: registry remove for %s: %w", beadID, err)
	}

	var survivors []string
	for _, t := range targets {
		if isAlive(t.pid, t.pgid) {
			survivors = append(survivors, fmt.Sprintf("%s(pid=%d,pgid=%d)", t.name, t.pid, t.pgid))
		}
	}
	if len(survivors) > 0 {
		return fmt.Errorf("[processBackend] TerminateBead %s: manual cleanup required: %v", beadID, survivors)
	}
	return nil
}

// signalTerminate sends SIGTERM to the entire process group (when pgid
// is recorded) or to the leader PID alone (when it isn't — registry
// rows from before spi-w65pr1 omit PGID). Errors are folded to nil
// because the only sane reaction is "keep going to the next entry".
func signalTerminate(pid, pgid int) {
	if pgid > 0 {
		_ = killPGID(pgid, syscall.SIGTERM)
		return
	}
	if pid > 0 && dolt.ProcessAlive(pid) {
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Signal(syscall.SIGTERM)
		}
	}
}

// signalKill is the SIGKILL counterpart to signalTerminate.
func signalKill(pid, pgid int) {
	if pgid > 0 {
		_ = killPGID(pgid, syscall.SIGKILL)
		return
	}
	if pid > 0 && dolt.ProcessAlive(pid) {
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Signal(syscall.SIGKILL)
		}
	}
}

// isAlive reports whether the entry's process group (or PID when the
// row has no PGID) still has a live member.
func isAlive(pid, pgid int) bool {
	if pgid > 0 {
		return pgidAlive(pgid)
	}
	return pid > 0 && dolt.ProcessAlive(pid)
}

// anyTargetAlive returns true if any of the targets still has a live
// process group / PID. Generic over the target shape so the wait loop
// in TerminateBead can stay readable.
func anyTargetAlive[T any](ts []T, get func(T) (int, int)) bool {
	for _, t := range ts {
		pid, pgid := get(t)
		if isAlive(pid, pgid) {
			return true
		}
	}
	return false
}
