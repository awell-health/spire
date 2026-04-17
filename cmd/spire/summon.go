package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/executor"
	spgit "github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/spf13/cobra"
	"github.com/steveyegge/beads"
)

// --- Type aliases for backward compatibility ---

type wizardRegistry = agent.Registry
type localWizard = agent.Entry

var summonCmd = &cobra.Command{
	Use:   "summon <bead-id>... | <N> [flags]",
	Short: "Summon wizards (--targets <ids>, --auto)",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var fullArgs []string
		targets, _ := cmd.Flags().GetString("targets")
		target, _ := cmd.Flags().GetString("target")
		if targets != "" && target != "" {
			return fmt.Errorf("--target and --targets are aliases; use one or the other, not both")
		}
		if targets != "" {
			fullArgs = append(fullArgs, "--targets", targets)
		}
		if target != "" {
			// Alias: --target maps to --targets
			fullArgs = append(fullArgs, "--targets", target)
		}
		if auto, _ := cmd.Flags().GetBool("auto"); auto {
			fullArgs = append(fullArgs, "--auto")
		}
		if dispatch, _ := cmd.Flags().GetString("dispatch"); dispatch != "" {
			fullArgs = append(fullArgs, "--dispatch", dispatch)
		}
		fullArgs = append(fullArgs, args...)
		return cmdSummon(fullArgs)
	},
}

var dismissCmd = &cobra.Command{
	Use:   "dismiss <N|--all> [flags]",
	Short: "Dismiss wizards (--all, --targets)",
	RunE: func(cmd *cobra.Command, args []string) error {
		var fullArgs []string
		if all, _ := cmd.Flags().GetBool("all"); all {
			fullArgs = append(fullArgs, "--all")
		}
		targets, _ := cmd.Flags().GetString("targets")
		target, _ := cmd.Flags().GetString("target")
		if targets != "" && target != "" {
			return fmt.Errorf("--target and --targets are aliases; use one or the other, not both")
		}
		if targets != "" {
			fullArgs = append(fullArgs, "--targets", targets)
		}
		if target != "" {
			fullArgs = append(fullArgs, "--targets", target)
		}
		fullArgs = append(fullArgs, args...)
		return cmdDismiss(fullArgs)
	},
}

func init() {
	summonCmd.Flags().String("targets", "", "Comma-separated bead IDs to target")
	summonCmd.Flags().String("target", "", "Alias for --targets")
	summonCmd.Flags().Bool("auto", false, "Auto mode")
	summonCmd.Flags().String("dispatch", "", "Override dispatch mode: sequential, wave, or direct (persists as dispatch:<mode> label; omit to use formula default)")


	dismissCmd.Flags().Bool("all", false, "Dismiss all wizards")
	dismissCmd.Flags().String("targets", "", "Comma-separated bead IDs to dismiss")
	dismissCmd.Flags().String("target", "", "Alias for --targets")
}

// --- Registry function wrappers ---

func wizardRegistryPath() string                              { return agent.RegistryPath() }
func loadWizardRegistry() wizardRegistry                      { return agent.LoadRegistry() }
func saveWizardRegistry(reg wizardRegistry)                   { agent.SaveRegistry(reg) }
func wizardRegistryAdd(entry localWizard) error               { return agent.RegistryAdd(entry) }
func wizardRegistryRemove(name string) error                  { return agent.RegistryRemove(name) }
func wizardRegistryUpdate(name string, f func(*localWizard)) error {
	return agent.RegistryUpdate(name, f)
}
func findLiveWizardForBead(reg wizardRegistry, beadID string) *localWizard {
	return agent.FindLiveForBead(reg, beadID)
}
func wizardsForTower(reg wizardRegistry, tower string) []localWizard {
	return agent.WizardsForTower(reg, tower)
}

