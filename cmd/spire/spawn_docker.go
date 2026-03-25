package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/awell-health/spire/pkg/repoconfig"
)

// dockerSpawner spawns agents as Docker containers.
type dockerSpawner struct {
	// Image overrides the default container image.
	// If empty, falls back to spire.yaml config, then to defaultDockerImage.
	Image string

	// Network overrides the Docker network mode.
	// If empty, falls back to spire.yaml config, then "host".
	Network string

	// ExtraVolumes are additional -v mounts (host:container format).
	ExtraVolumes []string

	// ExtraEnv are additional -e KEY=VALUE environment entries.
	ExtraEnv []string
}

const defaultDockerImage = "ghcr.io/awell-health/spire-agent:latest"

// dockerHandle tracks a running Docker container.
type dockerHandle struct {
	name        string
	containerID string
	exited      atomic.Bool
}

// newDockerSpawner creates a dockerSpawner, loading defaults from spire.yaml
// if available.
func newDockerSpawner() *dockerSpawner {
	s := &dockerSpawner{}

	cwd, _ := os.Getwd()
	rc, err := repoconfig.Load(cwd)
	if err == nil && rc != nil {
		if rc.Agent.Docker.Image != "" {
			s.Image = rc.Agent.Docker.Image
		}
		if rc.Agent.Docker.Network != "" {
			s.Network = rc.Agent.Docker.Network
		}
		s.ExtraVolumes = rc.Agent.Docker.ExtraVolumes
		s.ExtraEnv = rc.Agent.Docker.ExtraEnv
	}

	return s
}

func (s *dockerSpawner) resolvedImage() string {
	if s.Image != "" {
		return s.Image
	}
	return defaultDockerImage
}

func (s *dockerSpawner) resolvedNetwork() string {
	if s.Network != "" {
		return s.Network
	}
	return "host"
}

func (s *dockerSpawner) Spawn(cfg SpawnConfig) (AgentHandle, error) {
	// Map role to spire subcommand.
	var subcmd string
	switch cfg.Role {
	case RoleApprentice:
		subcmd = "wizard-run"
	case RoleSage:
		subcmd = "wizard-review"
	case RoleWizard:
		subcmd = "workshop"
	case RoleExecutor:
		subcmd = "execute"
	default:
		return nil, fmt.Errorf("unknown spawn role: %q", cfg.Role)
	}

	// Build the entrypoint command.
	entryCmd := []string{"spire", subcmd, cfg.BeadID, "--name", cfg.Name}
	entryCmd = append(entryCmd, cfg.ExtraArgs...)

	// Container name includes agent name for uniqueness across retries/rounds.
	containerName := fmt.Sprintf("spire-%s", sanitizeContainerName(cfg.Name))

	// Resolve paths for volume mounts.
	hostConfigDir, err := configDir()
	if err != nil {
		return nil, fmt.Errorf("resolve config dir: %w", err)
	}

	// Resolve the repo root (cwd) for mounting.
	repoRoot, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("resolve working dir: %w", err)
	}

	// Container-side config dir: a fixed path so configDir() resolves inside the container.
	containerConfigDir := "/spire/config"

	// Build docker run arguments.
	args := []string{
		"run", "-d",
		"--name", containerName,
		"--network", s.resolvedNetwork(),
		"-w", repoRoot,
		// Discovery labels for List() to find running containers.
		"--label", fmt.Sprintf("spire.agent=%s", cfg.Name),
		"--label", fmt.Sprintf("spire.bead=%s", cfg.BeadID),
		"--label", fmt.Sprintf("spire.role=%s", string(cfg.Role)),
		// Volume mounts: host config dir → container config dir, and repo root.
		"-v", fmt.Sprintf("%s:%s", hostConfigDir, containerConfigDir),
		"-v", fmt.Sprintf("%s:%s", repoRoot, repoRoot),
		// Set SPIRE_CONFIG_DIR so configDir() resolves inside the container.
		"-e", fmt.Sprintf("SPIRE_CONFIG_DIR=%s", containerConfigDir),
	}

	// Inherit key environment variables.
	for _, key := range []string{"BEADS_DIR", "SPIRE_TOWER", "ANTHROPIC_API_KEY", "GITHUB_TOKEN"} {
		if val := os.Getenv(key); val != "" {
			args = append(args, "-e", fmt.Sprintf("%s=%s", key, val))
		}
	}

	// Extra volumes from config.
	for _, v := range s.ExtraVolumes {
		args = append(args, "-v", v)
	}

	// Extra env from config.
	for _, e := range s.ExtraEnv {
		args = append(args, "-e", e)
	}

	// Image and command.
	args = append(args, s.resolvedImage())
	args = append(args, entryCmd...)

	// Run docker.
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("docker", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker run: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	containerID := strings.TrimSpace(stdout.String())
	if containerID == "" {
		return nil, fmt.Errorf("docker run returned empty container ID")
	}

	h := &dockerHandle{
		name:        cfg.Name,
		containerID: containerID,
	}

	// If LogPath is set, stream container logs to the file in the background.
	if cfg.LogPath != "" {
		os.MkdirAll(filepath.Dir(cfg.LogPath), 0755)
		go dockerStreamLogs(containerID, cfg.LogPath)
	}

	return h, nil
}

