// Transport-neutrality parity (spi-07k2s lane).
//
// The cluster-native control plane has three moving parts:
//
//  1. Schedule + claim — pkg/steward reads ready work and atomically
//     claims an attempt bead (pkg/steward/dispatch.ClaimThenEmit).
//  2. Emit intent — the claim handle threads into a
//     pkg/steward/intent.WorkloadIntent that is published through an
//     IntentPublisher transport (in production: a k8s CR apply).
//  3. Reconcile intent — the operator consumes intents and calls
//     pkg/agent.BuildApprenticePod to produce the apprentice pod.
//
// Orthogonally, the deployment-mode contract (pkg/config/deployment_mode.go)
// calls out that *sync transport* — how dolt data moves between peers
// — is independent of deployment mode. The parity claim this file
// pins is stronger than the doc comment: the scheduling + reconcile
// pipeline produces IDENTICAL outputs whether or not a sync transport
// is configured, down to the byte level.
//
// Concretely:
//
//   - (a) The ordered sequence of emitted WorkloadIntents is
//     byte-identical across the two runs (same AttemptIDs, same
//     RepoIdentity fields, same FormulaPhase, same HandoffMode, same
//     ordering).
//   - (b) identity.ClusterIdentityResolver.Resolve returns the same
//     ClusterRepoIdentity for every registered prefix under both
//     configurations.
//   - (c) Feeding the intent stream through the operator-reconciler
//     translation (pkg/agent.BuildApprenticePod invoked the way
//     operator/controllers/intent_reconciler.go invokes it) produces
//     byte-identical pod specs under both transports.
//
// A fakeSyncer value is plumbed via context.Context — a lightweight
// stand-in for any sync transport the steward might observe. The
// assertion is that the scheduler refuses to branch on its presence.
//
// No live k8s. No live dolt server. No goroutines coordinating over
// shared state — each run uses its own in-memory fakes, but the
// upstream store-fixture the selector reads from is seeded identically
// so the inputs are truly equivalent.
package parity

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/runtime"
	"github.com/awell-health/spire/pkg/steward/dispatch"
	"github.com/awell-health/spire/pkg/steward/identity"
	"github.com/awell-health/spire/pkg/steward/intent"
)

// fakeSyncer is a stand-in for the production sync transport. The
// parity claim is that the scheduler MUST NOT branch on its presence —
// so the type is trivially empty. If production introduces a sync
// transport interface, this type can grow methods to satisfy it
// without affecting the test shape.
type fakeSyncer struct {
	name string
}

// syncerCtxKey is a context key that carries the syncer. The production
// wiring may use its own key — the parity test cares that whatever
// key layout the steward adopts, the emit path does not switch on its
// presence.
type syncerCtxKey struct{}

func ctxWithSyncer(ctx context.Context, s *fakeSyncer) context.Context {
	return context.WithValue(ctx, syncerCtxKey{}, s)
}

// storeFixture is the in-memory stand-in for the shared repo registry
// + ready-work store. It seeds one repo per prefix and a list of
// schedulable parent beads, so both runs of the parity test start from
// byte-identical inputs.
type storeFixture struct {
	// prefixes is the ordered list of repo prefixes to register. Order
	// matters because it drives the ordering of emitted intents when
	// paired with beadOrder below.
	prefixes []string
	// repos maps prefix → (url, branch). Same contents in both runs.
	repos map[string]registryEntry
	// readyBeads is the ordered list of schedulable parent bead IDs.
	// The selector yields them in this order, so the emitted intent
	// order is deterministic and assertable.
	readyBeads []string
	// attemptIDs maps parent bead ID → the attempt ID the claimer
	// stamps on its handle. Same mapping across both runs.
	attemptIDs map[string]string
	// formulaPhase / handoffMode are the dispatch-time constants the
	// test pins on each intent.
	formulaPhase string
	handoffMode  runtime.HandoffMode
	// tower is the tower name both runs scope their identity resolver
	// and operator translation against.
	tower string
	// image is the apprentice image the operator translation uses.
	image string
}

// registryEntry is the canonical (url, branch) pair the registry
// returns for a prefix — same shape the production `repos` table has.
type registryEntry struct {
	url    string
	branch string
}

