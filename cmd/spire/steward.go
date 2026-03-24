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

func cmdSteward(args []string) error {
	// Parse flags — staleThreshold left at zero to detect "not overridden".
	interval := 2 * time.Minute
	var staleOverride time.Duration
	once := false
	dryRun := false
	noAssign := false // skip sending assignment messages (managed agents get work via operator)
	mode := StewardModeAuto
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
		case "--mode":
			if i+1 >= len(args) {
				return fmt.Errorf("--mode requires a value: auto, local, or k8s")
			}
			i++
			switch args[i] {
			case "auto", "local", "k8s":
				mode = StewardMode(args[i])
			default:
				return fmt.Errorf("--mode: unknown value %q (use: auto, local, k8s)", args[i])
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
			return fmt.Errorf("unknown flag: %s\nusage: spire steward [--once] [--dry-run] [--interval 2m] [--stale-threshold 15m] [--mode auto|local|k8s] [--agents a,b,c]", args[i])
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

	effectiveMode := resolveMode(mode)
	log.Printf("[steward] starting (mode=%s, interval=%s, once=%v, dry-run=%v, stale=%s, shutdown=%s)",
		effectiveMode, interval, once, dryRun, staleThreshold, shutdownThreshold)
	if len(agentList) > 0 {
		log.Printf("[steward] agents: %s", strings.Join(agentList, ", "))
	}

	// Align project_id before the first cycle — ensures metadata.json
	// matches the dolt server even after restarts that change the ID.
	ensureProjectID()

	cycleNum := 1

	// Run first cycle immediately.
	stewardCycle(cycleNum, dryRun, noAssign, effectiveMode, staleThreshold, shutdownThreshold, agentList)
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
			stewardCycle(cycleNum, dryRun, noAssign, effectiveMode, staleThreshold, shutdownThreshold, agentList)
			cycleNum++
		case sig := <-sigCh:
			log.Printf("[steward] received %s, shutting down after %d cycles", sig, cycleNum-1)
			return nil
		}
	}
}

// stewardCycle executes one steward cycle: commit, pull, assess, assign, stale/shutdown check, push.
func stewardCycle(cycleNum int, dryRun, noAssign bool, mode StewardMode, staleThreshold, shutdownThreshold time.Duration, agentList []string) {
	start := time.Now()
	log.Printf("[steward] ═══ cycle %d ═══════════════════════════════", cycleNum)

	// Step 1: Commit any local changes (pull/push disabled — shared dolt server is source of truth).
	_ = storeCommitPending("steward cycle sync")

	// Step 2: Assess — find ready work.
	ready, err := storeGetReadyWork(beads.WorkFilter{})
	if err != nil {
		log.Printf("[steward] ready: error — %s", err)
		pushState()
		log.Printf("[steward] ═══ cycle %d complete (%.1fs) ════════════════", cycleNum, time.Since(start).Seconds())
		return
	}

	// Step 3: Load roster. In local mode, sourced from the wizard registry;
	// in k8s mode, from SpireAgent CRs then bead registrations.
	roster := loadRoster(agentList, mode)
	var busy map[string]bool
	if mode == StewardModeLocal {
		busy = localBusyAgents()
	} else {
		busy = findBusyAgents()
	}
	idleCount := len(roster) - len(busy)
	if idleCount < 0 {
		idleCount = 0
	}

	log.Printf("[steward] ready: %d beads | roster: %d wizard(s) (%d busy, %d idle)",
		len(ready), len(roster), len(busy), idleCount)

	if len(roster) == 0 {
		checkBeadHealth(staleThreshold, shutdownThreshold, dryRun, mode)
		pushState()
		log.Printf("[steward] ═══ cycle %d complete (%.1fs) ════════════════", cycleNum, time.Since(start).Seconds())
		return
	}

	// Load local agent config once if we may need it for spawning.
	var localCfg *LocalStewardConfig
	if mode == StewardModeLocal {
		localCfg = loadLocalStewardConfig()
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
		if hasLabel(bead, "owner:") != "" {
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
			log.Printf("[steward] [dry-run] would assign %s → %s", bead.ID, agent)
			assigned++
			continue
		}

		if noAssign {
			// Managed agents get work via operator (SpireWorkloads), not messages.
			log.Printf("[steward] assigned: %s → %s (P%d) [no-assign: operator handles pods]", bead.ID, agent, bead.Priority)
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
			log.Printf("[steward] send failed: %s → %s: %s", bead.ID, agent, sendErr)
			continue
		}

		log.Printf("[steward] assigned: %s → %s (P%d)", bead.ID, agent, bead.Priority)
		busy[agent] = true
		assigned++

		// In local mode, spawn the agent process after assignment.
		if mode == StewardModeLocal && localCfg != nil {
			pid, spawnErr := spawnLocalAgent(agent, bead.ID, localCfg)
			if spawnErr != nil {
				log.Printf("[steward] spawn failed: %s → %s: %s", bead.ID, agent, spawnErr)
			} else if pid > 0 {
				recordWizardPID(agent, pid)
			}
		}
	}

	if assigned > 0 {
		log.Printf("[steward] assigned: %d bead(s)", assigned)
	}

	// Step 4b: Detect standalone tasks ready for review.
	detectReviewReady(dryRun)

	// Step 4c: Detect tasks with review feedback that need wizard re-engagement.
	detectReviewFeedback(dryRun)

	// Step 5: Stale + shutdown check.
	staleCount, shutdownCount := checkBeadHealth(staleThreshold, shutdownThreshold, dryRun, mode)
	if staleCount > 0 || shutdownCount > 0 {
		log.Printf("[steward] stale: %d warning(s), %d shutdown(s)", staleCount, shutdownCount)
	} else {
		log.Printf("[steward] stale: none")
	}

	// Step 6: Push.
	pushState()

	log.Printf("[steward] ═══ cycle %d complete (%.1fs) ════════════════", cycleNum, time.Since(start).Seconds())
}

