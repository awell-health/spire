# Spire Lifecycle Management — Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `spire up/down/shutdown/status` commands for process lifecycle management, a `requireDolt()` guard for user-facing commands, and strip LaunchAgent setup from `setup.sh`.

**Architecture:** PID-file based process management in `.spire/` directory. New file `doltserver.go` for dolt server lifecycle. Each command in its own file. Daemon self-registers its PID.

**Tech Stack:** Go 1.26 (stdlib only), beads CLI (`bd`), existing `spire` infrastructure

**Spec:** `docs/superpowers/specs/2026-03-16-spire-lifecycle-management.md`

---

## File Structure

```
cmd/spire/
  doltserver.go  — NEW: dolt server config, start, stop, status, reachability, requireDolt
  up.go          — NEW: spire up command
  down.go        — NEW: spire down command
  shutdown.go    — NEW: spire shutdown command
  status.go      — NEW: spire status command (rename to avoid conflict: lifecycle_status.go)
  main.go        — MODIFY: add up/down/shutdown/status cases
  daemon.go      — MODIFY: write daemon.pid on startup
  collect.go     — MODIFY: add requireDolt()
  send.go        — MODIFY: add requireDolt()
  focus.go       — MODIFY: add requireDolt()
  grok.go        — MODIFY: add requireDolt()
  read.go        — MODIFY: add requireDolt()
  serve.go       — MODIFY: add requireDolt()
  spire_test.go  — MODIFY: add lifecycle tests

setup.sh         — MODIFY: strip LaunchAgent sections
```

Note: Use `lifecycle_status.go` as the filename to avoid any potential collision with other status-related code.

---

## Chunk 1: Dolt Server Management (spi-ohs.1)

### Task 1: doltserver.go — config, start, stop, status, reachability

**Files:**
- Create: `cmd/spire/doltserver.go`

- [ ] **Step 1: Write doltserver.go with config and directory helpers**

Create `cmd/spire/doltserver.go`:

```go
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

// spireDir returns the path to the .spire/ directory (sibling of .beads/).
// Creates it if it does not exist.
func spireDir() (string, error) {
	// Walk up from cwd looking for .beads/
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".beads")); err == nil {
			sd := filepath.Join(dir, ".spire")
			os.MkdirAll(sd, 0755)
			return sd, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	// Fallback: use cwd
	sd := filepath.Join(".", ".spire")
	os.MkdirAll(sd, 0755)
	return sd, nil
}

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

// doltPort returns the configured dolt server port.
func doltPort() string {
	if p := os.Getenv("BEADS_DOLT_SERVER_PORT"); p != "" {
		return p
	}
	return "3307"
}

// doltHost returns the configured dolt server host.
func doltHost() string {
	if h := os.Getenv("BEADS_DOLT_SERVER_HOST"); h != "" {
		return h
	}
	return "127.0.0.1"
}
```

- [ ] **Step 2: Add PID file helpers**

Append to `doltserver.go`:

```go
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
```

- [ ] **Step 3: Add dolt server start/stop/status functions**

Append to `doltserver.go`:

```go
// doltPIDPath returns the path to the dolt PID file.
func doltPIDPath() string {
	sd, _ := spireDir()
	return filepath.Join(sd, "dolt.pid")
}

// daemonPIDPath returns the path to the daemon PID file.
func daemonPIDPath() string {
	sd, _ := spireDir()
	return filepath.Join(sd, "daemon.pid")
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

// doltStatus returns the current state of the dolt server.
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

// doltWriteConfig writes the dolt server config file to .spire/dolt-config.yaml.
func doltWriteConfig() (string, error) {
	sd, err := spireDir()
	if err != nil {
		return "", err
	}
	configPath := filepath.Join(sd, "dolt-config.yaml")
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
		return pid, nil // already running
	}
	if !running && reachable {
		// Port is in use by something else
		return 0, fmt.Errorf("port %s already in use (not by our dolt process)", doltPort())
	}

	// Ensure data dir exists and is initialized
	dataDir := doltDataDir()
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return 0, fmt.Errorf("create dolt data dir: %w", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, ".dolt")); os.IsNotExist(err) {
		// Initialize dolt
		initCmd := exec.Command("dolt", "init")
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
	doltBin, err := exec.LookPath("dolt")
	if err != nil {
		return 0, fmt.Errorf("dolt not found in PATH")
	}

	cmd := exec.Command(doltBin, "sql-server", "--config", configPath)
	cmd.Dir = dataDir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// Redirect output to log files
	sd, _ := spireDir()
	logFile, _ := os.OpenFile(filepath.Join(sd, "dolt.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	errFile, _ := os.OpenFile(filepath.Join(sd, "dolt.error.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	cmd.Stdout = logFile
	cmd.Stderr = errFile

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start dolt: %w", err)
	}

	newPID := cmd.Process.Pid
	writePID(doltPIDPath(), newPID)

	// Release the process so it continues after we exit
	cmd.Process.Release()

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
```

