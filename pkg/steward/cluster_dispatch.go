package steward

// Cluster-native dispatch — the second of the three deployment-mode paths
// the steward branches on. This file MUST stay free of the local-native
// concerns:
//
//   - It MUST NOT read LocalBindings.State, LocalBindings.LocalPath, or
//     cfg.Instances. Cluster repo identity comes only from
//     identity.ClusterIdentityResolver, backed by the shared tower repo
//     registry.
//   - It MUST NOT call backend.Spawn. Cluster-native scheduling does not
//     create pods; it emits intent.WorkloadIntent values and the operator
//     reconciles them into pods.
//   - It MUST go through dispatch.ClaimThenEmit for every dispatch. The
//     attempt bead row created by the claim is the canonical ownership
//     seam — the in-process busy map and per-bead mutex are explicitly
//     not allowed as substitutes.
//
// The file deliberately contains no k8s.io imports. Talking to a cluster
// is the IntentPublisher's concern, plumbed in from cmd/spire wiring.

import (
	"context"
	"errors"
	"log"

	"github.com/awell-health/spire/pkg/runtime"
	"github.com/awell-health/spire/pkg/steward/dispatch"
	"github.com/awell-health/spire/pkg/steward/identity"
	"github.com/awell-health/spire/pkg/steward/intent"
	"github.com/awell-health/spire/pkg/store"
)

// ClusterDispatchConfig bundles the three cluster-native seams the
// steward consumes when EffectiveDeploymentMode is cluster-native:
// repo-identity resolution, attempt claiming, and intent publishing.
//
// The fields hold interfaces so cmd/spire can wire production
// implementations (SQL-backed registry resolver, store-backed claimer,
// CR-apply publisher) and tests can wire fakes. A nil
// ClusterDispatchConfig — or any nil field within it — disables
// cluster-native dispatch and the steward logs and skips. The local
// dispatch path is unaffected.
type ClusterDispatchConfig struct {
	// Resolver maps a repo prefix to its canonical ClusterRepoIdentity
	// using the shared tower registry. Required when DeploymentMode is
	// cluster-native.
	Resolver identity.ClusterIdentityResolver

	// Claimer atomically opens an attempt bead for the candidate the
	// selector offers. Production callers wire dispatch.StoreClaimer;
	// tests wire fakes. Required.
	Claimer dispatch.AttemptClaimer

	// Publisher delivers each emitted WorkloadIntent to the reconciler
	// transport (a Kubernetes CR apply, in production). Required.
	Publisher intent.IntentPublisher

	// FormulaPhase, when non-empty, overrides the default "implement"
	// phase the steward stamps on each emitted intent. Tests use this
	// to pin the value; production normally leaves it empty.
	FormulaPhase string

	// HandoffMode, when non-empty, overrides the default
	// runtime.HandoffBundle the steward stamps on each emitted intent.
	HandoffMode string
}

// dispatchClusterNative emits a WorkloadIntent for each schedulable
// bead in this cycle, threading the AttemptID from the claimed attempt
// bead through dispatch.ClaimThenEmit. It returns the number of
// successful emits.
//
// The function is the cluster-native counterpart to the local-native
// backend.Spawn loop in TowerCycle. It runs only when the tower's
// EffectiveDeploymentMode is cluster-native and the StewardConfig
// carries a fully populated ClusterDispatchConfig.
//
// Per-bead failures are logged and skipped — one bad prefix or a stale
// attempt does not stop the rest of the cycle. The function does not
// touch local backend state, LocalBindings, or cfg.Instances.
func dispatchClusterNative(
	ctx context.Context,
	logPrefix string,
	schedulable []store.Bead,
	cfg StewardConfig,
) int {
	cd := cfg.ClusterDispatch
	if cd == nil {
		log.Printf("[steward] %scluster-native: ClusterDispatch is not configured — skipping dispatch", logPrefix)
		return 0
	}
	if cd.Resolver == nil || cd.Claimer == nil || cd.Publisher == nil {
		log.Printf("[steward] %scluster-native: incomplete ClusterDispatch (Resolver+Claimer+Publisher all required) — skipping dispatch", logPrefix)
		return 0
	}

	emitter := publisherEmitter{publisher: cd.Publisher}
	handoffMode := cd.HandoffMode
	if handoffMode == "" {
		handoffMode = string(runtime.HandoffBundle)
	}

	emitted := 0
	for _, bead := range schedulable {
		beadID := bead.ID
		repoPrefix := beadRepoPrefix(beadID)

		formulaPhase := beadDispatchPhase(cd.FormulaPhase, bead.Type)

		ident, err := cd.Resolver.Resolve(ctx, repoPrefix)
		if err != nil {
			log.Printf("[steward] %scluster-native: resolve repo %q for %s: %s", logPrefix, repoPrefix, beadID, err)
			continue
		}

		if cfg.DryRun {
			log.Printf("[steward] %s[dry-run] cluster-native: would emit WorkloadIntent for %s (prefix=%s)", logPrefix, beadID, repoPrefix)
			emitted++
			continue
		}

		buildIntent := buildClusterIntent(ident, formulaPhase, handoffMode)
		err = dispatch.ClaimThenEmit(ctx, cd.Claimer, emitter, singleBeadSelector{id: beadID}, buildIntent)
		if err != nil {
			if errors.Is(err, dispatch.ErrNoClaimedAttempt) {
				log.Printf("[steward] %scluster-native: dispatch %s: emit refused without claim (programmer error)", logPrefix, beadID)
			} else {
				log.Printf("[steward] %scluster-native: dispatch %s: %s", logPrefix, beadID, err)
			}
			continue
		}
		emitted++
	}

	if emitted > 0 {
		log.Printf("[steward] %scluster-native: emitted %d intent(s)", logPrefix, emitted)
	}

	return emitted
}

