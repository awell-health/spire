package main

import (
	"fmt"
	"os"

	"github.com/awell-health/spire/pkg/integration"
	"github.com/awell-health/spire/pkg/observability"
	"github.com/spf13/cobra"
	"github.com/steveyegge/beads"
)

// LinearIssue is a type alias for integration.LinearIssue, kept for backward
// compatibility with callers in cmd/spire that reference the type.
type LinearIssue = integration.LinearIssue

var grokCmd = &cobra.Command{
	Use:   "grok <bead-id>",
	Short: "Focus + live Linear context",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmdGrok(args)
	},
}

// linearAPIKey delegates to pkg/integration.
func linearAPIKey() string {
	return integration.LinearAPIKey()
}

// linearGraphQLURL, linearAuthHeader removed — no callers in cmd/spire.

// fetchLinearIssue delegates to pkg/integration.
func fetchLinearIssue(apiKey, identifier string) (*LinearIssue, error) {
	return integration.FetchLinearIssue(apiKey, identifier)
}

// printLinearContext delegates to pkg/integration.
func printLinearContext(issue *LinearIssue) {
	integration.PrintLinearContext(issue)
}

func cmdGrok(args []string) error {
	if err := requireDolt(); err != nil {
		return err
	}

	if len(args) < 1 {
		return fmt.Errorf("usage: spire grok <bead-id>")
	}
	id := args[0]

	// --- Bead-local context (same as focus) ---

	// 1. Fetch the target bead
	target, err := storeGetBead(id)
	if err != nil {
		return fmt.Errorf("grok %s: %w", id, err)
	}

	// 2. Check if a molecule already exists (don't pour -- grok is read-only)
	var molID string
	existingMols, _ := storeListBeads(beads.IssueFilter{IDPrefix: "spi-", Labels: []string{"workflow:" + id}, Status: statusPtr(beads.StatusOpen)})
	if len(existingMols) > 0 {
		molID = existingMols[0].ID
	}

	// 3. Get molecule progress if available
	var progressOut string
	if molID != "" {
		progressOut, _ = bd("mol", "progress", molID)
	}

	// 4. Assemble bead-local output
	fmt.Printf("--- Task %s ---\n", target.ID)
	fmt.Printf("Title: %s\n", target.Title)
	fmt.Printf("Status: %s\n", target.Status)
	fmt.Printf("Priority: P%d\n", target.Priority)
	if target.Description != "" {
		fmt.Printf("Description: %s\n", target.Description)
	}
	fmt.Println()

	// Workflow progress
	if progressOut != "" {
		fmt.Println("--- Workflow (spire-agent-work) ---")
		fmt.Println(progressOut)
		fmt.Println()
	}

	// Related beads (from dependency graph)
	relDeps, _ := storeGetDepsWithMeta(id)
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

	// Messages that reference this bead
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

	// Thread context (parent + siblings)
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

	// Comments
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

	// Agent runs
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

	// --- Linear enrichment ---

	linearID := hasLabel(target, "linear:")
	if linearID == "" {
		// No linear: label -- nothing to enrich, done.
		return nil
	}

	apiKey := linearAPIKey()
	if apiKey == "" {
		fmt.Fprintf(os.Stderr, "spire: warning: LINEAR_API_KEY not set, skipping Linear enrichment\n")
		return nil
	}

	issue, err := fetchLinearIssue(apiKey, linearID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "spire: warning: Linear API error: %s\n", err)
		return nil
	}

	if issue == nil {
		fmt.Fprintf(os.Stderr, "spire: warning: Linear issue %s not found\n", linearID)
		return nil
	}

	printLinearContext(issue)

	return nil
}
