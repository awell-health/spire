//go:build unix

package main

import (
	"bytes"
	"log"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// spawnDetachedChild starts a long-lived child process in its own
// process group (Setpgid=true), mirroring how pkg/agent spawns wizards
// in production. Returns the child PID and a cleanup that ensures the
// process is reaped regardless of test outcome.
func spawnDetachedChild(t *testing.T) (int, func()) {
	t.Helper()
	// `sleep 30` is a sufficient ceiling for a unit test; the cleanup
	// below kills it once we're done so the time bound is conservative
	// not load-bearing.
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn child: %v", err)
	}
	pid := cmd.Process.Pid

	// Setpgid only takes effect at exec; poll briefly for the kernel
	// to install the new PGID rather than racing against the child
	// barely existing.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if pgid, err := syscall.Getpgid(pid); err == nil && pgid == pid {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	cleanup := func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
	return pid, cleanup
}

// TestDismissLocal_PGIDSandbox_SkipsCrossGroupByDefault is the central
// regression test for spi-e16f5t: dismissLocal must refuse to signal a
// wizard whose PGID does not match the caller's. We spawn a child in
// its own process group, write a registry entry pointing at it, and
// assert dismissLocal(... allowCrossGroup=false) leaves it alive — and
// that the skip is visibly logged with the expected diagnostic line.
func TestDismissLocal_PGIDSandbox_SkipsCrossGroupByDefault(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	childPID, cleanup := spawnDetachedChild(t)
	defer cleanup()

	childPGID, err := syscall.Getpgid(childPID)
	if err != nil {
		t.Fatalf("child pgid: %v", err)
	}
	selfPGID, _ := syscall.Getpgid(os.Getpid())
	if childPGID == selfPGID {
		t.Fatalf("setup invariant broken: child PGID %d == test PGID %d (Setpgid not honored?)", childPGID, selfPGID)
	}

	saveWizardRegistry(wizardRegistry{
		Wizards: []localWizard{
			{Name: "test-wizard", PID: childPID, PGID: childPGID, BeadID: ""},
		},
	})

	// Capture log output so we can assert the skip diagnostic fired —
	// the bead author specifically wants this line greppable.
	var logBuf bytes.Buffer
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&logBuf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	}()

	if err := dismissLocal(1, false, nil, false); err != nil {
		t.Fatalf("dismissLocal: %v", err)
	}

	// Give the SIGINT (which we expect to NOT have been sent) a moment
	// to land if the sandbox failed.
	time.Sleep(100 * time.Millisecond)

	if !processAlive(childPID) {
		t.Fatal("child process was signaled despite cross-group PGID — sandbox is not blocking the signal")
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "dismissLocal: skipping wizard") {
		t.Errorf("expected skip diagnostic in log output, got: %q", logged)
	}
	if !strings.Contains(logged, "not in caller's process group") {
		t.Errorf("expected skip reason in log output, got: %q", logged)
	}
	if !strings.Contains(logged, "--allow-cross-group") {
		t.Errorf("expected log to mention the --allow-cross-group override, got: %q", logged)
	}

	// Skipped wizard must remain in the registry — operators rerunning
	// dismiss with --allow-cross-group still need to find it.
	reg := loadWizardRegistry()
	if len(reg.Wizards) != 1 || reg.Wizards[0].PID != childPID {
		t.Errorf("expected skipped wizard to remain in registry, got %+v", reg.Wizards)
	}
}

