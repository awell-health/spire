// Package steward implements the steward coordination cycle and daemon loop.
//
// The steward is responsible for:
//   - Ready work selection and assignment to idle agents
//   - Stale detection and shutdown enforcement
//   - Review routing (detecting beads ready for review, re-engaging wizards on feedback)
//   - Backend dispatch decisions (round-robin assignment)
//
// The daemon is responsible for:
//   - Tower config derivation and sync
//   - Dolt remote sync (pull/push)
//   - Linear epic sync and webhook processing
//   - Agent inbox delivery
//   - Dead agent reaping
//
// cmd/spire keeps only thin command adapters for flag parsing and process wiring.
package steward

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/alerts"
	"github.com/awell-health/spire/pkg/bd"
	"github.com/awell-health/spire/pkg/beadlifecycle"
	"github.com/awell-health/spire/pkg/cleric"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/dolt"
	"github.com/awell-health/spire/pkg/executor"
	"github.com/awell-health/spire/pkg/formula"
	spgit "github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/awell-health/spire/pkg/steward/attached"
	"github.com/awell-health/spire/pkg/steward/intent"
	"github.com/awell-health/spire/pkg/store"
	"github.com/awell-health/spire/pkg/wizardregistry"
	"github.com/steveyegge/beads"
)

// --- Test-replaceable function variables ---

// CreateBeadFunc is a test-replaceable function for store.CreateBead.
var CreateBeadFunc = store.CreateBead

// GetActiveAttemptFunc is a test-replaceable function for store.GetActiveAttempt.
var GetActiveAttemptFunc = store.GetActiveAttempt

// GetDBForRoutingFunc is a test-replaceable function for getDBForRouting.
var GetDBForRoutingFunc = getDBForRouting

// AddLabelFunc is a test-replaceable function for store.AddLabel.
var AddLabelFunc = store.AddLabel

// ListBeadsFunc is a test-replaceable function for store.ListBeads.
var ListBeadsFunc = store.ListBeads

// RaiseCorruptedBeadAlertFunc is a test-replaceable function for RaiseCorruptedBeadAlert.
var RaiseCorruptedBeadAlertFunc = RaiseCorruptedBeadAlert

// GetChildrenFunc is a test-replaceable function for store.GetChildren.
var GetChildrenFunc = store.GetChildren

// GetStepBeadsFunc is a test-replaceable function for store.GetStepBeads.
// Used by DetectReviewReady so tests can drive the review dispatch path
// with synthetic step beads without a live store.
var GetStepBeadsFunc = store.GetStepBeads

// GetBeadFunc is a test-replaceable function for store.GetBead.
var GetBeadFunc = store.GetBead

// GetCommentsFunc is a test-replaceable function for store.GetComments.
var GetCommentsFunc = store.GetComments

// RemoveLabelFunc is a test-replaceable function for store.RemoveLabel.
var RemoveLabelFunc = store.RemoveLabel

// UnhookStepBeadFunc is a test-replaceable function for store.UnhookStepBead.
var UnhookStepBeadFunc = store.UnhookStepBead

// GetHookedStepsFunc is a test-replaceable function for store.GetHookedSteps.
var GetHookedStepsFunc = store.GetHookedSteps

// GetAttemptInstanceFunc is a test-replaceable function for store.GetAttemptInstance.
var GetAttemptInstanceFunc = store.GetAttemptInstance

// IsOwnedByInstanceFunc is a test-replaceable function for store.IsOwnedByInstance.
var IsOwnedByInstanceFunc = store.IsOwnedByInstance

// InstanceIDFunc is a test-replaceable function for config.InstanceID.
var InstanceIDFunc = config.InstanceID

// GetDependentsWithMetaFunc is a test-replaceable function for store.GetDependentsWithMeta.
var GetDependentsWithMetaFunc = store.GetDependentsWithMeta

// CreateAttemptBeadAtomicFunc is a test-replaceable function for store.CreateAttemptBeadAtomic.
// Used by the cleric summon path to claim recovery beads before spawning.
var CreateAttemptBeadAtomicFunc = store.CreateAttemptBeadAtomic

// StampAttemptInstanceFunc is a test-replaceable function for store.StampAttemptInstance.
var StampAttemptInstanceFunc = store.StampAttemptInstance

// InstanceNameFunc is a test-replaceable function for config.InstanceName.
var InstanceNameFunc = config.InstanceName

// UpdateBeadFunc is a test-replaceable function for store.UpdateBead.
var UpdateBeadFunc = store.UpdateBead

// AddDepTypedFunc is a test-replaceable function for store.AddDepTyped.
var AddDepTypedFunc = store.AddDepTyped

// SendMessageFunc creates a message bead. Test-replaceable.
var SendMessageFunc = sendMessage

// reviewRegistryListFunc is a test-replaceable hook for the local-native
// review-feedback fallback. Production points at agent.RegistryList
// directly; tests stub it so the cluster-native path can assert "no
// registry interaction" without touching the on-disk wizards.json. The
// cluster-native branch in DetectReviewFeedback never calls this — see
// lookupReviewOwner for the cluster-safe ownership surface.
var reviewRegistryListFunc = agent.RegistryList

// OrphanSweepFunc is a test-replaceable hook for the steward-side orphan
// sweep. Production wires it to beadlifecycle.OrphanSweep with the
// daemonLifecycleDeps and localRegistryAdapter from lifecycle_deps.go.
// The sweep was moved here from DaemonTowerCycle (spi-4d2i71) so it
// runs in the same sequential cycle as SweepHookedSteps and cannot
// race with the hooked-resume path across processes.
var OrphanSweepFunc = func() (beadlifecycle.SweepReport, error) {
	return beadlifecycle.OrphanSweep(newDaemonLifecycleDeps(), newLocalRegistryAdapter(), beadlifecycle.OrphanScope{All: true})
}

// RegistryRemoveFunc is a test-replaceable hook for removing a single
// wizard registry entry. Used by the hooked-resume path's belt-and-
// suspenders cleanup (spi-4d2i71): the stale entry for the wizard
// being resumed is removed before the parent bead flips to
// in_progress, so a sync-only daemon (`spire up --no-steward`) cannot
// observe the dead entry and clobber the bead status mid-resume.
var RegistryRemoveFunc = func(ctx context.Context, id string) error {
	return newLocalRegistryAdapter().Remove(ctx, id)
}

// removeStaleWizardEntry deletes the wizard registry entry for wizName
// before the hooked-resume path flips the parent bead to in_progress.
// Belt-and-suspenders defense (spi-4d2i71) for sync-only daemon mode
// (`spire up --no-steward`): if such a daemon runs an orphan sweep
// concurrently with the steward's resume, removing the stale entry
// here means the sweep cannot find a dead PID to mis-classify and
// clobber the bead status. Errors other than not-found are logged and
// otherwise ignored — the primary fix is moving OrphanSweep into the
// same sequential TowerCycle as the resume.
func removeStaleWizardEntry(wizName string) {
	if wizName == "" {
		return
	}
	if err := RegistryRemoveFunc(context.Background(), wizName); err != nil {
		// ErrNotFound is expected when the entry was already cleared
		// (or the wizard never registered); silent.
		if !errors.Is(err, wizardregistry.ErrNotFound) {
			log.Printf("[steward] hooked sweep: remove stale registry entry %s: %s", wizName, err)
		}
	}
}

// shouldRunLocalRegistryOps reports whether the steward may read or
// mutate the local wizard registry (~/.config/spire/wizards.json) for
// the given deployment mode. Only local-native towers may; cluster-
// native towers see pod names that won't appear in the local registry,
// so using it as a liveness oracle would mis-classify live pod
// attempts as orphans (spi-40rtru).
//
// Empty mode counts as local-native to preserve the PhaseDispatch
// zero-value contract documented in cluster_dispatch.go: tests that
// don't exercise cluster-native paths can pass a zero-value
// PhaseDispatch, and the dispatchPhase seam treats zero-value as
// local-native by falling through to backend.Spawn. The stricter
// fail-closed guard for unknown deployment mode lives in TowerCycle's
// modeLoadOK gate, not here.
func shouldRunLocalRegistryOps(mode config.DeploymentMode) bool {
	return mode == "" || mode == config.DeploymentModeLocalNative
}

// CheckExistingAlertFunc checks whether an open corrupted-bead alert already exists.
// Checks both caused-by (current) and related (legacy) deps to find the link.
var CheckExistingAlertFunc = func(beadID string) bool {
	dependents, err := store.GetDependentsWithMeta(beadID)
	if err != nil {
		return false
	}
	for _, dep := range dependents {
		if dep.DependencyType != "caused-by" && dep.DependencyType != beads.DepRelated {
			continue
		}
		if dep.Status == beads.StatusClosed {
			continue
		}
		for _, l := range dep.Labels {
			if l == "alert:corrupted-bead" {
				return true
			}
		}
	}
	return false
}

// stewardBeadOps adapts the steward's test-replaceable function vars to the
// alerts.BeadOps interface so tests can substitute CreateBeadFunc without
// bypassing the alerts ownership boundary.
type stewardBeadOps struct{}

func (stewardBeadOps) CreateBead(opts store.CreateOpts) (string, error) {
	return CreateBeadFunc(opts)
}

func (stewardBeadOps) AddDepTyped(from, to, depType string) error {
	return AddDepTypedFunc(from, to, depType)
}

// CreateAlertFunc creates the alert bead for a corrupted bead and links it via a caused-by dep.
var CreateAlertFunc = func(beadID, msg string) error {
	_, err := alerts.Raise(stewardBeadOps{}, beadID, alerts.ClassAlert, msg,
		alerts.WithSubclass("corrupted-bead"))
	return err
}

// StewardConfig holds configuration for the steward cycle.
type StewardConfig struct {
	DryRun            bool
	NoAssign          bool
	Backend           agent.Backend
	StaleThreshold    time.Duration
	ShutdownThreshold time.Duration
	// DispatchedTimeout is the short-timeout threshold that flips stuck
	// `dispatched` beads back to `ready`. The steward transitions
	// ready→dispatched at emit time, and the wizard is expected to
	// claim (flipping dispatched→in_progress) within a short window
	// once the cluster-native pod starts. If the pod never starts —
	// image pull error, node pressure, etc. — this timeout recovers
	// the bead so another dispatch cycle can pick it up. Distinct
	// from StaleThreshold (which covers long-running in_progress);
	// typical default is ~5m vs StaleThreshold's hours.
	// Zero = disabled (no dispatched stale recovery).
	DispatchedTimeout time.Duration
	AgentList         []string
	MetricsPort       int // 0 = disabled; non-zero = start HTTP metrics server

	ConcurrencyLimiter *ConcurrencyLimiter      // nil = no limit enforcement
	MergeQueue         *MergeQueue              // nil = no merge queue
	TrustChecker       *TrustChecker            // nil = no trust checks
	ABRouter           *ABRouter                // nil = no A/B routing
	CycleStats         *CycleStats              // nil = no stats tracking
	GraphStateStore    executor.GraphStateStore // nil = use default file-backed store

	// ClusterDispatch carries the cluster-native scheduler seams
	// (identity resolver, attempt claimer, intent publisher) used when
	// the tower's EffectiveDeploymentMode is cluster-native. Nil — or a
	// nil field within it — disables cluster-native dispatch and the
	// steward logs and skips. The local-native dispatch path is
	// unaffected.
	//
	// When BuildClusterDispatch is also set, ClusterDispatch acts as a
	// pre-built override (tests use it); production callers leave it nil
	// and set BuildClusterDispatch instead so the config is constructed
	// inside TowerCycle's per-tower DB context.
	ClusterDispatch *ClusterDispatchConfig

	// BuildClusterDispatch, when non-nil, is invoked inside TowerCycle
	// on the cluster-native branch AFTER StoreOpenAtFunc has opened the
	// tower's dolt store (so store.ActiveDB is valid) and BEFORE
	// dispatchClusterNative runs. It returns the fully-populated
	// *ClusterDispatchConfig for this cycle, or nil to fall back to the
	// existing "ClusterDispatch is not configured" skip.
	//
	// The factory pattern exists because the cluster-native seams
	// (SQLRegistryStore, StoreClaimer, DoltPublisher) are all backed by
	// the per-tower *sql.DB that only becomes available once the
	// per-tower store is opened. Building the config eagerly at daemon
	// startup would capture a stale (or nil) DB; building it here means
	// every cycle sees the currently-active tower's connection.
	//
	// When both ClusterDispatch and BuildClusterDispatch are set, the
	// explicit ClusterDispatch wins (test-override path).
	BuildClusterDispatch func(towerName string) *ClusterDispatchConfig
}

