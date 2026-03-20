package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/metrics"
)

// sendTestFailure notifies the wizard that tests failed on their branch.
func sendTestFailure(child Bead, result *TestResult) error {
	msg := fmt.Sprintf("Tests failed on feat/%s during %s. Output:\n%s",
		child.ID, result.Stage, truncate(result.Output, 2000))

	agent := resolveWizardAgent(child)
	return spireSend(agent, msg, child.ID, 1)
}

// sendReviewToWizard sends structured review feedback to the wizard.
func sendReviewToWizard(child Bead, review *Review) error {
	reviewJSON, _ := json.MarshalIndent(review, "", "  ")
	msg := fmt.Sprintf("Review for feat/%s: %s\n\n%s",
		child.ID, review.Verdict, string(reviewJSON))

	agent := resolveWizardAgent(child)
	return spireSend(agent, msg, child.ID, 1)
}

// escalateToHuman sends an escalation message when review rounds are exhausted.
func escalateToHuman(child Bead, review *Review, rounds int) error {
	msg := fmt.Sprintf("Escalation: %s (%s) failed after %d review rounds. Latest issues:\n%s",
		child.ID, child.Title, rounds, review.Summary)

	// Escalate to steward (who routes to human).
	if err := spireSend("steward", msg, child.ID, 0); err != nil {
		return err
	}

	// Also add a comment to the bead.
	return bdComment(child.ID, fmt.Sprintf("Escalated after %d review rounds. Summary: %s", rounds, review.Summary))
}

// reportToSteward sends a rejection report.
func reportToSteward(child Bead, review *Review) error {
	msg := fmt.Sprintf("Rejected: %s (%s). %s", child.ID, child.Title, review.Summary)
	return spireSend("steward", msg, child.ID, 1)
}

// recordRun records an agent run metric for an artificer review.
func recordRun(child Bead, epicID, model string, result string, review *Review, usage tokenUsage, diffStats [3]int) error {
	run := metrics.AgentRun{
		ID:               metrics.GenerateID(),
		BeadID:           child.ID,
		EpicID:           epicID,
		Model:            model,
		Role:             "artificer",
		Result:           result,
		ContextTokensIn:  usage.InputTokens,
		ContextTokensOut: usage.OutputTokens,
		TotalTokens:      usage.InputTokens + usage.OutputTokens,
		StartedAt:        time.Now().UTC().Format(time.RFC3339),
		CompletedAt:      time.Now().UTC().Format(time.RFC3339),
		FilesChanged:     diffStats[0],
		LinesAdded:       diffStats[1],
		LinesRemoved:     diffStats[2],
	}

	if review != nil {
		run.ArtificerVerdict = review.Verdict
	}

	if err := metrics.Record(run); err != nil {
		log.Printf("[artificer] failed to record metric: %v", err)
		return err
	}
	return nil
}

// reportEpicProgress adds a summary comment to the epic bead.
func reportEpicProgress(epicID string, states map[string]*ChildState) error {
	var pending, reviewing, approved, merged, rejected int
	for _, cs := range states {
		switch cs.Verdict {
		case "pending", "":
			pending++
		case "request_changes":
			reviewing++
		case "approved":
			approved++
		case "merged":
			merged++
		case "rejected":
			rejected++
		}
	}

	total := len(states)
	msg := fmt.Sprintf("Progress: %d/%d merged", merged, total)
	if approved > 0 {
		msg += fmt.Sprintf(", %d approved", approved)
	}
	if reviewing > 0 {
		msg += fmt.Sprintf(", %d in review", reviewing)
	}
	if pending > 0 {
		msg += fmt.Sprintf(", %d pending", pending)
	}
	if rejected > 0 {
		msg += fmt.Sprintf(", %d rejected", rejected)
	}

	return bdComment(epicID, msg)
}

// resolveWizardAgent determines the wizard agent name for a child bead.
func resolveWizardAgent(child Bead) string {
	// Look for an owner label.
	for _, label := range child.Labels {
		if strings.HasPrefix(label, "owner:") {
			return strings.TrimPrefix(label, "owner:")
		}
	}
	// Fallback: send to steward for routing.
	return "steward"
}

// spireSend sends a message via the spire CLI.
func spireSend(agent, message, refBeadID string, priority int) error {
	args := []string{"send", agent, message}
	if refBeadID != "" {
		args = append(args, "--ref", refBeadID)
	}
	args = append(args, "--priority", fmt.Sprintf("%d", priority))

	cmd := exec.Command("spire", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("spire send to %s: %w\n%s", agent, err, stderr.String())
	}
	return nil
}

// bdComment adds a comment to a bead.
func bdComment(beadID, comment string) error {
	cmd := exec.Command("bd", "comments", "add", beadID, comment)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("bd comments add %s: %w\n%s", beadID, err, stderr.String())
	}
	return nil
}

// bd runs a bd command and returns stdout.
func bd(args ...string) (string, error) {
	cmd := exec.Command("bd", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("bd %s: %w\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

// bdJSON runs a bd command with --json and unmarshals the result.
func bdJSON(result any, args ...string) error {
	args = append(args, "--json")
	out, err := bd(args...)
	if err != nil {
		return err
	}
	if out == "" {
		return nil
	}
	return json.Unmarshal([]byte(out), result)
}
