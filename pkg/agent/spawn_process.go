package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
)

// ProcessSpawner spawns agents as local OS processes.
type ProcessSpawner struct{}

// ProcessHandle tracks a locally-spawned agent process.
type ProcessHandle struct {
	name   string
	cmd    *exec.Cmd
	exited atomic.Bool
}

// NewProcessHandle creates a ProcessHandle wrapping an already-started exec.Cmd.
// Used by tests that need to control the underlying process directly.
func NewProcessHandle(name string, cmd *exec.Cmd) *ProcessHandle {
	return &ProcessHandle{name: name, cmd: cmd}
}

func (s *ProcessSpawner) Spawn(cfg SpawnConfig) (Handle, error) {
	spireBin, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("find spire binary: %w", err)
	}

	// Map role to spire subcommand.
	subcmd, err := roleToSubcmd(cfg.Role)
	if err != nil {
		return nil, err
	}

	args := append([]string{}, subcmd...)
	args = append(args, cfg.BeadID, "--name", cfg.Name)
	if cfg.StartRef != "" {
		args = append(args, "--start-ref", cfg.StartRef)
	}

	// Write custom prompt to a temp file to avoid arg-length limits and shell
	// escaping issues with multi-line prompts. The wizard subprocess reads and
	// removes the file after parsing.
	if cfg.CustomPrompt != "" {
		f, err := os.CreateTemp("", "spire-prompt-*.txt")
		if err != nil {
			return nil, fmt.Errorf("write custom prompt temp file: %w", err)
		}
		if _, err := f.WriteString(cfg.CustomPrompt); err != nil {
			f.Close()
			os.Remove(f.Name())
			return nil, fmt.Errorf("write custom prompt: %w", err)
		}
		f.Close()
		args = append(args, "--custom-prompt-file", f.Name())
	}

	args = append(args, cfg.ExtraArgs...)

	cmd := exec.Command(spireBin, args...)
	cmd.Env = os.Environ()
	applyProcessEnv(cmd, cfg)

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

	return &ProcessHandle{name: cfg.Name, cmd: cmd}, nil
}

// Wait blocks until the process exits.
func (h *ProcessHandle) Wait() error {
	err := h.cmd.Wait()
	h.exited.Store(true)
	return err
}

// Signal sends a signal to the process.
func (h *ProcessHandle) Signal(sig os.Signal) error {
	if h.exited.Load() {
		return fmt.Errorf("process already exited")
	}
	if h.cmd.Process == nil {
		return fmt.Errorf("process not started")
	}
	return h.cmd.Process.Signal(sig)
}

// Alive returns true if the process is still running.
func (h *ProcessHandle) Alive() bool {
	if h.exited.Load() {
		return false
	}
	if h.cmd.Process == nil {
		return false
	}
	return h.cmd.Process.Signal(syscall.Signal(0)) == nil
}

// Name returns the agent name.
func (h *ProcessHandle) Name() string { return h.name }

// Identifier returns the PID as a string.
func (h *ProcessHandle) Identifier() string {
	if h.cmd.Process != nil {
		return strconv.Itoa(h.cmd.Process.Pid)
	}
	return ""
}

// applyProcessEnv injects all SpawnConfig-derived env vars into cmd.Env.
// Extracted from Spawn so tests can verify the config-to-env translation
// without actually starting a process.
func applyProcessEnv(cmd *exec.Cmd, cfg SpawnConfig) {
	if cfg.Tower != "" {
		setEnv(cmd, "SPIRE_TOWER", cfg.Tower)
	}
	if cfg.Provider != "" {
		setEnv(cmd, "SPIRE_PROVIDER", cfg.Provider)
	}
	if cfg.Role != "" {
		setEnv(cmd, "SPIRE_ROLE", string(cfg.Role))
	}

	// Apprentice identity env vars. Transport-agnostic: the apprentice reads
	// them to resolve which bead to write to and what role to claim at
	// submit time.
	if cfg.BeadID != "" {
		setEnv(cmd, "SPIRE_BEAD_ID", cfg.BeadID)
	}
	if cfg.AttemptID != "" {
		setEnv(cmd, "SPIRE_ATTEMPT_ID", cfg.AttemptID)
	}
	if cfg.ApprenticeIdx != "" {
		setEnv(cmd, "SPIRE_APPRENTICE_IDX", cfg.ApprenticeIdx)
	}

	// OTLP telemetry. The daemon's OTLP receiver listens on localhost:4317
	// (or SPIRE_OTLP_PORT).
	otlpPort := os.Getenv("SPIRE_OTLP_PORT")
	if otlpPort == "" {
		otlpPort = "4317"
	}
	setEnv(cmd, "OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:"+otlpPort)

	// Resource attributes carry bead context so the receiver can correlate
	// spans to beads without post-hoc matching.
	var resAttrs []string
	if cfg.BeadID != "" {
		resAttrs = append(resAttrs, "bead.id="+cfg.BeadID)
	}
	if cfg.Name != "" {
		resAttrs = append(resAttrs, "agent.name="+cfg.Name)
	}
	if cfg.Step != "" {
		resAttrs = append(resAttrs, "step="+cfg.Step)
	}
	if cfg.Tower != "" {
		resAttrs = append(resAttrs, "tower="+cfg.Tower)
	}
	if len(resAttrs) > 0 {
		setEnv(cmd, "OTEL_RESOURCE_ATTRIBUTES", strings.Join(resAttrs, ","))
	}

	// Claude Code: enable built-in OTel telemetry with trace export.
	if cfg.Provider == "" || cfg.Provider == "claude" {
		setEnv(cmd, "CLAUDE_CODE_ENABLE_TELEMETRY", "1")
		setEnv(cmd, "CLAUDE_CODE_ENHANCED_TELEMETRY_BETA", "1")
		setEnv(cmd, "OTEL_TRACES_EXPORTER", "otlp")
		setEnv(cmd, "OTEL_LOGS_EXPORTER", "otlp")
		setEnv(cmd, "OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")
	}
}

// setEnv sets or replaces an environment variable in cmd.Env.
func setEnv(cmd *exec.Cmd, key, value string) {
	prefix := key + "="
	for i, e := range cmd.Env {
		if strings.HasPrefix(e, prefix) {
			cmd.Env[i] = prefix + value
			return
		}
	}
	cmd.Env = append(cmd.Env, prefix+value)
}

// roleToSubcmd maps a SpawnRole to the spire subcommand argv tokens.
// Returns a slice so multi-word role-scoped subcommands (e.g. "apprentice run")
// can be spliced into the command line by each backend.
//
// RoleWizard and RoleExecutor both map to "execute": the in-pod command is
// `spire execute`, and the wizard identity lives in the enum (surfaced via the
// SPIRE_ROLE env var and role-specific pod spec / resources).
func roleToSubcmd(role SpawnRole) ([]string, error) {
	switch role {
	case RoleApprentice:
		return []string{"apprentice", "run"}, nil
	case RoleSage:
		return []string{"sage", "review"}, nil
	case RoleWizard:
		return []string{"execute"}, nil
	case RoleExecutor:
		return []string{"execute"}, nil
	default:
		return nil, fmt.Errorf("unknown spawn role: %q", role)
	}
}
