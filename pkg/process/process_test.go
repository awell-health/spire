package process

import (
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// TestProcessAlive_Zombie verifies that ProcessAlive returns false for a
// zombie (defunct) child — kill -0 alone reports zombies as alive, which
// previously masked dead wizards from the board, `spire up` cleanup, and
// OrphanSweep. Regression for spi-k2bz93.
func TestProcessAlive_Zombie(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("zombie detection only implemented on linux/darwin (have %s)", runtime.GOOS)
	}

	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	pid := cmd.Process.Pid
	// Reap at end so the test doesn't leak a zombie itself.
	defer func() { _ = cmd.Wait() }()

	// Wait until the child has exited but stays unreaped (zombie window).
	// Poll Signal(0): once the kernel marks the entry zombie it still
	// returns success, but isZombie should now be true. Cap at ~3s.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		// Signal(0) must still succeed (process entry still exists).
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatalf("child reaped before zombie was observed: %v", err)
		}
		if isZombie(pid) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !isZombie(pid) {
		t.Fatalf("expected pid %d to be in zombie state within deadline", pid)
	}
	if ProcessAlive(pid) {
		t.Fatalf("ProcessAlive(%d) returned true for a zombie process; want false", pid)
	}
}

// TestProcessAlive_LiveProcess sanity-checks that a live, non-zombie
// process still reports alive. Without this the zombie tightening could
// silently flip every PID dead and we'd never notice.
func TestProcessAlive_LiveProcess(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "sleep 5")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	if !ProcessAlive(cmd.Process.Pid) {
		t.Fatalf("ProcessAlive(%d) returned false for a live process", cmd.Process.Pid)
	}
}

func TestProcessAlive_DeadPID(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Wait failed: %v", err)
	}
	if ProcessAlive(pid) {
		t.Fatalf("ProcessAlive(%d) returned true for a reaped process", pid)
	}
}

func TestProcessAlive_NonPositive(t *testing.T) {
	if ProcessAlive(0) {
		t.Fatal("ProcessAlive(0) should be false")
	}
	if ProcessAlive(-1) {
		t.Fatal("ProcessAlive(-1) should be false")
	}
}
