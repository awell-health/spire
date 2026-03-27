package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/steveyegge/beads"
)

// agentNames extracts agent names from an AgentInfo slice.
// If override is provided, it takes priority (explicit --agents flag).
func agentNames(agents []AgentInfo, override []string) []string {
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

// busySet builds a set of agent names that are currently busy.
// An agent is busy if it is alive (has a running process/container/pod).
func busySet(agents []AgentInfo) map[string]bool {
	busy := make(map[string]bool)
	for _, a := range agents {
		if a.Alive {
			busy[a.Name] = true
		}
	}
	return busy
}

func cmdSteward(args []string) error {
	// Parse flags — staleThreshold left at zero to detect "not overridden".
	interval := 2 * time.Minute
	var staleOverride time.Duration
	once := false
	dryRun := false
	noAssign := false // skip sending assignment messages (managed agents get work via operator)
	backendName := "" // default: auto-resolve from ResolveBackend
	var agentList []string

	for i := 0; i < len(args); i++ {
		arg := args[i]

		// Handle --flag=value syntax
		if strings.Contains(arg, "=") {
			parts := strings.SplitN(arg, "=", 2)
			arg = parts[0]
			args = append(args[:i+1], append([]string{parts[1]}, args[i+1:]...)...)
			args[i] = arg
		}

		switch arg {
		case "--interval":
			if i+1 >= len(args) {
				return fmt.Errorf("--interval requires a value (e.g., 2m, 30s, 5m)")
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil {
				secs, serr := strconv.Atoi(args[i])
				if serr != nil {
					return fmt.Errorf("--interval: invalid duration %q", args[i])
				}
				d = time.Duration(secs) * time.Second
			}
			interval = d
		case "--stale-threshold":
			if i+1 >= len(args) {
				return fmt.Errorf("--stale-threshold requires a value (e.g., 15m, 30m)")
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil {
				return fmt.Errorf("--stale-threshold: invalid duration %q", args[i])
			}
			staleOverride = d
		case "--once":
			once = true
		case "--dry-run":
			dryRun = true
		case "--no-assign":
			noAssign = true
		case "--backend":
			if i+1 >= len(args) {
				return fmt.Errorf("--backend requires a value: process, docker, or k8s")
			}
			i++
			switch args[i] {
			case "process", "docker", "k8s":
				backendName = args[i]
			default:
				return fmt.Errorf("--backend: unknown value %q (use: process, docker, k8s)", args[i])
			}
		case "--agents":
			if i+1 >= len(args) {
				return fmt.Errorf("--agents requires a comma-separated list of agent names")
			}
			i++
			for _, a := range strings.Split(args[i], ",") {
				a = strings.TrimSpace(a)
				if a != "" {
					agentList = append(agentList, a)
				}
			}
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire steward [--once] [--dry-run] [--interval 2m] [--stale-threshold 15m] [--backend process|docker|k8s] [--agents a,b,c]", args[i])
		}
	}

	// Read stale + shutdown (timeout) from spire.yaml.
	//
	// Stale = agent.stale (warning: wizard exceeded guidelines, create alert)
	// Shutdown = agent.timeout (fatal: tower kills the pod)
	//
	// Defaults: stale=10m, shutdown=15m. stale must be < timeout.
	// --stale-threshold flag overrides agent.stale if explicitly set.
	var staleThreshold, shutdownThreshold time.Duration
	cwd, _ := os.Getwd()
	if cfg, err := repoconfig.Load(cwd); err == nil {
		if cfg.Agent.Stale != "" {
			if d, err := time.ParseDuration(cfg.Agent.Stale); err == nil {
				staleThreshold = d
			}
		}
		if cfg.Agent.Timeout != "" {
			if d, err := time.ParseDuration(cfg.Agent.Timeout); err == nil {
				shutdownThreshold = d
			}
		}
	}
	if staleThreshold == 0 {
		staleThreshold = 10 * time.Minute
	}
	if shutdownThreshold == 0 {
		shutdownThreshold = 15 * time.Minute
	}
	// Explicit flag overrides config.
	if staleOverride > 0 {
		staleThreshold = staleOverride
	}

	backend := ResolveBackend(backendName)
	log.Printf("[steward] starting (backend=%s, interval=%s, once=%v, dry-run=%v, stale=%s, shutdown=%s)",
		backendName, interval, once, dryRun, staleThreshold, shutdownThreshold)
	if backendName == "" {
		log.Printf("[steward] backend auto-resolved to process")
	}
	if len(agentList) > 0 {
		log.Printf("[steward] agents: %s", strings.Join(agentList, ", "))
	}

	// Align project_id — only for the CWD tower (legacy behavior).
	// Multi-tower alignment is handled per-cycle via openStoreAt().
	ensureProjectID()

	cycleNum := 1

	// Run first cycle immediately.
	stewardCycle(cycleNum, dryRun, noAssign, backend, staleThreshold, shutdownThreshold, agentList)
	cycleNum++

	if once {
		return nil
	}

	// Set up signal handling for graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			stewardCycle(cycleNum, dryRun, noAssign, backend, staleThreshold, shutdownThreshold, agentList)
			cycleNum++
		case sig := <-sigCh:
			log.Printf("[steward] received %s, shutting down after %d cycles", sig, cycleNum-1)
			return nil
		}
	}
}

// stewardCycle iterates all towers and runs a steward cycle for each.
func stewardCycle(cycleNum int, dryRun, noAssign bool, backend AgentBackend, staleThreshold, shutdownThreshold time.Duration, agentList []string) {
	start := time.Now()
	log.Printf("[steward] ═══ cycle %d ═══════════════════════════════", cycleNum)

	towers, err := listTowerConfigs()
	if err != nil {
		log.Printf("[steward] list towers: %s", err)
		return
	}

	if len(towers) == 0 {
		// Fallback: single-tower mode (no tower configs, use default store).
		stewardTowerCycle(cycleNum, "", dryRun, noAssign, backend, staleThreshold, shutdownThreshold, agentList)
	} else {
		for _, tower := range towers {
			stewardTowerCycle(cycleNum, tower.Name, dryRun, noAssign, backend, staleThreshold, shutdownThreshold, agentList)
		}
	}

	log.Printf("[steward] ═══ cycle %d complete (%.1fs) ════════════════", cycleNum, time.Since(start).Seconds())
}

// stewardTowerCycle runs one steward cycle for a specific tower.
// If towerName is "", uses the default store (legacy single-tower mode).
func stewardTowerCycle(cycleNum int, towerName string, dryRun, noAssign bool, backend AgentBackend, staleThreshold, shutdownThreshold time.Duration, agentList []string) {
	prefix := ""
	if towerName != "" {
		prefix = "[" + towerName + "] "

		// Open store for this tower's .beads/ directory.
		beadsDir := beadsDirForTower(towerName)
		if beadsDir == "" {
			log.Printf("[steward] %sno .beads/ directory found, skipping", prefix)
			return
		}
		if _, err := openStoreAt(beadsDir); err != nil {
			log.Printf("[steward] %sopen store: %s", prefix, err)
			return
		}
		defer resetStore()
		log.Printf("[steward] %s───────────────────────────────", prefix)
	}

	// Step 1: Commit any local changes (pull/push disabled — shared dolt server is source of truth).
	_ = storeCommitPending("steward cycle sync")

	// Step 2: Assess — find ready work.
	ready, err := storeGetReadyWork(beads.WorkFilter{})
	if err != nil {
		log.Printf("[steward] %sready: error — %s", prefix, err)
		pushState()
		return
	}

	// Step 3: Load roster via backend.List() — one code path for all backends.
	agents, _ := backend.List()
	roster := agentNames(agents, agentList)
	busy := busySet(agents)
	idleCount := len(roster) - len(busy)
	if idleCount < 0 {
		idleCount = 0
	}

	log.Printf("[steward] %sready: %d beads | roster: %d wizard(s) (%d busy, %d idle)",
		prefix, len(ready), len(roster), len(busy), idleCount)

	if len(roster) == 0 {
		checkBeadHealth(staleThreshold, shutdownThreshold, dryRun, backend)
		pushState()
		return
	}

	// Step 4: Assign ready beads to idle agents (round-robin).
	assigned := 0
	agentIdx := 0
	for _, bead := range ready {
		// Skip message, template, and already-owned beads.
		if hasLabel(bead, "msg") != "" || containsLabel(bead, "msg") {
			continue
		}
		if containsLabel(bead, "template") {
			continue
		}
		// Skip beads with an active attempt child (someone is already working).
		// The attempt bead is the authority — owner: label is not used here.
		if attempt, err := storeGetActiveAttemptFunc(bead.ID); err == nil && attempt != nil {
			continue
		}

		// Find next idle agent (round-robin).
		agent := ""
		for attempts := 0; attempts < len(roster); attempts++ {
			candidate := roster[agentIdx%len(roster)]
			agentIdx++
			if !busy[candidate] {
				agent = candidate
				break
			}
		}

		if agent == "" {
			continue // all agents busy
		}

		if dryRun {
			log.Printf("[steward] %s[dry-run] would assign %s → %s", prefix, bead.ID, agent)
			assigned++
			continue
		}

		if noAssign {
			// Managed agents get work via operator (SpireWorkloads), not messages.
			log.Printf("[steward] %sassigned: %s → %s (P%d) [no-assign: operator handles pods]", prefix, bead.ID, agent, bead.Priority)
			busy[agent] = true
			assigned++
			continue
		}

		// Send assignment message (for external/unmanaged agents).
		msg := fmt.Sprintf("Please claim and work on %s: %s", bead.ID, bead.Title)
		sendArgs := []string{
			"send", agent, msg,
			"--ref", bead.ID,
			"-p", strconv.Itoa(bead.Priority),
			"--as", "steward",
		}
		_, sendErr := runSpire(sendArgs...)
		if sendErr != nil {
			log.Printf("[steward] %ssend failed: %s → %s: %s", prefix, bead.ID, agent, sendErr)
			continue
		}

		log.Printf("[steward] %sassigned: %s → %s (P%d)", prefix, bead.ID, agent, bead.Priority)
		busy[agent] = true
		assigned++

		// Spawn the agent via backend after assignment.
		handle, spawnErr := backend.Spawn(SpawnConfig{
			Name:    agent,
			BeadID:  bead.ID,
			Role:    RoleApprentice,
			LogPath: filepath.Join(doltGlobalDir(), "wizards", agent+".log"),
		})
		if spawnErr != nil {
			log.Printf("[steward] spawn failed: %s → %s: %s", bead.ID, agent, spawnErr)
		} else if handle != nil {
			log.Printf("[steward] spawned %s for %s (%s)", agent, bead.ID, handle.Identifier())
		}
	}

	if assigned > 0 {
		log.Printf("[steward] %sassigned: %d bead(s)", prefix, assigned)
	}

	// Step 4b: Detect standalone tasks ready for review.
	detectReviewReady(dryRun, backend)

	// Step 4c: Detect tasks with review feedback that need wizard re-engagement.
	detectReviewFeedback(dryRun)

	// Step 5: Stale + shutdown check.
	staleCount, shutdownCount := checkBeadHealth(staleThreshold, shutdownThreshold, dryRun, backend)
	if staleCount > 0 || shutdownCount > 0 {
		log.Printf("[steward] %sstale: %d warning(s), %d shutdown(s)", prefix, staleCount, shutdownCount)
	} else {
		log.Printf("[steward] %sstale: none", prefix)
	}

	// Step 6: Push.
	pushState()
}

// beadsDirForTower finds the .beads/ directory for the given tower name.
// Uses the same pattern as the daemon: doltDataDir()/tower.Database/.beads.
func beadsDirForTower(towerName string) string {
	towers, err := listTowerConfigs()
	if err != nil {
		return ""
	}
	for _, t := range towers {
		if t.Name == towerName {
			d := filepath.Join(doltDataDir(), t.Database, ".beads")
			if info, err := os.Stat(d); err == nil && info.IsDir() {
				return d
			}
			return ""
		}
	}
	return ""
}

// checkBeadHealth checks in_progress beads against two thresholds:
//   - stale: wizard exceeded guidelines (warning + alert bead)
//   - shutdown: tower kills the wizard via backend.Kill()
//
// Returns (staleCount, shutdownCount).
func checkBeadHealth(staleThreshold, shutdownThreshold time.Duration, dryRun bool, backend AgentBackend) (int, int) {
	inProgress, err := storeListBeads(beads.IssueFilter{Status: statusPtr(beads.StatusInProgress)})
	if err != nil {
		log.Printf("[steward] check health: %s", err)
		return 0, 0
	}

	now := time.Now()
	staleCount, shutdownCount := 0, 0

	for _, b := range inProgress {
		// Skip review-approved beads — they're parked waiting for merge
		if containsLabel(b, "review-approved") {
			continue
		}

		updatedAt := hasLabel(b, "updated:")
		if updatedAt == "" {
			continue
		}

		t, err := time.Parse(time.RFC3339, updatedAt)
		if err != nil {
			t, err = time.Parse("2006-01-02 15:04:05", updatedAt)
			if err != nil {
				continue
			}
		}

		age := now.Sub(t)
		owner := ""
		if attempt, err := storeGetActiveAttemptFunc(b.ID); err == nil && attempt != nil {
			owner = hasLabel(*attempt, "agent:")
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

// detectReviewReady finds in_progress beads that need review routing.
// A bead is ready for review when:
//   - It has a closed implement step bead (from workflow molecule), AND
//   - It has no review-round child beads (first review), OR all review-round
//     beads are closed with verdict "approve" (re-review after merge failure)
//   - It has no active (in_progress) review-round bead (review already running)
//
// This replaces the legacy label-based query (review-ready label).
func detectReviewReady(dryRun bool, backend AgentBackend) {
	inProgress, err := storeListBeads(beads.IssueFilter{Status: statusPtr(beads.StatusInProgress)})
	if err != nil {
		log.Printf("[steward] detectReviewReady: %s", err)
		return
	}

	for _, b := range inProgress {
		// Skip child beads (step beads, review-round beads, attempt beads).
		if isStepBead(b) || isReviewRoundBead(b) || containsLabel(b, "attempt") {
			continue
		}

		// Check if implement step is closed.
		steps, sErr := storeGetStepBeads(b.ID)
		if sErr != nil || len(steps) == 0 {
			continue // no workflow molecule — not eligible
		}
		implClosed := false
		for _, s := range steps {
			if stepBeadPhaseName(s) == "implement" && s.Status == "closed" {
				implClosed = true
				break
			}
		}
		if !implClosed {
			continue
		}

		// Check review-round beads.
		reviews, rErr := storeGetReviewBeads(b.ID)
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
			verdict := reviewBeadVerdict(lastReview)
			// Only re-route if last verdict was "approve" (merge may have failed).
			// "request_changes" is handled by detectReviewFeedback.
			if verdict != "approve" {
				continue
			}
		}

		// Skip if already approved (label still present from verdict-only mode).
		if containsLabel(b, "review-approved") {
			continue
		}

		if dryRun {
			log.Printf("[steward] [dry-run] would route %s to review pod", b.ID)
			continue
		}

		log.Printf("[steward] routing %s for review (round %d)", b.ID, len(reviews)+1)

		reviewerName := "reviewer-" + sanitizeK8sLabel(b.ID)

		handle, spawnErr := backend.Spawn(SpawnConfig{
			Name:    reviewerName,
			BeadID:  b.ID,
			Role:    RoleSage,
			LogPath: filepath.Join(doltGlobalDir(), "wizards", reviewerName+".log"),
		})
		if spawnErr != nil {
			log.Printf("[steward] failed to spawn reviewer for %s: %v", b.ID, spawnErr)
		} else {
			log.Printf("[steward] spawned reviewer %s for %s (%s)", reviewerName, b.ID, handle.Identifier())
		}
	}
}

// reviewBeadVerdict extracts the verdict string from a closed review-round bead's description.
// The description format is "verdict: <value>\n\n<summary>".
// Returns "" if the bead has no verdict or the description doesn't match the expected format.
func reviewBeadVerdict(b Bead) string {
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

// detectReviewFeedback finds in_progress beads whose last review-round bead
// is closed with verdict "request_changes" and no active attempt bead (wizard
// not already working on it). It re-spawns a wizard to address the feedback.
//
// This replaces the legacy label-based query (review-feedback label).
func detectReviewFeedback(dryRun bool) {
	inProgress, err := storeListBeads(beads.IssueFilter{Status: statusPtr(beads.StatusInProgress)})
	if err != nil {
		log.Printf("[steward] detectReviewFeedback: %s", err)
		return
	}

	for _, b := range inProgress {
		// Skip child beads.
		if isStepBead(b) || isReviewRoundBead(b) || containsLabel(b, "attempt") {
			continue
		}

		// Check review-round beads.
		reviews, rErr := storeGetReviewBeads(b.ID)
		if rErr != nil || len(reviews) == 0 {
			continue
		}

		lastReview := reviews[len(reviews)-1]
		// Must be closed with request_changes verdict.
		if lastReview.Status != "closed" || reviewBeadVerdict(lastReview) != "request_changes" {
			continue
		}

		// Skip if there's already an active attempt (wizard already working on it).
		if attempt, aErr := storeGetActiveAttemptFunc(b.ID); aErr == nil && attempt != nil {
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
		reg := loadWizardRegistry()
		for _, w := range reg.Wizards {
			if w.BeadID == b.ID && w.Phase != "review" {
				owner = w.Name
				break
			}
		}

		msg := fmt.Sprintf("Review feedback on %s: %s — please address feedback on the existing branch and push again", b.ID, b.Title)
		sendArgs := []string{
			"send", owner, msg,
			"--ref", b.ID,
			"-p", strconv.Itoa(b.Priority),
			"--as", "steward",
		}
		if _, err := runSpire(sendArgs...); err != nil {
			log.Printf("[steward] failed to re-engage wizard for %s: %v", b.ID, err)
			continue
		}
	}
}

// sanitizeK8sLabel makes a bead ID safe for k8s resource names.
func sanitizeK8sLabel(s string) string {
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

// runSpire runs a spire subcommand by calling the spire binary.
func runSpire(args ...string) (string, error) {
	// Find our own binary path to call ourselves
	exe, err := os.Executable()
	if err != nil {
		exe = "spire"
	}

	cmd := exec.Command(exe, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err != nil {
		return "", fmt.Errorf("spire %s: %s\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}