func cmdSummon(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: spire summon <bead-id>... | <N> [--targets <ids>] [--auto] [--dispatch <mode>]")
	}

	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	var count int
	var targets string
	var auto bool
	var dispatch string
	var targetIDs []string
	var hasExplicitCount bool

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--for":
			return fmt.Errorf("--for has been removed; use --targets <bead-id> for exact targeting")
		case "--targets":
			if i+1 >= len(args) {
				return fmt.Errorf("--targets requires comma-separated bead IDs")
			}
			i++
			targets = args[i]
		case "--dispatch":
			if i+1 >= len(args) {
				return fmt.Errorf("--dispatch requires a mode: sequential, wave, or direct")
			}
			i++
			dispatch = args[i]
		case "--auto":
			auto = true
		default:
			if strings.Contains(args[i], "-") {
				// Looks like a bead ID (e.g. spi-xxx)
				targetIDs = append(targetIDs, args[i])
			} else {
				n, err := strconv.Atoi(args[i])
				if err != nil {
					return fmt.Errorf("expected a bead ID or number, got %q\nusage: spire summon <bead-id>... | <N> [--targets <ids>] [--auto] [--dispatch <mode>]", args[i])
				}
				count = n
				hasExplicitCount = true
			}
		}
	}

	// Validate dispatch mode if provided.
	if dispatch != "" {
		switch dispatch {
		case "sequential", "wave", "direct":
			// valid
		default:
			return fmt.Errorf("invalid dispatch mode %q: must be sequential, wave, or direct", dispatch)
		}
	}

	if auto {
		fmt.Println("Auto mode not yet implemented. Run spire summon N to summon agents manually.")
		return nil
	}

	// Positional bead IDs and --targets are mutually exclusive.
	if len(targetIDs) > 0 && targets != "" {
		return fmt.Errorf("cannot combine positional bead IDs with --targets")
	}

	// Positional bead IDs and an explicit numeric count are mutually exclusive.
	if len(targetIDs) > 0 && hasExplicitCount {
		return fmt.Errorf("cannot combine positional bead IDs with a numeric count")
	}

	// Positional bead IDs: infer count from the number of IDs.
	if len(targetIDs) > 0 {
		count = len(targetIDs)
	}

	// If --targets provided, split and pass directly.
	if targets != "" {
		for _, id := range strings.Split(targets, ",") {
			id = strings.TrimSpace(id)
			if id != "" {
				targetIDs = append(targetIDs, id)
			}
		}
		count = len(targetIDs)
	}

	if count <= 0 {
		return fmt.Errorf("summon requires a positive number")
	}

	// Detect mode: k8s or local.
	if isK8sAvailable() {
		return summonK8s(count)
	}
	return summonLocal(count, targetIDs, dispatch)
}

func cmdDismiss(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: spire dismiss <N|--all> [--targets <ids>]")
	}

	dismissAll := false
	count := 0
	var targets string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--all":
			dismissAll = true
		case "--targets":
			if i+1 >= len(args) {
				return fmt.Errorf("--targets requires comma-separated bead IDs")
			}
			i++
			targets = args[i]
		default:
			n, err := strconv.Atoi(args[i])
			if err != nil {
				return fmt.Errorf("expected a number, --all, or --targets, got %q", args[i])
			}
			count = n
		}
	}

	var targetIDs []string
	if targets != "" {
		for _, id := range strings.Split(targets, ",") {
			id = strings.TrimSpace(id)
			if id != "" {
				targetIDs = append(targetIDs, id)
			}
		}
	}

	if isK8sAvailable() {
		return dismissK8s(count, dismissAll)
	}
	return dismissLocal(count, dismissAll, targetIDs)
}

// --- k8s mode ---

func summonK8s(count int) error {
	// Find existing wizard count to name them sequentially.
	existing := countK8sWizards()

	for i := 0; i < count; i++ {
		name := fmt.Sprintf("wizard-%d", existing+i+1)
		if err := createSpireAgentCR(name); err != nil {
			return fmt.Errorf("failed to summon %s: %w", name, err)
		}
		fmt.Printf("  %s%s%s summoned to the tower\n", cyan, name, reset)
	}

	fmt.Printf("\n%d wizard(s) summoned. The steward will assign work on the next cycle.\n", count)
	return nil
}

