package steward

// Tests for the cluster-native phase dispatch seam added in spi-agmsk5.
//
// The review and hooked-sweep paths used to call backend.Spawn
// unconditionally, bypassing the operator in cluster-native mode. These
// tests pin the seam behavior:
//
//   - dispatchPhase routes to dispatchPhaseClusterNative in cluster-
//     native mode and to backend.Spawn otherwise.
//   - dispatchPhaseClusterNative emits a WorkloadIntent stamped with the
//     requested phase and a fresh dispatch_seq from NextDispatchSeqFunc.
//   - The helper never calls backend.Spawn, preserving the cluster_dispatch.go
//     file-level invariant.

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/steward/intent"
)

// phaseTrackingPublisher captures every Publish call so tests can
// assert on the emitted WorkloadIntent shape.
type phaseTrackingPublisher struct {
	published []intent.WorkloadIntent
	err       error
}

func (p *phaseTrackingPublisher) Publish(_ context.Context, i intent.WorkloadIntent) error {
	if p.err != nil {
		return p.err
	}
	p.published = append(p.published, i)
	return nil
}

// phaseSpyBackend records Spawn calls so tests can assert the cluster-
// native path never invokes backend.Spawn.
type phaseSpyBackend struct {
	spawns []agent.SpawnConfig
}

func (b *phaseSpyBackend) Spawn(cfg agent.SpawnConfig) (agent.Handle, error) {
	b.spawns = append(b.spawns, cfg)
	return &phaseFakeHandle{id: cfg.Name}, nil
}
func (b *phaseSpyBackend) List() ([]agent.Info, error)                   { return nil, nil }
func (b *phaseSpyBackend) Logs(_ string) (io.ReadCloser, error)          { return nil, os.ErrNotExist }
func (b *phaseSpyBackend) Kill(_ string) error                           { return nil }

type phaseFakeHandle struct{ id string }

func (h *phaseFakeHandle) Wait() error                { return nil }
func (h *phaseFakeHandle) Signal(_ os.Signal) error   { return nil }
func (h *phaseFakeHandle) Alive() bool                { return true }
func (h *phaseFakeHandle) Name() string               { return h.id }
func (h *phaseFakeHandle) Identifier() string         { return h.id }

// withStubbedNextDispatchSeq swaps NextDispatchSeqFunc for the duration
// of a test, returning a deterministic sequence so assertions about
// DispatchSeq are stable without a real dolt store.
func withStubbedNextDispatchSeq(t *testing.T, seq int) {
	t.Helper()
	orig := NextDispatchSeqFunc
	NextDispatchSeqFunc = func(_ string) (int, error) { return seq, nil }
	t.Cleanup(func() { NextDispatchSeqFunc = orig })
}

func TestDispatchPhaseClusterNative_EmitsPhaseKeyedIntent(t *testing.T) {
	withStubbedNextDispatchSeq(t, 7)

	pub := &phaseTrackingPublisher{}
	cd := &ClusterDispatchConfig{
		Resolver:  fakeResolver{},
		Publisher: pub,
	}

	err := dispatchPhaseClusterNative(context.Background(), cd, "spi-rev1", intent.PhaseReview)
	if err != nil {
		t.Fatalf("dispatchPhaseClusterNative: %v", err)
	}
	if len(pub.published) != 1 {
		t.Fatalf("published = %d intents, want 1", len(pub.published))
	}
	got := pub.published[0]
	if got.TaskID != "spi-rev1" {
		t.Errorf("TaskID = %q, want spi-rev1", got.TaskID)
	}
	if got.DispatchSeq != 7 {
		t.Errorf("DispatchSeq = %d, want 7 (stub)", got.DispatchSeq)
	}
	if got.FormulaPhase != intent.PhaseReview {
		t.Errorf("FormulaPhase = %q, want %q", got.FormulaPhase, intent.PhaseReview)
	}
	if got.RepoIdentity.URL == "" || got.RepoIdentity.BaseBranch == "" {
		t.Errorf("RepoIdentity incomplete: %+v", got.RepoIdentity)
	}
	if got.HandoffMode == "" {
		t.Errorf("HandoffMode empty — resolver should set default")
	}
}