---

## Chunk 2: Spire Up (spi-ohs.2)

### Task 2: up.go — bootstrap dolt + daemon

**Files:**
- Create: `cmd/spire/up.go`

- [ ] **Step 1: Write up.go**

Create `cmd/spire/up.go`:

```go
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

func cmdUp(args []string) error {
	// Parse flags
	interval := "2m"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--interval":
			if i+1 >= len(args) {
				return fmt.Errorf("--interval requires a value")
			}
			i++
			interval = args[i]
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire up [--interval 2m]", args[i])
		}
	}

	// Step 1: Start dolt server
	fmt.Print("dolt server: ")
	pid, running, reachable := doltServerStatus()
	if running && reachable {
		fmt.Printf("already running (pid %d, port %s)\n", pid, doltPort())
	} else {
		newPID, err := doltStart()
		if err != nil {
			fmt.Printf("error: %s\n", err)
			return fmt.Errorf("cannot start dolt server: %w", err)
		}
		fmt.Printf("started (pid %d, port %s)\n", newPID, doltPort())
	}

	// Step 2: Start daemon
	fmt.Print("spire daemon: ")
	daemonPID := readPID(daemonPIDPath())
	if daemonPID > 0 && processAlive(daemonPID) {
		fmt.Printf("already running (pid %d)\n", daemonPID)
		return nil
	}

	// Remove stale PID file
	if daemonPID > 0 {
		os.Remove(daemonPIDPath())
	}

	// Start daemon as background process
	spireBin, err := os.Executable()
	if err != nil {
		spireBin, err = exec.LookPath("spire")
		if err != nil {
			return fmt.Errorf("cannot find spire binary")
		}
	}

	cmd := exec.Command(spireBin, "daemon", "--interval", interval)
	cmd.Dir, _ = os.Getwd()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = os.Environ()

	// Redirect daemon output to log files
	sd, _ := spireDir()
	logFile, _ := os.OpenFile(sd+"/daemon.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	errFile, _ := os.OpenFile(sd+"/daemon.error.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	cmd.Stdout = logFile
	cmd.Stderr = errFile

	if err := cmd.Start(); err != nil {
		fmt.Printf("error: %s\n", err)
		return fmt.Errorf("cannot start daemon: %w", err)
	}

	newPID := cmd.Process.Pid
	writePID(daemonPIDPath(), newPID)
	cmd.Process.Release()

	// Brief wait to confirm it stayed alive
	time.Sleep(500 * time.Millisecond)
	if processAlive(newPID) {
		fmt.Printf("started (pid %d, interval %s)\n", newPID, interval)
	} else {
		fmt.Printf("started but may have exited (pid %d)\n", newPID)
	}

	return nil
}
```

- [ ] **Step 2: Update daemon.go to write its own PID file**

Add near the top of `cmdDaemon()`, after flag parsing:

```go
// Write our PID file so spire down can find us
if sd, err := spireDir(); err == nil {
    writePID(filepath.Join(sd, "daemon.pid"), os.Getpid())
}
```

---

## Chunk 3: Spire Down (spi-ohs.3)

### Task 3: down.go — stop daemon

**Files:**
- Create: `cmd/spire/down.go`

- [ ] **Step 1: Write down.go**

Create `cmd/spire/down.go`:

