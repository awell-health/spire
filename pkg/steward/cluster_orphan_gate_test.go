package steward

// cluster_orphan_gate_test.go — regression coverage for spi-40rtru.
//
// After spi-4d2i71 moved OrphanSweep into TowerCycle, the sweep was wired
// to the local wizard registry (~/.config/spire/wizards.json) and ran
// before deployment-mode resolution. For cluster-native (and cluster-
// attach) towers that registry is not the ownership/liveness plane —
// pod attempts won't appear in it, so OrphanSweep would mis-classify
// live pod-owned attempts as orphans, close the attempt with
// `interrupted:orphan`, and reopen the parent. The fix gates the
// local-registry ops on the tower's EffectiveDeploymentMode (loaded
// before the sweep) and fails closed when the load fails.
//
// These tests pin the gate end-to-end:
//
//   1. TowerCycle on a cluster-native tower MUST NOT invoke
//      OrphanSweepFunc — the seam that defaults to the local registry.
//   2. SweepHookedSteps with PhaseDispatch{Mode: cluster-native} MUST
//      NOT invoke RegistryRemoveFunc (the belt-and-suspenders cleanup
//      removed by spi-4d2i71). The local-native code path must still
//      fire it (covered by orphan_race_test.go).
//   3. The shouldRunLocalRegistryOps helper documents the modes that
//      are allowed to touch the local registry. The mapping changing
//      out from under either of the above tests would silently
//      reintroduce the bug.

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/lifecycle"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/executor"
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
	"github.com/awell-health/spire/pkg/steward/dispatch"
	"github.com/awell-health/spire/pkg/steward/identity"
	"github.com/awell-health/spire/pkg/steward/intent"
	"github.com/steveyegge/beads"
)

// TestTowerCycle_ClusterNative_SkipsOrphanSweep proves a cluster-native
// TowerCycle never invokes OrphanSweepFunc. Pre-fix the sweep ran
// unconditionally near the top of the cycle and would consult the local
// wizard registry — for cluster-native towers that registry has no
// useful entries and the sweep would close live pod attempts with
// `interrupted:orphan`.
func TestTowerCycle_ClusterNative_SkipsOrphanSweep(t *testing.T) {
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	tc := &config.TowerConfig{
		Name:           "cluster-tower",
		DeploymentMode: config.DeploymentModeClusterNative,
	}
	modeRoutingSetup(t, tc, []store.Bead{{ID: "spi-cn1", Type: "task"}})

	// Spy on OrphanSweepFunc — its invocation is the proxy for "did
	// the cycle consult the local wizard registry". A correct
	// cluster-native cycle must skip this seam entirely.
	sweepCalls := 0
	origSweep := OrphanSweepFunc
	OrphanSweepFunc = func() (lifecycle.SweepReport, error) {
		sweepCalls++
		return lifecycle.SweepReport{}, nil
	}
	defer func() { OrphanSweepFunc = origSweep }()

	// RegistryRemoveFunc panics if called — even the per-name removal
	// belongs to local-native only. SweepHookedSteps is a no-op here
	// (no hooked beads are seeded), but wiring the panic spy proves
	// the mode gate would catch a regression at the cleric path too.
	origRemove := RegistryRemoveFunc
	RegistryRemoveFunc = func(_ context.Context, id string) error {
		t.Fatalf("RegistryRemoveFunc invoked for %q on cluster-native tower; local registry must not be touched", id)
		return nil
	}
	defer func() { RegistryRemoveFunc = origRemove }()

	registry := &fakeRegistryStore{
		rows: map[string]fakeRegistryRow{
			"spi": {url: "https://example.test/cn.git", branch: "main"},
		},
	}
	cfg := StewardConfig{
		Backend:           panicSpawnBackend{},
		StaleThreshold:    10 * time.Minute,
		ShutdownThreshold: 30 * time.Minute,
		ClusterDispatch: &ClusterDispatchConfig{
			Resolver: &identity.DefaultClusterIdentityResolver{
				Registry: registry,
			},
			Claimer:   &countingClaimer{},
			Publisher: &recordingPublisher{},
		},
	}

	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("cluster-native cycle panicked (likely a missed mode gate): %v", p)
		}
	}()

	TowerCycle(1, "cluster-tower", cfg)

	if sweepCalls != 0 {
		t.Fatalf("OrphanSweepFunc invoked %d time(s) on cluster-native tower; want 0 (local registry must not be consulted)", sweepCalls)
	}
}

