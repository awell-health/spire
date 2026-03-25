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

	"github.com/steveyegge/beads"
)

// wizardRegistry tracks locally summoned wizards.
type wizardRegistry struct {
	Wizards []localWizard `json:"wizards"`
}

type localWizard struct {
	Name           string `json:"name"`
	PID            int    `json:"pid"`
	BeadID         string `json:"bead_id"`
	Worktree       string `json:"worktree"`
	StartedAt      string `json:"started_at"`
	Phase          string `json:"phase,omitempty"`
	PhaseStartedAt string `json:"phase_started_at,omitempty"`
	Tower          string `json:"tower,omitempty"`
}

func cmdSummon(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: spire summon <N> [--for <epic-id>] [--targets <ids>] [--auto]")
	}

	var count int
	var forEpic string
	var targets string
	var auto bool

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--for":
			if i+1 >= len(args) {
				return fmt.Errorf("--for requires an epic bead ID")
			}
			i++
			forEpic = args[i]
		case "--targets":
			if i+1 >= len(args) {
				return fmt.Errorf("--targets requires comma-separated bead IDs")
			}
			i++
			targets = args[i]
		case "--auto":
			auto = true
		default:
			n, err := strconv.Atoi(args[i])
			if err != nil {
				return fmt.Errorf("expected a number, got %q\nusage: spire summon <N> [--for <epic-id>] [--targets <ids>] [--auto]", args[i])
			}
			count = n
		}
	}

	if auto {
		fmt.Println("Auto mode not yet implemented. Run spire summon N to summon agents manually.")
		return nil
	}

	// If --targets provided, split and pass directly.
	var targetIDs []string
	if targets != "" {
		for _, id := range strings.Split(targets, ",") {
			id = strings.TrimSpace(id)
			if id != "" {
				targetIDs = append(targetIDs, id)
			}
		}
		count = len(targetIDs)
	}

	// If --for epic, count = number of ready children.
	if forEpic != "" && count == 0 {
		ready, err := storeGetReadyWork(beads.WorkFilter{})
		if err == nil {
			for _, b := range ready {
				if b.Parent == forEpic || strings.HasPrefix(b.ID, forEpic+".") {
					count++
				}
			}
		}
		if count == 0 {
			fmt.Printf("No ready children for %s. Nothing to summon.\n", forEpic)
			return nil
		}
		fmt.Printf("Epic %s has %d ready children. Summoning %d wizard(s).\n", forEpic, count, count)
	}

	if count <= 0 {
		return fmt.Errorf("summon requires a positive number")
	}

	// Detect mode: k8s or local.
	if isK8sAvailable() {
		return summonK8s(count)
	}
	return summonLocal(count, targetIDs)
}

func cmdDismiss(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: spire dismiss <N|--all>")
	}

	dismissAll := false
	count := 0

	for _, arg := range args {
		switch arg {
		case "--all":
			dismissAll = true
		default:
			n, err := strconv.Atoi(arg)
			if err != nil {
				return fmt.Errorf("expected a number or --all, got %q", arg)
			}
			count = n
		}
	}

	if isK8sAvailable() {
		return dismissK8s(count, dismissAll)
	}
	return dismissLocal(count, dismissAll)
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
	// Detect repo URL from git remote.
	repoURL := ""
	if out, err := exec.Command("git", "config", "--get", "remote.origin.url").Output(); err == nil {
		repoURL = strings.TrimSpace(string(out))
	}

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
  repoBranch: "main"
