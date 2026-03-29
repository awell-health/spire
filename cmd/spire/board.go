package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/board"
	"github.com/awell-health/spire/pkg/config"
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

	if flagJSON {
		cols, err := board.FetchBoard(opts, identity)
		if err != nil {
			return err
		}
		out := cols.ToJSON()
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}

	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return fmt.Errorf("spire board now launches the interactive TUI by default; use `spire board --json` for non-interactive output")
	}

	fetchAgents := func() []board.LocalAgent {
		reg := agent.LoadRegistry()
		reg = cleanDeadWizards(reg)
		return reg.Wizards
	}

	actionFn := func(action board.PendingAction, beadID string) bool {
		return executeBoardAction(action, beadID)
	}

	inlineActionFn := func(action board.PendingAction, beadID string) error {
		return executeInlineAction(action, beadID)
	}

	opts.RootCmd = rootCmd

	return board.RunBoardTUI(opts, identity, fetchAgents, actionFn, inlineActionFn)
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
		if err := summonLocal(1, []string{beadID}); err != nil {
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
	}
	return false
}

// executeInlineAction runs an action within the TUI via tea.Cmd (no exit-relaunch).
// Returns nil on success, error on failure.
func executeInlineAction(action board.PendingAction, beadID string) error {
	switch action {
	case board.ActionSummon:
		return summonLocal(1, []string{beadID})
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
	case board.ActionTrace:
		return cmdTrace([]string{beadID})
	case board.ActionAdvance:
		return cmdAdvance([]string{beadID})
	case board.ActionClose:
		return storeCloseBead(beadID)
	}
	return fmt.Errorf("unknown inline action: %d", action)
}