// beadDispatchPhase decides the FormulaPhase string the steward
// stamps on a bead-level WorkloadIntent.
//
// Resolution order:
//   1. override — when ClusterDispatchConfig.FormulaPhase is set, it
//      wins. Tests pin this; production normally leaves it empty.
//   2. bead type — task / bug / epic / feature / chore. The operator
//      classifies these as bead-level (intent.IsBeadLevelPhase) and
//      routes them to a wizard pod.
//   3. fallback — intent.PhaseWizard. Beads with an empty type
//      string still need to dispatch somewhere; the canonical
//      bead-level value is "wizard".
func beadDispatchPhase(override, beadType string) string {
	if override != "" {
		return override
	}
	if beadType != "" {
		return beadType
	}
	return intent.PhaseWizard
}

// buildClusterIntent returns the buildIntent closure the steward hands
// to dispatch.ClaimThenEmit. The closure stamps the claimed AttemptID
// onto a pre-computed intent so dispatch.ValidateHandle inside the
// emitter passes.
func buildClusterIntent(ident identity.ClusterRepoIdentity, formulaPhase, handoffMode string) func(*dispatch.AttemptHandle) intent.WorkloadIntent {
	return func(h *dispatch.AttemptHandle) intent.WorkloadIntent {
		return intent.WorkloadIntent{
			AttemptID: h.AttemptID,
			RepoIdentity: intent.RepoIdentity{
				URL:        ident.URL,
				BaseBranch: ident.BaseBranch,
				Prefix:     ident.Prefix,
			},
			FormulaPhase: formulaPhase,
			HandoffMode:  handoffMode,
		}
	}
}

// publisherEmitter adapts an intent.IntentPublisher into a
// dispatch.DispatchEmitter. Emit calls dispatch.ValidateHandle first so
// the seam invariant (no Publish without a matching claimed handle)
// holds end-to-end.
type publisherEmitter struct {
	publisher intent.IntentPublisher
}

// Emit implements dispatch.DispatchEmitter. It guards against missing
// or mismatched handles via dispatch.ValidateHandle, then delegates to
// the wrapped IntentPublisher.
func (e publisherEmitter) Emit(ctx context.Context, h *dispatch.AttemptHandle, i intent.WorkloadIntent) error {
	if err := dispatch.ValidateHandle(h, i); err != nil {
		return err
	}
	return e.publisher.Publish(ctx, i)
}

// singleBeadSelector is a dispatch.ReadyWorkSelector that yields a
// single, pre-known parent bead ID. The steward already filters
// schedulable work via store.GetSchedulableWork at the cycle entry, so
// per-bead dispatch wraps each candidate in a single-element selector
// rather than re-scanning the whole store inside ClaimNext.
//
// The selector adds no uniqueness semantics — the AttemptClaimer's
// shared-store atomic claim is still the only ownership seam.
type singleBeadSelector struct {
	id string
}

// SelectReady returns the configured bead ID. An empty id yields an
// empty slice, which ClaimNext interprets as "nothing ready".
func (s singleBeadSelector) SelectReady(_ context.Context) ([]string, error) {
	if s.id == "" {
		return nil, nil
	}
	return []string{s.id}, nil
}