// TestTowerCycle_AttachedReserved_SkipsOrphanSweep proves the same gate
// fires for attached-reserved. Desktop cluster-attach must not run any
// local lifecycle maintenance against the attached tower; the gate
// invariant is "anything other than local-native skips the sweep."
func TestTowerCycle_AttachedReserved_SkipsOrphanSweep(t *testing.T) {
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	tc := &config.TowerConfig{
		Name:           "attached-tower",
		DeploymentMode: config.DeploymentModeAttachedReserved,
	}
	modeRoutingSetup(t, tc, []store.Bead{{ID: "spi-ar1", Type: "task"}})

	sweepCalls := 0
	origSweep := OrphanSweepFunc
	OrphanSweepFunc = func() (lifecycle.SweepReport, error) {
		sweepCalls++
		return lifecycle.SweepReport{}, nil
	}
	defer func() { OrphanSweepFunc = origSweep }()

	cfg := StewardConfig{
		Backend:           panicSpawnBackend{},
		StaleThreshold:    10 * time.Minute,
		ShutdownThreshold: 30 * time.Minute,
	}

	TowerCycle(1, "attached-tower", cfg)

	if sweepCalls != 0 {
		t.Fatalf("OrphanSweepFunc invoked %d time(s) on attached-reserved tower; want 0", sweepCalls)
	}
}

// TestTowerCycle_LocalNative_RunsOrphanSweep proves the gate doesn't
// regress local-native: the local-native sweep still fires every
// cycle (this is the spi-4d2i71 fix being preserved).
func TestTowerCycle_LocalNative_RunsOrphanSweep(t *testing.T) {
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	tc := &config.TowerConfig{
		Name:           "local-tower",
		DeploymentMode: config.DeploymentModeLocalNative,
	}
	modeRoutingSetup(t, tc, nil)

	sweepCalls := 0
	origSweep := OrphanSweepFunc
	OrphanSweepFunc = func() (lifecycle.SweepReport, error) {
		sweepCalls++
		return lifecycle.SweepReport{}, nil
	}
	defer func() { OrphanSweepFunc = origSweep }()

	cfg := StewardConfig{
		Backend:           &spawnTrackingBackend{},
		StaleThreshold:    10 * time.Minute,
		ShutdownThreshold: 30 * time.Minute,
	}

	TowerCycle(1, "local-tower", cfg)

	if sweepCalls != 1 {
		t.Fatalf("OrphanSweepFunc invoked %d time(s) on local-native tower; want 1", sweepCalls)
	}
}

// TestTowerCycle_DefaultTower_RunsOrphanSweep covers the
// no-tower-config path (towerName==""). The default tower is by
// definition single-host, so the sweep fires.
func TestTowerCycle_DefaultTower_RunsOrphanSweep(t *testing.T) {
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	modeRoutingSetup(t, &config.TowerConfig{}, nil)

	sweepCalls := 0
	origSweep := OrphanSweepFunc
	OrphanSweepFunc = func() (lifecycle.SweepReport, error) {
		sweepCalls++
		return lifecycle.SweepReport{}, nil
	}
	defer func() { OrphanSweepFunc = origSweep }()

	cfg := StewardConfig{
		Backend:           &spawnTrackingBackend{},
		StaleThreshold:    10 * time.Minute,
		ShutdownThreshold: 30 * time.Minute,
	}

	TowerCycle(1, "", cfg)

	if sweepCalls != 1 {
		t.Fatalf("OrphanSweepFunc invoked %d time(s) on default tower; want 1", sweepCalls)
	}
}

