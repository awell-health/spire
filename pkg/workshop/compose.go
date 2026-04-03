package workshop

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/awell-health/spire/pkg/formula"
)

// ComposeInteractive walks the user through building a v3 step-graph formula.
// Returns the built graph, TOML bytes, and any error.
func ComposeInteractive(name string, in io.Reader, out io.Writer) (*formula.FormulaStepGraph, []byte, error) {
	reader := bufio.NewReader(in)
	gb := NewGraphBuilder(name)

	fmt.Fprintf(out, "\nSpire Workshop — composing v3 formula %q\n\n", name)

	// Bead type — selects defaults and pre-population
	beadTypes := []string{"task", "bug", "feature", "epic", "chore", "design", "recovery"}
	btIdx, err := promptChoice(reader, out, "Target bead type", beadTypes, 0)
	if err != nil {
		return nil, nil, err
	}
	beadType := beadTypes[btIdx]

	// Description
	desc := promptString(reader, out, "Description", "")
	gb.SetDescription(desc)

	// Workspaces
	fmt.Fprintf(out, "\n=== Workspaces ===\n")
	fmt.Fprintf(out, "Add workspaces (empty name to finish):\n")
	for {
		wsName := promptString(reader, out, "  Workspace name", "")
		if wsName == "" {
			break
		}
		kinds := KnownWorkspaceKinds()
		kindIdx, err := promptChoice(reader, out, "  Kind", kinds, 0)
		if err != nil {
			return nil, nil, err
		}
		ws := formula.WorkspaceDecl{Kind: kinds[kindIdx]}
		if ws.Kind != formula.WorkspaceKindRepo {
			ws.Branch = promptString(reader, out, "  Branch template", "")
			ws.Base = promptString(reader, out, "  Base branch", "")
		}
		scopes := KnownWorkspaceScopes()
		scopeIdx, _ := promptChoice(reader, out, "  Scope", scopes, 1) // default "run"
		ws.Scope = scopes[scopeIdx]
		ownerships := KnownWorkspaceOwnerships()
		ownIdx, _ := promptChoice(reader, out, "  Ownership", ownerships, 0)
		ws.Ownership = ownerships[ownIdx]
		cleanups := KnownWorkspaceCleanups()
		cleanIdx, _ := promptChoice(reader, out, "  Cleanup", cleanups, 1) // default "terminal"
		ws.Cleanup = cleanups[cleanIdx]

		if err := gb.AddWorkspace(wsName, ws); err != nil {
			fmt.Fprintf(out, "  Error: %v\n", err)
			continue
		}
	}

	// Variables
	fmt.Fprintf(out, "\n=== Variables ===\n")
	fmt.Fprintf(out, "Add variables (empty name to finish):\n")
	for {
		varName := promptString(reader, out, "  Variable name", "")
		if varName == "" {
			break
		}
		varTypes := KnownVarTypes()
		typeIdx, _ := promptChoice(reader, out, "  Type", varTypes, 1) // default "string"
		varDesc := promptString(reader, out, "  Description", "")
		varRequired := promptBool(reader, out, "  Required", true)
		varDefault := promptString(reader, out, "  Default value", "")
		gb.AddVar(varName, formula.FormulaVar{
			Type:        varTypes[typeIdx],
			Description: varDesc,
			Required:    varRequired,
			Default:     varDefault,
		})
	}

	// Pre-populate recovery steps when composing for recovery bead type
	if beadType == "recovery" {
		fmt.Fprintf(out, "\n=== Pre-populated recovery steps ===\n")
		for _, s := range RecoveryStepDefaults() {
			cfg := formula.StepConfig{
				Kind:   "op",
				Action: s.Flow,
				Title:  s.Title,
			}
			if s.Flow != "bead.finish" {
				cfg.Action = "wizard.run"
				cfg.Flow = s.Flow
			} else {
				cfg.Action = "bead.finish"
			}
			if err := gb.AddStep(s.Name, cfg); err != nil {
				fmt.Fprintf(out, "  Warning: %v\n", err)
			} else {
				fmt.Fprintf(out, "  + %s (%s)\n", s.Name, s.Title)
			}
		}
	}

	// Steps
	fmt.Fprintf(out, "\n=== Steps ===\n")
	fmt.Fprintf(out, "Add steps (empty name to finish):\n")
	for {
		stepName := promptString(reader, out, "  Step name", "")
		if stepName == "" {
			break
		}
		cfg := formula.StepConfig{}

		kinds := KnownStepKinds()
		kindIdx, _ := promptChoice(reader, out, "  Kind", kinds, 0)
		cfg.Kind = kinds[kindIdx]

		actions := append([]string{"(none)"}, KnownActions()...)
		actIdx, _ := promptChoice(reader, out, "  Action", actions, 0)
		if actIdx > 0 {
			cfg.Action = actions[actIdx]
		}

		if cfg.Action == formula.OpcodeWizardRun {
			cfg.Flow = promptString(reader, out, "  Flow", "")
		}
		if cfg.Kind == formula.StepKindCall {
			cfg.Graph = promptString(reader, out, "  Graph name", "")
		}

		cfg.Workspace = promptString(reader, out, "  Workspace ref", "")

		needsStr := promptString(reader, out, "  Needs (comma-separated)", "")
		if needsStr != "" {
			for _, n := range strings.Split(needsStr, ",") {
				n = strings.TrimSpace(n)
				if n != "" {
					cfg.Needs = append(cfg.Needs, n)
				}
			}
		}

		producesStr := promptString(reader, out, "  Produces (comma-separated)", "")
		if producesStr != "" {
			for _, p := range strings.Split(producesStr, ",") {
				p = strings.TrimSpace(p)
				if p != "" {
					cfg.Produces = append(cfg.Produces, p)
				}
			}
		}

		cfg.Terminal = promptBool(reader, out, "  Terminal", false)

		if err := gb.AddStep(stepName, cfg); err != nil {
			fmt.Fprintf(out, "  Error: %v\n", err)
			continue
		}
	}

	// Build and review
	tomlBytes, err := gb.MarshalTOML()
	if err != nil {
		return nil, nil, fmt.Errorf("marshal formula: %w", err)
	}

	g, err := gb.Build()
	if err != nil {
		return nil, nil, err
	}

	for {
		fmt.Fprintf(out, "\n--- Generated TOML ---\n%s\n", tomlBytes)
		choice, err := promptChoice(reader, out, "Action", []string{"save", "validate", "quit"}, 0)
		if err != nil {
			return nil, nil, err
		}

		switch choice {
		case 0: // save
			path, err := saveDraft(name, tomlBytes)
			if err != nil {
				return nil, nil, fmt.Errorf("save draft: %w", err)
			}
			fmt.Fprintf(out, "Formula saved to %s\n", path)
			return g, tomlBytes, nil

		case 1: // validate
			issues, err := validateBuiltFormula(tomlBytes)
			if err != nil {
				fmt.Fprintf(out, "Validation error: %v\n", err)
				continue
			}
			if len(issues) == 0 {
				fmt.Fprintf(out, "No issues found.\n")
			} else {
				for _, iss := range issues {
					prefix := "ERROR"
					if iss.Level == "warning" {
						prefix = "WARN "
					}
					if iss.Phase != "" {
						fmt.Fprintf(out, "  %s [%s] %s\n", prefix, iss.Phase, iss.Message)
					} else {
						fmt.Fprintf(out, "  %s %s\n", prefix, iss.Message)
					}
				}
			}

		case 2: // quit
			return g, tomlBytes, nil
		}
	}
}

