package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

// newTestHandle creates a processHandle wrapping a started command.
func newTestHandle(t *testing.T, name string, command string, args ...string) *processHandle {
	t.Helper()
	cmd := exec.Command(command, args...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return &processHandle{name: name, cmd: cmd}
}

// --- processHandle tests ---

func TestProcessHandle_Name(t *testing.T) {
	h := newTestHandle(t, "test-agent", "sleep", "10")
	if h.Name() != "test-agent" {
		t.Errorf("Name() = %q, want %q", h.Name(), "test-agent")
	}
}

func TestProcessHandle_Identifier(t *testing.T) {
	h := newTestHandle(t, "test-agent", "sleep", "10")
	id := h.Identifier()
	pid, err := strconv.Atoi(id)
	if err != nil {
		t.Fatalf("Identifier() = %q, not a valid PID", id)
	}
	if pid <= 0 {
		t.Errorf("Identifier() PID = %d, want > 0", pid)
	}
}

func TestProcessHandle_Alive_Running(t *testing.T) {
	h := newTestHandle(t, "test-agent", "sleep", "10")
	if !h.Alive() {
		t.Error("Alive() = false for running process")
	}
}

func TestProcessHandle_Alive_AfterWait(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &processHandle{name: "done-agent", cmd: cmd}
	if err := h.Wait(); err != nil {
		t.Fatal(err)
	}
	if h.Alive() {
		t.Error("Alive() = true after Wait()")
	}
}

func TestProcessHandle_Wait_Success(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &processHandle{name: "ok-agent", cmd: cmd}
	if err := h.Wait(); err != nil {
		t.Errorf("Wait() = %v, want nil", err)
	}
}

func TestProcessHandle_Wait_Failure(t *testing.T) {
	cmd := exec.Command("false")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &processHandle{name: "fail-agent", cmd: cmd}
	if err := h.Wait(); err == nil {
		t.Error("Wait() = nil, want error for non-zero exit")
	}
}

func TestProcessHandle_Signal(t *testing.T) {
	h := newTestHandle(t, "signal-agent", "sleep", "60")

	// Process should be alive.
	if !h.Alive() {
		t.Fatal("expected process alive before signal")
	}

	// Send SIGTERM.
	if err := h.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("Signal(SIGTERM) = %v", err)
	}

	// Wait should return with a signal error.
	err := h.Wait()
	if err == nil {
		t.Error("Wait() after SIGTERM should return error")
	}

	if h.Alive() {
		t.Error("Alive() = true after SIGTERM + Wait()")
	}
}

func TestProcessHandle_Signal_AfterExit(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h := &processHandle{name: "exited-agent", cmd: cmd}
	h.Wait()

	err := h.Signal(os.Interrupt)
	if err == nil {
		t.Error("Signal after exit should return error")
	}
}

// --- NewSpawner factory tests ---

func TestNewSpawner_Process(t *testing.T) {
	s := NewSpawner("process")
	if _, ok := s.(*processSpawner); !ok {
		t.Errorf("NewSpawner(\"process\") returned %T, want *processSpawner", s)
	}
}

func TestNewSpawner_Empty(t *testing.T) {
	s := NewSpawner("")
	if _, ok := s.(*processSpawner); !ok {
		t.Errorf("NewSpawner(\"\") returned %T, want *processSpawner", s)
	}
}

func TestNewSpawner_Unknown(t *testing.T) {
	s := NewSpawner("docker")
	if _, ok := s.(*processSpawner); !ok {
		t.Errorf("NewSpawner(\"docker\") returned %T, want *processSpawner (fallback)", s)
	}
}

// --- SpawnConfig role mapping test ---

func TestProcessSpawner_InvalidRole(t *testing.T) {
	s := &processSpawner{}
	_, err := s.Spawn(SpawnConfig{
		Name:   "test",
		BeadID: "test-1",
		Role:   SpawnRole("invalid"),
	})
	if err == nil {
		t.Error("Spawn with invalid role should return error")
	}
}

// --- processSpawner.Spawn with log file ---

func TestProcessSpawner_SpawnWithLogPath(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "test.log")
	s := &processSpawner{}

	// Spawn "true" would require the binary to be spire. Instead, verify
	// that an invalid role produces an error and a valid config at least
	// attempts to start. The role→subcommand mapping is covered by
	// TestProcessSpawner_InvalidRole. Here we verify log file creation.
	_, err := s.Spawn(SpawnConfig{
		Name:    "log-test",
		BeadID:  "test-1",
		Role:    SpawnRole("bogus"),
		LogPath: logPath,
	})
	if err == nil {
		t.Fatal("expected error for bogus role")
	}

	// Log file should NOT be created for a role validation failure
	// (we fail before opening it).
	if _, statErr := os.Stat(logPath); statErr == nil {
		t.Error("log file should not be created on role validation failure")
	}
}
