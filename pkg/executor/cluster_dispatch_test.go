package executor

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/steward/intent"
)

// fakeClusterChildDispatcher is the test-side implementation of
// executor.ClusterChildDispatcher used by the cluster-mode dispatch
// tests. It records every Dispatch call so tests can assert on the
// (Role, Phase, TaskID, Reason, Runtime, RepoIdentity) shape of the
// emitted intent. Errors are configurable per-test via dispatchErr.
type fakeClusterChildDispatcher struct {
	mu          sync.Mutex
	calls       []intent.WorkloadIntent
	dispatchErr error
}

func (f *fakeClusterChildDispatcher) Dispatch(_ context.Context, wi intent.WorkloadIntent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, wi)
	return f.dispatchErr
}

func (f *fakeClusterChildDispatcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeClusterChildDispatcher) lastCall() intent.WorkloadIntent {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return intent.WorkloadIntent{}
	}
	return f.calls[len(f.calls)-1]
}

// clusterTowerConfig builds an ActiveTowerConfig fn that reports the
// effective deployment mode mode. Used by every test to flip the
// executor's useClusterChildDispatch() helper between cluster-native
// and local-native paths without touching ambient process env.
func clusterTowerConfig(mode config.DeploymentMode) func() (*TowerConfig, error) {
	return func() (*TowerConfig, error) {
		return &TowerConfig{Name: "tower-test", DeploymentMode: mode}, nil
	}
}

