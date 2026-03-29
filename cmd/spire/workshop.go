package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/workshop"
)

// cmdWorkshop dispatches workshop subcommands.
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
	case "dry-run":
		return cmdWorkshopDryRun(args[1:])
	case "test":
		return cmdWorkshopTest(args[1:])
	case "publish":
		return cmdWorkshopPublish(args[1:])
	case "unpublish":
		return cmdWorkshopUnpublish(args[1:])
	case "help", "--help", "-h":
		printWorkshopUsage()
		return nil
	default:
		return fmt.Errorf("workshop: unknown subcommand %q\n\nRun 'spire workshop help' for usage", args[0])
	}
}

func printWorkshopUsage() {
	fmt.Println(`Usage: spire workshop <subcommand> [args]

Subcommands:
  list [--custom|--embedded|--all] [--json]   List available formulas
  show <name>                                  Show formula details
  validate <name>                              Validate a formula
  compose                                      Interactive formula builder
  dry-run <name> [--json] [--bead <id>]       Simulate formula execution (no side effects)
  test <name> --bead <id>                      Dry-run with full bead context
  publish <name>                               Copy formula to tower's .beads/formulas/
  unpublish <name>                             Remove published formula`)
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

// cmdWorkshopDryRun handles: spire workshop dry-run <name> [--json] [--bead <id>]
func cmdWorkshopDryRun(args []string) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	var name, beadID string
	var jsonOutput bool

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonOutput = true
		case "--bead":
			if i+1 >= len(args) {
				return fmt.Errorf("--bead requires a value")
			}
			i++
			beadID = args[i]
		default:
			if name == "" && !strings.HasPrefix(args[i], "-") {
				name = args[i]
			} else {
				return fmt.Errorf("unexpected argument: %s", args[i])
			}
		}
	}

	if name == "" {
		return fmt.Errorf("usage: spire workshop dry-run <name> [--json] [--bead <id>]")
	}

	f, err := formula.LoadFormulaByName(name)
	if err != nil {
		return fmt.Errorf("load formula %q: %w", name, err)
	}

	var loadBead func(string) (workshop.BeadInfo, error)
	if beadID != "" {
		loadBead = func(id string) (workshop.BeadInfo, error) {
			b, err := storeGetBead(id)
			if err != nil {
				return workshop.BeadInfo{}, err
			}
			return workshop.BeadInfo{
				ID:     b.ID,
				Type:   b.Type,
				Labels: b.Labels,
				Title:  b.Title,
			}, nil
		}
	}

	result, err := workshop.DryRun(f, beadID, loadBead)
	if err != nil {
		return fmt.Errorf("dry-run: %w", err)
	}

	if jsonOutput {
		data, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal result: %w", err)
		}
		fmt.Println(string(data))
		return nil
	}

	printDryRunResult(result)
	return nil
}

// cmdWorkshopTest handles: spire workshop test <name> --bead <id>
func cmdWorkshopTest(args []string) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	var name, beadID string
	var jsonOutput bool

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonOutput = true
		case "--bead":
			if i+1 >= len(args) {
				return fmt.Errorf("--bead requires a value")
			}
			i++
			beadID = args[i]
		default:
			if name == "" && !strings.HasPrefix(args[i], "-") {
				name = args[i]
			} else {
				return fmt.Errorf("unexpected argument: %s", args[i])
			}
		}
	}

	if name == "" || beadID == "" {
		return fmt.Errorf("usage: spire workshop test <name> --bead <id>")
	}

	f, err := formula.LoadFormulaByName(name)
	if err != nil {
		return fmt.Errorf("load formula %q: %w", name, err)
	}

	// Load actual bead
	bead, err := storeGetBead(beadID)
	if err != nil {
		return fmt.Errorf("load bead %s: %w", beadID, err)
	}

	// Check formula resolution: what formula would this bead normally use?
	resolvedName := resolveFormulaName(bead)
	if resolvedName != name {
		fmt.Fprintf(os.Stderr, "Note: bead %s would normally use formula %q (you specified %q)\n\n", beadID, resolvedName, name)
	}

	loadBead := func(id string) (workshop.BeadInfo, error) {
		b, err := storeGetBead(id)
		if err != nil {
			return workshop.BeadInfo{}, err
		}
		return workshop.BeadInfo{
			ID:     b.ID,
			Type:   b.Type,
			Labels: b.Labels,
			Title:  b.Title,
		}, nil
	}

	result, err := workshop.DryRun(f, beadID, loadBead)
	if err != nil {
		return fmt.Errorf("dry-run: %w", err)
	}

	// Also simulate review step graph if review phase is enabled
	var stepResult *workshop.StepGraphSimulation
	if f.PhaseEnabled("review") {
		if g, err := formula.LoadReviewPhaseFormula(); err == nil {
			stepResult, _ = workshop.DryRunStepGraph(g)
		}
	}

	if jsonOutput {
		out := struct {
			DryRun    *workshop.DryRunResult       `json:"dry_run"`
			ReviewDAG *workshop.StepGraphSimulation `json:"review_dag,omitempty"`
		}{
			DryRun:    result,
			ReviewDAG: stepResult,
		}
		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal result: %w", err)
		}
		fmt.Println(string(data))
		return nil
	}

	fmt.Printf("Test: %s against bead %s (%s, type=%s)\n\n", name, beadID, bead.Title, bead.Type)
	printDryRunResult(result)

	if stepResult != nil {
		fmt.Println()
		printStepGraphResult(stepResult)
	}

	return nil
}

