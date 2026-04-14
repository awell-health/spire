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
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/dolt"
	"github.com/awell-health/spire/pkg/executor"
	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// --- Test-replaceable function variables ---

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

// CreateAlertFunc creates the alert bead for a corrupted bead and links it via a caused-by dep.
var CreateAlertFunc = func(beadID, msg string) error {
	alertID, err := store.CreateBead(store.CreateOpts{
		Title:    msg,
		Priority: 0,
		Type:     beads.TypeTask,
		Labels:   []string{"alert:corrupted-bead"},
	})
	if err != nil {
		return err
	}
	if alertID != "" {
		if derr := store.AddDepTyped(alertID, beadID, "caused-by"); derr != nil {
			log.Printf("[store] warning: add caused-by dep %s→%s: %s", alertID, beadID, derr)
		}
	}
	return nil
}

// StewardConfig holds configuration for the steward cycle.
type StewardConfig struct {
	DryRun            bool
	NoAssign          bool
	Backend           agent.Backend
	StaleThreshold    time.Duration
	ShutdownThreshold time.Duration
	AgentList         []string
	MetricsPort       int // 0 = disabled; non-zero = start HTTP metrics server

	ConcurrencyLimiter *ConcurrencyLimiter      // nil = no limit enforcement
	MergeQueue         *MergeQueue              // nil = no merge queue
	TrustChecker       *TrustChecker            // nil = no trust checks
	ABRouter           *ABRouter                // nil = no A/B routing
	CycleStats         *CycleStats              // nil = no stats tracking
	GraphStateStore    executor.GraphStateStore // nil = use default file-backed store
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
		beadsDir := BeadsDirForTower(towerName)
		if beadsDir == "" {
			log.Printf("[steward] %sno .beads/ directory found, skipping", prefix)
			return
		}
		if _, err := store.OpenAt(beadsDir); err != nil {
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

	// Load tower config for MaxConcurrent.
	var maxConcurrent int
	if tc, err := LoadTowerConfigFunc(towerName); err == nil {
		maxConcurrent = tc.MaxConcurrent
	}

	log.Printf("[steward] %sready: %d beads | agents: %d alive | max_concurrent: %d",
		prefix, len(schedulable), aliveCount, maxConcurrent)

	// Step 4: Auto-summon — spawn new wizards for schedulable work up to capacity.
	spawned := 0
	for _, bead := range schedulable {
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
			if db := GetDBForRoutingFunc(); db != nil {
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

		// Spawn wizard.
		handle, spawnErr := cfg.Backend.Spawn(agent.SpawnConfig{
			Name:    wizardName,
			BeadID:  bead.ID,
			Role:    agent.RoleWizard,
			Tower:   towerName,
			LogPath: filepath.Join(dolt.GlobalDir(), "wizards", wizardName+".log"),
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
			agents = append(agents, agent.Info{Name: wizardName, Alive: true})
			cfg.ConcurrencyLimiter.Refresh(towerName, agents)
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

	// Step 5: Stale + shutdown check.
	staleCount, shutdownCount := CheckBeadHealth(cfg.StaleThreshold, cfg.ShutdownThreshold, cfg.DryRun, cfg.Backend)
	if staleCount > 0 || shutdownCount > 0 {
		log.Printf("[steward] %sstale: %d warning(s), %d shutdown(s)", prefix, staleCount, shutdownCount)
	} else {
		log.Printf("[steward] %sstale: none", prefix)
	}

	// Step 6b: Process merge queue (one per cycle to serialize).
	if cfg.MergeQueue != nil && cfg.MergeQueue.Depth() > 0 {
		result := cfg.MergeQueue.ProcessNext(context.Background(), ExecuteMergeFunc)
		if result != nil {
			if result.Success {
				log.Printf("[steward] %smerge queue: %s merged (%s)", prefix, result.BeadID, result.SHA)
				// Record clean merge for trust.
				if cfg.TrustChecker != nil {
					if db := GetDBForRoutingFunc(); db != nil {
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
					if db := GetDBForRoutingFunc(); db != nil {
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

// CheckBeadHealth checks in_progress beads against two thresholds:
//   - stale: wizard exceeded guidelines (warning + alert bead)
//   - shutdown: tower kills the wizard via backend.Kill()
//
// Returns (staleCount, shutdownCount).
func CheckBeadHealth(staleThreshold, shutdownThreshold time.Duration, dryRun bool, backend agent.Backend) (int, int) {
	inProgress, err := ListBeadsFunc(beads.IssueFilter{Status: store.StatusPtr(beads.StatusInProgress)})
	if err != nil {
		log.Printf("[steward] check health: %s", err)
		return 0, 0
	}

	now := time.Now()
	staleCount, shutdownCount := 0, 0

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
		}

		if age > shutdownThreshold {
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
			Name:    reviewerName,
			BeadID:  b.ID,
			Role:    agent.RoleSage,
			Tower:   towerName,
			LogPath: filepath.Join(dolt.GlobalDir(), "wizards", reviewerName+".log"),
		})
		if spawnErr != nil {
			log.Printf("[steward] failed to spawn reviewer for %s: %v", b.ID, spawnErr)
		} else {
			log.Printf("[steward] spawned reviewer %s for %s (%s)", reviewerName, b.ID, handle.Identifier())
		}
	}
}

// ReviewBeadVerdict extracts the verdict string from a closed review-round bead.
// Prefers the "review_verdict" metadata key (structured); falls back to
// parsing the description prefix "verdict: <value>" for legacy beads.
func ReviewBeadVerdict(b store.Bead) string {
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
		reg := agent.LoadRegistry()
		for _, w := range reg.Wizards {
			if w.BeadID == b.ID && w.Phase != "review" {
				owner = w.Name
				break
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
	// 1. List all graph states with hooked steps via the store abstraction.
	// In local mode this scans configDir/runtime/*/graph_state.json;
	// in cluster mode it queries the Dolt graph_state table.
	entries, err := graphStore.ListHooked(towerName)
	if err != nil {
		log.Printf("[steward] hooked sweep: list hooked: %s", err)
		return 0
	}

	resummoned := 0
	for _, entry := range entries {
		agentName := entry.AgentName
		gs := entry.State

		// 2. Find hooked steps.
		for stepName, ss := range gs.Steps {
			if ss.Status != "hooked" {
				continue
			}

			// 3. Resolve the hooked condition.
			// Two hook types:
			//   a) check.design-linked: design_ref output → check if design bead is closed with content
			//   b) human.approve: no design_ref → check if awaiting-approval label was cleared
			designRef := ss.Outputs["design_ref"]
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
				log.Printf("[steward] hooked sweep: design bead %s resolved for %s step %s", designRef, agentName, stepName)
				resolved = true
			} else {
				// Human approval hook (or other label-based hook): check if
				// awaiting-approval and needs-human labels have been cleared.
				bead, err := GetBeadFunc(gs.BeadID)
				if err != nil {
					log.Printf("[steward] hooked sweep: get bead %s: %s", gs.BeadID, err)
					continue
				}
				if !store.ContainsLabel(bead, "awaiting-approval") && !store.ContainsLabel(bead, "needs-human") {
					log.Printf("[steward] hooked sweep: approval labels cleared for %s step %s", agentName, stepName)
					resolved = true
				} else {
					continue // still waiting for approval
				}
			}

			if !resolved {
				continue
			}

			if dryRun {
				log.Printf("[steward] [dry-run] would reset step %s and re-summon %s", stepName, agentName)
				continue
			}

			// 4. Reset hooked step to pending via the graph state store.
			ss.Status = "pending"
			ss.Outputs = nil
			ss.StartedAt = ""
			ss.CompletedAt = ""
			gs.Steps[stepName] = ss
			if err := graphStore.Save(agentName, gs); err != nil {
				log.Printf("[steward] hooked sweep: save graph state for %s: %s", agentName, err)
				continue
			}

			// 5. Re-summon wizard.
			handle, spawnErr := backend.Spawn(agent.SpawnConfig{
				Name:    agentName,
				BeadID:  gs.BeadID,
				Role:    agent.RoleApprentice,
				Tower:   towerName,
				LogPath: filepath.Join(dolt.GlobalDir(), "wizards", agentName+".log"),
			})
			if spawnErr != nil {
				log.Printf("[steward] hooked sweep: spawn %s: %s", agentName, spawnErr)
			} else if handle != nil {
				log.Printf("[steward] hooked sweep: re-summoned %s for %s (%s)", agentName, gs.BeadID, handle.Identifier())
			}
			resummoned++
			break // one hooked step per agent is enough
		}
	}
	return resummoned
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
func sendMessage(to, from, body, ref string, priority int) (string, error) {
	labels := []string{"msg", "to:" + to, "from:" + from}
	if ref != "" {
		labels = append(labels, "ref:"+ref)
	}
	return store.CreateBead(store.CreateOpts{
		Title:    body,
		Priority: priority,
		Type:     "message",
		Prefix:   "spi",
		Labels:   labels,
	})
}

// executeMerge is the merge callback for the merge queue.
// Performs: git fetch, rebase onto base, push.
// This is a placeholder — real git operations require repo context from pkg/git.
func executeMerge(ctx context.Context, req MergeRequest) MergeResult {
	// TODO: Wire to real git operations via pkg/git once merge infra is ready.
	// For now, return a failure so the merge queue doesn't silently drop requests.
	return MergeResult{
		BeadID:  req.BeadID,
		Success: false,
		Error:   fmt.Errorf("merge execution not yet wired to git operations"),
	}
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
func getDBForRouting() *sql.DB {
	dsn := fmt.Sprintf("root:@tcp(%s:%s)/", dolt.Host(), dolt.Port())
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