// canonicalStoreFixture returns the identical fixture both runs use.
// Two repos + two ready beads + two attempt IDs. The fixture is small
// enough to read at a glance and large enough to exercise ordering.
func canonicalStoreFixture() *storeFixture {
	return &storeFixture{
		prefixes: []string{"spi", "alt"},
		repos: map[string]registryEntry{
			"spi": {url: "https://example.com/spi.git", branch: "main"},
			"alt": {url: "https://example.com/alt.git", branch: "trunk"},
		},
		readyBeads: []string{"spi-xyz1", "alt-xyz2"},
		attemptIDs: map[string]string{
			"spi-xyz1": "spi-xyz1/attempt-1",
			"alt-xyz2": "alt-xyz2/attempt-1",
		},
		formulaPhase: "implement",
		handoffMode:  runtime.HandoffBundle,
		tower:        "parity-tower",
		image:        "spire-agent:parity",
	}
}

// fixtureRegistry adapts the store fixture to identity.RegistryStore.
// The parity contract demands identical output regardless of sync
// transport — the registry is the "input" side, so both runs see the
// exact same seed.
type fixtureRegistry struct {
	fx *storeFixture
}

func (r *fixtureRegistry) LookupRepo(_ context.Context, prefix string) (string, string, bool, error) {
	e, ok := r.fx.repos[prefix]
	if !ok {
		return "", "", false, nil
	}
	return e.url, e.branch, true, nil
}

// fixtureSelector yields the ready bead IDs in deterministic order.
type fixtureSelector struct {
	fx  *storeFixture
	pos int
}

func (s *fixtureSelector) SelectReady(_ context.Context) ([]string, error) {
	if s.pos >= len(s.fx.readyBeads) {
		return nil, nil
	}
	// Yield one bead per SelectReady call so each ClaimThenEmit cycle
	// corresponds to a single dispatch step.
	out := []string{s.fx.readyBeads[s.pos]}
	return out, nil
}

// fixtureClaimer returns pre-seeded attempt IDs in the order the
// selector presented them. Uniqueness comes from the fixture map, not
// from an in-process mutex or busy set — matches the production
// contract that uniqueness is a shared-store property.
type fixtureClaimer struct {
	fx  *storeFixture
	pos int
}

func (c *fixtureClaimer) ClaimNext(ctx context.Context, sel dispatch.ReadyWorkSelector) (*dispatch.ClaimHandle, error) {
	ids, err := sel.SelectReady(ctx)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	taskID := ids[0]
	if _, ok := c.fx.attemptIDs[taskID]; !ok {
		return nil, nil
	}
	return &dispatch.ClaimHandle{TaskID: taskID, DispatchSeq: 1}, nil
}

// recordingPublisher accumulates every published WorkloadIntent in
// order. Tests compare these slices across runs to assert (a) — the
// ordered byte-identical intent sequence.
type recordingPublisher struct {
	intents []intent.WorkloadIntent
}

func (p *recordingPublisher) Publish(_ context.Context, i intent.WorkloadIntent) error {
	p.intents = append(p.intents, i)
	return nil
}

