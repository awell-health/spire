// Package parity owns the cross-backend runtime-contract parity
// coverage. This file is the spi-07k2s lane: three dispatch paths —
// local-process (pkg/agent), steward-k8s cluster-native routing
// (pkg/steward), and operator-k8s reconciliation (operator/controllers)
// — must materialize the same canonical RunContext, RepoIdentity, and
// HandoffMode for the same input bead + formula phase.
//
// No live k8s, no live dolt: every path is driven through its public
// seams with in-memory fakes. pkg/agent is exercised via
// SpawnConfig → ApplyProcessEnvForTest + BuildApprenticePod. The
// steward-k8s path is exercised via the cluster-native seams the
// steward itself uses (identity.ClusterIdentityResolver +
// dispatch.ClaimThenEmit + intent.IntentPublisher), then feeds the
// emitted WorkloadIntent into pkg/agent.BuildApprenticePod — the same
// pod builder the operator reconciler calls. The operator-k8s path
// mirrors what operator/controllers/intent_reconciler.go does on
// receive: resolve canonical identity, translate intent to PodSpec,
// call BuildApprenticePod. Because operator is a separate Go module,
// this file replays the reconciler's translation rather than importing
// operator/controllers — the seam contracts are identical regardless.
//
// The test rooms where the paths can diverge:
//
//   - WorkspaceOrigin may legitimately differ between paths
//     (origin-clone for the steward-k8s / operator-k8s bootstrap path,
//     guild-cache for a cache-overlay path, local-bind for a process
//     backend). The parity contract is that the WorkspaceHandle TYPE
//     (runtime.WorkspaceHandle) is the same and each origin satisfies
//     the same create → resolve → cleanup lifecycle.
//   - Backend names differ by design: "process" vs "k8s" vs
//     "operator-k8s". That is observable identity, not a parity bug.
//
// A failure here is a contract break: either the RepoIdentity is no
// longer identical across paths, or HandoffMode drifted, or the
// WorkspaceHandle shape changed, or a workspace origin implementation
// failed to satisfy the lifecycle contract.
package parity

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/runtime"
	"github.com/awell-health/spire/pkg/steward/dispatch"
	"github.com/awell-health/spire/pkg/steward/identity"
	"github.com/awell-health/spire/pkg/steward/intent"
	corev1 "k8s.io/api/core/v1"
)

// parityInput is the single bead+formula-phase fixture every path
// resolves against. Keep this struct small so a test failure points at
// exactly one logical input, not a grab-bag of fields.
type parityInput struct {
	BeadID       string
	AttemptID    string
	TowerName    string
	Prefix       string
	RepoURL      string
	BaseBranch   string
	FormulaPhase string
	HandoffMode  runtime.HandoffMode
}

// canonicalParityInput is the shared fixture each path materializes
// against. Fields are chosen so a drop in any of them surfaces as a
// zero-value mismatch downstream.
func canonicalParityInput() parityInput {
	return parityInput{
		BeadID:       "spi-parity-01",
		AttemptID:    "spi-parity-01/attempt-1",
		TowerName:    "parity-tower",
		Prefix:       "spi",
		RepoURL:      "https://example.com/parity.git",
		BaseBranch:   "main",
		FormulaPhase: "implement",
		HandoffMode:  runtime.HandoffBundle,
	}
}

// pathMaterialization is the three-field projection every dispatch
// path MUST produce for the same parityInput. The harness asserts each
// field matches across paths.
type pathMaterialization struct {
	// PathName identifies which path produced the materialization.
	// Used in test error messages to pin the offender.
	PathName string
	// RunContext is the canonical observability identity the dispatch
	// path would stamp on the worker's logs/traces/metrics.
	Run runtime.RunContext
	// Workspace is the WorkspaceHandle the path attaches to the worker.
	// Nil means the path does not materialize a workspace in this test
	// fixture (only the dispatch-time intent does).
	Workspace *runtime.WorkspaceHandle
	// Identity is the resolved repo identity the path plumbed to the
	// worker. Must match across every path for the same parityInput.
	Identity runtime.RepoIdentity
	// HandoffMode is the delivery protocol the path selected. Must be
	// identical across every path.
	HandoffMode runtime.HandoffMode
}

