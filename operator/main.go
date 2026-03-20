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
		stewardImage     string
	)
	flag.StringVar(&namespace, "namespace", "spire", "Namespace to watch")
	flag.DurationVar(&interval, "interval", 2*time.Minute, "Poll interval")
	flag.DurationVar(&staleThreshold, "stale-threshold", 4*time.Hour, "Time before marking work as stale")
	flag.DurationVar(&reassignAfter, "reassign-after", 6*time.Hour, "Time before reassigning stale work")
	flag.DurationVar(&offlineTimeout, "offline-timeout", 30*time.Minute, "Time before marking agent as offline")
	flag.StringVar(&stewardImage, "steward-image", "ghcr.io/awell-health/spire-steward:latest", "Image for managed agent pods")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("operator")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
	})
	if err != nil {
		log.Error(err, "unable to create manager")
		os.Exit(1)
	}

	// Start the bead watcher as a runnable (not a reconciler — it's a poll loop)
	beadWatcher := &controllers.BeadWatcher{
		Client:    mgr.GetClient(),
		Log:       log.WithName("bead-watcher"),
		Namespace: namespace,
		Interval:  interval,
	}
	if err := mgr.Add(beadWatcher); err != nil {
		log.Error(err, "unable to add bead watcher")
		os.Exit(1)
	}

	// Workload assigner — matches pending workloads to agents
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

	// Agent monitor — tracks heartbeats and manages pods
	monitor := &controllers.AgentMonitor{
		Client:         mgr.GetClient(),
		Log:            log.WithName("agent-monitor"),
		Namespace:      namespace,
		Interval:       interval,
		OfflineTimeout: offlineTimeout,
		StewardImage:     stewardImage,
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
