package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

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
link it with ref: to the design bead.

Workflow:
  spire design "Auth system overhaul"     # create design bead
  # brainstorm, add comments, iterate...
  bd comments add spi-xxx "approach A: ..."
  bd comments add spi-xxx "rejected because ..."
  # when ready to commit:
  spire file "Auth overhaul" -t epic -p 1 --label "ref:spi-xxx"`)
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

	// Detect prefix from CWD
	if cwd, err := realCwd(); err == nil {
		if cfg, err := loadConfig(); err == nil {
			if inst := findInstanceByPath(cfg, cwd); inst != nil {
				opts.Prefix = inst.Prefix
			}
		}
	}
	if opts.Prefix == "" {
		cfg, _ := loadConfig()
		var prefixes []string
		for p := range cfg.Instances {
			prefixes = append(prefixes, p)
		}
		return fmt.Errorf("--prefix required (registered: %s)", strings.Join(prefixes, ", "))
	}

	id, err := storeCreateBead(opts)
	if err != nil {
		return fmt.Errorf("design: %w", err)
	}

	fmt.Println(id)
	return nil
}
