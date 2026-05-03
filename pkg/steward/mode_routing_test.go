package steward

// Seam tests that pin the three deployment-mode branches of TowerCycle.
//
// The invariants the cluster work hinges on cannot be expressed as code
// style rules — they are emergent properties of which fakes were invoked
// when. These tests deliberately wire panicking fakes into the seams a
// given mode must not touch, so a regression that silently crosses a
// boundary surfaces as a test panic rather than as a log-line audit.
//
// The three branches and their invariants:
//
//   local-native       : must NOT emit WorkloadIntent, must NOT consult a
//                        cluster identity resolver or claimer. Direct
//                        Backend.Spawn is the only allowed dispatch.
//   cluster-native     : must NOT call Backend.Spawn, must NOT read
//                        LocalBindings state/paths. ClaimThenEmit via the
//                        ClusterDispatch seams is the only allowed
//                        dispatch.
//   attached-reserved  : must NOT spawn, emit, claim, or resolve. The
//                        surface is a typed not-implemented error
//                        (attached.ErrAttachedNotImplemented).

import (
	"context"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/steward/attached"
	"github.com/awell-health/spire/pkg/steward/dispatch"
	"github.com/awell-health/spire/pkg/steward/identity"
	"github.com/awell-health/spire/pkg/steward/intent"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// --- panicking fakes: each panics if called, pinning the "must not touch" seam ---

// panicPublisher.Publish panics if called. TowerCycle running in
// local-native or attached-reserved mode must NEVER invoke the
// ClusterDispatch.Publisher.
type panicPublisher struct{}

func (panicPublisher) Publish(_ context.Context, _ intent.WorkloadIntent) error {
	panic("mode routing: this branch must not invoke intent.IntentPublisher.Publish")
}

// panicSpawnBackend.Spawn panics if called. TowerCycle running in
// cluster-native or attached-reserved mode must NEVER invoke Backend.Spawn.
// The non-spawn methods (List/Logs/Kill) are called unconditionally by
// TowerCycle and return benign defaults so the cycle can complete.
type panicSpawnBackend struct{}

func (panicSpawnBackend) Spawn(_ agent.SpawnConfig) (agent.Handle, error) {
	panic("mode routing: this branch must not invoke agent.Backend.Spawn")
}
func (panicSpawnBackend) List() ([]agent.Info, error)                       { return nil, nil }
func (panicSpawnBackend) Logs(_ string) (io.ReadCloser, error)               { return nil, os.ErrNotExist }
func (panicSpawnBackend) Kill(_ string) error                                { return nil }
func (panicSpawnBackend) TerminateBead(_ context.Context, _ string) error    { return nil }

// panicResolver.Resolve panics if called. Local-native and
// attached-reserved paths must NEVER consult the cluster identity
// resolver; cluster-native MAY, but this fake is reserved for the paths
// that must not.
type panicResolver struct{}

func (panicResolver) Resolve(_ context.Context, _ string) (identity.ClusterRepoIdentity, error) {
	panic("mode routing: this branch must not consult identity.ClusterIdentityResolver")
}

// panicClaimer.ClaimNext panics if called. Same rationale as
// panicResolver — only the cluster-native path may claim.
type panicClaimer struct{}

func (panicClaimer) ClaimNext(_ context.Context, _ dispatch.ReadyWorkSelector) (*dispatch.ClaimHandle, error) {
	panic("mode routing: this branch must not consult dispatch.AttemptClaimer")
}

// panicLocalBindingsAccessor.Get panics if called. Wired into the
// cluster-native identity resolver to prove the resolver never reaches
// into LocalBindings state or paths — the seam rule from spi-sj18k.
type panicLocalBindingsAccessor struct{}

func (panicLocalBindingsAccessor) Get(_ string) (identity.LocalBindingSnapshot, bool) {
	panic("mode routing: cluster-native resolver must not consult LocalBindings")
}

// --- cluster-native fakes: record what happened rather than panic ---

// recordingPublisher captures every WorkloadIntent it receives. The
// cluster-native path emits exactly one intent per schedulable bead
// through this publisher.
type recordingPublisher struct {
	calls []intent.WorkloadIntent
}

func (p *recordingPublisher) Publish(_ context.Context, i intent.WorkloadIntent) error {
	p.calls = append(p.calls, i)
	return nil
}

// staticResolver returns a fixed identity for any prefix. It holds no
// LocalBindings accessor itself because the steward wires the accessor on
// the real DefaultClusterIdentityResolver; this fake stands in for that
// wire when the test cares about emit counts rather than accessor
// behavior.
type staticResolver struct {
	ident identity.ClusterRepoIdentity
}

func (r staticResolver) Resolve(_ context.Context, _ string) (identity.ClusterRepoIdentity, error) {
	return r.ident, nil
}

// countingClaimer returns a fresh ClaimHandle for each selected
// candidate, numbering dispatch slots so the Publisher sees distinct
// (TaskID, DispatchSeq) rows per bead.
type countingClaimer struct {
	perTask map[string]int
}

func (c *countingClaimer) ClaimNext(ctx context.Context, selector dispatch.ReadyWorkSelector) (*dispatch.ClaimHandle, error) {
	ids, err := selector.SelectReady(ctx)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if c.perTask == nil {
			c.perTask = make(map[string]int)
		}
		c.perTask[id]++
		return &dispatch.ClaimHandle{
			TaskID:      id,
			DispatchSeq: c.perTask[id],
			ClaimedAt:   time.Now().UTC(),
		}, nil
	}
	return nil, nil
}

