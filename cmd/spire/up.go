package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

var upCmd = &cobra.Command{
	Use:   "up",
	Short: "Start dolt server + daemon (--interval)",
	RunE: func(cmd *cobra.Command, args []string) error {
		var fullArgs []string
		if v, _ := cmd.Flags().GetString("interval"); v != "" {
			fullArgs = append(fullArgs, "--interval", v)
		}
		if steward, _ := cmd.Flags().GetBool("steward"); steward {
			fullArgs = append(fullArgs, "--steward")
		}
		if v, _ := cmd.Flags().GetString("backend"); v != "" {
			fullArgs = append(fullArgs, "--backend", v)
		}
		return cmdUp(fullArgs)
	},
}

func init() {
	upCmd.Flags().String("interval", "", "Daemon sync interval (e.g. 2m)")
	upCmd.Flags().Bool("steward", false, "Also start the steward")
	upCmd.Flags().String("backend", "", "Agent backend: process, docker, or k8s")
}

func cmdUp(args []string) error {
	// Parse flags
	interval := "2m"
	startSteward := false
	backendName := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--interval":
			if i+1 >= len(args) {
				return fmt.Errorf("--interval requires a value")
			}
			i++
			interval = args[i]
		case "--steward":
			startSteward = true
		case "--backend":
			if i+1 >= len(args) {
				return fmt.Errorf("--backend requires a value: process, docker, or k8s")
			}
			i++
			backendName = args[i]
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire up [--interval 2m] [--steward] [--backend process|docker|k8s]", args[i])
		}
	}

	// Step 0: Ensure dolt binary is available
	fmt.Print("dolt binary: ")
	binPath, err := doltEnsureBinary()
	if err != nil {
		fmt.Printf("error: %s\n", err)
		return fmt.Errorf("cannot ensure dolt binary: %w", err)
	}
	fmt.Printf("ok (%s)\n", binPath)

	// Step 1: Start dolt server
	fmt.Print("dolt server: ")
	pid, running, reachable := doltServerStatus()
	if running && reachable {
		fmt.Printf("already running (pid %d, port %s)\n", pid, doltPort())
	} else if reachable {
		fmt.Printf("running externally (port %s)\n", doltPort())
	} else {
		newPID, err := doltStart()
		if err != nil {
			fmt.Printf("error: %s\n", err)
			return fmt.Errorf("cannot start dolt server: %w", err)
		}
		fmt.Printf("started (pid %d, port %s)\n", newPID, doltPort())
	}

	// Step 2: Ensure tower configs are healthy.
	towers, _ := listTowerConfigs()
	if len(towers) == 0 {
		fmt.Println("towers: none configured")
	} else {
		// Ensure archmage identity (backfill from global git config for towers missing it).
		// Use --global to avoid picking up repo-local config set by wizard agents.
		globalGitName := gitConfigGet("--global", "user.name")
		globalGitEmail := gitConfigGet("--global", "user.email")
		for i, t := range towers {
			if t.Archmage.Name == "" && globalGitName != "" {
				towers[i].Archmage.Name = globalGitName
				towers[i].Archmage.Email = globalGitEmail
				saveTowerConfig(&towers[i])
				fmt.Printf("archmage identity: backfilled from global git config (%s <%s>)\n", towers[i].Archmage.Name, towers[i].Archmage.Email)
			}
		}

		// Ensure custom bead types
		fmt.Print("custom bead types: ")
		warned := 0
		for _, t := range towers {
			beadsDir := filepath.Join(doltDataDir(), t.Database, ".beads")
			if err := ensureCustomBeadTypes(beadsDir); err != nil {
				fmt.Printf("\n  warning: %s: %s", t.Database, err)
				warned++
			}
		}
		if warned > 0 {
			fmt.Println()
		} else {
			fmt.Printf("ok (%d tower(s))\n", len(towers))
		}

		// Ensure agent_runs + golden_prompts tables exist (idempotent)
		fmt.Print("agent_runs table: ")
		arWarned := 0
		for _, t := range towers {
			if err := ensureAgentRunsTable(t.Database); err != nil {
				fmt.Printf("\n  warning: %s: %s", t.Database, err)
				arWarned++
			}
		}
		if arWarned > 0 {
			fmt.Println()
		} else {
			fmt.Printf("ok (%d tower(s))\n", len(towers))
		}
	}

	// Step 2.5: Clean dead wizards from registry and remove stale state files.
	fmt.Print("dead wizard cleanup: ")
	{
		reg := loadWizardRegistry()
		cleaned := cleanDeadWizards(reg)
		if len(reg.Wizards) > len(cleaned.Wizards) {
			saveWizardRegistry(cleaned)
			fmt.Printf("reaped %d defunct process(es)\n", len(reg.Wizards)-len(cleaned.Wizards))
		} else {
			fmt.Println("none")
		}
	}

	// Find spire binary (shared by daemon and steward steps)
	spireBin, err := os.Executable()
	if err != nil {
		spireBin, err = exec.LookPath("spire")
		if err != nil {
			return fmt.Errorf("cannot find spire binary")
		}
	}
	gd := doltGlobalDir()

	// Step 2: Start daemon
	fmt.Print("spire daemon: ")
	daemonPID := readPID(daemonPIDPath())
	if daemonPID > 0 && processAlive(daemonPID) {
		fmt.Printf("already running (pid %d)\n", daemonPID)
	} else {
		// Remove stale PID file
		if daemonPID > 0 {
			os.Remove(daemonPIDPath())
		}

		cmd := exec.Command(spireBin, "daemon", "--interval", interval)
		cmd.Dir, _ = os.Getwd()
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		cmd.Env = os.Environ()

		// Redirect daemon output to log files in global dir
		logFile, _ := os.OpenFile(filepath.Join(gd, "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		errFile, _ := os.OpenFile(filepath.Join(gd, "daemon.error.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		cmd.Stdout = logFile
		cmd.Stderr = errFile

		if err := cmd.Start(); err != nil {
			fmt.Printf("error: %s\n", err)
			return fmt.Errorf("cannot start daemon: %w", err)
		}

		newPID := cmd.Process.Pid
		writePID(daemonPIDPath(), newPID)
		cmd.Process.Release()

		if logFile != nil {
			logFile.Close()
		}
		if errFile != nil {
			errFile.Close()
		}

		// Brief wait to confirm it stayed alive
		time.Sleep(500 * time.Millisecond)
		if processAlive(newPID) {
			fmt.Printf("started (pid %d, interval %s)\n", newPID, interval)
		} else {
			fmt.Printf("started but may have exited (pid %d)\n", newPID)
		}
	}

	// Step 3: Start steward (if --steward)
	if startSteward {
		fmt.Print("spire steward: ")
		stewardPID := readPID(stewardPIDPath())
		if stewardPID > 0 && processAlive(stewardPID) {
			fmt.Printf("already running (pid %d)\n", stewardPID)
		} else {
			// Remove stale PID file
			if stewardPID > 0 {
				os.Remove(stewardPIDPath())
			}

			stewardArgs := []string{"steward", "--interval", interval}
			if backendName != "" {
				stewardArgs = append(stewardArgs, "--backend", backendName)
			}

			cmd := exec.Command(spireBin, stewardArgs...)
			cmd.Dir, _ = os.Getwd()
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			cmd.Env = os.Environ()

			logFile, _ := os.OpenFile(filepath.Join(gd, "steward.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
			errFile, _ := os.OpenFile(filepath.Join(gd, "steward.error.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
			cmd.Stdout = logFile
			cmd.Stderr = errFile

			if err := cmd.Start(); err != nil {
				fmt.Printf("error: %s\n", err)
				return fmt.Errorf("cannot start steward: %w", err)
			}

			newPID := cmd.Process.Pid
			writePID(stewardPIDPath(), newPID)
			cmd.Process.Release()

			if logFile != nil {
				logFile.Close()
			}
			if errFile != nil {
				errFile.Close()
			}

			// Brief wait to confirm it stayed alive
			time.Sleep(500 * time.Millisecond)
			if processAlive(newPID) {
				fmt.Printf("started (pid %d, interval %s)\n", newPID, interval)
			} else {
				fmt.Printf("started but may have exited (pid %d)\n", newPID)
			}
		}
	}

	return nil
}

// ensureAgentRunsTable creates the agent_runs and golden_prompts tables if they
// don't exist. Idempotent — safe to call on every startup.
// Also runs schema migrations for existing tables (e.g. adding the phase column).
func ensureAgentRunsTable(database string) error {
	if _, err := rawDoltQuery(fmt.Sprintf("USE `%s`; %s", database, agentRunsTableSQL)); err != nil {
		return fmt.Errorf("create agent_runs: %w", err)
	}
	if _, err := rawDoltQuery(fmt.Sprintf("USE `%s`; %s", database, goldenPromptsTableSQL)); err != nil {
		return fmt.Errorf("create golden_prompts: %w", err)
	}

	// Migration 002: add phase column if it doesn't exist.
	out, err := rawDoltQuery(fmt.Sprintf("USE `%s`; SELECT COUNT(*) AS cnt FROM information_schema.columns WHERE table_name='agent_runs' AND column_name='phase'", database))
	if err == nil && !strings.Contains(out, "\"1\"") && !strings.Contains(out, "cnt\":1") {
		rawDoltQuery(fmt.Sprintf("USE `%s`; ALTER TABLE agent_runs ADD COLUMN phase VARCHAR(16) AFTER role", database))
		rawDoltQuery(fmt.Sprintf("USE `%s`; CREATE INDEX idx_agent_runs_phase ON agent_runs (phase)", database))
	}

	return nil
}
