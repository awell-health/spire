package steward

// Cluster-native dispatch — the second of the three deployment-mode paths
// the steward branches on. This file MUST stay free of the local-native
// concerns:
//
//   - It MUST NOT read LocalBindings.State, LocalBindings.LocalPath, or
//     cfg.Instances. Cluster repo identity comes only from
//     identity.ClusterIdentityResolver, backed by the shared tower repo
//     registry.
//   - The cluster-native code paths in this file (dispatchClusterNative,
//     dispatchPhaseClusterNative) MUST NOT call backend.Spawn.
//     Cluster-native scheduling does not create pods; it emits
//     intent.WorkloadIntent values and the operator reconciles them
//     into pods. The dispatchPhase seam's local-native branch DOES call
//     backend.Spawn — that is the shared mode-aware entry point for
//     per-phase dispatch and the Spawn call only runs when Mode is not
//     cluster-native. When Mode is cluster-native and the seam is
//     unwired, dispatchPhase fails closed with
//     ErrClusterDispatchUnavailable rather than falling back to Spawn.
//   - Bead-level dispatch MUST go through dispatch.ClaimThenEmit. The
//     attempt bead row created by the claim is the canonical ownership
//     seam — the in-process busy map and per-bead mutex are explicitly
//     not allowed as substitutes. Phase-level dispatch (review, hooked-
//     step resume) emits directly via the IntentPublisher because the
//     wizard's bead-level claim is already in place; per-phase intents
//     derive their uniqueness from the (task_id, dispatch_seq) PK on
//     workload_intents, bumped by store.NextDispatchSeq.
//
// The file deliberately contains no k8s.io imports. Talking to a cluster
// is the IntentPublisher's concern, plumbed in from cmd/spire wiring.

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/bd"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/runtime"
	"github.com/awell-health/spire/pkg/steward/dispatch"
	"github.com/awell-health/spire/pkg/steward/identity"
	"github.com/awell-health/spire/pkg/steward/intent"
	"github.com/awell-health/spire/pkg/store"

	"github.com/steveyegge/beads"
)

// ErrClusterDispatchUnavailable is the sentinel returned when a
// cluster-native dispatch path is reached but the cluster intent seam
// (ClusterDispatch on PhaseDispatch) is unwired. Callers and tests use
// errors.Is(err, ErrClusterDispatchUnavailable) to distinguish this
// fail-closed condition from other dispatch errors. There is no
// backend.Spawn fallback in cluster-native mode by design — the
// convergence is mechanical.
var ErrClusterDispatchUnavailable = errors.New("cluster dispatch seam unavailable")

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

	// MaxConcurrent is the tower-global cap on in-flight work at
	// dispatch time. In-flight means status IN ('dispatched',
	// 'in_progress') — both states hold a wizard slot. When the number
	// of in-flight beads is at or above MaxConcurrent, this cycle
	// emits zero new intents and the remainder wait in status=ready.
	// Zero (the default) disables the cap — unlimited dispatch,
	// matching the pre-cap behavior.
	MaxConcurrent int
}