// validateBuiltFormula validates TOML bytes as a v3 formula.
func validateBuiltFormula(data []byte) ([]Issue, error) {
	return validateV3(data), nil
}

// --- Prompt helpers ---

// promptString shows a prompt with optional default, returns the user's input.
func promptString(r *bufio.Reader, w io.Writer, prompt, defaultVal string) string {
	if defaultVal != "" {
		fmt.Fprintf(w, "%s [%s]: ", prompt, defaultVal)
	} else {
		fmt.Fprintf(w, "%s: ", prompt)
	}
	line, _ := r.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return defaultVal
	}
	return line
}

// promptChoice shows a numbered menu and returns the selected index.
func promptChoice(r *bufio.Reader, w io.Writer, prompt string, options []string, defaultIdx int) (int, error) {
	fmt.Fprintf(w, "%s:\n", prompt)
	for i, opt := range options {
		marker := "  "
		if i == defaultIdx {
			marker = "> "
		}
		fmt.Fprintf(w, "  %s%d) %s\n", marker, i+1, opt)
	}
	fmt.Fprintf(w, "Choice [%d]: ", defaultIdx+1)

	line, _ := r.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return defaultIdx, nil
	}
	n, err := strconv.Atoi(line)
	if err != nil || n < 1 || n > len(options) {
		return defaultIdx, nil
	}
	return n - 1, nil
}

// promptBool shows a yes/no prompt with a default.
func promptBool(r *bufio.Reader, w io.Writer, prompt string, defaultVal bool) bool {
	defStr := "y/N"
	if defaultVal {
		defStr = "Y/n"
	}
	fmt.Fprintf(w, "%s [%s]: ", prompt, defStr)
	line, _ := r.ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	if line == "" {
		return defaultVal
	}
	return line == "y" || line == "yes"
}

// promptMultiSelect shows checkboxes for toggling items by number. Enter confirms.
func promptMultiSelect(r *bufio.Reader, w io.Writer, prompt string, options []string, selected []bool) []bool {
	result := make([]bool, len(selected))
	copy(result, selected)

	for {
		fmt.Fprintf(w, "%s (toggle by number, Enter to confirm):\n", prompt)
		for i, opt := range options {
			check := "[ ]"
			if result[i] {
				check = "[x]"
			}
			fmt.Fprintf(w, "  %s %d) %s\n", check, i+1, opt)
		}
		fmt.Fprintf(w, "Toggle: ")

		line, _ := r.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			return result
		}
		n, err := strconv.Atoi(line)
		if err != nil || n < 1 || n > len(options) {
			continue
		}
		result[n-1] = !result[n-1]
	}
}

// draftDir returns the directory for saving draft formulas.
func draftDir() string {
	dir := os.Getenv("XDG_DATA_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dir, "spire", "formulas")
}

// saveDraft writes a formula TOML to the draft directory.
func saveDraft(name string, tomlBytes []byte) (string, error) {
	dir := draftDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("create draft dir: %w", err)
	}
	path := filepath.Join(dir, name+".formula.toml")
	if err := os.WriteFile(path, tomlBytes, 0644); err != nil {
		return "", err
	}
	return path, nil
}
