package main

import (
	"fmt"
	"os"
)

// executeBoardAction runs the pending action on the raw terminal after the TUI exits.
// Returns true if the TUI should be relaunched afterward.
func executeBoardAction(action boardPendingAction, beadID string) bool {
	switch action {
	case boardActionFocus:
		fmt.Println()
		if err := cmdFocus([]string{beadID}); err != nil {
			fmt.Fprintf(os.Stderr, "focus: %v\n", err)
		}
		fmt.Printf("\n%sPress Enter to return to board...%s ", dim, reset)
		fmt.Scanln()
		return true

	case boardActionSummon:
		fmt.Println()
		if err := summonLocal(1, []string{beadID}); err != nil {
			fmt.Fprintf(os.Stderr, "summon: %v\n", err)
		}
		fmt.Printf("\n%sPress Enter to return to board...%s ", dim, reset)
		fmt.Scanln()
		return true

	case boardActionClaim:
		fmt.Println()
		if err := cmdClaim([]string{beadID}); err != nil {
			fmt.Fprintf(os.Stderr, "claim: %v\n", err)
		}
		fmt.Printf("\n%sPress Enter to return to board...%s ", dim, reset)
		fmt.Scanln()
		return true

	case boardActionLogs:
		wizardName := "wizard-" + beadID
		fmt.Println()
		if err := cmdLogs([]string{wizardName}); err != nil {
			fmt.Fprintf(os.Stderr, "logs: %v\n", err)
		}
		return true
	}
	return false
}
