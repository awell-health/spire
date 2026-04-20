package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/awell-health/spire/pkg/store"
	"github.com/spf13/cobra"
)

// --- Test-replaceable seams (inline function vars so cmd/spire tests can
// swap them without touching cmd/spire/store_bridge.go) ---

var clericGetBeadFunc = storeGetBead
var clericSetBeadMetadataMapFunc = store.SetBeadMetadataMap
var clericNowFunc = func() time.Time { return time.Now().UTC() }
var clericGetwdFunc = os.Getwd

// beadIDRe matches a bead directory basename, e.g. "spi-abc123" or
// "web-9xx.1". Mirrors the prefix-dash-hex scheme used in CLAUDE.md and
// pkg/git/commitmsg.go.
var beadIDRe = regexp.MustCompile(`^[a-z]+-[a-z0-9]+(?:\.\d+)*$`)

var clericCmd = &cobra.Command{
	Use:   "cleric",
	Short: "Cleric lifecycle commands (failure recovery)",
	Long: `Cleric lifecycle commands.

A cleric is the role that drives the failure-recovery state machine — it
diagnoses a failed bead, executes recovery actions, and learns from the
outcome. The three subcommands map ~1:1 to the cleric formula actions:

  diagnose  → cleric.decide    (record next-action decision)
  execute   → cleric.execute   (run a named recovery action)
  learn     → cleric.learn     (record learning, finish)`,
}

var clericDiagnoseCmd = &cobra.Command{
	Use:   "diagnose <bead>",
	Short: "Record the cleric's diagnosis on a bead",
	Long: `Record the cleric's next-action decision on a bead.

Writes the decision to the bead's metadata under cleric_state=diagnosed,
along with an optional --decision text. Corresponds to formula action
cleric.decide.`,
	Args: cobra.ExactArgs(1),
	RunE: runClericDiagnose,
}

var clericExecuteCmd = &cobra.Command{
	Use:   "execute",
	Short: "Run a named recovery action against a bead",
	Long: `Run a named cleric recovery action.

The --action flag names the recovery step. The --bead flag identifies the
target; if absent, the bead ID is resolved from the SPIRE_BEAD environment
variable, or by walking the current working directory for a basename that
matches the bead-ID pattern. Corresponds to formula action cleric.execute.`,
	RunE: runClericExecute,
}

var clericLearnCmd = &cobra.Command{
	Use:   "learn <bead>",
	Short: "Record learning and transition cleric to finished",
	Long: `Record the cleric's learning notes on a bead and mark the cleric state
finished. Use --notes to attach learning text. Corresponds to formula
action cleric.learn.`,
	Args: cobra.ExactArgs(1),
	RunE: runClericLearn,
}

func init() {
	clericDiagnoseCmd.Flags().String("decision", "", "Next-action decision text")

	clericExecuteCmd.Flags().String("action", "", "Recovery action to run (required)")
	clericExecuteCmd.Flags().String("bead", "", "Bead ID (overrides cwd/SPIRE_BEAD auto-detect)")
	_ = clericExecuteCmd.MarkFlagRequired("action")

	clericLearnCmd.Flags().String("notes", "", "Learning notes text")

	clericCmd.AddCommand(clericDiagnoseCmd, clericExecuteCmd, clericLearnCmd)
	rootCmd.AddCommand(clericCmd)
}

func runClericDiagnose(cmd *cobra.Command, args []string) error {
	decision, _ := cmd.Flags().GetString("decision")
	return cmdClericDiagnose(args[0], decision)
}

func runClericExecute(cmd *cobra.Command, args []string) error {
	action, _ := cmd.Flags().GetString("action")
	beadOverride, _ := cmd.Flags().GetString("bead")
	return cmdClericExecute(action, beadOverride)
}

func runClericLearn(cmd *cobra.Command, args []string) error {
	notes, _ := cmd.Flags().GetString("notes")
	return cmdClericLearn(args[0], notes)
}

// cmdClericDiagnose writes the cleric's next-action decision to bead metadata.
func cmdClericDiagnose(beadID, decision string) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}
	if _, err := clericGetBeadFunc(beadID); err != nil {
		return fmt.Errorf("get bead %s: %w", beadID, err)
	}

	now := clericNowFunc().Format(time.RFC3339)
	meta := map[string]string{
		"cleric_state":        "diagnosed",
		"cleric_diagnosed_at": now,
	}
	if decision != "" {
		meta["cleric_decision"] = decision
	}
	if err := clericSetBeadMetadataMapFunc(beadID, meta); err != nil {
		return fmt.Errorf("write diagnosis metadata to %s: %w", beadID, err)
	}

	fmt.Printf("diagnosed %s (state=diagnosed)\n", beadID)
	return nil
}

// cmdClericExecute runs the named recovery action and updates cleric state.
func cmdClericExecute(action, beadOverride string) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}
	if action == "" {
		return fmt.Errorf("--action is required")
	}

	beadID := resolveClericBeadID(beadOverride)
	if beadID == "" {
		return fmt.Errorf("no bead ID resolved: pass --bead, set SPIRE_BEAD, or run from a bead-named worktree")
	}

	if _, err := clericGetBeadFunc(beadID); err != nil {
		return fmt.Errorf("get bead %s: %w", beadID, err)
	}

	now := clericNowFunc().Format(time.RFC3339)
	meta := map[string]string{
		"cleric_state":       "executed",
		"cleric_action":      action,
		"cleric_executed_at": now,
	}
	if err := clericSetBeadMetadataMapFunc(beadID, meta); err != nil {
		return fmt.Errorf("write execution metadata to %s: %w", beadID, err)
	}

	fmt.Printf("executed %s on %s (state=executed)\n", action, beadID)
	return nil
}

// cmdClericLearn records learning notes and transitions cleric state to finished.
func cmdClericLearn(beadID, notes string) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}
	if _, err := clericGetBeadFunc(beadID); err != nil {
		return fmt.Errorf("get bead %s: %w", beadID, err)
	}

	now := clericNowFunc().Format(time.RFC3339)
	meta := map[string]string{
		"cleric_state":      "finished",
		"cleric_learned_at": now,
	}
	if notes != "" {
		meta["cleric_learning"] = notes
	}
	if err := clericSetBeadMetadataMapFunc(beadID, meta); err != nil {
		return fmt.Errorf("write learning metadata to %s: %w", beadID, err)
	}

	fmt.Printf("learned on %s (state=finished)\n", beadID)
	return nil
}

// resolveClericBeadID resolves the target bead ID for `cleric execute`.
// Priority: explicit --bead flag → SPIRE_BEAD env → cwd walk.
func resolveClericBeadID(flag string) string {
	if flag != "" {
		return flag
	}
	if env := os.Getenv("SPIRE_BEAD"); env != "" {
		return env
	}
	return detectBeadIDFromCwd()
}

// detectBeadIDFromCwd walks up from the current working directory looking
// for a directory basename that matches the bead-ID pattern. Returns "" if
// no ancestor matches.
func detectBeadIDFromCwd() string {
	cwd, err := clericGetwdFunc()
	if err != nil || cwd == "" {
		return ""
	}
	dir := cwd
	for {
		if beadIDRe.MatchString(filepath.Base(dir)) {
			return filepath.Base(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