```go
package main

import (
	"fmt"
	"os"
	"syscall"
	"time"
)

func cmdDown(args []string) error {
	pid := readPID(daemonPIDPath())
	if pid <= 0 || !processAlive(pid) {
		if pid > 0 {
			os.Remove(daemonPIDPath())
		}
		fmt.Println("daemon: not running")
		return nil
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		os.Remove(daemonPIDPath())
		fmt.Println("daemon: not running")
		return nil
	}

	proc.Signal(syscall.SIGTERM)

	// Wait for graceful exit
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	if processAlive(pid) {
		proc.Signal(syscall.SIGKILL)
		time.Sleep(500 * time.Millisecond)
	}

	os.Remove(daemonPIDPath())
	fmt.Println("daemon: stopped (dolt still running)")
	return nil
}
```

---

## Chunk 4: Spire Shutdown (spi-ohs.4)

### Task 4: shutdown.go — full teardown

**Files:**
- Create: `cmd/spire/shutdown.go`

- [ ] **Step 1: Write shutdown.go**

Create `cmd/spire/shutdown.go`:

```go
package main

import "fmt"

func cmdShutdown(args []string) error {
	// Stop daemon first
	fmt.Print("daemon: ")
	pid := readPID(daemonPIDPath())
	if pid > 0 && processAlive(pid) {
		cmdDown(nil)
	} else {
		fmt.Println("not running")
	}

	// Stop dolt
	fmt.Print("dolt server: ")
	err := doltStop()
	if err != nil {
		fmt.Println(err)
	} else {
		fmt.Println("stopped")
	}

	return nil
}
```

---

## Chunk 5: Spire Status (spi-ohs.5)

### Task 5: lifecycle_status.go — health check

**Files:**
- Create: `cmd/spire/lifecycle_status.go`

- [ ] **Step 1: Write lifecycle_status.go**

Create `cmd/spire/lifecycle_status.go`:

```go
package main

import "fmt"

func cmdStatus(args []string) error {
	// Dolt server
	pid, running, reachable := doltServerStatus()
	if running && reachable {
		fmt.Printf("dolt server: running (pid %d, port %s, reachable)\n", pid, doltPort())
	} else if running {
		fmt.Printf("dolt server: running (pid %d, port %s, NOT reachable)\n", pid, doltPort())
	} else if reachable {
		fmt.Printf("dolt server: running externally (port %s reachable, no PID file)\n", doltPort())
	} else {
		fmt.Println("dolt server: not running")
	}

	// Daemon
	daemonPID := readPID(daemonPIDPath())
	if daemonPID > 0 && processAlive(daemonPID) {
		fmt.Printf("spire daemon: running (pid %d)\n", daemonPID)
	} else {
		if daemonPID > 0 {
			// Stale PID
			fmt.Println("spire daemon: not running (stale PID file cleaned)")
			os.Remove(daemonPIDPath())
		} else {
			fmt.Println("spire daemon: not running")
		}
	}

	return nil
}
```

Note: Add `"os"` import.

---

## Chunk 6: requireDolt Guard (spi-ohs.6)

### Task 6: Add requireDolt() to user-facing commands

**Files:**
- Modify: `cmd/spire/collect.go`
- Modify: `cmd/spire/send.go`
- Modify: `cmd/spire/focus.go`
- Modify: `cmd/spire/grok.go`
- Modify: `cmd/spire/read.go`
- Modify: `cmd/spire/serve.go`

- [ ] **Step 1: Add requireDolt() call at the top of each command function**

For each of these files, add as the first line of the command function:

```go
if err := requireDolt(); err != nil {
    return err
}
```

Specifically:
- `collect.go`: top of `cmdCollect()`
- `send.go`: top of `cmdSend()`
- `focus.go`: top of `cmdFocus()`
- `grok.go`: top of `cmdGrok()`
- `read.go`: top of `cmdRead()`
- `serve.go`: top of `cmdServe()`

---

## Chunk 7: Wire Up main.go

### Task 7: Add new commands to the switch

**Files:**
- Modify: `cmd/spire/main.go`

- [ ] **Step 1: Add cases for up, down, shutdown, status**

In `main.go`, add to the switch statement:

```go
case "up":
    err = cmdUp(args)
case "down":
    err = cmdDown(args)
case "shutdown":
    err = cmdShutdown(args)
case "status":
    err = cmdStatus(args)
```

- [ ] **Step 2: Update printUsage()**

Add to the usage string:

