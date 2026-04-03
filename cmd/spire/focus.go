package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/awell-health/spire/pkg/observability"
	"github.com/spf13/cobra"
	"github.com/steveyegge/beads"
)

var focusCmd = &cobra.Command{
	Use:   "focus <bead-id>",
	Short: "Assemble read-only context for a task",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmdFocus(args)
	},
}

func cmdFocus(args []string) error {
	if err := requireDolt(); err != nil {
		return err
	}

	if len(args) < 1 {
		return fmt.Errorf("usage: spire focus <bead-id>")
	}
	id := args[0]

	// 1. Fetch the target bead
	target, err := storeGetBead(id)
	if err != nil {
		return fmt.Errorf("focus %s: %w", id, err)
	}

	// 2. Determine current phase
	phase := getPhase(target)

	// 3. Try to load formula (optional — enriches context)
	formula, _ := ResolveFormula(target)

	// 4. Basic bead info (always shown)
	fmt.Printf("--- Task %s ---\n", target.ID)
	fmt.Printf("Title: %s\n", target.Title)
	fmt.Printf("Status: %s\n", target.Status)
	fmt.Printf("Priority: P%d\n", target.Priority)
	if phase != "" {
		fmt.Printf("Phase: %s\n", phase)
	}
	if target.Description != "" {
		fmt.Printf("Description: %s\n", target.Description)
	}
	fmt.Println()

	// 5. Show enabled phases from formula
	if formula != nil {
		enabled := formula.EnabledPhases()
		fmt.Printf("--- Workflow Phases ---\n")
		for _, p := range enabled {
			marker := "  "
			if p == phase {
				marker = "→ "
			}
			fmt.Printf("%s%s\n", marker, p)
		}
		fmt.Println()
	}

	// 6. Phase-specific context
	if formula != nil && phase != "" {
		if pc, ok := formula.Phases[phase]; ok {
			if len(pc.Context) > 0 {
				fmt.Printf("--- Phase Context (%s) ---\n", phase)
				fmt.Printf("Context paths: %s\n", strings.Join(pc.Context, ", "))
				if pc.Timeout != "" {
					fmt.Printf("Timeout: %s\n", pc.Timeout)
				}
				if pc.Model != "" {
					fmt.Printf("Model: %s\n", pc.Model)
				}
				fmt.Println()
			}
		}
	}

	// 7a. Recovery work section for interrupted beads.
	isInterrupted := false
	for _, l := range target.Labels {
		if strings.HasPrefix(l, "interrupted:") {
			isInterrupted = true
			break
		}
	}
	if isInterrupted {
		dependents, dErr := storeGetDependentsWithMeta(id)
		if dErr == nil {
			for _, dep := range dependents {
				if string(dep.DependencyType) != "recovery-for" {
					continue
				}
				if string(dep.Status) == "closed" {
					continue
				}
				fmt.Printf("--- Recovery work ---\n")
				fmt.Printf("  %s  %s  (%s)\n", dep.ID, dep.Title, dep.Status)
				fmt.Println()
				break // only show first open recovery bead
			}
		}
	}

	// 7b. Related beads (from dependency graph)
	relDeps, relErr := storeGetDepsWithMeta(id)
	if relErr != nil {
		fmt.Fprintf(os.Stderr, "warning: could not load related deps for %s: %v\n", id, relErr)
	}
	for _, dep := range relDeps {
		if dep.DependencyType == beads.DepParentChild {
			continue // parent-child shown elsewhere
		}
		fmt.Printf("--- %s: %s ---\n", dep.DependencyType, dep.ID)
		fmt.Printf("Title: %s\n", dep.Title)
		fmt.Printf("Status: %s\n", dep.Status)
		if dep.Description != "" {
			fmt.Printf("Description: %s\n", dep.Description)
		}
		fmt.Println()
	}

	// 8. Messages referencing this bead
	referrers, _ := storeListBeads(beads.IssueFilter{IDPrefix: "spi-", Labels: []string{"msg", "ref:" + id}, Status: statusPtr(beads.StatusOpen)})
	for _, m := range referrers {
		from := hasLabel(m, "from:")
		fmt.Printf("--- Referenced by %s ---\n", m.ID)
		if from != "" {
			fmt.Printf("From: %s\n", from)
		}
		fmt.Printf("Subject: %s\n", m.Title)
		fmt.Println()
	}

	// 9. Thread context (parent + siblings)
	if target.Parent != "" {
		parentBead, parentErr := storeGetBead(target.Parent)
		if parentErr == nil {
			fmt.Printf("--- Thread (parent: %s) ---\n", parentBead.ID)
			fmt.Printf("Subject: %s\n", parentBead.Title)

			siblings, _ := storeGetChildren(target.Parent)
			for _, s := range siblings {
				if s.ID == target.ID {
					continue
				}
				from := hasLabel(s, "from:")
				fmt.Printf("  %s [%s]: %s\n", s.ID, from, s.Title)
			}
			fmt.Println()
		}
	}

	// 10. Comments
	comments, commErr := storeGetComments(id)
	if commErr == nil && len(comments) > 0 {
		fmt.Printf("--- Comments (%d) ---\n", len(comments))
		for _, c := range comments {
			if c.Author != "" {
				fmt.Printf("[%s]: %s\n", c.Author, c.Text)
			} else {
				fmt.Println(c.Text)
			}
		}
		fmt.Println()
	}

	// 11. Agent runs
	runs, runErr := observability.RunsForBead(id)
	if runErr == nil && len(runs) > 0 {
		fmt.Printf("--- Agent Runs (%d) ---\n", len(runs))
		for _, r := range runs {
			model := observability.ToString(r["model"])
			role := observability.ToString(r["role"])
			result := observability.ToString(r["result"])
			dur := observability.ToInt(r["duration_seconds"])
			started := observability.ToString(r["started_at"])
			fmt.Printf("  %-20s %-10s %-10s %4ds  %s\n", model, role, result, dur, started)
		}
		fmt.Println()
	}

	return nil
}