func serialString(n int) string {
	if n == 0 {
		return "0"
	}
	var out []byte
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	return string(out)
}

// --- test scaffolding ---

// modeRoutingSetup installs the minimum set of function-var mocks TowerCycle
// needs to execute without a real store. The test caller supplies the
// tower config (to set DeploymentMode and LocalBindings) and the
// schedulable beads.
func modeRoutingSetup(t *testing.T, tc *config.TowerConfig, schedulable []store.Bead) {
	t.Helper()

	origBeadsDir := BeadsDirForTowerFunc
	origStoreOpen := StoreOpenAtFunc
	origCommit := CommitPendingFunc
	origSched := GetSchedulableWorkFunc
	origDispatchable := DispatchableBeadsFunc
	origLoadTower := LoadTowerConfigFunc
	origList := ListBeadsFunc
	origAttempt := GetActiveAttemptFunc
	origInstance := InstanceIDFunc
	origRaise := RaiseCorruptedBeadAlertFunc
	origChildren := GetChildrenFunc

	BeadsDirForTowerFunc = func(_ string) string { return "/fake/.beads" }
	StoreOpenAtFunc = func(_ string) (beads.Storage, error) { return nil, nil }
	CommitPendingFunc = func(_ string) error { return nil }
	GetSchedulableWorkFunc = func(_ beads.WorkFilter) (*store.ScheduleResult, error) {
		return &store.ScheduleResult{Schedulable: schedulable}, nil
	}
	// The dispatch loop now consumes lifecycle.DispatchableBeads
	// (spi-jzs5xq); fan the schedulable fixture through both the legacy
	// and the new hook so existing tests that exercise routing decisions
	// still drive a non-empty candidate set.
	DispatchableBeadsFunc = func(_ context.Context) ([]*store.Bead, error) {
		out := make([]*store.Bead, 0, len(schedulable))
		for i := range schedulable {
			out = append(out, &schedulable[i])
		}
		return out, nil
	}
	LoadTowerConfigFunc = func(_ string) (*config.TowerConfig, error) {
		return tc, nil
	}
	ListBeadsFunc = func(_ beads.IssueFilter) ([]store.Bead, error) { return nil, nil }
	GetActiveAttemptFunc = func(_ string) (*store.Bead, error) { return nil, nil }
	InstanceIDFunc = func() string { return "mode-routing-test-instance" }
	RaiseCorruptedBeadAlertFunc = func(_ string, _ error) {}
	GetChildrenFunc = func(_ string) ([]store.Bead, error) { return nil, nil }

	t.Cleanup(func() {
		BeadsDirForTowerFunc = origBeadsDir
		StoreOpenAtFunc = origStoreOpen
		CommitPendingFunc = origCommit
		GetSchedulableWorkFunc = origSched
		DispatchableBeadsFunc = origDispatchable
		LoadTowerConfigFunc = origLoadTower
		ListBeadsFunc = origList
		GetActiveAttemptFunc = origAttempt
		InstanceIDFunc = origInstance
		RaiseCorruptedBeadAlertFunc = origRaise
		GetChildrenFunc = origChildren
	})
}

// --- local-native: must not emit, must not touch cluster seams ---

