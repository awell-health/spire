package executor

// handoff.go — explicit HandoffMode selection plus the spi-xplwy chunk 5a
// quarantine for the legacy push transport.
//
// Context (docs/design/spi-xplwy-runtime-contract.md §3 "Chunk 5 — mark and
// then remove transitional handoff"): phase 5a labels every remaining push
// path as HandoffTransitional and emits a deprecation log + counter so the
// migration can show zero transitional use before phase 5b removes the code.
//
// This file is the ONE place that:
//   - defines the selection rule (same-owner → borrowed; cross-owner bundle
//     vs push → HandoffBundle or HandoffTransitional),
//   - bumps spire_handoff_transitional_total on every transitional selection,
//   - emits the Warn-level deprecation log with full RunContext identity,
//   - honors the SPIRE_FAIL_ON_TRANSITIONAL_HANDOFF=1 CI parity gate.
//
// The pkg/agent backends still execute push/bundle mechanics — nothing is
// removed here. Phase 5b is a separate bead.

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/runtime"
)

// EnvFailOnTransitionalHandoff gates the hard-fail behavior. When the env
// var is set to "1" (or "true", "yes"), recordHandoffSelection returns an
// error instead of just logging the deprecation. Default off in production;
// CI parity lanes set it on so a silently-returning push path fails the lane.
const EnvFailOnTransitionalHandoff = "SPIRE_FAIL_ON_TRANSITIONAL_HANDOFF"

// DeprecationMessageTransitional is the exact Warn-level line emitted when
// HandoffTransitional is selected. Kept as a constant so tests can assert on
// it and future observability consumers can key structured log parsing.
const DeprecationMessageTransitional = "DEPRECATION: transitional push handoff selected (spi-xplwy chunk 5b will remove this); migrate to bundle"

// handoffCounterKey is the low-cardinality label tuple that the
// spire_handoff_transitional_total counter is bucketed by. Bead/attempt/run
// IDs are intentionally OFF this tuple — §1.4 of the design spec requires
// high-cardinality identifiers stay in logs/traces, not on counter labels.
type handoffCounterKey struct {
	Tower   string
	Prefix  string
	Role    string
	Backend string
}

// transitionalCounter is the in-memory bump target for the deprecation
// counter. A future task (spi-xplwy chunk 6) wires these into the
// /metrics Prometheus endpoint; for now the counter lives here so the
// deprecation is observable to tests and to any operator willing to read
// it via HandoffTransitionalCount.
var (
	transitionalCounterMu sync.Mutex
	transitionalCounter   = map[handoffCounterKey]uint64{}
)

// HandoffTransitionalCount returns the current count for the given label
// tuple. Exposed for tests and for the chunk-6 metrics wiring.
func HandoffTransitionalCount(tower, prefix, role, backend string) uint64 {
	transitionalCounterMu.Lock()
	defer transitionalCounterMu.Unlock()
	return transitionalCounter[handoffCounterKey{tower, prefix, role, backend}]
}

// HandoffTransitionalTotal returns the sum across every label tuple — useful
// when tests don't care about the bucketing and just want to confirm a bump.
func HandoffTransitionalTotal() uint64 {
	transitionalCounterMu.Lock()
	defer transitionalCounterMu.Unlock()
	var total uint64
	for _, v := range transitionalCounter {
		total += v
	}
	return total
}

// ResetHandoffTransitionalCounters clears all counter state. Intended for
// test isolation — production callers never touch this.
func ResetHandoffTransitionalCounters() {
	transitionalCounterMu.Lock()
	defer transitionalCounterMu.Unlock()
	transitionalCounter = map[handoffCounterKey]uint64{}
}

// bumpTransitionalCounter increments the counter for the given label tuple.
func bumpTransitionalCounter(tower, prefix, role, backend string) {
	transitionalCounterMu.Lock()
	defer transitionalCounterMu.Unlock()
	transitionalCounter[handoffCounterKey{tower, prefix, role, backend}]++
}

// failOnTransitionalHandoff reports whether the env gate is set. Reads are
// not cached — tests override the env var between sub-cases.
func failOnTransitionalHandoff() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(EnvFailOnTransitionalHandoff)))
	switch v {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// recordHandoffSelection is the single choke point for HandoffMode-dependent
// side effects. Callers pass the mode selected by the executor and the
// RunContext that will ride on the spawn; this function handles:
//
//   1. HandoffTransitional: bump the counter and emit the Warn-level
//      deprecation log with full identity.
//   2. SPIRE_FAIL_ON_TRANSITIONAL_HANDOFF=1: promote the deprecation to a
//      hard error — CI parity lanes use this to catch accidental
//      regressions before they ship.
//   3. Other modes: no-op.
//
// The log sink is injected so tests can capture output without reaching
// into e.log. Production callers pass e.log directly.
func recordHandoffSelection(logf func(string, ...interface{}), mode HandoffMode, run RunContext) error {
	if mode != HandoffTransitional {
		return nil
	}

	bumpTransitionalCounter(run.TowerName, run.Prefix, string(run.Role), run.Backend)

	if logf != nil {
		// Emit the canonical RunContext field vocabulary (docs/design/
		// spi-xplwy-runtime-contract.md §1.4). runtime.LogFields renders
		// every field — including empty ones — so downstream log parsers
		// see a stable schema regardless of which fields the dispatch
		// site populated.
		logf("%s%s", DeprecationMessageTransitional, runtime.LogFields(run))
	}

	if failOnTransitionalHandoff() {
		return fmt.Errorf("%s=1: refusing transitional handoff (tower=%s prefix=%s bead=%s role=%s)",
			EnvFailOnTransitionalHandoff,
			run.TowerName,
			run.Prefix,
			run.BeadID,
			run.Role,
		)
	}

	return nil
}

// apprenticeDeliveryHandoff decides between HandoffBundle and
// HandoffTransitional for a cross-owner apprentice dispatch. The tower's
// configured apprentice transport is the single source of truth:
//
//   - push    → HandoffTransitional (quarantined legacy path)
//   - bundle  → HandoffBundle       (canonical delivery)
//   - unknown → HandoffBundle       (conservative default; wizard-side
//                                    validation surfaces the bad config
//                                    when it actually tries to deliver)
//
// Callers that already know the tower config can pass it directly to
// avoid a second ActiveTowerConfig() roundtrip.
func apprenticeDeliveryHandoff(tower *TowerConfig) HandoffMode {
	if tower == nil {
		// Treat missing tower config as bundle-by-default: the system's
		// zero-value tower config resolves to bundle transport via
		// ApprenticeConfig.EffectiveTransport, so this keeps the two paths
		// aligned rather than silently flagging every un-configured test.
		return HandoffBundle
	}
	switch tower.Apprentice.EffectiveTransport() {
	case config.ApprenticeTransportPush:
		return HandoffTransitional
	case config.ApprenticeTransportBundle:
		return HandoffBundle
	default:
		return HandoffBundle
	}
}

// resolveApprenticeHandoff is the executor's helper to look up the tower and
// apply apprenticeDeliveryHandoff. Returns HandoffBundle when the tower
// accessor is absent or erroring — the wizard-side validator surfaces
// misconfiguration when it actually tries to deliver.
func (e *Executor) resolveApprenticeHandoff() HandoffMode {
	if e.deps == nil || e.deps.ActiveTowerConfig == nil {
		return HandoffBundle
	}
	tower, err := e.deps.ActiveTowerConfig()
	if err != nil {
		return HandoffBundle
	}
	return apprenticeDeliveryHandoff(tower)
}