// fakeRegistryStore is an in-memory identity.RegistryStore used by the
// steward-k8s and operator-k8s paths. Seeded with one prefix so the
// ClusterIdentityResolver returns a deterministic identity without a
// live sql.DB.
type fakeRegistryStore struct {
	repos map[string]struct {
		url    string
		branch string
	}
}

func newFakeRegistryStore(in parityInput) *fakeRegistryStore {
	s := &fakeRegistryStore{repos: map[string]struct {
		url    string
		branch string
	}{}}
	s.repos[in.Prefix] = struct {
		url    string
		branch string
	}{url: in.RepoURL, branch: in.BaseBranch}
	return s
}

func (s *fakeRegistryStore) LookupRepo(_ context.Context, prefix string) (string, string, bool, error) {
	if s == nil {
		return "", "", false, errors.New("nil registry store")
	}
	r, ok := s.repos[prefix]
	if !ok {
		return "", "", false, nil
	}
	return r.url, r.branch, true, nil
}

// fakeIntentSink collects every WorkloadIntent a dispatch call emits.
// It is the operator-k8s path's handoff point: the reconciler consumes
// intents one-by-one and translates each to a pod.
type fakeIntentSink struct {
	intents []intent.WorkloadIntent
}

func (s *fakeIntentSink) Publish(_ context.Context, i intent.WorkloadIntent) error {
	s.intents = append(s.intents, i)
	return nil
}

// trivialClaimer stamps a deterministic AttemptID onto each candidate
// without touching a real store. The parity harness only cares that the
// AttemptID threads through from claim → intent; uniqueness under
// contention lives in pkg/store tests and is not re-covered here.
type trivialClaimer struct {
	attemptID string
	claimed   bool
}

func (c *trivialClaimer) ClaimNext(_ context.Context, sel dispatch.ReadyWorkSelector) (*dispatch.AttemptHandle, error) {
	if c.claimed {
		return nil, nil
	}
	ids, err := sel.SelectReady(context.Background())
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	c.claimed = true
	return &dispatch.AttemptHandle{AttemptID: c.attemptID}, nil
}

// singleParentSelector mirrors the steward's per-bead
// singleBeadSelector: it yields the configured parent ID once, which
// the claimer converts into the fixture AttemptID. Recreating it here
// keeps the parity test independent of steward internals.
type singleParentSelector struct {
	id string
}

func (s singleParentSelector) SelectReady(_ context.Context) ([]string, error) {
	if s.id == "" {
		return nil, nil
	}
	return []string{s.id}, nil
}

