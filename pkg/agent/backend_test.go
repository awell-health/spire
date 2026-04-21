package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveBackendForRepo_ReadsExplicitPath is the spi-vrzhf regression
// test at the pkg/agent boundary. It simulates the operator-managed pod
// scenario where the process's CWD is above the clone (WorkingDir=/workspace)
// but the spire.yaml lives inside the clone (/workspace/<prefix>).
// ResolveBackendForRepo called with the clone path must still resolve
// agent.backend from the repo's spire.yaml, even when os.Getwd() does
// not contain one.
func TestResolveBackendForRepo_ReadsExplicitPath(t *testing.T) {
	// Build a fake "/workspace/<prefix>" layout under a temp root.
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	repoDir := filepath.Join(workspace, "spi")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	yaml := `agent:
  backend: docker
`
	if err := os.WriteFile(filepath.Join(repoDir, "spire.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write spire.yaml: %v", err)
	}

	// Deliberately chdir to the parent so the legacy cwd-based lookup
	// would miss the spire.yaml (matches the operator-pod bug).
	origCwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(origCwd) })
	if err := os.Chdir(workspace); err != nil {
		t.Fatalf("chdir workspace: %v", err)
	}

	// Cwd-based resolution should fall back to process (no spire.yaml
	// reachable from workspace).
	cwdBackend := ResolveBackend("")
	if _, ok := cwdBackend.(*ProcessBackend); !ok {
		t.Fatalf("ResolveBackend(\"\") from cwd=/workspace returned %T; precondition for the bug expects ProcessBackend fallback", cwdBackend)
	}

	// Explicit-path resolution should read the repo's spire.yaml and
	// return DockerBackend.
	explicitBackend := ResolveBackendForRepo("", repoDir)
	if _, ok := explicitBackend.(*DockerBackend); !ok {
		t.Fatalf("ResolveBackendForRepo(\"\", %q) returned %T, want *DockerBackend (agent.backend=docker in spire.yaml)", repoDir, explicitBackend)
	}
}

// TestResolveBackendForRepo_ExplicitOverridesConfig verifies that an
// explicit backend name wins over agent.backend in spire.yaml even when
// the repoDir is passed.
func TestResolveBackendForRepo_ExplicitOverridesConfig(t *testing.T) {
	dir := t.TempDir()
	yaml := `agent:
  backend: docker
`
	if err := os.WriteFile(filepath.Join(dir, "spire.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write spire.yaml: %v", err)
	}
	b := ResolveBackendForRepo("process", dir)
	if _, ok := b.(*ProcessBackend); !ok {
		t.Fatalf("ResolveBackendForRepo(\"process\", dir) returned %T, want *ProcessBackend", b)
	}
}

// TestResolveBackendForRepo_EmptyRepoDirFallsBackToCwd verifies that
// passing repoDir="" restores the legacy cwd-based behavior — callers
// with no bead context still work.
func TestResolveBackendForRepo_EmptyRepoDirFallsBackToCwd(t *testing.T) {
	dir := t.TempDir()
	yaml := `agent:
  backend: docker
`
	if err := os.WriteFile(filepath.Join(dir, "spire.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write spire.yaml: %v", err)
	}

	origCwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(origCwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	b := ResolveBackendForRepo("", "")
	if _, ok := b.(*DockerBackend); !ok {
		t.Fatalf("ResolveBackendForRepo(\"\", \"\") with cwd=spire.yaml returned %T, want *DockerBackend", b)
	}
}
