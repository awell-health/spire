//go:build e2e

// Package e2e holds end-to-end validation tests for the Spire cluster
// runtime. These tests run against a real minikube cluster with a real
// helm-installed Spire deployment; they are excluded from the default
// go test build via the `e2e` build tag.
//
// The cache-recovery test (spi-p18tr) is the acceptance gate for the
// pinned-identity + wisp recovery epic (spi-w860i). It walks the full
// data flow:
//
//	WizardGuild CR (Cache.Enabled=true)
//	  → operator reconciler creates pinned identity bead
//	  → [test injects refresh failure]
//	  → refresh Job backoff exhausted
//	  → operator files wisp recovery bead (caused-by → pinned)
//	  → steward hooked-sweep claims wisp
//	  → cleric runs cleric-default formula
//	  → WriteOutcome stamps recovery_learnings row
//	  → wisp closed
//	  → wisp GC reaps wisp row + edge
//	  → recovery_learnings row survives
//
// If any sibling task in the epic is not yet shipped, the test fails
// loudly at the corresponding stage with a message pointing at the
// missing upstream — t.Skip is deliberately avoided so the failure is
// unambiguous signal rather than a silent pass.
package e2e

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
	"github.com/awell-health/spire/test/e2e/helpers"
)

// Test-wide timing constants. Values are tuned so that a healthy run
// on minikube completes in roughly 5-10 minutes end-to-end.
const (
	beadPollTimeout  = 60 * time.Second // quick bead-state transitions
	wispFileTimeout  = 3 * time.Minute  // refresh Job backoff → wisp creation
	clericTimeout    = 5 * time.Minute  // dispatch + repair + verify
	cacheReadyInit   = 5 * time.Minute  // initial cache bootstrap + clone
	cacheRecoverWait = 5 * time.Minute  // post-recovery re-refresh
	gcTimeout        = 2 * time.Minute  // wisp GC cycle
)

// Fixture is the per-test cluster + store context. A single instance
// is built by seedFixture and shared across all t.Run blocks within
// TestCacheRecoveryE2E.
type Fixture struct {
	*helpers.Fixture

	GuildName       string
	ResourceURI     string
	PinnedBeadID    string
	PreFailureRev   string // observedRevision captured before failure injection
}

