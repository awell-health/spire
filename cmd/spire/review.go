package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/awell-health/spire/pkg/formula"
	"github.com/spf13/cobra"
)

var reviewCmd = &cobra.Command{
	Use:   "review <bead-id>",
	Short: "Assemble review context from bead commit history",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmdReview(args)
	},
}

func cmdReview(args []string) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}
	if err := requireDolt(); err != nil {
		return err
	}

	id := args[0]

	// 1. Fetch the target bead.
	target, err := storeGetBead(id)
	if err != nil {
		return fmt.Errorf("review %s: %w", id, err)
	}

	// 2. Resolve formula for step graph.
	anyFormula, _, _ := ResolveFormulaAny(target)
	var graph *formula.FormulaStepGraph
	if anyFormula != nil {
		graph, _ = anyFormula.(*formula.FormulaStepGraph)
	}

	// --- Header ---
	fmt.Printf("--- Task %s ---\n", target.ID)
	fmt.Printf("Title: %s\n", target.Title)
	fmt.Printf("Status: %s\n", target.Status)
	fmt.Printf("Priority: P%d\n", target.Priority)
	fmt.Printf("Type: %s\n", target.Type)
	if graph != nil {
		fmt.Printf("Formula: %s\n", graph.Name)
	}
	if target.Description != "" {
		fmt.Printf("Description: %s\n", target.Description)
	}
	fmt.Println()

	// 3. Parse metadata.commits — JSON-encoded []string of SHAs.
	var commits []string
	rawCommits := target.Meta("commits")
	if rawCommits != "" {
		if err := json.Unmarshal([]byte(rawCommits), &commits); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not parse metadata.commits: %v\n", err)
		}
	}

	// 4. Resolve repo for git operations.
	var repoPath string
	if len(commits) > 0 {
		rp, _, _, rerr := wizardResolveRepo(id)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not resolve repo for %s: %v\n", id, rerr)
		} else {
			repoPath = rp
		}
	}

	if len(commits) == 0 {
		fmt.Println("--- Commits ---")
		fmt.Println("No commits recorded.")
		fmt.Println()
	} else if repoPath == "" {
		fmt.Println("--- Commits ---")
		fmt.Printf("Commits recorded but repo not locally available: %s\n", strings.Join(commits, ", "))
		fmt.Println()
	} else {
		// Filter to reachable commits.
		reachable := filterReachableCommits(repoPath, commits)

		// --- Commits section ---
		fmt.Println("--- Commits ---")
		if len(reachable) == 0 {
			fmt.Println("All recorded commits are unreachable (force-pushed or rebased).")
		} else {
			gitArgs := []string{"-C", repoPath, "log", "--no-walk", "--format=%h %s (%an, %ar)"}
			gitArgs = append(gitArgs, reachable...)
			out, err := exec.Command("git", gitArgs...).CombinedOutput()
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: git log failed: %v\n", err)
			} else {
				fmt.Print(string(out))
			}
		}
		if len(reachable) < len(commits) {
			skipped := len(commits) - len(reachable)
			fmt.Fprintf(os.Stderr, "warning: %d commit(s) unreachable (force-pushed or rebased)\n", skipped)
		}
		fmt.Println()

		// --- Diff Stats section ---
		if len(reachable) > 0 {
			fmt.Println("--- Diff Stats ---")
			statsOut, statsErr := reviewDiff(repoPath, reachable, true)
			if statsErr == nil {
				fmt.Print(statsOut)
			} else {
				fmt.Fprintf(os.Stderr, "warning: diff stats failed: %v\n", statsErr)
			}
			fmt.Println()
		}

		// --- Diff section ---
		if len(reachable) > 0 {
			fmt.Println("--- Diff ---")
			diffOut, diffErr := reviewDiff(repoPath, reachable, false)
			if diffErr == nil {
				fmt.Print(diffOut)
			} else {
				fmt.Fprintf(os.Stderr, "warning: diff failed: %v\n", diffErr)
			}
			fmt.Println()
		}
	}

	// --- Review History ---
	reviews, revErr := storeGetReviewBeads(id)
	if revErr == nil && len(reviews) > 0 {
		fmt.Printf("--- Review History (%d rounds) ---\n", len(reviews))
		for _, rb := range reviews {
			rn := reviewRoundNumber(rb)
			verdict := reviewBeadVerdict(rb)
			sage := hasLabel(rb, "sage:")
			fmt.Printf("  Round %d: verdict=%s, sage=%s, status=%s\n", rn, verdict, sage, rb.Status)
			if rb.Description != "" {
				fmt.Printf("    %s\n", rb.Description)
			}
		}
		fmt.Println()
	}

	// Shared tail sections (deps, messages, comments, agent runs).
	focusTail(target, id)

	return nil
}

// filterReachableCommits returns the subset of SHAs that exist in the repo.
func filterReachableCommits(repoPath string, shas []string) []string {
	var reachable []string
	for _, sha := range shas {
		err := exec.Command("git", "-C", repoPath, "cat-file", "-e", sha).Run()
		if err == nil {
			reachable = append(reachable, sha)
		}
	}
	return reachable
}

// reviewDiff produces a combined diff for the given commits using per-commit
// git show. This approach is universally correct regardless of commit ancestry,
// root commits, rebases, or non-contiguous SHAs.
// If statsOnly is true, it produces --stat output instead.
func reviewDiff(repoPath string, commits []string, statsOnly bool) (string, error) {
	var buf strings.Builder
	for _, sha := range commits {
		args := []string{"-C", repoPath, "show", "--format="}
		if statsOnly {
			args = append(args, "--stat")
		}
		args = append(args, sha)
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			buf.WriteString(fmt.Sprintf("# commit %s: git show failed: %v\n", sha, err))
			continue
		}
		if len(commits) > 1 && !statsOnly {
			buf.WriteString(fmt.Sprintf("# commit %s\n", sha))
		}
		buf.Write(out)
	}
	return buf.String(), nil
}
