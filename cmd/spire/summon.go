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
)

// wizardRegistry tracks locally summoned wizards.
type wizardRegistry struct {
	Wizards []localWizard `json:"wizards"`
}

type localWizard struct {
	Name      string `json:"name"`
	PID       int    `json:"pid"`
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
		var ready []Bead
		if err := bdJSON(&ready, "ready"); err == nil {
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
	reg := loadWizardRegistry()

	// Clean up dead wizards first.
	reg = cleanDeadWizards(reg)

	existing := len(reg.Wizards)

	for i := 0; i < count; i++ {
		name := fmt.Sprintf("wizard-%d", existing+i+1)
		worktree := filepath.Join(os.TempDir(), "spire-wizard", name)

		// Create worktree directory.
		if err := os.MkdirAll(worktree, 0755); err != nil {
			return fmt.Errorf("failed to create worktree for %s: %w", name, err)
		}

		// Spawn the wizard as a background process.
		// The wizard runs: spire claim → spire focus → claude --print → push
		// For now, we just register them. The actual process spawning
		// will be implemented when we have the local wizard loop.
		reg.Wizards = append(reg.Wizards, localWizard{
			Name:      name,
			PID:       0, // placeholder until local wizard loop is implemented
			Worktree:  worktree,
			StartedAt: time.Now().UTC().Format(time.RFC3339),
		})

		fmt.Printf("  %s%s%s summoned (local mode)\n", cyan, name, reset)
	}

	saveWizardRegistry(reg)
	fmt.Printf("\n%d wizard(s) summoned locally. Run %sspire roster%s to check status.\n", count, bold, reset)
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
			if proc, err := os.FindProcess(w.PID); err == nil {
				// Check if process is still running.
				if err := proc.Signal(os.Signal(nil)); err != nil {
					continue // dead
				}
			}
		}
		alive = append(alive, w)
	}
	reg.Wizards = alive
	return reg
}
