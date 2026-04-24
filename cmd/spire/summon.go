package main

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/awell-health/spire/pkg/summon"
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

	// Route pkg/summon's seams through the cmd/spire-level mock points so
	// existing summon_test.go tests that reassign the CLI vars still intercept
	// the calls. The closures dereference the cmd/spire vars at each call, so
	// test swaps take effect immediately.
	summon.SpawnFunc = func(b agent.Backend, cfg agent.SpawnConfig) (agent.Handle, error) {
		return summonSpawnFunc(b, cfg)
	}
}

// --- Registry function wrappers ---

func wizardRegistryPath() string                { return agent.RegistryPath() }
func loadWizardRegistry() wizardRegistry        { return agent.LoadRegistry() }
func saveWizardRegistry(reg wizardRegistry)     { agent.SaveRegistry(reg) }
func wizardRegistryAdd(entry localWizard) error { return agent.RegistryAdd(entry) }
func wizardRegistryRemove(name string) error    { return agent.RegistryRemove(name) }
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

	// Pre-flight: for explicit target bead IDs, verify each prefix is
	// bound to a local path. Unbound prefixes silently default to CWD
	// downstream, which causes wizards to commit to the wrong repo. Fail
	// fast here before any wizard is spawned. (spi-rpuzs6)
	if err := preflightResolveTargets(targetIDs); err != nil {
		return err
	}

	// Detect mode: k8s or local.
	if isK8sAvailableFunc() {
		return summonK8s(count)
	}
	return summonLocal(count, targetIDs, dispatch)
}

// preflightResolveTargets verifies that every target bead ID has a
// locally-bound prefix before any wizard is spawned. On any failure it
// returns a diagnostic error listing each unbound prefix with the
// `spire repo bind` one-liner. Beads with resolvable prefixes are
// allowed through. Called before both summonK8s and summonLocal —
// k8s-mode wizards also need local repo bindings when they clone back
// to the operator's host, so the check is not local-mode-only.
func preflightResolveTargets(targetIDs []string) error {
	if len(targetIDs) == 0 {
		return nil
	}
	var problems []string
	for _, id := range targetIDs {
		if _, _, _, err := wizardResolveRepoForSummon(id); err != nil {
			problems = append(problems, formatSummonBindError(id, err))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(problems, "\n"))
}

// wizardResolveRepoForSummon is a test seam for preflightResolveTargets
// so unit tests can inject an in-memory resolver without shelling out to
// dolt. Production callers always go through wizardResolveRepo.
//
// It is wrapped in a closure rather than assigned directly
// (`= wizardResolveRepo`) because wizardResolveRepo is itself a var
// whose value is set in wizard_bridge.go's init() — package-level var
// initializers run before init(), so a direct assignment would capture
// the zero-valued (nil) function. The closure captures the var
// identifier, dereferencing it at call time after init() has run.
var wizardResolveRepoForSummon = func(beadID string) (string, string, string, error) {
	return wizardResolveRepo(beadID)
}

// formatSummonBindError renders a diagnostic message when a summoned
// bead's prefix has no local binding. The message names the prefix, its
// remote URL (if known), and the two bind one-liners the bug report
// calls for.
func formatSummonBindError(beadID string, resolveErr error) string {
	prefix := ""
	if idx := strings.Index(beadID, "-"); idx > 0 {
		prefix = beadID[:idx]
	}
	remote := lookupRemoteForPrefix(prefix)
	remoteLine := ""
	if remote != "" {
		remoteLine = fmt.Sprintf("  %s → %s (unbound)\n", prefix, remote)
	} else {
		remoteLine = fmt.Sprintf("  %s (unbound)\n", prefix)
	}
	return fmt.Sprintf(
		"spire summon %s: prefix %q has no local binding.\n%s\nBind it with:\n  spire repo bind %s /path/to/local/checkout\n\nOr clone + bind in one step:\n  spire repo clone %s\n\n(underlying error: %v)",
		beadID, prefix, remoteLine, prefix, prefix, resolveErr,
	)
}

// lookupRemoteForPrefix returns the shared repo URL for a prefix, or ""
// if it can't be determined (e.g. dolt unreachable, prefix not in the
// repos table). Used only for error messaging.
func lookupRemoteForPrefix(prefix string) string {
	if prefix == "" {
		return ""
	}
	cfg, err := loadConfig()
	if err != nil {
		return ""
	}
	database, ambiguous := resolveDatabase(cfg)
	if database == "" || ambiguous {
		return ""
	}
	sql := fmt.Sprintf("SELECT repo_url FROM `%s`.repos WHERE prefix = '%s'", database, sqlEscape(prefix))
	out, err := rawDoltQuery(sql)
	if err != nil {
		return ""
	}
	rows := parseDoltRows(out, []string{"repo_url"})
	if len(rows) == 0 {
		return ""
	}
	return rows[0]["repo_url"]
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

	if isK8sAvailableFunc() {
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
		if err := createWizardGuildCR(name); err != nil {
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
		if err := deleteWizardGuildCR(name); err != nil {
			log.Printf("failed to dismiss %s: %v", name, err)
			continue
		}
		fmt.Printf("  %s%s%s dismissed from the tower\n", dim, name, reset)
	}

	fmt.Printf("\n%d wizard(s) dismissed.\n", count)
	return nil
}

// isK8sAvailableFunc is the indirection used by cmdSummon / cmdDismiss so
// tests can stub k8s detection without shelling out. Production callers
// leave this alone; tests assign a func that returns a fixed bool.
var isK8sAvailableFunc = isK8sAvailable

// summonUpdateBeadFunc is a test-replaceable wrapper around storeUpdateBead
// used by summonLocal for status transitions.
var summonUpdateBeadFunc = storeUpdateBead

// summonSpawnFunc is the seam around backend.Spawn so unit tests can exercise
// summonLocal without fork/exec'ing the test binary (which would inherit
// SPIRE_CONFIG_DIR and race with t.TempDir's RemoveAll).
var summonSpawnFunc = func(b AgentBackend, cfg SpawnConfig) (agent.Handle, error) {
	return b.Spawn(cfg)
}

// isK8sAvailable probes for a reachable cluster with the "spire" namespace.
// Bounded by a short timeout so that a hung or unreachable kubectl context
// can't block `spire summon` / `spire dismiss` or, via transitive callers,
// the unit test suite.
func isK8sAvailable() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "kubectl", "get", "ns", "spire", "--no-headers").Run() == nil
}