// TestUseClusterChildDispatch_FlipsByModeAndDispatcher locks the helper's
// 2-condition truth table: cluster-native AND non-nil dispatcher → true,
// any other combination → false. This is the single source of mode/seam
// truth that every executor and wizard call site reads, so the matrix
// MUST stay tight (no silent fallback to local Spawn in cluster-native).
func TestUseClusterChildDispatch_FlipsByModeAndDispatcher(t *testing.T) {
	cases := []struct {
		name       string
		mode       config.DeploymentMode
		dispatcher ClusterChildDispatcher
		want       bool
	}{
		{name: "cluster + dispatcher", mode: config.DeploymentModeClusterNative, dispatcher: &fakeClusterChildDispatcher{}, want: true},
		{name: "cluster + nil dispatcher", mode: config.DeploymentModeClusterNative, dispatcher: nil, want: false},
		{name: "local + dispatcher", mode: config.DeploymentModeLocalNative, dispatcher: &fakeClusterChildDispatcher{}, want: false},
		{name: "local + nil dispatcher", mode: config.DeploymentModeLocalNative, dispatcher: nil, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := &Deps{
				ActiveTowerConfig:      clusterTowerConfig(tc.mode),
				ClusterChildDispatcher: tc.dispatcher,
			}
			e := NewForTest("spi-test", "wizard-test", nil, deps)
			if got := e.useClusterChildDispatch(); got != tc.want {
				t.Errorf("useClusterChildDispatch() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestUseClusterChildDispatch_NilTowerConfigFn confirms that an
// ActiveTowerConfig fn that is itself nil (e.g. a test harness that
// never wired tower lookup) returns false rather than panicking. The
// helper has to stay defensive because NewForTest constructs Deps
// without that fn for narrow unit tests.
func TestUseClusterChildDispatch_NilTowerConfigFn(t *testing.T) {
	deps := &Deps{
		ClusterChildDispatcher: &fakeClusterChildDispatcher{},
	}
	e := NewForTest("spi-test", "wizard-test", nil, deps)
	if e.useClusterChildDispatch() {
		t.Errorf("useClusterChildDispatch() = true with nil ActiveTowerConfig, want false")
	}
}

// TestActionWizardRun_ClusterNative_ImplementEmitsIntent verifies the
// implement step in cluster-native mode dispatches through
// ClusterChildDispatcher.Dispatch with Role=apprentice / Phase=implement
// and Spawner.Spawn is NOT called. This is the load-bearing assertion
// for migration track 1: cluster-native must not call backend.Spawn
// for executor-driven child work.
func TestActionWizardRun_ClusterNative_ImplementEmitsIntent(t *testing.T) {
	withClusterAgentImage(t, "ghcr.io/example/spire-agent:test")

	disp := &fakeClusterChildDispatcher{}
	spawnCalls := 0
	backend := &mockBackend{
		spawnFn: func(_ agent.SpawnConfig) (agent.Handle, error) {
			spawnCalls++
			return &mockHandle{}, nil
		},
	}

	dir := t.TempDir()
	deps := &Deps{
		Spawner:                backend,
		ClusterChildDispatcher: disp,
		ActiveTowerConfig:      clusterTowerConfig(config.DeploymentModeClusterNative),
		ConfigDir:              func() (string, error) { return dir, nil },
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-implement",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"implement": {Action: "wizard.run", Flow: "implement"},
		},
	}

	exec := NewGraphForTest("spi-test", "wizard-test", graph, nil, deps)
	exec.graphState.RepoPath = dir
	exec.graphState.BaseBranch = "main"
	exec.graphState.TowerName = "tower-test"

	step := StepConfig{Action: "wizard.run", Flow: "implement"}
	result := actionWizardRun(exec, "implement", step, exec.graphState)
	if result.Error != nil {
		t.Fatalf("actionWizardRun returned error: %v", result.Error)
	}

	if spawnCalls != 0 {
		t.Errorf("Spawner.Spawn was called %d times in cluster-native, want 0", spawnCalls)
	}
	if disp.callCount() != 1 {
		t.Fatalf("ClusterChildDispatcher.Dispatch called %d times, want 1", disp.callCount())
	}
	got := disp.lastCall()
	if got.TaskID != "spi-test" {
		t.Errorf("intent.TaskID = %q, want %q", got.TaskID, "spi-test")
	}
	if got.Role != intent.RoleApprentice {
		t.Errorf("intent.Role = %q, want %q", got.Role, intent.RoleApprentice)
	}
	if got.Phase != intent.PhaseImplement {
		t.Errorf("intent.Phase = %q, want %q", got.Phase, intent.PhaseImplement)
	}
	if got.RepoIdentity.Prefix != "spi" {
		t.Errorf("intent.RepoIdentity.Prefix = %q, want %q", got.RepoIdentity.Prefix, "spi")
	}
	if got.RepoIdentity.BaseBranch != "main" {
		t.Errorf("intent.RepoIdentity.BaseBranch = %q, want %q", got.RepoIdentity.BaseBranch, "main")
	}
	if got.Runtime.Image != "ghcr.io/example/spire-agent:test" {
		t.Errorf("intent.Runtime.Image = %q, want %q", got.Runtime.Image, "ghcr.io/example/spire-agent:test")
	}
	if got.Reason == "" {
		t.Errorf("intent.Reason is empty — should carry log/metric continuity tag")
	}
	if got.HandoffMode == "" {
		t.Errorf("intent.HandoffMode is empty — executor must stamp resolved handoff mode")
	}

	// Outputs should report dispatched (cluster-native is fire-and-publish;
	// the synchronous result.json read happens in the operator-materialized
	// pod, not in-process).
	if result.Outputs["result"] != "dispatched" {
		t.Errorf("result.Outputs[result] = %q, want %q", result.Outputs["result"], "dispatched")
	}
}

// TestActionWizardRun_ClusterNative_ReviewFixEmitsIntent confirms the
// review-fix step (the one routed through actionReviewFix +
// dispatchCommitProducingApprentice) reaches the cluster seam with the
// right (apprentice, review-fix) pair, NOT (apprentice, fix). The
// distinction is load-bearing: intent.Allowed has separate slots for
// PhaseFix (diagnostic fix) and PhaseReviewFix (post-review apprentice
// re-engagement) and the operator routes them to different builders.
func TestActionWizardRun_ClusterNative_ReviewFixEmitsIntent(t *testing.T) {
	withClusterAgentImage(t, "ghcr.io/example/spire-agent:test")

	disp := &fakeClusterChildDispatcher{}
	spawnCalls := 0
	backend := &mockBackend{
		spawnFn: func(_ agent.SpawnConfig) (agent.Handle, error) {
			spawnCalls++
			return &mockHandle{}, nil
		},
	}

	dir := t.TempDir()
	deps := &Deps{
		Spawner:                backend,
		ClusterChildDispatcher: disp,
		ActiveTowerConfig:      clusterTowerConfig(config.DeploymentModeClusterNative),
		ConfigDir:              func() (string, error) { return dir, nil },
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-review-fix",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"review-fix": {Action: "wizard.run", Flow: "review-fix"},
		},
	}

	exec := NewGraphForTest("spi-test", "wizard-test", graph, nil, deps)
	exec.graphState.RepoPath = dir
	exec.graphState.BaseBranch = "main"

	step := StepConfig{Action: "wizard.run", Flow: "review-fix"}
	result := actionWizardRun(exec, "review-fix", step, exec.graphState)
	if result.Error != nil {
		t.Fatalf("actionWizardRun returned error: %v", result.Error)
	}

	if spawnCalls != 0 {
		t.Errorf("Spawner.Spawn was called %d times for review-fix in cluster-native, want 0", spawnCalls)
	}
	if disp.callCount() != 1 {
		t.Fatalf("ClusterChildDispatcher.Dispatch called %d times, want 1", disp.callCount())
	}
	got := disp.lastCall()
	if got.Role != intent.RoleApprentice {
		t.Errorf("intent.Role = %q, want apprentice for review-fix", got.Role)
	}
	if got.Phase != intent.PhaseReviewFix {
		t.Errorf("intent.Phase = %q, want %q (review-fix must NOT collapse to PhaseFix)", got.Phase, intent.PhaseReviewFix)
	}
	if got.TaskID != "spi-test" {
		t.Errorf("intent.TaskID = %q, want %q (review-fix is a re-entry on the same task)", got.TaskID, "spi-test")
	}
}

// TestActionWizardRun_ClusterNative_SageReviewEmitsIntent covers the
// sage-review flow: Role must be sage and Phase must be review per
// intent.Allowed[RoleSage].
func TestActionWizardRun_ClusterNative_SageReviewEmitsIntent(t *testing.T) {
	withClusterAgentImage(t, "ghcr.io/example/spire-agent:test")

	disp := &fakeClusterChildDispatcher{}
	backend := &mockBackend{
		spawnFn: func(_ agent.SpawnConfig) (agent.Handle, error) {
			t.Fatal("Spawner.Spawn must not be called in cluster-native sage-review")
			return nil, nil
		},
	}

	dir := t.TempDir()
	deps := &Deps{
		Spawner:                backend,
		ClusterChildDispatcher: disp,
		ActiveTowerConfig:      clusterTowerConfig(config.DeploymentModeClusterNative),
		ConfigDir:              func() (string, error) { return dir, nil },
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-sage-review",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"sage-review": {Action: "wizard.run", Flow: "sage-review"},
		},
	}

	exec := NewGraphForTest("spi-test", "wizard-test", graph, nil, deps)
	exec.graphState.RepoPath = dir

	step := StepConfig{Action: "wizard.run", Flow: "sage-review"}
	if result := actionWizardRun(exec, "sage-review", step, exec.graphState); result.Error != nil {
		t.Fatalf("actionWizardRun returned error: %v", result.Error)
	}

	if disp.callCount() != 1 {
		t.Fatalf("ClusterChildDispatcher.Dispatch called %d times, want 1", disp.callCount())
	}
	got := disp.lastCall()
	if got.Role != intent.RoleSage {
		t.Errorf("intent.Role = %q, want sage for sage-review", got.Role)
	}
	if got.Phase != intent.PhaseReview {
		t.Errorf("intent.Phase = %q, want %q", got.Phase, intent.PhaseReview)
	}
}

// TestActionWizardRun_LocalNative_PreservesSpawn locks the parity:
// local-native (the unchanged shipping path) MUST still call
// Spawner.Spawn exactly once and MUST NOT touch ClusterChildDispatcher
// even when the dispatcher is wired. This protects against silent
// drift where a cluster-native dispatcher leaking into a local-native
// tower would route in-process work through the operator path.
func TestActionWizardRun_LocalNative_PreservesSpawn(t *testing.T) {
	disp := &fakeClusterChildDispatcher{}
	var spawnedCfg agent.SpawnConfig
	backend := &mockBackend{
		spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
			spawnedCfg = cfg
			return &mockHandle{}, nil
		},
	}

	dir := t.TempDir()
	deps := &Deps{
		Spawner:                backend,
		ClusterChildDispatcher: disp, // wired but should be ignored
		ActiveTowerConfig:      clusterTowerConfig(config.DeploymentModeLocalNative),
		ConfigDir:              func() (string, error) { return dir, nil },
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-implement",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"implement": {Action: "wizard.run", Flow: "implement"},
		},
	}

	exec := NewGraphForTest("spi-test", "wizard-test", graph, nil, deps)
	exec.graphState.RepoPath = dir
	exec.graphState.BaseBranch = "main"
	exec.graphState.TowerName = "tower-test"

	step := StepConfig{Action: "wizard.run", Flow: "implement"}
	if result := actionWizardRun(exec, "implement", step, exec.graphState); result.Error != nil {
		t.Fatalf("actionWizardRun returned error: %v", result.Error)
	}

	if disp.callCount() != 0 {
		t.Errorf("ClusterChildDispatcher.Dispatch called %d times in local-native, want 0", disp.callCount())
	}
	if spawnedCfg.BeadID != "spi-test" {
		t.Errorf("Spawner.Spawn cfg.BeadID = %q, want %q (local-native must still spawn)", spawnedCfg.BeadID, "spi-test")
	}
	if spawnedCfg.Role != agent.RoleApprentice {
		t.Errorf("Spawner.Spawn cfg.Role = %q, want %q", spawnedCfg.Role, agent.RoleApprentice)
	}
}