// TestTowerCycle_LocalNative_DoesNotEmitIntent pins the local-native
// invariant: no matter what ClusterDispatch is wired with, a local-native
// tower must reach Backend.Spawn and MUST NOT consult the publisher,
// claimer, or resolver. panicking fakes are wired so any cross-boundary
// call surfaces as a test panic.
func TestTowerCycle_LocalNative_DoesNotEmitIntent(t *testing.T) {
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	tc := &config.TowerConfig{
		Name:           "local-tower",
		DeploymentMode: config.DeploymentModeLocalNative,
		LocalBindings: map[string]*config.LocalRepoBinding{
			"spi": {Prefix: "spi", State: "bound", LocalPath: "/tmp/spi"},
		},
	}
	modeRoutingSetup(t, tc, []store.Bead{{ID: "spi-alpha", Type: "task"}})

	backend := &spawnTrackingBackend{}

	// Wire cluster-native seams with panicking fakes. A correct
	// local-native cycle must not reach any of them.
	cfg := StewardConfig{
		Backend:           backend,
		StaleThreshold:    10 * time.Minute,
		ShutdownThreshold: 30 * time.Minute,
		ClusterDispatch: &ClusterDispatchConfig{
			Resolver:  panicResolver{},
			Claimer:   panicClaimer{},
			Publisher: panicPublisher{},
		},
	}

	// Any panic from the fakes fails the test instead of the suite.
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("local-native cycle touched a cluster-native seam: %v", p)
		}
	}()

	TowerCycle(1, "local-tower", cfg)

	// Assert local-native actually dispatched — otherwise a future
	// regression that returns early silently would pass this test.
	if len(backend.spawns) != 1 {
		t.Fatalf("local-native: spawn count = %d, want 1", len(backend.spawns))
	}
	if backend.spawns[0].BeadID != "spi-alpha" {
		t.Errorf("local-native: spawned BeadID = %q, want %q", backend.spawns[0].BeadID, "spi-alpha")
	}
	if backend.spawns[0].Role != agent.RoleWizard {
		t.Errorf("local-native: spawn role = %q, want %q", backend.spawns[0].Role, agent.RoleWizard)
	}
}

// TestTowerCycle_LocalNative_StableWithoutClusterDispatch pins the
// separate property that ClusterDispatch is entirely optional on the
// local-native path — if a regression introduced a nil-deref when
// ClusterDispatch is unset, this test would catch it.
func TestTowerCycle_LocalNative_StableWithoutClusterDispatch(t *testing.T) {
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	tc := &config.TowerConfig{
		Name:           "local-tower",
		DeploymentMode: config.DeploymentModeLocalNative,
		LocalBindings: map[string]*config.LocalRepoBinding{
			"spi": {Prefix: "spi", State: "bound", LocalPath: "/tmp/spi"},
		},
	}
	modeRoutingSetup(t, tc, []store.Bead{{ID: "spi-beta", Type: "task"}})

	backend := &spawnTrackingBackend{}
	cfg := StewardConfig{
		Backend:           backend,
		StaleThreshold:    10 * time.Minute,
		ShutdownThreshold: 30 * time.Minute,
		// ClusterDispatch intentionally nil.
	}

	TowerCycle(1, "local-tower", cfg)

	if len(backend.spawns) != 1 {
		t.Fatalf("local-native (nil ClusterDispatch): spawn count = %d, want 1", len(backend.spawns))
	}
}

// --- cluster-native: must emit through the seam, must not spawn or read LocalBindings ---