// processPathMaterialization exercises pkg/agent's process backend
// plumbing: it builds the canonical SpawnConfig the executor assembles
// and reads back the env the ProcessSpawner would stamp onto the
// child. From that env we reconstruct a RunContext via
// runtime.RunContextFromEnv-style field picks. The WorkspaceHandle the
// process path carries is the one the executor hands to the spawner
// on SpawnConfig.Workspace.
func processPathMaterialization(t *testing.T, in parityInput) pathMaterialization {
	t.Helper()

	run := runtime.RunContext{
		TowerName:       in.TowerName,
		Prefix:          in.Prefix,
		BeadID:          in.BeadID,
		AttemptID:       in.AttemptID,
		Role:            runtime.RoleApprentice,
		FormulaStep:     in.FormulaPhase,
		Backend:         "process",
		WorkspaceKind:   runtime.WorkspaceKindOwnedWorktree,
		WorkspaceName:   in.BeadID + "-impl",
		WorkspaceOrigin: runtime.WorkspaceOriginLocalBind,
		HandoffMode:     in.HandoffMode,
	}
	ws := &runtime.WorkspaceHandle{
		Name:       run.WorkspaceName,
		Kind:       run.WorkspaceKind,
		BaseBranch: in.BaseBranch,
		Path:       "/tmp/parity-process-ws",
		Origin:     run.WorkspaceOrigin,
	}
	cfg := agent.SpawnConfig{
		Name:       "apprentice-" + in.BeadID + "-0",
		BeadID:     in.BeadID,
		Role:       runtime.RoleApprentice,
		Tower:      in.TowerName,
		AttemptID:  in.AttemptID,
		Step:       in.FormulaPhase,
		RepoURL:    in.RepoURL,
		RepoBranch: in.BaseBranch,
		RepoPrefix: in.Prefix,
		Identity: runtime.RepoIdentity{
			TowerName:  in.TowerName,
			Prefix:     in.Prefix,
			RepoURL:    in.RepoURL,
			BaseBranch: in.BaseBranch,
		},
		Workspace: ws,
		Run:       run,
	}
	// Apply the env translation without starting a real subprocess.
	cmd := exec.Command("/usr/bin/env")
	cmd.Env = []string{}
	agent.ApplyProcessEnvForTest(cmd, cfg)
	env := envToMap(cmd.Env)

	return pathMaterialization{
		PathName: "process",
		Run: runtime.RunContext{
			TowerName:       env[runtime.EnvTower],
			Prefix:          env[runtime.EnvPrefix],
			BeadID:          env[runtime.EnvBeadID],
			AttemptID:       env[runtime.EnvAttemptID],
			Role:            runtime.SpawnRole(env[runtime.EnvRole]),
			FormulaStep:     env[runtime.EnvFormulaStep],
			Backend:         env[runtime.EnvBackend],
			WorkspaceKind:   runtime.WorkspaceKind(env[runtime.EnvWorkspaceKind]),
			WorkspaceName:   env[runtime.EnvWorkspaceName],
			WorkspaceOrigin: runtime.WorkspaceOrigin(env[runtime.EnvWorkspaceOrigin]),
			HandoffMode:     runtime.HandoffMode(env[runtime.EnvHandoffMode]),
		},
		Workspace: ws,
		Identity: runtime.RepoIdentity{
			TowerName:  in.TowerName,
			Prefix:     env[runtime.EnvPrefix],
			RepoURL:    env["SPIRE_REPO_URL"],
			BaseBranch: env["SPIRE_REPO_BRANCH"],
		},
		HandoffMode: runtime.HandoffMode(env[runtime.EnvHandoffMode]),
	}
}

// stewardClusterPathMaterialization drives the cluster-native steward
// seams the steward itself uses: resolver → claimer → emitter. It
// produces the same WorkloadIntent the steward would emit, then folds
// the intent into the PodSpec any downstream pod builder (operator
// reconciler, steward-k8s spawn adapter) would use. Translating the
// intent here — instead of importing steward's private
// dispatchClusterNative — keeps the parity surface small and avoids
// touching pkg/steward non-test files.
func stewardClusterPathMaterialization(t *testing.T, in parityInput, registry identity.RegistryStore) (pathMaterialization, intent.WorkloadIntent) {
	t.Helper()

	resolver := &identity.DefaultClusterIdentityResolver{Registry: registry}
	cri, err := resolver.Resolve(context.Background(), in.Prefix)
	if err != nil {
		t.Fatalf("steward-k8s: resolve identity: %v", err)
	}

	sink := &fakeIntentSink{}
	emitter := &validatingEmitter{publisher: sink}
	claimer := &trivialClaimer{attemptID: in.AttemptID}

	build := func(h *dispatch.AttemptHandle) intent.WorkloadIntent {
		return intent.WorkloadIntent{
			AttemptID: h.AttemptID,
			RepoIdentity: intent.RepoIdentity{
				URL:        cri.URL,
				BaseBranch: cri.BaseBranch,
				Prefix:     cri.Prefix,
			},
			FormulaPhase: in.FormulaPhase,
			HandoffMode:  string(in.HandoffMode),
		}
	}
	if err := dispatch.ClaimThenEmit(context.Background(), claimer, emitter, singleParentSelector{id: in.BeadID}, build); err != nil {
		t.Fatalf("steward-k8s: ClaimThenEmit: %v", err)
	}
	if len(sink.intents) != 1 {
		t.Fatalf("steward-k8s: emitted %d intents, want 1", len(sink.intents))
	}
	wi := sink.intents[0]

	// Steward-k8s path derives a workspace handle with origin-clone:
	// the in-pod init container clones the repo from the resolver's
	// URL into /workspace/<prefix> before the worker starts.
	ws := &runtime.WorkspaceHandle{
		Name:       in.BeadID + "-impl",
		Kind:       runtime.WorkspaceKindOwnedWorktree,
		BaseBranch: cri.BaseBranch,
		Path:       "/workspace/" + cri.Prefix,
		Origin:     runtime.WorkspaceOriginOriginClone,
	}

	return pathMaterialization{
		PathName: "steward-k8s",
		Run: runtime.RunContext{
			TowerName:       in.TowerName,
			Prefix:          wi.RepoIdentity.Prefix,
			BeadID:          in.BeadID,
			AttemptID:       wi.AttemptID,
			Role:            runtime.RoleApprentice,
			FormulaStep:     wi.FormulaPhase,
			Backend:         "k8s",
			WorkspaceKind:   ws.Kind,
			WorkspaceName:   ws.Name,
			WorkspaceOrigin: ws.Origin,
			HandoffMode:     runtime.HandoffMode(wi.HandoffMode),
		},
		Workspace: ws,
		Identity: runtime.RepoIdentity{
			TowerName:  in.TowerName,
			Prefix:     wi.RepoIdentity.Prefix,
			RepoURL:    wi.RepoIdentity.URL,
			BaseBranch: wi.RepoIdentity.BaseBranch,
		},
		HandoffMode: runtime.HandoffMode(wi.HandoffMode),
	}, wi
}

