package main

import (
	"bytes"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/repoconfig"
)

// TestResult captures the output of running lint/build/test commands.
type TestResult struct {
	Passed   bool          `json:"passed"`
	Output   string        `json:"output"`
	Stage    string        `json:"stage"` // "lint", "build", "test"
	Duration time.Duration `json:"duration"`
}

// processMergeQueue merges approved PRs in dependency order, testing after each.
func processMergeQueue(dir string, states map[string]*ChildState, cfg *repoconfig.RepoConfig, epicID string) error {
	// Collect children that are approved and in the merge queue.
	var ready []string
	for id, cs := range states {
		if cs.InMergeQueue && cs.Verdict == "approved" && cs.PRNumber > 0 {
			ready = append(ready, id)
		}
	}

	if len(ready) == 0 {
		return nil
	}

	// Sort by dependency order.
	ordered, err := getDependencyOrder(epicID, ready)
	if err != nil {
		log.Printf("[artificer] warning: could not resolve dependencies, using original order: %v", err)
		ordered = ready
	}

	base := cfg.Branch.Base

	for _, childID := range ordered {
		cs := states[childID]
		branch := cs.Branch

		log.Printf("[artificer] merge queue: attempting %s (PR #%d)", childID, cs.PRNumber)

		// Checkout base and pull latest.
		if err := gitCheckoutBase(dir, base); err != nil {
			log.Printf("[artificer] failed to checkout base for merge: %v", err)
			continue
		}

		// Attempt merge.
		mergeMsg := fmt.Sprintf("Merge feat/%s: %s", childID, childID)
		if err := gitMerge(dir, branch, mergeMsg); err != nil {
			log.Printf("[artificer] merge conflict on %s: %v", childID, err)
			gitRevertMerge(dir) //nolint:errcheck
			cs.InMergeQueue = false
			// Notify wizard of conflict.
			spireSend(resolveWizardAgent(Bead{ID: childID}),
				fmt.Sprintf("Merge conflict on feat/%s — please rebase on %s and push again", childID, base),
				childID, 1) //nolint:errcheck
			continue
		}

		// Run tests on merged result.
		testResult := runTests(dir, base, cfg)
		if !testResult.Passed {
			log.Printf("[artificer] tests failed after merging %s: %s", childID, testResult.Stage)
			gitRevertMerge(dir) //nolint:errcheck
			cs.InMergeQueue = false
			spireSend(resolveWizardAgent(Bead{ID: childID}),
				fmt.Sprintf("Tests failed after merging feat/%s during %s. Please fix and push again.\n%s",
					childID, testResult.Stage, truncate(testResult.Output, 2000)),
				childID, 1) //nolint:errcheck
			continue
		}

		// Push merged base.
		if err := gitPush(dir, base); err != nil {
			log.Printf("[artificer] failed to push merged base: %v", err)
			gitRevertMerge(dir) //nolint:errcheck
			continue
		}

		// Merge the PR via GitHub (marks it as merged, deletes branch).
		if err := mergePR(dir, cs.PRNumber); err != nil {
			log.Printf("[artificer] warning: gh pr merge failed (branch already merged): %v", err)
		}

		cs.Verdict = "merged"
		cs.InMergeQueue = false
		log.Printf("[artificer] merged %s (PR #%d)", childID, cs.PRNumber)
	}

	return nil
}

// getDependencyOrder returns the given bead IDs sorted by their dependency graph.
// Children without dependencies come first.
func getDependencyOrder(epicID string, beadIDs []string) ([]string, error) {
	// Build the dependency map by querying each bead.
	deps := make(map[string][]string)
	idSet := make(map[string]bool)
	for _, id := range beadIDs {
		idSet[id] = true
		deps[id] = nil
	}

	// Query dependencies for each bead.
	for _, id := range beadIDs {
		out, err := bd("dep", "list", id)
		if err != nil {
			continue // No deps or command not available.
		}
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if line != "" && idSet[line] {
				deps[id] = append(deps[id], line)
			}
		}
	}

	return topologicalSort(deps), nil
}

// topologicalSort performs Kahn's algorithm on a dependency graph.
// deps maps node -> list of nodes it depends on (must complete before it).
func topologicalSort(deps map[string][]string) []string {
	// Build in-degree map.
	inDegree := make(map[string]int)
	dependents := make(map[string][]string) // reverse: who depends on me

	for node := range deps {
		if _, ok := inDegree[node]; !ok {
			inDegree[node] = 0
		}
		for _, dep := range deps[node] {
			inDegree[node]++
			dependents[dep] = append(dependents[dep], node)
			if _, ok := inDegree[dep]; !ok {
				inDegree[dep] = 0
			}
		}
	}

	// Start with nodes that have no dependencies.
	var queue []string
	for node, deg := range inDegree {
		if deg == 0 {
			queue = append(queue, node)
		}
	}

	var result []string
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		result = append(result, node)

		for _, dependent := range dependents[node] {
			inDegree[dependent]--
			if inDegree[dependent] == 0 {
				queue = append(queue, dependent)
			}
		}
	}

	// If there's a cycle, append remaining nodes anyway.
	if len(result) < len(deps) {
		for node := range deps {
			found := false
			for _, r := range result {
				if r == node {
					found = true
					break
				}
			}
			if !found {
				result = append(result, node)
			}
		}
	}

	return result
}

// runTests runs the lint, build, and test commands from the repo config.
// Tests are run on whatever is currently checked out.
func runTests(dir, branch string, cfg *repoconfig.RepoConfig) *TestResult {
	start := time.Now()

	// Install dependencies if needed.
	if cfg.Runtime.Install != "" {
		if out, err := shellCmd(dir, cfg.Runtime.Install); err != nil {
			return &TestResult{
				Passed:   false,
				Output:   out,
				Stage:    "install",
				Duration: time.Since(start),
			}
		}
	}

	// Lint.
	if cfg.Runtime.Lint != "" {
		if out, err := shellCmd(dir, cfg.Runtime.Lint); err != nil {
			return &TestResult{
				Passed:   false,
				Output:   out,
				Stage:    "lint",
				Duration: time.Since(start),
			}
		}
	}

	// Build.
	if cfg.Runtime.Build != "" {
		if out, err := shellCmd(dir, cfg.Runtime.Build); err != nil {
			return &TestResult{
				Passed:   false,
				Output:   out,
				Stage:    "build",
				Duration: time.Since(start),
			}
		}
	}

	// Test.
	if cfg.Runtime.Test != "" {
		if out, err := shellCmd(dir, cfg.Runtime.Test); err != nil {
			return &TestResult{
				Passed:   false,
				Output:   out,
				Stage:    "test",
				Duration: time.Since(start),
			}
		}
	}

	return &TestResult{
		Passed:   true,
		Stage:    "all",
		Duration: time.Since(start),
	}
}

// shellCmd runs a shell command string in the given directory and returns
// combined stdout+stderr output.
func shellCmd(dir, command string) (string, error) {
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = dir
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	return output.String(), err
}
