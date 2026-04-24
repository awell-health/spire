package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/board"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/wizard"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var boardCmd = &cobra.Command{
	Use:   "board",
	Short: "Interactive board TUI (--mine, --ready, --json)",
	RunE: func(cmd *cobra.Command, args []string) error {
		var fullArgs []string
		if mine, _ := cmd.Flags().GetBool("mine"); mine {
			fullArgs = append(fullArgs, "--mine")
		}
		if ready, _ := cmd.Flags().GetBool("ready"); ready {
			fullArgs = append(fullArgs, "--ready")
		}
		if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
			fullArgs = append(fullArgs, "--json")
		}
		if v, _ := cmd.Flags().GetString("interval"); v != "" {
			fullArgs = append(fullArgs, "--interval", v)
		}
		if v, _ := cmd.Flags().GetString("epic"); v != "" {
			fullArgs = append(fullArgs, "--epic", v)
		}
		return cmdBoard(fullArgs)
	},
}

func init() {
	boardCmd.Flags().Bool("mine", false, "Show only my beads")
	boardCmd.Flags().Bool("ready", false, "Show only ready beads")
	boardCmd.Flags().Bool("json", false, "Output as JSON")
	boardCmd.Flags().String("interval", "", "Refresh interval (e.g. 5s)")
	boardCmd.Flags().String("epic", "", "Filter by epic bead ID")
}

func cmdBoard(args []string) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}
	if err := requireDolt(); err != nil {
		return err
	}

	var (
		flagJSON bool
		opts     board.Opts
	)
	opts.Interval = 5 * time.Second

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--mine":
			opts.Mine = true
		case "--ready":
			opts.Ready = true
		case "--json":
			flagJSON = true
		case "--interval":
			if i+1 >= len(args) {
				return fmt.Errorf("--interval requires a value")
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil {
				return fmt.Errorf("--interval: invalid duration %q", args[i])
			}
			opts.Interval = d
		case "--epic":
			if i+1 >= len(args) {
				return fmt.Errorf("--epic requires a bead ID")
			}
			i++
			opts.Epic = args[i]
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire board [--mine] [--ready] [--epic <id>] [--json] [--interval 5s]", args[i])
		}
	}

	identity, _ := config.DetectIdentity("")

	// Resolve current tower name for header display.
	if tower, err := config.ResolveTowerConfig(); err == nil && tower != nil {
		opts.TowerName = tower.Name
	}

	// Inject tower list function for the T-key switcher (used by BoardMode internally).
	opts.ListTowersFn = func() []board.TowerItem {
		towers, err := config.ListTowerConfigs()
		if err != nil {
			return nil
		}
		items := make([]board.TowerItem, len(towers))
		for i, t := range towers {
			items[i] = board.TowerItem{
				Name:     t.Name,
				Database: t.Database,
				Active:   t.Name == opts.TowerName,
			}
		}
		return items
	}

	// Resolve available towers with beads dirs for the RootModel tower switcher.
	if towers, err := config.ListTowerConfigs(); err == nil {
		dd := doltDataDir()
		for _, t := range towers {
			item := board.TowerItem{
				Name:     t.Name,
				Database: t.Database,
				Active:   t.Name == opts.TowerName,
			}
			if dd != "" && t.Database != "" {
				item.BeadsDir = filepath.Join(dd, t.Database, ".beads")
			}
			opts.TowerItems = append(opts.TowerItems, item)
		}
	}

	if flagJSON {
		result, err := board.FetchBoard(opts, identity)
		if err != nil {
			return err
		}
		// Pre-fetch recovery refs for interrupted beads.
		recoveryRefs := make(map[string]*board.RecoveryRef)
		getDeps := board.StoreDeps()
		for _, b := range result.Columns.Hooked {
			if ref := board.FetchRecoveryRef(b.ID, getDeps); ref != nil {
				recoveryRefs[b.ID] = ref
			}
		}
		out := board.BoardJSON{
			ColumnsJSON: result.Columns.ToJSON(recoveryRefs),
			Warnings:    result.Warnings,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return fmt.Errorf("spire board now launches the interactive TUI by default; use `spire board --json` for non-interactive output")
	}

	fetchAgents := func() []board.LocalAgent {
		reg := agent.LoadRegistry()
		// Filter out dead entries (PID<=0 or process no longer alive) before
		// surfacing to the board. OrphanSweep (run by BeginWork/steward daemon)
		// handles bead-level cleanup; we only prune the display list here.
		var live []agent.Entry
		for _, w := range reg.Wizards {
			if w.PID > 0 && processAlive(w.PID) {
				live = append(live, w)
			}
		}
		return live
	}

	actionFn := func(action board.PendingAction, beadID string) bool {
		return executeBoardAction(action, beadID)
	}

	inlineActionFn := func(action board.PendingAction, beadID string) error {
		return executeInlineAction(action, beadID)
	}

	opts.RootCmd = rootCmd

	rejectDesignFn := func(beadID, feedback string) error {
		return storeAddComment(beadID, "Design rejected: "+feedback)
	}

	opts.ResolveFn = func(beadID, comment string) error {
		return resolveSourceBead(beadID, comment, false)
	}

	opts.TermContentFn = func(beadID string) (string, error) {
		return renderTraceForBoard(beadID)
	}

	return board.RunBoard(opts, identity, fetchAgents, actionFn, inlineActionFn, rejectDesignFn)
}