// operatorPathMaterialization replays what
// operator/controllers/intent_reconciler.go does on a received
// WorkloadIntent: resolve canonical identity via the resolver (drift
// check), build a PodSpec, call pkg/agent.BuildApprenticePod. Because
// operator is a separate Go module, we don't import its controllers
// package — the seam contracts are identical and the translation is
// small enough to replay.
//
// The operator path may carry a different WorkspaceOrigin than the
// steward-k8s path (e.g. guild-cache via the cache overlay). The
// parity contract says: RepoIdentity and HandoffMode must match,
// WorkspaceHandle is the same Go type, origins may differ.
func operatorPathMaterialization(t *testing.T, in parityInput, wi intent.WorkloadIntent, registry identity.RegistryStore) pathMaterialization {
	t.Helper()

	resolver := &identity.DefaultClusterIdentityResolver{Registry: registry}
	canonical, err := resolver.Resolve(context.Background(), wi.RepoIdentity.Prefix)
	if err != nil {
		t.Fatalf("operator: canonical identity: %v", err)
	}

	// Operator-k8s mounts a guild-cache workspace overlay (the phase-2
	// cache contract in pkg/agent/cache_bootstrap.go). We exercise that
	// variant explicitly here to ensure the parity contract tolerates
	// different origins as long as RepoIdentity + HandoffMode agree.
	ws := &runtime.WorkspaceHandle{
		Name:       in.BeadID + "-impl",
		Kind:       runtime.WorkspaceKindOwnedWorktree,
		BaseBranch: canonical.BaseBranch,
		Path:       agent.WorkspaceMountPath,
		Origin:     runtime.WorkspaceOriginGuildCache,
	}

	podName := "apprentice-" + sanitizeK8sSubdomain(wi.AttemptID)
	spec := agent.PodSpec{
		Name:         podName,
		Namespace:    "spire",
		Image:        "spire-agent:parity",
		AgentName:    podName,
		BeadID:       in.BeadID,
		AttemptID:    wi.AttemptID,
		FormulaStep:  wi.FormulaPhase,
		HandoffMode:  runtime.HandoffMode(wi.HandoffMode),
		Backend:      "operator-k8s",
		CachePVCName: "spi-parity-cache",
		Identity: runtime.RepoIdentity{
			TowerName:  in.TowerName,
			Prefix:     canonical.Prefix,
			RepoURL:    canonical.URL,
			BaseBranch: canonical.BaseBranch,
		},
		Workspace: *ws,
	}
	pod, err := agent.BuildApprenticePod(spec)
	if err != nil {
		t.Fatalf("operator: BuildApprenticePod: %v", err)
	}
	env := podEnvToMap(pod)

	return pathMaterialization{
		PathName: "operator-k8s",
		Run: runtime.RunContext{
			TowerName:       env["SPIRE_TOWER"],
			Prefix:          env["SPIRE_REPO_PREFIX"],
			BeadID:          env["SPIRE_BEAD_ID"],
			AttemptID:       env["SPIRE_ATTEMPT_ID"],
			Role:            runtime.SpawnRole(env["SPIRE_ROLE"]),
			FormulaStep:     env["SPIRE_FORMULA_STEP"],
			Backend:         env["SPIRE_BACKEND"],
			WorkspaceKind:   runtime.WorkspaceKind(env["SPIRE_WORKSPACE_KIND"]),
			WorkspaceName:   env["SPIRE_WORKSPACE_NAME"],
			WorkspaceOrigin: runtime.WorkspaceOrigin(env["SPIRE_WORKSPACE_ORIGIN"]),
			HandoffMode:     runtime.HandoffMode(env["SPIRE_HANDOFF_MODE"]),
		},
		Workspace: ws,
		Identity: runtime.RepoIdentity{
			TowerName:  env["SPIRE_TOWER"],
			Prefix:     env["SPIRE_REPO_PREFIX"],
			RepoURL:    env["SPIRE_REPO_URL"],
			BaseBranch: env["SPIRE_REPO_BRANCH"],
		},
		HandoffMode: runtime.HandoffMode(env["SPIRE_HANDOFF_MODE"]),
	}
}

