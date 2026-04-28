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
	towerpkg "github.com/awell-health/spire/pkg/tower"
	"github.com/spf13/cobra"
)

var upCmd = &cobra.Command{
	Use:   "up",
	Short: "Start the local control plane (dolt + daemon + steward)",
	RunE: func(cmd *cobra.Command, args []string) error {
		var fullArgs []string
		if v, _ := cmd.Flags().GetString("interval"); v != "" {
			fullArgs = append(fullArgs, "--interval", v)
		}
		if v, _ := cmd.Flags().GetString("steward-interval"); v != "" {
			fullArgs = append(fullArgs, "--steward-interval", v)
		}
		if noSteward, _ := cmd.Flags().GetBool("no-steward"); noSteward {
			fullArgs = append(fullArgs, "--no-steward")
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
	upCmd.Flags().String("steward-interval", "", "Steward cycle interval (e.g. 10s)")
	upCmd.Flags().Bool("no-steward", false, "Don't start the steward (sync-only/debug mode)")
	upCmd.Flags().Bool("steward", false, "Deprecated: steward starts by default; use --no-steward to opt out")
	_ = upCmd.Flags().MarkDeprecated("steward", "steward starts by default; use --no-steward to opt out")
	upCmd.Flags().String("backend", "", "Agent backend: process, docker, or k8s")
	upCmd.Flags().Int("metrics-port", 0, "Expose Prometheus metrics on this port (k8s mode; 0=disabled)")
}

// upOpts captures the parsed flags for `spire up`.
type upOpts struct {
	interval        string
	stewardInterval string
	startSteward    bool
	backendName     string
	metricsPort     string
}

// parseUpArgs parses the argv passed to cmdUp. The steward starts by default;
// `--no-steward` opts out. `--steward` is accepted as a back-compat no-op.
//
// The daemon and steward have independent intervals: `--interval` controls the
// daemon (heavy: dolt push/pull, Linear sync, OLAP ETL — 2m default), and
// `--steward-interval` controls the steward (cheap: local dolt queries +
// PID probes — 10s default for low ready→spawn latency).
func parseUpArgs(args []string) (upOpts, error) {
	opts := upOpts{
		interval:        "2m",
		stewardInterval: "10s",
		startSteward:    true,
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--interval":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("--interval requires a value")
			}
			i++
			opts.interval = args[i]
		case "--steward-interval":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("--steward-interval requires a value")
			}
			i++
			opts.stewardInterval = args[i]
		case "--steward":
			// Back-compat no-op: the steward starts by default now.
		case "--no-steward":
			opts.startSteward = false
		case "--backend":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("--backend requires a value: process, docker, or k8s")
			}
			i++
			opts.backendName = args[i]
		case "--metrics-port":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("--metrics-port requires a port number")
			}
			i++
			opts.metricsPort = args[i]
		default:
			return opts, fmt.Errorf("unknown flag: %s\nusage: spire up [--interval 2m] [--steward-interval 10s] [--no-steward] [--backend process|docker|k8s] [--metrics-port 9090]", args[i])
		}
	}
	return opts, nil
}