func TestDispatchPhaseClusterNative_HonorsHandoffModeOverride(t *testing.T) {
	withStubbedNextDispatchSeq(t, 1)
	pub := &phaseTrackingPublisher{}
	cd := &ClusterDispatchConfig{
		Resolver:    fakeResolver{},
		Publisher:   pub,
		HandoffMode: "custom-mode",
	}
	if err := dispatchPhaseClusterNative(context.Background(), cd, "spi-x", intent.PhaseImplement); err != nil {
		t.Fatalf("dispatchPhaseClusterNative: %v", err)
	}
	if pub.published[0].HandoffMode != "custom-mode" {
		t.Errorf("HandoffMode = %q, want custom-mode", pub.published[0].HandoffMode)
	}
}

func TestDispatchPhaseClusterNative_RequiresConfig(t *testing.T) {
	if err := dispatchPhaseClusterNative(context.Background(), nil, "spi-x", intent.PhaseReview); err == nil {
		t.Errorf("nil config: want error")
	}

	cd := &ClusterDispatchConfig{} // missing resolver+publisher
	if err := dispatchPhaseClusterNative(context.Background(), cd, "spi-x", intent.PhaseReview); err == nil {
		t.Errorf("empty config: want error")
	}

	cd = &ClusterDispatchConfig{Resolver: fakeResolver{}, Publisher: &phaseTrackingPublisher{}}
	if err := dispatchPhaseClusterNative(context.Background(), cd, "", intent.PhaseReview); err == nil {
		t.Errorf("empty bead ID: want error")
	}
	if err := dispatchPhaseClusterNative(context.Background(), cd, "spi-x", ""); err == nil {
		t.Errorf("empty phase: want error")
	}
}

func TestDispatchPhaseClusterNative_PropagatesPublisherError(t *testing.T) {
	withStubbedNextDispatchSeq(t, 1)
	wantErr := errors.New("boom")
	cd := &ClusterDispatchConfig{
		Resolver:  fakeResolver{},
		Publisher: &phaseTrackingPublisher{err: wantErr},
	}
	err := dispatchPhaseClusterNative(context.Background(), cd, "spi-x", intent.PhaseReview)
	if err == nil || !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want wrapping %v", err, wantErr)
	}
}

func TestDispatchPhase_ClusterNativeEmitsIntentAndSkipsSpawn(t *testing.T) {
	withStubbedNextDispatchSeq(t, 2)
	pub := &phaseTrackingPublisher{}
	backend := &phaseSpyBackend{}

	pd := PhaseDispatch{
		Mode: config.DeploymentModeClusterNative,
		ClusterDispatch: &ClusterDispatchConfig{
			Resolver:  fakeResolver{},
			Publisher: pub,
		},
	}
	handle, err := dispatchPhase(context.Background(), pd, backend, agent.SpawnConfig{
		Name:   "reviewer-spi-rev1",
		BeadID: "spi-rev1",
		Role:   agent.RoleSage,
	}, intent.PhaseReview)
	if err != nil {
		t.Fatalf("dispatchPhase: %v", err)
	}
	if handle != nil {
		t.Errorf("handle = %v, want nil (cluster-native emits intent, no local handle)", handle)
	}
	if len(backend.spawns) != 0 {
		t.Errorf("backend.Spawn called %d time(s), want 0 — cluster-native must not spawn", len(backend.spawns))
	}
	if len(pub.published) != 1 || pub.published[0].FormulaPhase != intent.PhaseReview {
		t.Errorf("published intents = %+v, want one with phase %q", pub.published, intent.PhaseReview)
	}
}