// TestActionWizardRun_ClusterNative_DispatchErrorPropagates verifies
// that a Dispatch failure surfaces as the step's ActionResult.Error
// with a "cluster dispatch <step>:" prefix — the same wrapping shape
// the local-native path uses for spawn errors. Tests that callers can
// distinguish dispatch-publish failures from validation failures via
// the wrapped error chain.
func TestActionWizardRun_ClusterNative_DispatchErrorPropagates(t *testing.T) {
	withClusterAgentImage(t, "ghcr.io/example/spire-agent:test")

	publishErr := errors.New("publish failed")
	disp := &fakeClusterChildDispatcher{dispatchErr: publishErr}
	backend := &mockBackend{
		spawnFn: func(_ agent.SpawnConfig) (agent.Handle, error) {
			t.Fatal("Spawner.Spawn must not be called even when cluster Dispatch fails")
			return nil, nil
		},
	}

	dir := t.TempDir()
	deps := &Deps{
		Spawner:                backend,
		ClusterChildDispatcher: disp,
		ActiveTowerConfig:      clusterTowerConfig(config.DeploymentModeClusterNative),
		ConfigDir:              func() (string, error) { return dir, nil },
	}

	graph := &formula.FormulaStepGraph{
		Name:    "test-implement",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"implement": {Action: "wizard.run", Flow: "implement"},
		},
	}

	exec := NewGraphForTest("spi-test", "wizard-test", graph, nil, deps)
	exec.graphState.RepoPath = dir

	step := StepConfig{Action: "wizard.run", Flow: "implement"}
	result := actionWizardRun(exec, "implement", step, exec.graphState)
	if result.Error == nil {
		t.Fatalf("actionWizardRun returned nil error, want wrapped publish error")
	}
	if !errors.Is(result.Error, publishErr) {
		t.Errorf("returned error %v does not wrap %v", result.Error, publishErr)
	}
}

