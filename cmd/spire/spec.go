package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var specCmd = &cobra.Command{
	Use:   "spec <title> [flags]",
	Short: "Scaffold a spec and file it (--no-file, --break <id>)",
	RunE: func(cmd *cobra.Command, args []string) error {
		var fullArgs []string
		if v, _ := cmd.Flags().GetString("type"); v != "" {
			fullArgs = append(fullArgs, "-t", v)
		}
		if cmd.Flags().Changed("priority") {
			p, _ := cmd.Flags().GetInt("priority")
			fullArgs = append(fullArgs, "-p", strconv.Itoa(p))
		}
		if noFile, _ := cmd.Flags().GetBool("no-file"); noFile {
			fullArgs = append(fullArgs, "--no-file")
		}
		if v, _ := cmd.Flags().GetString("break"); v != "" {
			fullArgs = append(fullArgs, "--break", v)
		}
		if v, _ := cmd.Flags().GetString("dir"); v != "" {
			fullArgs = append(fullArgs, "--dir", v)
		}
		fullArgs = append(fullArgs, args...)
		return cmdSpec(fullArgs)
	},
}

func init() {
	specCmd.Flags().StringP("type", "t", "epic", "Bead type")
	specCmd.Flags().IntP("priority", "p", 2, "Priority (0-4)")
	specCmd.Flags().Bool("no-file", false, "Don't create a bead, just write the spec file")
	specCmd.Flags().String("break", "", "Bead ID to break down")
	specCmd.Flags().String("dir", "", "Output directory for spec file")
}

var specTemplate = `# Spec: %s

**Date:** %s
**Bead:** %s
**Status:** Draft

## Problem

What problem does this solve? Who is affected?

## Design

How should this be built? What are the key decisions?

## Changes

### 1. <First change>

Description and implementation details.

### 2. <Second change>

Description and implementation details.

## Testing

How do you verify this works?

## Implementation order

1. First step
2. Second step
3. Third step
`

func slugify(title string) string {
	s := strings.ToLower(title)
	// Replace spaces and underscores with hyphens
	s = strings.ReplaceAll(s, " ", "-")
	s = strings.ReplaceAll(s, "_", "-")
	// Strip non-alphanumeric (keep hyphens)
	re := regexp.MustCompile(`[^a-z0-9-]`)
	s = re.ReplaceAllString(s, "")
	// Collapse multiple hyphens
	re2 := regexp.MustCompile(`-{2,}`)
	s = re2.ReplaceAllString(s, "-")
	// Trim leading/trailing hyphens
	s = strings.Trim(s, "-")
	return s
}

