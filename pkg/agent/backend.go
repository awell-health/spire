package agent

import (
	"log"
	"os"

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
//   - unknown         -> log warning, fall back to process
//
// ResolveBackend replaces NewSpawner as the preferred factory.
// ---------------------------------------------------------------------------

func ResolveBackend(name string) Backend {
	if name == "" {
		// Auto-resolve: read from spire.yaml, then fall back to detection.
		cwd, _ := os.Getwd()
		if cfg, err := repoconfig.Load(cwd); err == nil && cfg.Agent.Backend != "" {
			name = cfg.Agent.Backend
		}
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