// dispatchClusterNative emits a WorkloadIntent for each schedulable
// task in this cycle, threading the (TaskID, DispatchSeq) from the
// dispatch claim through dispatch.ClaimThenEmit. It returns the number
// of successful emits.
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

	// Tower-global concurrency cap: count beads already in-flight
	// (status dispatched or in_progress) and compute the remaining
	// slots this cycle may fill. MaxConcurrent <= 0 disables the cap.
	inFlight := 0
	remaining := -1 // -1 = unlimited
	if cd.MaxConcurrent > 0 {
		inFlight = countInFlight()
		remaining = cd.MaxConcurrent - inFlight
		if remaining <= 0 {
			log.Printf("[steward] %scluster-native: capped at %d/%d in flight — skipping dispatch", logPrefix, inFlight, cd.MaxConcurrent)
			return 0
		}
	}

	emitted := 0
	for _, bead := range schedulable {
		if remaining == 0 {
			log.Printf("[steward] %scluster-native: cap %d reached mid-cycle — deferring %d candidate(s)", logPrefix, cd.MaxConcurrent, len(schedulable)-emitted)
			break
		}
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
		if remaining > 0 {
			remaining--
		}
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
// to dispatch.ClaimThenEmit. The closure stamps the claimed
// (TaskID, DispatchSeq) onto a pre-computed intent so
// dispatch.ValidateHandle inside the emitter passes.
func buildClusterIntent(ident identity.ClusterRepoIdentity, formulaPhase, handoffMode string) func(*dispatch.ClaimHandle) intent.WorkloadIntent {
	return func(h *dispatch.ClaimHandle) intent.WorkloadIntent {
		return intent.WorkloadIntent{
			TaskID:      h.TaskID,
			DispatchSeq: h.DispatchSeq,
			Reason:      h.Reason,
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
func (e publisherEmitter) Emit(ctx context.Context, h *dispatch.ClaimHandle, i intent.WorkloadIntent) error {
	if err := dispatch.ValidateHandle(h, i); err != nil {
		return err
	}
	return e.publisher.Publish(ctx, i)
}

// countInFlight returns the number of top-level work beads currently
// holding a wizard slot — status IN ('dispatched', 'in_progress').
// The predicate deliberately includes `dispatched` so the 50–90s
// window between emit and wizard-claim still counts toward the
// concurrency cap; without it the cap would burst through N+M in
// flight (N in_progress + M dispatched) until the wizards caught up.
//
// Internal/child beads are skipped via store.IsWorkBead, matching the
// filter the rest of the scheduler uses.
func countInFlight() int {
	inProg, err := ListBeadsFunc(beads.IssueFilter{Status: store.StatusPtr(beads.StatusInProgress)})
	if err != nil {
		log.Printf("[steward] count in-flight (in_progress): %s", err)
	}
	disp, err := ListBeadsFunc(beads.IssueFilter{Status: store.StatusPtr(beads.Status(bd.StatusDispatched))})
	if err != nil {
		log.Printf("[steward] count in-flight (dispatched): %s", err)
	}
	n := 0
	for _, b := range inProg {
		if store.IsWorkBead(b) {
			n++
		}
	}
	for _, b := range disp {
		if store.IsWorkBead(b) {
			n++
		}
	}
	return n
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

// PhaseDispatch bundles the deployment-mode seam state that per-phase
// dispatch points (DetectReviewReady, SweepHookedSteps) need to route a
// workload correctly. The zero value behaves as local-native mode: the
// caller's backend.Spawn is invoked directly and ClusterDispatch is
// unused. Tests that don't exercise cluster-native paths can pass a zero
// value.
type PhaseDispatch struct {
	// Mode is the tower's effective deployment mode. When set to
	// config.DeploymentModeClusterNative and ClusterDispatch is non-nil,
	// dispatchPhase emits a WorkloadIntent instead of calling
	// backend.Spawn.
	Mode config.DeploymentMode

	// ClusterDispatch carries the cluster-native seams used when Mode is
	// cluster-native. Nil makes dispatchPhase fail closed with
	// ErrClusterDispatchUnavailable rather than silently falling back to
	// backend.Spawn — the cluster path must never reach backend.Spawn,
	// even on misconfiguration.
	ClusterDispatch *ClusterDispatchConfig
}

// dispatchPhase is the single mode-aware entry point for per-phase
// workload dispatch — review routing and hooked-step resume. It branches
// on pd.Mode:
//
//   - cluster-native: emits a phase-keyed WorkloadIntent via
//     dispatchPhaseClusterNative; the operator reconciles it into the
//     correct pod shape (sage for review, apprentice for implement/fix,
//     wizard for bead-level). Returns a nil Handle because no local
//     process is created. If ClusterDispatch is nil the call fails
//     closed with ErrClusterDispatchUnavailable; backend.Spawn is
//     unreachable from this branch by design.
//   - local-native (and zero value): forwards to backend.Spawn with the
//     provided SpawnConfig, preserving historical behavior.
//
// All per-phase dispatch points in pkg/steward MUST go through this seam
// so the "cluster-native never calls backend.Spawn" invariant documented
// at the top of this file holds end-to-end. Callers route each site by
// passing the corresponding intent phase constant (intent.PhaseReview,
// intent.PhaseWizard, intent.PhaseImplement, etc.).
func dispatchPhase(ctx context.Context, pd PhaseDispatch, backend agent.Backend, sc agent.SpawnConfig, phase string) (agent.Handle, error) {
	if pd.Mode == config.DeploymentModeClusterNative {
		if pd.ClusterDispatch == nil {
			return nil, fmt.Errorf("steward: cluster dispatch seam unavailable for %s (phase %q): %w",
				sc.BeadID, phase, ErrClusterDispatchUnavailable)
		}
		if err := dispatchPhaseClusterNative(ctx, pd.ClusterDispatch, sc.BeadID, phase); err != nil {
			return nil, err
		}
		return nil, nil
	}
	return backend.Spawn(sc)
}

// dispatchPhaseClusterNative emits a phase-keyed WorkloadIntent for the
// given bead and phase. It is the phase-level counterpart to
// dispatchClusterNative and is invoked when a per-phase workload (review
// pod, hooked-step resume) needs to run while the bead's wizard attempt
// is already active.
//
// Unlike dispatchClusterNative this path does NOT claim a bead-level
// dispatch slot — the wizard already claimed the bead, and the
// (task_id, dispatch_seq) PK on workload_intents is the uniqueness
// mechanism for the emitted row. store.NextDispatchSeq bumps the
// sequence monotonically across all prior intents for the task, so a
// per-phase emit never collides with the bead-level intent or a prior
// phase-level emit.
//
// The function preserves the file-level invariant that we never call
// backend.Spawn: it only writes to the intent outbox; the operator
// reconciles the resulting pod.
//
// KNOWN GAP — steward producer migration follow-up: this function does
// not yet populate WorkloadIntent.Role, .Phase, or .Runtime.Image. The
// operator's intent.Validate (operator/controllers/intent_reconciler.go)
// requires all three and will drop the emitted intent. Until the
// steward producer migration lands (separate follow-up under
// spi-5bzu9r), per-phase and bead-level emits from this function are
// effectively no-ops on the cluster path. Executor- and wizard-side
// emits (apprentice/sage children built via
// pkg/executor.childIntentForApprentice and childIntentForSage) DO
// populate the triple and are unaffected. Doc tables in
// docs/VISION-CLUSTER.md, docs/ARCHITECTURE.md, and operator/README.md
// flag this gap so consumers do not infer steward emits work today.
func dispatchPhaseClusterNative(ctx context.Context, cd *ClusterDispatchConfig, beadID, phase string) error {
	if cd == nil {
		return errors.New("cluster-native phase dispatch: ClusterDispatch is not configured")
	}
	if cd.Resolver == nil || cd.Publisher == nil {
		return errors.New("cluster-native phase dispatch: Resolver and Publisher both required")
	}
	if beadID == "" {
		return errors.New("cluster-native phase dispatch: beadID is required")
	}
	if phase == "" {
		return errors.New("cluster-native phase dispatch: phase is required")
	}

	repoPrefix := beadRepoPrefix(beadID)
	ident, err := cd.Resolver.Resolve(ctx, repoPrefix)
	if err != nil {
		return fmt.Errorf("cluster-native phase dispatch: resolve repo %q for %s: %w", repoPrefix, beadID, err)
	}

	seq, err := NextDispatchSeqFunc(beadID)
	if err != nil {
		return fmt.Errorf("cluster-native phase dispatch: next dispatch seq for %s: %w", beadID, err)
	}

	handoffMode := cd.HandoffMode
	if handoffMode == "" {
		handoffMode = string(runtime.HandoffBundle)
	}

	wi := intent.WorkloadIntent{
		TaskID:      beadID,
		DispatchSeq: seq,
		Reason:      "phase:" + phase,
		RepoIdentity: intent.RepoIdentity{
			URL:        ident.URL,
			BaseBranch: ident.BaseBranch,
			Prefix:     ident.Prefix,
		},
		FormulaPhase: phase,
		HandoffMode:  handoffMode,
	}

	if err := cd.Publisher.Publish(ctx, wi); err != nil {
		return fmt.Errorf("cluster-native phase dispatch: publish %s (%s, seq=%d): %w", beadID, phase, seq, err)
	}
	return nil
}

// NextDispatchSeqFunc is a test-replaceable hook for store.NextDispatchSeq
// so unit tests can avoid depending on an open dolt connection.
var NextDispatchSeqFunc = store.NextDispatchSeq
