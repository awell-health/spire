//go:build unix

package agent

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/dolt"
)

// TestProcessBackend_TerminateBead_ReapsDetachedChild is the spi-w65pr1
// regression test required by the bug spec: spawn a parent that forks a
// detached child, register the parent's PGID against the bead, run
// TerminateBead, and assert no bead-scoped child process remains.
//
// The fork shape mirrors production: the wizard/apprentice is launched
// with Setpgid=true (see applyDetachAttrs), it forks a long-lived
// claude/codex/sleeper, then it exits. The forked child reparents to
// PID 1 but retains the parent's PGID. A pre-fix per-PID SIGTERM on the
// already-dead parent leaves the child alive — the exact bug reported
// while repairing spi-gg2y2j. PGID-scoped TerminateBead must reap it.
func TestProcessBackend_TerminateBead_ReapsDetachedChild(t *testing.T) {
	t.Setenv("SPIRE_CONFIG_DIR", t.TempDir())

	beadID := "spi-tbtest"

	// Launch a shell that backgrounds a long sleep, prints the sleep PID,
	// then exits. Setpgid puts the shell in its own process group; the
	// backgrounded sleep inherits the shell's PGID (jobctl is off in
	// non-interactive sh). After the shell exits, the sleep is the only
	// surviving member of the group — exactly the spi-w65pr1 shape.
	cmd := exec.Command("sh", "-c", "sleep 30 & echo $! ; exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("spawn shell: %v", err)
	}
	parentPID := cmd.Process.Pid
	pgid, err := syscall.Getpgid(parentPID)
	if err != nil {
		t.Fatalf("Getpgid(parent=%d): %v", parentPID, err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || childPID <= 0 {
		t.Fatalf("parse child PID from %q: %v", out, err)
	}
	// cmd.Output already waited on the shell, so parentPID is reaped.

	// Belt-and-suspenders: if the assertion fails, kill the sleep
	// directly so we don't leak a 30s process on the dev machine.
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })

	// Precondition: the child must outlive the parent. If the platform's
	// shell decided to put the background sleep in its own group, the
	// test premise breaks — fail loudly so we know to revisit the fixture.
	if !dolt.ProcessAlive(childPID) {
		t.Fatalf("precondition: child PID %d not alive after parent exit", childPID)
	}
	childPgid, err := syscall.Getpgid(childPID)
	if err != nil {
		t.Fatalf("Getpgid(child=%d): %v", childPID, err)
	}
	if childPgid != pgid {
		t.Fatalf("precondition: child PGID = %d, want %d (same group as parent — required for spi-w65pr1 reap)", childPgid, pgid)
	}

	// Mirror what ProcessBackend.Spawn would write for a real wizard.
	if err := RegistryAdd(Entry{
		Name:   "wizard-" + beadID,
		PID:    parentPID,
		PGID:   pgid,
		BeadID: beadID,
	}); err != nil {
		t.Fatalf("RegistryAdd: %v", err)
	}

	// Shrink the SIGTERM→SIGKILL grace so the test runs in <1s.
	origGrace := terminateBeadGracePeriod
	terminateBeadGracePeriod = 500 * time.Millisecond
	t.Cleanup(func() { terminateBeadGracePeriod = origGrace })

	if err := NewProcessBackend().TerminateBead(context.Background(), beadID); err != nil {
		t.Fatalf("TerminateBead: %v", err)
	}

	// Poll for child death — kill is async; the kernel takes a few ms to
	// mark the process gone after SIGTERM/SIGKILL is delivered.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !dolt.ProcessAlive(childPID) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if dolt.ProcessAlive(childPID) {
		t.Errorf("child PID %d still alive after TerminateBead — detached child was not reaped (spi-w65pr1)", childPID)
	}

	// Successful reap clears the bead's registry rows.
	leftovers, err := EntriesForBead(beadID)
	if err != nil {
		t.Fatalf("EntriesForBead: %v", err)
	}
	if len(leftovers) != 0 {
		t.Errorf("registry entries for %s = %d, want 0 (TerminateBead must clear them on success)", beadID, len(leftovers))
	}
}

// TestProcessBackend_TerminateBead_NoEntriesIsNoop verifies the
// idempotent behavior: TerminateBead on a bead with no registered
// processes returns nil instead of an error. Reset must be safely
// callable on already-quiet beads (the gateway POST /reset endpoint and
// the manual `spire reset` CLI both rely on this).
func TestProcessBackend_TerminateBead_NoEntriesIsNoop(t *testing.T) {
	t.Setenv("SPIRE_CONFIG_DIR", t.TempDir())
	if err := NewProcessBackend().TerminateBead(context.Background(), "spi-noent"); err != nil {
		t.Errorf("TerminateBead with no entries returned %v, want nil", err)
	}
}

// TestProcessBackend_TerminateBead_RequiresBeadID rejects an empty bead
// ID up-front. Without this guard, a buggy caller passing "" would scan
// the registry for entries with an empty BeadID and could nuke whatever
// matched — better to fail closed than to risk a wildcard reap.
func TestProcessBackend_TerminateBead_RequiresBeadID(t *testing.T) {
	t.Setenv("SPIRE_CONFIG_DIR", t.TempDir())
	if err := NewProcessBackend().TerminateBead(context.Background(), ""); err == nil {
		t.Errorf("TerminateBead(\"\") returned nil, want error to prevent wildcard reap")
	}
}

// TestProcessBackend_TerminateBead_FallbackToPIDWhenNoPGID covers the
// pre-spi-w65pr1 registry rows: an entry written before the PGID column
// existed has PGID=0, so signalTerminate must fall back to signalling
// the leader PID directly. This keeps reset working on registries that
// pre-date the upgrade.
func TestProcessBackend_TerminateBead_FallbackToPIDWhenNoPGID(t *testing.T) {
	t.Setenv("SPIRE_CONFIG_DIR", t.TempDir())

	beadID := "spi-fbtest"

	// Spawn a single sleep with no Setpgid so PGID == its own PID; we
	// then deliberately register PGID=0 to exercise the fallback path.
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn sleep: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})

	if err := RegistryAdd(Entry{
		Name:   "wizard-" + beadID,
		PID:    pid,
		PGID:   0, // simulate pre-upgrade row
		BeadID: beadID,
	}); err != nil {
		t.Fatalf("RegistryAdd: %v", err)
	}

	origGrace := terminateBeadGracePeriod
	terminateBeadGracePeriod = 500 * time.Millisecond
	t.Cleanup(func() { terminateBeadGracePeriod = origGrace })

	if err := NewProcessBackend().TerminateBead(context.Background(), beadID); err != nil {
		t.Fatalf("TerminateBead: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !dolt.ProcessAlive(pid) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if dolt.ProcessAlive(pid) {
		t.Errorf("PID %d still alive after TerminateBead — per-PID fallback did not fire", pid)
	}
}
