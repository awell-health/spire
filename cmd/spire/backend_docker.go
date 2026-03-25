package main

import (
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// dockerBackend implements AgentBackend for Docker container execution.
// It wraps the existing dockerSpawner and adds observation/cleanup capabilities
// using docker CLI commands and container labels for discovery.
type dockerBackend struct {
	spawner *dockerSpawner
}

func newDockerBackend() *dockerBackend {
	return &dockerBackend{spawner: newDockerSpawner()}
}

// Spawn delegates to the underlying dockerSpawner.
func (b *dockerBackend) Spawn(cfg SpawnConfig) (AgentHandle, error) {
	return b.spawner.Spawn(cfg)
}

// List discovers all Spire-managed containers via label filtering and returns
// an AgentInfo for each one.
func (b *dockerBackend) List() ([]AgentInfo, error) {
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
	var infos []AgentInfo
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
func (b *dockerBackend) inspectContainer(id string) (AgentInfo, error) {
	// Inspect format: agent-name \t bead-id \t role \t running \t created
	format := `{{index .Config.Labels "spire.agent"}}` +
		"\t" + `{{index .Config.Labels "spire.bead"}}` +
		"\t" + `{{index .Config.Labels "spire.role"}}` +
		"\t" + `{{.State.Running}}` +
		"\t" + `{{.Created}}`

	out, err := exec.Command("docker", "inspect", "--format", format, id).Output()
	if err != nil {
		return AgentInfo{}, fmt.Errorf("docker inspect %s: %w", id, err)
	}

	return parseDockerInspect(id, strings.TrimSpace(string(out)))
}

// parseDockerInspect parses a single line of tab-separated docker inspect output
// into an AgentInfo. Exported logic is tested in backend_docker_test.go.
func parseDockerInspect(id, line string) (AgentInfo, error) {
	parts := strings.SplitN(line, "\t", 5)
	if len(parts) < 5 {
		return AgentInfo{}, fmt.Errorf("unexpected inspect output: %q", line)
	}

	alive := parts[3] == "true"

	// Docker Created timestamps use RFC 3339 with nanoseconds.
	startedAt, _ := time.Parse(time.RFC3339Nano, parts[4])

	return AgentInfo{
		Name:       parts[0],
		BeadID:     parts[1],
		Phase:      "", // phase is bead-level, not container-level
		Alive:      alive,
		Identifier: id,
		StartedAt:  startedAt,
	}, nil
}

// Logs returns a reader over the combined stdout/stderr of the named agent's
// container. The container is located by its spire.agent label.
func (b *dockerBackend) Logs(name string) (io.ReadCloser, error) {
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
func (b *dockerBackend) Kill(name string) error {
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
func (b *dockerBackend) findContainer(name string) (string, error) {
	out, err := exec.Command(
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
