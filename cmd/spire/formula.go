package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/formula"
	"github.com/spf13/cobra"
)

var formulaCmd = &cobra.Command{
	Use:   "formula",
	Short: "Manage tower-level formulas",
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Help()
	},
}

var formulaListCmd = &cobra.Command{
	Use:   "list",
	Short: "List tower formulas",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if d := resolveBeadsDir(); d != "" {
			os.Setenv("BEADS_DIR", d)
		}
		return cmdFormulaList()
	},
}

var formulaShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Display raw TOML of a tower formula",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if d := resolveBeadsDir(); d != "" {
			os.Setenv("BEADS_DIR", d)
		}
		return cmdFormulaShow(args[0])
	},
}

var formulaPublishCmd = &cobra.Command{
	Use:   "publish <file>",
	Short: "Publish a formula to the tower",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if d := resolveBeadsDir(); d != "" {
			os.Setenv("BEADS_DIR", d)
		}
		return cmdFormulaPublish(args[0])
	},
}

var formulaRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Remove a tower formula",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if d := resolveBeadsDir(); d != "" {
			os.Setenv("BEADS_DIR", d)
		}
		return cmdFormulaRemove(args[0])
	},
}

func init() {
	formulaCmd.AddCommand(formulaListCmd, formulaShowCmd, formulaPublishCmd, formulaRemoveCmd)
}

// storeTowerDB returns a *sql.DB for the tower's dolt database by
// type-asserting the active beads store to its underlying RawDBAccessor.
func storeTowerDB() (*sql.DB, error) {
	s, err := ensureStore()
	if err != nil {
		return nil, err
	}
	type dbAccessor interface {
		DB() *sql.DB
	}
	accessor, ok := s.(dbAccessor)
	if !ok {
		return nil, fmt.Errorf("store does not support raw DB access")
	}
	return accessor.DB(), nil
}

func cmdFormulaList() error {
	formulas, err := storeListTowerFormulas()
	if err != nil {
		return err
	}
	if len(formulas) == 0 {
		fmt.Println("no tower formulas")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tDESCRIPTION\tUPDATED")
	for _, f := range formulas {
		fmt.Fprintf(w, "%s\t%s\t%s\n", f.Name, f.Description, f.UpdatedAt.Format("2006-01-02 15:04"))
	}
	return w.Flush()
}

func cmdFormulaShow(name string) error {
	content, err := storeGetTowerFormula(name)
	if err != nil {
		return err
	}
	fmt.Print(content)
	return nil
}

func cmdFormulaPublish(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading formula file: %w", err)
	}
	content := string(raw)

	// Validate before writing
	g, err := formula.ParseFormulaStepGraph(raw)
	if err != nil {
		return fmt.Errorf("invalid formula: %w", err)
	}

	// Derive name from formula metadata; fall back to filename stem
	name := g.Name
	if name == "" {
		base := filepath.Base(path)
		name = strings.TrimSuffix(base, ".formula.toml")
	}
	desc := g.Description

	author := ""
	if id, idErr := config.DetectIdentity(""); idErr == nil {
		author = id
	}

	if err := storePublishTowerFormula(name, content, desc, author); err != nil {
		return err
	}
	fmt.Printf("published formula %q\n", name)
	return nil
}

func cmdFormulaRemove(name string) error {
	if err := storeRemoveTowerFormula(name); err != nil {
		return err
	}
	fmt.Printf("removed formula %q\n", name)
	return nil
}
