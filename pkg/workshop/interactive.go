package workshop

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Interactive runs a simple REPL for formula exploration.
// Commands: list, show <name>, validate <name>, help, exit/quit/q.
func Interactive() error {
	fmt.Println("Spire Workshop — formula explorer")
	fmt.Println("Type 'help' for available commands, 'exit' to quit.")
	fmt.Println()

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("workshop> ")
		if !scanner.Scan() {
			// EOF
			fmt.Println()
			return nil
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		parts := strings.Fields(line)
		cmd := parts[0]
		cmdArgs := parts[1:]

		switch cmd {
		case "list", "ls":
			formulas, err := ListFormulas()
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				continue
			}
			printFormulaTable(formulas)

		case "show", "describe":
			if len(cmdArgs) == 0 {
				fmt.Fprintln(os.Stderr, "usage: show <formula-name>")
				continue
			}
			output, err := Show(cmdArgs[0])
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				continue
			}
			fmt.Print(output)

		case "validate":
			if len(cmdArgs) == 0 {
				fmt.Fprintln(os.Stderr, "usage: validate <formula-name>")
				continue
			}
			issues, err := Validate(cmdArgs[0])
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				continue
			}
			printIssues(issues)

		case "help", "?":
			fmt.Println("Commands:")
			fmt.Println("  list                List all available formulas")
			fmt.Println("  show <name>         Display formula with phase diagram")
			fmt.Println("  validate <name>     Validate formula syntax and logic")
			fmt.Println("  help                Show this help")
			fmt.Println("  exit                Quit workshop")

		case "exit", "quit", "q":
			return nil

		default:
			fmt.Fprintf(os.Stderr, "unknown command: %s (try 'help')\n", cmd)
		}
	}
}

// printFormulaTable prints a simple table of formula info.
func printFormulaTable(formulas []FormulaInfo) {
	if len(formulas) == 0 {
		fmt.Println("No formulas found.")
		return
	}

	// Calculate column widths
	nameW := 4 // "NAME"
	for _, f := range formulas {
		if len(f.Name) > nameW {
			nameW = len(f.Name)
		}
	}

	fmt.Printf("%-*s  VER  SOURCE    PHASES\n", nameW, "NAME")
	fmt.Printf("%-*s  ---  ------    ------\n", nameW, strings.Repeat("-", nameW))
	for _, f := range formulas {
		phases := strings.Join(f.Phases, ", ")
		fmt.Printf("%-*s  v%d   %-8s  %s\n", nameW, f.Name, f.Version, f.Source, phases)
	}
}

// printIssues prints validation issues to stdout.
func printIssues(issues []Issue) {
	if len(issues) == 0 {
		fmt.Println("No issues found.")
		return
	}
	for _, iss := range issues {
		prefix := "ERROR"
		if iss.Level == "warning" {
			prefix = "WARN "
		}
		if iss.Phase != "" {
			fmt.Printf("  %s [%s] %s\n", prefix, iss.Phase, iss.Message)
		} else {
			fmt.Printf("  %s %s\n", prefix, iss.Message)
		}
	}
}
