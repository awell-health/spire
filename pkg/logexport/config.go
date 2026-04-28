package logexport

import (
	"fmt"
	"time"

	"github.com/awell-health/spire/pkg/logartifact"
)

// Env var names. These are read by the sidecar entrypoint and must match
// what the pod-builder injects. Defining them in one place keeps the
// pod-builder, sidecar binary, and in-process embed in lockstep — a
// rename in one site that misses another would silently produce empty
// fields in the manifest.
const (
	// EnvLogRoot is the in-pod path of the shared log directory. Mirrors
	// runctx.EnvLogRoot; re-declared here so pkg/logexport does not pull
	// pkg/runctx into the sidecar binary's import surface (the sidecar
	// has no agent-side dependencies).
	EnvLogRoot = "SPIRE_LOG_ROOT"

	// EnvLogStoreBackend selects the artifact backend: "local" or "gcs".
	// Empty defaults to "local" — local-native installs do not need to
	// set this explicitly. Mirrors PodSpec.LogStoreBackend.
	EnvLogStoreBackend = "LOGSTORE_BACKEND"

	// EnvLogStoreGCSBucket is the GCS bucket name for the gcs backend.
	// Required when backend=gcs. Mirrors PodSpec.LogStoreGCSBucket.
	EnvLogStoreGCSBucket = "LOGSTORE_GCS_BUCKET"

	// EnvLogStoreGCSPrefix is the optional object-name prefix inside
	// the bucket. Mirrors PodSpec.LogStoreGCSPrefix.
	EnvLogStoreGCSPrefix = "LOGSTORE_GCS_PREFIX"

	// EnvDrainDeadline is the maximum time the exporter waits during
	// flush before giving up on in-flight artifact finalize calls.
	// Pre-stop hook coordinates with this so terminationGracePeriodSeconds
	// is set high enough for the deadline to elapse cleanly.
	EnvDrainDeadline = "SPIRE_LOG_EXPORTER_DRAIN_DEADLINE"

	// EnvScanInterval is the polling interval the tailer uses when no
	// fsnotify event has fired. Empty defaults to ScanIntervalDefault.
	EnvScanInterval = "SPIRE_LOG_EXPORTER_SCAN_INTERVAL"

	// EnvIdleFinalize is the time-of-inactivity threshold that closes a
	// tailed file's artifact even if the file has not been removed.
	// Empty defaults to IdleFinalizeDefault.
	EnvIdleFinalize = "SPIRE_LOG_EXPORTER_IDLE_FINALIZE"
)

// Default backend values. Kept narrow so a typo in the env propagates as
// a typed error rather than a silent fall-through to local writes.
const (
	BackendLocal = "local"
	BackendGCS   = "gcs"
)

// Default tunables. The values match the operational guidance in the
// spi-k1cnof plan: 1s scan, 30s idle finalize, 25s drain (paired with a
// 30s pod terminationGracePeriodSeconds on the pod-builder side).
const (
	ScanIntervalDefault = 1 * time.Second
	IdleFinalizeDefault = 30 * time.Second
	DrainDeadlineDefault = 25 * time.Second
)

// Config is the resolved runtime configuration for a single Exporter
// instance. It is independent of os.Getenv so tests can construct it
// directly; the binary entrypoint loads from env via LoadConfigFromEnv.
type Config struct {
	// Root is the absolute path of the shared log directory the tailer
	// watches. Required.
	Root string

	// Backend is "local" or "gcs". Empty defaults to local.
	Backend string

	// GCSBucket is required when Backend=="gcs". Pod-builder emits this
	// from helm's logStore.gcs.bucket value.
	GCSBucket string

	// GCSPrefix is the optional object-name prefix inside the bucket.
	GCSPrefix string

	// ScanInterval is the tailer's polling cadence. Zero defaults to
	// ScanIntervalDefault.
	ScanInterval time.Duration

	// IdleFinalize is the inactivity-based finalize threshold for an
	// open artifact. Zero defaults to IdleFinalizeDefault.
	IdleFinalize time.Duration

	// DrainDeadline caps how long Flush waits to finalize all open
	// artifacts during shutdown. Zero defaults to DrainDeadlineDefault.
	DrainDeadline time.Duration

	// Visibility is the access class the exporter applies to every
	// artifact it uploads. Empty defaults to engineer_only — forensic
	// fidelity is the safest default for unattended cluster captures.
	// Per-stream redaction policy lives in the agent's transcript
	// writer, not here (see spi-cmy90h).
	Visibility logartifact.Visibility
}

// Validate rejects configurations that would surface as failures at
// first write. Called by the binary entrypoint and by the in-process
// embed at construction so misconfiguration produces a typed error
// instead of a half-running exporter.
func (c Config) Validate() error {
	if c.Root == "" {
		return fmt.Errorf("logexport: Config.Root is required")
	}
	switch c.effectiveBackend() {
	case BackendLocal:
		// no extra fields required
	case BackendGCS:
		if c.GCSBucket == "" {
			return fmt.Errorf("logexport: Config.GCSBucket is required when Backend=%q", BackendGCS)
		}
	default:
		return fmt.Errorf("logexport: Config.Backend %q is unknown (want %q or %q)",
			c.Backend, BackendLocal, BackendGCS)
	}
	return nil
}

// effectiveBackend returns Config.Backend with empty coerced to the
// safe default (local). Used by both Validate and the binary entrypoint.
func (c Config) effectiveBackend() string {
	if c.Backend == "" {
		return BackendLocal
	}
	return c.Backend
}

// EffectiveBackend returns the resolved backend identifier. Exported
// for the binary entrypoint and pod-builder tests.
func (c Config) EffectiveBackend() string { return c.effectiveBackend() }

// EffectiveScanInterval returns the resolved scan cadence.
func (c Config) EffectiveScanInterval() time.Duration {
	if c.ScanInterval <= 0 {
		return ScanIntervalDefault
	}
	return c.ScanInterval
}

// EffectiveIdleFinalize returns the resolved idle-finalize threshold.
func (c Config) EffectiveIdleFinalize() time.Duration {
	if c.IdleFinalize <= 0 {
		return IdleFinalizeDefault
	}
	return c.IdleFinalize
}

// EffectiveDrainDeadline returns the resolved drain deadline.
func (c Config) EffectiveDrainDeadline() time.Duration {
	if c.DrainDeadline <= 0 {
		return DrainDeadlineDefault
	}
	return c.DrainDeadline
}

// EffectiveVisibility returns the resolved visibility class. Empty or
// invalid values fail closed at engineer_only (the safe default).
func (c Config) EffectiveVisibility() logartifact.Visibility {
	if c.Visibility == "" || !c.Visibility.Valid() {
		return logartifact.VisibilityEngineerOnly
	}
	return c.Visibility
}