// TestTowerCycle_ClusterNative_EmitsIntentAndSkipsSpawn pins the
// cluster-native invariants:
//
//   - Backend.Spawn is never called (panicSpawnBackend).
//   - Publisher.Publish is called exactly once per schedulable bead.
//   - The identity resolver does not dereference a LocalBindings
//     accessor (panicLocalBindingsAccessor wired into
//     DefaultClusterIdentityResolver.LocalBindings).
//   - A TowerConfig whose LocalBindings carry a "skipped" state does
//     NOT suppress cluster-native dispatch (proving the cluster branch
//     does not apply the local-native bind-state gate).
func TestTowerCycle_ClusterNative_EmitsIntentAndSkipsSpawn(t *testing.T) {
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	// LocalBindings whose State would DROP the bead on the local-native
	// path. Cluster-native must ignore State/LocalPath outright, so the
	// publisher must still see the bead.
	tc := &config.TowerConfig{
		Name:           "cluster-tower",
		DeploymentMode: config.DeploymentModeClusterNative,
		LocalBindings: map[string]*config.LocalRepoBinding{
			"spi": {Prefix: "spi", State: "skipped", LocalPath: "/tmp/must-not-be-read"},
		},
	}
	schedulable := []store.Bead{
		{ID: "spi-gamma", Type: "task"},
		{ID: "spi-delta", Type: "task"},
	}
	modeRoutingSetup(t, tc, schedulable)

	// Wire a real DefaultClusterIdentityResolver whose LocalBindings
	// accessor is the panicking fake — proves the resolver code path
	// never reaches for LocalBindings on the cluster-native branch.
	registry := &fakeRegistryStore{
		rows: map[string]fakeRegistryRow{
			"spi": {url: "https://example.test/spire.git", branch: "main"},
		},
	}
	resolver := &identity.DefaultClusterIdentityResolver{
		Registry:      registry,
		LocalBindings: panicLocalBindingsAccessor{},
	}
	claimer := &countingClaimer{}
	pub := &recordingPublisher{}

	backend := panicSpawnBackend{}
	cfg := StewardConfig{
		Backend:           backend,
		StaleThreshold:    10 * time.Minute,
		ShutdownThreshold: 30 * time.Minute,
		ClusterDispatch: &ClusterDispatchConfig{
			Resolver:  resolver,
			Claimer:   claimer,
			Publisher: pub,
		},
	}

	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("cluster-native cycle crossed a boundary (spawn or LocalBindings): %v", p)
		}
	}()

	TowerCycle(1, "cluster-tower", cfg)

	if got := len(pub.calls); got != len(schedulable) {
		t.Fatalf("cluster-native: publisher calls = %d, want %d", got, len(schedulable))
	}
	seen := map[string]bool{}
	for _, c := range pub.calls {
		if c.TaskID == "" {
			t.Errorf("cluster-native: emitted intent has empty TaskID: %+v", c)
		}
		if c.DispatchSeq < 1 {
			t.Errorf("cluster-native: emitted intent has invalid DispatchSeq=%d: %+v", c.DispatchSeq, c)
		}
		if c.RepoIdentity.URL != "https://example.test/spire.git" {
			t.Errorf("cluster-native: intent URL = %q, want canonical registry URL", c.RepoIdentity.URL)
		}
		if c.RepoIdentity.Prefix != "spi" {
			t.Errorf("cluster-native: intent Prefix = %q, want %q", c.RepoIdentity.Prefix, "spi")
		}
		key := fmt.Sprintf("%s#%d", c.TaskID, c.DispatchSeq)
		if seen[key] {
			t.Errorf("cluster-native: duplicate (TaskID, DispatchSeq) %q across emits", key)
		}
		seen[key] = true
	}
}

// TestTowerCycle_ClusterNative_NilClusterDispatchIsInert pins the "nil
// ClusterDispatch means skip cluster dispatch" contract: the cycle does
// not panic, does not spawn, does not emit. This is the production
// guard when a cluster-native tower boots without the cmd/spire wiring
// having finished populating ClusterDispatch yet.
func TestTowerCycle_ClusterNative_NilClusterDispatchIsInert(t *testing.T) {
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	tc := &config.TowerConfig{
		Name:           "cluster-tower",
		DeploymentMode: config.DeploymentModeClusterNative,
	}
	modeRoutingSetup(t, tc, []store.Bead{{ID: "spi-eps", Type: "task"}})

	backend := panicSpawnBackend{}
	cfg := StewardConfig{
		Backend:           backend,
		StaleThreshold:    10 * time.Minute,
		ShutdownThreshold: 30 * time.Minute,
		// ClusterDispatch intentionally nil.
	}

	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("cluster-native with nil ClusterDispatch panicked: %v", p)
		}
	}()

	TowerCycle(1, "cluster-tower", cfg)
	// No assertions on mutations; the invariant is "no panic, no spawn",
	// and Spawn would panic via panicSpawnBackend if it ran.
}

