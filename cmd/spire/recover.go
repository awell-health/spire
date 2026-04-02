package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	spgit "github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/spf13/cobra"
)

var recoverCmd = &cobra.Command{
	Use:   "recover <bead-id>",
	Short: "Diagnose and recover an interrupted bead",
	Long: `Diagnose an interrupted parent bead, classify the failure mode,
propose ranked recovery actions, and optionally execute one.

Flags:
  --dry-run   Diagnose only, do not execute any action
  --json      Output structured JSON (diagnosis + actions)
  --auto      Non-interactive: execute first non-destructive action;
              exit 2 if all destructive, exit 3 if wizard still running
  --action    Execute a specific action by name`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		jsonOut, _ := cmd.Flags().GetBool("json")
		auto, _ := cmd.Flags().GetBool("auto")
		action, _ := cmd.Flags().GetString("action")
		return cmdRecover(args[0], dryRun, jsonOut, auto, action)
	},
}

func init() {
	recoverCmd.Flags().Bool("dry-run", false, "Diagnose only, do not execute")
	recoverCmd.Flags().Bool("json", false, "Output structured JSON")
	recoverCmd.Flags().Bool("auto", false, "Non-interactive: auto-execute non-destructive action")
	recoverCmd.Flags().String("action", "", "Execute a specific action by name")
}

func cmdRecover(beadID string, dryRun, jsonOut, auto bool, actionName string) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	deps := buildRecoveryDeps()

	// --- Diagnose ---
	diag, err := recovery.Diagnose(beadID, deps)
	if err != nil {
		if jsonOut {
			out, _ := json.MarshalIndent(map[string]string{"error": err.Error()}, "", "  ")
			fmt.Println(string(out))
		}
		return err
	}

	// --- Dry-run: print and exit ---
	if dryRun {
		if jsonOut {
			return printDiagnosisJSON(diag, nil)
		}
		printDiagnosis(diag)
		return nil
	}

	// --- JSON without action: print and exit ---
	if jsonOut && actionName == "" && !auto {
		return printDiagnosisJSON(diag, nil)
	}

	// --- Find the action to execute ---
	var chosen *recovery.RecoveryAction

	if actionName != "" {
		// --action flag: find by name.
		for i := range diag.Actions {
			if diag.Actions[i].Name == actionName {
				chosen = &diag.Actions[i]
				break
			}
		}
		if chosen == nil {
			return fmt.Errorf("action %q not found in diagnosis (available: %s)", actionName, actionNames(diag.Actions))
		}
	} else if auto {
		// --auto: wizard running → exit 3.
		if diag.WizardRunning {
			if jsonOut {
				printDiagnosisJSON(diag, nil)
			} else {
				fmt.Printf("wizard %s is still running (not executing any action)\n", diag.WizardName)
			}
			os.Exit(recovery.ExitWizardRunning)
		}
		// Find first non-destructive action.
		for i := range diag.Actions {
			if !diag.Actions[i].Destructive {
				chosen = &diag.Actions[i]
				break
			}
		}
		if chosen == nil {
			// All destructive → exit 2.
			if jsonOut {
				printDiagnosisJSON(diag, nil)
			} else {
				printDiagnosis(diag)
				fmt.Println("\nAll proposed actions are destructive — requires human approval.")
			}
			os.Exit(recovery.ExitAllDestructive)
		}
	} else {
		// Interactive mode.
		if diag.WizardRunning {
			fmt.Printf("%s⚠ wizard %s is still running (pid may be active)%s\n", yellow, diag.WizardName, reset)
		}
		printDiagnosis(diag)
		if len(diag.Actions) == 0 {
			fmt.Println("No recovery actions available.")
			return nil
		}

		fmt.Println("\nChoose a recovery action:")
		for i, a := range diag.Actions {
			marker := " "
			if a.Destructive {
				marker = "!"
			}
			fmt.Printf("  %s[%d]%s %s%s — %s\n", cyan, i+1, reset, marker, a.Name, a.Description)
			if a.Warning != "" {
				fmt.Printf("       %s⚠ %s%s\n", yellow, a.Warning, reset)
			}
			fmt.Printf("       %s%s%s\n", dim, a.Equivalent, reset)
		}

		reader := bufio.NewReader(os.Stdin)
		fmt.Print("\nAction number (or q to quit): ")
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)
		if input == "q" || input == "" {
			return nil
		}

		idx, err := strconv.Atoi(input)
		if err != nil || idx < 1 || idx > len(diag.Actions) {
			return fmt.Errorf("invalid choice: %s", input)
		}
		chosen = &diag.Actions[idx-1]
	}

	// --- Confirm destructive actions ---
	if chosen.Destructive && !auto {
		fmt.Printf("\n%s⚠ %s is destructive: %s%s\n", red, chosen.Name, chosen.Description, reset)
		if diag.Git != nil && diag.Git.WorktreeDirty {
			fmt.Printf("%s  worktree has uncommitted changes that will be lost%s\n", red, reset)
		}
		fmt.Printf("Type %q to confirm: ", beadID)
		reader := bufio.NewReader(os.Stdin)
		confirm, _ := reader.ReadString('\n')
		confirm = strings.TrimSpace(confirm)
		if confirm != beadID {
			fmt.Println("Cancelled.")
			return nil
		}
	}

	// --- Execute ---
	fmt.Printf("  executing: %s\n", chosen.Equivalent)
	if execErr := executeRecoveryAction(beadID, chosen); execErr != nil {
		if jsonOut {
			printDiagnosisJSON(diag, &recovery.VerifyResult{Clean: false})
		}
		return fmt.Errorf("execute %s: %w", chosen.Name, execErr)
	}

	// --- Verify ---
	verifyResult, verifyErr := recovery.Verify(beadID, deps)
	if verifyErr != nil {
		fmt.Printf("  %s(verify warning: %s)%s\n", dim, verifyErr, reset)
	}

	if jsonOut {
		return printDiagnosisJSON(diag, verifyResult)
	}

	if verifyResult != nil {
		if verifyResult.Clean {
			fmt.Printf("  %s✓ recovery complete — interrupted state cleared%s\n", green, reset)
		} else {
			if len(verifyResult.InterruptLabels) > 0 {
				fmt.Printf("  %s⚠ remaining interrupted labels: %s%s\n", yellow, strings.Join(verifyResult.InterruptLabels, ", "), reset)
			}
			if verifyResult.NeedsHuman {
				fmt.Printf("  %s⚠ needs-human label still present%s\n", yellow, reset)
			}
			if verifyResult.AlertsOpen > 0 {
				fmt.Printf("  %s⚠ %d open alert beads remain%s\n", yellow, verifyResult.AlertsOpen, reset)
			}
		}
	}

	return nil
}

