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
	"github.com/awell-health/spire/pkg/beadlifecycle"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/executor"
	spgit "github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/awell-health/spire/pkg/summon"
	"github.com/awell-health/spire/pkg/wizard"
	"github.com/spf13/cobra"
	"github.com/steveyegge/beads"
)

// --- Type aliases for backward compatibility ---

type wizardRegistry = agent.Registry
type localWizard = agent.Entry

var summonCmd = &cobra.Command{
	Use:   "summon <bead-id>... | <N> [flags]",
	Short: "Summon wizards (--targets <ids>, --auto, --auth, --turbo, -H)",
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
		if authSlot, _ := cmd.Flags().GetString("auth"); authSlot != "" {
			fullArgs = append(fullArgs, "--auth", authSlot)
		}
		if turbo, _ := cmd.Flags().GetBool("turbo"); turbo {
			fullArgs = append(fullArgs, "--turbo")
		}
		if headers, _ := cmd.Flags().GetStringArray("header"); len(headers) > 0 {
			for _, h := range headers {
				fullArgs = append(fullArgs, "-H", h)
			}
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
	summonCmd.Flags().String("auth", "", "Auth slot to use for this run: subscription or api-key")
	summonCmd.Flags().Bool("turbo", false, "Alias for --auth=api-key")
	summonCmd.Flags().StringArrayP("header", "H", nil, "Ephemeral Anthropic header: x-anthropic-api-key or x-anthropic-token (repeatable)")

	dismissCmd.Flags().Bool("all", false, "Dismiss all wizards")
	dismissCmd.Flags().String("targets", "", "Comma-separated bead IDs to dismiss")
	dismissCmd.Flags().String("target", "", "Alias for --targets")

	// Route pkg/summon's seams through the cmd/spire-level mock points so
	// existing summon_test.go tests that reassign the CLI vars still intercept
	// the calls. The closures dereference the cmd/spire vars at each call, so
	// test swaps take effect immediately.
	summon.SpawnFunc = func(b agent.Backend, cfg agent.SpawnConfig) (agent.Handle, error) {
		// Stamp the bead's selected AuthContext onto cfg so the backend
		// injects the right Anthropic env var. Lookup-by-bead-ID stays
		// consistent across local + cluster spawn paths.
		if cfg.BeadID != "" && len(cfg.AuthEnv) == 0 {
			env, slot := authEnvForBead(cfg.BeadID)
			cfg.AuthEnv = env
			cfg.AuthSlot = slot
		}
		return summonSpawnFunc(b, cfg)
	}

	// Wire pkg/summon's Registry seam to the local-mode wizardregistry
	// adapter (spi-p6unf3). The CLI runs only on the laptop, so local is
	// the right impl here; cluster summons go through the operator's
	// wizardregistry/cluster path, not this code.
	summon.Registry = newLocalRegistryAdapter()
}

// --- Registry function wrappers ---

func wizardRegistryPath() string                { return agent.RegistryPath() }
func loadWizardRegistry() wizardRegistry        { return agent.LoadRegistry() }
func saveWizardRegistry(reg wizardRegistry)     { agent.SaveRegistry(reg) }
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
		return fmt.Errorf("usage: spire summon <bead-id>... | <N> [--targets <ids>] [--auto] [--dispatch <mode>] [--auth <slot>] [--turbo] [-H name:value]")
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
	var authFlags wizard.SelectFlags
	var rawHeaders []string

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--for":
			return fmt.Errorf("--for has been removed; use --targets <bead-id> for exact targeting")
		case args[i] == "--targets":
			if i+1 >= len(args) {
				return fmt.Errorf("--targets requires comma-separated bead IDs")
			}
			i++
			targets = args[i]
		case args[i] == "--dispatch":
			if i+1 >= len(args) {
				return fmt.Errorf("--dispatch requires a mode: sequential, wave, or direct")
			}
			i++
			dispatch = args[i]
		case args[i] == "--auto":
			auto = true
		case args[i] == "--auth":
			if i+1 >= len(args) {
				return fmt.Errorf("--auth requires a slot: subscription or api-key")
			}
			i++
			authFlags.AuthSlot = args[i]
		case args[i] == "--turbo":
			authFlags.Turbo = true
		case args[i] == "-H", args[i] == "--header":
			if i+1 >= len(args) {
				return fmt.Errorf("%s requires a header value (e.g. 'x-anthropic-api-key: sk-ant-…')", args[i])
			}
			i++
			rawHeaders = append(rawHeaders, args[i])
		default:
			if strings.Contains(args[i], "-") {
				// Looks like a bead ID (e.g. spi-xxx)
				targetIDs = append(targetIDs, args[i])
			} else {
				n, err := strconv.Atoi(args[i])
				if err != nil {
					return fmt.Errorf("expected a bead ID or number, got %q\nusage: spire summon <bead-id>... | <N> [--targets <ids>] [--auto] [--dispatch <mode>] [--auth <slot>] [--turbo] [-H name:value]", args[i])
				}
				count = n
				hasExplicitCount = true
			}
		}
	}

	// Parse -H headers separately so ValidateFlags runs once against the
	// fully-populated SelectFlags and the caller gets one consolidated
	// error rather than one-per-header. ParseSummonHeaders also rejects
	// any unsupported header name with a clear error.
	parsed, err := wizard.ParseSummonHeaders(rawHeaders)
	if err != nil {
		return err
	}
	authFlags.HeaderAPIKey = parsed.HeaderAPIKey
	authFlags.HeaderToken = parsed.HeaderToken
	if err := wizard.ValidateFlags(authFlags); err != nil {
		return err
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

	// Dispatch on the active tower's deployment mode rather than on
	// kubectl reachability. The active tower is the source of truth for
	// where work runs; probing kubectl can pick up an unrelated cluster
	// (a developer's minikube, a stale dev cluster) and silently subvert
	// the user's tower choice. See spi-jsxa3v.
	tower, err := activeTowerConfigFunc()
	if err != nil {
		return fmt.Errorf("summon: resolve active tower: %w", err)
	}
	switch tower.EffectiveDeploymentMode() {
	case config.DeploymentModeClusterNative:
		if !isK8sAvailableFunc() {
			return fmt.Errorf("summon: tower %q is cluster-native but kubectl cannot reach the spire namespace", tower.Name)
		}
		// Targeted summon (positional bead IDs or --targets) is local-only:
		// summonK8s creates generic WizardGuild capacity (wizard-N) and
		// silently discards target IDs. Reject here so operators don't
		// believe a specific bead has been claimed when only generic
		// cluster capacity was created. Use `spire ready` to enqueue a
		// specific bead for the cluster steward/operator. (spi-v1hcrs)
		if len(targetIDs) > 0 {
			return fmt.Errorf(
				"spire summon <bead-id> is not supported in cluster mode: "+
					"summon creates generic wizard capacity (wizard-N), it "+
					"does not bind to a specific bead.\n\n"+
					"To enqueue a specific bead for cluster dispatch, use:\n"+
					"  spire ready %s\n\n"+
					"To create generic cluster capacity, use a count:\n"+
					"  spire summon <N>",
				strings.Join(targetIDs, " "),
			)
		}
		return summonK8sFunc(count)
	case config.DeploymentModeLocalNative:
		return summonLocal(count, targetIDs, dispatch, authFlags)
	case config.DeploymentModeAttachedReserved:
		return fmt.Errorf("summon: attached-reserved mode is not yet supported")
	case config.DeploymentModeUnknown:
		return fmt.Errorf(
			"summon: tower %q has no DeploymentMode set; configure it in "+
				"~/.config/spire/towers/%s.json (deployment_mode = "+
				"\"local-native\" | \"cluster-native\") and retry",
			tower.Name, tower.Name)
	default:
		return fmt.Errorf("summon: unknown deployment mode %q on tower %q",
			tower.EffectiveDeploymentMode(), tower.Name)
	}
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

	tower, err := activeTowerConfigFunc()
	if err != nil {
		return fmt.Errorf("dismiss: resolve active tower: %w", err)
	}
	// Gateway-mode dispatch (bead-level dismiss): when the active tower is
	// gateway-mode AND the caller named explicit targets, route to
	// POST /api/v1/beads/{id}/dismiss for each target. The gateway endpoint
	// has the broader "cancel the bead's work entirely" semantics — it
	// closes the bead and cleans the worktree+branch — which is the right
	// behaviour for an attached cluster tower where the local CLI has no
	// process to kill anyway. Bare counts (no targets) stay on the
	// wizard-pool path; they have no mapping to the bead-level endpoint.
	if tower != nil && tower.IsGateway() && len(targetIDs) > 0 {
		var lastErr error
		for _, id := range targetIDs {
			if err := dismissBeadViaGatewayFunc(context.Background(), id); err != nil {
				fmt.Fprintf(os.Stderr, "dismiss %s: %v\n", id, err)
				lastErr = err
			}
		}
		return lastErr
	}
	switch tower.EffectiveDeploymentMode() {
	case config.DeploymentModeClusterNative:
		if !isK8sAvailableFunc() {
			return fmt.Errorf("dismiss: tower %q is cluster-native but kubectl cannot reach the spire namespace", tower.Name)
		}
		return dismissK8s(count, dismissAll)
	case config.DeploymentModeLocalNative:
		return dismissLocal(count, dismissAll, targetIDs)
	case config.DeploymentModeAttachedReserved:
		return fmt.Errorf("dismiss: attached-reserved mode is not yet supported")
	case config.DeploymentModeUnknown:
		// spi-od41sr: bare-count dismiss against an in-memory tower with no
		// DeploymentMode used to fall through to dismissLocal and SIGINT
		// every wizard in the local registry. Reject Unknown explicitly so
		// future test fixtures (and any tower JSON missing the field) hit
		// a clear error instead of the silent local-native side effect.
		return fmt.Errorf(
			"dismiss: tower %q has no DeploymentMode set; configure it in "+
				"~/.config/spire/towers/%s.json (deployment_mode = "+
				"\"local-native\" | \"cluster-native\") and retry",
			tower.Name, tower.Name)
	default:
		return fmt.Errorf("dismiss: unknown deployment mode %q on tower %q",
			tower.EffectiveDeploymentMode(), tower.Name)
	}
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

// summonK8sFunc is the indirection around summonK8s used by cmdSummon so
// tests can verify (a) that targeted summon never reaches it in cluster
// mode and (b) that bare counts still do, without needing a real
// kubectl/cluster. Production callers leave this alone.
var summonK8sFunc = summonK8s

// isK8sAvailableFunc is the indirection used by cmdSummon / cmdDismiss so
// tests can stub k8s detection without shelling out. Production callers
// leave this alone; tests assign a func that returns a fixed bool.
//
// As of spi-jsxa3v this is no longer the primary dispatch signal — the
// active tower's deployment mode is. It survives as a defensive
// reachability check on the cluster-native branch (so a cluster-native
// tower with no reachable cluster fails loudly instead of silently
// taking the local path).
var isK8sAvailableFunc = isK8sAvailable

// activeTowerConfigFunc is the indirection used by cmdSummon / cmdDismiss
// so tests can drive dispatch through a fake tower config without
// touching the real config dir. Production callers leave this alone.
var activeTowerConfigFunc = activeTowerConfig

// summonUpdateBeadFunc is a test-replaceable wrapper around storeUpdateBead
// used by summonLocal for status transitions.
var summonUpdateBeadFunc = storeUpdateBead

// summonBeginWorkFunc is a test-replaceable wrapper around beadlifecycle.BeginWork
// used by summonLocal to set up per-bead work state.
var summonBeginWorkFunc = beadlifecycle.BeginWork

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

// summonTarget pairs a candidate bead with the auth context resolved for it
// during the pre-flight pass. It exists so the three summon passes
// (pre-flight → BeginWork → SpawnWizard) can share state without
// re-resolving and so SelectAuth is invoked exactly once per bead per
// summon call.
type summonTarget struct {
	bead    Bead
	authCtx *config.AuthContext
}

// summonLocal drives the local-mode summon pipeline as three passes:
//
//  1. Resolve and validate every candidate, then call SelectAuth. This is
//     a pure read pass — no state mutation. If auth selection fails for
//     any candidate, we abort here, BEFORE any attempt bead is created or
//     any source bead transitions to in_progress. Fixes spi-c13c4w: the
//     prior shape ran BeginWork (which creates the attempt + flips state
//     + runs OrphanSweep) before SelectAuth, leaving an orphan attempt
//     behind on auth-failure paths.
//  2. For each target, run BeginWork (targeted path only — creates the
//     attempt bead and transitions the source bead). The no-targets path
//     leaves attempt creation to ClaimWork in the spawned wizard.
//  3. For each target, attach the resolved auth to graph state and call
//     SpawnWizard.
func summonLocal(count int, targetIDs []string, dispatch string, authFlags wizard.SelectFlags) error {
	// Resolve the auth config once for the whole summon call — SelectAuth is
	// pure so the same config serves every candidate. Errors reading the
	// config file are fatal: if the operator has credentials configured but
	// we can't read them, silently falling back to a default would spawn a
	// wizard with the wrong credential. A missing config file is fine (the
	// returned AuthConfig has nil slots and AutoPromoteOn429=true); that
	// only fails SelectAuth when a flag/rule actually needs an unconfigured
	// slot, which is the error we want to surface.
	authCfg, err := selectAuthReadConfig()
	if err != nil {
		return fmt.Errorf("read auth config: %w", err)
	}

	// Load registry for live-agent deduplication in scanOrphanedBeads.
	// Dead-wizard cleanup is now handled by beadlifecycle.OrphanSweep
	// (called inside BeginWork), so we skip the old cleanDeadWizards pass.
	reg := loadWizardRegistry()

	// Phase 0: Gather candidate beads. For the targeted path this also
	// validates each bead's status. No state mutation here.
	var candidates []Bead
	if len(targetIDs) > 0 {
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
			case "hooked", "open", "ready", "in_progress", "":
				candidates = append(candidates, bead)
			}
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

	// Pass 1: pre-flight auth selection. Pure read — no state mutation.
	// SelectAuth is invoked exactly once per bead and the result is stashed
	// on summonTarget for pass 3. If selection fails, abort the whole summon
	// before any BeginWork has run so we don't leave orphan attempts behind.
	targets := make([]summonTarget, 0, count)
	for i := 0; i < count; i++ {
		bead := candidates[i]
		authCtx, authErr := wizard.SelectAuth(authCfg, bead.Priority, authFlags)
		if authErr != nil {
			return fmt.Errorf("auth selection for %s: %w", bead.ID, authErr)
		}
		targets = append(targets, summonTarget{bead: bead, authCtx: authCtx})
	}

	// Pass 2: BeginWork (targeted path only). Creates the attempt bead,
	// runs OrphanSweep, transitions the source bead to in_progress, and
	// upserts a registry placeholder. The wizard subprocess's `spire claim`
	// reclaims this attempt via ClaimWork's same-agent reclaim path.
	// The no-targets path skips this — ClaimWork in the spawned wizard
	// creates the attempt itself, matching prior behavior.
	if len(targetIDs) > 0 {
		towerName := ""
		if tc, err := activeTowerConfig(); err == nil && tc != nil {
			towerName = tc.Name
		} else if t := os.Getenv("SPIRE_TOWER"); t != "" {
			towerName = t
		}
		for i := range targets {
			id := targets[i].bead.ID
			wizardName := "wizard-" + id
			worktree := filepath.Join(os.TempDir(), "spire-wizard", wizardName, id)
			if _, berr := summonBeginWorkFunc(newLifecycleDeps(), newLocalRegistryAdapter(), id, beadlifecycle.BeginOpts{
				Mode:      beadlifecycle.ModeLocal,
				AgentName: wizardName,
				Worktree:  worktree,
				Tower:     towerName,
			}); berr != nil {
				return fmt.Errorf("begin work for %s: %w", id, berr)
			}
			targets[i].bead.Status = "in_progress"
		}
	}

	// Pass 3: spawn wizards using the auth context resolved in pass 1.
	logDir := filepath.Join(doltGlobalDir(), "wizards")

	spawned := 0
	for _, tgt := range targets {
		bead := tgt.bead
		authCtx := tgt.authCtx

		setBeadAuthContext(bead.ID, authCtx)
		fmt.Printf("  %s → auth: %s%s\n", bead.ID, authCtx.SlotName(), ephemeralSuffix(authCtx))

		// Check for existing graph state so the CLI print can say "resuming"
		// vs "starting". This is display-only; the wizard itself decides
		// whether to resume from graph state.
		existingGraphState, _ := executor.LoadGraphState("wizard-"+bead.ID, configDir)

		if err := attachAuthToRunState("wizard-"+bead.ID, authCtx, existingGraphState); err != nil {
			return fmt.Errorf("attach auth to %s run state: %w", bead.ID, err)
		}

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

// scanOrphanedBeads returns beads that have executor state but no live wizard process.

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

// selectAuthReadConfig is a seam around config.ReadAuthConfig so summon tests
// can inject a fixture without a real credentials file. Defaults to the
// real reader; tests that need a specific auth state reassign this before
// calling summonLocal.
var selectAuthReadConfig = config.ReadAuthConfig

// attachAuthToRunState writes the selected AuthContext onto the wizard's
// per-run graph state so the spawned wizard subprocess (which runs in a
// fresh process) reads the same credential the CLI selected. If a graph
// state file already exists (resumption), the Auth field is updated in
// place; otherwise a preliminary state is written with only BeadID,
// AgentName, and Auth populated — NewGraph's load path merges it into a
// fresh graph state the first time the wizard runs.
//
// existingState is what summonLocal already loaded for the resumption-
// detection branch; passing it here avoids a second disk read. Nil means
// no prior state, which is the fresh-spawn case.
var attachAuthToRunState = func(agentName string, auth *config.AuthContext, existingState *executor.GraphState) error {
	if auth == nil {
		return nil // nothing to attach
	}
	state := existingState
	if state == nil {
		state = &executor.GraphState{
			BeadID:    strings.TrimPrefix(agentName, "wizard-"),
			AgentName: agentName,
		}
	}
	state.Auth = auth
	return state.Save(agentName, configDir)
}
