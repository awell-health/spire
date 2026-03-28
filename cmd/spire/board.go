package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/board"
	"github.com/awell-health/spire/pkg/config"
	"golang.org/x/term"
)

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

	fetchBoard := func(o board.Opts) (board.Columns, error) {
		return board.FetchBoard(o, identity)
	}

	fetchAgents := func() []board.LocalAgent {
		reg := agent.LoadRegistry()
		reg = cleanDeadWizards(reg)
		return reg.Wizards
	}

	actionFn := func(action board.PendingAction, beadID string) bool {
		return executeBoardAction(action, beadID)
	}

	return board.RunBoardTUI(opts, fetchBoard, fetchAgents, actionFn)
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
		return true
	}
	return false
}