func countK8sWizards() int {
	return len(listK8sWizards())
}

func listK8sWizards() []string {
	cmd := exec.Command("kubectl", "get", "wizardguild", "-n", "spire",
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

func createWizardGuildCR(name string) error {
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

	// spec.cache is the canonical cluster-native substrate (spi-gvrfv):
	// the operator provisions a read-only guild-owned cache PVC and every
	// wizard pod mounts it via the cache-bootstrap init container. The
	// old repo-bootstrap origin-clone path is retired, so a guild without
	// spec.cache would never schedule.
	manifest := fmt.Sprintf(`apiVersion: spire.awell.io/v1alpha1
kind: WizardGuild
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
  cache:
    size: 10Gi
    accessMode: ReadOnlyMany
    refreshInterval: 5m
`, name, name, repoURL, repoBranch)

	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", err, string(out))
	}
	return nil
}

func deleteWizardGuildCR(name string) error {
	cmd := exec.Command("kubectl", "delete", "wizardguild", name, "-n", "spire")
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

	// Orphan-attempt reconciliation before the interpreter starts.
	// If a prior wizard was kill -9'd mid-attempt, its attempt bead stays
	// in_progress forever with no live process and no graph_state.json —
	// the next summon would stack a second attempt on top. Close the
	// orphan here so the wizard's next CreateAttemptBead call starts from
	// a clean slate. Seam 15 (wizard dead → wizard resumed) in spi-1dk71j.
	reconcileOrphanAttempts(targetIDs, reg)

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
				if err := summonUpdateBeadFunc(id, map[string]interface{}{"status": "in_progress"}); err != nil {
					return fmt.Errorf("transition hooked bead %s to in_progress: %w", id, err)
				}
				bead.Status = "in_progress"
			case "open", "ready":
				if err := summonUpdateBeadFunc(id, map[string]interface{}{"status": "in_progress"}); err != nil {
					return fmt.Errorf("transition %s bead %s to in_progress: %w", bead.Status, id, err)
				}
				bead.Status = "in_progress"
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

	spawned := 0
	for i := 0; i < count; i++ {
		bead := candidates[i]

		// Check for existing graph state so the CLI print can say "resuming"
		// vs "starting". This is display-only; the wizard itself decides
		// whether to resume from graph state.
		existingGraphState, _ := executor.LoadGraphState("wizard-"+bead.ID, configDir)

		res, err := summon.SpawnWizard(bead, dispatch)
		if errors.Is(err, summon.ErrAlreadyRunning) {
			fmt.Printf("  %s — skipping\n", err)
			continue
		}
		if err != nil {
			return err
		}

		if dispatch != "" {
			fmt.Printf("  %s → dispatch override: %s%s\n", bead.ID, dispatch, reset)
		}
		if existingGraphState != nil && existingGraphState.ActiveStep != "" {
			fmt.Printf("  %s%s%s → resuming %s from %s step\n", cyan, res.WizardName, reset, bead.ID, existingGraphState.ActiveStep)
		} else {
			fmt.Printf("  %s%s%s → starting %s (%s)\n", cyan, res.WizardName, reset, bead.ID, bead.Title)
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

// orphanReconcilerSeams holds the test-replaceable function pointers used by
// reconcileOrphanAttempts. Production wiring uses the package-level store
// and executor helpers; tests assign their own implementations to exercise
// the reconciler without a live dolt server or runtime directory.
type orphanReconcilerSeams struct {
	GetChildren       func(parentID string) ([]Bead, error)
	ListBeads         func(filter beads.IssueFilter) ([]Bead, error)
	LoadGraphState    func(agentName string) (*executor.GraphState, error)
	AddLabel          func(beadID, label string) error
	AddComment        func(beadID, text string) error
	CloseBead         func(beadID string) error
	ProcessAliveCheck func(pid int) bool
}

// defaultOrphanReconcilerSeams returns the production seam wiring.
func defaultOrphanReconcilerSeams() orphanReconcilerSeams {
	return orphanReconcilerSeams{
		GetChildren: storeGetChildren,
		ListBeads:   storeListBeads,
		LoadGraphState: func(agentName string) (*executor.GraphState, error) {
			return executor.LoadGraphState(agentName, configDir)
		},
		AddLabel:          storeAddLabel,
		AddComment:        storeAddComment,
		CloseBead:         storeCloseBead,
		ProcessAliveCheck: processAlive,
	}
}

// reconcileOrphanAttempts scans for in_progress/open attempt beads whose
// parent wizard is neither alive (no process in the registry) nor has
// graph state on disk, and closes them as interrupted:orphan. Runs before
// the interpreter starts so the next summon creates a fresh attempt bead
// rather than stacking a second one on top of the ghost attempt.
//
// This is seam 15 from spi-1dk71j ("wizard dead → wizard resumed"): when
// a wizard is kill -9'd mid-attempt with no reset ever running, the
// attempt stays in_progress forever because neither the wizard (dead) nor
// the reset path (not invoked) cleans it up. Next summon would create a
// second attempt, and the first would leak.
//
// Idempotency: the scan rechecks state each time. Calling it twice in a
// row closes nothing new — the second run finds the orphans already
// closed (status=closed short-circuits). Zero-target scans also no-op.
//
// Strict additivity: we only close an attempt when BOTH (a) the registry
// shows no live wizard process AND (b) the wizard has no graph_state.json
// on disk. Either signal alone is treated as "wizard may still be alive"
// and the attempt is left alone. False positives here cause real data
// loss (a running wizard's attempt bead closed underneath it).
//
// targetIDs (optional) scopes the reconciliation to specific parent beads;
// empty means all in_progress attempt beads in the tower.
func reconcileOrphanAttempts(targetIDs []string, liveReg wizardRegistry) {
	reconcileOrphanAttemptsWithSeams(targetIDs, liveReg, defaultOrphanReconcilerSeams())
}

// reconcileOrphanAttemptsWithSeams is the test-seamed form. See
// reconcileOrphanAttempts for the full contract.
func reconcileOrphanAttemptsWithSeams(targetIDs []string, liveReg wizardRegistry, seams orphanReconcilerSeams) {
	// Build set of live agent names from the registry.
	liveAgents := make(map[string]bool)
	for _, w := range liveReg.Wizards {
		if w.PID > 0 && seams.ProcessAliveCheck(w.PID) {
			liveAgents[w.Name] = true
		}
	}

	var attemptCandidates []Bead
	if len(targetIDs) > 0 {
		// Scoped scan: only look at children of the named parents.
		for _, parentID := range targetIDs {
			children, err := seams.GetChildren(parentID)
			if err != nil {
				continue
			}
			for _, c := range children {
				if isAttemptBead(c) && (c.Status == "in_progress" || c.Status == "open") {
					attemptCandidates = append(attemptCandidates, c)
				}
			}
		}
	} else {
		// Tower-wide scan: every in_progress attempt bead.
		items, err := seams.ListBeads(beads.IssueFilter{})
		if err != nil {
			return
		}
		for _, b := range items {
			if !isAttemptBead(b) {
				continue
			}
			if b.Status != "in_progress" && b.Status != "open" {
				continue
			}
			attemptCandidates = append(attemptCandidates, b)
		}
	}

	for _, att := range attemptCandidates {
		// Resolve the wizard name from the attempt's agent:<name> label.
		agentName := hasLabel(att, "agent:")
		if agentName == "" {
			// Legacy attempt beads may lack the label; fall back to the
			// parent-derived name "wizard-<parent>".
			if att.Parent != "" {
				agentName = "wizard-" + att.Parent
			}
		}
		if agentName == "" {
			continue // cannot identify the wizard — leave alone
		}

		// Gate A: registry shows a live wizard process → skip.
		if liveAgents[agentName] {
			continue
		}

		// Gate B: graph_state.json on disk → skip (wizard may be about
		// to resume; reaper will clean up if the process is truly dead).
		existingGraphState, _ := seams.LoadGraphState(agentName)
		if existingGraphState != nil {
			continue
		}

		// Both gates cleared — this attempt is an orphan. Close it.
		if err := seams.AddLabel(att.ID, "interrupted:orphan"); err != nil {
			fmt.Printf("  %s(note: could not label orphan attempt %s: %s)%s\n", dim, att.ID, err, reset)
		}
		if err := seams.AddComment(att.ID, "Closed by summon orphan reconciler: wizard has no live process and no graph state on disk"); err != nil {
			// Best-effort — do not abort on comment failure.
			_ = err
		}
		if err := seams.CloseBead(att.ID); err != nil {
			fmt.Printf("  %s(note: could not close orphan attempt %s: %s)%s\n", dim, att.ID, err, reset)
			continue
		}
		fmt.Printf("  %s✗ closed orphan attempt %s (wizard %s: dead + no state)%s\n", dim, att.ID, agentName, reset)
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