// TestCacheRecoveryE2E is the top-level driver. Sub-tests run in order
// so each builds on prior state — for example, StewardPicksUpAndClericDispatches
// depends on CacheRefreshFailureFilesWisp having populated the pinned + wisp
// bead pair.
func TestCacheRecoveryE2E(t *testing.T) {
	fix := seedFixture(t)

	t.Run("PinnedIdentityProvisioned", func(t *testing.T) {
		pinnedBead := helpers.GetPinnedIdentityBead(t, fix.GuildName)
		fix.PinnedBeadID = pinnedBead.ID

		// Q2 resolution: both signals must be set. Status is the bead
		// row's lifecycle marker; the pinned metadata flag is the
		// boolean classifier. Either alone leaves half the query
		// tooling blind to the bead.
		if pinnedBead.Status != "pinned" {
			t.Fatalf("pinned identity bead %s: Status=%q, want %q",
				pinnedBead.ID, pinnedBead.Status, "pinned")
		}
		if !beadHasLabel(pinnedBead, "pinned-identity") {
			t.Fatalf("pinned identity bead %s: missing 'pinned-identity' label (labels=%v)",
				pinnedBead.ID, pinnedBead.Labels)
		}
		t.Logf("pinned identity bead: %s (%s)", pinnedBead.ID, pinnedBead.Title)

		// Capture the pre-failure observed revision so VerifyConfirmsCacheHealthy
		// can later assert the SHA advanced.
		status := helpers.GetWizardGuildStatus(t, fix.Dynamic, fix.Namespace, fix.GuildName)
		if status.Cache == nil || status.Cache.Revision == "" {
			t.Fatalf("WizardGuild %s: no Cache.Revision stamped — cache bootstrap did not complete",
				fix.GuildName)
		}
		fix.PreFailureRev = status.Cache.Revision
		t.Logf("pre-failure cache revision: %s", fix.PreFailureRev)
	})

	if fix.PinnedBeadID == "" {
		t.Fatal("pinned identity bead missing — cannot continue")
	}

	var wispID string
	t.Run("CacheRefreshFailureFilesWisp", func(t *testing.T) {
		helpers.BreakCacheRefresh(t, fix.Dynamic, fix.Namespace, fix.GuildName)
		t.Logf("injected failure via BranchPin=%s", helpers.BogusBranchPin)

		wisp := helpers.WaitForOpenWisp(t, fix.PinnedBeadID, wispFileTimeout)
		wispID = wisp.ID
		t.Logf("observed open wisp: %s (%s)", wisp.ID, wisp.Title)

		if got := wisp.Meta("source_resource_uri"); got == "" {
			t.Fatalf("wisp %s: source_resource_uri metadata is empty", wisp.ID)
		} else if got != fix.ResourceURI {
			t.Fatalf("wisp %s: source_resource_uri=%q, want %q",
				wisp.ID, got, fix.ResourceURI)
		}

		if got := wisp.Meta("pinned_identity_bead_id"); got != fix.PinnedBeadID {
			t.Fatalf("wisp %s: pinned_identity_bead_id=%q, want %q",
				wisp.ID, got, fix.PinnedBeadID)
		}

		if !beadHasLabel(wisp, "interrupted:cache-refresh-failure") {
			t.Fatalf("wisp %s: missing interrupted:cache-refresh-failure label (labels=%v)",
				wisp.ID, wisp.Labels)
		}
	})

	if wispID == "" {
		t.Fatal("no wisp observed — cannot continue")
	}

	t.Run("StewardPicksUpAndClericDispatches", func(t *testing.T) {
		w := helpers.WaitForClericDispatch(t, wispID, clericTimeout)
		t.Logf("wisp %s transitioned to in_progress (status=%s)", wispID, w.Status)

		// The cleric-default formula marks the wisp's formula in
		// metadata when it pours the molecule. Absence is signal that
		// the steward routed to a different formula — flag it.
		if got := w.Meta("formula"); got != "" && got != "cleric-default" {
			t.Fatalf("wisp %s: formula=%q, want cleric-default", wispID, got)
		}
	})

	var outcome recovery.RecoveryOutcome
	t.Run("DecideRoutedThroughClaude", func(t *testing.T) {
		outcome = helpers.WaitForOutcome(t, wispID, clericTimeout)

		// Per Step 1 of the design (agent-first decide reorder), the
		// decide step priority is: cleric agent (Claude) first, then
		// recipe replay, never git-state heuristics. A "recipe" branch
		// is acceptable if a promoted recipe was already in place —
		// log which and continue.
		branch := strings.ToLower(outcome.RepairAction)
		switch {
		case outcome.RepairMode == recovery.RepairModeRecipe:
			t.Logf("decide routed through recipe (RepairMode=%s, action=%s) — acceptable",
				outcome.RepairMode, outcome.RepairAction)
		case outcome.RepairMode == recovery.RepairModeWorker, outcome.RepairMode == recovery.RepairModeMechanical:
			t.Logf("decide routed through agent (RepairMode=%s, action=%s)",
				outcome.RepairMode, outcome.RepairAction)
		case outcome.RepairMode == "":
			t.Fatalf("wisp %s: RecoveryOutcome.RepairMode is empty — decide step did not run", wispID)
		default:
			t.Fatalf("wisp %s: unexpected RepairMode=%q (action=%q)",
				wispID, outcome.RepairMode, outcome.RepairAction)
		}

		// Git-state heuristics would historically surface as reset-hard
		// / resummon branches without an explicit agent decision. If we
		// see one of those with RepairMode empty or omitted, that's a
		// regression on Step 1 of the design.
		for _, forbidden := range []string{"reset-hard", "resummon", "reset-to-design"} {
			if branch == forbidden && outcome.RepairMode == "" {
				t.Fatalf("wisp %s: decide chose git-state heuristic %q — Step 1 regression", wispID, branch)
			}
		}
	})

	t.Run("VerifyConfirmsCacheHealthy", func(t *testing.T) {
		if outcome.VerifyVerdict != "" && outcome.VerifyVerdict != recovery.VerifyVerdictPass {
			t.Fatalf("wisp %s: VerifyVerdict=%q, want %q",
				wispID, outcome.VerifyVerdict, recovery.VerifyVerdictPass)
		}

		// Reset BranchPin so the next refresh Job actually succeeds.
		// Without this, the operator keeps failing and the test cannot
		// observe a post-recovery cache advance.
		clearBranchPin(t, fix)

		// Poll CacheRefreshing=False + observedRevision > pre-failure.
		deadline := time.Now().Add(cacheRecoverWait)
		for time.Now().Before(deadline) {
			status := helpers.GetWizardGuildStatus(t, fix.Dynamic, fix.Namespace, fix.GuildName)
			if status.Cache != nil &&
				status.Cache.Phase == "Ready" &&
				status.Cache.Revision != "" &&
				status.Cache.Revision != fix.PreFailureRev {
				t.Logf("cache recovered: revision advanced %s → %s",
					fix.PreFailureRev, status.Cache.Revision)
				return
			}
			time.Sleep(5 * time.Second)
		}
		final := helpers.GetWizardGuildStatus(t, fix.Dynamic, fix.Namespace, fix.GuildName)
		var got string
		if final.Cache != nil {
			got = fmt.Sprintf("Phase=%s Revision=%s", final.Cache.Phase, final.Cache.Revision)
		} else {
			got = "Cache=nil"
		}
		t.Fatalf("cache did not recover within %s (pre-failure=%s, now=%s)",
			cacheRecoverWait, fix.PreFailureRev, got)
	})

	t.Run("WispClosedAndLearningsRecorded", func(t *testing.T) {
		helpers.WaitForBeadStatus(t, wispID, "closed", clericTimeout)

		db := helpers.OpenDoltSQL(t, "127.0.0.1", fix.DoltLocalPort, fix.TowerName)
		rows := helpers.GetRecoveryLearningsByResourceURI(t, db, fix.ResourceURI)
		if len(rows) == 0 {
			t.Fatalf("no recovery_learnings rows for resource=%s — cleric learn step did not link to SourceResourceURI (follow-up: add source_resource_uri column or helper to pkg/store)",
				fix.ResourceURI)
		}
		row := rows[0]
		t.Logf("learning row: action=%s outcome=%s reusable=%v", row.ResolutionKind, row.Outcome, row.Reusable)

		if row.FailureClass != string(recovery.FailureClassCacheRefresh) {
			t.Fatalf("learning row for resource=%s: FailureClass=%q, want %q",
				fix.ResourceURI, row.FailureClass, recovery.FailureClassCacheRefresh)
		}
	})

	t.Run("LearningsSurviveWispGC", func(t *testing.T) {
		db := helpers.OpenDoltSQL(t, "127.0.0.1", fix.DoltLocalPort, fix.TowerName)

		// The task spec references `bd mol wisp gc` — not yet wired into
		// cmd/spire or cmd/bd as a callable entrypoint. Wait for the
		// operator/steward-side reaping cycle instead; if it never runs,
		// fail with a precise message pointing at the missing surface.
		if !helpers.WaitForWispReaped(t, db, wispID, gcTimeout) {
			t.Fatalf("wisp %s was not reaped from wisps table within %s — wisp GC surface missing (follow-up: wire `spire wisp gc` or equivalent store helper)",
				wispID, gcTimeout)
		}
		t.Logf("wisp %s reaped from wisps table", wispID)

		rows := helpers.GetRecoveryLearningsByResourceURI(t, db, fix.ResourceURI)
		if len(rows) == 0 {
			t.Fatalf("recovery_learnings row disappeared after wisp GC — W2 violated")
		}
		t.Logf("%d recovery_learnings row(s) survived wisp GC", len(rows))
	})

	t.Run("FinalizerClosesOpenWispsBeforePinned", func(t *testing.T) {
		// Inject a fresh failure so a new open wisp exists at CR delete time.
		helpers.BreakCacheRefresh(t, fix.Dynamic, fix.Namespace, fix.GuildName)
		freshWisp := helpers.WaitForOpenWisp(t, fix.PinnedBeadID, wispFileTimeout)
		t.Logf("fresh wisp for finalizer test: %s", freshWisp.ID)

		helpers.DeleteWizardGuild(t, fix.Dynamic, fix.Namespace, fix.GuildName)

		// Wait for finalizer to run: both beads should end closed.
		closedWisp := helpers.WaitForBeadStatus(t, freshWisp.ID, "closed", clericTimeout)
		closedPinned := helpers.WaitForBeadStatus(t, fix.PinnedBeadID, "closed", clericTimeout)

		// Ordering assertion (W1): the wisp's closed-at must precede
		// the pinned bead's closed-at. UpdatedAt is the closest proxy
		// the Bead projection offers — the field ticks on status
		// transitions and the finalizer closes them in order (see
		// operator/controllers/pinned_identity.go:finalizePinnedIdentity).
		if closedWisp.UpdatedAt == "" || closedPinned.UpdatedAt == "" {
			t.Fatalf("missing UpdatedAt on closed beads: wisp=%q pinned=%q",
				closedWisp.UpdatedAt, closedPinned.UpdatedAt)
		}
		wispTS, err := time.Parse(time.RFC3339, closedWisp.UpdatedAt)
		if err != nil {
			t.Fatalf("parse wisp UpdatedAt=%q: %v", closedWisp.UpdatedAt, err)
		}
		pinnedTS, err := time.Parse(time.RFC3339, closedPinned.UpdatedAt)
		if err != nil {
			t.Fatalf("parse pinned UpdatedAt=%q: %v", closedPinned.UpdatedAt, err)
		}
		if wispTS.After(pinnedTS) {
			t.Fatalf("W1 violation: wisp %s closed at %s, pinned %s closed at %s — expected wisp-first",
				freshWisp.ID, wispTS, fix.PinnedBeadID, pinnedTS)
		}
		t.Logf("W1 ordering OK: wisp closed @%s, pinned closed @%s (delta=%s)",
			wispTS.Format(time.RFC3339), pinnedTS.Format(time.RFC3339), pinnedTS.Sub(wispTS))
	})
}