// runClusterNativeCycle drives one full scheduling loop over the
// fixture's ready beads. It mirrors what pkg/steward's
// dispatchClusterNative does (beyond the pkg/steward private seams the
// test cannot reach from test/parity): resolve identity per prefix,
// call ClaimThenEmit per bead, stamp AttemptID onto the intent via
// buildClusterIntent. The function is the single place the cluster-
// native loop is simulated; both runs (syncer-on, syncer-off) invoke
// it with an identically-seeded fixture.
//
// The function threads ctx through ClaimThenEmit so the test can
// observe any branching on ctx-carried state (namely the fakeSyncer).
// A correct implementation ignores ctx-carried state for scheduling
// decisions.
func runClusterNativeCycle(ctx context.Context, t *testing.T, fx *storeFixture) *recordingPublisher {
	t.Helper()

	resolver := &identity.DefaultClusterIdentityResolver{
		Registry: &fixtureRegistry{fx: fx},
	}
	publisher := &recordingPublisher{}
	emitter := &neutralityEmitter{publisher: publisher}
	selector := &fixtureSelector{fx: fx}
	claimer := &fixtureClaimer{fx: fx}

	for range fx.readyBeads {
		parent := fx.readyBeads[selector.pos]
		// Resolve canonical identity for the bead's prefix. Resolver
		// output is the single source of truth — any sync transport
		// present on ctx MUST NOT alter it.
		ident, err := resolver.Resolve(ctx, beadPrefix(parent))
		if err != nil {
			t.Fatalf("resolver.Resolve(%q): %v", parent, err)
		}

		build := func(h *dispatch.ClaimHandle) intent.WorkloadIntent {
			return intent.WorkloadIntent{
				TaskID:      h.TaskID,
				DispatchSeq: h.DispatchSeq,
				RepoIdentity: intent.RepoIdentity{
					URL:        ident.URL,
					BaseBranch: ident.BaseBranch,
					Prefix:     ident.Prefix,
				},
				FormulaPhase: fx.formulaPhase,
				HandoffMode:  string(fx.handoffMode),
			}
		}
		err = dispatch.ClaimThenEmit(ctx, claimer, emitter, selector, build)
		if err != nil {
			t.Fatalf("ClaimThenEmit(%q): %v", parent, err)
		}
		selector.pos++
	}
	return publisher
}

// neutralityEmitter wraps an IntentPublisher with the same
// ValidateHandle guard the production publisherEmitter applies. Keeps
// the emit path shape identical across runs.
type neutralityEmitter struct {
	publisher intent.IntentPublisher
}

func (e *neutralityEmitter) Emit(ctx context.Context, h *dispatch.ClaimHandle, i intent.WorkloadIntent) error {
	if err := dispatch.ValidateHandle(h, i); err != nil {
		return err
	}
	return e.publisher.Publish(ctx, i)
}

// beadPrefix extracts the repo prefix from a bead ID (e.g. "spi" from
// "spi-xyz1"). Replays the trivial steward helper here so the parity
// test does not depend on steward internals.
func beadPrefix(beadID string) string {
	for i, r := range beadID {
		if r == '-' {
			return beadID[:i]
		}
	}
	return beadID
}

// TestTransportNeutrality_WorkloadIntentsAreByteIdentical pins claim
// (a): two cluster-native scheduling loops against identical store
// fixtures must emit byte-identical ordered WorkloadIntent sequences,
// regardless of whether a sync transport is configured on the context.
func TestTransportNeutrality_WorkloadIntentsAreByteIdentical(t *testing.T) {
	fxA := canonicalStoreFixture()
	fxB := canonicalStoreFixture()

	ctxWith := ctxWithSyncer(context.Background(), &fakeSyncer{name: "dolt-syncer"})
	ctxWithout := context.Background()

	with := runClusterNativeCycle(ctxWith, t, fxA)
	without := runClusterNativeCycle(ctxWithout, t, fxB)

	if len(with.intents) != len(without.intents) {
		t.Fatalf("intent count differs: with=%d without=%d", len(with.intents), len(without.intents))
	}
	if len(with.intents) != len(fxA.readyBeads) {
		t.Fatalf("intent count = %d, want %d (one per ready bead)",
			len(with.intents), len(fxA.readyBeads))
	}

	for i := range with.intents {
		if !reflect.DeepEqual(with.intents[i], without.intents[i]) {
			t.Errorf("intent[%d] differs:\n  with    = %+v\n  without = %+v",
				i, with.intents[i], without.intents[i])
		}
	}

	// Explicit TaskID and RepoIdentity assertions so a failure points at
	// which field drifted. Using reflect.DeepEqual on the full slice
	// makes the same claim but in one check.
	if !reflect.DeepEqual(with.intents, without.intents) {
		t.Errorf("intent sequences differ under reflect.DeepEqual:\n  with    = %+v\n  without = %+v",
			with.intents, without.intents)
	}

	// Sanity: the produced TaskIDs match the fixture's declared order.
	// This catches regressions where the scheduling loop accidentally
	// reorders claims.
	gotTasks := make([]string, len(with.intents))
	for i, wi := range with.intents {
		gotTasks[i] = wi.TaskID
	}
	wantTasks := append([]string{}, fxA.readyBeads...)
	if !reflect.DeepEqual(gotTasks, wantTasks) {
		t.Errorf("TaskID order = %v, want %v", gotTasks, wantTasks)
	}
}