// AgentNames extracts agent names from an agent.Info slice.
// If override is provided, it takes priority (explicit --agents flag).
func AgentNames(agents []agent.Info, override []string) []string {
	if len(override) > 0 {
		return override
	}
	seen := make(map[string]bool)
	var names []string
	for _, a := range agents {
		if !seen[a.Name] {
			seen[a.Name] = true
			names = append(names, a.Name)
		}
	}
	return names
}

// BusySet builds a set of agent names that are currently busy.
// An agent is busy if it is alive (has a running process/container/pod).
func BusySet(agents []agent.Info) map[string]bool {
	busy := make(map[string]bool)
	for _, a := range agents {
		if a.Alive {
			busy[a.Name] = true
		}
	}
	return busy
}

// Cycle iterates all towers and runs a steward cycle for each.
func Cycle(cycleNum int, cfg StewardConfig) {
	start := time.Now()
	log.Printf("[steward] ═══ cycle %d ═══════════════════════════════", cycleNum)

	towers, err := config.ListTowerConfigs()
	if err != nil {
		log.Printf("[steward] list towers: %s", err)
		return
	}

	if len(towers) == 0 {
		// Fallback: single-tower mode (no tower configs, use default store).
		TowerCycle(cycleNum, "", cfg)
	} else {
		for _, tower := range towers {
			TowerCycle(cycleNum, tower.Name, cfg)
		}
	}

	log.Printf("[steward] ═══ cycle %d complete (%.1fs) ════════════════", cycleNum, time.Since(start).Seconds())
}

// TowerCycle runs one steward cycle for a specific tower.
// If towerName is "", uses the default store (legacy single-tower mode).
func TowerCycle(cycleNum int, towerName string, cfg StewardConfig) {
	cycleStart := time.Now()
	prefix := ""
	if towerName != "" {
		prefix = "[" + towerName + "] "

		// Open store for this tower's .beads/ directory.
		beadsDir := BeadsDirForTowerFunc(towerName)
		if beadsDir == "" {
			log.Printf("[steward] %sno .beads/ directory found, skipping", prefix)
			return
		}
		if _, err := StoreOpenAtFunc(beadsDir); err != nil {
			log.Printf("[steward] %sopen store: %s", prefix, err)
			return
		}
		defer store.Reset()
		log.Printf("[steward] %s───────────────────────────────", prefix)
	}

	// Step 1: Commit any local changes (pull/push disabled — shared dolt server is source of truth).
	_ = CommitPendingFunc("steward cycle sync")

	// Resolve the tower's deployment mode early so local-registry
	// maintenance below can branch on it. The default tower
	// (towerName=="") implies local-native because the no-tower-config
	// case is by definition single-host. modeLoadOK is the explicit
	// signal for fail-closed semantics: if LoadTowerConfigFunc errors
	// for a named tower we cannot tell whether it's a cluster tower, so
	// the orphan sweep is skipped to avoid clobbering live pod
	// attempts in a misconfigured cluster tower (spi-40rtru). The
	// dispatch step further down reuses these values; loading here
	// keeps a single source of truth per cycle.
	var towerBindings map[string]*config.LocalRepoBinding
	deploymentMode := config.Default()
	modeLoadOK := true
	if towerName != "" {
		tower, tErr := LoadTowerConfigFunc(towerName)
		if tErr != nil {
			log.Printf("[steward] %sload tower config: %s — disabling local-registry maintenance for this cycle", prefix, tErr)
			modeLoadOK = false
		} else {
			towerBindings = tower.LocalBindings
			deploymentMode = tower.EffectiveDeploymentMode()
		}
	}

	// Step 1b: OrphanSweep — close orphaned attempt beads whose wizards are
	// no longer alive. Moved here from DaemonTowerCycle (spi-4d2i71): running
	// it inside TowerCycle puts it in the same sequential cycle as
	// SweepHookedSteps so the two lifecycle ops cannot interleave across the
	// daemon and steward processes. It must run BEFORE GetSchedulableWorkFunc
	// so any beads it reopens are visible to scheduling in the same cycle,
	// and BEFORE SweepHookedSteps so a hooked-resume cannot be clobbered by
	// a stale registry entry mid-cycle.
	//
	// Mode gate (spi-40rtru): the default OrphanSweepFunc consults the
	// local wizard registry for liveness. That registry is per-machine
	// bookkeeping owned by pkg/agent's process backend — cluster-native
	// pod attempts won't appear in it, and treating their absence as
	// "orphan" would close the attempt and reopen the parent
	// erroneously. Skip on cluster-native (and on any non-local mode
	// until a cluster wizardregistry.Registry is injected) and on mode-
	// load failure (fail closed).
	switch {
	case !modeLoadOK:
		// Already logged above.
	case shouldRunLocalRegistryOps(deploymentMode):
		if report, serr := OrphanSweepFunc(); serr != nil {
			log.Printf("[steward] %sorphan sweep error: %s", prefix, serr)
		} else {
			if report.Dead > 0 || report.Cleaned > 0 || len(report.Errors) > 0 {
				log.Printf("[steward] %sorphan sweep: examined %d, dead %d, cleaned %d, errors %d", prefix, report.Examined, report.Dead, report.Cleaned, len(report.Errors))
			}
			for _, rerr := range report.Errors {
				log.Printf("[steward] %sorphan sweep warning: %s", prefix, rerr)
			}
		}
	default:
		log.Printf("[steward] %sskipping local-registry orphan sweep: tower %q mode=%s; cluster registry not yet injected (spi-40rtru)", prefix, towerName, deploymentMode)
	}

	// Step 2: Assess — find schedulable work (ready + policy-filtered).
	schedResult, err := GetSchedulableWorkFunc(beads.WorkFilter{})
	if err != nil {
		log.Printf("[steward] %sready: error — %s", prefix, err)
		pushState()
		return
	}

	// Handle quarantined beads (invariant violations like multiple open attempts).
	for _, q := range schedResult.Quarantined {
		log.Printf("[steward] quarantining %s (multiple open attempts): %v", q.ID, q.Error)
		RaiseCorruptedBeadAlertFunc(q.ID, q.Error)
	}

	schedulable := schedResult.Schedulable

	// Step 3: Load roster and refresh concurrency limiter.
	agents, _ := cfg.Backend.List()
	aliveCount := 0
	for _, a := range agents {
		if a.Alive {
			aliveCount++
		}
	}
	if cfg.ConcurrencyLimiter != nil {
		cfg.ConcurrencyLimiter.Refresh(towerName, agents)
	}

	// Load tower config for MaxConcurrent and database name.
	var maxConcurrent int
	var dbName string
	if tc, err := LoadTowerConfigFunc(towerName); err == nil {
		maxConcurrent = tc.MaxConcurrent
		dbName = tc.Database
	}
	if dbName == "" {
		dbName = DaemonDB
	}
	if dbName == "" {
		dbName, _ = config.DetectDBName()
	}

	log.Printf("[steward] %sready: %d beads | agents: %d alive | max_concurrent: %d",
		prefix, len(schedulable), aliveCount, maxConcurrent)

	// Step 4: Auto-summon — spawn new wizards for schedulable work up to capacity.
	// towerBindings and deploymentMode were resolved at the top of this
	// cycle (so the orphan sweep could gate on mode); reuse them here.

	// Resolve ClusterDispatch lazily for cluster-native mode: if the
	// caller set it explicitly (test override) use that; otherwise invoke
	// the factory, which builds the config against the per-tower DB that
	// was opened above by StoreOpenAtFunc. The resolved value is shared
	// across the bead-level dispatch branch below and the phase-level
	// dispatch seam used by the review/hooked-sweep paths at step 4b/4d
	// — invoking the factory once per cycle, not once per call site.
	cycleClusterDispatch := cfg.ClusterDispatch
	if deploymentMode == config.DeploymentModeClusterNative && cycleClusterDispatch == nil && cfg.BuildClusterDispatch != nil {
		cycleClusterDispatch = cfg.BuildClusterDispatch(towerName)
	}

	// Branch on deployment mode. Local-native runs the existing direct-
	// spawn loop; cluster-native emits WorkloadIntents; attached-reserved
	// is a typed not-implemented surface that skips dispatch entirely.
	spawned := 0
	switch deploymentMode {
	case config.DeploymentModeAttachedReserved:
		log.Printf("[steward] %sattached-reserved: dispatch skipped — %s", prefix, attached.ErrAttachedNotImplemented)

	case config.DeploymentModeClusterNative:
		cycleCfg := cfg
		cycleCfg.ClusterDispatch = cycleClusterDispatch
		spawned = dispatchClusterNative(context.Background(), prefix, schedulable, cycleCfg)

	default: // DeploymentModeLocalNative — the unchanged historical path
		for _, bead := range schedulable {
			// Filter by repo bind state: only spawn for prefixes bound on this instance.
			beadPrefix := beadRepoPrefix(bead.ID)
			if towerBindings != nil {
				binding, ok := towerBindings[beadPrefix]
				if !ok || binding == nil || binding.State != "bound" {
					log.Printf("[steward] %sskipping %s: prefix %s not bound on this instance", prefix, bead.ID, beadPrefix)
					continue
				}
			}

			// Check concurrency limit.
			if cfg.ConcurrencyLimiter != nil && !cfg.ConcurrencyLimiter.CanSpawn(towerName, maxConcurrent) {
				log.Printf("[steward] %sconcurrency limit reached (%d), deferring remaining work", prefix, maxConcurrent)
				break
			}

			if cfg.DryRun {
				log.Printf("[steward] %s[dry-run] would summon wizard for %s", prefix, bead.ID)
				spawned++
				continue
			}

			// A/B routing: select formula variant if experiment is active.
			if cfg.ABRouter != nil {
				if db := GetDBForRoutingFunc(dbName); db != nil {
					formulaName := formula.ResolveV3Name(formula.BeadInfo{
						ID:     bead.ID,
						Type:   bead.Type,
						Labels: bead.Labels,
					})
					variant, _ := cfg.ABRouter.SelectVariant(context.Background(), db, towerName, formulaName, bead.ID)
					if variant != formulaName {
						AddLabelFunc(bead.ID, "formula:"+variant)
						log.Printf("[steward] %sA/B routing: %s → %s", prefix, bead.ID, variant)
					}
					db.Close()
				}
			}

			// Generate wizard name from bead ID.
			wizardName := "wizard-" + SanitizeK8sLabel(bead.ID)

			// Repo bootstrap inputs for the k8s backend's wizard pod init
			// containers (spi-fopwn). Sourced from the tower's LocalBinding
			// for this prefix, which reconcileSharedRepos populates from the
			// shared repos table. Other backends (process, docker) ignore
			// these fields. If the binding is missing fields we log and
			// continue: buildWizardPod will fail-fast with a clear error on
			// the k8s path, and the process backend doesn't need them.
			var repoURL, repoBranch string
			if towerBindings != nil {
				if binding, ok := towerBindings[beadPrefix]; ok && binding != nil {
					repoURL = binding.RepoURL
					repoBranch = binding.SharedBranch
				}
			}

			// Summon the wizard with RoleWizard so the backend can apply the
			// canonical wizard pod spec and resource tier. Include InstanceID so
			// the process backend writes it to the registry entry.
			handle, spawnErr := cfg.Backend.Spawn(agent.SpawnConfig{
				Name:       wizardName,
				BeadID:     bead.ID,
				Role:       agent.RoleWizard,
				Tower:      towerName,
				InstanceID: InstanceIDFunc(),
				LogPath:    filepath.Join(dolt.GlobalDir(), "wizards", wizardName+".log"),
				RepoURL:    repoURL,
				RepoBranch: repoBranch,
				RepoPrefix: beadPrefix,
			})
			if spawnErr != nil {
				log.Printf("[steward] %sspawn failed: %s → %s: %s", prefix, bead.ID, wizardName, spawnErr)
				continue
			}
			if handle != nil {
				log.Printf("[steward] %ssummoned %s for %s (%s)", prefix, wizardName, bead.ID, handle.Identifier())
			}
			spawned++

			// Update concurrency limiter to account for the newly spawned agent
			// so CanSpawn reflects within-cycle spawns.
			if cfg.ConcurrencyLimiter != nil {
				agents = append(agents, agent.Info{Name: wizardName, Alive: true, Tower: towerName})
				cfg.ConcurrencyLimiter.Refresh(towerName, agents)
			}
		}
	}

	if spawned > 0 {
		log.Printf("[steward] %ssummoned: %d wizard(s)", prefix, spawned)
	}

	// Per-phase dispatch seam: the review-routing and hooked-sweep paths
	// need to emit cluster-native WorkloadIntents instead of calling
	// backend.Spawn when the tower's EffectiveDeploymentMode is
	// cluster-native. Bundle the state they need into a PhaseDispatch so
	// each callee can branch through dispatchPhase. Local-native towers
	// get a PhaseDispatch with Mode set; ClusterDispatch is read only on
	// the cluster-native branch. Reuse the same cycleClusterDispatch the
	// bead-level dispatch used above so the factory fires once per cycle.
	phaseDispatch := PhaseDispatch{
		Mode:            deploymentMode,
		ClusterDispatch: cycleClusterDispatch,
	}

	// Step 4b: Detect standalone tasks ready for review.
	DetectReviewReady(cfg.DryRun, cfg.Backend, towerName, towerBindings, phaseDispatch)

	// Step 4c: Detect tasks with review feedback that need wizard re-engagement.
	// Pass the tower's effective deployment mode so the lookup branches
	// correctly: cluster-native fails closed when shared-state owner data is
	// missing, local-native falls back to the legacy wizards.json registry.
	DetectReviewFeedback(cfg.DryRun, deploymentMode)

	// Step 4d: Sweep hooked graph steps.
	// Use configured store if available, otherwise fall back to file-backed store.
	graphStore := cfg.GraphStateStore
	if graphStore == nil {
		graphStore = &executor.FileGraphStateStore{ConfigDir: config.Dir}
	}
	if hookedCount := SweepHookedSteps(cfg.DryRun, cfg.Backend, towerName, graphStore, phaseDispatch); hookedCount > 0 {
		log.Printf("[steward] %shooked sweep: re-summoned %d wizard(s)", prefix, hookedCount)
	}

	// Step 4e: Detect merge-ready beads and enqueue.
	if cfg.MergeQueue != nil {
		DetectMergeReady(cfg.DryRun, cfg.MergeQueue)
	}

	// Step 5: Stale + shutdown check. Pass the resolved deployment mode so
	// cluster-native and attached-reserved towers fail closed (the cluster
	// owner is the authority for shutdowns; local-registry timeout decisions
	// would mis-classify live pod attempts).
	staleCount, shutdownCount := CheckBeadHealth(cfg.StaleThreshold, cfg.ShutdownThreshold, cfg.DryRun, cfg.Backend, deploymentMode)
	if staleCount > 0 || shutdownCount > 0 {
		log.Printf("[steward] %sstale: %d warning(s), %d shutdown(s)", prefix, staleCount, shutdownCount)
	} else {
		log.Printf("[steward] %sstale: none", prefix)
	}

	// Step 5b: Short-timeout stale sweep for beads stuck in `dispatched`.
	// A dispatched bead whose pod never came up (image pull, node pressure)
	// must be recovered back to ready so a subsequent dispatch cycle can
	// retry; otherwise its slot stays occupied under the concurrency cap.
	if cfg.DispatchedTimeout > 0 {
		if reverted := RecoverStaleDispatched(cfg.DispatchedTimeout, cfg.DryRun); reverted > 0 {
			log.Printf("[steward] %sdispatched sweep: reverted %d bead(s) to ready", prefix, reverted)
		}
	}

	// Step 6b: Process merge queue (one per cycle to serialize).
	if cfg.MergeQueue != nil && cfg.MergeQueue.Depth() > 0 {
		result := cfg.MergeQueue.ProcessNext(context.Background(), ExecuteMergeFunc)
		if result != nil {
			if result.Success {
				log.Printf("[steward] %smerge queue: %s merged (%s)", prefix, result.BeadID, result.SHA)
				// Record clean merge for trust.
				if cfg.TrustChecker != nil {
					if db := GetDBForRoutingFunc(dbName); db != nil {
						repoPrefix := beadRepoPrefix(result.BeadID)
						rec, _ := cfg.TrustChecker.RecordAndEvaluate(context.Background(), db, towerName, repoPrefix, true)
						if rec != nil {
							log.Printf("[steward] %strust: %s level=%d consecutive_clean=%d", prefix, repoPrefix, rec.Level, rec.ConsecutiveClean)
						}
						db.Close()
					}
				}
			} else {
				log.Printf("[steward] %smerge queue: %s failed: %s", prefix, result.BeadID, result.Error)
				// Record failed merge for trust.
				if cfg.TrustChecker != nil {
					if db := GetDBForRoutingFunc(dbName); db != nil {
						repoPrefix := beadRepoPrefix(result.BeadID)
						cfg.TrustChecker.RecordAndEvaluate(context.Background(), db, towerName, repoPrefix, false)
						db.Close()
					}
				}
			}
		}
	}

	// Record cycle stats.
	if cfg.CycleStats != nil {
		cfg.CycleStats.Record(CycleStatsSnapshot{
			LastCycleAt:      time.Now(),
			CycleDuration:    time.Since(cycleStart),
			ActiveAgents:     aliveCount,
			QueueDepth:       mergeQueueDepth(cfg.MergeQueue),
			SchedulableWork:  len(schedulable),
			SpawnedThisCycle: spawned,
			Tower:            towerName,
		})
	}

	// Step 7: Push.
	pushState()
}