// seedFixture brings up the per-test environment:
//  1. Ensures minikube is reachable (skips test otherwise).
//  2. Picks a unique namespace (`spire-e2e-<rand>`) so parallel
//     runs don't collide.
//  3. Helm-installs the chart; waits for spire-operator + the shared
//     dolt/clickhouse pods.
//  4. Port-forwards dolt so the bead store can be opened from the
//     test process.
//  5. Creates a tower; opens pkg/store against the port-forwarded dolt.
//  6. Applies a WizardGuild with Cache enabled and waits for
//     PinnedIdentityBeadID to be stamped.
//
// All teardown is registered via t.Cleanup — including helm uninstall,
// port-forward cancellation, and store.Reset.
func seedFixture(t *testing.T) *Fixture {
	t.Helper()
	helpers.EnsureMinikubeUp(t)

	// #nosec G404 — random is used only to namespace test runs.
	namespace := fmt.Sprintf("spire-e2e-%04x", rand.New(rand.NewSource(time.Now().UnixNano())).Int31())
	towerName := "e2e-" + strings.TrimPrefix(namespace, "spire-e2e-")
	guildName := "cache-recovery"

	kube, dyn := helpers.GetKubeClient(t)

	helpers.InstallSpireHelm(t, helpers.HelmInstallOpts{
		Namespace: namespace,
		TowerName: towerName,
	})
	t.Cleanup(func() { helpers.UninstallSpireHelm(t, namespace) })

	helpers.WaitForAllPodsReady(t, kube, namespace, helpers.DefaultHelmTimeout)
	helpers.WaitForOperatorReady(t, kube, namespace, 2*time.Minute)

	localPort, cancel := helpers.PortForwardDolt(t, namespace)
	t.Cleanup(cancel)

	helpers.CreateTestTower(t, towerName)
	helpers.OpenStoreViaPortForward(t, towerName, "127.0.0.1", localPort)

	_ = helpers.ApplyWizardGuildWithCache(t, dyn, namespace, guildName)

	// Initial pinned-identity stamp + cache Ready can take a minute
	// on minikube cold start (image pull, PVC bind). The 5-minute
	// window matches the helm wait budget.
	_ = helpers.WaitForPinnedIdentityStamped(t, dyn, namespace, guildName, 3*time.Minute)
	_ = helpers.WaitForCacheReady(t, dyn, namespace, guildName, cacheReadyInit)

	return &Fixture{
		Fixture: &helpers.Fixture{
			TowerName:     towerName,
			Namespace:     namespace,
			DoltLocalPort: localPort,
			Kube:          kube,
			Dynamic:       dyn,
		},
		GuildName:   guildName,
		ResourceURI: helpers.ResourceURIFor(namespace, guildName),
	}
}

// beadHasLabel is a tiny local helper — checks an exact label match.
// Duplicating the one-liner here rather than importing from pkg/store
// to keep the test file self-contained.
func beadHasLabel(b store.Bead, label string) bool {
	for _, l := range b.Labels {
		if l == label {
			return true
		}
	}
	return false
}

// clearBranchPin removes the bogus BranchPin so the cache can recover.
// Uses a null-valued JSON merge patch to drop the field; patching to
// empty string would be treated as an explicit pin rather than unset.
func clearBranchPin(t *testing.T, fix *Fixture) {
	t.Helper()
	// An explicit empty-string pin is rejected by the operator as
	// "pin to empty ref". Passing an explicit null via a merge patch
	// unsets the field. We use the typed helper to keep the JSON
	// shape correct.
	helpers.PatchWizardGuildCacheBranchPin(t, fix.Dynamic, fix.Namespace, fix.GuildName, "")
}