```
  up                    Start dolt server + daemon (--interval)
  down                  Stop daemon (dolt keeps running)
  shutdown              Stop daemon + dolt server
  status                Show running state of dolt + daemon
```

---

## Chunk 8: Strip LaunchAgent from setup.sh (spi-ohs.7)

### Task 8: Clean up setup.sh

**Files:**
- Modify: `setup.sh`

- [ ] **Step 1: Remove Step 2 LaunchAgent plist creation**

Replace the entire "Step 2: Central dolt server" section. Keep:
- Dolt data dir creation and initialization
- Dolt config.yaml writing
- Env var setup in `~/.zshrc`

Remove:
- LaunchAgent plist creation and loading
- The `launchctl` commands

- [ ] **Step 2: Remove Step 8 Spire daemon LaunchAgent**

Remove the entire "Step 8: Spire daemon LaunchAgent" section.

- [ ] **Step 3: Update printed instructions**

Change the "Next steps" output at the end to say:

```
echo "  spire up                       # start dolt + daemon"
```

Instead of referencing daemon LaunchAgent.

Also update `welcome.go` line referencing `spire daemon` to say `spire up`.

---

## Chunk 9: Tests

### Task 9: Add tests to spire_test.go

**Files:**
- Modify: `cmd/spire/spire_test.go`

- [ ] **Step 1: Add unit tests**

```go
func TestReadWritePID(t *testing.T) {
    // Test write and read back
    tmpFile := filepath.Join(t.TempDir(), "test.pid")
    err := writePID(tmpFile, 12345)
    if err != nil {
        t.Fatalf("writePID error: %v", err)
    }
    got := readPID(tmpFile)
    if got != 12345 {
        t.Errorf("readPID = %d, want 12345", got)
    }
}

func TestReadPIDMissing(t *testing.T) {
    got := readPID("/nonexistent/path/test.pid")
    if got != 0 {
        t.Errorf("readPID(missing) = %d, want 0", got)
    }
}

func TestProcessAlive(t *testing.T) {
    // Current process should be alive
    if !processAlive(os.Getpid()) {
        t.Error("processAlive(self) = false, want true")
    }
    // PID 0 should not be alive
    if processAlive(0) {
        t.Error("processAlive(0) = true, want false")
    }
    // Very large PID should not be alive
    if processAlive(99999999) {
        t.Error("processAlive(99999999) = true, want false")
    }
}

func TestDoltPort(t *testing.T) {
    os.Unsetenv("BEADS_DOLT_SERVER_PORT")
    if p := doltPort(); p != "3307" {
        t.Errorf("doltPort() = %q, want %q", p, "3307")
    }
    os.Setenv("BEADS_DOLT_SERVER_PORT", "3308")
    defer os.Unsetenv("BEADS_DOLT_SERVER_PORT")
    if p := doltPort(); p != "3308" {
        t.Errorf("doltPort() = %q, want %q", p, "3308")
    }
}

func TestDoltHost(t *testing.T) {
    os.Unsetenv("BEADS_DOLT_SERVER_HOST")
    if h := doltHost(); h != "127.0.0.1" {
        t.Errorf("doltHost() = %q, want %q", h, "127.0.0.1")
    }
}
```

- [ ] **Step 2: Add integration test for up/status/down/shutdown**

```go
func TestIntegrationLifecycle(t *testing.T) {
    requireBd(t)
    // This test requires dolt to be available
    if _, err := exec.LookPath("dolt"); err != nil {
        t.Skip("dolt not available")
    }

    // Test status when nothing is running
    err := cmdStatus(nil)
    if err != nil {
        t.Fatalf("status error: %v", err)
    }

    // Test that requireDolt works (should succeed since dolt is running via env)
    if doltIsReachable() {
        err = requireDolt()
        if err != nil {
            t.Errorf("requireDolt() failed but dolt is reachable: %v", err)
        }
    }
}
```

---

## Chunk 10: Build and verify

- [ ] **Step 1: Build**

```bash
cd /Users/jb/awell/spire && go build ./cmd/spire/
```

- [ ] **Step 2: Run tests**

```bash
cd /Users/jb/awell/spire && go test ./cmd/spire/ -v -count=1
```

- [ ] **Step 3: Verify new commands**

```bash
./spire up --help 2>&1 || true
./spire status
./spire down
./spire shutdown
./spire help
```