// BeadsDirForTower finds the .beads/ directory for the given tower name.
// Uses the same pattern as the daemon: doltDataDir()/tower.Database/.beads.
func BeadsDirForTower(towerName string) string {
	towers, err := config.ListTowerConfigs()
	if err != nil {
		return ""
	}
	for _, t := range towers {
		if t.Name == towerName {
			d := filepath.Join(dolt.DataDir(), t.Database, ".beads")
			if info, err := os.Stat(d); err == nil && info.IsDir() {
				return d
			}
			return ""
		}
	}
	return ""
}

// RecoverStaleDispatched flips beads stuck in `dispatched` back to `ready`
// when they have sat past timeout. It exists because the steward
// transitions `ready → dispatched` atomically at emit time, but the
// `dispatched → in_progress` transition is owned by the wizard and only
// happens once the cluster-native workload pod starts and runs
// `spire claim`. If the pod never starts (image pull error, node
// pressure, crash loop), the bead would sit in `dispatched` forever
// while holding an in-flight slot under the concurrency cap.
//
// The threshold is a SHORT timeout (typical default ~5 minutes): long
// enough for a normal pod startup + claim, short enough that a failing
// dispatch frees its slot for another try promptly. Contrast with
// StaleThreshold (hours) used for in_progress recovery.
//
// Returns the number of beads reverted. dryRun logs the action without
// performing the UPDATE.
func RecoverStaleDispatched(timeout time.Duration, dryRun bool) int {
	stuck, err := ListBeadsFunc(beads.IssueFilter{Status: store.StatusPtr(beads.Status(bd.StatusDispatched))})
	if err != nil {
		log.Printf("[steward] recover dispatched: %s", err)
		return 0
	}
	now := time.Now()
	reverted := 0
	for _, b := range stuck {
		if !store.IsWorkBead(b) {
			continue
		}
		if b.UpdatedAt == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, b.UpdatedAt)
		if err != nil {
			t, err = time.Parse("2006-01-02 15:04:05", b.UpdatedAt)
			if err != nil {
				continue
			}
		}
		if now.Sub(t) < timeout {
			continue
		}
		if dryRun {
			log.Printf("[steward] [dry-run] dispatched→ready: %s (%s) age=%s", b.ID, b.Title, now.Sub(t).Round(time.Second))
			reverted++
			continue
		}
		if err := UpdateBeadFunc(b.ID, map[string]interface{}{"status": "ready"}); err != nil {
			log.Printf("[steward] recover dispatched %s: %s", b.ID, err)
			continue
		}
		log.Printf("[steward] dispatched→ready: %s (%s) age=%s", b.ID, b.Title, now.Sub(t).Round(time.Second))
		reverted++
	}
	return reverted
}

func beadHealthValue(v string) string {
	if strings.TrimSpace(v) == "" {
		return "-"
	}
	return v
}

func beadHealthTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func beadHealthOwnerState(owner string, aliveByOwner map[string]bool) string {
	if owner == "" {
		return "missing"
	}
	alive, ok := aliveByOwner[owner]
	if !ok {
		return "unknown"
	}
	if alive {
		return "alive"
	}
	return "dead"
}

func beadHealthContext(
	b store.Bead,
	attempt *store.Bead,
	meta *store.InstanceMeta,
	metaState string,
	heartbeatState string,
	localInstanceID string,
	aliveByOwner map[string]bool,
	owner string,
	clockAt time.Time,
	staleThreshold time.Duration,
	shutdownThreshold time.Duration,
) string {
	attemptID := "-"
	attemptStatus := "-"
	if attempt != nil {
		attemptID = beadHealthValue(attempt.ID)
		attemptStatus = beadHealthValue(attempt.Status)
	}

	attemptInstanceID := "-"
	instanceName := "-"
	sessionID := "-"
	backend := "-"
	tower := "-"
	leaseStartedAt := "-"
	lastSeenAt := "-"
	if meta != nil {
		attemptInstanceID = beadHealthValue(meta.InstanceID)
		instanceName = beadHealthValue(meta.InstanceName)
		sessionID = beadHealthValue(meta.SessionID)
		backend = beadHealthValue(meta.Backend)
		tower = beadHealthValue(meta.Tower)
		leaseStartedAt = beadHealthValue(meta.StartedAt)
		lastSeenAt = beadHealthValue(meta.LastSeenAt)
	}

	return fmt.Sprintf(
		"attempt=%q attempt_status=%q owner=%q owner_state=%q attempt_meta=%q heartbeat_state=%q local_instance=%q attempt_instance=%q instance_name=%q session_id=%q backend=%q tower=%q lease_started_at=%q last_seen_at=%q bead_updated_at=%q clock_at=%q stale_threshold=%s shutdown_threshold=%s",
		attemptID,
		attemptStatus,
		beadHealthValue(owner),
		beadHealthOwnerState(owner, aliveByOwner),
		beadHealthValue(metaState),
		beadHealthValue(heartbeatState),
		beadHealthValue(localInstanceID),
		attemptInstanceID,
		instanceName,
		sessionID,
		backend,
		tower,
		leaseStartedAt,
		lastSeenAt,
		beadHealthValue(b.UpdatedAt),
		beadHealthTime(clockAt),
		staleThreshold,
		shutdownThreshold,
	)
}

