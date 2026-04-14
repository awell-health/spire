package agent

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// DockerBackend implements Backend for Docker container execution.
// It wraps the existing DockerSpawner and adds observation/cleanup capabilities
// using docker CLI commands and container labels for discovery.
type DockerBackend struct {
	spawner *DockerSpawner
}

// NewDockerBackend creates a new docker backend.
func NewDockerBackend() *DockerBackend {
	return &DockerBackend{spawner: newDockerSpawner()}
}

func newDockerBackend() *DockerBackend {
	return NewDockerBackend()
}

// Spawn delegates to the underlying DockerSpawner.
func (b *DockerBackend) Spawn(cfg SpawnConfig) (Handle, error) {
	return b.spawner.Spawn(cfg)
}

// List discovers all Spire-managed containers via label filtering and returns
// an Info for each one.
func (b *DockerBackend) List() ([]Info, error) {
	out, err := exec.Command(
		"docker", "ps", "-a",
		"--filter", "label=spire.agent",
		"--format", "{{.ID}}",
	).Output()
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}

	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return nil, nil
	}

	ids := strings.Split(raw, "\n")
	var infos []Info
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		info, err := b.inspectContainer(id)
		if err != nil {
			continue // container may have been removed between ps and inspect
		}
		infos = append(infos, info)
	}
	return infos, nil
}

// inspectContainer reads labels and state from a single container.
func (b *DockerBackend) inspectContainer(id string) (Info, error) {
	// Inspect format: agent-name \t bead-id \t role \t running \t created \t tower
	format := `{{index .Config.Labels "spire.agent"}}` +
		"\t" + `{{index .Config.Labels "spire.bead"}}` +
		"\t" + `{{index .Config.Labels "spire.role"}}` +
		"\t" + `{{.State.Running}}` +
		"\t" + `{{.Created}}` +
		"\t" + `{{index .Config.Labels "spire.tower"}}`

	out, err := exec.Command("docker", "inspect", "--format", format, id).Output()
	if err != nil {
		return Info{}, fmt.Errorf("docker inspect %s: %w", id, err)
	}

	return ParseDockerInspect(id, strings.TrimSpace(string(out)))
}

// ParseDockerInspect parses a single line of tab-separated docker inspect output
// into an Info. Exported for testing.
func ParseDockerInspect(id, line string) (Info, error) {
	parts := strings.SplitN(line, "\t", 6)
	if len(parts) < 5 {
		return Info{}, fmt.Errorf("unexpected inspect output: %q", line)
	}

	alive := parts[3] == "true"

	// Docker Created timestamps use RFC 3339 with nanoseconds.
	startedAt, _ := time.Parse(time.RFC3339Nano, parts[4])

	var tower string
	if len(parts) >= 6 {
		tower = parts[5]
	}

	return Info{
		Name:       parts[0],
		BeadID:     parts[1],
		Phase:      "", // phase is bead-level, not container-level
		Alive:      alive,
		Identifier: id,
		StartedAt:  startedAt,
		Tower:      tower,
	}, nil
}

// Logs returns a reader over the combined stdout/stderr of the named agent's
// container. The container is located by its spire.agent label.
func (b *DockerBackend) Logs(name string) (io.ReadCloser, error) {
	id, err := b.findContainer(name)
	if err != nil {
		return nil, err
	}

	// Use shell to merge stdout and stderr (docker logs writes to both).
	cmd := exec.Command("docker", "logs", id)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("docker logs pipe: %w", err)
	}
	cmd.Stderr = cmd.Stdout // merge stderr into stdout pipe

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("docker logs start: %w", err)
	}

	// Return a wrapper that waits for the command to finish when closed.
	return &cmdReadCloser{ReadCloser: stdout, cmd: cmd}, nil
}

// cmdReadCloser wraps a pipe reader and waits for the command to exit on Close.
type cmdReadCloser struct {
	io.ReadCloser
	cmd *exec.Cmd
}

func (c *cmdReadCloser) Close() error {
	_ = c.ReadCloser.Close()
	return c.cmd.Wait()
}

// Kill stops and removes the named agent's container.
func (b *DockerBackend) Kill(name string) error {
	id, err := b.findContainer(name)
	if err != nil {
		return err
	}

	// Stop with a 10-second grace period, then force remove.
	if out, err := exec.Command("docker", "stop", "--time=10", id).CombinedOutput(); err != nil {
		return fmt.Errorf("docker stop: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("docker", "rm", "-f", id).CombinedOutput(); err != nil {
		return fmt.Errorf("docker rm: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// findContainer locates a container by the spire.agent=<name> label.
// Returns the container ID or an error if not found.
func (b *DockerBackend) findContainer(name string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx,
		"docker", "ps", "-a",
		"--filter", fmt.Sprintf("label=spire.agent=%s", name),
		"--format", "{{.ID}}",
	).Output()
	if err != nil {
		return "", fmt.Errorf("docker ps: %w", err)
	}

	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", fmt.Errorf("no container found for agent %q", name)
	}

	// If multiple containers match, take the first (most recent).
	if idx := strings.Index(id, "\n"); idx >= 0 {
		id = id[:idx]
	}
	return id, nil
}
