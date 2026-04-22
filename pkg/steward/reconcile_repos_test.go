package steward

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/config"
)

// captureDaemonLog redirects the standard logger to a buffer for the
// duration of the test and returns that buffer. The default logger is
// restored on cleanup.
func captureDaemonLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	origOut := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(origOut)
		log.SetFlags(origFlags)
	})
	return &buf
}

// TestReconcileSharedRepos_ClusterNativeSkips pins the laptop-only invariant
// of reconcileSharedRepos: when the tower's EffectiveDeploymentMode is
// cluster-native, the function returns early with a skip log and performs
// no dolt read or LocalBindings write. Running it in cluster-native replicas
// would cross-wire per-machine LocalBindings state that has no meaning
// across pods (see spi-v8372, spi-e8rne).
//
// The test proves the early-return by asserting the skip log line was
// emitted. Because the function short-circuits before dolt.RawQuery, no
// dolt server is needed — a reachable dolt would prove nothing about
// whether the gate fired vs. the query returning zero rows.
func TestReconcileSharedRepos_ClusterNativeSkips(t *testing.T) {
	buf := captureDaemonLog(t)

	tower := config.TowerConfig{
		Name:           "cluster-tower",
		Database:       "beads_cluster",
		DeploymentMode: config.DeploymentModeClusterNative,
	}

	if err := reconcileSharedRepos(tower); err != nil {
		t.Fatalf("reconcileSharedRepos(cluster-native) returned error: %v", err)
	}

	got := buf.String()
	if !strings.Contains(got, "skipping (cluster-native mode") {
		t.Errorf("cluster-native: expected skip log line, got: %q", got)
	}
	if !strings.Contains(got, tower.Name) {
		t.Errorf("cluster-native: skip log missing tower name %q, got: %q", tower.Name, got)
	}
}

// TestReconcileSharedRepos_LocalNativeProceeds pins the opposite invariant:
// when EffectiveDeploymentMode is local-native, the gate does NOT fire and
// the function proceeds past the early-return. We cannot assert the full
// reconcile path here without a dolt server, but we can prove the gate
// is off by verifying the skip log line is absent. If a future regression
// broadened the gate to "anything not local-native," this test catches it.
func TestReconcileSharedRepos_LocalNativeProceeds(t *testing.T) {
	buf := captureDaemonLog(t)

	tower := config.TowerConfig{
		Name:           "laptop-tower",
		Database:       "beads_laptop",
		DeploymentMode: config.DeploymentModeLocalNative,
	}

	if err := reconcileSharedRepos(tower); err != nil {
		t.Fatalf("reconcileSharedRepos(local-native) returned error: %v", err)
	}

	if got := buf.String(); strings.Contains(got, "skipping (cluster-native mode") {
		t.Errorf("local-native: gate fired unexpectedly, log: %q", got)
	}
}

// TestReconcileSharedRepos_EmptyModeProceeds pins the empty/unset mode
// behavior: EffectiveDeploymentMode falls back to Default() (local-native),
// so legacy tower configs written before the field existed must not be
// silently gated. This is the contract in pkg/config/tower.go and the
// reason the gate compares against the explicit cluster-native constant
// rather than "!= local-native".
func TestReconcileSharedRepos_EmptyModeProceeds(t *testing.T) {
	buf := captureDaemonLog(t)

	tower := config.TowerConfig{
		Name:     "legacy-tower",
		Database: "beads_legacy",
		// DeploymentMode intentionally unset — must resolve to local-native.
	}

	if err := reconcileSharedRepos(tower); err != nil {
		t.Fatalf("reconcileSharedRepos(empty mode) returned error: %v", err)
	}

	if got := buf.String(); strings.Contains(got, "skipping (cluster-native mode") {
		t.Errorf("empty mode: gate fired unexpectedly (empty must default to local-native), log: %q", got)
	}
}

// TestReconcileSharedRepos_AttachedReservedNotGated pins the design
// decision that attached-reserved is NOT silently gated by this function.
// attached-reserved is a declaration of intent with no execution surface;
// the dispatch layer surfaces attached.ErrAttachedNotImplemented. This
// function treats attached-reserved as local-native (proceeds past the
// gate). If future work adds an attached execution surface, this test is
// the one to revisit — but today, the gate must match ONLY the explicit
// cluster-native constant.
func TestReconcileSharedRepos_AttachedReservedNotGated(t *testing.T) {
	buf := captureDaemonLog(t)

	tower := config.TowerConfig{
		Name:           "attached-tower",
		Database:       "beads_attached",
		DeploymentMode: config.DeploymentModeAttachedReserved,
	}

	if err := reconcileSharedRepos(tower); err != nil {
		t.Fatalf("reconcileSharedRepos(attached-reserved) returned error: %v", err)
	}

	if got := buf.String(); strings.Contains(got, "skipping (cluster-native mode") {
		t.Errorf("attached-reserved: gate fired unexpectedly (only cluster-native should gate), log: %q", got)
	}
}