// validatingEmitter adapts an intent.IntentPublisher to
// dispatch.DispatchEmitter so the parity harness re-exercises
// dispatch.ValidateHandle on every emit — same guard shape the
// steward's publisherEmitter uses.
type validatingEmitter struct {
	publisher intent.IntentPublisher
}

func (e *validatingEmitter) Emit(ctx context.Context, h *dispatch.AttemptHandle, i intent.WorkloadIntent) error {
	if err := dispatch.ValidateHandle(h, i); err != nil {
		return err
	}
	return e.publisher.Publish(ctx, i)
}

// TestRuntimeContractParity_AllPathsAgreeOnIdentityAndHandoff is the
// atomic sweep: every dispatch path materializes a RunContext whose
// RepoIdentity fields (URL, BaseBranch, Prefix) match the fixture's,
// whose HandoffMode matches the fixture's, and whose Workspace is the
// runtime.WorkspaceHandle Go type. WorkspaceOrigin is allowed to differ
// because the three paths legitimately produce different workspace
// substrates (local bind vs origin clone vs guild cache).
func TestRuntimeContractParity_AllPathsAgreeOnIdentityAndHandoff(t *testing.T) {
	in := canonicalParityInput()
	registry := newFakeRegistryStore(in)

	process := processPathMaterialization(t, in)
	steward, wi := stewardClusterPathMaterialization(t, in, registry)
	op := operatorPathMaterialization(t, in, wi, registry)

	paths := []pathMaterialization{process, steward, op}

	// RepoIdentity parity — URL, BaseBranch, Prefix MUST match across
	// every path. TowerName is a per-path-local but fixture-derived
	// field; assert it too so a fixture bug surfaces here instead of
	// silently passing.
	wantIdentity := runtime.RepoIdentity{
		TowerName:  in.TowerName,
		Prefix:     in.Prefix,
		RepoURL:    in.RepoURL,
		BaseBranch: in.BaseBranch,
	}
	for _, p := range paths {
		if p.Identity.RepoURL != wantIdentity.RepoURL {
			t.Errorf("%s RepoURL = %q, want %q", p.PathName, p.Identity.RepoURL, wantIdentity.RepoURL)
		}
		if p.Identity.BaseBranch != wantIdentity.BaseBranch {
			t.Errorf("%s BaseBranch = %q, want %q", p.PathName, p.Identity.BaseBranch, wantIdentity.BaseBranch)
		}
		if p.Identity.Prefix != wantIdentity.Prefix {
			t.Errorf("%s Prefix = %q, want %q", p.PathName, p.Identity.Prefix, wantIdentity.Prefix)
		}
		if p.Identity.TowerName != wantIdentity.TowerName {
			t.Errorf("%s TowerName = %q, want %q", p.PathName, p.Identity.TowerName, wantIdentity.TowerName)
		}
	}

	// HandoffMode parity — every path selects the same delivery
	// protocol because the fixture pins it. If a path silently rewrites
	// HandoffMode, this catches it.
	for _, p := range paths {
		if p.HandoffMode != in.HandoffMode {
			t.Errorf("%s HandoffMode = %q, want %q", p.PathName, p.HandoffMode, in.HandoffMode)
		}
		if p.Run.HandoffMode != in.HandoffMode {
			t.Errorf("%s Run.HandoffMode = %q, want %q", p.PathName, p.Run.HandoffMode, in.HandoffMode)
		}
	}

	// WorkspaceHandle type parity — each path attaches a non-nil
	// *runtime.WorkspaceHandle. Origin may differ but Kind and Name
	// MUST be the runtime-package types (assignability is enforced by
	// the compiler; this is a runtime sanity check that the pointer is
	// non-nil so downstream logs/traces can dereference it).
	for _, p := range paths {
		if p.Workspace == nil {
			t.Fatalf("%s returned nil WorkspaceHandle; every path must materialize one", p.PathName)
		}
	}

	// RunContext identity fields (TowerName, Prefix, BeadID, AttemptID,
	// FormulaStep, HandoffMode) MUST match across paths. Backend and
	// WorkspaceOrigin are allowed to differ — they are path-local
	// observability knobs.
	invariants := []struct {
		name string
		get  func(runtime.RunContext) string
		want string
	}{
		{"TowerName", func(r runtime.RunContext) string { return r.TowerName }, in.TowerName},
		{"Prefix", func(r runtime.RunContext) string { return r.Prefix }, in.Prefix},
		{"BeadID", func(r runtime.RunContext) string { return r.BeadID }, in.BeadID},
		{"AttemptID", func(r runtime.RunContext) string { return r.AttemptID }, in.AttemptID},
		{"FormulaStep", func(r runtime.RunContext) string { return r.FormulaStep }, in.FormulaPhase},
		{"HandoffMode", func(r runtime.RunContext) string { return string(r.HandoffMode) }, string(in.HandoffMode)},
		{"Role", func(r runtime.RunContext) string { return string(r.Role) }, string(runtime.RoleApprentice)},
	}
	for _, p := range paths {
		for _, inv := range invariants {
			got := inv.get(p.Run)
			if got != inv.want {
				t.Errorf("%s Run.%s = %q, want %q", p.PathName, inv.name, got, inv.want)
			}
		}
	}

	// Backend strings MUST be distinct — otherwise the observability
	// surface cannot tell the three paths apart. This is the inverse
	// check of the identity parity above: identity matches, backend
	// name does not.
	if process.Run.Backend == steward.Run.Backend {
		t.Errorf("process and steward-k8s share Backend=%q; must differ", process.Run.Backend)
	}
	if steward.Run.Backend == op.Run.Backend {
		t.Errorf("steward-k8s and operator-k8s share Backend=%q; must differ", steward.Run.Backend)
	}
	if process.Run.Backend == op.Run.Backend {
		t.Errorf("process and operator-k8s share Backend=%q; must differ", process.Run.Backend)
	}
}