// executeRecoveryAction delegates to existing Spire commands.
func executeRecoveryAction(beadID string, action *recovery.RecoveryAction) error {
	switch {
	case action.Name == "resummon":
		return cmdResummon([]string{beadID})
	case action.Name == "reset-hard":
		return cmdReset([]string{beadID, "--hard"})
	case strings.HasPrefix(action.Name, "reset-to-"):
		phase := strings.TrimPrefix(action.Name, "reset-to-")
		return cmdReset([]string{beadID, "--to", phase})
	case action.Name == "close":
		return cmdClose([]string{beadID})
	case action.Name == "manual-fix" || action.Name == "manual-review":
		fmt.Println("  This action requires manual intervention — no automated execution.")
		return nil
	default:
		return fmt.Errorf("unknown recovery action: %s", action.Name)
	}
}

// printDiagnosis prints a human-readable diagnosis to stdout.
func printDiagnosis(diag *recovery.Diagnosis) {
	fmt.Printf("\n%sDiagnosis for %s%s\n", bold, diag.BeadID, reset)
	fmt.Printf("  Title:    %s\n", diag.Title)
	fmt.Printf("  Status:   %s\n", diag.Status)
	fmt.Printf("  Failure:  %s%s%s (%s)\n", red, diag.FailureMode, reset, diag.InterruptLabel)
	if diag.Phase != "" {
		fmt.Printf("  Phase:    %s\n", diag.Phase)
	}
	fmt.Printf("  Attempts: %d\n", diag.AttemptCount)
	if diag.LastAttemptResult != "" {
		fmt.Printf("  Last:     %s\n", diag.LastAttemptResult)
	}

	if diag.Git != nil {
		fmt.Printf("  Branch:   %s (exists=%v)\n", diag.Git.BranchName, diag.Git.BranchExists)
		if diag.Git.WorktreeExists {
			dirty := ""
			if diag.Git.WorktreeDirty {
				dirty = " (dirty)"
			}
			fmt.Printf("  Worktree: exists%s\n", dirty)
		}
	}

	if diag.WizardRunning {
		fmt.Printf("  Wizard:   %s%s running%s\n", yellow, diag.WizardName, reset)
	}

	if len(diag.AlertBeads) > 0 {
		fmt.Printf("  Alerts:   ")
		for i, a := range diag.AlertBeads {
			if i > 0 {
				fmt.Print(", ")
			}
			fmt.Printf("%s (%s)", a.ID, a.Label)
		}
		fmt.Println()
	}
}

