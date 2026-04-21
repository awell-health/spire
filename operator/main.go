package main

import (
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	spirev1 "github.com/awell-health/spire/operator/api/v1alpha1"
	"github.com/awell-health/spire/operator/controllers"
	"github.com/awell-health/spire/pkg/store"
)

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

	// Workload assigner — matches pending workloads to agents.
	// Validates pending workloads against the shared scheduling policy
	// (store.GetSchedulableWork) before assigning.
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

	// Database identity defaults: if --database is unset, fall back to
	// --namespace to match the helm convention where the chart's
	// release-scoped database name equals the install namespace.
	if database == "" {
		database = namespace
	}

	// Agent monitor — tracks heartbeats and manages pods
	monitor := &controllers.AgentMonitor{
		Client:         mgr.GetClient(),
		Log:            log.WithName("agent-monitor"),
		Namespace:      namespace,
		Interval:       interval,
		OfflineTimeout: offlineTimeout,
		StewardImage:   stewardImage,
		Database:       database,
		Prefix:         prefix,
		DolthubRemote:  dolthubRemote,
	}
	if err := mgr.Add(monitor); err != nil {
		log.Error(err, "unable to add agent monitor")
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