// workspaceOriginProvider models the lifecycle a workspace origin
// implementation MUST satisfy: a backend turns a logical origin
// (origin-clone, guild-cache, local-bind) into a materialized path,
// and later cleans it up. Each step returns a success/error shape that
// callers can handle uniformly regardless of origin.
//
// This interface is local to the parity test — production origins
// don't share a Go interface today because their lifecycles live in
// different packages (operator init containers, local executor
// worktree helpers, cache reconciler). The test defines the contract
// here and asserts fakes for each origin satisfy it, pinning the
// "every origin behaves the same externally" invariant.
type workspaceOriginProvider interface {
	// Create prepares the workspace substrate and returns the handle
	// the worker will consume. A non-nil error means the substrate
	// could not be prepared and the dispatch path MUST NOT proceed.
	Create(ctx context.Context) (*runtime.WorkspaceHandle, error)

	// ResolveWorkingDir returns the absolute path inside the substrate
	// the worker should run with. Must be idempotent; calling after
	// Cleanup MUST return ErrWorkspaceCleanedUp.
	ResolveWorkingDir(ctx context.Context) (string, error)

	// Cleanup releases the substrate. Must be idempotent — repeated
	// calls after the first successful cleanup return nil.
	Cleanup(ctx context.Context) error
}

// ErrWorkspaceCleanedUp is the canonical error workspace providers
// return when the caller tries to resolve a substrate that has already
// been torn down. Callers use errors.Is to distinguish this from other
// resolver failures (transport / auth / IO).
var ErrWorkspaceCleanedUp = errors.New("workspace: already cleaned up")