// CheckBeadHealth checks in_progress beads against two thresholds:
//   - stale: wizard exceeded guidelines (warning + alert bead)
//   - shutdown: tower kills the wizard via backend.Kill()
//
// The clock is the active attempt bead's last_seen_at heartbeat (written by
// the executor every ~30s) when present — NOT the parent bead's UpdatedAt.
// A long-running implement/review/merge step heartbeats while the parent
// bead may not be mutated for >15m; using the parent clock killed live work
// (spi-9ixgqy).
//
// Behavior in each clock-state:
//   - active attempt with non-empty last_seen_at: heartbeat is the clock;
//     a shutdown will kill only if the backend reports the owner Alive.
//     A stale heartbeat with a dead owner falls to the orphan sweep.
//   - active attempt with empty last_seen_at: conservative — heartbeat may
//     not have fired yet (BeginAttempt + 30s rate-limit window). Never kill;
//     warn only when b.UpdatedAt crosses the stale threshold.
//   - no active attempt: warn-only via b.UpdatedAt, matching the historical
//     "no owner" behavior.
//
// Only attempts owned by this instance are processed. Foreign attempts are
// skipped. Unstamped pre-migration attempts are treated as local for
// backward compatibility.
//
// Mode gate: cluster-native and attached-reserved towers skip this check
// entirely — the cluster owner (pod status, operator) is the authority for
// shutdowns. Empty mode counts as local-native (matches the PhaseDispatch
// zero-value contract).
//
// Returns (staleCount, shutdownCount).
func CheckBeadHealth(staleThreshold, shutdownThreshold time.Duration, dryRun bool, backend agent.Backend, deploymentMode config.DeploymentMode) (int, int) {
	if !shouldRunLocalRegistryOps(deploymentMode) {
		log.Printf("[steward] skipping bead health: tower mode=%s; cluster owner is the authority for shutdowns", deploymentMode)
		return 0, 0
	}

	inProgress, err := ListBeadsFunc(beads.IssueFilter{Status: store.StatusPtr(beads.StatusInProgress)})
	if err != nil {
		log.Printf("[steward] check health: %s", err)
		return 0, 0
	}

	// Build owner→Alive map once per call so the shutdown branch doesn't
	// hit the backend repeatedly.
	aliveByOwner := map[string]bool{}
	if backend != nil {
		if agents, lerr := backend.List(); lerr != nil {
			log.Printf("[steward] check health: backend list: %s", lerr)
		} else {
			for _, a := range agents {
				aliveByOwner[a.Name] = a.Alive
			}
		}
	}

	localInstanceID := InstanceIDFunc()
	now := time.Now()
	staleCount, shutdownCount, foreignCount := 0, 0, 0

	for _, b := range inProgress {
		// Skip internal tracking beads — only top-level work beads need health checks.
		if !store.IsWorkBead(b) {
			continue
		}
		if store.ContainsLabel(b, "review-approved") {
			continue
		}

		// Look up the active attempt first — it (not the parent bead) is
		// the primary source of liveness/clock truth.
		owner := ""
		metaState := "no-attempt"
		heartbeatState := "no-attempt"
		attempt, aErr := GetActiveAttemptFunc(b.ID)
		if aErr != nil {
			// Invariant violation: multiple open attempts. Raise an alert
			// and fall through to the no-attempt path (no owner, warn-only).
			log.Printf("[steward] %s has multiple open attempts (invariant violation): %v", b.ID, aErr)
			RaiseCorruptedBeadAlertFunc(b.ID, aErr)
			attempt = nil
		}

		// Foreign-instance scoping must short-circuit before any clock
		// evaluation: a foreign attempt is some other instance's
		// responsibility.
		var meta *store.InstanceMeta
		if attempt != nil {
			metaState = "unstamped"
			heartbeatState = "unstamped"
			owner = store.HasLabel(*attempt, "agent:")
			m, metaErr := GetAttemptInstanceFunc(attempt.ID)
			if metaErr != nil {
				metaState = "lookup-error"
				heartbeatState = "lookup-error"
				log.Printf("[steward] check health: get instance meta for %s: %s | %s", attempt.ID, metaErr, beadHealthContext(b, attempt, nil, metaState, heartbeatState, localInstanceID, aliveByOwner, owner, time.Time{}, staleThreshold, shutdownThreshold))
			} else if m != nil && m.InstanceID != "" && m.InstanceID != localInstanceID {
				metaState = "present"
				if m.LastSeenAt != "" {
					heartbeatState = "raw"
				} else {
					heartbeatState = "missing"
				}
				log.Printf("[steward] foreign attempt %s on instance %s — skipping health check | %s", attempt.ID, m.InstanceID, beadHealthContext(b, attempt, m, metaState, heartbeatState, localInstanceID, aliveByOwner, owner, time.Time{}, staleThreshold, shutdownThreshold))
				foreignCount++
				continue
			}
			// meta == nil || meta.InstanceID == "": backward compat (unstamped) — treat as local.
			// meta.InstanceID == localInstanceID: local — proceed with health check.
			meta = m
			if meta != nil {
				metaState = "present"
				if meta.LastSeenAt != "" {
					heartbeatState = "raw"
				} else {
					heartbeatState = "missing"
				}
			}
		}

		// Choose the clock. heartbeatDriven == true gates the shutdown
		// branch — anything else is warn-only, no matter how old the bead is.
		var (
			t                time.Time
			heartbeatDriven  bool
			clockSourceLabel string
		)
		if attempt != nil && meta != nil && meta.LastSeenAt != "" {
			parsed, perr := time.Parse(time.RFC3339, meta.LastSeenAt)
			if perr != nil {
				heartbeatState = "parse-error"
				log.Printf("[steward] check health: parse last_seen_at for %s: %s | %s", attempt.ID, perr, beadHealthContext(b, attempt, meta, metaState, heartbeatState, localInstanceID, aliveByOwner, owner, time.Time{}, staleThreshold, shutdownThreshold))
			} else {
				t = parsed
				heartbeatDriven = true
				heartbeatState = "parsed"
				clockSourceLabel = "heartbeat"
			}
		}
		if !heartbeatDriven {
			// Fallback to bead UpdatedAt for warn-only purposes (no
			// heartbeat / no attempt / unparseable heartbeat).
			if b.UpdatedAt == "" {
				log.Printf("[steward] check health: no fallback clock for %s | %s", b.ID, beadHealthContext(b, attempt, meta, metaState, heartbeatState, localInstanceID, aliveByOwner, owner, time.Time{}, staleThreshold, shutdownThreshold))
				continue
			}
			parsed, perr := time.Parse(time.RFC3339, b.UpdatedAt)
			if perr != nil {
				parsed, perr = time.Parse("2006-01-02 15:04:05", b.UpdatedAt)
				if perr != nil {
					log.Printf("[steward] check health: parse bead updated_at for %s: %s | %s", b.ID, perr, beadHealthContext(b, attempt, meta, metaState, heartbeatState, localInstanceID, aliveByOwner, owner, time.Time{}, staleThreshold, shutdownThreshold))
					continue
				}
			}
			t = parsed
			clockSourceLabel = "bead"
		}

		age := now.Sub(t)

		if heartbeatDriven && age > shutdownThreshold {
			// Without an identifiable owner there is no wizard to kill —
			// calling Kill("") just spams "agent \"\" not found in
			// registry" each cycle. Log stale and move on.
			if owner == "" {
				staleCount++
				log.Printf("[steward] STALE (no owner): %s (%s) age=%s clock=%s — not killing, investigate orphan | %s", b.ID, b.Title, age.Round(time.Second), clockSourceLabel, beadHealthContext(b, attempt, meta, metaState, heartbeatState, localInstanceID, aliveByOwner, owner, t, staleThreshold, shutdownThreshold))
				continue
			}
			// Liveness gate: only kill processes the backend reports
			// alive. A dead-owner attempt is reaped by the orphan sweep
			// on its next pass; killing again here is redundant.
			if alive, ok := aliveByOwner[owner]; !ok || !alive {
				staleCount++
				log.Printf("[steward] STALE (owner gone): %s (%s) owner=%s age=%s — orphan sweep will reconcile | %s", b.ID, b.Title, owner, age.Round(time.Second), beadHealthContext(b, attempt, meta, metaState, heartbeatState, localInstanceID, aliveByOwner, owner, t, staleThreshold, shutdownThreshold))
				continue
			}
			shutdownCount++
			if dryRun {
				log.Printf("[steward] [dry-run] SHUTDOWN: %s (%s) owner=%s age=%s clock=%s | %s", b.ID, b.Title, owner, age.Round(time.Second), clockSourceLabel, beadHealthContext(b, attempt, meta, metaState, heartbeatState, localInstanceID, aliveByOwner, owner, t, staleThreshold, shutdownThreshold))
			} else {
				log.Printf("[steward] SHUTDOWN: %s (%s) owner=%s age=%s clock=%s — killing wizard | %s", b.ID, b.Title, owner, age.Round(time.Second), clockSourceLabel, beadHealthContext(b, attempt, meta, metaState, heartbeatState, localInstanceID, aliveByOwner, owner, t, staleThreshold, shutdownThreshold))
				if killErr := backend.Kill(owner); killErr != nil {
					log.Printf("[steward] kill %s: %s", owner, killErr)
				}
			}
		} else if age > staleThreshold {
			// Warn-only path. Reasons we may be here:
			//   - heartbeat-driven and age in (stale, shutdown]
			//   - attempt without heartbeat data (be conservative: never kill)
			//   - no active attempt at all (no owner to kill)
			staleCount++
			label := "STALE"
			switch {
			case attempt != nil && !heartbeatDriven:
				label = "STALE (no heartbeat)"
			case attempt == nil:
				label = "STALE (no attempt)"
			}
			if dryRun {
				log.Printf("[steward] [dry-run] %s: %s (%s) owner=%s age=%s clock=%s | %s", label, b.ID, b.Title, owner, age.Round(time.Second), clockSourceLabel, beadHealthContext(b, attempt, meta, metaState, heartbeatState, localInstanceID, aliveByOwner, owner, t, staleThreshold, shutdownThreshold))
			} else {
				log.Printf("[steward] %s: %s (%s) owner=%s age=%s clock=%s | %s", label, b.ID, b.Title, owner, age.Round(time.Second), clockSourceLabel, beadHealthContext(b, attempt, meta, metaState, heartbeatState, localInstanceID, aliveByOwner, owner, t, staleThreshold, shutdownThreshold))
			}
		}
	}

	if foreignCount > 0 {
		log.Printf("[steward] check health: skipped %d foreign attempt(s)", foreignCount)
	}

	return staleCount, shutdownCount
}

// CleanUpdatedLabels removes stale updated:<timestamp> labels from open/in_progress
// beads. These labels were written by the old touchUpdatedLabel() heartbeat mechanism
// and are no longer used — CheckBeadHealth now reads the active attempt's
// last_seen_at metadata as the primary clock (with b.UpdatedAt only as a
// warn-only fallback).
func CleanUpdatedLabels() int {
	all, err := ListBeadsFunc(beads.IssueFilter{
		ExcludeStatus: []beads.Status{beads.StatusClosed},
	})
	if err != nil {
		log.Printf("[steward] clean updated labels: list beads: %s", err)
		return 0
	}

	cleaned := 0
	for _, b := range all {
		label := store.HasLabel(b, "updated:")
		if label == "" {
			continue
		}
		if err := RemoveLabelFunc(b.ID, "updated:"+label); err != nil {
			log.Printf("[steward] clean updated label from %s: %s", b.ID, err)
			continue
		}
		cleaned++
	}
	if cleaned > 0 {
		log.Printf("[steward] cleaned %d stale updated: labels", cleaned)
	}
	return cleaned
}