// TestTowerCycle_TowerLoadFails_SkipsOrphanSweep covers the fail-closed
// path: if LoadTowerConfigFunc errors for a named tower, the cycle
// can't tell whether it's local or cluster, so the sweep is skipped.
// Pre-fix this case fell through to running the sweep (the default
// initial mode was local-native), which would clobber a misconfigured
// cluster tower mid-recovery.
func TestTowerCycle_TowerLoadFails_SkipsOrphanSweep(t *testing.T) {
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	modeRoutingSetup(t, nil, nil)
	// Override the success-returning stub installed by modeRoutingSetup
	// with one that fails — we explicitly want the load-error path.
	LoadTowerConfigFunc = func(_ string) (*config.TowerConfig, error) {
		return nil, fmt.Errorf("simulated tower-config load failure")
	}

	sweepCalls := 0
	origSweep := OrphanSweepFunc
	OrphanSweepFunc = func() (lifecycle.SweepReport, error) {
		sweepCalls++
		return lifecycle.SweepReport{}, nil
	}
	defer func() { OrphanSweepFunc = origSweep }()

	cfg := StewardConfig{
		Backend:           &spawnTrackingBackend{},
		StaleThreshold:    10 * time.Minute,
		ShutdownThreshold: 30 * time.Minute,
	}

	TowerCycle(1, "broken-tower", cfg)

	if sweepCalls != 0 {
		t.Fatalf("OrphanSweepFunc invoked %d time(s) when tower-config load failed; want 0 (fail-closed)", sweepCalls)
	}
}

// TestSweepHookedSteps_ClusterNative_SkipsRegistryRemove proves the
// cluster-mode SweepHookedSteps does not invoke removeStaleWizardEntry
// (the belt-and-suspenders cleanup added in spi-4d2i71). Cluster pods
// register through the cluster ownership plane, not the local wizards.json,
// so any registry touch from the steward in cluster mode is at best
// useless and at worst a route into the same false-orphan trap.
func TestSweepHookedSteps_ClusterNative_SkipsRegistryRemove(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", cfgDir)
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	cleanup := stubFailureEvidenceHooks(t)
	defer cleanup()

	hookedStatus := beads.Status("hooked")
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		if filter.Status != nil && *filter.Status == hookedStatus {
			return []store.Bead{
				{ID: "spi-cnh1", Status: "hooked", Type: "task"},
			}, nil
		}
		return nil, nil
	}
	GetActiveAttemptFunc = func(parentID string) (*store.Bead, error) {
		// Active attempt looks like a cluster pod entry: agent label
		// is the pod name; it would NOT appear in the local registry.
		if parentID == "spi-cnh1" {
			return &store.Bead{
				ID:     "spi-cnh1.attempt-1",
				Status: "in_progress",
				Labels: []string{"attempt", "agent:wizard-pod-cnh1"},
			}, nil
		}
		return nil, nil
	}
	IsOwnedByInstanceFunc = func(string, string) (bool, error) { return true, nil }
	InstanceIDFunc = func() string { return "local-instance" }

	GetHookedStepsFunc = func(parentID string) ([]store.Bead, error) {
		if parentID == "spi-cnh1" {
			return []store.Bead{
				{ID: "spi-cnh1.step-impl", Status: "hooked", Labels: []string{"step:implement-failed"}},
			}, nil
		}
		return nil, nil
	}
	GetBeadFunc = func(id string) (store.Bead, error) {
		switch id {
		case "spi-cnh1":
			return store.Bead{
				ID: "spi-cnh1", Status: "hooked", Type: "task",
				Labels: []string{"needs-human"},
			}, nil
		case "spi-recovery-cnh1":
			return store.Bead{
				ID: "spi-recovery-cnh1", Status: "closed", Type: "recovery",
				Metadata: map[string]string{
					recovery.KeyRecoveryOutcome: mustMarshalOutcome(t, recovery.RecoveryOutcome{
						SourceBeadID:  "spi-cnh1",
						Decision:      recovery.DecisionResume,
						VerifyVerdict: recovery.VerifyVerdictPass,
					}),
				},
			}, nil
		}
		return store.Bead{}, fmt.Errorf("not found: %s", id)
	}
	GetCommentsFunc = func(id string) ([]*beads.Comment, error) { return nil, nil }
	GetDependentsWithMetaFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		if id == "spi-cnh1" {
			return []*beads.IssueWithDependencyMetadata{{
				Issue:          beads.Issue{ID: "spi-recovery-cnh1", IssueType: "recovery", Status: "closed"},
				DependencyType: "caused-by",
			}}, nil
		}
		return nil, nil
	}
	UnhookStepBeadFunc = func(id string) error { return nil }
	UpdateBeadFunc = func(id string, fields map[string]interface{}) error { return nil }

	// RegistryRemoveFunc panics if called. The cluster-native gate must
	// short-circuit before any local-registry write happens.
	origRegistryRemove := RegistryRemoveFunc
	RegistryRemoveFunc = func(_ context.Context, id string) error {
		t.Fatalf("RegistryRemoveFunc invoked for %q in cluster-native SweepHookedSteps; local registry must not be touched", id)
		return nil
	}
	defer func() { RegistryRemoveFunc = origRegistryRemove }()

	// Cluster-native PhaseDispatch with a recording publisher so the
	// resume dispatch lands without panicking — the test isn't about
	// emit shape, just about NOT removing the local registry entry.
	pub := &recordingPublisher{}
	registry := &fakeRegistryStore{rows: map[string]fakeRegistryRow{"spi": {url: "https://example.test/cn.git", branch: "main"}}}
	pd := PhaseDispatch{
		Mode: config.DeploymentModeClusterNative,
		ClusterDispatch: &ClusterDispatchConfig{
			Resolver:  &identity.DefaultClusterIdentityResolver{Registry: registry},
			Claimer:   &countingClaimer{},
			Publisher: pub,
		},
	}
	gsStore := &executor.FileGraphStateStore{ConfigDir: func() (string, error) { return cfgDir, nil }}
	_ = SweepHookedSteps(false, panicSpawnBackend{}, "cluster-tower", gsStore, pd)
}

