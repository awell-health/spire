package main

import (
	"flag"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	spirev1 "github.com/awell-health/spire/operator/api/v1alpha1"
	"github.com/awell-health/spire/operator/controllers"
	"github.com/awell-health/spire/pkg/steward/identity"
	"github.com/awell-health/spire/pkg/steward/intent"
	"github.com/awell-health/spire/pkg/store"
	"github.com/awell-health/spire/pkg/wizardregistry"
	wzcluster "github.com/awell-health/spire/pkg/wizardregistry/cluster"
)

// operatorIntentPollInterval is the cadence the operator polls the
// workload_intents outbox for un-reconciled rows. Matches
// intent.defaultConsumerPollInterval but named locally for visibility
// in the operator's wiring.
const operatorIntentPollInterval = 2 * time.Second

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(spirev1.AddToScheme(scheme))
}

func main() {
	var (
		namespace      string
		interval       time.Duration
		staleThreshold time.Duration
		reassignAfter  time.Duration
		offlineTimeout time.Duration
		stewardImage   string
		beadsDir       string
	)
	flag.StringVar(&namespace, "namespace", "spire", "Namespace to watch")
	flag.DurationVar(&interval, "interval", 2*time.Minute, "Poll interval")
	flag.DurationVar(&staleThreshold, "stale-threshold", 4*time.Hour, "Time before marking work as stale")
	flag.DurationVar(&reassignAfter, "reassign-after", 6*time.Hour, "Time before reassigning stale work")
	flag.DurationVar(&offlineTimeout, "offline-timeout", 30*time.Minute, "Time before marking agent as offline")
	flag.StringVar(&stewardImage, "steward-image", "ghcr.io/awell-health/spire-steward:latest", "Image for managed agent pods")
	flag.StringVar(&beadsDir, "beads-dir", "", "Path to .beads directory for scheduling validation (required)")

	// Runtime-contract identity inputs (docs/design/spi-xplwy-runtime-contract.md §1.1).
	//
	// Read ONCE at startup so pod-building code never reaches into process
	// env for tower/prefix/dolthub identity — the ypoqx rule extended to
	// the operator. Helm plumbs these from the chart values; for local
	// development set them explicitly via --database / --prefix /
	// --dolthub-remote or via the same-named env vars.
	var (
		database      string
		prefix        string
		dolthubRemote string
	)
	flag.StringVar(&database, "database", os.Getenv("BEADS_DATABASE"), "Dolt database name (tower identity). Defaults to $BEADS_DATABASE; falls back to --namespace when unset.")
	flag.StringVar(&prefix, "prefix", os.Getenv("BEADS_PREFIX"), "Default bead prefix. Defaults to $BEADS_PREFIX.")
	flag.StringVar(&dolthubRemote, "dolthub-remote", os.Getenv("DOLTHUB_REMOTE"), "DoltHub remote URL for tower-attach init containers. Defaults to $DOLTHUB_REMOTE.")

	// Authoritative in-cluster dolt sql server address (host:port, no
	// scheme). Stamped onto every PodSpec the IntentWorkloadReconciler
	// builds so tower-attach / in-pod workers hit the cluster dolt
	// service instead of falling back to the laptop default
	// 127.0.0.1:3307 (spi-o4f4eh). Helm injects DOLT_HOST + DOLT_PORT
	// into the operator deployment env; the default composes them.
	var doltURL string
	flag.StringVar(&doltURL, "dolt-url", "", "Cluster dolt sql server URL (host:port). Defaults to $DOLT_HOST:$DOLT_PORT.")

	// Guild cache reconciler inputs — deployment-time defaults plumbed
	// from the chart's `cache.*` values (see helm/spire/values.yaml and
	// the `spire.cachePVCSpec` helpers in _helpers.tpl). The per-guild
	// WizardGuild.Spec.CacheSpec fields override these.
	var (
		cacheGitImage          string
		cacheReconcileInterval time.Duration
		cacheStorageClass      string
		cacheDefaultSize       string
		cacheDefaultAccessMode string
	)
	flag.StringVar(&cacheGitImage, "cache-git-image", "alpine/git:latest", "Container image for guild cache refresh Jobs (must ship git + sh)")
	flag.DurationVar(&cacheReconcileInterval, "cache-reconcile-interval", 1*time.Minute, "How often the cache reconciler wakes up to check refresh cadence")
	flag.StringVar(&cacheStorageClass, "cache-storage-class", os.Getenv("SPIRE_CACHE_STORAGE_CLASS"), "Default StorageClass for guild-owned repo cache PVCs (chart fallback)")
	flag.StringVar(&cacheDefaultSize, "cache-default-size", firstNonEmpty(os.Getenv("SPIRE_CACHE_DEFAULT_SIZE"), "10Gi"), "Default size for guild-owned repo cache PVCs (chart fallback)")
	flag.StringVar(&cacheDefaultAccessMode, "cache-default-access-mode", firstNonEmpty(os.Getenv("SPIRE_CACHE_DEFAULT_ACCESS_MODE"), "ReadOnlyMany"), "Default access mode for guild-owned repo cache PVCs (chart fallback)")

	// OperatorEnableLegacyScheduler gate (spi-njzmg). Canonical
	// cluster-native operator is a pure reconciler of WorkloadIntent;
	// the legacy BeadWatcher + WorkloadAssigner loops are gated off by
	// default. Set true only as a transitional co-existence path
	// during the spi-sj18k migration. The CR's
	// SpireConfigSpec.EnableLegacyScheduler (resolved at reconciler
	// wiring time, if ever) takes precedence over this flag when set.
	var enableLegacyScheduler bool
	flag.BoolVar(&enableLegacyScheduler, "enable-legacy-scheduler", false, "Start the legacy BeadWatcher/WorkloadAssigner control loops alongside the intent reconciler. Default false (canonical cluster-native).")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("operator")

	if beadsDir == "" {
		log.Error(nil, "--beads-dir is required for scheduling validation")
		os.Exit(1)
	}
	store.BeadsDirResolver = func() string { return beadsDir }
	if _, err := store.Ensure(beadsDir); err != nil {
		log.Error(err, "failed to open beads store at startup", "beadsDir", beadsDir)
		os.Exit(1)
	}
	defer store.Reset()

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
	})
	if err != nil {
		log.Error(err, "unable to create manager")
		os.Exit(1)
	}

	// OperatorEnableLegacyScheduler gate (spi-njzmg). BeadWatcher and
	// WorkloadAssigner are transitional loops retained only for the
	// spi-sj18k migration window. Canonical cluster-native leaves them
	// off — the intent reconciler below is the authoritative dispatch
	// path.
	if enableLegacyScheduler {
		assigner := &controllers.WorkloadAssigner{
			Client:            mgr.GetClient(),
			Log:               log.WithName("workload-assigner"),
			Namespace:         namespace,
			Interval:          interval,
			StaleThreshold:    staleThreshold,
			ReassignThreshold: reassignAfter,
		}
		if err := mgr.Add(assigner); err != nil {
			log.Error(err, "unable to add workload assigner")
			os.Exit(1)
		}
		log.Info("legacy scheduler gate ON — starting WorkloadAssigner (transitional)")
	} else {
		log.Info("legacy scheduler gate OFF — WorkloadAssigner not started; canonical reconciler path only")
	}

	// Database identity defaults: if --database is unset, fall back to
	// --namespace to match the helm convention where the chart's
	// release-scoped database name equals the install namespace.
	if database == "" {
		database = namespace
	}

	// Dolt URL resolution (spi-o4f4eh). Prefer the explicit flag; else
	// compose from the helm-injected DOLT_HOST + DOLT_PORT env; else
	// fall back to the chart's own default service DNS. When both env
	// vars are unset in non-cluster contexts the reconciler stamps an
	// empty value onto PodSpec and pkg/agent.buildEnv omits the dolt
	// vars — logged here so it is not silent.
	if doltURL == "" {
		if host, port := os.Getenv("DOLT_HOST"), os.Getenv("DOLT_PORT"); host != "" && port != "" {
			doltURL = host + ":" + port
		} else {
			doltURL = "spire-dolt." + namespace + ".svc:3306"
			log.Info("--dolt-url not set and DOLT_HOST/DOLT_PORT unset; falling back to chart default",
				"dolt_url", doltURL)
		}
	}

	// Agent monitor — tracks heartbeats and manages pods.
	// Resolver is the canonical source of cluster repo identity per
	// spi-njzmg. When wired, WizardGuild Repo/RepoBranch are treated
	// as projection-only and reconciled to the resolver's output.
	resolver := newClusterIdentityResolver()

	// Shared GCP SA identity — helm populates SPIRE_GCP_* when the
	// chart deploys with bundleStore.backend=gcs. Read once at startup
	// so pod-build code never hunts the process env per pod. Empty
	// Secret name means no GCS overlay; local-backend pods keep today's
	// shape.
	gcpSecretName := os.Getenv("SPIRE_GCP_SECRET_NAME")
	gcpMountPath := os.Getenv("SPIRE_GCP_MOUNT_PATH")
	gcpKeyName := os.Getenv("SPIRE_GCP_KEY_NAME")

	// Cluster analytics backend. Helm sets these on the operator pod
	// when clickhouse.enabled=true so wizard/apprentice pods reach the
	// in-cluster ClickHouse service instead of trying DuckDB (which
	// fails at runtime because the agent image builds with CGO=0). Both
	// empty = laptop default (DuckDB); we don't second-guess the chart.
	olapBackend := os.Getenv("SPIRE_OLAP_BACKEND")
	olapDSN := os.Getenv("SPIRE_CLICKHOUSE_DSN")

	// Log artifact substrate. Helm sets these on the operator pod from
	// .Values.logStore.* (spi-hzeyz9). The operator stamps them onto
	// every wizard/apprentice/sage pod it builds so the in-pod log
	// substrate (pkg/logartifact) — and the future passive exporter
	// sidecar from spi-k1cnof — picks the right backend without each
	// pod-build site having to read process env. Empty backend keeps
	// pods on the in-binary default; "local" pins the local filesystem
	// backend; "gcs" routes writes through the cloud-native substrate.
	logStoreBackend := os.Getenv("LOGSTORE_BACKEND")
	logStoreGCSBucket := os.Getenv("LOGSTORE_GCS_BUCKET")
	logStoreGCSPrefix := os.Getenv("LOGSTORE_GCS_PREFIX")
	logStoreRetentionDays := os.Getenv("LOGSTORE_RETENTION_DAYS")

	// Wizard-liveness boundary (spi-p6unf3). The operator owns the
	// canonical cluster Registry: a wizardregistry/cluster instance
	// backed by live k8s pod-phase reads in the operator's namespace.
	// AgentMonitor consumes Registry.IsAlive for liveness queries; pod
	// reaping, stale-pod deletion, and phase updates remain k8s-native
	// because they need the live pod object to delete/patch it. The
	// Registry impl is read-only from clients (Upsert/Remove return
	// ErrReadOnly); the operator reconciliation loop is the sole writer
	// of cluster pod state.
	var clusterRegistry wizardregistry.Registry
	if k8sCfg, kerr := ctrl.GetConfig(); kerr == nil {
		if cs, cserr := kubernetes.NewForConfig(k8sCfg); cserr == nil {
			clusterRegistry = wzcluster.New(cs, wzcluster.Options{Namespace: namespace})
		} else {
			log.Error(cserr, "wizardregistry/cluster: build clientset failed; AgentMonitor.Registry will be nil")
		}
	} else {
		log.Error(kerr, "wizardregistry/cluster: get rest.Config failed; AgentMonitor.Registry will be nil")
	}

	monitor := &controllers.AgentMonitor{
		Client:                mgr.GetClient(),
		Log:                   log.WithName("agent-monitor"),
		Namespace:             namespace,
		Interval:              interval,
		OfflineTimeout:        offlineTimeout,
		StewardImage:          stewardImage,
		Database:              database,
		Prefix:                prefix,
		DolthubRemote:         dolthubRemote,
		Resolver:              resolver,
		Registry:              clusterRegistry,
		GCSSecretName:         gcpSecretName,
		GCSMountPath:          gcpMountPath,
		GCSKeyName:            gcpKeyName,
		OLAPBackend:           olapBackend,
		OLAPDSN:               olapDSN,
		LogStoreBackend:       logStoreBackend,
		LogStoreGCSBucket:     logStoreGCSBucket,
		LogStoreGCSPrefix:     logStoreGCSPrefix,
		LogStoreRetentionDays: logStoreRetentionDays,
	}
	if err := mgr.Add(monitor); err != nil {
		log.Error(err, "unable to add agent monitor")
		os.Exit(1)
	}

	// Cache reconciler — materializes per-WizardGuild repo cache PVCs
	// and schedules refresh Jobs (spi-myzn5). Inactive for guilds that
	// leave Spec.Cache unset.
	cacheReconciler := &controllers.CacheReconciler{
		Client:                 mgr.GetClient(),
		Log:                    log.WithName("cache-reconciler"),
		Namespace:              namespace,
		Interval:               cacheReconcileInterval,
		GitImage:               cacheGitImage,
		ChartCacheStorageClass: cacheStorageClass,
		ChartCacheSize:         cacheDefaultSize,
		ChartCacheAccessMode:   corev1.PersistentVolumeAccessMode(cacheDefaultAccessMode),
		Database:               database,
		Prefix:                 prefix,
	}
	if err := mgr.Add(cacheReconciler); err != nil {
		log.Error(err, "unable to add cache reconciler")
		os.Exit(1)
	}

	// Intent reconciler (spi-njzmg) — canonical cluster-native path.
	// Consumes pkg/steward/intent.WorkloadIntent values and reconciles
	// apprentice pods via pkg/agent.BuildApprenticePod. Resolver is
	// the authoritative source of cluster repo identity; CR-only
	// fields are never trusted for scheduling decisions.
	//
	// The IntentConsumer is a DoltConsumer reading from the shared
	// workload_intents outbox the steward publishes into (spi-t8a57s).
	// EnsureWorkloadIntentsTable is defense-in-depth — whichever pod
	// comes up first creates the table; the steward's factory does the
	// same on its side.
	var intentConsumer intent.IntentConsumer
	if db, ok := store.ActiveDB(); ok && db != nil {
		if err := intent.EnsureWorkloadIntentsTable(db); err != nil {
			log.Error(err, "failed to ensure workload_intents table; intent reconciler will self-disable")
		} else {
			intentConsumer = intent.NewDoltConsumer(db, operatorIntentPollInterval)
		}
	} else {
		log.Info("no active dolt DB; intent reconciler will self-disable (no IntentConsumer wired)")
	}

	// Credentials Secret the reconciler stamps onto every apprentice
	// PodSpec. Helm plumbs SPIRE_CREDENTIALS_SECRET from
	// spire.secretName (see helm/spire/templates/operator.yaml). Empty
	// defers to pkg/agent.DefaultCredentialsSecret — correct for
	// non-Helm/test contexts but yields a hardcoded "spire-credentials"
	// lookup that will fail against release-prefixed secrets.
	credentialsSecret := os.Getenv("SPIRE_CREDENTIALS_SECRET")
	if credentialsSecret == "" {
		log.Info("SPIRE_CREDENTIALS_SECRET not set; apprentice pods will fall back to pkg/agent.DefaultCredentialsSecret")
	}

	intentReconciler := &controllers.IntentWorkloadReconciler{
		Client:                mgr.GetClient(),
		Log:                   log.WithName("intent-reconciler"),
		Namespace:             namespace,
		Image:                 stewardImage,
		Tower:                 database,
		DolthubRemote:         dolthubRemote,
		DoltURL:               doltURL,
		Resolver:              resolver,
		Consumer:              intentConsumer,
		CredentialsSecret:     credentialsSecret,
		GCSSecretName:         gcpSecretName,
		GCSMountPath:          gcpMountPath,
		GCSKeyName:            gcpKeyName,
		LogStoreBackend:       logStoreBackend,
		LogStoreGCSBucket:     logStoreGCSBucket,
		LogStoreGCSPrefix:     logStoreGCSPrefix,
		LogStoreRetentionDays: logStoreRetentionDays,
		OLAPBackend:           olapBackend,
		OLAPDSN:               olapDSN,
	}
	if err := mgr.Add(intentReconciler); err != nil {
		log.Error(err, "unable to add intent reconciler")
		os.Exit(1)
	}

	log.Info("starting operator",
		"namespace", namespace,
		"interval", interval,
		"staleThreshold", staleThreshold,
	)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "operator exited with error")
		os.Exit(1)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// newClusterIdentityResolver returns a DefaultClusterIdentityResolver
// backed by the shared tower repo registry (the `repos` table in dolt).
// Returns nil when the store is not ready — callers log the condition
// and run without drift checking. The operator always has a ready
// store at Start (store.Ensure runs unconditionally in main), but the
// bind returns ok=false for test mocks that do not expose *sql.DB.
//
// This is the operator's single canonical source of cluster repo
// identity per spi-njzmg. Scheduling decisions NEVER read
// URL/BaseBranch/Prefix directly off CRs — the resolver's output wins.
func newClusterIdentityResolver() identity.ClusterIdentityResolver {
	db, ok := store.ActiveDB()
	if !ok || db == nil {
		return nil
	}
	return &identity.DefaultClusterIdentityResolver{
		Registry: identity.NewSQLRegistryStore(db),
	}
}