// DetectReviewReady finds in_progress beads that need review routing.
// A bead is ready for review when:
//   - It has a closed implement step bead (from workflow molecule), AND
//   - It has no review-round child beads (first review), OR all review-round
//     beads are closed with verdict "approve" (re-review after merge failure)
//   - It has no active (in_progress) review-round bead (review already running)
//
// This replaces the legacy label-based query (review-ready label).
//
// pd carries the tower's deployment-mode seam: when the mode is
// cluster-native and pd.ClusterDispatch is populated, review dispatch
// emits a PhaseReview WorkloadIntent instead of calling backend.Spawn
// directly. Tests that exercise only local-native behavior can pass a
// zero-value PhaseDispatch.
//
// towerBindings maps a bead prefix to its LocalRepoBinding; used to
// source the canonical repo identity (RepoURL/BaseBranch/local path)
// that executor.PopulateRuntimeContract stamps onto SpawnConfig before
// reaching the backend. Pass nil when the tower is unknown (legacy
// single-tower mode) — the helper falls back to the binding's
// SharedBranch of "" and PopulateRuntimeContract fills kind=repo
// defaults for the workspace so cluster-backend validation still sees
// non-empty Identity/Workspace fields.
func DetectReviewReady(dryRun bool, backend agent.Backend, towerName string, towerBindings map[string]*config.LocalRepoBinding, pd PhaseDispatch) {
	inProgress, err := ListBeadsFunc(beads.IssueFilter{Status: store.StatusPtr(beads.StatusInProgress)})
	if err != nil {
		log.Printf("[steward] detectReviewReady: %s", err)
		return
	}

	for _, b := range inProgress {
		// Skip child beads (step beads, review-round beads, attempt beads).
		if store.IsStepBead(b) || store.IsReviewRoundBead(b) || store.ContainsLabel(b, "attempt") {
			continue
		}

		// Check if implement step is closed.
		steps, sErr := GetStepBeadsFunc(b.ID)
		if sErr != nil || len(steps) == 0 {
			continue // no workflow molecule — not eligible
		}
		implClosed := false
		for _, s := range steps {
			if store.StepBeadPhaseName(s) == "implement" && s.Status == "closed" {
				implClosed = true
				break
			}
		}
		if !implClosed {
			continue
		}

		// Check review-round beads.
		reviews, rErr := GetReviewBeads(b.ID)
		if rErr != nil {
			continue
		}

		// If there's an active (in_progress) review bead, a review is already running.
		hasActiveReview := false
		for _, r := range reviews {
			if r.Status == "in_progress" {
				hasActiveReview = true
				break
			}
		}
		if hasActiveReview {
			continue
		}

		// If there are closed review beads, check the last one's verdict.
		if len(reviews) > 0 {
			lastReview := reviews[len(reviews)-1]
			verdict := ReviewBeadVerdict(lastReview)
			// Only re-route if last verdict was "approve" (merge may have failed).
			// "request_changes" is handled by DetectReviewFeedback.
			if verdict != "approve" {
				continue
			}
		}

		// Skip if already approved (label still present from verdict-only mode).
		if store.ContainsLabel(b, "review-approved") {
			continue
		}

		if dryRun {
			log.Printf("[steward] [dry-run] would route %s to review pod", b.ID)
			continue
		}

		log.Printf("[steward] routing %s for review (round %d)", b.ID, len(reviews)+1)

		reviewerName := "reviewer-" + SanitizeK8sLabel(b.ID)

		sc := agent.SpawnConfig{
			Name:       reviewerName,
			BeadID:     b.ID,
			Role:       agent.RoleSage,
			Tower:      towerName,
			InstanceID: InstanceIDFunc(),
			LogPath:    filepath.Join(dolt.GlobalDir(), "wizards", reviewerName+".log"),
		}

		// Populate the canonical runtime-contract fields. Cluster backends
		// reject any SpawnConfig with empty Identity/Workspace at
		// buildSubstratePod (ErrIdentityRequired / ErrWorkspaceRequired).
		// Sage review is a same-owner read of the implement workspace, so
		// HandoffBorrowed is the correct delivery semantic — the reviewer
		// does not produce commits (see pkg/executor/graph_actions.go
		// wizardRunSpawn which uses the same default for sage-review).
		beadPrefix := beadRepoPrefix(b.ID)
		var repoURL, repoPath, baseBranch string
		if binding, ok := towerBindings[beadPrefix]; ok && binding != nil {
			repoURL = binding.RepoURL
			repoPath = binding.LocalPath
			baseBranch = binding.SharedBranch
		}
		sc, contractErr := executor.PopulateRuntimeContract(sc, executor.RuntimeContractInputs{
			TowerName:   towerName,
			RepoURL:     repoURL,
			RepoPath:    repoPath,
			BaseBranch:  baseBranch,
			RunStep:     "review",
			Backend:     agent.ResolveBackendName(repoPath),
			HandoffMode: executor.HandoffBorrowed,
			Log: func(format string, args ...interface{}) {
				log.Printf("[steward] "+format, args...)
			},
		})
		if contractErr != nil {
			log.Printf("[steward] failed to populate runtime contract for %s: %v", b.ID, contractErr)
			continue
		}

		handle, spawnErr := dispatchPhase(context.Background(), pd, backend, sc, intent.PhaseReview)
		if spawnErr != nil {
			log.Printf("[steward] failed to route reviewer for %s: %v", b.ID, spawnErr)
		} else if handle != nil {
			log.Printf("[steward] spawned reviewer %s for %s (%s)", reviewerName, b.ID, handle.Identifier())
		} else {
			log.Printf("[steward] emitted review intent for %s (phase=%s)", b.ID, intent.PhaseReview)
		}
	}
}

// DetectMergeReady scans in_progress beads with the "review-approved" label
// and a "feat-branch:" label, resolves the repo path from config, and enqueues
// a MergeRequest for each eligible bead. Skips beads already in the queue.
func DetectMergeReady(dryRun bool, mq *MergeQueue) {
	inProgress, err := ListBeadsFunc(beads.IssueFilter{Status: store.StatusPtr(beads.StatusInProgress)})
	if err != nil {
		log.Printf("[steward] detectMergeReady: %s", err)
		return
	}

	for _, b := range inProgress {
		if !store.IsWorkBead(b) {
			continue
		}
		if !store.ContainsLabel(b, "review-approved") {
			continue
		}
		if mq.Contains(b.ID) {
			continue
		}

		// Extract branch from feat-branch: label.
		branch := store.HasLabel(b, "feat-branch:")
		if branch == "" {
			continue
		}

		// Resolve repo path from config.
		cfg, err := ConfigLoadFunc()
		if err != nil {
			log.Printf("[steward] detectMergeReady: load config: %s", err)
			continue
		}
		prefix := beadRepoPrefix(b.ID)
		inst, ok := cfg.Instances[prefix]
		if !ok || inst.Path == "" {
			log.Printf("[steward] detectMergeReady: no registered repo for prefix %q (bead %s), skipping", prefix, b.ID)
			continue
		}

		// Resolve base branch: bead's base-branch: label overrides, else default.
		baseBranch := store.HasLabel(b, "base-branch:")
		if baseBranch == "" {
			baseBranch = repoconfig.DefaultBranchBase
		}

		if dryRun {
			log.Printf("[steward] [dry-run] would enqueue %s for merge (%s → %s)", b.ID, branch, baseBranch)
			continue
		}

		mq.Enqueue(MergeRequest{
			BeadID:     b.ID,
			Branch:     branch,
			BaseBranch: baseBranch,
			RepoPath:   inst.Path,
			EnqueuedAt: time.Now(),
		})
		log.Printf("[steward] enqueued %s for merge (%s → %s)", b.ID, branch, baseBranch)
	}
}

// ReviewBeadVerdict extracts the verdict string from a closed review-round bead.
// Precedence: arbiter_verdict (binding) > review_verdict (sage) >
// description prefix "verdict: <value>" (legacy beads).
func ReviewBeadVerdict(b store.Bead) string {
	if v := arbiterVerdictFromMeta(b); v != "" {
		return v
	}
	if v := b.Meta("review_verdict"); v != "" {
		return v
	}
	// Legacy fallback: parse description.
	if b.Description == "" {
		return ""
	}
	if strings.HasPrefix(b.Description, "verdict: ") {
		line := b.Description
		if idx := strings.Index(line, "\n"); idx >= 0 {
			line = line[:idx]
		}
		return strings.TrimPrefix(line, "verdict: ")
	}
	return ""
}