`, name, name, repoURL)

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

func summonLocal(count int, targetIDs []string) error {
	var candidates []Bead

	reg := loadWizardRegistry()
	reg = cleanDeadWizards(reg)

	if len(targetIDs) > 0 {
		// Look up each target bead directly.
		for _, id := range targetIDs {
			bead, err := storeGetBead(id)
			if err != nil {
				return fmt.Errorf("target %s: %w", id, err)
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

		// Check for existing executor state to determine resume vs fresh start.
		existingState, _ := loadExecutorState(name)

		// Resolve formula for the bead — best-effort, fall back to default.
		formulaName := resolveFormulaName(bead)

		handle, err := backend.Spawn(SpawnConfig{
			Name:      name,
			BeadID:    bead.ID,
			Role:      RoleExecutor,
			LogPath:   filepath.Join(logDir, name+".log"),
			ExtraArgs: []string{"--formula", formulaName},
		})
		if err != nil {
			return fmt.Errorf("spawn %s: %w", name, err)
		}

		// Resolve tower for this wizard.
		towerName := ""
		if tc, err := activeTowerConfig(); err == nil {
			towerName = tc.Name
		} else if tName := os.Getenv("SPIRE_TOWER"); tName != "" {
			towerName = tName
		} else if cfg, err := loadConfig(); err == nil && cfg.ActiveTower != "" {
			towerName = cfg.ActiveTower
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

		if existingState != nil && existingState.Phase != "" {
			fmt.Printf("  %s%s%s → resuming %s from %s phase [%s] formula=%s\n", cyan, name, reset, bead.ID, existingState.Phase, handle.Identifier(), formulaName)
		} else {
			fmt.Printf("  %s%s%s → starting %s (%s) [%s] formula=%s\n", cyan, name, reset, bead.ID, bead.Title, handle.Identifier(), formulaName)
		}
		spawned++
	}

	fmt.Printf("\n%d wizard(s) summoned. Logs: %s\n", spawned, logDir)
	fmt.Printf("Run %sspire roster%s to check status.\n", bold, reset)
	return nil
}

func dismissLocal(count int, all bool) error {
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
func dismissCleanupBead(w localWizard) {
	if w.BeadID == "" {
		return
	}

	// Remove owner label.
	storeRemoveLabel(w.BeadID, "owner:"+w.Name)

	// Remove any other wizard labels.
	storeRemoveLabel(w.BeadID, "implemented-by:"+w.Name)
	storeRemoveLabel(w.BeadID, "review-ready")
	storeRemoveLabel(w.BeadID, "review-feedback")

	// Reopen if still in_progress.
	if err := storeUpdateBead(w.BeadID, map[string]interface{}{"status": "open"}); err != nil {
		// Not fatal — bead may already be open or closed.
		fmt.Printf("  %s(note: could not reopen %s: %s)%s\n", dim, w.BeadID, err, reset)
	} else {
		fmt.Printf("  %s↺ %s reopened%s\n", yellow, w.BeadID, reset)
	}
}

func wizardRegistryPath() string {
	dir, err := configDir()
	if err != nil {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config", "spire")
	}
	return filepath.Join(dir, "wizards.json")
}

func loadWizardRegistry() wizardRegistry {
	var reg wizardRegistry
	data, err := os.ReadFile(wizardRegistryPath())
	if err != nil {
		return reg
	}
	json.Unmarshal(data, &reg)
	return reg
}

func saveWizardRegistry(reg wizardRegistry) {
	path := wizardRegistryPath()
	os.MkdirAll(filepath.Dir(path), 0755)
	data, _ := json.MarshalIndent(reg, "", "  ")
	os.WriteFile(path, data, 0644)
}

func cleanDeadWizards(reg wizardRegistry) wizardRegistry {
	var alive []localWizard
	for _, w := range reg.Wizards {
		if w.PID <= 0 {
			continue // placeholder entry with no real process — prune it
		}
		if !processAlive(w.PID) {
			continue // dead
		}
		alive = append(alive, w)
	}
	reg.Wizards = alive
	return reg
}

// wizardsForTower returns wizards matching the given tower (or all if tower is "").
func wizardsForTower(reg wizardRegistry, tower string) []localWizard {
	if tower == "" {
		return reg.Wizards
	}
	var result []localWizard
	for _, w := range reg.Wizards {
		if w.Tower == tower {
			result = append(result, w)
		}
	}
	return result
}

// wizardRegistryLock acquires a file lock for the wizard registry.
// Returns a cleanup function that releases the lock.
func wizardRegistryLock() (func(), error) {
	lockPath := wizardRegistryPath() + ".lock"
	os.MkdirAll(filepath.Dir(lockPath), 0755)

	deadline := time.Now().Add(5 * time.Second)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err == nil {
			f.Close()
			return func() { os.Remove(lockPath) }, nil
		}
		if time.Now().After(deadline) {
			// Force-remove stale lock and retry once
			os.Remove(lockPath)
			f, err = os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
			if err != nil {
				return nil, fmt.Errorf("acquire registry lock: %w", err)
			}
			f.Close()
			return func() { os.Remove(lockPath) }, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// wizardRegistryAdd adds or replaces an entry in the wizard registry (file-locked).
func wizardRegistryAdd(entry localWizard) error {
	unlock, err := wizardRegistryLock()
	if err != nil {
		return err
	}
	defer unlock()

	reg := loadWizardRegistry()

	// Deduplicate by name — replace if exists, append if new.
	found := false
	for i, w := range reg.Wizards {
		if w.Name == entry.Name {
			reg.Wizards[i] = entry
			found = true
			break
		}
	}
	if !found {
		reg.Wizards = append(reg.Wizards, entry)
	}

	saveWizardRegistry(reg)
	return nil
}

// wizardRegistryRemove removes an entry by name from the wizard registry (file-locked).
func wizardRegistryRemove(name string) error {
	unlock, err := wizardRegistryLock()
	if err != nil {
		return err
	}
	defer unlock()

	reg := loadWizardRegistry()

	var kept []localWizard
	for _, w := range reg.Wizards {
		if w.Name != name {
			kept = append(kept, w)
		}
	}
	reg.Wizards = kept

	saveWizardRegistry(reg)
	return nil
}

// wizardRegistryUpdate updates an entry by name using the provided function (file-locked).
func wizardRegistryUpdate(name string, update func(*localWizard)) error {
	unlock, err := wizardRegistryLock()
	if err != nil {
		return err
	}
	defer unlock()

	reg := loadWizardRegistry()

	for i := range reg.Wizards {
		if reg.Wizards[i].Name == name {
			update(&reg.Wizards[i])
			saveWizardRegistry(reg)
			return nil
		}
	}
	return fmt.Errorf("wizard %q not found in registry", name)
}

// findLiveWizardForBead returns the first registry entry for the given bead, or nil.
// The caller is expected to have already cleaned dead wizards from the registry.
func findLiveWizardForBead(reg wizardRegistry, beadID string) *localWizard {
	for i := range reg.Wizards {
		if reg.Wizards[i].BeadID == beadID {
			return &reg.Wizards[i]
		}
	}
	return nil
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

		statePath := filepath.Join(runtimeDir, agentName, "state.json")
		data, err := os.ReadFile(statePath)
		if err != nil {
			continue
		}
		var state executorState
		if err := json.Unmarshal(data, &state); err != nil {
			continue
		}

		if state.BeadID == "" || seen[state.BeadID] {
			continue
		}
		seen[state.BeadID] = true

		bead, err := storeGetBead(state.BeadID)
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