// TestSweepHookedSteps_StandardResume_ClusterNative_SkipsRegistryRemove
// covers the non-failure resume branch (design-linked / human-approval
// hook resolved). Same gate, different code path.
func TestSweepHookedSteps_StandardResume_ClusterNative_SkipsRegistryRemove(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", cfgDir)
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	cleanup := stubFailureEvidenceHooks(t)
	defer cleanup()

	hookedStatus := beads.Status("hooked")
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		if filter.Status != nil && *filter.Status == hookedStatus {
			return []store.Bead{{ID: "spi-cnh2", Status: "hooked", Type: "task"}}, nil
		}
		return nil, nil
	}
	GetActiveAttemptFunc = func(parentID string) (*store.Bead, error) { return nil, nil }
	InstanceIDFunc = func() string { return "local-instance" }
	GetHookedStepsFunc = func(parentID string) ([]store.Bead, error) {
		if parentID == "spi-cnh2" {
			return []store.Bead{
				{ID: "spi-cnh2.step-design", Status: "hooked", Labels: []string{"step:check.design-linked"}},
			}, nil
		}
		return nil, nil
	}
	GetBeadFunc = func(id string) (store.Bead, error) {
		if id == "spi-cnh2" {
			return store.Bead{ID: "spi-cnh2", Status: "hooked", Type: "task"}, nil
		}
		return store.Bead{}, fmt.Errorf("not found: %s", id)
	}
	GetCommentsFunc = func(id string) ([]*beads.Comment, error) { return nil, nil }
	UnhookStepBeadFunc = func(id string) error { return nil }
	UpdateBeadFunc = func(id string, fields map[string]interface{}) error { return nil }

	origRegistryRemove := RegistryRemoveFunc
	RegistryRemoveFunc = func(_ context.Context, id string) error {
		t.Fatalf("RegistryRemoveFunc invoked for %q in cluster-native standard-resume path; local registry must not be touched", id)
		return nil
	}
	defer func() { RegistryRemoveFunc = origRegistryRemove }()

	registry := &fakeRegistryStore{rows: map[string]fakeRegistryRow{"spi": {url: "https://example.test/cn.git", branch: "main"}}}
	pd := PhaseDispatch{
		Mode: config.DeploymentModeClusterNative,
		ClusterDispatch: &ClusterDispatchConfig{
			Resolver:  &identity.DefaultClusterIdentityResolver{Registry: registry},
			Claimer:   &countingClaimer{},
			Publisher: &recordingPublisher{},
		},
	}
	gsStore := &executor.FileGraphStateStore{ConfigDir: func() (string, error) { return cfgDir, nil }}
	_ = SweepHookedSteps(false, panicSpawnBackend{}, "cluster-tower", gsStore, pd)
}