// arbiterVerdictFromMeta parses the "arbiter_verdict" metadata JSON payload
// and returns its verdict field. Returns "" when the key is absent or the
// payload is unparseable — callers fall back to the sage-written
// review_verdict in either case.
func arbiterVerdictFromMeta(b store.Bead) string {
	raw := b.Meta("arbiter_verdict")
	if raw == "" {
		return ""
	}
	var payload struct {
		Verdict string `json:"verdict"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return ""
	}
	return payload.Verdict
}

// ReviewOwnerRef identifies the agent that owns a bead's in-progress work
// for the purpose of review-feedback re-engagement. It is the cluster-safe
// alternative to a local-registry lookup: the fields are sourced from the
// shared bead store (Dolt-backed) and are visible to every steward replica.
//
// AgentID is the wizard agent name (e.g. "wizard-spi-abcd"). It matches the
// Name field on a registry entry on local-native, but is sourced from the
// attempt bead's `agent:<name>` label rather than from the in-process
// registry so the value is correct on cluster replicas that never wrote
// the registry file.
//
// AttemptID is the bead ID of the most recent attempt bead — empty when
// no attempt bead exists. The .3 dispatch follow-up uses this to thread a
// (task_id, dispatch_seq) intent through to the operator; today it is
// produced and observed but not consumed by the message-only re-engagement
// path.
type ReviewOwnerRef struct {
	AgentID   string
	AttemptID string
}

// IsZero reports whether the ref carries no usable ownership data.
func (r ReviewOwnerRef) IsZero() bool {
	return r.AgentID == "" && r.AttemptID == ""
}

// lookupReviewOwner resolves the agent that owns a bead's in-progress work
// via shared state (Dolt-backed). This is the cluster-safe surface; the
// local in-process registry is local-only and must not be referenced from
// cluster control paths.
//
// The lookup picks the highest-numbered attempt bead — open or closed —
// and reads its `agent:<name>` label. The attempt bead is the canonical
// shared-state ownership surface (see pkg/steward/README.md "Bead status
// lifecycle" and the rule "Do not use registry-based duplicate detection
// for spawn decisions"). For request-changes re-entry the active attempt
// has already closed, so this function deliberately does not require an
// open attempt; it walks all attempt children and returns the most
// recent. spi-5bzu9r.1's runtime contract extends WorkloadIntent with
// explicit Role/Phase/Runtime fields but does not introduce a separate
// `implemented-by` typed dep, so attempt metadata remains the
// authoritative shared-state surface for review re-engagement.
//
// Returns a zero ReviewOwnerRef (with nil error) when no attempt bead
// exists. The caller decides how to handle that — cluster-native fails
// closed, local-native may consult the legacy wizards.json registry.
func lookupReviewOwner(beadID string) (ReviewOwnerRef, error) {
	children, err := GetChildrenFunc(beadID)
	if err != nil {
		return ReviewOwnerRef{}, fmt.Errorf("get children for %s: %w", beadID, err)
	}
	var latest *store.Bead
	latestN := -1
	for i := range children {
		child := children[i]
		if !store.IsAttemptBead(child) {
			continue
		}
		n := store.AttemptNumber(child)
		if n > latestN {
			latest = &children[i]
			latestN = n
		}
	}
	if latest == nil {
		return ReviewOwnerRef{}, nil
	}
	return ReviewOwnerRef{
		AgentID:   store.HasLabel(*latest, "agent:"),
		AttemptID: latest.ID,
	}, nil
}

// DetectReviewFeedback finds in_progress beads whose last review-round bead
// is closed with verdict "request_changes" and no active attempt bead (wizard
// not already working on it). It re-engages the owning wizard so the
// feedback can be addressed.
//
// mode gates the ownership lookup. In cluster-native, ownership is sourced
// from shared state via lookupReviewOwner and the function fails closed
// when no owner is found — the local wizards.json registry is never
// consulted because cluster replicas don't write it. In local-native, the
// same shared-state lookup runs first; the registry is only used as a
// legacy fallback for towers that haven't migrated their attempt-bead
// writes yet. The "actually re-engage this owner" mechanic (intent
// emission, dispatcher call) is owned by spi-5bzu9r.3; this function
// stops at producing the owner ref and emitting the message.
func DetectReviewFeedback(dryRun bool, mode config.DeploymentMode) {
	inProgress, err := ListBeadsFunc(beads.IssueFilter{Status: store.StatusPtr(beads.StatusInProgress)})
	if err != nil {
		log.Printf("[steward] detectReviewFeedback: %s", err)
		return
	}

	for _, b := range inProgress {
		// Skip child beads.
		if store.IsStepBead(b) || store.IsReviewRoundBead(b) || store.ContainsLabel(b, "attempt") {
			continue
		}

		// Check review-round beads.
		reviews, rErr := GetReviewBeads(b.ID)
		if rErr != nil || len(reviews) == 0 {
			continue
		}

		lastReview := reviews[len(reviews)-1]
		// Must be closed with request_changes verdict.
		if lastReview.Status != "closed" || ReviewBeadVerdict(lastReview) != "request_changes" {
			continue
		}

		// Skip if there's already an active attempt (wizard already working on it).
		// Fail closed: if GetActiveAttemptFunc returns an error (e.g. multiple
		// open attempts), skip re-engagement and raise an alert.
		reEngageAttempt, reEngageErr := GetActiveAttemptFunc(b.ID)
		if reEngageErr != nil {
			log.Printf("[steward] quarantining %s (multiple open attempts): %v", b.ID, reEngageErr)
			RaiseCorruptedBeadAlertFunc(b.ID, reEngageErr)
			continue
		}
		if reEngageAttempt != nil {
			continue
		}

		if dryRun {
			log.Printf("[steward] [dry-run] would re-engage wizard for %s (review feedback)", b.ID)
			continue
		}

		log.Printf("[steward] re-engaging wizard for %s (review feedback round %d)", b.ID, len(reviews))

		// Resolve the wizard owner from the shared-state surface (attempt
		// beads). Cluster control paths must NOT fall back to the local
		// in-process registry — its entries are process-local and have no
		// meaning across cluster replicas.
		owner := ""
		ownerRef, lookupErr := lookupReviewOwner(b.ID)
		if lookupErr != nil {
			log.Printf("[steward] review-feedback: lookup owner for %s: %v", b.ID, lookupErr)
		}
		owner = ownerRef.AgentID

		if owner == "" {
			if mode == config.DeploymentModeClusterNative {
				// Fail closed: cluster-native requires a shared-state
				// ownership surface. Skipping with a diagnostic is
				// safer than re-engaging an unknown wizard or leaking
				// through local-only paths. The diagnostic names the
				// queried surface (attempt beads) so a human can
				// investigate the gap.
				log.Printf("[steward] review-feedback: cluster-native lookup found no owner for %s (queried: attempt-bead agent: label) — skipping re-engagement", b.ID)
				continue
			}
			// LOCAL-ONLY: registry is process-local; cluster control
			// paths use lookupReviewOwner and never reach this branch.
			if regEntries, regErr := reviewRegistryListFunc(); regErr == nil {
				for _, w := range regEntries {
					if w.BeadID == b.ID {
						owner = w.Name
						break
					}
				}
			}
			if owner == "" {
				owner = "wizard"
			}
		}

		msg := fmt.Sprintf("Review feedback on %s: %s — please address feedback on the existing branch and push again", b.ID, b.Title)
		if _, err := SendMessageFunc(owner, "steward", msg, b.ID, b.Priority); err != nil {
			log.Printf("[steward] failed to re-engage wizard for %s: %v", b.ID, err)
			continue
		}
	}
}

// SweepHookedSteps finds graph states with hooked steps and re-summons
// wizards when the hooked condition is resolved (e.g., design bead closed).
//
// pd carries the tower's deployment-mode seam: when the mode is
// cluster-native and pd.ClusterDispatch is populated, re-summon emits a
// bead-level WorkloadIntent instead of calling backend.Spawn directly —
// the operator then reconciles a fresh wizard pod for the resumed bead.
// The cleric-summon branch for recovery beads (a different bead than the
// hooked parent) is also routed through the same seam. Tests that only
// exercise local-native behavior can pass a zero-value PhaseDispatch.
func SweepHookedSteps(dryRun bool, backend agent.Backend, towerName string, graphStore executor.GraphStateStore, pd PhaseDispatch) int {
	resummoned := 0

	// --- Primary path: bead-status-driven sweep ---
	// Query for work beads with status='hooked'. For each, find child step beads
	// with status='hooked' and check if their hook condition is resolved.
	hookedStatus := beads.Status("hooked")
	hookedParents, err := ListBeadsFunc(beads.IssueFilter{Status: &hookedStatus})
	if err != nil {
		log.Printf("[steward] hooked sweep: list hooked beads: %s", err)
	}

	// Track which bead IDs we resolved via bead-status path so the graph-state
	// fallback can skip them.
	resolvedBeadIDs := make(map[string]bool)

	localInstanceID := InstanceIDFunc()

	for _, parent := range hookedParents {
		// Only sweep work types (task, bug, feature, epic).
		switch parent.Type {
		case "task", "bug", "feature", "epic":
		default:
			continue
		}

		// Restrict auto-resume to locally-owned attempts.
		attempt, aErr := GetActiveAttemptFunc(parent.ID)
		if aErr == nil && attempt != nil {
			owned, oErr := IsOwnedByInstanceFunc(attempt.ID, localInstanceID)
			if oErr != nil {
				log.Printf("[steward] hooked sweep: check ownership for %s: %s", attempt.ID, oErr)
			} else if !owned {
				log.Printf("[steward] hooked sweep: skipping %s — foreign attempt %s", parent.ID, attempt.ID)
				continue
			}
		}

		// Find hooked step bead children.
		hookedSteps, err := GetHookedStepsFunc(parent.ID)
		if err != nil {
			log.Printf("[steward] hooked sweep: get hooked steps for %s: %s", parent.ID, err)
			continue
		}
		if len(hookedSteps) == 0 {
			continue
		}

		// Load graph state to get outputs and agent name for condition checking.
		// Try to find the agent name from the graph state store.
		var gs *executor.GraphState
		var agentName string

		// Scan graph state entries to find the one matching this bead.
		entries, _ := graphStore.ListHooked(towerName)
		for _, entry := range entries {
			if entry.BeadID == parent.ID {
				gs = entry.State
				agentName = entry.AgentName
				break
			}
		}

		anyResolved := false
		for _, stepBead := range hookedSteps {
			stepName := store.StepBeadPhaseName(stepBead)
			if stepName == "" {
				continue
			}

			// Resolve the hooked condition using graph state outputs if available.
			// Two hook types:
			//   a) check.design-linked: design_ref output → check if design bead is closed with content
			//   b) human.approve: no design_ref → check if awaiting-approval label was cleared
			var designRef string
			if gs != nil {
				if ss, ok := gs.Steps[stepName]; ok {
					designRef = ss.Outputs["design_ref"]
				}
			}

			resolved := false
			if designRef != "" {
				// Design-linked hook: check if design bead is now closed with content.
				designBead, err := GetBeadFunc(designRef)
				if err != nil {
					log.Printf("[steward] hooked sweep: get design bead %s: %s", designRef, err)
					continue
				}
				if designBead.Status != "closed" {
					continue // still waiting
				}
				comments, _ := GetCommentsFunc(designRef)
				if len(comments) == 0 && designBead.Description == "" {
					continue // closed but empty
				}
				log.Printf("[steward] hooked sweep: design bead %s resolved for %s step %s", designRef, parent.ID, stepName)
				resolved = true
			} else {
				// Human approval hook (or other label-based hook): check if
				// awaiting-approval and needs-human labels have been cleared.
				parentBead, err := GetBeadFunc(parent.ID)
				if err != nil {
					log.Printf("[steward] hooked sweep: get bead %s: %s", parent.ID, err)
					continue
				}
				if !store.ContainsLabel(parentBead, "awaiting-approval") && !store.ContainsLabel(parentBead, "needs-human") {
					log.Printf("[steward] hooked sweep: approval labels cleared for %s step %s", parent.ID, stepName)
					resolved = true
				} else {
					continue // still waiting for approval
				}
			}

			if !resolved {
				continue
			}

			if dryRun {
				log.Printf("[steward] [dry-run] would unhook step %s (%s) and re-summon for %s", stepName, stepBead.ID, parent.ID)
				anyResolved = true
				continue
			}

			// 1. Unhook the step bead (status hooked → open).
			if err := UnhookStepBeadFunc(stepBead.ID); err != nil {
				log.Printf("[steward] hooked sweep: unhook step bead %s: %s", stepBead.ID, err)
				continue
			}

			// Also reset the graph state step to pending if we have it.
			if gs != nil {
				if ss, ok := gs.Steps[stepName]; ok {
					ss.Status = "pending"
					ss.Outputs = nil
					ss.StartedAt = ""
					ss.CompletedAt = ""
					gs.Steps[stepName] = ss
					if err := graphStore.Save(agentName, gs); err != nil {
						log.Printf("[steward] hooked sweep: save graph state for %s: %s", agentName, err)
					}
				}
			}

			anyResolved = true
			break // one hooked step per parent is enough per cycle
		}

		if !anyResolved {
			// Failure-evidence path: check if this hooked bead has a recovery/alert
			// bead linked via caused-by. If so, summon a cleric (or detect that the
			// cleric already succeeded).
			evidence, found := findFailureEvidence(parent.ID)
			if !found {
				continue // not a failure hook — nothing to do
			}

			// Check if the recovery bead is already closed (cleric finished).
			recoveryBead, rbErr := GetBeadFunc(evidence.RecoveryBeadID)
			if rbErr != nil {
				log.Printf("[steward] hooked sweep: get recovery bead %s: %s", evidence.RecoveryBeadID, rbErr)
				continue
			}

			// awaiting_review (cleric runtime, spi-hhkozk): the cleric has
			// posted a proposal and the human review owns the gate. The
			// steward must NOT redispatch — leave the bead alone until the
			// gateway sets the gate output and flips status back to
			// in_progress (or closes via cleric.takeover).
			if recoveryBead.Status == "awaiting_review" {
				log.Printf("[steward] hooked sweep: recovery %s awaiting_review — human gate owns it, skipping", evidence.RecoveryBeadID)
				continue
			}

			if recoveryBead.Status == "closed" {
				// Cleric finished. Check resolution: a closed recovery bead
				// with the new cleric.finish outcome (cleric_outcome=
				// approve+executed) OR the legacy DecisionResume outcome
				// resumes the wizard. A bead with the new takeover outcome
				// (source carries needs-manual label) is left hooked for the
				// human. Beads without ANY recorded outcome default to the
				// leave-hooked path for safety.
				if !recoveryShouldResume(recoveryBead) {
					log.Printf("[steward] hooked sweep: recovery %s did not resolve to resume (takeover or no outcome) for %s — leaving hooked for human", evidence.RecoveryBeadID, parent.ID)
					continue
				}

				// Success path: unhook all hooked steps and resummon wizard.
				log.Printf("[steward] hooked sweep: recovery %s succeeded for %s — unhooking and resuming", evidence.RecoveryBeadID, parent.ID)

				if dryRun {
					log.Printf("[steward] [dry-run] would unhook %s after cleric success and re-summon wizard", parent.ID)
					resolvedBeadIDs[parent.ID] = true
					resummoned++
					continue
				}

				for _, stepBead := range hookedSteps {
					if uhErr := UnhookStepBeadFunc(stepBead.ID); uhErr != nil {
						log.Printf("[steward] hooked sweep: unhook step bead %s: %s", stepBead.ID, uhErr)
					}
					// Reset graph state step to pending.
					stepName := store.StepBeadPhaseName(stepBead)
					if gs != nil && stepName != "" {
						if ss, ok := gs.Steps[stepName]; ok {
							ss.Status = "pending"
							ss.Outputs = nil
							ss.StartedAt = ""
							ss.CompletedAt = ""
							gs.Steps[stepName] = ss
						}
					}
				}
				if gs != nil {
					if err := graphStore.Save(agentName, gs); err != nil {
						log.Printf("[steward] hooked sweep: save graph state for %s: %s", agentName, err)
					}
				}

				// Resolve the resumed wizard name first so the stale
				// registry row can be cleared BEFORE the parent bead
				// flips to in_progress (spi-4d2i71). If the entry is
				// removed before the status update, no concurrent
				// orphan sweep can find a dead PID to declare against
				// this bead during the resume window.
				//
				// Mode gate (spi-40rtru): the local registry is
				// per-machine bookkeeping; for cluster-native towers
				// the wizard runs in a pod and has no entry to clear,
				// so the call would touch wizards.json for a name
				// that has no business being there.
				wizName := agentName
				if wizName == "" {
					wizName = "wizard-" + SanitizeK8sLabel(parent.ID)
				}
				if shouldRunLocalRegistryOps(pd.Mode) {
					removeStaleWizardEntry(wizName)
				}

				// Set parent bead back to in_progress.
				if err := UpdateBeadFunc(parent.ID, map[string]interface{}{
					"status": "in_progress",
				}); err != nil {
					log.Printf("[steward] hooked sweep: set %s to in_progress: %s", parent.ID, err)
				} else {
					log.Printf("[steward] hooked sweep: %s no longer hooked, set to in_progress", parent.ID)
				}

				// Re-summon the wizard.
				handle, spawnErr := dispatchPhase(context.Background(), pd, backend, agent.SpawnConfig{
					Name:       wizName,
					BeadID:     parent.ID,
					Role:       agent.RoleWizard,
					Tower:      towerName,
					InstanceID: localInstanceID,
					LogPath:    filepath.Join(dolt.GlobalDir(), "wizards", wizName+".log"),
				}, hookedResumePhase(parent.Type))
				if spawnErr != nil {
					log.Printf("[steward] hooked sweep: dispatch %s: %s", wizName, spawnErr)
				} else if handle != nil {
					log.Printf("[steward] hooked sweep: re-summoned %s for %s after cleric success (%s)", wizName, parent.ID, handle.Identifier())
				} else {
					log.Printf("[steward] hooked sweep: emitted resume intent for %s after cleric success", parent.ID)
				}
				resolvedBeadIDs[parent.ID] = true
				resummoned++
				continue
			}

			// Recovery bead is still open — claim it before spawning a cleric.
			clericName := "cleric-" + SanitizeK8sLabel(evidence.RecoveryBeadID)

			if dryRun {
				log.Printf("[steward] [dry-run] would summon cleric for recovery %s (source %s)", evidence.RecoveryBeadID, parent.ID)
				resummoned++
				continue
			}

			// Claim the recovery bead atomically. CreateAttemptBeadAtomic
			// rejects if another agent already has an active attempt,
			// preventing double-summon across instances.
			attemptID, claimErr := CreateAttemptBeadAtomicFunc(evidence.RecoveryBeadID, clericName, "", "")
			if claimErr != nil {
				log.Printf("[steward] hooked sweep: recovery %s already claimed: %s", evidence.RecoveryBeadID, claimErr)
				continue
			}

			// Stamp instance ownership on the attempt bead.
			now := time.Now().UTC().Format(time.RFC3339)
			if stampErr := StampAttemptInstanceFunc(attemptID, store.InstanceMeta{
				InstanceID:   localInstanceID,
				InstanceName: InstanceNameFunc(),
				Backend:      "process",
				Tower:        towerName,
				StartedAt:    now,
				LastSeenAt:   now,
			}); stampErr != nil {
				log.Printf("[steward] hooked sweep: stamp instance on %s: %s", attemptID, stampErr)
			}

			// Set recovery bead to in_progress.
			if upErr := UpdateBeadFunc(evidence.RecoveryBeadID, map[string]interface{}{
				"status": "in_progress",
			}); upErr != nil {
				log.Printf("[steward] hooked sweep: set recovery %s to in_progress: %s", evidence.RecoveryBeadID, upErr)
			}

			log.Printf("[steward] hooked sweep: summoning cleric %s for recovery %s (source %s)", clericName, evidence.RecoveryBeadID, parent.ID)

			// Dispatch the recovery bead (not the parent) — the cleric's
			// workload is the recovery bead itself. The recovery bead's
			// type ("recovery") is not a bead-level phase the operator
			// routes, so use clericDispatchPhase() for a phase the
			// cluster intent contract recognizes. The local backend
			// ignores FormulaPhase; only cluster-native dispatch reads
			// it.
			handle, spawnErr := dispatchPhase(context.Background(), pd, backend, agent.SpawnConfig{
				Name:       clericName,
				BeadID:     evidence.RecoveryBeadID,
				Role:       agent.RoleCleric,
				Tower:      towerName,
				InstanceID: localInstanceID,
				LogPath:    filepath.Join(dolt.GlobalDir(), "wizards", clericName+".log"),
			}, clericDispatchPhase())
			if spawnErr != nil {
				log.Printf("[steward] hooked sweep: dispatch cleric %s: %s", clericName, spawnErr)
			} else if handle != nil {
				log.Printf("[steward] hooked sweep: summoned cleric %s for %s (%s)", clericName, evidence.RecoveryBeadID, handle.Identifier())
			} else {
				log.Printf("[steward] hooked sweep: emitted cleric intent for recovery %s", evidence.RecoveryBeadID)
			}
			resummoned++
			continue
		}

		resolvedBeadIDs[parent.ID] = true

		if dryRun {
			resummoned++
			continue
		}

		// Resolve the resumed wizard name and clear its stale registry
		// row BEFORE flipping the parent bead status (spi-4d2i71).
		// Belt-and-suspenders defense for sync-only daemon mode: if a
		// daemon-side orphan sweep ever runs concurrently here, the
		// stale entry is already gone, so it cannot mis-classify the
		// bead as orphaned during the resume window.
		//
		// Mode gate (spi-40rtru): the local registry is per-machine
		// bookkeeping; cluster-native pods register elsewhere and
		// have no entry to clear here.
		if agentName == "" {
			agentName = "wizard-" + SanitizeK8sLabel(parent.ID)
		}
		if shouldRunLocalRegistryOps(pd.Mode) {
			removeStaleWizardEntry(agentName)
		}

		// 2. Check if any other step beads for this parent are still hooked.
		remainingHooked, _ := GetHookedStepsFunc(parent.ID)
		if len(remainingHooked) == 0 {
			// 3. No more hooked steps — set parent bead status back to in_progress.
			if err := UpdateBeadFunc(parent.ID, map[string]interface{}{
				"status": "in_progress",
			}); err != nil {
				log.Printf("[steward] hooked sweep: set %s to in_progress: %s", parent.ID, err)
			} else {
				log.Printf("[steward] hooked sweep: %s no longer hooked, set to in_progress", parent.ID)
			}
		}

		// 4. Re-summon wizard.
		if agentName == "" {
			agentName = "wizard-" + SanitizeK8sLabel(parent.ID)
		}
		handle, spawnErr := dispatchPhase(context.Background(), pd, backend, agent.SpawnConfig{
			Name:       agentName,
			BeadID:     parent.ID,
			Role:       agent.RoleWizard,
			Tower:      towerName,
			InstanceID: localInstanceID,
			LogPath:    filepath.Join(dolt.GlobalDir(), "wizards", agentName+".log"),
		}, hookedResumePhase(parent.Type))
		if spawnErr != nil {
			log.Printf("[steward] hooked sweep: dispatch %s: %s", agentName, spawnErr)
		} else if handle != nil {
			log.Printf("[steward] hooked sweep: re-summoned %s for %s (%s)", agentName, parent.ID, handle.Identifier())
		} else {
			log.Printf("[steward] hooked sweep: emitted resume intent for %s", parent.ID)
		}
		resummoned++
	}

	// --- Secondary path: graph-state-driven fallback ---
	// Scan graph_state.json files for hooked steps not yet covered by bead status.
	// This handles cases where graph state has hooked steps but bead status hasn't
	// been updated yet (e.g., pre-migration data).
	entries, err := graphStore.ListHooked(towerName)
	if err != nil {
		log.Printf("[steward] hooked sweep: list hooked graph states: %s", err)
		return resummoned
	}

	for _, entry := range entries {
		// Skip beads already resolved via the primary bead-status path.
		if resolvedBeadIDs[entry.BeadID] {
			continue
		}

		// Restrict auto-resume to locally-owned attempts.
		attempt, aErr := GetActiveAttemptFunc(entry.BeadID)
		if aErr == nil && attempt != nil {
			owned, oErr := IsOwnedByInstanceFunc(attempt.ID, localInstanceID)
			if oErr != nil {
				log.Printf("[steward] hooked sweep: check ownership for %s: %s", attempt.ID, oErr)
			} else if !owned {
				log.Printf("[steward] hooked sweep: skipping %s — foreign attempt %s (graph-state fallback)", entry.BeadID, attempt.ID)
				continue
			}
		}

		agentName := entry.AgentName
		gs := entry.State

		for stepName, ss := range gs.Steps {
			if ss.Status != "hooked" {
				continue
			}

			designRef := ss.Outputs["design_ref"]
			resolved := false

			if designRef != "" {
				designBead, err := GetBeadFunc(designRef)
				if err != nil {
					log.Printf("[steward] hooked sweep: get design bead %s: %s", designRef, err)
					continue
				}
				if designBead.Status != "closed" {
					continue
				}
				comments, _ := GetCommentsFunc(designRef)
				if len(comments) == 0 && designBead.Description == "" {
					continue
				}
				log.Printf("[steward] hooked sweep: design bead %s resolved for %s step %s (graph-state fallback)", designRef, agentName, stepName)
				resolved = true
			} else {
				bead, err := GetBeadFunc(gs.BeadID)
				if err != nil {
					log.Printf("[steward] hooked sweep: get bead %s: %s", gs.BeadID, err)
					continue
				}
				if !store.ContainsLabel(bead, "awaiting-approval") && !store.ContainsLabel(bead, "needs-human") {
					log.Printf("[steward] hooked sweep: approval labels cleared for %s step %s (graph-state fallback)", agentName, stepName)
					resolved = true
				} else {
					continue
				}
			}

			if !resolved {
				continue
			}

			if dryRun {
				log.Printf("[steward] [dry-run] would reset step %s and re-summon %s (graph-state fallback)", stepName, agentName)
				continue
			}

			// Reset graph state step to pending.
			ss.Status = "pending"
			ss.Outputs = nil
			ss.StartedAt = ""
			ss.CompletedAt = ""
			gs.Steps[stepName] = ss
			if err := graphStore.Save(agentName, gs); err != nil {
				log.Printf("[steward] hooked sweep: save graph state for %s: %s", agentName, err)
				continue
			}

			// Also unhook the step bead if we can find it.
			if stepBeadID, ok := gs.StepBeadIDs[stepName]; ok && stepBeadID != "" {
				if err := UnhookStepBeadFunc(stepBeadID); err != nil {
					log.Printf("[steward] hooked sweep: unhook step bead %s: %s", stepBeadID, err)
				}
			}

			// Check parent bead status and clear hooked if no more hooked steps.
			parentBead, _ := GetBeadFunc(gs.BeadID)
			if parentBead.Status == "hooked" {
				remainingHooked, _ := GetHookedStepsFunc(gs.BeadID)
				if len(remainingHooked) == 0 {
					if err := UpdateBeadFunc(gs.BeadID, map[string]interface{}{
						"status": "in_progress",
					}); err != nil {
						log.Printf("[steward] hooked sweep: set %s to in_progress: %s", gs.BeadID, err)
					}
				}
			}

			// Re-summon wizard.
			parentForPhase, _ := GetBeadFunc(gs.BeadID)
			handle, spawnErr := dispatchPhase(context.Background(), pd, backend, agent.SpawnConfig{
				Name:       agentName,
				BeadID:     gs.BeadID,
				Role:       agent.RoleWizard,
				Tower:      towerName,
				InstanceID: localInstanceID,
				LogPath:    filepath.Join(dolt.GlobalDir(), "wizards", agentName+".log"),
			}, hookedResumePhase(parentForPhase.Type))
			if spawnErr != nil {
				log.Printf("[steward] hooked sweep: dispatch %s: %s", agentName, spawnErr)
			} else if handle != nil {
				log.Printf("[steward] hooked sweep: re-summoned %s for %s (%s) (graph-state fallback)", agentName, gs.BeadID, handle.Identifier())
			} else {
				log.Printf("[steward] hooked sweep: emitted resume intent for %s (graph-state fallback)", gs.BeadID)
			}
			resummoned++
			break
		}
	}

	return resummoned
}

// hookedResumePhase returns the FormulaPhase a hooked-sweep resume should
// stamp on an emitted WorkloadIntent. A resume re-dispatches the whole
// bead (the wizard walks the formula and picks up the hooked step), so
// the phase is bead-level — same resolution rule as dispatchClusterNative:
// the bead's type when present, intent.PhaseWizard as the fallback.
// Keeping this behind a single helper lets tests pin the rule and future
// changes to "which phase does a resume use" stay in one place.
func hookedResumePhase(beadType string) string {
	return beadDispatchPhase("", beadType)
}

// clericDispatchPhase returns the FormulaPhase the steward stamps on a
// cleric dispatch intent. Cleric dispatch is bead-level (the cleric
// runs the recovery bead's formula end-to-end inside a wizard-shaped
// pod), so the operator must route it through the bead-level pod
// builder — the same routing wizards use.
//
// The recovery bead's type ("recovery") is NOT a bead-level phase per
// intent.IsBeadLevelPhase, so stamping the bead type would emit
// formula_phase=recovery — an unsupported value the operator drops.
// Returning intent.PhaseWizard makes the operator route to a wizard
// pod that walks the recovery formula. Isolating the choice here
// gives a single point to update when the cluster intent contract
// gains a dedicated cleric role/phase pair.
func clericDispatchPhase() string {
	return intent.PhaseWizard
}

// recoveryShouldResume reports whether the steward should unhook the
// source bead and re-summon the wizard given a closed recovery bead.
// True iff cleric.execute recorded a real success AND cleric.finish
// stamped approve+executed (cleric runtime), OR the legacy recovery
// cycle wrote a DecisionResume outcome. A takeover outcome (source
// carries needs-manual label and no outcome here) returns false — the
// human is expected to fix and unhook manually. A failed cleric.execute
// (stub gateway, gateway error, non-success result) yields
// cleric_outcome=approve+failed and execute_success="false"; both gate
// checks reject so the source bead is left hooked for human takeover.
//
// The two conditions (outcome string + strict success marker) are both
// required: the outcome string is the historical contract and remains
// the primary check, but the strict marker (spi-skfsia finding 2) is
// the authoritative signal because it is set ONLY by cleric.Execute on
// real gateway success. Defending in depth here so any future audit /
// listing path that also writes the outcome string (without running
// execute) cannot accidentally trigger a resume.
func recoveryShouldResume(bead store.Bead) bool {
	// New cleric runtime (spi-hhkozk + spi-skfsia): both the outcome
	// string AND the strict success marker must agree before resuming.
	if bead.Meta(cleric.MetadataKeyOutcome) == "approve+executed" &&
		bead.Meta(cleric.MetadataKeyExecuteSuccess) == "true" {
		return true
	}
	// Legacy in-wizard recovery cycle (pre-foundation): the
	// DecisionResume outcome means the wizard should resume.
	if outcome, ok := recovery.ReadOutcome(bead); ok {
		return outcome.Decision == recovery.DecisionResume
	}
	return false
}

// FailureEvidence holds the IDs of recovery and alert beads linked to a hooked parent
// via caused-by dependencies.
type FailureEvidence struct {
	RecoveryBeadID string
	AlertBeadIDs   []string
}

// findFailureEvidence queries dependents of a hooked bead for caused-by deps
// pointing from recovery or alert beads. Returns the latest recovery bead
// (deterministic: newer CreatedAt wins, ties broken by higher ID), plus any
// alert bead IDs. Picking the latest matters because a parent may carry
// historical caused-by edges from prior recovery attempts (closed with
// DecisionResume or DecisionEscalate); the sweep must act on the newest
// recovery, not any closed one it happens to iterate first.
func findFailureEvidence(beadID string) (FailureEvidence, bool) {
	dependents, err := GetDependentsWithMetaFunc(beadID)
	if err != nil {
		log.Printf("[steward] findFailureEvidence: get dependents for %s: %s", beadID, err)
		return FailureEvidence{}, false
	}

	var evidence FailureEvidence
	var latestRecovery *beads.IssueWithDependencyMetadata

	for _, dep := range dependents {
		if string(dep.DependencyType) != "caused-by" {
			continue
		}
		b, bErr := GetBeadFunc(dep.ID)
		if bErr != nil {
			continue
		}
		if b.Type == "recovery" {
			if latestRecovery == nil ||
				dep.CreatedAt.After(latestRecovery.CreatedAt) ||
				(dep.CreatedAt.Equal(latestRecovery.CreatedAt) && dep.ID > latestRecovery.ID) {
				latestRecovery = dep
			}
		} else if store.ContainsLabel(b, "alert:") || b.Type == "alert" {
			evidence.AlertBeadIDs = append(evidence.AlertBeadIDs, b.ID)
		}
	}

	if latestRecovery != nil {
		evidence.RecoveryBeadID = latestRecovery.ID
	}
	return evidence, evidence.RecoveryBeadID != ""
}

// GetReviewBeads returns review-round child beads for a parent, sorted by round number.
// Uses the test-replaceable GetChildrenFunc so tests can inject fake children.
func GetReviewBeads(parentID string) ([]store.Bead, error) {
	children, err := GetChildrenFunc(parentID)
	if err != nil {
		return nil, err
	}
	var reviews []store.Bead
	for _, child := range children {
		if store.IsReviewRoundBead(child) {
			reviews = append(reviews, child)
		}
	}
	// Sort by round number.
	for i := 0; i < len(reviews); i++ {
		for j := i + 1; j < len(reviews); j++ {
			ri := store.ReviewRoundNumber(reviews[i])
			rj := store.ReviewRoundNumber(reviews[j])
			if rj < ri {
				reviews[i], reviews[j] = reviews[j], reviews[i]
			}
		}
	}
	return reviews, nil
}

// RaiseCorruptedBeadAlert creates an alert bead for a corrupted bead (e.g. multiple
// open attempts). Deduplicates: skips creation if an alert already exists.
func RaiseCorruptedBeadAlert(beadID string, violation error) {
	if CheckExistingAlertFunc(beadID) {
		log.Printf("[store] alert already exists for corrupted bead %s, skipping duplicate", beadID)
		return
	}
	msg := fmt.Sprintf("corrupted bead %s: %v", beadID, violation)
	if err := CreateAlertFunc(beadID, msg); err != nil {
		log.Printf("[store] failed to raise alert for corrupted bead %s: %v", beadID, err)
	}
}

// SanitizeK8sLabel makes a bead ID safe for k8s resource names.
func SanitizeK8sLabel(s string) string {
	result := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
			result = append(result, c)
		case c >= 'A' && c <= 'Z':
			result = append(result, c+32)
		case c == '.' || c == '_':
			result = append(result, '-')
		}
	}
	return string(result)
}

// pushState is intentionally a no-op. The shared dolt server is the source
// of truth — there is no remote to push to. DoltHub backup, if desired,
// is handled by the syncer pod, not the steward cycle.
func pushState() {}

// sendMessage creates a message bead with the appropriate labels.
//
// When ref != "" the message is attributed to a source bead and ownership of
// the creation goes through pkg/alerts so the exclusive-owner invariant holds.
// When ref == "" the message is a tower-level notification with no source bead;
// this is the sole legitimate exception to the pkg/alerts ownership rule
// because alerts.Raise requires a non-empty sourceBeadID.
func sendMessage(to, from, body, ref string, priority int) (string, error) {
	if ref != "" {
		return alerts.Raise(stewardBeadOps{}, ref, alerts.ClassArchmageMsg, body,
			alerts.WithFrom(from),
			alerts.WithPriority(priority),
			alerts.WithExtraLabels("to:"+to))
	}
	// sole legitimate exception: tower-level message has no source bead.
	labels := []string{"msg", "to:" + to, "from:" + from}
	return CreateBeadFunc(store.CreateOpts{
		Title:    body,
		Priority: priority,
		Type:     "message",
		Prefix:   "",
		Labels:   labels,
	})
}

// executeMerge is the merge callback for the merge queue.
// Resumes the staging worktree, calls MergeToMain, pushes, and cleans up the branch.
func executeMerge(ctx context.Context, req MergeRequest) MergeResult {
	repoPath := req.RepoPath
	if repoPath == "" {
		// Resolve repo path from config using bead prefix.
		cfg, err := ConfigLoadFunc()
		if err != nil {
			return MergeResult{BeadID: req.BeadID, Success: false, Error: fmt.Errorf("load config: %w", err)}
		}
		prefix := beadRepoPrefix(req.BeadID)
		inst, ok := cfg.Instances[prefix]
		if !ok || inst.Path == "" {
			return MergeResult{BeadID: req.BeadID, Success: false, Error: fmt.Errorf("no registered repo for prefix %q", prefix)}
		}
		repoPath = inst.Path
	}

	wtDir := filepath.Join(repoPath, ".worktrees", req.BeadID)
	if _, err := os.Stat(wtDir); os.IsNotExist(err) {
		return MergeResult{BeadID: req.BeadID, Success: false, Error: fmt.Errorf("no staging worktree found at %s", wtDir)}
	}

	baseBranch := repoconfig.ResolveBranchBase(req.BaseBranch)

	stagingWt := spgit.ResumeStagingWorktree(repoPath, wtDir, req.Branch, baseBranch, log.Printf)

	mergeEnv := os.Environ()
	if err := stagingWt.MergeToMain(baseBranch, mergeEnv, "", "", nil); err != nil {
		return MergeResult{BeadID: req.BeadID, Success: false, Error: fmt.Errorf("merge to main: %w", err)}
	}

	rc := &spgit.RepoContext{Dir: repoPath, BaseBranch: baseBranch, Log: log.Printf}

	if err := rc.Push("origin", baseBranch, mergeEnv); err != nil {
		return MergeResult{BeadID: req.BeadID, Success: false, Error: fmt.Errorf("push %s: %w", baseBranch, err)}
	}

	// Clean up the feature branch (local + remote). Errors are non-fatal.
	if err := rc.DeleteBranch(req.Branch); err != nil {
		log.Printf("[steward] delete local branch %s: %s", req.Branch, err)
	}
	if err := rc.DeleteRemoteBranch("origin", req.Branch); err != nil {
		log.Printf("[steward] delete remote branch %s: %s", req.Branch, err)
	}

	sha := rc.HeadSHA()
	return MergeResult{BeadID: req.BeadID, Success: true, SHA: sha}
}

// beadRepoPrefix extracts the repo prefix from a bead ID (e.g., "spi" from "spi-abc").
func beadRepoPrefix(beadID string) string {
	if idx := strings.Index(beadID, "-"); idx > 0 {
		return beadID[:idx]
	}
	return beadID
}

// mergeQueueDepth safely returns depth (0 if queue is nil).
func mergeQueueDepth(mq *MergeQueue) int {
	if mq == nil {
		return 0
	}
	return mq.Depth()
}

// getDBForRouting opens a *sql.DB connection to the dolt server for trust/routing queries.
// Returns nil on error (callers should nil-check and skip).
func getDBForRouting(dbName string) *sql.DB {
	dsn := fmt.Sprintf("root:@tcp(%s:%s)/%s", dolt.Host(), dolt.Port(), dbName)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil
	}
	return db
}

// ExecuteMergeFunc is the merge callback used by TowerCycle. Test-replaceable.
var ExecuteMergeFunc = executeMerge

// CommitPendingFunc is a test-replaceable function for store.CommitPending.
var CommitPendingFunc = store.CommitPending

// GetSchedulableWorkFunc is a test-replaceable function for store.GetSchedulableWork.
var GetSchedulableWorkFunc = store.GetSchedulableWork

// LoadTowerConfigFunc is a test-replaceable function for config.LoadTowerConfig.
var LoadTowerConfigFunc = config.LoadTowerConfig

// ConfigLoadFunc is a test-replaceable function for config.Load.
var ConfigLoadFunc = config.Load

// BeadsDirForTowerFunc is a test-replaceable function for BeadsDirForTower.
var BeadsDirForTowerFunc = BeadsDirForTower

// StoreOpenAtFunc is a test-replaceable function for store.OpenAt.
var StoreOpenAtFunc = store.OpenAt
