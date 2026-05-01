package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/awell-health/spire/pkg/store"
	"github.com/spf13/cobra"
)

// Test-replaceable seams for cmdFile (mirrors the alert.go pattern).
var fileCreateBead = storeCreateBead
var fileAddDepTyped = storeAddDepTyped
var fileAddLabel = storeAddLabel
var fileGetBead = storeGetBead

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
		if v, _ := cmd.Flags().GetString("design"); v != "" {
			fullArgs = append(fullArgs, "--design", v)
		}
		if v, _ := cmd.Flags().GetString("caused-by"); v != "" {
			fullArgs = append(fullArgs, "--caused-by", v)
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
	fileCmd.Flags().String("branch", "", "Base branch override for execution")
	fileCmd.Flags().String("merge-mode", "", "Merge mode: merge or pr")
	fileCmd.Flags().String("ref", "", "Bead ID to link via discovered-from dep (unvalidated)")
	fileCmd.Flags().String("design", "", "Design bead ID (validated: must be type=design)")
	fileCmd.Flags().String("caused-by", "", "Bead ID this bead was caused by (adds caused-by dep)")
	fileCmd.Flags().StringP("type", "t", "", "Bead type (task, bug, feature, epic, chore, recovery)")
	fileCmd.Flags().IntP("priority", "p", 0, "Priority (0-4)")
	fileCmd.Flags().String("label", "", "Comma-separated labels")
	fileCmd.Flags().String("description", "", "Bead description")
	fileCmd.Flags().String("parent", "", "Parent bead ID for hierarchical IDs")
}

func cmdFile(args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Println("usage: spire file <title> [--prefix <prefix>] [--branch <base-branch>] [--merge-mode <merge|pr>] [--ref <bead-id>] [--design <design-bead-id>] [--caused-by <bead-id>] [bd create flags...]")
		return nil
	}

	// Extract spire-specific flags from args; pass everything else to bd create
	var prefix, branch, mergeMode, ref, design, causedBy string
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
		case args[i] == "--design":
			if i+1 >= len(args) {
				return fmt.Errorf("--design requires a value")
			}
			i++
			design = args[i]
		case strings.HasPrefix(args[i], "--design="):
			design = strings.TrimPrefix(args[i], "--design=")
		case args[i] == "--caused-by":
			if i+1 >= len(args) {
				return fmt.Errorf("--caused-by requires a value")
			}
			i++
			causedBy = args[i]
		case strings.HasPrefix(args[i], "--caused-by="):
			causedBy = strings.TrimPrefix(args[i], "--caused-by=")
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

	// Validate --design and --caused-by referents BEFORE bead creation so a
	// failed lookup doesn't leave a half-created bead behind.
	if design != "" {
		b, err := fileGetBead(design)
		if err != nil {
			return fmt.Errorf("file: --design %s: %w", design, err)
		}
		if b.Type != "design" {
			return fmt.Errorf("file: --design %s: bead is type=%s, not design — use --ref for non-design discovered-from links", design, b.Type)
		}
	}
	if causedBy != "" {
		if _, err := fileGetBead(causedBy); err != nil {
			return fmt.Errorf("file: --caused-by %s: %w", causedBy, err)
		}
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
				typeStr := remaining[i]
				// Strict parse: surface typos like "-t taks" instead of
				// silently downgrading to task.
				t, err := store.ParseIssueType(typeStr)
				if err != nil {
					return err
				}
				// Reject internal bookkeeping types at the human-facing
				// CLI boundary — they're created by the executor
				// (formula pipeline), never by humans.
				if store.IsInternalType(t) {
					return fmt.Errorf("type %q is internal and only created by the executor (formula pipeline); cannot be filed via spire file", typeStr)
				}
				opts.Type = t
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

	id, err := fileCreateBead(opts)
	if err != nil {
		return fmt.Errorf("file: %w", err)
	}

	// Add branch and merge-mode labels if provided.
	if branch != "" {
		if lerr := fileAddLabel(id, "base-branch:"+branch); lerr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to add base-branch label: %v\n", lerr)
		}
	}
	if mergeMode != "" {
		if lerr := fileAddLabel(id, "merge-mode:"+mergeMode); lerr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to add merge-mode label: %v\n", lerr)
		}
	}
	// Link to a referenced bead via discovered-from dep if --ref was provided
	// (unvalidated escape hatch).
	if ref != "" {
		if derr := fileAddDepTyped(id, ref, "discovered-from"); derr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to link ref bead %s: %v\n", ref, derr)
		}
	}
	// Link to the design bead via discovered-from dep if --design was provided
	// (validated form: target was already confirmed type=design above).
	if design != "" {
		if derr := fileAddDepTyped(id, design, "discovered-from"); derr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to link design bead %s: %v\n", design, derr)
		}
	}
	// Caused-by uses strict failure semantics: if the dep can't be added,
	// surface the new bead ID (so the user can clean up) and return the error.
	if causedBy != "" {
		if derr := fileAddDepTyped(id, causedBy, store.DepCausedBy); derr != nil {
			fmt.Println(id)
			return fmt.Errorf("file: created bead %s but failed to add caused-by dep to %s: %w", id, causedBy, derr)
		}
	}

	fmt.Println(id)
	return nil
}
