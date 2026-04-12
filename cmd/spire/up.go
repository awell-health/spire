package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/awell-health/spire/pkg/olap"
	"github.com/awell-health/spire/pkg/process"
	"github.com/awell-health/spire/pkg/store"
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
		if v, _ := cmd.Flags().GetInt("metrics-port"); v > 0 {
			fullArgs = append(fullArgs, "--metrics-port", strconv.Itoa(v))
		}
		return cmdUp(fullArgs)
	},
}

func init() {
	upCmd.Flags().String("interval", "", "Daemon sync interval (e.g. 2m)")
	upCmd.Flags().Bool("steward", false, "Also start the steward")
	upCmd.Flags().String("backend", "", "Agent backend: process, docker, or k8s")
	upCmd.Flags().Int("metrics-port", 0, "Expose Prometheus metrics on this port (k8s mode; 0=disabled)")
}

func cmdUp(args []string) error {
	// Parse flags
	interval := "2m"
	startSteward := false
	backendName := ""
	metricsPort := ""
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
		case "--metrics-port":
			if i+1 >= len(args) {
				return fmt.Errorf("--metrics-port requires a port number")
			}
			i++
			metricsPort = args[i]
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire up [--interval 2m] [--steward] [--backend process|docker|k8s] [--metrics-port 9090]", args[i])
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

	// Pin spire's dolt port as the authoritative port for all beads operations.
	// The beads library resolves port as: BEADS_DOLT_SERVER_PORT env > dolt-server.port file > config.yaml.
	// Setting the env var ensures child processes (daemon, steward, bd) always hit spire's server.
	os.Setenv("BEADS_DOLT_SERVER_PORT", doltPort())

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

		// Remove stale dolt-server.port files from tower .beads/ dirs.
		// bd init or beads auto-start can leave these behind, causing the beads
		// library to connect to a shadow dolt server instead of spire's.
		for _, t := range towers {
			portFile := filepath.Join(doltDataDir(), t.Database, ".beads", "dolt-server.port")
			if _, err := os.Stat(portFile); err == nil {
				os.Remove(portFile)
				fmt.Printf("removed stale %s/.beads/dolt-server.port\n", t.Database)
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

		// Migrate label-identified internal beads to proper types
		fmt.Print("internal type migration: ")
		migWarned := 0
		for _, t := range towers {
			beadsDir := filepath.Join(doltDataDir(), t.Database, ".beads")
			if _, err := store.OpenAt(beadsDir); err != nil {
				fmt.Printf("\n  warning: %s: %s", t.Database, err)
				migWarned++
				continue
			}
			if err := store.MigrateInternalTypes(); err != nil {
				fmt.Printf("\n  warning: %s: %s", t.Database, err)
				migWarned++
			}
			store.Reset()
		}
		if migWarned > 0 {
			fmt.Println()
		} else {
			fmt.Printf("ok (%d tower(s))\n", len(towers))
		}

		// Ensure agent_runs + golden_prompts tables exist and columns are up-to-date.
		// Skip migrations when stored spire_version matches the binary (schema is current).
		fmt.Print("spire tables: ")
		arWarned := 0
		arSkipped := 0
		for _, t := range towers {
			beadsDir := filepath.Join(doltDataDir(), t.Database, ".beads")
			storedVer := readSpireVersion(beadsDir)
			action := decideVersionAction(storedVer)

			if action.warn {
				fmt.Fprintf(os.Stderr, "\nWarning: Tower %s was written by Spire %s, you're running %s. Run: brew upgrade spire\n",
					t.Name, action.storedVersion, action.binaryVersion)
			}

			if action.skipMigrations {
				arSkipped++
			} else {
				if err := migrateSpireTables(t.Database); err != nil {
					fmt.Printf("\n  warning: %s: %s", t.Database, err)
					arWarned++
				}
			}

			if action.writeVersion {
				writeSpireVersion(beadsDir, action.binaryVersion)
			}
		}
		if arWarned > 0 {
			fmt.Println()
		} else if arSkipped == len(towers) {
			fmt.Printf("ok (%d tower(s), skipped — version match)\n", len(towers))
		} else {
			fmt.Printf("ok (%d tower(s))\n", len(towers))
		}

		// Initialize OLAP (DuckDB) database and run initial ETL sync.
		// Uses the open→write→close pattern (olap.WriteFunc) so no persistent
		// DuckDB connection is held. Retry-on-lock handles the rare case where
		// the daemon is already running and mid-flush.
		fmt.Print("olap database: ")
		olapWarned := 0
		for _, t := range towers {
			olapPath := t.OLAPPath()
			if err := os.MkdirAll(filepath.Dir(olapPath), 0700); err != nil {
				fmt.Printf("\n  warning: %s: mkdir: %s", t.Name, err)
				olapWarned++
				continue
			}
			if err := olap.EnsureSchema(olapPath); err != nil {
				fmt.Printf("\n  warning: %s: init: %s", t.Name, err)
				olapWarned++
				continue
			}
			// Run initial ETL sync from Dolt into DuckDB.
			dsn := fmt.Sprintf("root:@tcp(%s:%s)/%s?parseTime=true", doltHost(), doltPort(), t.Database)
			doltConn, err := sql.Open("mysql", dsn)
			if err != nil {
				fmt.Printf("\n  warning: %s: dolt connect: %s", t.Name, err)
				olapWarned++
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			etl := olap.NewETLPath(olapPath)
			n, syncErr := etl.Sync(ctx, doltConn)
			cancel()
			doltConn.Close()
			if syncErr != nil {
				fmt.Printf("\n  warning: %s: sync: %s", t.Name, syncErr)
				olapWarned++
			} else if n > 0 {
				log.Printf("[up] [%s] olap: initial sync — %d rows", t.Name, n)
			}
		}
		if olapWarned > 0 {
			fmt.Println()
		} else {
			fmt.Printf("ok (%d tower(s))\n", len(towers))
		}
	}

	// Step 2.5: Clean dead wizards from registry and remove stale state files.
	fmt.Print("dead wizard cleanup: ")
	{
		reg := loadWizardRegistry()
		cleaned := cleanDeadWizards(reg, false)
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

		cwd, _ := os.Getwd()
		newPID, err := process.SpawnBackground(process.SpawnOpts{
			Name:    "daemon",
			Bin:     spireBin,
			Args:    []string{"daemon", "--interval", interval},
			Dir:     cwd,
			Env:     os.Environ(),
			LogDir:  gd,
			PIDPath: daemonPIDPath(),
		})
		if err != nil {
			fmt.Printf("error: %s\n", err)
			return fmt.Errorf("cannot start daemon: %w", err)
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
			if metricsPort != "" {
				stewardArgs = append(stewardArgs, "--metrics-port", metricsPort)
			}

			cwd, _ := os.Getwd()
			newPID, err := process.SpawnBackground(process.SpawnOpts{
				Name:    "steward",
				Bin:     spireBin,
				Args:    stewardArgs,
				Dir:     cwd,
				Env:     os.Environ(),
				LogDir:  gd,
				PIDPath: stewardPIDPath(),
			})
			if err != nil {
				fmt.Printf("error: %s\n", err)
				return fmt.Errorf("cannot start steward: %w", err)
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

// migrateSpireTables creates the agent_runs and golden_prompts tables if they
// don't exist, then runs column migrations for any missing columns.
// Idempotent — safe to call on every startup.
//
// ensureAgentRunsTable is kept as an alias for backward compatibility.
func migrateSpireTables(database string) error {
	// Create tables if they don't exist (initial schema).
	if _, err := rawDoltQuery(fmt.Sprintf("USE `%s`; %s", database, agentRunsTableSQL)); err != nil {
		return fmt.Errorf("create agent_runs: %w", err)
	}
	if _, err := rawDoltQuery(fmt.Sprintf("USE `%s`; %s", database, goldenPromptsTableSQL)); err != nil {
		return fmt.Errorf("create golden_prompts: %w", err)
	}
	if _, err := rawDoltQuery(fmt.Sprintf("USE `%s`; %s", database, formulasTableSQL)); err != nil {
		return fmt.Errorf("create formulas: %w", err)
	}
	if _, err := rawDoltQuery(fmt.Sprintf("USE `%s`; %s", database, recoveryLearningsTableSQL)); err != nil {
		return fmt.Errorf("create recovery_learnings: %w", err)
	}

	// Run column migrations — each entry checks SHOW COLUMNS and adds if missing.
	for _, m := range spireMigrations {
		if err := ensureColumn(database, m); err != nil {
			log.Printf("warning: migration %s.%s: %s", m.table, m.column, err)
		}
	}

	return nil
}

// ensureAgentRunsTable is the old name for migrateSpireTables, kept for
// backward compatibility with doctor.go and any other callers.
var ensureAgentRunsTable = migrateSpireTables

// ensureColumn checks whether a column exists in a table and adds it if missing.
// Uses SHOW COLUMNS LIKE to detect presence (MySQL 8.0 / Dolt compatible).
// If the column already exists, this is a no-op.
func ensureColumn(database string, m columnMigration) error {
	out, err := rawDoltQuery(fmt.Sprintf("USE `%s`; SHOW COLUMNS FROM %s LIKE '%s'", database, m.table, m.column))
	if err != nil {
		// Table may not exist yet — not fatal for migration purposes.
		return nil
	}
	if strings.Contains(out, m.column) {
		// Column already exists — nothing to do.
		return nil
	}

	// Column is missing — add it.
	if _, err := rawDoltQuery(fmt.Sprintf("USE `%s`; ALTER TABLE %s %s", database, m.table, m.ddl)); err != nil {
		return fmt.Errorf("ALTER TABLE %s %s: %w", m.table, m.ddl, err)
	}

	// Create index if specified. Errors are non-fatal (index may already exist).
	if m.index != "" {
		if _, err := rawDoltQuery(fmt.Sprintf("USE `%s`; %s", database, m.index)); err != nil {
			log.Printf("warning: index for %s.%s: %s", m.table, m.column, err)
		}
	}

	return nil
}
