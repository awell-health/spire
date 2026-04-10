package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/dolt"
	"github.com/spf13/cobra"
)

var sqlCmd = &cobra.Command{
	Use:   "sql [query]",
	Short: "Run SQL against the dolt server",
	Long: `Run a SQL query directly against the tower's dolt database.

If a query is given, executes it and prints the result.
If no query is given, opens an interactive dolt SQL shell.

Examples:
  spire sql "SELECT * FROM dolt_conflicts"
  spire sql "SHOW TABLES"
  spire sql "SELECT * FROM dolt_status"
  spire sql`,
	Args:                  cobra.ArbitraryArgs,
	DisableFlagsInUseLine: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmdSQL(args)
	},
}

func cmdSQL(args []string) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	if err := requireDolt(); err != nil {
		return err
	}

	tower, err := config.ResolveTowerConfig()
	if err != nil {
		return fmt.Errorf("sql: resolve tower: %w", err)
	}
	dbName := tower.Database

	if len(args) == 0 {
		return dolt.InteractiveSQL(dbName)
	}

	query := strings.Join(args, " ")
	output, err := dolt.SQL(query, false, dbName, nil)
	if err != nil {
		return err
	}
	if output != "" {
		fmt.Println(output)
	}
	return nil
}