func cmdUp(args []string) error {
	opts, err := parseUpArgs(args)
	if err != nil {
		return err
	}
	interval := opts.interval
	stewardInterval := opts.stewardInterval
	startSteward := opts.startSteward
	backendName := opts.backendName
	metricsPort := opts.metricsPort

	// Prevent multiple 'spire up' from racing.
	lockPath := filepath.Join(doltGlobalDir(), "spire-up.lock")
	lock, lockErr := process.AcquireLock(lockPath)
	if lockErr != nil {
		return fmt.Errorf("cannot start: %s", lockErr)
	}
	defer lock.Release()

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

		// Ensure custom bead statuses (e.g. ready:active)
		fmt.Print("custom bead statuses: ")
		statusWarned := 0
		for _, t := range towers {
			beadsDir := filepath.Join(doltDataDir(), t.Database, ".beads")
			if err := ensureCustomBeadStatuses(beadsDir); err != nil {
				fmt.Printf("\n  warning: %s: %s", t.Database, err)
				statusWarned++
			}
		}
		if statusWarned > 0 {
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
			if syncErr != nil {
				fmt.Printf("\n  warning: %s: sync: %s", t.Name, syncErr)
				olapWarned++
			} else if n > 0 {
				log.Printf("[up] [%s] olap: initial sync — %d rows", t.Name, n)
			}
			// Sync the bead_lifecycle sidecar alongside agent_runs. Runs
			// even when agent_runs had zero rows — lifecycle rows exist for
			// every bead, not just those with agent activity.
			nL, syncErrL := etl.SyncBeadLifecycle(ctx, doltConn)
			if syncErrL != nil {
				fmt.Printf("\n  warning: %s: lifecycle sync: %s", t.Name, syncErrL)
				olapWarned++
			} else if nL > 0 {
				log.Printf("[up] [%s] olap: bead_lifecycle sync — %d rows", t.Name, nL)
			}
			cancel()
			doltConn.Close()
		}
		if olapWarned > 0 {
			fmt.Println()
		} else {
			fmt.Printf("ok (%d tower(s))\n", len(towers))
		}
	}

	// Step 2.5: Prune dead wizard registry entries (PID no longer alive).
	// Bead-level cleanup (attempt close, bead reopen) is handled by
	// beadlifecycle.OrphanSweep which runs on every BeginWork and steward tick.
	fmt.Print("dead wizard cleanup: ")
	{
		reg := loadWizardRegistry()
		before := len(reg.Wizards)
		var live []localWizard
		for _, w := range reg.Wizards {
			if w.PID > 0 && processAlive(w.PID) {
				live = append(live, w)
			}
		}
		if len(live) < before {
			reg.Wizards = live
			saveWizardRegistry(reg)
			fmt.Printf("pruned %d defunct process(es)\n", before-len(live))
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

	// Step 3: Start steward (default; skipped by --no-steward).
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

			stewardArgs := []string{"steward", "--interval", stewardInterval}
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
				fmt.Printf("started (pid %d, interval %s)\n", newPID, stewardInterval)
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
	// Apply Spire's schema extensions (repos, agent_runs, bead_lifecycle)
	// — idempotent via CREATE TABLE IF NOT EXISTS. Running here is the
	// defense-in-depth backstop if a blank tower was bootstrapped by an
	// older binary that predated the shared helper.
	if err := towerpkg.ApplySpireExtensions(rawDoltQuery, database); err != nil {
		return err
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
	// bead_lifecycle is a sidecar keyed by bead_id that holds first-
	// transition timestamps (filed/ready/started/closed) for every bead.
	// It complements agent_runs — one row per bead, not per run.
	if _, err := rawDoltQuery(fmt.Sprintf("USE `%s`; %s", database, store.BeadLifecycleTableSQL)); err != nil {
		return fmt.Errorf("create bead_lifecycle: %w", err)
	}
	// workload_intents is the dolt-backed outbox the cluster-native
	// steward publishes WorkloadIntent values into and the operator's
	// IntentWorkloadReconciler consumes from. Local-native towers
	// still create the table (idempotent DDL; costs nothing) so a
	// later switch to cluster-native doesn't require a second
	// migration pass.
	if _, err := rawDoltQuery(fmt.Sprintf("USE `%s`; %s", database, store.WorkloadIntentsTableSQL)); err != nil {
		return fmt.Errorf("create workload_intents: %w", err)
	}
	// agent_log_artifacts is the manifest/index for log artifacts whose
	// bytes live in pkg/logartifact's local filesystem or GCS backends.
	// Idempotent DDL; costs nothing for local-native towers that never
	// need the manifest until cluster log export lands.
	if _, err := rawDoltQuery(fmt.Sprintf("USE `%s`; %s", database, store.AgentLogArtifactsTableSQL)); err != nil {
		return fmt.Errorf("create agent_log_artifacts: %w", err)
	}

	// Run column migrations — each entry checks SHOW COLUMNS and adds if missing.
	for _, m := range spireMigrations {
		if err := ensureColumn(database, m); err != nil {
			log.Printf("warning: migration %s.%s: %s", m.table, m.column, err)
		}
	}

	// Backfill bead_lifecycle from the beads `issues` table. INSERT IGNORE
	// is idempotent — historical beads get filed_at from created_at and
	// closed_at from issues.closed_at; ready_at / started_at stay NULL.
	if err := backfillBeadLifecycle(database); err != nil {
		log.Printf("warning: backfill bead_lifecycle for %s: %s", database, err)
	}

	return nil
}

// backfillBeadLifecycle seeds the sidecar table from the beads library's
// `issues` table on first run. See store.BackfillBeadLifecycle for SQL
// semantics; here we dispatch via the dolt CLI because cmd/spire already
// uses rawDoltQuery elsewhere for schema management.
func backfillBeadLifecycle(database string) error {
	sqlStmt := `INSERT IGNORE INTO bead_lifecycle (
        bead_id, bead_type, filed_at, ready_at, started_at, closed_at,
        updated_at, review_count, fix_count, arbiter_count
    ) SELECT
        id, issue_type, created_at, NULL, NULL, closed_at,
        COALESCE(updated_at, created_at), NULL, NULL, NULL
    FROM issues`
	if _, err := rawDoltQuery(fmt.Sprintf("USE `%s`; %s", database, sqlStmt)); err != nil {
		return err
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