// TestTowerCycle_ClusterNative_InvokesBuildClusterDispatch pins the
// factory contract: when ClusterDispatch is nil but
// BuildClusterDispatch is set, TowerCycle calls the factory ONCE per
// cycle (scoped to towerName) and dispatches through the returned
// config. This is the shape cmd/spire uses in production — the factory
// is the only place store.ActiveDB() is read, because the DB is only
// valid inside TowerCycle's StoreOpenAtFunc-scoped block.
func TestTowerCycle_ClusterNative_InvokesBuildClusterDispatch(t *testing.T) {
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	tc := &config.TowerConfig{
		Name:           "factory-tower",
		DeploymentMode: config.DeploymentModeClusterNative,
	}
	schedulable := []store.Bead{{ID: "spi-factory1", Type: "task"}}
	modeRoutingSetup(t, tc, schedulable)

	registry := &fakeRegistryStore{
		rows: map[string]fakeRegistryRow{
			"spi": {url: "https://example.test/factory.git", branch: "main"},
		},
	}
	claimer := &countingClaimer{}
	pub := &recordingPublisher{}

	factoryCalls := 0
	factory := func(name string) *ClusterDispatchConfig {
		factoryCalls++
		if name != "factory-tower" {
			t.Errorf("factory: tower name = %q, want %q", name, "factory-tower")
		}
		return &ClusterDispatchConfig{
			Resolver: &identity.DefaultClusterIdentityResolver{
				Registry: registry,
			},
			Claimer:   claimer,
			Publisher: pub,
		}
	}

	backend := panicSpawnBackend{}
	cfg := StewardConfig{
		Backend:              backend,
		StaleThreshold:       10 * time.Minute,
		ShutdownThreshold:    30 * time.Minute,
		BuildClusterDispatch: factory,
	}

	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("factory-wired cluster cycle panicked: %v", p)
		}
	}()

	TowerCycle(1, "factory-tower", cfg)

	if factoryCalls != 1 {
		t.Fatalf("factory calls = %d, want 1", factoryCalls)
	}
	if got := len(pub.calls); got != 1 {
		t.Fatalf("publisher calls = %d, want 1", got)
	}
	if pub.calls[0].TaskID == "" {
		t.Errorf("emitted intent has empty TaskID")
	}
	if pub.calls[0].DispatchSeq < 1 {
		t.Errorf("emitted intent has invalid DispatchSeq=%d", pub.calls[0].DispatchSeq)
	}
	if pub.calls[0].RepoIdentity.URL != "https://example.test/factory.git" {
		t.Errorf("intent URL = %q, want factory.git URL", pub.calls[0].RepoIdentity.URL)
	}
}

// TestTowerCycle_ClusterNative_FactoryOverriddenByExplicitDispatch pins
// the precedence contract: when a caller sets BOTH ClusterDispatch and
// BuildClusterDispatch, the explicit ClusterDispatch wins and the
// factory is never invoked. This keeps test overrides sharp — a test
// that wires an explicit panicking fake must not see its fake silently
// replaced by a factory-built real config.
func TestTowerCycle_ClusterNative_FactoryOverriddenByExplicitDispatch(t *testing.T) {
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	tc := &config.TowerConfig{
		Name:           "override-tower",
		DeploymentMode: config.DeploymentModeClusterNative,
	}
	modeRoutingSetup(t, tc, []store.Bead{{ID: "spi-override1", Type: "task"}})

	factoryCalls := 0
	factory := func(_ string) *ClusterDispatchConfig {
		factoryCalls++
		return nil
	}

	registry := &fakeRegistryStore{
		rows: map[string]fakeRegistryRow{
			"spi": {url: "https://example.test/override.git", branch: "main"},
		},
	}
	claimer := &countingClaimer{}
	pub := &recordingPublisher{}

	backend := panicSpawnBackend{}
	cfg := StewardConfig{
		Backend:           backend,
		StaleThreshold:    10 * time.Minute,
		ShutdownThreshold: 30 * time.Minute,
		ClusterDispatch: &ClusterDispatchConfig{
			Resolver: &identity.DefaultClusterIdentityResolver{
				Registry: registry,
			},
			Claimer:   claimer,
			Publisher: pub,
		},
		BuildClusterDispatch: factory,
	}

	TowerCycle(1, "override-tower", cfg)

	if factoryCalls != 0 {
		t.Errorf("factory called %d times when explicit ClusterDispatch set; want 0", factoryCalls)
	}
	if got := len(pub.calls); got != 1 {
		t.Fatalf("publisher calls = %d, want 1", got)
	}
}