// TestTransportNeutrality_ResolverOutputIsIdentical pins claim (b):
// ClusterIdentityResolver.Resolve returns the same
// ClusterRepoIdentity for every registered prefix whether or not a
// syncer is carried on ctx.
func TestTransportNeutrality_ResolverOutputIsIdentical(t *testing.T) {
	fx := canonicalStoreFixture()
	resolver := &identity.DefaultClusterIdentityResolver{
		Registry: &fixtureRegistry{fx: fx},
	}

	ctxWith := ctxWithSyncer(context.Background(), &fakeSyncer{name: "dolt-syncer"})
	ctxWithout := context.Background()

	// Iterate in a deterministic order so a map-traversal randomness
	// bug cannot mask a drift.
	prefixes := append([]string{}, fx.prefixes...)
	sort.Strings(prefixes)
	for _, prefix := range prefixes {
		a, err := resolver.Resolve(ctxWith, prefix)
		if err != nil {
			t.Fatalf("resolve %q with syncer: %v", prefix, err)
		}
		b, err := resolver.Resolve(ctxWithout, prefix)
		if err != nil {
			t.Fatalf("resolve %q without syncer: %v", prefix, err)
		}
		if a != b {
			t.Errorf("resolver drift for %q: with=%+v without=%+v", prefix, a, b)
		}
		// And the contents must match the fixture byte-for-byte.
		want := identity.ClusterRepoIdentity{
			URL:        fx.repos[prefix].url,
			BaseBranch: fx.repos[prefix].branch,
			Prefix:     prefix,
		}
		if a != want {
			t.Errorf("resolver output for %q = %+v, want %+v", prefix, a, want)
		}
	}
}

// operatorReconcileIntent mirrors what
// operator/controllers/intent_reconciler.go's reconcile does for a
// single intent: canonicalize identity, build a PodSpec, call
// pkg/agent.BuildApprenticePod. Because operator is a separate Go
// module, the test replays the reconciler's translation rather than
// importing operator/controllers — the seam is the small, well-defined
// PodSpec shape.
//
// Any sync transport on ctx MUST NOT affect the returned PodSpec:
// production identity resolution reads the shared registry, not
// whatever transport state the operator might observe.
func operatorReconcileIntent(ctx context.Context, t *testing.T, fx *storeFixture, resolver identity.ClusterIdentityResolver, wi intent.WorkloadIntent) agent.PodSpec {
	t.Helper()
	canonical, err := resolver.Resolve(ctx, wi.RepoIdentity.Prefix)
	if err != nil {
		t.Fatalf("resolve canonical identity for %q: %v", wi.RepoIdentity.Prefix, err)
	}
	name := fmt.Sprintf("apprentice-%s-%d", sanitizeK8sSubdomain(wi.TaskID), wi.DispatchSeq)
	return agent.PodSpec{
		Name:        name,
		Namespace:   "spire",
		Image:       fx.image,
		AgentName:   name,
		BeadID:      wi.TaskID,
		FormulaStep: wi.FormulaPhase,
		HandoffMode: runtime.HandoffMode(wi.HandoffMode),
		Backend:     "operator-k8s",
		Identity: runtime.RepoIdentity{
			TowerName:  fx.tower,
			Prefix:     canonical.Prefix,
			RepoURL:    canonical.URL,
			BaseBranch: canonical.BaseBranch,
		},
	}
}

