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
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/dolt"
	"github.com/awell-health/spire/pkg/executor"
	"github.com/awell-health/spire/pkg/formula"
	spgit "github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/registry"
	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/awell-health/spire/pkg/steward/attached"
	"github.com/awell-health/spire/pkg/store"
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
	// Also load the tower's EffectiveDeploymentMode so the dispatch step
	// branches on it. The default tower (towerName=="") and any tower whose
	// config can't be loaded both fall through to local-native, preserving
	// pre-deployment-mode behavior.
	var towerBindings map[string]*config.LocalRepoBinding
	deploymentMode := config.Default()
	if towerName != "" {
		if tower, tErr := LoadTowerConfigFunc(towerName); tErr == nil {
			towerBindings = tower.LocalBindings
			deploymentMode = tower.EffectiveDeploymentMode()
		}
	}

	// Branch on deployment mode. Local-native runs the existing direct-
	// spawn loop; cluster-native emits WorkloadIntents; attached-reserved
	// is a typed not-implemented surface that skips dispatch entirely.
	spawned := 0
	switch deploymentMode {
	case config.DeploymentModeAttachedReserved:
		log.Printf("[steward] %sattached-reserved: dispatch skipped — %s", prefix, attached.ErrAttachedNotImplemented)

	case config.DeploymentModeClusterNative:
		// Resolve ClusterDispatch lazily: if the caller set it
		// explicitly (test override) use that; otherwise invoke the
		// factory, which builds the config against the per-tower DB
		// that was opened above by StoreOpenAtFunc.
		cycleCfg := cfg
		if cycleCfg.ClusterDispatch == nil && cycleCfg.BuildClusterDispatch != nil {
			cycleCfg.ClusterDispatch = cycleCfg.BuildClusterDispatch(towerName)
		}
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

	// Step 4b: Detect standalone tasks ready for review.
	DetectReviewReady(cfg.DryRun, cfg.Backend, towerName)

	// Step 4c: Detect tasks with review feedback that need wizard re-engagement.
	DetectReviewFeedback(cfg.DryRun)

	// Step 4d: Sweep hooked graph steps.
	// Use configured store if available, otherwise fall back to file-backed store.
	graphStore := cfg.GraphStateStore
	if graphStore == nil {
		graphStore = &executor.FileGraphStateStore{ConfigDir: config.Dir}
	}
	if hookedCount := SweepHookedSteps(cfg.DryRun, cfg.Backend, towerName, graphStore); hookedCount > 0 {
		log.Printf("[steward] %shooked sweep: re-summoned %d wizard(s)", prefix, hookedCount)
	}

	// Step 4e: Detect merge-ready beads and enqueue.
	if cfg.MergeQueue != nil {
		DetectMergeReady(cfg.DryRun, cfg.MergeQueue)
	}

	// Step 5: Stale + shutdown check.
	staleCount, shutdownCount := CheckBeadHealth(cfg.StaleThreshold, cfg.ShutdownThreshold, cfg.DryRun, cfg.Backend)
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

// CheckBeadHealth checks in_progress beads against two thresholds:
//   - stale: wizard exceeded guidelines (warning + alert bead)
//   - shutdown: tower kills the wizard via backend.Kill()
//
// Only processes attempts owned by this instance. Foreign attempts (owned by
// another instance) are skipped. Unstamped pre-migration attempts are treated
// as local for backward compatibility.
//
// Returns (staleCount, shutdownCount).
func CheckBeadHealth(staleThreshold, shutdownThreshold time.Duration, dryRun bool, backend agent.Backend) (int, int) {
	inProgress, err := ListBeadsFunc(beads.IssueFilter{Status: store.StatusPtr(beads.StatusInProgress)})
	if err != nil {
		log.Printf("[steward] check health: %s", err)
		return 0, 0
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

		age := now.Sub(t)
		owner := ""
		attempt, aErr := GetActiveAttemptFunc(b.ID)
		if aErr != nil {
			// Invariant violation: multiple open attempts. Raise an alert and
			// continue health checking with empty owner (Kill("") will fail
			// gracefully if the shutdown threshold is also exceeded).
			log.Printf("[steward] %s has multiple open attempts (invariant violation): %v", b.ID, aErr)
			RaiseCorruptedBeadAlert(b.ID, aErr)
		} else if attempt != nil {
			owner = store.HasLabel(*attempt, "agent:")

			// Check instance ownership of the attempt.
			meta, metaErr := GetAttemptInstanceFunc(attempt.ID)
			if metaErr != nil {
				log.Printf("[steward] check health: get instance meta for %s: %s", attempt.ID, metaErr)
			} else if meta != nil && meta.InstanceID != "" && meta.InstanceID != localInstanceID {
				// Foreign attempt — skip health check.
				log.Printf("[steward] foreign attempt %s on instance %s — skipping health check", attempt.ID, meta.InstanceID)
				foreignCount++
				continue
			}
			// meta == nil || meta.InstanceID == "": backward compat (unstamped) — treat as local.
			// meta.InstanceID == localInstanceID: local — proceed with health check.
		}

		if age > shutdownThreshold {
			// Without an identifiable owner there is no wizard to kill —
			// calling Kill("") just spams "agent \"\" not found in
			// registry" each cycle. This typically means the attempt bead
			// was never created or never stamped (e.g. schema mismatch,
			// foreign-instance work whose attempt lookup failed). Log
			// stale and move on; a human can investigate the orphan.
			if owner == "" {
				staleCount++
				log.Printf("[steward] STALE (no owner): %s (%s) age=%s — not killing, investigate orphan", b.ID, b.Title, age.Round(time.Second))
				continue
			}
			// Fatal: kill the wizard via backend.
			shutdownCount++
			if dryRun {
				log.Printf("[steward] [dry-run] SHUTDOWN: %s (%s) owner=%s age=%s", b.ID, b.Title, owner, age.Round(time.Second))
			} else {
				log.Printf("[steward] SHUTDOWN: %s (%s) owner=%s age=%s — killing wizard", b.ID, b.Title, owner, age.Round(time.Second))
				if killErr := backend.Kill(owner); killErr != nil {
					log.Printf("[steward] kill %s: %s", owner, killErr)
				}
			}
		} else if age > staleThreshold {
			// Warning: wizard exceeded guidelines.
			staleCount++
			if dryRun {
				log.Printf("[steward] [dry-run] STALE: %s (%s) owner=%s age=%s", b.ID, b.Title, owner, age.Round(time.Second))
			} else {
				log.Printf("[steward] STALE: %s (%s) owner=%s age=%s", b.ID, b.Title, owner, age.Round(time.Second))
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
// and are no longer used — CheckBeadHealth now reads b.UpdatedAt directly.
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
func DetectReviewReady(dryRun bool, backend agent.Backend, towerName string) {
	inProgress, err := store.ListBeads(beads.IssueFilter{Status: store.StatusPtr(beads.StatusInProgress)})
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
		steps, sErr := store.GetStepBeads(b.ID)
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

		handle, spawnErr := backend.Spawn(agent.SpawnConfig{
			Name:       reviewerName,
			BeadID:     b.ID,
			Role:       agent.RoleSage,
			Tower:      towerName,
			InstanceID: InstanceIDFunc(),
			LogPath:    filepath.Join(dolt.GlobalDir(), "wizards", reviewerName+".log"),
		})
		if spawnErr != nil {
			log.Printf("[steward] failed to spawn reviewer for %s: %v", b.ID, spawnErr)
		} else {
			log.Printf("[steward] spawned reviewer %s for %s (%s)", reviewerName, b.ID, handle.Identifier())
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

// DetectReviewFeedback finds in_progress beads whose last review-round bead
// is closed with verdict "request_changes" and no active attempt bead (wizard
// not already working on it). It re-spawns a wizard to address the feedback.
func DetectReviewFeedback(dryRun bool) {
	inProgress, err := store.ListBeads(beads.IssueFilter{Status: store.StatusPtr(beads.StatusInProgress)})
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

		// Find the wizard owner from the last attempt or fall back.
		owner := "wizard"
		// Check wizard registry for a wizard associated with this bead.
		if regEntries, err := registry.List(); err == nil {
			for _, w := range regEntries {
				if w.BeadID == b.ID {
					owner = w.Name
					break
				}
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
func SweepHookedSteps(dryRun bool, backend agent.Backend, towerName string, graphStore executor.GraphStateStore) int {
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

			if recoveryBead.Status == "closed" {
				// Cleric finished. Check resolution: DecisionResume → unhook + resummon,
				// DecisionEscalate → leave hooked for human attention. Beads without a
				// recorded outcome (older beads or missing writes) default to the
				// leave-hooked path for safety.
				outcome, haveOutcome := recovery.ReadOutcome(recoveryBead)
				if !haveOutcome || outcome.Decision == recovery.DecisionEscalate {
					log.Printf("[steward] hooked sweep: recovery %s escalated (or no outcome) for %s — leaving hooked for human", evidence.RecoveryBeadID, parent.ID)
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

				// Set parent bead back to in_progress.
				if err := UpdateBeadFunc(parent.ID, map[string]interface{}{
					"status": "in_progress",
				}); err != nil {
					log.Printf("[steward] hooked sweep: set %s to in_progress: %s", parent.ID, err)
				} else {
					log.Printf("[steward] hooked sweep: %s no longer hooked, set to in_progress", parent.ID)
				}

				// Re-summon the wizard.
				wizName := agentName
				if wizName == "" {
					wizName = "wizard-" + SanitizeK8sLabel(parent.ID)
				}
				handle, spawnErr := backend.Spawn(agent.SpawnConfig{
					Name:       wizName,
					BeadID:     parent.ID,
					Role:       agent.RoleApprentice,
					Tower:      towerName,
					InstanceID: localInstanceID,
					LogPath:    filepath.Join(dolt.GlobalDir(), "wizards", wizName+".log"),
				})
				if spawnErr != nil {
					log.Printf("[steward] hooked sweep: spawn %s: %s", wizName, spawnErr)
				} else if handle != nil {
					log.Printf("[steward] hooked sweep: re-summoned %s for %s after cleric success (%s)", wizName, parent.ID, handle.Identifier())
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

			handle, spawnErr := backend.Spawn(agent.SpawnConfig{
				Name:       clericName,
				BeadID:     evidence.RecoveryBeadID,
				Role:       agent.RoleExecutor,
				Tower:      towerName,
				InstanceID: localInstanceID,
				LogPath:    filepath.Join(dolt.GlobalDir(), "wizards", clericName+".log"),
			})
			if spawnErr != nil {
				log.Printf("[steward] hooked sweep: spawn cleric %s: %s", clericName, spawnErr)
			} else if handle != nil {
				log.Printf("[steward] hooked sweep: summoned cleric %s for %s (%s)", clericName, evidence.RecoveryBeadID, handle.Identifier())
			}
			resummoned++
			continue
		}

		resolvedBeadIDs[parent.ID] = true

		if dryRun {
			resummoned++
			continue
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
		handle, spawnErr := backend.Spawn(agent.SpawnConfig{
			Name:       agentName,
			BeadID:     parent.ID,
			Role:       agent.RoleApprentice,
			Tower:      towerName,
			InstanceID: localInstanceID,
			LogPath:    filepath.Join(dolt.GlobalDir(), "wizards", agentName+".log"),
		})
		if spawnErr != nil {
			log.Printf("[steward] hooked sweep: spawn %s: %s", agentName, spawnErr)
		} else if handle != nil {
			log.Printf("[steward] hooked sweep: re-summoned %s for %s (%s)", agentName, parent.ID, handle.Identifier())
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
			handle, spawnErr := backend.Spawn(agent.SpawnConfig{
				Name:       agentName,
				BeadID:     gs.BeadID,
				Role:       agent.RoleApprentice,
				Tower:      towerName,
				InstanceID: localInstanceID,
				LogPath:    filepath.Join(dolt.GlobalDir(), "wizards", agentName+".log"),
			})
			if spawnErr != nil {
				log.Printf("[steward] hooked sweep: spawn %s: %s", agentName, spawnErr)
			} else if handle != nil {
				log.Printf("[steward] hooked sweep: re-summoned %s for %s (%s) (graph-state fallback)", agentName, gs.BeadID, handle.Identifier())
			}
			resummoned++
			break
		}
	}

	return resummoned
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