// TestTowerCycle_ClusterNative_NilFactoryResultSkipsDispatch pins the
// "factory returned nil" fallback: the cycle logs and skips dispatch,
// does not panic, does not spawn. Production uses this path when
// store.ActiveDB() is not yet available (e.g. a test-backed steward
// whose store does not expose *sql.DB).
func TestTowerCycle_ClusterNative_NilFactoryResultSkipsDispatch(t *testing.T) {
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	tc := &config.TowerConfig{
		Name:           "nil-factory-tower",
		DeploymentMode: config.DeploymentModeClusterNative,
	}
	modeRoutingSetup(t, tc, []store.Bead{{ID: "spi-nilfactory1", Type: "task"}})

	factoryCalls := 0
	factory := func(_ string) *ClusterDispatchConfig {
		factoryCalls++
		return nil
	}

	backend := panicSpawnBackend{}
	cfg := StewardConfig{
		Backend:              backend,
		StaleThreshold:       10 * time.Minute,
		ShutdownThreshold:    30 * time.Minute,
		BuildClusterDispatch: factory,
	}

	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("nil-factory cluster cycle panicked: %v", p)
		}
	}()

	TowerCycle(1, "nil-factory-tower", cfg)

	if factoryCalls != 1 {
		t.Errorf("factory calls = %d, want 1", factoryCalls)
	}
}

// --- attached-reserved: must not spawn, claim, emit, or resolve ---

// TestTowerCycle_AttachedReserved_NoDispatch pins the attached-reserved
// invariants: no spawn, no emit, no claim, no resolve. Every cluster
// seam is wired with a panicking fake and Backend.Spawn panics on call.
// If the branching regresses to fall through to another mode, one of the
// panics surfaces as a test failure.
//
// The contract also documents that attached-reserved surfaces a typed
// "not implemented" error (attached.ErrAttachedNotImplemented). The
// TowerCycle entry point does not return an error today, so we assert
// the equivalent observable: no dispatch happened. A companion test in
// pkg/steward/attached pins that AttachedDispatch itself returns that
// typed error.
func TestTowerCycle_AttachedReserved_NoDispatch(t *testing.T) {
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	tc := &config.TowerConfig{
		Name:           "attached-tower",
		DeploymentMode: config.DeploymentModeAttachedReserved,
		LocalBindings: map[string]*config.LocalRepoBinding{
			"spi": {Prefix: "spi", State: "bound", LocalPath: "/tmp/attached"},
		},
	}
	modeRoutingSetup(t, tc, []store.Bead{{ID: "spi-zeta", Type: "task"}})

	backend := panicSpawnBackend{}
	cfg := StewardConfig{
		Backend:           backend,
		StaleThreshold:    10 * time.Minute,
		ShutdownThreshold: 30 * time.Minute,
		// Every cluster seam panics if consulted — attached-reserved
		// must not consult any of them.
		ClusterDispatch: &ClusterDispatchConfig{
			Resolver:  panicResolver{},
			Claimer:   panicClaimer{},
			Publisher: panicPublisher{},
		},
	}

	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("attached-reserved cycle crossed a boundary: %v", p)
		}
	}()

	TowerCycle(1, "attached-tower", cfg)
}

// TestErrAttachedNotImplemented_IsTyped documents the typed surface the
// scheduling entry must use when attached-reserved is consulted. The
// steward dispatch switch already logs this sentinel; this test ensures
// the sentinel itself stays reachable from the steward package — if a
// refactor renames or relocates it, downstream callers that assert with
// errors.Is will break loudly instead of silently.
func TestErrAttachedNotImplemented_IsTyped(t *testing.T) {
	if attached.ErrAttachedNotImplemented == nil {
		t.Fatal("attached.ErrAttachedNotImplemented is nil; the typed surface is required")
	}
	if msg := attached.ErrAttachedNotImplemented.Error(); msg == "" {
		t.Fatal("attached.ErrAttachedNotImplemented has empty message")
	}
}

// --- shared test types: a minimal RegistryStore for the cluster-native test ---

type fakeRegistryRow struct {
	url    string
	branch string
}

type fakeRegistryStore struct {
	rows map[string]fakeRegistryRow
}

func (s *fakeRegistryStore) LookupRepo(_ context.Context, prefix string) (string, string, bool, error) {
	r, ok := s.rows[prefix]
	if !ok {
		return "", "", false, nil
	}
	return r.url, r.branch, true, nil
}

// Compile-time interface conformance for the fakes.
var (
	_ intent.IntentPublisher          = (*recordingPublisher)(nil)
	_ intent.IntentPublisher          = panicPublisher{}
	_ identity.ClusterIdentityResolver = staticResolver{}
	_ identity.ClusterIdentityResolver = panicResolver{}
	_ dispatch.AttemptClaimer          = (*countingClaimer)(nil)
	_ dispatch.AttemptClaimer          = panicClaimer{}
	_ agent.Backend                    = panicSpawnBackend{}
	_ identity.LocalBindingsAccessor   = panicLocalBindingsAccessor{}
	_ identity.RegistryStore           = (*fakeRegistryStore)(nil)
)
