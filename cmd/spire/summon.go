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
	Name      string `json:"name"`
	PID       int    `json:"pid"`
	BeadID    string `json:"bead_id"`
	Worktree  string `json:"worktree"`
	StartedAt string `json:"started_at"`
}

func cmdSummon(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: spire summon <N> [--for <epic-id>]")
	}

	var count int
	var forEpic string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--for":
			if i+1 >= len(args) {
				return fmt.Errorf("--for requires an epic bead ID")
			}
			i++
			forEpic = args[i]
		default:
			n, err := strconv.Atoi(args[i])
			if err != nil {
				return fmt.Errorf("expected a number, got %q\nusage: spire summon <N> [--for <epic-id>]", args[i])
			}
			count = n
		}
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
	return summonLocal(count)
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

func summonLocal(count int) error {
	// Find ready beads to assign.
	ready, err := storeGetReadyWork(beads.WorkFilter{})
	if err != nil {
		return fmt.Errorf("query ready work: %w", err)
	}

	// Filter to actionable beads (tasks/bugs, not epics).
	var candidates []Bead
	for _, b := range ready {
		if b.Type == "epic" {
			continue
		}
		candidates = append(candidates, b)
	}

	if len(candidates) == 0 {
		fmt.Println("No ready beads to work on.")
		return nil
	}
	if count > len(candidates) {
		fmt.Printf("Only %d ready bead(s) available (requested %d).\n", len(candidates), count)
		count = len(candidates)
	}

	reg := loadWizardRegistry()
	reg = cleanDeadWizards(reg)
	existing := len(reg.Wizards)

	spireBin, err := os.Executable()
	if err != nil {
		spireBin = os.Args[0]
	}

	logDir := filepath.Join(doltGlobalDir(), "wizards")
	os.MkdirAll(logDir, 0755)

	for i := 0; i < count; i++ {
		bead := candidates[i]
		name := fmt.Sprintf("wizard-%d", existing+i+1)

		// Spawn: spire wizard-run <bead-id> --name <wizard-name>
		logPath := filepath.Join(logDir, name+".log")
		logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			return fmt.Errorf("open log for %s: %w", name, err)
		}

		cmd := exec.Command(spireBin, "wizard-run", bead.ID, "--name", name)
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		cmd.Env = os.Environ()

		if err := cmd.Start(); err != nil {
			logFile.Close()
			return fmt.Errorf("spawn %s: %w", name, err)
		}
		logFile.Close() // child owns the fd now

		worktree := filepath.Join(os.TempDir(), "spire-wizard", name, bead.ID)
		reg.Wizards = append(reg.Wizards, localWizard{
			Name:      name,
			PID:       cmd.Process.Pid,
			BeadID:    bead.ID,
			Worktree:  worktree,
			StartedAt: time.Now().UTC().Format(time.RFC3339),
		})

		fmt.Printf("  %s%s%s → %s (%s) [pid %d]\n", cyan, name, reset, bead.ID, bead.Title, cmd.Process.Pid)
	}

	saveWizardRegistry(reg)
	fmt.Printf("\n%d wizard(s) summoned. Logs: %s\n", count, logDir)
	fmt.Printf("Run %sspire roster%s to check status.\n", bold, reset)
	return nil
}

func dismissLocal(count int, all bool) error {
	reg := loadWizardRegistry()
	reg = cleanDeadWizards(reg)

	if all {
		count = len(reg.Wizards)
	}
	if count > len(reg.Wizards) {
		count = len(reg.Wizards)
	}
	if count == 0 {
		fmt.Println("No local wizards to dismiss.")
		return nil
	}

	// Dismiss from the end.
	for i := 0; i < count; i++ {
		idx := len(reg.Wizards) - 1 - i
		w := reg.Wizards[idx]
		if w.PID > 0 {
			// Kill the process.
			if proc, err := os.FindProcess(w.PID); err == nil {
				proc.Signal(os.Interrupt)
			}
		}
		fmt.Printf("  %s%s%s dismissed\n", dim, w.Name, reset)
	}

	reg.Wizards = reg.Wizards[:len(reg.Wizards)-count]
	saveWizardRegistry(reg)
	fmt.Printf("\n%d wizard(s) dismissed.\n", count)
	return nil
}

func wizardRegistryPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "spire", "wizards.json")
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
		if w.PID > 0 {
			if !processAlive(w.PID) {
				continue // dead
			}
		}
		alive = append(alive, w)
	}
	reg.Wizards = alive
	return reg
}

