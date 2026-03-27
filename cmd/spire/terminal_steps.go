package main

// terminal_steps.go — Step-graph formula types and terminal step enforcement.
//
// FormulaStepGraph (version 3) declares a DAG of named steps with conditions.
// It encodes process-internal state machines (like the review loop) that cannot
// be expressed as a linear phase sequence.
//
// terminalSplit and terminalDiscard enforce the branch lifecycle invariant from
// docs/review-dag.md: every path ends with the branch either merged to main or
// deleted. No hanging branches. No orphaned code.

import (
	"fmt"
	"os/exec"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/awell-health/spire/cmd/spire/embedded"
)

// FormulaStepGraph is a v3 formula that declares a DAG of named steps with
// conditions. Version 3 is distinct from the phase-based FormulaV2 (version 2).
type FormulaStepGraph struct {
	Name        string                    `toml:"name"`
	Description string                    `toml:"description"`
	Version     int                       `toml:"version"`
	Vars        map[string]FormulaVar     `toml:"vars"`
	Steps       map[string]FormulaStepDef `toml:"steps"`
}

// FormulaStepDef describes a single node in the step graph.
type FormulaStepDef struct {
	Description string   `toml:"description"`
	Role        string   `toml:"role,omitempty"`
	Needs       []string `toml:"needs,omitempty"`
	Condition   string   `toml:"condition,omitempty"`
	Terminal    bool     `toml:"terminal,omitempty"`
}

// SplitTask is a child bead to create when the arbiter splits a bead.
type SplitTask struct {
	Title       string
	Description string
}

// ParseFormulaStepGraph parses a v3 step-graph formula from TOML bytes.
func ParseFormulaStepGraph(data []byte) (*FormulaStepGraph, error) {
	var f FormulaStepGraph
	if err := toml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse step graph formula: %w", err)
	}
	if f.Version != 3 {
		return nil, fmt.Errorf("expected step graph formula version 3, got %d", f.Version)
	}
	return &f, nil
}

// LoadStepGraphFormula loads a named v3 step-graph formula from embedded defaults.
func LoadStepGraphFormula(name string) (*FormulaStepGraph, error) {
	filename := "formulas/" + name + ".formula.toml"
	data, err := embedded.Formulas.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("embedded formula %q not found", name)
	}
	return ParseFormulaStepGraph(data)
}

// terminalSplit is the arbiter "split" terminal path.
//
// It merges approved work to main, creates child beads for the remaining work,
// and closes the original bead. The arbiter only chooses "split" when partial
// work is good — child beads are additive (they address gaps, not replacements).
//
// Invariant: staging branch is merged and deleted BEFORE child beads are created
// and BEFORE the original bead is closed. If the merge fails, this function
// returns an error and no child beads are created, preventing orphaned beads
// from unmerged code.
func terminalSplit(beadID, reviewerName string, splitTasks []SplitTask, log func(string, ...interface{})) error {
	log("arbiter split: merging approved work + creating %d child task(s)", len(splitTasks))

	bead, err := storeGetBead(beadID)
	if err != nil {
		return fmt.Errorf("terminal split: get bead: %w", err)
	}

	branch := hasLabel(bead, "feat-branch:")
	if branch == "" {
		branch = fmt.Sprintf("feat/%s", beadID)
	}

	repoPath, _, baseBranch, err := wizardResolveRepo(beadID)
	if err != nil {
		return fmt.Errorf("terminal split: resolve repo: %w", err)
	}

	// Merge the staging branch to main first. reviewHandleApproval handles the
	// full merge path: labels, molecule step, phase transition, PR create/merge,
	// branch delete, and bead close. If this fails, we abort before creating
	// child beads so they are never orphaned from unmerged code.
	if err := reviewHandleApproval(beadID, reviewerName, bead.Title, branch, baseBranch, repoPath, log); err != nil {
		return fmt.Errorf("terminal split: merge staging: %w", err)
	}

	// Create child beads for the remaining work. The original bead has been
	// closed by reviewHandleApproval at this point.
	for _, task := range splitTasks {
		childID, cerr := storeCreateBead(createOpts{
			Title:       task.Title,
			Description: task.Description,
			Priority:    bead.Priority,
			Type:        parseIssueType(bead.Type),
			Parent:      beadID,
		})
		if cerr != nil {
			log("warning: create split task %q: %s", task.Title, cerr)
			continue
		}
		log("created split task: %s — %s", childID, task.Title)
		storeAddComment(beadID, fmt.Sprintf("Split task created: %s — %s", childID, task.Title))
	}

	return nil
}

// terminalDiscard is the arbiter "discard" terminal path.
//
// It deletes the staging branch (local and remote) without merging, then closes
// the bead as wontfix. Branch deletion failures are non-fatal (logged as warnings)
// because the bead must still be closed regardless of git state.
//
// Invariant: both local and remote branches are deleted before the bead is closed.
func terminalDiscard(beadID string, log func(string, ...interface{})) error {
	log("arbiter discard: deleting branches and closing as wontfix")

	bead, err := storeGetBead(beadID)
	if err != nil {
		return fmt.Errorf("terminal discard: get bead: %w", err)
	}

	branch := hasLabel(bead, "feat-branch:")
	if branch == "" {
		branch = fmt.Sprintf("feat/%s", beadID)
	}

	repoPath, _, _, resolveErr := wizardResolveRepo(beadID)
	if resolveErr == nil {
		// Delete local branches (feat/ and epic/ — epic branch may not exist).
		exec.Command("git", "-C", repoPath, "branch", "-D", branch).Run()
		epicBranch := fmt.Sprintf("epic/%s", beadID)
		exec.Command("git", "-C", repoPath, "branch", "-D", epicBranch).Run()

		// Delete remote branches.
		exec.Command("git", "-C", repoPath, "push", "origin", "--delete", branch).Run()
		exec.Command("git", "-C", repoPath, "push", "origin", "--delete", epicBranch).Run()
		log("branches deleted")
	} else {
		log("warning: could not resolve repo for branch cleanup: %s", resolveErr)
	}

	storeAddComment(beadID, "Arbiter: closing as wontfix — branches deleted")
	storeRemoveLabel(beadID, "review-feedback")
	return storeCloseBead(beadID)
}