func cmdSpec(args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Println(`usage: spire spec <title> [flags]
       spire spec --break <bead-id>

Scaffold a design spec and optionally file it as a bead.

Flags:
  -t <type>       Bead type (default: epic)
  -p <priority>   Priority (default: 2)
  --no-file       Create spec file only, don't file a bead
  --break <id>    Break an existing spec's Implementation order into child beads
  --dir <path>    Custom output directory (default: docs/superpowers/specs/)`)
		return nil
	}

	// Parse flags
	var (
		title    string
		beadType = "epic"
		priority = "2"
		noFile   bool
		breakID  string
		dir      = "docs/superpowers/specs"
	)

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--no-file":
			noFile = true
		case args[i] == "--break":
			if i+1 >= len(args) {
				return fmt.Errorf("--break requires a bead ID")
			}
			i++
			breakID = args[i]
		case args[i] == "-t":
			if i+1 >= len(args) {
				return fmt.Errorf("-t requires a type value")
			}
			i++
			beadType = args[i]
		case args[i] == "-p":
			if i+1 >= len(args) {
				return fmt.Errorf("-p requires a priority value")
			}
			i++
			priority = args[i]
		case args[i] == "--dir":
			if i+1 >= len(args) {
				return fmt.Errorf("--dir requires a path")
			}
			i++
			dir = args[i]
		case strings.HasPrefix(args[i], "--dir="):
			dir = strings.TrimPrefix(args[i], "--dir=")
		case strings.HasPrefix(args[i], "-t="):
			beadType = strings.TrimPrefix(args[i], "-t=")
		case strings.HasPrefix(args[i], "-p="):
			priority = strings.TrimPrefix(args[i], "-p=")
		default:
			if title == "" {
				title = args[i]
			} else {
				return fmt.Errorf("unexpected argument: %s", args[i])
			}
		}
	}

	// Handle --break flow
	if breakID != "" {
		return specBreak(breakID, dir)
	}

	if title == "" {
		return fmt.Errorf("title is required")
	}

	date := time.Now().Format("2006-01-02")
	slug := slugify(title)
	filename := fmt.Sprintf("%s-%s.md", date, slug)
	specPath := filepath.Join(dir, filename)

	// Create directory if needed
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}

	// File a bead (unless --no-file)
	beadID := "(none)"
	if !noFile {
		pri := 2
		if p, err := fmt.Sscanf(priority, "%d", &pri); p == 0 || err != nil {
			pri = 2
		}
		id, err := storeCreateBead(createOpts{
			Title:    title,
			Type:     parseIssueType(beadType),
			Priority: pri,
		})
		if err != nil {
			return fmt.Errorf("file bead: %w", err)
		}
		beadID = id
	}

	// Write the spec file
	content := fmt.Sprintf(specTemplate, title, date, beadID)
	if err := os.WriteFile(specPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("write spec: %w", err)
	}

	fmt.Printf("spec: %s\n", specPath)
	if beadID != "(none)" {
		fmt.Printf("bead: %s\n", beadID)
	}

	return nil
}

// specBreak reads a spec file associated with a bead and creates child beads
// from the "Implementation order" section.
func specBreak(beadID string, dir string) error {
	// Find the spec file containing this bead ID
	specPath, err := findSpecByBead(beadID, dir)
	if err != nil {
		return fmt.Errorf("find spec for %s: %w", beadID, err)
	}

	// Read the spec file
	f, err := os.Open(specPath)
	if err != nil {
		return fmt.Errorf("open spec: %w", err)
	}
	defer f.Close()

	// Parse the Implementation order section
	steps := parseImplementationOrder(f)
	if len(steps) == 0 {
		return fmt.Errorf("no numbered items found in Implementation order section of %s", specPath)
	}

	fmt.Printf("Breaking %s into %d child beads:\n", beadID, len(steps))

	for _, step := range steps {
		id, err := storeCreateBead(createOpts{
			Title:    step,
			Type:     parseIssueType("task"),
			Priority: 2,
			Parent:   beadID,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "  warning: failed to create bead for %q: %s\n", step, err)
			continue
		}
		fmt.Printf("  %s  %s\n", id, step)
	}

	return nil
}

// findSpecByBead searches the specs directory for a file containing **Bead:** <beadID>.
func findSpecByBead(beadID string, dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("read directory %s: %w", dir, err)
	}

	needle := fmt.Sprintf("**Bead:** %s", beadID)

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		if strings.Contains(string(data), needle) {
			return path, nil
		}
	}

	return "", fmt.Errorf("no spec file found containing %q in %s", needle, dir)
}

// parseImplementationOrder reads a spec file and extracts numbered items
// from the "Implementation order" section.
func parseImplementationOrder(f *os.File) []string {
	scanner := bufio.NewScanner(f)
	inSection := false
	var steps []string

	// Match lines like "1. First step" or "2. Second step"
	numberedRe := regexp.MustCompile(`^\d+\.\s+(.+)`)

	for scanner.Scan() {
		line := scanner.Text()

		// Detect the Implementation order heading
		if strings.HasPrefix(line, "## Implementation order") {
			inSection = true
			continue
		}

		// If we hit another heading, stop
		if inSection && strings.HasPrefix(line, "## ") {
			break
		}

		if inSection {
			if m := numberedRe.FindStringSubmatch(line); m != nil {
				steps = append(steps, m[1])
			}
		}
	}

	return steps
}