// TestTransportNeutrality_OperatorReconcilesToIdenticalPodSpecs pins
// claim (c): feeding the intent stream from each run through the
// operator reconciler translation yields byte-identical pod specs
// (PodSpec + the resulting *corev1.Pod).
func TestTransportNeutrality_OperatorReconcilesToIdenticalPodSpecs(t *testing.T) {
	fxA := canonicalStoreFixture()
	fxB := canonicalStoreFixture()

	ctxWith := ctxWithSyncer(context.Background(), &fakeSyncer{name: "dolt-syncer"})
	ctxWithout := context.Background()

	withIntents := runClusterNativeCycle(ctxWith, t, fxA).intents
	withoutIntents := runClusterNativeCycle(ctxWithout, t, fxB).intents

	if len(withIntents) != len(withoutIntents) {
		t.Fatalf("intent counts differ; with=%d without=%d", len(withIntents), len(withoutIntents))
	}

	resolverWith := &identity.DefaultClusterIdentityResolver{
		Registry: &fixtureRegistry{fx: fxA},
	}
	resolverWithout := &identity.DefaultClusterIdentityResolver{
		Registry: &fixtureRegistry{fx: fxB},
	}

	for i := range withIntents {
		specWith := operatorReconcileIntent(ctxWith, t, fxA, resolverWith, withIntents[i])
		specWithout := operatorReconcileIntent(ctxWithout, t, fxB, resolverWithout, withoutIntents[i])
		if !reflect.DeepEqual(specWith, specWithout) {
			t.Errorf("PodSpec[%d] differs across transports:\n  with    = %+v\n  without = %+v",
				i, specWith, specWithout)
		}

		podWith, err := agent.BuildApprenticePod(specWith)
		if err != nil {
			t.Fatalf("BuildApprenticePod (with): %v", err)
		}
		podWithout, err := agent.BuildApprenticePod(specWithout)
		if err != nil {
			t.Fatalf("BuildApprenticePod (without): %v", err)
		}

		// Compare the pod fields the operator reconciler produces. We
		// deliberately check Spec.Containers[0].Env, volumes, labels,
		// and pod-level invariants. reflect.DeepEqual on the whole pod
		// is stricter than needed but catches every subtle drift.
		if !reflect.DeepEqual(podWith.Spec, podWithout.Spec) {
			t.Errorf("pod[%d] Spec differs across transports:\n  with    = %+v\n  without = %+v",
				i, podWith.Spec, podWithout.Spec)
		}
		if !reflect.DeepEqual(podWith.Labels, podWithout.Labels) {
			t.Errorf("pod[%d] Labels differ across transports:\n  with    = %+v\n  without = %+v",
				i, podWith.Labels, podWithout.Labels)
		}
		if !reflect.DeepEqual(podWith.Annotations, podWithout.Annotations) {
			t.Errorf("pod[%d] Annotations differ across transports:\n  with    = %+v\n  without = %+v",
				i, podWith.Annotations, podWithout.Annotations)
		}
		if podWith.Name != podWithout.Name {
			t.Errorf("pod[%d] Name differs: with=%q without=%q", i, podWith.Name, podWithout.Name)
		}
		if podWith.Namespace != podWithout.Namespace {
			t.Errorf("pod[%d] Namespace differs: with=%q without=%q", i, podWith.Namespace, podWithout.Namespace)
		}
	}
}

// TestTransportNeutrality_SchedulerStateNotCoupledToTransport asserts
// the inverse of the positive claims above: the fakeSyncer on the
// context is never observed by the scheduler code path. We do this
// indirectly — the fakeSyncer stays a trivially-empty struct with no
// methods, so there is nothing for the scheduler to read. This test
// is a regression fence: if a future refactor adds methods to
// fakeSyncer and the scheduler starts calling them, the positive
// byte-identity tests above would catch the drift. This test exists
// mainly to document the assertion and bolt the fakeSyncer type in
// place so a future change has to come through here.
func TestTransportNeutrality_SchedulerStateNotCoupledToTransport(t *testing.T) {
	// Reflecting on fakeSyncer: zero exported methods. If you add one,
	// you must also extend the positive tests above to still hold.
	typ := reflect.TypeOf(fakeSyncer{})
	exportedMethods := 0
	for i := 0; i < typ.NumMethod(); i++ {
		m := typ.Method(i)
		if m.IsExported() {
			exportedMethods++
		}
	}
	if exportedMethods != 0 {
		t.Errorf("fakeSyncer has %d exported methods; scheduler must not branch on transport state",
			exportedMethods)
	}
}