// TestStepRoleAndPhase pins the (SpawnRole, flow, stepName) →
// (intent.Role, intent.Phase) mapping. The mapping must stay aligned
// with intent.Allowed; the (apprentice, review-fix) pair in particular
// is load-bearing — collapsing review-fix into PhaseFix would route
// cluster review-fix through the wrong pod-builder.
func TestStepRoleAndPhase(t *testing.T) {
	cases := []struct {
		name      string
		role      agent.SpawnRole
		flow      string
		stepName  string
		wantRole  intent.Role
		wantPhase intent.Phase
		wantOK    bool
	}{
		{name: "apprentice/implement flow", role: agent.RoleApprentice, flow: "implement", wantRole: intent.RoleApprentice, wantPhase: intent.PhaseImplement, wantOK: true},
		{name: "apprentice/review-fix flow", role: agent.RoleApprentice, flow: "review-fix", wantRole: intent.RoleApprentice, wantPhase: intent.PhaseReviewFix, wantOK: true},
		{name: "apprentice/fix flow", role: agent.RoleApprentice, flow: "fix", wantRole: intent.RoleApprentice, wantPhase: intent.PhaseFix, wantOK: true},
		{name: "apprentice/recovery-verify flow → implement", role: agent.RoleApprentice, flow: "recovery-verify", wantRole: intent.RoleApprentice, wantPhase: intent.PhaseImplement, wantOK: true},
		{name: "sage/sage-review flow", role: agent.RoleSage, flow: "sage-review", wantRole: intent.RoleSage, wantPhase: intent.PhaseReview, wantOK: true},
		{name: "sage/review flow", role: agent.RoleSage, flow: "review", wantRole: intent.RoleSage, wantPhase: intent.PhaseReview, wantOK: true},
		{name: "apprentice/no flow + step=implement", role: agent.RoleApprentice, flow: "", stepName: "implement", wantRole: intent.RoleApprentice, wantPhase: intent.PhaseImplement, wantOK: true},
		{name: "apprentice/unknown flow", role: agent.RoleApprentice, flow: "unknown-flow", wantOK: false},
		{name: "sage/unknown flow", role: agent.RoleSage, flow: "unknown-flow", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, p, ok := stepRoleAndPhase(tc.role, tc.flow, tc.stepName)
			if ok != tc.wantOK {
				t.Errorf("stepRoleAndPhase ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if r != tc.wantRole {
				t.Errorf("stepRoleAndPhase Role = %q, want %q", r, tc.wantRole)
			}
			if p != tc.wantPhase {
				t.Errorf("stepRoleAndPhase Phase = %q, want %q", p, tc.wantPhase)
			}
			// Cross-check: the (Role, Phase) pair must appear in
			// intent.Allowed. If this assertion fires, it means the
			// executor mapping has drifted from the .1 contract — the
			// operator side will reject the intent at Validate.
			if _, ok := intent.Allowed[r][p]; !ok {
				t.Errorf("(%q, %q) not present in intent.Allowed — executor mapping drifted from .1 contract", r, p)
			}
		})
	}
}

