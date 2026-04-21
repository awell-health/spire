package agent

import (
	"log"
	"os"
	"path/filepath"

	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/awell-health/spire/pkg/runtime"
)

// ---------------------------------------------------------------------------
// Compile-time interface checks
// ---------------------------------------------------------------------------

var _ Backend = (*ProcessBackend)(nil)
var _ Backend = (*DockerBackend)(nil)
var _ Backend = (*K8sBackend)(nil)
var _ Handle = (*K8sHandle)(nil)

// ---------------------------------------------------------------------------
// ResolveBackend returns a Backend for the given backend name.
//
//   - "process" or "" -> processBackend  (local OS processes)
//   - "docker"        -> dockerBackend   (Docker containers)
//   - "k8s"           -> k8sBackend      (Kubernetes pods)
//   - unknown         -> log warning, fall back to process
//
// Use ResolveBackendForRepo instead when the caller knows the repo root.
// Passing an explicit path avoids a silent fallback when the process's
// current working directory does not contain spire.yaml (e.g. the
// operator's wizard pod used to run with WorkingDir=/workspace, one
// directory above the cloned repo at /workspace/<prefix>). See spi-vrzhf.
//
// ResolveBackend replaces NewSpawner as the preferred factory.
// ---------------------------------------------------------------------------

func ResolveBackend(name string) Backend {
	cwd, _ := os.Getwd()
	return ResolveBackendForRepo(name, cwd)
}

// ResolveBackendForRepo is the explicit-path variant of ResolveBackend.
// It reads spire.yaml from repoDir (walking up the directory tree) when
// name is empty. Pass the resolved repo checkout path — not the current
// working directory — whenever the caller has a bead or registered-repo
// context, so backend resolution survives callers whose CWD is not the
// repo root.
//
// When repoDir is empty the function falls back to os.Getwd(), matching
// the legacy ResolveBackend behavior. In that path an additional runtime
// assertion fires if SPIRE_REPO_PREFIX is set but no spire.yaml is
// reachable from cwd — the exact operator-managed-pod symptom from
// spi-vrzhf. The assertion is a loud log line, not a fatal error,
// because the backend machinery must still return a usable Backend.
func ResolveBackendForRepo(name, repoDir string) Backend {
	if name == "" {
		name = resolveBackendNameFromConfig(repoDir)
	}
	switch name {
	case "process", "":
		return newProcessBackend()
	case "docker":
		return newDockerBackend()
	case "k8s", "kubernetes":
		b, err := NewK8sBackend()
		if err != nil {
			log.Printf("[backend] k8s backend init failed: %v, falling back to process%s", err, runtime.LogFields(runtime.RunContextFromEnv()))
			return newProcessBackend()
		}
		return b
	default:
		log.Printf("[backend] unknown backend %q, falling back to process%s", name, runtime.LogFields(runtime.RunContextFromEnv()))
		return newProcessBackend()
	}
}

// resolveBackendNameFromConfig reads agent.backend from spire.yaml,
// walking up from repoDir. Returns "" when no config is found or the
// field is unset; callers fall through to process.
//
// When repoDir is empty we fall back to cwd and fire the runtime
// assertion described on ResolveBackendForRepo.
func resolveBackendNameFromConfig(repoDir string) string {
	dir := repoDir
	explicit := dir != ""
	if dir == "" {
		dir, _ = os.Getwd()
	}
	cfg, err := repoconfig.Load(dir)
	if err != nil || cfg == nil {
		if !explicit {
			assertCWDRootMatchesPrefix(dir)
		}
		return ""
	}
	if !explicit && cfg.Agent.Backend == "" {
		// Reached here via cwd fallback and no backend was configured.
		// If we're in a bootstrapped pod whose working dir is above the
		// clone, the repoconfig.Load call above returned a default
		// (zero-value) config — warn so the operator notices.
		assertCWDRootMatchesPrefix(dir)
	}
	return cfg.Agent.Backend
}

// assertCWDRootMatchesPrefix is a best-effort runtime check for the
// operator-managed-pod regression fixed in spi-vrzhf. When
// SPIRE_REPO_PREFIX is set (the repo-bootstrap init container ran), the
// cloned repo lives at /workspace/<prefix> and the wizard subprocess
// must run inside that directory. If it does not, ResolveBackend("")
// silently falls back to the process backend because spire.yaml is not
// reachable from cwd.
//
// The check is informational — it logs but never panics — because the
// surrounding ResolveBackend contract must always return a Backend.
// Tests and local execution, where SPIRE_REPO_PREFIX is unset, skip the
// check entirely.
func assertCWDRootMatchesPrefix(cwd string) {
	prefix := os.Getenv("SPIRE_REPO_PREFIX")
	if prefix == "" {
		return
	}
	// spire.yaml lives at the repo root. If we can't find one walking up
	// from cwd, the process started outside the clone. Check explicitly
	// against the canonical clone path so the message names the directory
	// the caller should have been in.
	expected := filepath.Join("/workspace", prefix)
	if _, err := os.Stat(filepath.Join(cwd, "spire.yaml")); err == nil {
		return
	}
	if _, err := os.Stat(filepath.Join(expected, "spire.yaml")); err == nil {
		log.Printf("[backend] WARN ResolveBackend called with cwd=%q but SPIRE_REPO_PREFIX=%q resolves to %q; spire.yaml not reachable from cwd. The caller should pass repoDir=%q to ResolveBackendForRepo. Falling back to process backend.%s",
			cwd, prefix, expected, expected, runtime.LogFields(runtime.RunContextFromEnv()))
	}
}