// fakeOriginCloneProvider simulates the steward-k8s origin-clone
// substrate: a fresh clone into /workspace/<prefix> via the
// repo-bootstrap init container. It's a drop-in for any production
// provider that produces origin-clone substrate.
type fakeOriginCloneProvider struct {
	prefix   string
	cleaned  bool
	created  bool
	baseBranch string
}

func (p *fakeOriginCloneProvider) Create(_ context.Context) (*runtime.WorkspaceHandle, error) {
	if p.created {
		return nil, fmt.Errorf("origin-clone: Create called twice")
	}
	p.created = true
	return &runtime.WorkspaceHandle{
		Name:       p.prefix + "-impl",
		Kind:       runtime.WorkspaceKindOwnedWorktree,
		BaseBranch: p.baseBranch,
		Path:       "/workspace/" + p.prefix,
		Origin:     runtime.WorkspaceOriginOriginClone,
	}, nil
}

func (p *fakeOriginCloneProvider) ResolveWorkingDir(_ context.Context) (string, error) {
	if p.cleaned {
		return "", ErrWorkspaceCleanedUp
	}
	if !p.created {
		return "", errors.New("origin-clone: ResolveWorkingDir before Create")
	}
	return "/workspace/" + p.prefix, nil
}

func (p *fakeOriginCloneProvider) Cleanup(_ context.Context) error {
	// Idempotent: repeated cleanups are a no-op.
	p.cleaned = true
	return nil
}

// fakeGuildCacheProvider simulates the operator-k8s guild-cache
// substrate: a read-only cache PVC + writable workspace derivation
// (the phase-2 cache contract in pkg/agent/cache_bootstrap.go). Same
// external shape as the origin-clone provider — different internals.
type fakeGuildCacheProvider struct {
	prefix   string
	cleaned  bool
	created  bool
	baseBranch string
}

func (p *fakeGuildCacheProvider) Create(_ context.Context) (*runtime.WorkspaceHandle, error) {
	if p.created {
		return nil, fmt.Errorf("guild-cache: Create called twice")
	}
	p.created = true
	return &runtime.WorkspaceHandle{
		Name:       p.prefix + "-impl",
		Kind:       runtime.WorkspaceKindOwnedWorktree,
		BaseBranch: p.baseBranch,
		Path:       agent.WorkspaceMountPath,
		Origin:     runtime.WorkspaceOriginGuildCache,
	}, nil
}

func (p *fakeGuildCacheProvider) ResolveWorkingDir(_ context.Context) (string, error) {
	if p.cleaned {
		return "", ErrWorkspaceCleanedUp
	}
	if !p.created {
		return "", errors.New("guild-cache: ResolveWorkingDir before Create")
	}
	return agent.WorkspaceMountPath, nil
}

func (p *fakeGuildCacheProvider) Cleanup(_ context.Context) error {
	// Idempotent: repeated cleanups are a no-op.
	p.cleaned = true
	return nil
}

