package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"

	"github.com/awell-health/spire/pkg/repoconfig"
)

// createOrUpdatePR creates or updates a GitHub PR for an approved child bead.
// Returns the PR number.
func createOrUpdatePR(dir string, child Bead, branch string, review *Review, cfg *repoconfig.RepoConfig) (int, error) {
	// Check if a PR already exists for this branch.
	existing, err := findExistingPR(dir, branch)
	if err != nil {
		log.Printf("[artificer] warning: could not check for existing PR: %v", err)
	}

	if existing > 0 {
		// Update existing PR body.
		body := buildPRBody(child, review, "")
		if err := ghCmd(dir, "pr", "edit", fmt.Sprintf("%d", existing), "--body", body); err != nil {
			return existing, fmt.Errorf("update PR #%d: %w", existing, err)
		}
		log.Printf("[artificer] updated PR #%d for %s", existing, child.ID)
		return existing, nil
	}

	// Create new PR.
	title := fmt.Sprintf("feat(%s): %s", child.ID, child.Title)
	if len(title) > 72 {
		title = title[:69] + "..."
	}

	body := buildPRBody(child, review, "")
	base := resolveTargetBranch(&child, nil, cfg)

	args := []string{
		"pr", "create",
		"--head", branch,
		"--base", base,
		"--title", title,
		"--body", body,
	}

	// Add labels.
	labels := append([]string{}, cfg.PR.Labels...)
	labels = append(labels, "artificer-approved")
	for _, l := range labels {
		args = append(args, "--label", l)
	}

	// Add reviewers.
	for _, r := range cfg.PR.Reviewers {
		args = append(args, "--reviewer", r)
	}

	out, err := ghOutput(dir, args...)
	if err != nil {
		return 0, fmt.Errorf("create PR for %s: %w", child.ID, err)
	}

	// gh pr create outputs the PR URL. Extract the number.
	prNum := extractPRNumber(out)
	log.Printf("[artificer] created PR #%d for %s: %s", prNum, child.ID, out)

	// Enable auto-merge if configured.
	if cfg.PR.AutoMerge && prNum > 0 {
		if err := ghCmd(dir, "pr", "merge", fmt.Sprintf("%d", prNum), "--auto", "--squash"); err != nil {
			log.Printf("[artificer] warning: could not enable auto-merge on PR #%d: %v", prNum, err)
		}
	}

	return prNum, nil
}

// mergePR merges a PR via gh, squashing and deleting the branch.
func mergePR(dir string, prNumber int) error {
	return ghCmd(dir, "pr", "merge", fmt.Sprintf("%d", prNumber), "--squash", "--delete-branch")
}

// buildPRBody constructs the markdown body for a PR.
func buildPRBody(child Bead, review *Review, epicID string) string {
	var b strings.Builder

	b.WriteString("## Summary\n\n")
	if review != nil {
		b.WriteString(review.Summary)
	} else {
		b.WriteString(child.Title)
	}
	b.WriteString("\n\n")

	b.WriteString(fmt.Sprintf("**Bead**: `%s`\n", child.ID))
	if epicID != "" {
		b.WriteString(fmt.Sprintf("**Epic**: `%s`\n", epicID))
	}
	b.WriteString("\n")

	if review != nil && len(review.Issues) > 0 {
		b.WriteString("## Review Notes\n\n")
		for _, issue := range review.Issues {
			severity := issue.Severity
			if severity == "" {
				severity = "info"
			}
			if issue.Line > 0 {
				b.WriteString(fmt.Sprintf("- **%s** `%s:%d` — %s\n", severity, issue.File, issue.Line, issue.Message))
			} else {
				b.WriteString(fmt.Sprintf("- **%s** `%s` — %s\n", severity, issue.File, issue.Message))
			}
		}
		b.WriteString("\n")
	}

	b.WriteString("---\n")
	b.WriteString("Reviewed by the Artificer (Opus)\n")

	return b.String()
}

// findExistingPR checks if a PR already exists for the given branch.
// Returns the PR number or 0 if none found.
func findExistingPR(dir, branch string) (int, error) {
	out, err := ghOutput(dir, "pr", "list", "--head", branch, "--json", "number", "--jq", ".[0].number")
	if err != nil {
		return 0, err
	}
	out = strings.TrimSpace(out)
	if out == "" || out == "null" {
		return 0, nil
	}

	var num int
	if _, err := fmt.Sscanf(out, "%d", &num); err != nil {
		return 0, nil
	}
	return num, nil
}

// extractPRNumber extracts a PR number from a gh pr create output URL.
// e.g., "https://github.com/org/repo/pull/42" → 42
func extractPRNumber(output string) int {
	output = strings.TrimSpace(output)
	parts := strings.Split(output, "/")
	if len(parts) == 0 {
		return 0
	}
	var num int
	fmt.Sscanf(parts[len(parts)-1], "%d", &num)
	return num
}

// ghCmd runs a gh command in the given directory.
func ghCmd(dir string, args ...string) error {
	cmd := exec.Command("gh", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh %s: %w\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return nil
}

// ghOutput runs a gh command and returns its trimmed stdout.
func ghOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("gh", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gh %s: %w\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

// ghJSON runs a gh command with JSON output and unmarshals the result.
func ghJSON(dir string, result any, args ...string) error {
	out, err := ghOutput(dir, args...)
	if err != nil {
		return err
	}
	if out == "" {
		return nil
	}
	return json.Unmarshal([]byte(out), result)
}
