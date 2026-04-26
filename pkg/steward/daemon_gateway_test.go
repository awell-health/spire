package steward

import (
	"sync/atomic"
	"testing"

	"github.com/awell-health/spire/pkg/config"
)

// TestRunWebhookQueueForTower_GatewayModeSkipsBoth pins the per-tower
// gateway skip the daemon relies on. DaemonTowerCycle sits at the
// gateway/direct boundary: a gateway tower must not let
// ensureWebhookQueue (CREATE TABLE webhook_queue) nor
// integration.ProcessWebhookQueue (UPDATE webhook_queue SET processed = 1)
// run, because both reach local Dolt directly via DoltSQL and the
// cluster's own daemon owns the canonical webhook_queue in
// cluster-as-truth deployments.
//
// The test installs counter callbacks for both legs and asserts neither
// is invoked when tower.Mode == TowerModeGateway. This is the test
// coverage the round-1 review called out at daemon.go:283.
func TestRunWebhookQueueForTower_GatewayModeSkipsBoth(t *testing.T) {
	prevEnsure := ensureWebhookQueueFn
	prevProcess := processWebhookQueueFn
	t.Cleanup(func() {
		ensureWebhookQueueFn = prevEnsure
		processWebhookQueueFn = prevProcess
	})

	var ensureCalled, processCalled int32
	ensureWebhookQueueFn = func() { atomic.AddInt32(&ensureCalled, 1) }
	processWebhookQueueFn = func() (int, int) {
		atomic.AddInt32(&processCalled, 1)
		return 0, 0
	}

	tower := config.TowerConfig{
		Name:     "spi",
		Database: "beads_spi",
		Mode:     config.TowerModeGateway,
		URL:      "http://127.0.0.1:3030",
	}

	processed, errs := runWebhookQueueForTower(tower)
	if processed != 0 || errs != 0 {
		t.Errorf("runWebhookQueueForTower gateway-mode: (processed,errs) = (%d,%d), want (0,0)", processed, errs)
	}
	if got := atomic.LoadInt32(&ensureCalled); got != 0 {
		t.Errorf("ensureWebhookQueue was called %d time(s) under gateway-mode; want 0 (table is server-owned)", got)
	}
	if got := atomic.LoadInt32(&processCalled); got != 0 {
		t.Errorf("ProcessWebhookQueue was called %d time(s) under gateway-mode; want 0 (queue is server-owned)", got)
	}
}

// TestRunWebhookQueueForTower_DirectModeRunsBoth confirms direct-mode
// passthrough — the guard must not regress to "always skip", which would
// silently break local-native webhook intake. Both ensureWebhookQueue
// and ProcessWebhookQueue must be invoked exactly once for a direct
// tower, and the (processed, errors) pair the helper returns must match
// what processWebhookQueueFn returned.
func TestRunWebhookQueueForTower_DirectModeRunsBoth(t *testing.T) {
	prevEnsure := ensureWebhookQueueFn
	prevProcess := processWebhookQueueFn
	t.Cleanup(func() {
		ensureWebhookQueueFn = prevEnsure
		processWebhookQueueFn = prevProcess
	})

	var ensureCalled, processCalled int32
	ensureWebhookQueueFn = func() { atomic.AddInt32(&ensureCalled, 1) }
	processWebhookQueueFn = func() (int, int) {
		atomic.AddInt32(&processCalled, 1)
		return 3, 1
	}

	tower := config.TowerConfig{
		Name:     "spi-local",
		Database: "beads_spi",
		Mode:     config.TowerModeDirect,
	}

	processed, errs := runWebhookQueueForTower(tower)
	if processed != 3 || errs != 1 {
		t.Errorf("runWebhookQueueForTower direct-mode: (processed,errs) = (%d,%d), want (3,1)", processed, errs)
	}
	if got := atomic.LoadInt32(&ensureCalled); got != 1 {
		t.Errorf("ensureWebhookQueue was called %d time(s) under direct-mode; want 1", got)
	}
	if got := atomic.LoadInt32(&processCalled); got != 1 {
		t.Errorf("ProcessWebhookQueue was called %d time(s) under direct-mode; want 1", got)
	}
}

// TestRunWebhookQueueForTower_EmptyModeRunsBoth covers the legacy
// default: a tower with Mode == "" is treated as direct. The
// IsGatewayMode predicate returns false for empty mode, so the helper
// must drive both legs the same way TowerModeDirect does.
func TestRunWebhookQueueForTower_EmptyModeRunsBoth(t *testing.T) {
	prevEnsure := ensureWebhookQueueFn
	prevProcess := processWebhookQueueFn
	t.Cleanup(func() {
		ensureWebhookQueueFn = prevEnsure
		processWebhookQueueFn = prevProcess
	})

	var ensureCalled, processCalled int32
	ensureWebhookQueueFn = func() { atomic.AddInt32(&ensureCalled, 1) }
	processWebhookQueueFn = func() (int, int) {
		atomic.AddInt32(&processCalled, 1)
		return 0, 0
	}

	tower := config.TowerConfig{
		Name:     "spi-legacy",
		Database: "beads_spi",
		// Mode left empty (legacy direct default).
	}

	if _, _ = runWebhookQueueForTower(tower); false {
	}
	if got := atomic.LoadInt32(&ensureCalled); got != 1 {
		t.Errorf("ensureWebhookQueue was called %d time(s) under empty-mode; want 1", got)
	}
	if got := atomic.LoadInt32(&processCalled); got != 1 {
		t.Errorf("ProcessWebhookQueue was called %d time(s) under empty-mode; want 1", got)
	}
}