// cmdWorkshopPublish handles: spire workshop publish <name>
func cmdWorkshopPublish(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: spire workshop publish <name>")
	}
	name := args[0]

	beadsDir := resolveBeadsDir()
	if beadsDir == "" {
		return fmt.Errorf("no tower configured — cannot publish (run 'spire tower create' first)")
	}
	os.Setenv("BEADS_DIR", beadsDir)

	dest, err := workshop.Publish(name, beadsDir)
	if err != nil {
		return fmt.Errorf("publish: %w", err)
	}

	fmt.Printf("Published %s to %s\n", name, dest)
	fmt.Println("Beads will now use this formula via layered resolution (disk overrides embedded defaults).")
	return nil
}

// cmdWorkshopUnpublish handles: spire workshop unpublish <name>
func cmdWorkshopUnpublish(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: spire workshop unpublish <name>")
	}
	name := args[0]

	beadsDir := resolveBeadsDir()
	if beadsDir == "" {
		return fmt.Errorf("no tower configured — cannot unpublish")
	}
	os.Setenv("BEADS_DIR", beadsDir)

	if err := workshop.Unpublish(name, beadsDir); err != nil {
		return fmt.Errorf("unpublish: %w", err)
	}

	fmt.Printf("Unpublished %s — beads will fall back to embedded default\n", name)
	return nil
}

// printDryRunResult renders a human-readable dry-run report.
func printDryRunResult(r *workshop.DryRunResult) {
	fmt.Printf("Formula: %s (v%d)\n", r.Formula, r.Version)
	fmt.Printf("Phases: %s\n\n", strings.Join(r.EnabledPhases, " → "))

	for _, p := range r.Phases {
		fmt.Printf("[%s]\n", p.Name)

		var details []string
		details = append(details, "Role: "+p.Role)
		if p.Model != "" {
			details = append(details, "Model: "+p.Model)
		}
		if p.Timeout != "" {
			details = append(details, "Timeout: "+p.Timeout)
		}
		fmt.Printf("  %s\n", strings.Join(details, " | "))

		var extras []string
		if p.Dispatch != "" && p.Dispatch != "direct" {
			extras = append(extras, "Dispatch: "+p.Dispatch)
		}
		if p.Worktree {
			extras = append(extras, "Worktree: yes")
		}
		if p.MaxBuildFixRounds > 0 {
			extras = append(extras, fmt.Sprintf("Build-fix rounds: %d", p.MaxBuildFixRounds))
		}
		if p.StagingBranch != "" {
			extras = append(extras, "Staging: "+p.StagingBranch)
		}
		if p.Auto {
			extras = append(extras, "Auto: yes")
		}
		if p.Strategy != "" && p.Strategy != "squash" {
			extras = append(extras, "Strategy: "+p.Strategy)
		}
		if len(extras) > 0 {
			fmt.Printf("  %s\n", strings.Join(extras, " | "))
		}

		fmt.Printf("  → %s\n\n", p.Description)
	}

	if len(r.Errors) > 0 {
		fmt.Println("Errors:")
		for _, e := range r.Errors {
			fmt.Printf("  - %s\n", e)
		}
	}
}

// printStepGraphResult renders a human-readable step graph simulation.
func printStepGraphResult(r *workshop.StepGraphSimulation) {
	fmt.Printf("Review DAG: %s (v%d)\n", r.Formula, r.Version)
	fmt.Printf("Entry: %s\n\n", r.Entry)

	fmt.Println("Steps:")
	for _, s := range r.Steps {
		terminal := ""
		if s.Terminal {
			terminal = " [terminal]"
		}
		fmt.Printf("  %s (%s)%s\n", s.Name, s.Role, terminal)
		if s.Title != "" {
			fmt.Printf("    Title: %s\n", s.Title)
		}
		if len(s.Needs) > 0 {
			fmt.Printf("    Needs: %s\n", strings.Join(s.Needs, ", "))
		}
		if s.Condition != "" {
			fmt.Printf("    When: %s\n", s.Condition)
		}
	}

	if len(r.Paths) > 0 {
		fmt.Printf("\nExecution paths (%d):\n", len(r.Paths))
		for i, path := range r.Paths {
			fmt.Printf("  %d. %s\n", i+1, strings.Join(path, " → "))
		}
	}

	if len(r.Errors) > 0 {
		fmt.Println("\nErrors:")
		for _, e := range r.Errors {
			fmt.Printf("  - %s\n", e)
		}
	}
}