// TestDismissLocal_PGIDSandbox_OverrideSignalsCrossGroup verifies that
// passing allowCrossGroup=true bypasses the PGID check and signals the
// child even though it sits in a different process group. This is the
// operator escape hatch — without it, every CLI dismiss in production
// (where wizards are spawned with Setpgid=true) would silently no-op.
func TestDismissLocal_PGIDSandbox_OverrideSignalsCrossGroup(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	childPID, cleanup := spawnDetachedChild(t)
	defer cleanup()

	childPGID, err := syscall.Getpgid(childPID)
	if err != nil {
		t.Fatalf("child pgid: %v", err)
	}

	saveWizardRegistry(wizardRegistry{
		Wizards: []localWizard{
			{Name: "test-wizard", PID: childPID, PGID: childPGID, BeadID: ""},
		},
	})

	if err := dismissLocal(1, false, nil, true); err != nil {
		t.Fatalf("dismissLocal: %v", err)
	}

	// SIGINT should reap `sleep`. Poll briefly to absorb scheduling
	// jitter on busy CI runners — the assertion is "did it die," not
	// "did it die in 10ms."
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(childPID) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if processAlive(childPID) {
		t.Fatal("child process is still alive after dismissLocal(allowCrossGroup=true) — override is not reaching the signal")
	}

	// Dismissed wizard must be removed from the registry.
	reg := loadWizardRegistry()
	for _, w := range reg.Wizards {
		if w.PID == childPID {
			t.Errorf("dismissed wizard PID %d should have been removed from registry", childPID)
		}
	}
}

// TestDismissLocal_PGIDSandbox_TargetsPathSkips verifies the targets
// path (--targets bead-id) honors the same PGID sandbox. Without this,
// `spire dismiss --targets <bead>` from a test would silently signal
// the operator's wizard even when the bare-count path correctly skips.
func TestDismissLocal_PGIDSandbox_TargetsPathSkips(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	childPID, cleanup := spawnDetachedChild(t)
	defer cleanup()

	childPGID, err := syscall.Getpgid(childPID)
	if err != nil {
		t.Fatalf("child pgid: %v", err)
	}

	saveWizardRegistry(wizardRegistry{
		Wizards: []localWizard{
			{Name: "test-wizard", PID: childPID, PGID: childPGID, BeadID: "spi-target-test"},
		},
	})

	var logBuf bytes.Buffer
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&logBuf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	}()

	if err := dismissLocal(0, false, []string{"spi-target-test"}, false); err != nil {
		t.Fatalf("dismissLocal: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	if !processAlive(childPID) {
		t.Fatal("targets-path: child was signaled despite cross-group PGID")
	}
	if !strings.Contains(logBuf.String(), "dismissLocal: skipping wizard") {
		t.Errorf("targets-path: expected skip diagnostic, got: %q", logBuf.String())
	}

	reg := loadWizardRegistry()
	if len(reg.Wizards) != 1 || reg.Wizards[0].PID != childPID {
		t.Errorf("targets-path: skipped wizard should remain in registry, got %+v", reg.Wizards)
	}
}

// TestDismissLocal_PGIDSandbox_SamePGIDSignals verifies the friendly
// case: a wizard whose PGID matches the caller's IS signaled. This
// protects against a regression where the sandbox over-skips and never
// signals anyone. Uses the test process's own PID — its PGID matches
// selfPGID by definition.
func TestDismissLocal_PGIDSandbox_SamePGIDSignals(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	// Spawn a child that does NOT call Setpgid, so it inherits the
	// test process's PGID.
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn child: %v", err)
	}
	childPID := cmd.Process.Pid
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	childPGID, err := syscall.Getpgid(childPID)
	if err != nil {
		t.Fatalf("child pgid: %v", err)
	}
	selfPGID, _ := syscall.Getpgid(os.Getpid())
	if childPGID != selfPGID {
		t.Skipf("test environment did not give child the test's PGID (child=%d test=%d); same-PGID path can't be exercised here",
			childPGID, selfPGID)
	}

	saveWizardRegistry(wizardRegistry{
		Wizards: []localWizard{
			{Name: "same-pgid-wizard", PID: childPID, PGID: childPGID, BeadID: ""},
		},
	})

	if err := dismissLocal(1, false, nil, false); err != nil {
		t.Fatalf("dismissLocal: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(childPID) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("same-PGID child was not signaled — sandbox is over-skipping in the friendly case")
}