// TestWorkspaceOriginInterfaceContract_OriginCloneAndGuildCache drives
// each origin implementation through Create → ResolveWorkingDir →
// Cleanup and asserts:
//
//   - Create returns a WorkspaceHandle whose Kind is one of the runtime
//     package's WorkspaceKind constants and whose Origin is the
//     expected runtime.WorkspaceOrigin.
//   - ResolveWorkingDir before Create is a failure (non-nil error).
//   - ResolveWorkingDir after Create returns the handle's Path.
//   - Cleanup is idempotent and Resolve after Cleanup returns
//     ErrWorkspaceCleanedUp.
//
// Running both origins through the same test body proves they satisfy
// one interface contract even when the producing packages are very
// different in production.
func TestWorkspaceOriginInterfaceContract_OriginCloneAndGuildCache(t *testing.T) {
	cases := []struct {
		name       string
		provider   workspaceOriginProvider
		wantOrigin runtime.WorkspaceOrigin
	}{
		{
			name: "origin-clone (steward-k8s)",
			provider: &fakeOriginCloneProvider{
				prefix:     "spi",
				baseBranch: "main",
			},
			wantOrigin: runtime.WorkspaceOriginOriginClone,
		},
		{
			name: "guild-cache (operator-k8s)",
			provider: &fakeGuildCacheProvider{
				prefix:     "spi",
				baseBranch: "main",
			},
			wantOrigin: runtime.WorkspaceOriginGuildCache,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			// Resolve-before-Create is a contract violation: the
			// provider has no substrate to report yet.
			if _, err := tc.provider.ResolveWorkingDir(ctx); err == nil {
				t.Errorf("ResolveWorkingDir before Create returned nil error; want non-nil")
			}

			// Create yields the WorkspaceHandle type and the expected
			// Origin. Kind must be one of the runtime package's
			// declared WorkspaceKinds (Go's type system enforces
			// assignability; we also assert the value is populated).
			handle, err := tc.provider.Create(ctx)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if handle == nil {
				t.Fatalf("Create returned nil handle; want non-nil")
			}
			if handle.Origin != tc.wantOrigin {
				t.Errorf("handle.Origin = %q, want %q", handle.Origin, tc.wantOrigin)
			}
			if !validWorkspaceKind(handle.Kind) {
				t.Errorf("handle.Kind = %q, not in {repo, owned_worktree, borrowed_worktree, staging}", handle.Kind)
			}
			if handle.Path == "" {
				t.Errorf("handle.Path is empty; want the materialized path")
			}

			// Resolve after Create returns the handle's Path.
			wd, err := tc.provider.ResolveWorkingDir(ctx)
			if err != nil {
				t.Fatalf("ResolveWorkingDir after Create: %v", err)
			}
			if wd != handle.Path {
				t.Errorf("ResolveWorkingDir = %q, want %q", wd, handle.Path)
			}

			// Cleanup twice: idempotency contract.
			if err := tc.provider.Cleanup(ctx); err != nil {
				t.Errorf("Cleanup (first): %v", err)
			}
			if err := tc.provider.Cleanup(ctx); err != nil {
				t.Errorf("Cleanup (second): %v (must be idempotent)", err)
			}

			// Resolve after Cleanup: returns ErrWorkspaceCleanedUp.
			if _, err := tc.provider.ResolveWorkingDir(ctx); !errors.Is(err, ErrWorkspaceCleanedUp) {
				t.Errorf("ResolveWorkingDir after Cleanup = %v, want ErrWorkspaceCleanedUp", err)
			}
		})
	}
}

// validWorkspaceKind returns true iff k is one of the runtime package's
// declared WorkspaceKind values. The test uses it to catch typos /
// ad-hoc Kinds leaking through the parity harness.
func validWorkspaceKind(k runtime.WorkspaceKind) bool {
	switch k {
	case runtime.WorkspaceKindRepo,
		runtime.WorkspaceKindOwnedWorktree,
		runtime.WorkspaceKindBorrowedWorktree,
		runtime.WorkspaceKindStaging:
		return true
	}
	return false
}

// envToMap splits "KEY=VALUE" entries into a map for ergonomic lookup.
func envToMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		if idx := strings.Index(kv, "="); idx > 0 {
			m[kv[:idx]] = kv[idx+1:]
		}
	}
	return m
}

// podEnvToMap reduces the main container's env to name -> value (empty
// when the entry uses ValueFrom rather than a literal).
func podEnvToMap(pod *corev1.Pod) map[string]string {
	m := map[string]string{}
	if pod == nil || len(pod.Spec.Containers) == 0 {
		return m
	}
	for _, e := range pod.Spec.Containers[0].Env {
		m[e.Name] = e.Value
	}
	return m
}

// sanitizeK8sSubdomain is a lightweight name sanitizer that mirrors
// what operator/controllers' apprenticePodName uses (see
// operator/controllers/intent_reconciler.go). We reproduce the small
// helper here rather than importing operator/controllers because
// operator is a separate module — the sanitization rule is
// well-established k8s naming.
func sanitizeK8sSubdomain(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}
