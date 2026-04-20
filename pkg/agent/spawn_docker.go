package agent

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/repoconfig"
)

// DockerSpawner spawns agents as Docker containers.
type DockerSpawner struct {
	// Image overrides the default container image.
	// If empty, falls back to spire.yaml config, then to DefaultDockerImage.
	Image string

	// Network overrides the Docker network mode.
	// If empty, falls back to spire.yaml config, then "host".
	Network string

	// ExtraVolumes are additional -v mounts (host:container format).
	ExtraVolumes []string

	// ExtraEnv are additional -e KEY=VALUE environment entries.
	ExtraEnv []string
}

// DefaultDockerImage is the default container image for docker-based agent execution.
const DefaultDockerImage = "ghcr.io/awell-health/spire-agent:latest"

// DockerHandle tracks a running Docker container.
type DockerHandle struct {
	name        string
	containerID string
	exited      atomic.Bool
}

// NewDockerHandle creates a DockerHandle for testing purposes.
func NewDockerHandle(name, containerID string) *DockerHandle {
	return &DockerHandle{name: name, containerID: containerID}
}

// SetExited marks the handle as exited (for testing).
func (h *DockerHandle) SetExited() {
	h.exited.Store(true)
}

// newDockerSpawner creates a DockerSpawner, loading defaults from spire.yaml
// if available.
func newDockerSpawner() *DockerSpawner {
	s := &DockerSpawner{}

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

// ResolvedImage returns the effective Docker image, falling back to DefaultDockerImage.
func (s *DockerSpawner) ResolvedImage() string {
	if s.Image != "" {
		return s.Image
	}
	return DefaultDockerImage
}

// ResolvedNetwork returns the effective Docker network, falling back to "host".
func (s *DockerSpawner) ResolvedNetwork() string {
	if s.Network != "" {
		return s.Network
	}
	return "host"
}

func (s *DockerSpawner) Spawn(cfg SpawnConfig) (Handle, error) {
	// Map role to spire subcommand.
	subcmd, err := roleToSubcmd(cfg.Role)
	if err != nil {
		return nil, err
	}

	// Build the entrypoint command.
	entryCmd := []string{"spire", subcmd, cfg.BeadID, "--name", cfg.Name}
	if cfg.StartRef != "" {
		entryCmd = append(entryCmd, "--start-ref", cfg.StartRef)
	}

	// Write custom prompt to a temp file inside the repo root (which is
	// volume-mounted into the container) so the wizard subprocess can read it.
	if cfg.CustomPrompt != "" {
		repoRoot, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolve working dir for prompt file: %w", err)
		}
		f, err := os.CreateTemp(repoRoot, ".spire-prompt-*.txt")
		if err != nil {
			return nil, fmt.Errorf("write custom prompt temp file: %w", err)
		}
		if _, err := f.WriteString(cfg.CustomPrompt); err != nil {
			f.Close()
			os.Remove(f.Name())
			return nil, fmt.Errorf("write custom prompt: %w", err)
		}
		f.Close()
		entryCmd = append(entryCmd, "--custom-prompt-file", f.Name())
	}

	entryCmd = append(entryCmd, cfg.ExtraArgs...)

	// Container name includes agent name for uniqueness across retries/rounds.
	containerName := fmt.Sprintf("spire-%s", SanitizeContainerName(cfg.Name))

	// Resolve paths for volume mounts.
	hostConfigDir, err := config.Dir()
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
		"--network", s.ResolvedNetwork(),
		"-w", repoRoot,
		// Discovery labels for List() to find running containers.
		"--label", fmt.Sprintf("spire.agent=%s", cfg.Name),
		"--label", fmt.Sprintf("spire.bead=%s", cfg.BeadID),
		"--label", fmt.Sprintf("spire.role=%s", string(cfg.Role)),
		"--label", fmt.Sprintf("spire.tower=%s", cfg.Tower),
		// Volume mounts: host config dir -> container config dir, and repo root.
		"-v", fmt.Sprintf("%s:%s", hostConfigDir, containerConfigDir),
		"-v", fmt.Sprintf("%s:%s", repoRoot, repoRoot),
		// Set SPIRE_CONFIG_DIR so configDir() resolves inside the container.
		"-e", fmt.Sprintf("SPIRE_CONFIG_DIR=%s", containerConfigDir),
	}

	// Inherit key environment variables.
	for _, key := range []string{"BEADS_DIR", "ANTHROPIC_API_KEY", "GITHUB_TOKEN"} {
		if val := os.Getenv(key); val != "" {
			args = append(args, "-e", fmt.Sprintf("%s=%s", key, val))
		}
	}
	// SPIRE_TOWER: prefer explicit cfg.Tower, fall back to env.
	if tower := cfg.Tower; tower != "" {
		args = append(args, "-e", fmt.Sprintf("SPIRE_TOWER=%s", tower))
	} else if val := os.Getenv("SPIRE_TOWER"); val != "" {
		args = append(args, "-e", fmt.Sprintf("SPIRE_TOWER=%s", val))
	}

	// SPIRE_ROLE: surface the agent's role so the SubagentStart hook can
	// emit the correct per-role command catalog.
	args = append(args, appendRoleDockerArgs(cfg)...)

	// Apprentice identity env vars. Transport-agnostic: the apprentice reads
	// them to resolve which bead to write to and what role to claim at
	// submit time.
	args = append(args, appendIdentityDockerArgs(cfg)...)

	// Extra volumes from config.
	for _, v := range s.ExtraVolumes {
		args = append(args, "-v", v)
	}

	// Extra env from config.
	for _, e := range s.ExtraEnv {
		args = append(args, "-e", e)
	}

	// Image and command.
	args = append(args, s.ResolvedImage())
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

	h := &DockerHandle{
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
func (h *DockerHandle) Wait() error {
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
func (h *DockerHandle) Signal(sig os.Signal) error {
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
func (h *DockerHandle) Alive() bool {
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
func (h *DockerHandle) Name() string { return h.name }

// Identifier returns the Docker container ID.
func (h *DockerHandle) Identifier() string { return h.containerID }

// appendRoleDockerArgs returns the `-e SPIRE_ROLE=<role>` pair when the
// config carries a role. Returns nil for an empty role so unset spawns
// don't leak an empty env var into the container.
func appendRoleDockerArgs(cfg SpawnConfig) []string {
	if cfg.Role == "" {
		return nil
	}
	return []string{"-e", fmt.Sprintf("SPIRE_ROLE=%s", string(cfg.Role))}
}

// appendIdentityDockerArgs returns the `-e KEY=VALUE` args for the three
// apprentice identity env vars. Exported for backend-level tests that
// verify the config-to-env translation.
func appendIdentityDockerArgs(cfg SpawnConfig) []string {
	var out []string
	if cfg.BeadID != "" {
		out = append(out, "-e", fmt.Sprintf("SPIRE_BEAD_ID=%s", cfg.BeadID))
	}
	if cfg.AttemptID != "" {
		out = append(out, "-e", fmt.Sprintf("SPIRE_ATTEMPT_ID=%s", cfg.AttemptID))
	}
	if cfg.ApprenticeIdx != "" {
		out = append(out, "-e", fmt.Sprintf("SPIRE_APPRENTICE_IDX=%s", cfg.ApprenticeIdx))
	}
	return out
}

// SanitizeContainerName replaces characters not allowed in Docker container
// names with hyphens. Docker allows [a-zA-Z0-9_.-].
func SanitizeContainerName(s string) string {
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