// TestShouldRunLocalRegistryOps documents the mode→permission mapping.
// A regression in the helper would silently undo the gate; this test
// pins the expected truth table.
func TestShouldRunLocalRegistryOps(t *testing.T) {
	cases := []struct {
		mode config.DeploymentMode
		want bool
	}{
		{config.DeploymentModeLocalNative, true},
		{"", true}, // PhaseDispatch zero-value contract
		{config.DeploymentModeClusterNative, false},
		{config.DeploymentModeAttachedReserved, false},
		{"some-future-mode", false},
	}
	for _, c := range cases {
		got := shouldRunLocalRegistryOps(c.mode)
		if got != c.want {
			t.Errorf("shouldRunLocalRegistryOps(%q) = %v, want %v", c.mode, got, c.want)
		}
	}
}

// TestTowerCycle_ClusterNative_DoesNotClobberPodAttempt is the end-to-end
// invariant from the bead: a cluster-native cycle observing a hooked
// parent with a pod-owned attempt MUST NOT close the attempt or reopen
// the parent. Pre-fix the local-registry OrphanSweep would do exactly
// that because the pod's agent name is absent from wizards.json. The
// gate now skips the sweep entirely.
func TestTowerCycle_ClusterNative_DoesNotClobberPodAttempt(t *testing.T) {
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	tc := &config.TowerConfig{
		Name:           "cluster-pod-tower",
		DeploymentMode: config.DeploymentModeClusterNative,
	}
	modeRoutingSetup(t, tc, nil)

	// Track mutations to pod-owned attempt + parent. If the gate
	// regresses, the sweep would close the attempt and reopen the
	// parent — caught by these counters.
	var (
		mu              sync.Mutex
		closedAttempts  []string
		reopenedParents []string
	)

	// Fail the test if anyone closes the pod attempt or reopens the parent.
	origUpdate := UpdateBeadFunc
	UpdateBeadFunc = func(id string, fields map[string]interface{}) error {
		mu.Lock()
		defer mu.Unlock()
		if id == "spi-pod1" {
			if status, ok := fields["status"].(string); ok && status == "open" {
				reopenedParents = append(reopenedParents, id)
			}
		}
		return nil
	}
	defer func() { UpdateBeadFunc = origUpdate }()

	// We don't substitute store.CloseAttemptBead directly (it's not a
	// function var here) — but the gate prevents OrphanSweepFunc from
	// running, which is the only path that would close the attempt
	// in this scenario. We instead wire OrphanSweepFunc to record a
	// would-have-cleaned attempt so the test fails loudly if the gate
	// drops.
	origSweep := OrphanSweepFunc
	OrphanSweepFunc = func() (lifecycle.SweepReport, error) {
		mu.Lock()
		defer mu.Unlock()
		closedAttempts = append(closedAttempts, "spi-pod1.attempt-1")
		return lifecycle.SweepReport{Cleaned: 1}, nil
	}
	defer func() { OrphanSweepFunc = origSweep }()

	cfg := StewardConfig{
		Backend:           panicSpawnBackend{},
		StaleThreshold:    10 * time.Minute,
		ShutdownThreshold: 30 * time.Minute,
		ClusterDispatch: &ClusterDispatchConfig{
			Resolver:  &identity.DefaultClusterIdentityResolver{Registry: &fakeRegistryStore{rows: map[string]fakeRegistryRow{"spi": {url: "https://example.test/pod.git", branch: "main"}}}},
			Claimer:   &countingClaimer{},
			Publisher: &recordingPublisher{},
		},
	}

	TowerCycle(1, "cluster-pod-tower", cfg)

	mu.Lock()
	defer mu.Unlock()
	if len(closedAttempts) != 0 {
		t.Errorf("OrphanSweepFunc fired (would have closed %v) — cluster-native gate did not skip the sweep", closedAttempts)
	}
	if len(reopenedParents) != 0 {
		t.Errorf("Parent bead(s) reopened to status=open: %v — pod-owned attempts must not be clobbered", reopenedParents)
	}
}

// Compile-time check: the cluster-native test seams referenced above
// satisfy their interfaces. These are already enforced in mode_routing_test.go,
// but a duplicated assert here makes this test file independently
// readable and catches a refactor that drops the file accidentally.
var (
	_ identity.ClusterIdentityResolver = (*identity.DefaultClusterIdentityResolver)(nil)
	_ dispatch.AttemptClaimer          = (*countingClaimer)(nil)
	_ intent.IntentPublisher           = (*recordingPublisher)(nil)
	_ agent.Backend                    = panicSpawnBackend{}
)