// printDiagnosisJSON outputs diagnosis and optional verify result as JSON.
func printDiagnosisJSON(diag *recovery.Diagnosis, verify *recovery.VerifyResult) error {
	out := struct {
		Diagnosis *recovery.Diagnosis    `json:"diagnosis"`
		Verify    *recovery.VerifyResult `json:"verify,omitempty"`
	}{
		Diagnosis: diag,
		Verify:    verify,
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

// actionNames returns a comma-separated list of action names.
func actionNames(actions []recovery.RecoveryAction) string {
	names := make([]string, len(actions))
	for i, a := range actions {
		names[i] = a.Name
	}
	return strings.Join(names, ", ")
}

// buildRecoveryDeps wires up recovery.Deps from cmd/spire bridge functions.
func buildRecoveryDeps() *recovery.Deps {
	return &recovery.Deps{
		GetBead: func(id string) (recovery.DepBead, error) {
			b, err := storeGetBead(id)
			if err != nil {
				return recovery.DepBead{}, err
			}
			return recovery.DepBead{
				ID:     b.ID,
				Title:  b.Title,
				Status: b.Status,
				Labels: b.Labels,
				Parent: b.Parent,
			}, nil
		},
		GetChildren: func(parentID string) ([]recovery.DepBead, error) {
			children, err := storeGetChildren(parentID)
			if err != nil {
				return nil, err
			}
			result := make([]recovery.DepBead, len(children))
			for i, c := range children {
				result[i] = recovery.DepBead{
					ID:     c.ID,
					Title:  c.Title,
					Status: c.Status,
					Labels: c.Labels,
					Parent: c.Parent,
				}
			}
			return result, nil
		},
		GetDependentsWithMeta: func(id string) ([]recovery.DepDependent, error) {
			deps, err := storeGetDependentsWithMeta(id)
			if err != nil {
				return nil, err
			}
			result := make([]recovery.DepDependent, len(deps))
			for i, d := range deps {
				result[i] = recovery.DepDependent{
					ID:             d.ID,
					Status:         string(d.Status),
					Labels:         d.Labels,
					DependencyType: string(d.DependencyType),
				}
			}
			return result, nil
		},
		LoadExecutorState: func(agentName string) (*recovery.RuntimeState, error) {
			state, err := loadExecutorState(agentName)
			if err != nil {
				return nil, err
			}
			if state == nil {
				return nil, fmt.Errorf("no state")
			}
			return &recovery.RuntimeState{
				Phase:         state.Phase,
				Wave:          state.Wave,
				StagingBranch: state.StagingBranch,
				AttemptBeadID: state.AttemptBeadID,
				StepBeadIDs:   state.StepBeadIDs,
			}, nil
		},
		LookupRegistry: func(beadID string) (string, int, bool, error) {
			reg := loadWizardRegistry()
			wiz := findLiveWizardForBead(reg, beadID)
			if wiz == nil {
				return "wizard-" + beadID, 0, false, nil
			}
			alive := wiz.PID > 0 && processAlive(wiz.PID)
			return wiz.Name, wiz.PID, alive, nil
		},
		ResolveRepo: func(beadID string) (string, string, error) {
			repoPath, _, baseBranch, err := wizardResolveRepo(beadID)
			if err != nil {
				return "", "", err
			}
			return repoPath, baseBranch, nil
		},
		CheckBranchExists: func(repoPath, branch string) bool {
			rc := &spgit.RepoContext{Dir: repoPath}
			return rc.BranchExists(branch)
		},
		CheckWorktreeExists: func(dir string) bool {
			info, err := os.Stat(dir)
			return err == nil && info.IsDir()
		},
		CheckWorktreeDirty: func(dir string) bool {
			cmd := exec.Command("git", "status", "--porcelain")
			cmd.Dir = dir
			out, err := cmd.Output()
			return err == nil && strings.TrimSpace(string(out)) != ""
		},
	}
}
