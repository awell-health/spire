package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)


// doltDataDir returns the dolt database directory.
func doltDataDir() string {
	if d := os.Getenv("DOLT_DATA_DIR"); d != "" {
		return d
	}
	if runtime.GOOS == "darwin" {
		return "/opt/homebrew/var/dolt"
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "dolt")
}

// doltGlobalDir returns a user-level directory for dolt server state (PID, config, logs).
// This is kept separate from any repo's .spire/ since dolt is a shared singleton.
func doltGlobalDir() string {
	if d := os.Getenv("SPIRE_DOLT_DIR"); d != "" {
		os.MkdirAll(d, 0755)
		return d
	}
	home, _ := os.UserHomeDir()
	d := filepath.Join(home, ".local", "share", "spire")
	os.MkdirAll(d, 0755)
	return d
}

// doltPort returns the configured dolt server port.
func doltPort() string {
	for _, key := range []string{"BEADS_DOLT_SERVER_PORT", "DOLT_PORT"} {
		if p := os.Getenv(key); p != "" {
			return p
		}
	}
	return "3307"
}

// doltHost returns the configured dolt server host.
func doltHost() string {
	for _, key := range []string{"BEADS_DOLT_SERVER_HOST", "DOLT_HOST"} {
		if h := os.Getenv(key); h != "" {
			return h
		}
	}
	return "127.0.0.1"
}

// --- PID file helpers ---

// readPID reads a PID from a file. Returns 0 if file does not exist or is invalid.
func readPID(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return pid
}

// writePID writes a PID to a file.
func writePID(path string, pid int) error {
	return os.WriteFile(path, []byte(strconv.Itoa(pid)), 0644)
}

// processAlive checks if a process with the given PID is running.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, FindProcess always succeeds. Use kill -0 to check.
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}

// doltBin returns the resolved dolt binary path, falling back to "dolt" if
// no managed or PATH binary is found. All dolt command invocations should
// use this instead of a hardcoded "dolt" string.
func doltBin() string {
	if p := doltResolvedBinPath(); p != "" {
		return p
	}
	return "dolt"
}

// --- Dolt server lifecycle ---

// doltPIDPath returns the path to the dolt PID file.
// Uses the global dolt dir so the PID is shared across all repos on this machine.
func doltPIDPath() string {
	return filepath.Join(doltGlobalDir(), "dolt.pid")
}

// daemonPIDPath returns the path to the daemon PID file.
// Uses the global dir so up/down/status work from any directory.
func daemonPIDPath() string {
	return filepath.Join(doltGlobalDir(), "daemon.pid")
}

// doltIsReachable checks if the dolt server is reachable via TCP.
func doltIsReachable() bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(doltHost(), doltPort()), 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// requireDolt checks that the dolt server is reachable. Returns a user-friendly error if not.
func requireDolt() error {
	if doltIsReachable() {
		return nil
	}
	return fmt.Errorf("dolt not reachable on %s:%s — run: spire up", doltHost(), doltPort())
}

// doltServerStatus returns the current state of the dolt server.
func doltServerStatus() (pid int, running bool, reachable bool) {
	pid = readPID(doltPIDPath())
	if pid > 0 && processAlive(pid) {
		running = true
		reachable = doltIsReachable()
	} else if pid > 0 {
		// Stale PID file
		os.Remove(doltPIDPath())
		pid = 0
		reachable = doltIsReachable() // port may be held by another process
	} else {
		reachable = doltIsReachable() // no PID file, but maybe started externally
	}
	return
}

// doltWriteConfig writes the dolt server config file to the global spire dir.
func doltWriteConfig() (string, error) {
	configPath := filepath.Join(doltGlobalDir(), "dolt-config.yaml")
	content := fmt.Sprintf(`listener:
  host: "%s"
  port: %s
  max_connections: 100
`, doltHost(), doltPort())
	return configPath, os.WriteFile(configPath, []byte(content), 0644)
}

