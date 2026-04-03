package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

var fileCmd = &cobra.Command{
	Use:   "file <title> [flags]",
	Short: "Create a bead (--prefix, -t type, -p priority)",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// Reconstruct args with flags for the existing parser
		var fullArgs []string
		if v, _ := cmd.Flags().GetString("prefix"); v != "" {
			fullArgs = append(fullArgs, "--prefix", v)
		}
		if v, _ := cmd.Flags().GetString("branch"); v != "" {
			fullArgs = append(fullArgs, "--branch", v)
		}
		if v, _ := cmd.Flags().GetString("merge-mode"); v != "" {
			fullArgs = append(fullArgs, "--merge-mode", v)
		}
		if v, _ := cmd.Flags().GetString("ref"); v != "" {
			fullArgs = append(fullArgs, "--ref", v)
		}
		if v, _ := cmd.Flags().GetString("type"); v != "" {
			fullArgs = append(fullArgs, "-t", v)
		}
		if cmd.Flags().Changed("priority") {
			v, _ := cmd.Flags().GetInt("priority")
			fullArgs = append(fullArgs, "-p", strconv.Itoa(v))
		}
		if v, _ := cmd.Flags().GetString("label"); v != "" {
			fullArgs = append(fullArgs, "--label", v)
		}
		if v, _ := cmd.Flags().GetString("description"); v != "" {
			fullArgs = append(fullArgs, "--description", v)
		}
		if v, _ := cmd.Flags().GetString("parent"); v != "" {
			fullArgs = append(fullArgs, "--parent", v)
		}
		fullArgs = append(fullArgs, args...)
		return cmdFile(fullArgs)
	},
}

func init() {
	fileCmd.Flags().String("prefix", "", "Repo prefix")
	fileCmd.Flags().String("branch", "", "Feature branch name")
	fileCmd.Flags().String("merge-mode", "", "Merge mode: merge or pr")
	fileCmd.Flags().String("ref", "", "Design bead ID to link via discovered-from dep")
	fileCmd.Flags().StringP("type", "t", "", "Bead type (task, bug, feature, epic, chore, recovery)")
	fileCmd.Flags().IntP("priority", "p", 0, "Priority (0-4)")
	fileCmd.Flags().String("label", "", "Comma-separated labels")
	fileCmd.Flags().String("description", "", "Bead description")
	fileCmd.Flags().String("parent", "", "Parent bead ID for hierarchical IDs")
}

func cmdFile(args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Println("usage: spire file <title> [--prefix <prefix>] [--branch <name>] [--merge-mode <merge|pr>] [--ref <design-bead-id>] [bd create flags...]")
		return nil
	}

	// Extract spire-specific flags from args; pass everything else to bd create
	var prefix, branch, mergeMode, ref string
	remaining := []string{}

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--prefix":
			if i+1 >= len(args) {
				return fmt.Errorf("--prefix requires a value")
			}
			i++
			prefix = args[i]
		case strings.HasPrefix(args[i], "--prefix="):
			prefix = strings.TrimPrefix(args[i], "--prefix=")
		case args[i] == "--branch":
			if i+1 >= len(args) {
				return fmt.Errorf("--branch requires a value")
			}
			i++
			branch = args[i]
		case strings.HasPrefix(args[i], "--branch="):
			branch = strings.TrimPrefix(args[i], "--branch=")
		case args[i] == "--merge-mode":
			if i+1 >= len(args) {
				return fmt.Errorf("--merge-mode requires a value")
			}
			i++
			mergeMode = args[i]
		case strings.HasPrefix(args[i], "--merge-mode="):
			mergeMode = strings.TrimPrefix(args[i], "--merge-mode=")
		case args[i] == "--ref":
			if i+1 >= len(args) {
				return fmt.Errorf("--ref requires a value")
			}
			i++
			ref = args[i]
		case strings.HasPrefix(args[i], "--ref="):
			ref = strings.TrimPrefix(args[i], "--ref=")
		default:
			remaining = append(remaining, args[i])
		}
	}

	// Validate merge-mode if provided.
	if mergeMode != "" && mergeMode != "merge" && mergeMode != "pr" {
		return fmt.Errorf("--merge-mode must be 'merge' or 'pr', got %q", mergeMode)
	}

	// Fall back to CWD detection
	var instPath string
	if prefix == "" {
		if cwd, err := realCwd(); err == nil {
			if cfg, err := loadConfig(); err == nil {
				if inst := findInstanceByPath(cfg, cwd); inst != nil {
					prefix = inst.Prefix
					instPath = inst.Path
				}
			}
		}
	}

	// Fall back to active tower's hub prefix
	if prefix == "" {
		if tower, err := activeTowerConfig(); err == nil && tower != nil && tower.HubPrefix != "" {
			prefix = tower.HubPrefix
		}
	}

	// Still no prefix — helpful error
	if prefix == "" {
		return fmt.Errorf("no repo registered for this directory.\nRun `spire repo add` to register, or use `--prefix <name>`")
	}

	// If prefix was supplied explicitly, look up the instance path
	if instPath == "" {
		if cfg, err := loadConfig(); err == nil {
			if inst, ok := cfg.Instances[prefix]; ok {
				instPath = inst.Path
			}
		}
	}

	// Chdir to the instance path so bd can find .beads/redirect regardless
	// of where the caller (e.g. an AI agent) invoked spire from.
	if instPath != "" {
		os.Chdir(instPath) //nolint
	}

	// Parse remaining args into createOpts.
	// First positional arg is title; then flags: -t/--type, -p/--priority,
	// --label/--labels, --description, --parent.
	opts := createOpts{Prefix: prefix, Type: parseIssueType("task")}
	for i := 0; i < len(remaining); i++ {
		switch {
		case remaining[i] == "-t" || remaining[i] == "--type":
			if i+1 < len(remaining) {
				i++
				opts.Type = parseIssueType(remaining[i])
			}
		case remaining[i] == "-p" || remaining[i] == "--priority":
			if i+1 < len(remaining) {
				i++
				if p, perr := strconv.Atoi(remaining[i]); perr == nil {
					opts.Priority = p
				}
			}
		case remaining[i] == "--label" || remaining[i] == "--labels":
			if i+1 < len(remaining) {
				i++
				for _, l := range strings.Split(remaining[i], ",") {
					if l = strings.TrimSpace(l); l != "" {
						opts.Labels = append(opts.Labels, l)
					}
				}
			}
		case remaining[i] == "--description":
			if i+1 < len(remaining) {
				i++
				opts.Description = remaining[i]
			}
		case remaining[i] == "--parent":
			if i+1 < len(remaining) {
				i++
				opts.Parent = remaining[i]
			}
		default:
			if opts.Title == "" {
				opts.Title = remaining[i]
			}
		}
	}

	if opts.Title == "" {
		return fmt.Errorf("file: title is required")
	}

	id, err := storeCreateBead(opts)
	if err != nil {
		return fmt.Errorf("file: %w", err)
	}

	// Add branch and merge-mode labels if provided.
	if branch != "" {
		if lerr := storeAddLabel(id, "branch:"+branch); lerr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to add branch label: %v\n", lerr)
		}
	}
	if mergeMode != "" {
		if lerr := storeAddLabel(id, "merge-mode:"+mergeMode); lerr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to add merge-mode label: %v\n", lerr)
		}
	}
	// Link to a design bead via discovered-from dep if --ref was provided.
	if ref != "" {
		if derr := storeAddDepTyped(id, ref, "discovered-from"); derr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to link design bead %s: %v\n", ref, derr)
		}
	}

	fmt.Println(id)
	return nil
}