// executeBoardAction runs the pending action on the raw terminal after the TUI exits.
// Returns true if the TUI should be relaunched afterward.
func executeBoardAction(action board.PendingAction, beadID string) bool {
	switch action {
	case board.ActionFocus:
		fmt.Println()
		if err := cmdFocus([]string{beadID}); err != nil {
			fmt.Fprintf(os.Stderr, "focus: %v\n", err)
		}
		fmt.Printf("\n%sPress Enter to return to board...%s ", board.Dim, board.Reset)
		fmt.Scanln()
		return true

	case board.ActionSummon:
		fmt.Println()
		if err := summonLocal(1, []string{beadID}, "", wizard.SelectFlags{}); err != nil {
			fmt.Fprintf(os.Stderr, "summon: %v\n", err)
		}
		fmt.Printf("\n%sPress Enter to return to board...%s ", board.Dim, board.Reset)
		fmt.Scanln()
		return true

	case board.ActionClaim:
		fmt.Println()
		if err := cmdClaim([]string{beadID}); err != nil {
			fmt.Fprintf(os.Stderr, "claim: %v\n", err)
		}
		fmt.Printf("\n%sPress Enter to return to board...%s ", board.Dim, board.Reset)
		fmt.Scanln()
		return true

	case board.ActionLogs:
		wizardName := "wizard-" + beadID
		fmt.Println()
		if err := cmdLogs([]string{wizardName}); err != nil {
			fmt.Fprintf(os.Stderr, "logs: %v\n", err)
		}
		fmt.Printf("\n%sPress Enter to return to board...%s ", board.Dim, board.Reset)
		fmt.Scanln()
		return true

	case board.ActionResummon:
		fmt.Println()
		if err := cmdResummon([]string{beadID}); err != nil {
			fmt.Fprintf(os.Stderr, "resummon: %v\n", err)
		}
		fmt.Printf("\n%sPress Enter to return to board...%s ", board.Dim, board.Reset)
		fmt.Scanln()
		return true

	case board.ActionClose:
		fmt.Printf("\nClose bead %s? [y/N] ", beadID)
		var answer string
		fmt.Scanln(&answer)
		if answer == "y" || answer == "Y" {
			if err := storeCloseBead(beadID); err != nil {
				fmt.Fprintf(os.Stderr, "close: %v\n", err)
			} else {
				fmt.Printf("Closed %s\n", beadID)
			}
		}
		return true

	case board.ActionGrok:
		fmt.Println()
		if err := cmdGrok([]string{beadID}); err != nil {
			fmt.Fprintf(os.Stderr, "grok: %v\n", err)
		}
		fmt.Printf("\n%sPress Enter to return to board...%s ", board.Dim, board.Reset)
		fmt.Scanln()
		return true

	case board.ActionTrace:
		fmt.Println()
		output, err := renderTraceForBoard(beadID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "trace: %v\n", err)
		} else {
			fmt.Print(output)
		}
		fmt.Printf("\n%sPress Enter to return to board...%s ", board.Dim, board.Reset)
		fmt.Scanln()
		return true
	}
	return false
}

// executeInlineAction runs an action within the TUI via tea.Cmd (no exit-relaunch).
// Captures stdout/stderr to prevent garbling the TUI's alt-screen.
// Returns nil on success, error on failure.
func executeInlineAction(action board.PendingAction, beadID string) error {
	// Redirect stdout/stderr to discard during inline execution.
	// The TUI owns the terminal — command output would garble the alt-screen.
	oldStdout, oldStderr := os.Stdout, os.Stderr
	devNull, _ := os.Open(os.DevNull)
	if devNull != nil {
		os.Stdout = devNull
		os.Stderr = devNull
		defer func() {
			os.Stdout = oldStdout
			os.Stderr = oldStderr
			devNull.Close()
		}()
	}
	switch action {
	case board.ActionSummon:
		return summonLocal(1, []string{beadID}, "", wizard.SelectFlags{})
	case board.ActionResummon:
		return cmdResummon([]string{beadID})
	case board.ActionUnsummon:
		return cmdDismiss([]string{"1", "--targets", beadID})
	case board.ActionResetSoft:
		return cmdReset([]string{beadID})
	case board.ActionResetHard:
		return cmdReset([]string{beadID, "--hard"})
	case board.ActionGrok:
		return cmdGrok([]string{beadID})
	case board.ActionClose:
		return storeCloseBead(beadID)
	case board.ActionApprove:
		// Approve a needs-human design bead: remove the label and close it.
		_ = storeRemoveLabel(beadID, "needs-human")
		return storeCloseBead(beadID)
	case board.ActionApproveGate:
		// Approve a human.approve gate: remove labels, add comment, reset hooked steps.
		return cmdApprove(beadID, "")
	case board.ActionApproveDesign:
		// Approve a design bead: close it (signals acceptance).
		return storeCloseBead(beadID)
	case board.ActionDefer:
		// Toggle deferred status: open/ready → deferred, deferred → open.
		b, err := storeGetBead(beadID)
		if err != nil {
			return fmt.Errorf("defer: get bead: %w", err)
		}
		var newStatus string
		switch b.Status {
		case "open", "ready":
			newStatus = "deferred"
		case "deferred":
			newStatus = "open"
		default:
			return fmt.Errorf("defer: cannot toggle bead in status %q", b.Status)
		}
		return storeUpdateBead(beadID, map[string]interface{}{"status": newStatus})
	case board.ActionReady:
		return storeUpdateBead(beadID, map[string]interface{}{"status": "ready"})
	case board.ActionResume:
		return cmdApprove(beadID, "")
	}
	return fmt.Errorf("unknown inline action: %d", action)
}