// doltStart starts the dolt sql-server as a background process.
// Returns the PID if started, or an error.
func doltStart() (int, error) {
	// Check if already running
	pid, running, reachable := doltServerStatus()
	if running && reachable {
		return pid, nil
	}
	if !running && reachable {
		return 0, fmt.Errorf("port %s already in use (not by our dolt process)", doltPort())
	}

	// Ensure data dir exists and is initialized
	dataDir := doltDataDir()
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return 0, fmt.Errorf("create dolt data dir: %w", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, ".dolt")); os.IsNotExist(err) {
		initCmd := exec.Command(doltBin(), "init")
		initCmd.Dir = dataDir
		if out, err := initCmd.CombinedOutput(); err != nil {
			return 0, fmt.Errorf("dolt init: %s\n%s", err, string(out))
		}
	}

	// Write config
	configPath, err := doltWriteConfig()
	if err != nil {
		return 0, fmt.Errorf("write dolt config: %w", err)
	}

	// Start dolt sql-server
	binPath := doltBin()
	if binPath == "dolt" {
		// Bare "dolt" fallback — verify it's actually in PATH
		if _, err := exec.LookPath("dolt"); err != nil {
			return 0, fmt.Errorf("dolt not found — run `spire up` to auto-download, or install manually")
		}
	}

	cmd := exec.Command(binPath, "sql-server", "--config", configPath)
	cmd.Dir = dataDir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	// Redirect output to log files in global dir (shared across repos)
	gd := doltGlobalDir()
	logFile, _ := os.OpenFile(filepath.Join(gd, "dolt.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	errFile, _ := os.OpenFile(filepath.Join(gd, "dolt.error.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	cmd.Stdout = logFile
	cmd.Stderr = errFile

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start dolt: %w", err)
	}

	newPID := cmd.Process.Pid
	writePID(doltPIDPath(), newPID)

	// Release the process so it continues after we exit
	cmd.Process.Release()

	// Close log file handles (the child process has its own references)
	if logFile != nil {
		logFile.Close()
	}
	if errFile != nil {
		errFile.Close()
	}

	// Wait for port to become reachable
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if doltIsReachable() {
			return newPID, nil
		}
		time.Sleep(200 * time.Millisecond)
	}

	return newPID, fmt.Errorf("dolt started (pid %d) but port %s not reachable after 5s", newPID, doltPort())
}

// doltStop stops the dolt server.
func doltStop() error {
	pid := readPID(doltPIDPath())
	if pid <= 0 || !processAlive(pid) {
		os.Remove(doltPIDPath())
		return fmt.Errorf("not running")
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		os.Remove(doltPIDPath())
		return fmt.Errorf("not running")
	}

	// Send SIGTERM
	proc.Signal(syscall.SIGTERM)

	// Wait up to 5 seconds
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			os.Remove(doltPIDPath())
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Force kill
	proc.Signal(syscall.SIGKILL)
	time.Sleep(500 * time.Millisecond)
	os.Remove(doltPIDPath())
	return nil
}

// ensureDatabase creates a database on the dolt server if it doesn't exist.
// Uses a raw dolt connection without --use-db to avoid the chicken-and-egg problem.
func ensureDatabase(name string) error {
	cmd := exec.Command(doltBin(),
		"--host", doltHost(),
		"--port", doltPort(),
		"--user", "root",
		"--no-tls",
		"sql", "-q", fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s`", name),
	)
	cmd.Env = append(os.Environ(), "DOLT_CLI_PASSWORD=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s\n%s", err, string(out))
	}
	return nil
}

// stopProcess stops a process by PID with SIGTERM then SIGKILL.
// Removes the PID file when done.
func stopProcess(pidPath string) (bool, error) {
	pid := readPID(pidPath)
	if pid <= 0 || !processAlive(pid) {
		if pid > 0 {
			os.Remove(pidPath)
		}
		return false, nil
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		os.Remove(pidPath)
		return false, nil
	}

	proc.Signal(syscall.SIGTERM)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			os.Remove(pidPath)
			return true, nil
		}
		time.Sleep(200 * time.Millisecond)
	}

	proc.Signal(syscall.SIGKILL)
	time.Sleep(500 * time.Millisecond)
	os.Remove(pidPath)
	return true, nil
}
