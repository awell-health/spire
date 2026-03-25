package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func cmdFile(args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Println("usage: spire file <title> [--prefix <prefix>] [--branch <name>] [--merge-mode <merge|pr>] [bd create flags...]")
		return nil
	}

	// Extract spire-specific flags from args; pass everything else to bd create
	var prefix, branch, mergeMode string
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

	fmt.Println(id)
	return nil
}
