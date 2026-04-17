package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/spf13/cobra"
)

var designCmd = &cobra.Command{
	Use:   "design <title> [flags]",
	Short: "Create a design bead (brainstorm/exploration artifact)",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var fullArgs []string
		if cmd.Flags().Changed("priority") {
			p, _ := cmd.Flags().GetInt("priority")
			fullArgs = append(fullArgs, "-p", strconv.Itoa(p))
		}
		if v, _ := cmd.Flags().GetString("description"); v != "" {
			fullArgs = append(fullArgs, "-d", v)
		}
		if v, _ := cmd.Flags().GetString("parent"); v != "" {
			fullArgs = append(fullArgs, "--parent", v)
		}
		if v, _ := cmd.Flags().GetString("prefix"); v != "" {
			fullArgs = append(fullArgs, "--prefix", v)
		}
		if v, _ := cmd.Flags().GetString("label"); v != "" {
			fullArgs = append(fullArgs, "--label", v)
		}
		fullArgs = append(fullArgs, args...)
		return cmdDesign(fullArgs)
	},
}

func init() {
	designCmd.Flags().IntP("priority", "p", 3, "Priority (0-4)")
	designCmd.Flags().StringP("description", "d", "", "Description")
	designCmd.Flags().String("parent", "", "Parent bead ID")
	designCmd.Flags().String("prefix", "", "Repo prefix")
	designCmd.Flags().String("label", "", "Comma-separated labels")
}

var (
	designCreateBeadFunc      = func(opts createOpts) (string, error) { return storeCreateBead(opts) }
	designAddLabelFunc        = func(id, label string) error { return storeAddLabel(id, label) }
	designResolvePrefixFunc   = resolveDesignPrefix
	designRequireApprovalFunc = resolveDesignRequireApproval
)

func resolveDesignPrefix(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if cwd, err := realCwd(); err == nil {
		if cfg, err := loadConfig(); err == nil {
			if inst := findInstanceByPath(cfg, cwd); inst != nil {
				return inst.Prefix, nil
			}
		}
	}
	if tower, err := activeTowerConfig(); err == nil && tower != nil && tower.HubPrefix != "" {
		return tower.HubPrefix, nil
	}
	return "", fmt.Errorf("no repo registered for this directory.\nRun `spire repo add` to register, or use `spire design --prefix <name> \"Title\"`")
}

func resolveDesignRequireApproval() bool {
	if cwd, err := os.Getwd(); err == nil {
		if rc, rcErr := repoconfig.Load(cwd); rcErr == nil {
			return repoconfig.ResolveDesignRequireApproval(rc.Design.RequireApproval)
		}
	}
	return true
}

// cmdDesign creates a design bead — a thinking artifact for brainstorming
// and exploration. Design beads capture the "why" and "why not" before
// committing to work items (tasks, epics, bugs).
//
// Usage: spire design "Title" [-p priority] [-d description] [--parent id]
func cmdDesign(args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Println(`usage: spire design "Title" [-p priority] [-d description] [--parent id]

Create a design bead — a thinking artifact for brainstorming and exploration.

Design beads are not work items. They capture exploration, rejected approaches,
and design decisions. When the design is settled, create a task/epic/bug and
link it to the design bead with a discovered-from dependency.

Workflow:
  spire design "Auth system overhaul"     # create design bead → spi-xxx
  # brainstorm, add comments, iterate...
  bd comments add spi-xxx "approach A: ..."
  bd comments add spi-xxx "rejected because ..."
  # when ready to commit:
  spire file "Auth overhaul" -t epic -p 1 --ref spi-xxx
  # or manually:
  spire file "Auth overhaul" -t epic -p 1
  bd dep add <epic-id> spi-xxx --type discovered-from`)
		return nil
	}

	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	// Parse args
	opts := createOpts{Type: parseIssueType("design")}
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-p" || args[i] == "--priority":
			if i+1 < len(args) {
				i++
				if p, err := strconv.Atoi(args[i]); err == nil {
					opts.Priority = p
				}
			}
		case args[i] == "-d" || args[i] == "--description":
			if i+1 < len(args) {
				i++
				opts.Description = args[i]
			}
		case args[i] == "--parent":
			if i+1 < len(args) {
				i++
				opts.Parent = args[i]
			}
		case args[i] == "--prefix":
			if i+1 < len(args) {
				i++
				opts.Prefix = args[i]
			}
		case args[i] == "--label" || args[i] == "--labels":
			if i+1 < len(args) {
				i++
				for _, l := range strings.Split(args[i], ",") {
					if l = strings.TrimSpace(l); l != "" {
						opts.Labels = append(opts.Labels, l)
					}
				}
			}
		default:
			if opts.Title == "" {
				opts.Title = args[i]
			}
		}
	}

	if opts.Title == "" {
		return fmt.Errorf("design: title is required")
	}

	prefix, err := designResolvePrefixFunc(opts.Prefix)
	if err != nil {
		return err
	}
	opts.Prefix = prefix

	id, err := designCreateBeadFunc(opts)
	if err != nil {
		return fmt.Errorf("design: %w", err)
	}

	if designRequireApprovalFunc() {
		if err := designAddLabelFunc(id, "needs-human"); err != nil {
			return fmt.Errorf("design: add needs-human label: %w", err)
		}
	}

	fmt.Println(id)
	return nil
}