func TestDispatchPhase_LocalNativeForwardsToBackend(t *testing.T) {
	pub := &phaseTrackingPublisher{}
	backend := &phaseSpyBackend{}

	pd := PhaseDispatch{
		Mode: config.DeploymentModeLocalNative,
		ClusterDispatch: &ClusterDispatchConfig{
			Resolver:  fakeResolver{},
			Publisher: pub,
		},
	}
	sc := agent.SpawnConfig{Name: "reviewer-spi-rev1", BeadID: "spi-rev1", Role: agent.RoleSage}
	handle, err := dispatchPhase(context.Background(), pd, backend, sc, intent.PhaseReview)
	if err != nil {
		t.Fatalf("dispatchPhase: %v", err)
	}
	if handle == nil {
		t.Errorf("handle = nil, want non-nil — local-native should spawn")
	}
	if len(backend.spawns) != 1 {
		t.Fatalf("backend.Spawn called %d time(s), want 1", len(backend.spawns))
	}
	if backend.spawns[0].Name != sc.Name || backend.spawns[0].BeadID != sc.BeadID || backend.spawns[0].Role != sc.Role {
		t.Errorf("spawn cfg = %+v, want %+v (phase must not mutate SpawnConfig)", backend.spawns[0], sc)
	}
	if len(pub.published) != 0 {
		t.Errorf("published = %d, want 0 — local-native must not emit", len(pub.published))
	}
}

func TestDispatchPhase_ZeroValueDefaultsToSpawn(t *testing.T) {
	// A PhaseDispatch{} (zero value, Mode == "") falls through to the
	// local-native branch so existing test callers that don't exercise
	// cluster-native behavior keep working.
	backend := &phaseSpyBackend{}
	pd := PhaseDispatch{}
	handle, err := dispatchPhase(context.Background(), pd, backend, agent.SpawnConfig{
		Name:   "x",
		BeadID: "spi-x",
	}, intent.PhaseReview)
	if err != nil {
		t.Fatalf("dispatchPhase: %v", err)
	}
	if handle == nil {
		t.Errorf("handle = nil, want non-nil (zero PhaseDispatch → local-native spawn)")
	}
	if len(backend.spawns) != 1 {
		t.Errorf("backend.Spawn called %d time(s), want 1", len(backend.spawns))
	}
}

func TestDispatchPhase_ClusterNativeWithoutConfigFallsBackToSpawn(t *testing.T) {
	// Safety valve: if the tower is cluster-native but ClusterDispatch
	// isn't wired (e.g. factory returned nil mid-cycle), fall back to
	// backend.Spawn with a log line rather than silently dropping the
	// work. The observable behavior for callers is "handle non-nil,
	// backend.Spawn called".
	backend := &phaseSpyBackend{}
	pd := PhaseDispatch{Mode: config.DeploymentModeClusterNative, ClusterDispatch: nil}
	handle, err := dispatchPhase(context.Background(), pd, backend, agent.SpawnConfig{
		Name:   "x",
		BeadID: "spi-x",
	}, intent.PhaseReview)
	if err != nil {
		t.Fatalf("dispatchPhase: %v", err)
	}
	if handle == nil {
		t.Errorf("handle = nil, want non-nil (fallback should spawn)")
	}
	if len(backend.spawns) != 1 {
		t.Errorf("backend.Spawn called %d time(s), want 1 (fallback)", len(backend.spawns))
	}
}

// TestClusterDispatchFile_NoBackendSpawnOutsideSeam pins the cluster_dispatch.go
// file-level invariant: after this change, the file must still contain
// no direct backend.Spawn calls. dispatchPhase's local-native branch
// invokes the passed-in backend, but that call lives by design in the
// seam helper; the invariant guards against new dispatch paths being
// added that bypass the mode-aware branch.
func TestHookedResumePhase_ReturnsBeadLevel(t *testing.T) {
	cases := []struct {
		beadType string
		want     string
	}{
		{"task", "task"},
		{"bug", "bug"},
		{"feature", "feature"},
		{"epic", "epic"},
		{"chore", "chore"},
		{"", intent.PhaseWizard},
	}
	for _, tc := range cases {
		got := hookedResumePhase(tc.beadType)
		if got != tc.want {
			t.Errorf("hookedResumePhase(%q) = %q, want %q", tc.beadType, got, tc.want)
		}
		if !intent.IsBeadLevelPhase(got) {
			t.Errorf("hookedResumePhase(%q) = %q, which is not a bead-level phase — operator would misroute",
				tc.beadType, got)
		}
	}
}
