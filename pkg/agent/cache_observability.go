package agent

// Observability vocabulary for the phase-2 cluster repo-cache contract
// (spi-sn7o3). These labels and metric names describe cache/bootstrap
// behavior only — identity labels (tower, prefix, bead, role, workspace
// kind/name/origin, formula step, backend) are already canonicalized in
// backend_k8s.go from the runtime-contract track (spi-xplwy) and MUST
// be reused rather than redeclared here.
const (
	// LabelBootstrapSource identifies how a wizard pod's workspace
	// was seeded. The canonical value for phase-2 cluster wizards is
	// BootstrapSourceGuildCache; earlier per-pod origin clones and
	// local process spawns use their own values in their own code
	// paths and are not this package's concern.
	LabelBootstrapSource = "spire.io/bootstrap-source"

	// LabelCacheRevision is the git revision (or cache generation
	// token) the guild cache exposed at the moment a pod bound it.
	// High enough cardinality that it should be applied as an
	// annotation on pods; it is included here as the canonical key
	// name so metric/log emitters agree on spelling.
	LabelCacheRevision = "spire.io/cache-revision"

	// LabelStartupPhase tags emissions from the phases of the
	// cache-to-workspace bootstrap sequence. Accepted values are the
	// StartupPhase* constants below.
	LabelStartupPhase = "spire.io/startup-phase"

	// LabelCacheFreshness classifies the cache state observed at
	// bind time (e.g. "fresh", "stale", "refreshing"). Concrete
	// string values are owned by the cache reconciler (spi-myzn5)
	// and the bootstrap helper implementation (spi-jetfb).
	LabelCacheFreshness = "spire.io/cache-freshness"
)

// Canonical value for LabelBootstrapSource when a wizard pod was
// materialized from a WizardGuild-managed repo cache.
const BootstrapSourceGuildCache = "guild-cache"

// Accepted values for LabelStartupPhase. Phases are ordered:
// cache-ready (the pod has observed the cache and confirmed it is
// usable) → workspace-derive (MaterializeWorkspaceFromCache is
// running) → local-bind-bootstrap (BindLocalRepo is running).
const (
	StartupPhaseCacheReady         = "cache-ready"
	StartupPhaseWorkspaceDerive    = "workspace-derive"
	StartupPhaseLocalBindBootstrap = "local-bind-bootstrap"
)

// Metric names emitted by the cache + bootstrap code paths. Names
// follow the existing spire_* Prometheus convention (see
// pkg/steward/metrics_server.go). Cardinality of labels attached to
// these metrics MUST stay within the low-cardinality canonical
// identity set (tower, prefix, role, workspace kind/origin, backend)
// plus the cache labels declared above; bead_id / attempt_id / run_id
// are log/trace fields, never metric labels (spi-xplwy §1.4).
const (
	// MetricCacheRefreshDuration: histogram of how long a guild
	// cache refresh cycle took (operator-side).
	MetricCacheRefreshDuration = "spire_cache_refresh_duration_seconds"

	// MetricCacheStaleness: gauge of seconds since the guild cache
	// was last refreshed, as observed at pod bind time.
	MetricCacheStaleness = "spire_cache_staleness_seconds"

	// MetricBootstrapDuration: histogram of how long the
	// cache→workspace bootstrap (both MaterializeWorkspaceFromCache
	// and BindLocalRepo) took inside the init container.
	MetricBootstrapDuration = "spire_bootstrap_duration_seconds"

	// MetricBootstrapSuccess: counter of bootstrap attempts
	// partitioned by success/failure (via a low-cardinality result
	// label owned by the bootstrap helper implementation).
	MetricBootstrapSuccess = "spire_bootstrap_success_total"
)