func dismissK8s(count int, all bool) error {
	wizards := listK8sWizards()
	if all {
		count = len(wizards)
	}
	if count > len(wizards) {
		count = len(wizards)
	}
	if count == 0 {
		fmt.Println("No wizards to dismiss.")
		return nil
	}

	// Dismiss from the end (highest numbered first).
	for i := len(wizards) - 1; i >= len(wizards)-count; i-- {
		name := wizards[i]
		if err := deleteSpireAgentCR(name); err != nil {
			log.Printf("failed to dismiss %s: %v", name, err)
			continue
		}
		fmt.Printf("  %s%s%s dismissed from the tower\n", dim, name, reset)
	}

	fmt.Printf("\n%d wizard(s) dismissed.\n", count)
	return nil
}

func isK8sAvailable() bool {
	cmd := exec.Command("kubectl", "get", "ns", "spire", "--no-headers")
	return cmd.Run() == nil
}

func countK8sWizards() int {
	return len(listK8sWizards())
}

func listK8sWizards() []string {
	cmd := exec.Command("kubectl", "get", "spireagent", "-n", "spire",
		"-o", "jsonpath={.items[*].metadata.name}")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	names := strings.Fields(strings.TrimSpace(string(out)))
	var wizards []string
	for _, n := range names {
		if strings.HasPrefix(n, "wizard-") {
			wizards = append(wizards, n)
		}
	}
	return wizards
}

func createSpireAgentCR(name string) error {
	// Detect repo URL and default branch from the cwd's git repo. Prefer
	// spire.yaml's branch.base, then the checked-out branch, then the
	// system default. Avoids hardcoding "main" for repos that base work on
	// develop/trunk/etc.
	cwd, _ := os.Getwd()
	rc := &spgit.RepoContext{Dir: cwd}
	repoURL := rc.RemoteURL("origin")

	repoBranch := ""
	if cfg, err := repoconfig.Load(cwd); err == nil && cfg != nil {
		repoBranch = cfg.Branch.Base
	}
	if repoBranch == "" {
		if b := rc.CurrentBranch(); b != "" && b != "HEAD" {
			repoBranch = b
		}
	}
	repoBranch = repoconfig.ResolveBranchBase(repoBranch)

	manifest := fmt.Sprintf(`apiVersion: spire.awell.io/v1alpha1
kind: SpireAgent
metadata:
  name: %s
  namespace: spire
spec:
  mode: managed
  displayName: "%s"
  prefixes:
    - "spi-"
  maxConcurrent: 1
  repo: "%s"
  repoBranch: "%s"
`, name, name, repoURL, repoBranch)

	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", err, string(out))
	}
	return nil
}

func deleteSpireAgentCR(name string) error {
	cmd := exec.Command("kubectl", "delete", "spireagent", name, "-n", "spire")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", err, string(out))
	}
	return nil
}

// --- Local mode ---

