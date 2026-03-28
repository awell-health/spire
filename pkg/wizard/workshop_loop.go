package wizard

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// WorkshopLoop is the wizard's main event loop.
// It dispatches to phase-specific handlers and saves state after each action.
func WorkshopLoop(state *WorkshopState, spawner Backend, deps *Deps) error {
	for {
		// Check inbox for messages between actions
		WorkshopCheckInbox(state, deps)

		// Save state before each phase handler
		if err := SaveWorkshopState(state, deps); err != nil {
			return fmt.Errorf("save state: %w", err)
		}

		var err error
		switch state.Phase {
		case "design":
			err = WorkshopDesign(state)
		case "plan":
			err = WorkshopPlan(state)
		case "implement":
			err = WorkshopImplement(state, spawner, deps)
		case "review":
			err = WorkshopReview(state, spawner, deps)
		case "merge":
			err = WorkshopMerge(state, deps)
		default:
			return fmt.Errorf("unknown phase: %s", state.Phase)
		}

		if err != nil {
			return fmt.Errorf("phase %s: %w", state.Phase, err)
		}

		// Check if epic is closed (done)
		bead, err := deps.GetBead(state.EpicID)
		if err != nil {
			return fmt.Errorf("check epic: %w", err)
		}
		if bead.Status == "closed" {
			fmt.Fprintf(os.Stderr, "[workshop] epic %s is closed — exiting\n", state.EpicID)
			return SaveWorkshopState(state, deps)
		}
	}
}

// WorkshopCheckInbox reads the local inbox file for any messages.
// Messages are logged but not acted on in this function — the phase
// handlers decide what to do with them.
func WorkshopCheckInbox(state *WorkshopState, deps *Deps) []InboxMessage {
	agentName := "wizard-" + state.EpicID
	data, err := deps.ReadInboxFile(agentName)
	if err != nil {
		return nil
	}
	var inbox InboxFile
	if err := json.Unmarshal(data, &inbox); err != nil {
		return nil
	}
	if len(inbox.Messages) > 0 {
		fmt.Fprintf(os.Stderr, "[workshop] %d message(s) in inbox\n", len(inbox.Messages))
	}
	return inbox.Messages
}

// WorkshopConsultClaude invokes Claude with the given prompt using --resume
// for persistent context. Creates a new session on first call.
func WorkshopConsultClaude(state *WorkshopState, prompt string) (string, error) {
	args := []string{
		"--dangerously-skip-permissions",
		"-p", prompt,
		"--model", "claude-opus-4-6",
		"--output-format", "text",
	}

	if state.SessionID != "" {
		args = append(args, "--resume", state.SessionID)
	}

	cmd := exec.Command("claude", args...)
	cmd.Env = os.Environ()
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("claude: %w", err)
	}

	// TODO: capture session ID from claude output for --resume
	// For now, each invocation is independent

	return strings.TrimSpace(string(out)), nil
}

// WorkshopDesign handles the design phase.
// For now, this is human-driven — the wizard prints instructions and exits.
func WorkshopDesign(state *WorkshopState) error {
	fmt.Fprintf(os.Stderr, "[workshop] design phase — this is archmage-driven\n")
	fmt.Fprintf(os.Stderr, "[workshop] when design is complete, transition to plan:\n")
	fmt.Fprintf(os.Stderr, "  bd label remove %s \"phase:design\"\n", state.EpicID)
	fmt.Fprintf(os.Stderr, "  bd label add %s \"phase:plan\"\n", state.EpicID)
	fmt.Fprintf(os.Stderr, "[workshop] then re-run: spire workshop %s\n", state.EpicID)
	return fmt.Errorf("waiting for archmage to complete design")
}

// WorkshopPlan handles the plan phase.
// For now, this is human-driven — the wizard prints instructions and exits.
func WorkshopPlan(state *WorkshopState) error {
	fmt.Fprintf(os.Stderr, "[workshop] plan phase — this is archmage-driven\n")
	fmt.Fprintf(os.Stderr, "[workshop] break the epic into subtasks, then transition to implement:\n")
	fmt.Fprintf(os.Stderr, "  bd label remove %s \"phase:plan\"\n", state.EpicID)
	fmt.Fprintf(os.Stderr, "  bd label add %s \"phase:implement\"\n", state.EpicID)
	fmt.Fprintf(os.Stderr, "[workshop] then re-run: spire workshop %s\n", state.EpicID)
	return fmt.Errorf("waiting for archmage to complete plan")
}

// WorkshopMerge handles the merge phase.
func WorkshopMerge(state *WorkshopState, deps *Deps) error {
	fmt.Fprintf(os.Stderr, "[workshop] merge phase\n")

	// Use the existing ReviewMerge function from wizard_review.go
	bead, err := deps.GetBead(state.EpicID)
	if err != nil {
		return fmt.Errorf("get epic: %w", err)
	}

	branch := deps.HasLabel(bead, "feat-branch:")
	if branch == "" {
		branch = fmt.Sprintf("epic/%s", state.EpicID)
	}

	repoPath, _, baseBranch, err := deps.ResolveRepo(state.EpicID)
	if err != nil {
		return fmt.Errorf("resolve repo: %w", err)
	}

	log := func(format string, a ...interface{}) {
		fmt.Fprintf(os.Stderr, "[workshop] "+format+"\n", a...)
	}

	if err := ReviewMerge(state.EpicID, bead.Title, branch, baseBranch, repoPath, deps, log); err != nil {
		return fmt.Errorf("merge: %w", err)
	}

	// Close the epic
	deps.RemoveLabel(state.EpicID, "phase:merge")
	if err := deps.CloseBead(state.EpicID); err != nil {
		return fmt.Errorf("close epic: %w", err)
	}

	state.Phase = "done"
	fmt.Fprintf(os.Stderr, "[workshop] epic %s merged and closed\n", state.EpicID)
	return nil
}