// dockerStreamLogs runs `docker logs -f <container>` and writes output to the
// given file path. Intended to be called as a goroutine; exits when the
// container stops or the logs command fails.
func dockerStreamLogs(containerID, logPath string) {
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return
	}
	defer logFile.Close()

	cmd := exec.Command("docker", "logs", "-f", containerID)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Run() // blocks until container exits or error
}

// Wait blocks until the container exits. Removes the container on return.
func (h *dockerHandle) Wait() error {
	defer func() {
		h.exited.Store(true)
		// Clean up the container.
		exec.Command("docker", "rm", "-f", h.containerID).Run()
	}()

	var stdout, stderr bytes.Buffer
	cmd := exec.Command("docker", "wait", h.containerID)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker wait: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	exitCode := strings.TrimSpace(stdout.String())
	if exitCode != "0" {
		return fmt.Errorf("container exited with code %s", exitCode)
	}
	return nil
}

// Signal sends a signal to the container.
// SIGTERM maps to `docker stop`, SIGKILL maps to `docker kill`.
func (h *dockerHandle) Signal(sig os.Signal) error {
	if h.exited.Load() {
		return fmt.Errorf("container already exited")
	}

	switch sig {
	case syscall.SIGTERM, os.Interrupt:
		out, err := exec.Command("docker", "stop", "--time=10", h.containerID).CombinedOutput()
		if err != nil {
			return fmt.Errorf("docker stop: %w: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	case syscall.SIGKILL:
		out, err := exec.Command("docker", "kill", h.containerID).CombinedOutput()
		if err != nil {
			return fmt.Errorf("docker kill: %w: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	default:
		// For other signals, try docker kill --signal.
		sigName := fmt.Sprintf("%v", sig)
		out, err := exec.Command("docker", "kill", "--signal", sigName, h.containerID).CombinedOutput()
		if err != nil {
			return fmt.Errorf("docker kill --signal %s: %w: %s", sigName, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
}

// Alive returns true if the container is running.
func (h *dockerHandle) Alive() bool {
	if h.exited.Load() {
		return false
	}
	out, err := exec.Command("docker", "inspect", "--format", "{{.State.Running}}", h.containerID).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

// Name returns the agent name.
func (h *dockerHandle) Name() string { return h.name }

// Identifier returns the Docker container ID.
func (h *dockerHandle) Identifier() string { return h.containerID }

// sanitizeContainerName replaces characters not allowed in Docker container
// names with hyphens. Docker allows [a-zA-Z0-9_.-].
func sanitizeContainerName(s string) string {
	var b strings.Builder
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '.' || c == '-' {
			b.WriteRune(c)
		} else {
			b.WriteRune('-')
		}
	}
	return b.String()
}