// loadRoster returns a list of registered agent names.
//
// Local mode: reads from the wizard registry (created by `spire summon`).
// K8s mode: checks SpireAgent CRs, then falls back to bead registrations.
// An explicit agentList always takes priority.
func loadRoster(agentList []string, mode StewardMode) []string {
	if len(agentList) > 0 {
		return agentList
	}

	// Local mode — use wizard registry as the source of truth for agent slots.
	if mode == StewardModeLocal {
		return localRoster()
	}

	exclude := map[string]bool{
		"steward": true, "mayor": true,
		"spi": true, "awell": true,
	}

	// K8s mode: try SpireAgent CRs first — canonical source in cluster.
	cmd := exec.Command("kubectl", "get", "spireagent", "-n", "spire",
		"-o", "jsonpath={.items[*].metadata.name}")
	if out, err := cmd.Output(); err == nil {
		var names []string
		for _, name := range strings.Fields(strings.TrimSpace(string(out))) {
			if !exclude[name] {
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			return names
		}
	}

	// Fallback: query beads for agent registrations.
	agents, err := storeListBeads(beads.IssueFilter{Labels: []string{"agent"}, Status: statusPtr(beads.StatusOpen)})
	if err != nil {
		log.Printf("[steward] load roster: %s", err)
		return nil
	}

	var names []string
	for _, a := range agents {
		name := hasLabel(a, "name:")
		if name != "" && !exclude[name] {
			names = append(names, name)
		}
	}

	return names
}

// findBusyAgents returns a set of agent names that currently have in_progress beads.
func findBusyAgents() map[string]bool {
	busy := make(map[string]bool)

	inProgress, err := storeListBeads(beads.IssueFilter{Status: statusPtr(beads.StatusInProgress)})
	if err != nil {
		log.Printf("[steward] find busy agents: %s", err)
		return busy
	}

	for _, b := range inProgress {
		owner := hasLabel(b, "owner:")
		if owner != "" {
			busy[owner] = true
		}
	}

	return busy
}

// checkBeadHealth checks in_progress beads against two thresholds:
//   - stale: wizard exceeded guidelines (warning + alert bead)
//   - shutdown: tower kills the wizard (delete pod or kill process)
//
// Returns (staleCount, shutdownCount).
func checkBeadHealth(staleThreshold, shutdownThreshold time.Duration, dryRun bool, mode StewardMode) (int, int) {
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
		owner := hasLabel(b, "owner:")

		if age > shutdownThreshold {
			// Fatal: kill the wizard (pod in k8s, process locally).
			shutdownCount++
			if dryRun {
				log.Printf("[steward] [dry-run] SHUTDOWN: %s (%s) owner=%s age=%s", b.ID, b.Title, owner, age.Round(time.Second))
			} else {
				log.Printf("[steward] SHUTDOWN: %s (%s) owner=%s age=%s — killing wizard", b.ID, b.Title, owner, age.Round(time.Second))
				killWizardProcess(owner, b.ID, mode)
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

// killWizardProcess terminates the wizard responsible for a bead.
// In local mode, sends SIGTERM to the tracked PID.
// In k8s mode, deletes the pod via kubectl.
func killWizardProcess(agentName, beadID string, mode StewardMode) {
	if mode == StewardModeLocal {
		killLocalWizard(agentName, beadID)
		return
	}
	killWizardPod(agentName, beadID)
}

// killWizardPod deletes the k8s pod for a wizard working on a bead.
// Falls back gracefully if not running in k8s.
func killWizardPod(agentName, beadID string) {
	// Try to find and delete the pod by labels.
	cmd := exec.Command("kubectl", "delete", "pod",
		"-n", "spire",
		"-l", fmt.Sprintf("spire.awell.io/agent=%s,spire.awell.io/bead=%s", agentName, beadID),
		"--grace-period=10")
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("[steward] kill pod failed for %s/%s: %s %s", agentName, beadID, err, string(out))
	} else {
		log.Printf("[steward] killed pod for %s/%s", agentName, beadID)
	}
}

// detectReviewReady finds standalone tasks with the "review-ready" label
// and routes them to a review pod (artificer --mode=review).
func detectReviewReady(dryRun bool) {
	reviewBeads, err := storeListBeads(beads.IssueFilter{Labels: []string{"review-ready"}, Status: statusPtr(beads.StatusInProgress)})
	if err != nil {
		log.Printf("[steward] detectReviewReady: %s", err)
		return
	}

	for _, b := range reviewBeads {
		// Skip if already assigned for review.
		if hasLabel(b, "review-assigned") != "" || containsLabel(b, "review-assigned") {
			continue
		}
		// Skip if already approved.
		if containsLabel(b, "review-approved") {
			continue
		}

		if dryRun {
			log.Printf("[steward] [dry-run] would route %s to review pod", b.ID)
			continue
		}

		log.Printf("[steward] routing %s for standalone review", b.ID)

		// Mark as review-assigned so we don't double-route.
		storeAddLabel(b.ID, "review-assigned")

		if isK8sAvailable() {
			// K8s mode: create a SpireWorkload CR for the operator.
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(fmt.Sprintf(`apiVersion: spire.awell.io/v1alpha1
kind: SpireWorkload
metadata:
  name: review-%s
  namespace: spire
spec:
  beadId: %s
  title: "Review %s"
  priority: %d
  type: review
`, sanitizeK8sLabel(b.ID), b.ID, b.Title, b.Priority))
			if out, err := cmd.CombinedOutput(); err != nil {
				log.Printf("[steward] failed to create review workload for %s: %v\n%s", b.ID, err, string(out))
				// Roll back review-assigned so the next cycle can retry.
				storeRemoveLabel(b.ID, "review-assigned")
			}
		} else {
			// Local mode: spawn wizard-review directly.
			implBy := hasLabel(b, "implemented-by:")
			reviewerName := "reviewer-" + sanitizeK8sLabel(b.ID)
			if implBy != "" {
				reviewerName = implBy + "-review"
			}

			spireBin, _ := os.Executable()
			logDir := filepath.Join(doltGlobalDir(), "wizards")
			os.MkdirAll(logDir, 0755)
			logFile, _ := os.OpenFile(filepath.Join(logDir, reviewerName+".log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)

			cmd := exec.Command(spireBin, "wizard-review", b.ID, "--name", reviewerName)
			cmd.Env = os.Environ()
			if logFile != nil {
				cmd.Stdout = logFile
				cmd.Stderr = logFile
			}
			if err := cmd.Start(); err != nil {
				log.Printf("[steward] failed to spawn local reviewer for %s: %v", b.ID, err)
				storeRemoveLabel(b.ID, "review-assigned")
				if logFile != nil {
					logFile.Close()
				}
			} else {
				log.Printf("[steward] spawned local reviewer %s for %s (pid %d)", reviewerName, b.ID, cmd.Process.Pid)
				if logFile != nil {
					logFile.Close()
				}
			}
		}
	}
}

// detectReviewFeedback finds tasks with "review-feedback" label (without
// "review-ready" or "review-assigned") and re-spawns a wizard to address feedback.
func detectReviewFeedback(dryRun bool) {
	feedbackBeads, err := storeListBeads(beads.IssueFilter{Labels: []string{"review-feedback"}, Status: statusPtr(beads.StatusInProgress)})
	if err != nil {
		log.Printf("[steward] detectReviewFeedback: %s", err)
		return
	}

	for _, b := range feedbackBeads {
		// Skip if already re-queued for review or reassigned.
		if containsLabel(b, "review-ready") || containsLabel(b, "review-assigned") {
			continue
		}

		if dryRun {
			log.Printf("[steward] [dry-run] would re-engage wizard for %s (review feedback)", b.ID)
			continue
		}

		log.Printf("[steward] re-engaging wizard for %s (review feedback)", b.ID)

		// Find the wizard owner and send an assignment message.
		owner := hasLabel(b, "owner:")
		if owner == "" {
			owner = "wizard" // fallback
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

		// Remove review-feedback so we don't re-trigger, the wizard will add review-ready when done.
		storeRemoveLabel(b.ID, "review-feedback")
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