func summonLocal(count int, targetIDs []string, dispatch string) error {
	var candidates []Bead

	reg := loadWizardRegistry()
	before := len(reg.Wizards)
	reg = cleanDeadWizards(reg, false)
	if len(reg.Wizards) < before {
		saveWizardRegistry(reg)
	}

	if len(targetIDs) > 0 {
		// Look up each target bead directly.
		for _, id := range targetIDs {
			bead, err := storeGetBeadFunc(id)
			if err != nil {
				return fmt.Errorf("target %s: %w", id, err)
			}
			if bead.Type == "design" {
				return fmt.Errorf("target %s is a design bead — design beads are not executable. Use spire approve to close it", id)
			}
			switch bead.Status {
			case "closed", "done":
				return fmt.Errorf("target %s is closed — reopen it first (bd update %s --status open)", id, id)
			case "deferred":
				return fmt.Errorf("target %s is deferred — set to open or ready first (bd update %s --status open)", id, id)
			case "hooked":
				// Transition to in_progress before summoning — do NOT unhook step beads,
				// let the wizard/executor evaluate the hook condition and decide.
				if err := storeUpdateBead(id, map[string]interface{}{"status": "in_progress"}); err != nil {
					return fmt.Errorf("transition hooked bead %s to in_progress: %w", id, err)
				}
			}
			candidates = append(candidates, bead)
		}
		count = len(candidates)
	} else {
		// Find ready beads to assign — all bead types welcome.
		ready, err := storeGetReadyWork(beads.WorkFilter{})
		if err != nil {
			return fmt.Errorf("query ready work: %w", err)
		}

		// Prepend orphaned resumable beads (have executor state but no live process).
		orphans := scanOrphanedBeads(reg)
		if len(orphans) > 0 {
			readyIDs := make(map[string]bool)
			for _, b := range ready {
				readyIDs[b.ID] = true
			}
			for _, b := range orphans {
				if !readyIDs[b.ID] {
					candidates = append(candidates, b)
				}
			}
		}
		candidates = append(candidates, ready...)
	}

	if len(candidates) == 0 {
		fmt.Println("No ready beads to work on.")
		return nil
	}
	if count > len(candidates) {
		fmt.Printf("Only %d ready bead(s) available (requested %d).\n", len(candidates), count)
		count = len(candidates)
	}

	// Persist dispatch override as a label on each target bead.
	if dispatch != "" {
		for _, bead := range candidates[:count] {
			// Remove any existing dispatch: label to ensure at most one.
			for _, l := range bead.Labels {
				if strings.HasPrefix(l, "dispatch:") {
					if err := storeRemoveLabel(bead.ID, l); err != nil {
						return fmt.Errorf("remove existing dispatch label %q for %s: %w", l, bead.ID, err)
					}
				}
			}
			if err := storeAddLabel(bead.ID, "dispatch:"+dispatch); err != nil {
				return fmt.Errorf("persist dispatch override for %s: %w", bead.ID, err)
			}
			fmt.Printf("  %s → dispatch override: %s%s\n", bead.ID, dispatch, reset)
		}
	}

	logDir := filepath.Join(doltGlobalDir(), "wizards")
	backend := ResolveBackend("")

	spawned := 0
	for i := 0; i < count; i++ {
		bead := candidates[i]
		name := "wizard-" + bead.ID

		// Skip if a live wizard is already running for this bead.
		if w := findLiveWizardForBead(reg, bead.ID); w != nil && processAlive(w.PID) {
			fmt.Printf("  %s already running for %s (pid %d) — skipping\n", w.Name, bead.ID, w.PID)
			continue
		}

		// Check for existing graph state to determine resume vs fresh start.
		existingGraphState, _ := executor.LoadGraphState(name, configDir)

		// Resolve formula for the bead — best-effort, fall back to default.
		formulaName := resolveFormulaName(bead)

		// Resolve tower for this wizard.
		towerName := ""
		if tc, err := activeTowerConfig(); err == nil {
			towerName = tc.Name
		} else if tName := os.Getenv("SPIRE_TOWER"); tName != "" {
			towerName = tName
		} else if cfg, err := loadConfig(); err == nil && cfg.ActiveTower != "" {
			towerName = cfg.ActiveTower
		}

		handle, err := backend.Spawn(SpawnConfig{
			Name:      name,
			BeadID:    bead.ID,
			Role:      RoleExecutor,
			Tower:     towerName,
			LogPath:   filepath.Join(logDir, name+".log"),
			ExtraArgs: []string{"--formula", formulaName},
		})
		if err != nil {
			return fmt.Errorf("spawn %s: %w", name, err)
		}

		pid, _ := strconv.Atoi(handle.Identifier())
		worktree := filepath.Join(os.TempDir(), "spire-wizard", name, bead.ID)
		if err := wizardRegistryAdd(localWizard{
			Name:      name,
			PID:       pid,
			BeadID:    bead.ID,
			Worktree:  worktree,
			StartedAt: time.Now().UTC().Format(time.RFC3339),
			Tower:     towerName,
		}); err != nil {
			log.Printf("warning: registry add for %s: %v", name, err)
		}

		if existingGraphState != nil && existingGraphState.ActiveStep != "" {
			fmt.Printf("  %s%s%s → resuming %s from %s step [%s] formula=%s\n", cyan, name, reset, bead.ID, existingGraphState.ActiveStep, handle.Identifier(), formulaName)
		} else {
			fmt.Printf("  %s%s%s → starting %s (%s) [%s] formula=%s\n", cyan, name, reset, bead.ID, bead.Title, handle.Identifier(), formulaName)
		}
		spawned++
	}

	fmt.Printf("\n%d wizard(s) summoned. Logs: %s\n", spawned, logDir)
	fmt.Printf("Run %sspire roster%s to check status.\n", bold, reset)
	return nil
}

