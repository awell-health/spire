package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/awell-health/spire/pkg/workshop"
)

func cmdWorkshop(args []string) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	if len(args) == 0 {
		return workshop.Interactive()
	}

	switch args[0] {
	case "list":
		return cmdWorkshopList(args[1:])
	case "show", "describe":
		return cmdWorkshopShow(args[1:])
	case "validate":
		return cmdWorkshopValidate(args[1:])
	case "compose":
		return cmdWorkshopCompose(args[1:])
	default:
		return fmt.Errorf("workshop: unknown subcommand %q (try: list, show, validate, compose)", args[0])
	}
}

func cmdWorkshopList(args []string) error {
	var filterSource string
	var jsonOutput bool

	// Simple flag parsing
	var remaining []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--custom":
			filterSource = "custom"
		case "--embedded":
			filterSource = "embedded"
		case "--all":
			filterSource = ""
		case "--json":
			jsonOutput = true
		default:
			remaining = append(remaining, args[i])
		}
	}
	_ = remaining

	formulas, err := workshop.ListFormulas()
	if err != nil {
		return err
	}

	// Filter by source if requested
	if filterSource != "" {
		var filtered []workshop.FormulaInfo
		for _, f := range formulas {
			if f.Source == filterSource {
				filtered = append(filtered, f)
			}
		}
		formulas = filtered
	}

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(formulas)
	}

	// Table output
	if len(formulas) == 0 {
		fmt.Println("No formulas found.")
		return nil
	}

	nameW := 4
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
	return nil
}

func cmdWorkshopShow(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: spire workshop show <formula-name>")
	}
	output, err := workshop.Show(args[0])
	if err != nil {
		return err
	}
	fmt.Print(output)
	return nil
}

func cmdWorkshopValidate(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: spire workshop validate <formula-name>")
	}
	issues, err := workshop.Validate(args[0])
	if err != nil {
		return err
	}

	if len(issues) == 0 {
		fmt.Printf("%s: no issues found\n", args[0])
		return nil
	}

	hasErrors := false
	for _, iss := range issues {
		prefix := "ERROR"
		if iss.Level == "warning" {
			prefix = "WARN "
		} else {
			hasErrors = true
		}
		if iss.Phase != "" {
			fmt.Printf("  %s [%s] %s\n", prefix, iss.Phase, iss.Message)
		} else {
			fmt.Printf("  %s %s\n", prefix, iss.Message)
		}
	}

	if hasErrors {
		os.Exit(1)
	}
	return nil
}
