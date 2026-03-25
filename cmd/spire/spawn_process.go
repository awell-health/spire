package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
)

// processSpawner spawns agents as local OS processes.
type processSpawner struct{}

// processHandle tracks a locally-spawned agent process.
type processHandle struct {
	name   string
	cmd    *exec.Cmd
	exited atomic.Bool
}

func (s *processSpawner) Spawn(cfg SpawnConfig) (AgentHandle, error) {
	spireBin, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("find spire binary: %w", err)
	}

	// Map role to spire subcommand.
	var subcmd string
	switch cfg.Role {
	case RoleApprentice:
		subcmd = "wizard-run"
	case RoleSage:
		subcmd = "wizard-review"
	case RoleWizard:
		subcmd = "workshop"
	default:
		return nil, fmt.Errorf("unknown spawn role: %q", cfg.Role)
	}

	args := []string{subcmd, cfg.BeadID, "--name", cfg.Name}
	args = append(args, cfg.ExtraArgs...)

	cmd := exec.Command(spireBin, args...)
	cmd.Env = os.Environ()

	if cfg.LogPath != "" {
		os.MkdirAll(filepath.Dir(cfg.LogPath), 0755)
		logFile, err := os.OpenFile(cfg.LogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			return nil, fmt.Errorf("open log %s: %w", cfg.LogPath, err)
		}
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		// Start duplicates the fd for the child. Close our copy after Start.
		defer logFile.Close()
	} else {
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	return &processHandle{name: cfg.Name, cmd: cmd}, nil
}

// Wait blocks until the process exits.
func (h *processHandle) Wait() error {
	err := h.cmd.Wait()
	h.exited.Store(true)
	return err
}

// Signal sends a signal to the process.
func (h *processHandle) Signal(sig os.Signal) error {
	if h.exited.Load() {
		return fmt.Errorf("process already exited")
	}
	if h.cmd.Process == nil {
		return fmt.Errorf("process not started")
	}
	return h.cmd.Process.Signal(sig)
}

// Alive returns true if the process is still running.
func (h *processHandle) Alive() bool {
	if h.exited.Load() {
		return false
	}
	if h.cmd.Process == nil {
		return false
	}
	return h.cmd.Process.Signal(syscall.Signal(0)) == nil
}

// Name returns the agent name.
func (h *processHandle) Name() string { return h.name }

// Identifier returns the PID as a string.
func (h *processHandle) Identifier() string {
	if h.cmd.Process != nil {
		return strconv.Itoa(h.cmd.Process.Pid)
	}
	return ""
}