func dismissLocal(count int, all bool, targets []string) error {
	reg := loadWizardRegistry()
	// Don't clean dead wizards first — we need them to clean up bead state.

	// Resolve current tower for scoping.
	currentTower := ""
	if tc, err := activeTowerConfig(); err == nil {
		currentTower = tc.Name
	} else if tName := os.Getenv("SPIRE_TOWER"); tName != "" {
		currentTower = tName
	} else if cfg, err := loadConfig(); err == nil && cfg.ActiveTower != "" {
		currentTower = cfg.ActiveTower
	}

	// Separate wizards by tower scope.
	var scoped []localWizard
	var other []localWizard
	for _, w := range reg.Wizards {
		if currentTower == "" || w.Tower == "" || w.Tower == currentTower {
			scoped = append(scoped, w)
		} else {
			other = append(other, w)
		}
	}

	// When --targets is given, dismiss exactly those wizards by BeadID.
	if len(targets) > 0 {
		targetSet := make(map[string]bool, len(targets))
		for _, id := range targets {
			targetSet[id] = true
		}

		dismissed := 0
		var remaining []localWizard
		for _, w := range scoped {
			if !targetSet[w.BeadID] {
				remaining = append(remaining, w)
				continue
			}
			alive := w.PID > 0 && processAlive(w.PID)
			if alive {
				if proc, err := os.FindProcess(w.PID); err == nil {
					proc.Signal(os.Interrupt)
				}
			}
			dismissCleanupBead(w)
			if alive {
				fmt.Printf("  %s%s%s dismissed (killed pid %d)\n", dim, w.Name, reset, w.PID)
			} else {
				fmt.Printf("  %s%s%s dismissed (was dead)\n", dim, w.Name, reset)
			}
			dismissed++
			delete(targetSet, w.BeadID)
		}

		// Warn about any targets that weren't found.
		for id := range targetSet {
			fmt.Printf("  %s(warning: no wizard found for bead %s — skipped)%s\n", dim, id, reset)
		}

		reg.Wizards = append(other, remaining...)
		saveWizardRegistry(reg)
		fmt.Printf("\n%d wizard(s) dismissed.\n", dismissed)
		return nil
	}

	if all {
		count = len(scoped)
	}
	if count > len(scoped) {
		count = len(scoped)
	}
	if count == 0 {
		fmt.Println("No local wizards to dismiss.")
		return nil
	}

	// Dismiss from the end of scoped list.
	for i := 0; i < count; i++ {
		idx := len(scoped) - 1 - i
		w := scoped[idx]
		alive := w.PID > 0 && processAlive(w.PID)
		if alive {
			if proc, err := os.FindProcess(w.PID); err == nil {
				proc.Signal(os.Interrupt)
			}
		}
		dismissCleanupBead(w)
		if alive {
			fmt.Printf("  %s%s%s dismissed (killed pid %d)\n", dim, w.Name, reset, w.PID)
		} else {
			fmt.Printf("  %s%s%s dismissed (was dead)\n", dim, w.Name, reset)
		}
	}

	// Rebuild: keep other tower wizards + remaining scoped wizards.
	remaining := other
	remaining = append(remaining, scoped[:len(scoped)-count]...)
	reg.Wizards = remaining
	saveWizardRegistry(reg)
	fmt.Printf("\n%d wizard(s) dismissed.\n", count)
	return nil
}