// TestDispatchDirectCore_ClusterNative_EmitsIntent_NoSpawn locks the
// dispatchDirectCore branch: in cluster-native the direct
// single-apprentice dispatch path (epic-default and any "direct"
// strategy in dispatch.children) emits one intent and never reaches
// Spawner.Spawn.
func TestDispatchDirectCore_ClusterNative_EmitsIntent_NoSpawn(t *testing.T) {
	withClusterAgentImage(t, "ghcr.io/example/spire-agent:test")

	disp := &fakeClusterChildDispatcher{}
	backend := &mockBackend{
		spawnFn: func(_ agent.SpawnConfig) (agent.Handle, error) {
			t.Fatal("Spawner.Spawn must not be called in cluster-native dispatchDirectCore")
			return nil, nil
		},
	}

	dir := t.TempDir()
	deps := &Deps{
		Spawner:                backend,
		ClusterChildDispatcher: disp,
		ActiveTowerConfig:      clusterTowerConfig(config.DeploymentModeClusterNative),
		ConfigDir:              func() (string, error) { return dir, nil },
		// recordAgentRun reaches into RecordAgentRun; nil-safe at the call site.
	}

	exec := NewForTest("spi-test", "wizard-test", &State{BeadID: "spi-test", AgentName: "wizard-test", RepoPath: dir, BaseBranch: "main"}, deps)

	if err := exec.dispatchDirectCore(nil, "claude-test", nil); err != nil {
		t.Fatalf("dispatchDirectCore returned error: %v", err)
	}

	if disp.callCount() != 1 {
		t.Fatalf("ClusterChildDispatcher.Dispatch called %d times, want 1", disp.callCount())
	}
	got := disp.lastCall()
	if got.Role != intent.RoleApprentice {
		t.Errorf("intent.Role = %q, want apprentice", got.Role)
	}
	if got.Phase != intent.PhaseImplement {
		t.Errorf("intent.Phase = %q, want %q", got.Phase, intent.PhaseImplement)
	}
	if got.TaskID != "spi-test" {
		t.Errorf("intent.TaskID = %q, want %q", got.TaskID, "spi-test")
	}
}

// withClusterAgentImage swaps the package-level clusterAgentImage var
// for the test scope so the executor's cluster-native dispatch sites
// get a deterministic Runtime.Image without touching process env.
func withClusterAgentImage(t *testing.T, image string) {
	t.Helper()
	old := clusterAgentImage
	clusterAgentImage = func() string { return image }
	t.Cleanup(func() { clusterAgentImage = old })
}
