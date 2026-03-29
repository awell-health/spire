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

// ComposeInteractive walks the user through building a formula interactively.
// Returns the built formula, TOML bytes, and any error.
func ComposeInteractive(name string, in io.Reader, out io.Writer) (*formula.FormulaV2, []byte, error) {
	reader := bufio.NewReader(in)
	builder := NewBuilder(name)

	fmt.Fprintf(out, "\nSpire Workshop — composing formula %q\n\n", name)

	// Step 1: description
	desc := promptString(reader, out, "Description", "")
	builder.SetDescription(desc)

	// Step 2: bead type
	types := []string{"task", "bug", "feature", "epic", "chore", "custom"}
	idx, err := promptChoice(reader, out, "Target bead type", types, 0)
	if err != nil {
		return nil, nil, err
	}
	beadType := types[idx]
	builder.SetBeadType(beadType)

	// Step 3: phase selection
	defaultPhases := DefaultPhasesForType(beadType)
	defaultSet := make(map[string]bool)
	for _, p := range defaultPhases {
		defaultSet[p] = true
	}
	preSelected := make([]bool, len(formula.ValidPhases))
	for i, p := range formula.ValidPhases {
		preSelected[i] = defaultSet[p]
	}
	selected := promptMultiSelect(reader, out, "Select phases", formula.ValidPhases, preSelected)
	for i, p := range formula.ValidPhases {
		if selected[i] {
			if err := builder.EnablePhase(p); err != nil {
				return nil, nil, err
			}
		}
	}

	if len(builder.Phases()) == 0 {
		return nil, nil, fmt.Errorf("no phases selected")
	}

	// Step 4: per-phase config
	for _, phase := range builder.Phases() {
		cfg, _ := builder.PhaseConfig(phase)
		fmt.Fprintf(out, "\n=== Configuring [%s] ===\n", phase)

		// Execution fields
		cfg.Role = promptString(reader, out, "  Role", cfg.GetRole())
		if phase != "merge" {
			cfg.Model = promptString(reader, out, "  Model", cfg.Model)
			cfg.Timeout = promptString(reader, out, "  Timeout", cfg.Timeout)
		}
		if phase == "implement" || phase == "plan" {
			cfg.Dispatch = promptString(reader, out, "  Dispatch (direct/wave/sequential)", cfg.GetDispatch())
		}

		// Boolean flags
		if phase == "implement" {
			cfg.Worktree = promptBool(reader, out, "  Worktree", cfg.Worktree)
			cfg.Apprentice = promptBool(reader, out, "  Apprentice mode", cfg.Apprentice)
		}
		if phase == "merge" {
			cfg.MergeStrategy = promptString(reader, out, "  Strategy (squash/merge/rebase)", cfg.GetMergeStrategy())
			cfg.Auto = promptBool(reader, out, "  Auto-merge", cfg.Auto)
		}
		if phase == "review" {
			cfg.VerdictOnly = promptBool(reader, out, "  Verdict only", cfg.VerdictOnly)
			cfg.Judgment = promptBool(reader, out, "  Judgment", cfg.Judgment)
			if cfg.RevisionPolicy == nil {
				cfg.RevisionPolicy = &formula.RevisionPolicy{MaxRounds: 3, ArbiterModel: "claude-opus-4-6"}
			}
			roundsStr := promptString(reader, out, "  Max review rounds", strconv.Itoa(cfg.RevisionPolicy.MaxRounds))
			if n, err := strconv.Atoi(roundsStr); err == nil && n > 0 {
				cfg.RevisionPolicy.MaxRounds = n
			}
			cfg.RevisionPolicy.ArbiterModel = promptString(reader, out, "  Arbiter model", cfg.RevisionPolicy.ArbiterModel)
		}

		// Branching
		if phase == "implement" || phase == "merge" {
			if cfg.StagingBranch != "" || cfg.GetDispatch() == "wave" {
				cfg.StagingBranch = promptString(reader, out, "  Staging branch", cfg.StagingBranch)
			}
		}

		// Context files
		if phase != "merge" {
			if len(cfg.Context) > 0 {
				ctxStr := promptString(reader, out, "  Context files (comma-separated)", strings.Join(cfg.Context, ", "))
				if ctxStr != "" {
					parts := strings.Split(ctxStr, ",")
					cfg.Context = nil
					for _, p := range parts {
						p = strings.TrimSpace(p)
						if p != "" {
							cfg.Context = append(cfg.Context, p)
						}
					}
				} else {
					cfg.Context = nil
				}
			}
		}

		if err := builder.ConfigurePhase(phase, cfg); err != nil {
			return nil, nil, err
		}
	}

	// Step 5: variables
	fmt.Fprintf(out, "\n=== Variables ===\n")
	fmt.Fprintf(out, "Add variables (empty name to finish):\n")
	for {
		varName := promptString(reader, out, "  Variable name", "")
		if varName == "" {
			break
		}
		varDesc := promptString(reader, out, "  Description", "")
		varRequired := promptBool(reader, out, "  Required", true)
		varDefault := promptString(reader, out, "  Default value", "")
		builder.AddVar(varName, formula.FormulaVar{
			Description: varDesc,
			Required:    varRequired,
			Default:     varDefault,
		})
	}

	// Step 6: review & save
	tomlBytes, err := builder.MarshalTOML()
	if err != nil {
		return nil, nil, fmt.Errorf("marshal formula: %w", err)
	}

	f, err := builder.Build()
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
			return f, tomlBytes, nil

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
			return f, tomlBytes, nil
		}
	}
}

// validateBuiltFormula runs validation on in-memory TOML bytes using the v2 validator.
func validateBuiltFormula(data []byte) ([]Issue, error) {
	return validateV2(data), nil
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
