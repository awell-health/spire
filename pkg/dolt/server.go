// Package dolt provides dolt server lifecycle management, binary resolution,
// push/pull/sync operations, and merge conflict resolution.
package dolt

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

// DataDir returns the dolt database directory.
func DataDir() string {
	if d := os.Getenv("DOLT_DATA_DIR"); d != "" {
		return d
	}
	if runtime.GOOS == "darwin" {
		return "/opt/homebrew/var/dolt"
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "dolt")
}

// GlobalDir returns a user-level directory for dolt server state (PID, config, logs).
// This is kept separate from any repo's .spire/ since dolt is a shared singleton.
func GlobalDir() string {
	if d := os.Getenv("SPIRE_DOLT_DIR"); d != "" {
		os.MkdirAll(d, 0755)
		return d
	}
	home, _ := os.UserHomeDir()
	d := filepath.Join(home, ".local", "share", "spire")
	os.MkdirAll(d, 0755)
	return d
}

// Port returns the configured dolt server port.
func Port() string {
	for _, key := range []string{"BEADS_DOLT_SERVER_PORT", "DOLT_PORT"} {
		if p := os.Getenv(key); p != "" {
			return p
		}
	}
	return "3307"
}

// Host returns the configured dolt server host.
func Host() string {
	for _, key := range []string{"BEADS_DOLT_SERVER_HOST", "DOLT_HOST"} {
		if h := os.Getenv(key); h != "" {
			return h
		}
	}
	return "127.0.0.1"
}

// --- PID file helpers ---

// DoltPIDPath returns the path to the dolt PID file.
// Uses the global dolt dir so the PID is shared across all repos on this machine.
func DoltPIDPath() string {
	return filepath.Join(GlobalDir(), "dolt.pid")
}

// DaemonPIDPath returns the path to the daemon PID file.
// Uses the global dir so up/down/status work from any directory.
func DaemonPIDPath() string {
	return filepath.Join(GlobalDir(), "daemon.pid")
}

// StewardPIDPath returns the path to the steward PID file.
// Uses the global dir so up/down/status work from any directory.
func StewardPIDPath() string {
	return filepath.Join(GlobalDir(), "steward.pid")
}

// IsReachable checks if the dolt server is reachable via TCP.
func IsReachable() bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(Host(), Port()), 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// RequireDolt checks that the dolt server is reachable. Returns a user-friendly error if not.
func RequireDolt() error {
	if IsReachable() {
		return nil
	}
	return fmt.Errorf("dolt not reachable on %s:%s — run: spire up", Host(), Port())
}

// ServerStatus returns the current state of the dolt server.
func ServerStatus() (pid int, running bool, reachable bool) {
	pid = ReadPID(DoltPIDPath())
	if pid > 0 && ProcessAlive(pid) {
		running = true
		reachable = IsReachable()
	} else if pid > 0 {
		// Stale PID file
		os.Remove(DoltPIDPath())
		pid = 0
		reachable = IsReachable() // port may be held by another process
	} else {
		reachable = IsReachable() // no PID file, but maybe started externally
	}
	return
}

// EnsureIdentity ensures dolt has user.name and user.email set globally.
// Without these, dolt init fails with "Author identity unknown".
// Prompts the user interactively if not already configured.
func EnsureIdentity() {
	bin := Bin()
	reader := bufio.NewReader(os.Stdin)

	// Check user.name
	out, err := exec.Command(bin, "config", "--global", "--get", "user.name").Output()
	if err != nil || len(out) == 0 || string(out) == "\n" {
		fmt.Println("Dolt needs your identity for commit history (like git).")
		fmt.Print("Your name: ")
		name, _ := reader.ReadString('\n')
		name = trimSpace(name)
		if name == "" {
			name = "spire-user"
		}
		exec.Command(bin, "config", "--global", "--add", "user.name", name).Run()
	}

	// Check user.email
	out, err = exec.Command(bin, "config", "--global", "--get", "user.email").Output()
	if err != nil || len(out) == 0 || string(out) == "\n" {
		fmt.Print("Your email: ")
		email, _ := reader.ReadString('\n')
		email = trimSpace(email)
		if email == "" {
			email = "spire@localhost"
		}
		exec.Command(bin, "config", "--global", "--add", "user.email", email).Run()
	}
}

// WriteConfig writes the dolt server config file to the global spire dir.
func WriteConfig() (string, error) {
	configPath := filepath.Join(GlobalDir(), "dolt-config.yaml")
	content := fmt.Sprintf(`listener:
  host: "%s"
  port: %s
  max_connections: 100
`, Host(), Port())
	return configPath, os.WriteFile(configPath, []byte(content), 0644)
}

// Start starts the dolt sql-server as a background process.
// Returns the PID if started, or an error.
func Start() (int, error) {
	// Check if already running
	pid, running, reachable := ServerStatus()
	if running && reachable {
		return pid, nil
	}
	if !running && reachable {
		return 0, fmt.Errorf("port %s already in use (not by our dolt process)", Port())
	}

	// Ensure data dir exists and is initialized
	dataDir := DataDir()
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return 0, fmt.Errorf("create dolt data dir: %w", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, ".dolt")); os.IsNotExist(err) {
		// Ensure dolt has user identity — dolt init fails without it.
		EnsureIdentity()
		initCmd := exec.Command(Bin(), "init")
		initCmd.Dir = dataDir
		if out, err := initCmd.CombinedOutput(); err != nil {
			return 0, fmt.Errorf("dolt init: %s\n%s", err, string(out))
		}
	}

	// Write config
	configPath, err := WriteConfig()
	if err != nil {
		return 0, fmt.Errorf("write dolt config: %w", err)
	}

	// Start dolt sql-server
	binPath := Bin()
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
	gd := GlobalDir()
	logFile, _ := os.OpenFile(filepath.Join(gd, "dolt.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	errFile, _ := os.OpenFile(filepath.Join(gd, "dolt.error.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	cmd.Stdout = logFile
	cmd.Stderr = errFile

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start dolt: %w", err)
	}

	newPID := cmd.Process.Pid
	WritePID(DoltPIDPath(), newPID)

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
		if IsReachable() {
			return newPID, nil
		}
		time.Sleep(200 * time.Millisecond)
	}

	return newPID, fmt.Errorf("dolt started (pid %d) but port %s not reachable after 5s", newPID, Port())
}

// Stop stops the dolt server.
func Stop() error {
	pid := ReadPID(DoltPIDPath())
	if pid <= 0 || !ProcessAlive(pid) {
		os.Remove(DoltPIDPath())
		return fmt.Errorf("not running")
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		os.Remove(DoltPIDPath())
		return fmt.Errorf("not running")
	}

	// Send SIGTERM
	proc.Signal(syscall.SIGTERM)

	// Wait up to 5 seconds
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !ProcessAlive(pid) {
			os.Remove(DoltPIDPath())
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Force kill
	proc.Signal(syscall.SIGKILL)
	time.Sleep(500 * time.Millisecond)
	os.Remove(DoltPIDPath())
	return nil
}

// EnsureDatabase creates a database on the dolt server if it doesn't exist.
// Uses a raw dolt connection without --use-db to avoid the chicken-and-egg problem.
func EnsureDatabase(name string) error {
	cmd := exec.Command(Bin(),
		"--host", Host(),
		"--port", Port(),
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
