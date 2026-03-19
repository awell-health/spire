package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-logr/zapr"
	"go.uber.org/zap"

	"github.com/awell-health/spire/operator/controllers"
)

func main() {
	var (
		namespace      = flag.String("namespace", "spire", "Namespace to watch")
		interval       = flag.Duration("interval", 2*time.Minute, "Poll interval")
		staleThreshold = flag.Duration("stale-threshold", 4*time.Hour, "Time before marking work as stale")
		reassignAfter  = flag.Duration("reassign-after", 6*time.Hour, "Time before reassigning stale work")
		offlineTimeout = flag.Duration("offline-timeout", 30*time.Minute, "Time before marking agent as offline")
		mayorImage     = flag.String("mayor-image", "ghcr.io/awell-health/spire-mayor:latest", "Image for managed agent pods")
	)
	flag.Parse()

	// Logger
	zapLog, _ := zap.NewProduction()
	log := zapr.NewLogger(zapLog)

	// TODO: Wire up controller-runtime client.
	// For Phase 1, this is a scaffold. The actual client setup requires:
	//   mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{...})
	// For now, we run the controllers with nil clients and shell out to bd/spire.
	// Phase 2 will add proper controller-runtime wiring.

	log.Info("spire operator starting",
		"namespace", *namespace,
		"interval", *interval,
		"staleThreshold", *staleThreshold,
	)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Start controllers in goroutines
	beadWatcher := &controllers.BeadWatcher{
		Log:       log.WithName("bead-watcher"),
		Namespace: *namespace,
		Interval:  *interval,
	}

	assigner := &controllers.WorkloadAssigner{
		Log:               log.WithName("workload-assigner"),
		Namespace:         *namespace,
		Interval:          *interval,
		StaleThreshold:    *staleThreshold,
		ReassignThreshold: *reassignAfter,
	}

	monitor := &controllers.AgentMonitor{
		Log:            log.WithName("agent-monitor"),
		Namespace:      *namespace,
		Interval:       *interval,
		OfflineTimeout: *offlineTimeout,
		MayorImage:     *mayorImage,
	}

	_ = beadWatcher
	_ = assigner
	_ = monitor

	// Phase 1: Just run spire mayor directly (no CRD watches yet)
	// Phase 2: Replace with controller-runtime manager
	log.Info("Phase 1 mode: running spire mayor loop directly")
	log.Info("Phase 2 will add CRD watches via controller-runtime")

	// For now, just run the bead watcher loop (which does pull/ready/push)
	// The assigner and monitor will be wired up when we add the k8s client
	go beadWatcher.Run(ctx)

	<-ctx.Done()
	log.Info("shutting down")
}