// dismissCleanupBead removes the owner label and reopens the bead if it's still in_progress.
// Does NOT reopen closed beads — they were closed intentionally.
func dismissCleanupBead(w localWizard) {
	if w.BeadID == "" {
		return
	}

	// Close any open alert beads that reference this bead.
	closeRelatedAlerts(w.BeadID)

	// Only reopen beads that are not closed (in_progress or open).
	bead, err := storeGetBead(w.BeadID)
	if err != nil {
		return
	}
	if bead.Status == "closed" {
		return // intentionally closed — do not reopen
	}
	if err := storeUpdateBead(w.BeadID, map[string]interface{}{"status": "open"}); err != nil {
		fmt.Printf("  %s(note: could not reopen %s: %s)%s\n", dim, w.BeadID, err, reset)
	} else {
		fmt.Printf("  %s↺ %s reopened%s\n", yellow, w.BeadID, reset)
	}
}

func cleanDeadWizards(reg wizardRegistry, quiet bool) wizardRegistry {
	var alive []localWizard
	for _, w := range reg.Wizards {
		if w.PID <= 0 {
			continue // placeholder entry with no real process — prune it
		}
		if !processAlive(w.PID) {
			reapDeadWizard(w, quiet)
			continue // dead
		}
		alive = append(alive, w)
	}
	reg.Wizards = alive
	return reg
}

// reapDeadWizard cleans up stale state for a wizard whose process is no longer alive.
// It removes the state file and cleans up bead labels so the bead can be re-summoned.
// When quiet is true, stdout output is suppressed (for TUI and JSON output paths).
func reapDeadWizard(w localWizard, quiet bool) {
	// Delete v3 graph state files (parent + nested sub-executors).
	removedState := removeGraphStateFilesQuiet(w.Name)

	if !quiet {
		if removedState {
			fmt.Printf("reaped stale wizard %s for %s (removed graph state)\n", w.Name, w.BeadID)
		} else {
			fmt.Printf("reaped stale wizard %s for %s\n", w.Name, w.BeadID)
		}
	}

	// Clean up bead labels and reopen if orphaned — but NOT if intentionally closed.
	if w.BeadID != "" {
		if w.Phase != "" {
			storeRemoveLabel(w.BeadID, "phase:"+w.Phase)
		}
		// Only reopen beads that are still in_progress (wizard died mid-work).
		// Do NOT reopen closed beads — they were closed intentionally.
		if bead, err := storeGetBead(w.BeadID); err == nil && bead.Status != "closed" {
			if err := storeUpdateBead(w.BeadID, map[string]interface{}{"status": "open"}); err == nil {
				if !quiet {
					fmt.Printf("  ↺ %s reopened\n", w.BeadID)
				}
			}
		}
	}
}

// scanOrphanedBeads returns beads that have executor state but no live wizard process.
// These are resumable candidates — the work was interrupted mid-flight.
func scanOrphanedBeads(liveReg wizardRegistry) []Bead {
	dir, err := configDir()
	if err != nil {
		return nil
	}

	runtimeDir := filepath.Join(dir, "runtime")
	entries, err := os.ReadDir(runtimeDir)
	if err != nil {
		return nil
	}

	// Build set of live agent names from the cleaned registry.
	liveAgents := make(map[string]bool)
	for _, w := range liveReg.Wizards {
		liveAgents[w.Name] = true
	}

	seen := make(map[string]bool)
	var orphans []Bead

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		agentName := entry.Name()

		// Skip agents that have a live process.
		if liveAgents[agentName] {
			continue
		}

		// Read graph_state.json to extract the bead ID.
		var beadID string
		gsPath := filepath.Join(runtimeDir, agentName, "graph_state.json")
		gsData, err := os.ReadFile(gsPath)
		if err == nil {
			var gs executor.GraphState
			if err := json.Unmarshal(gsData, &gs); err == nil {
				beadID = gs.BeadID
			}
		}

		if beadID == "" || seen[beadID] {
			continue
		}
		seen[beadID] = true

		bead, err := storeGetBead(beadID)
		if err != nil {
			continue
		}

		// Only resumable if still open/in_progress.
		if bead.Status == "closed" || bead.Status == "done" {
			continue
		}

		orphans = append(orphans, bead)
	}

	return orphans
}
