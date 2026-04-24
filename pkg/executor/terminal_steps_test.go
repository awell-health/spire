package executor

import (
	"testing"

	"github.com/steveyegge/beads"
)

// TestTerminalSplit_ChildrenAreIndependentTasks exercises the arbiter "split"
// terminal path and asserts the three guarantees that distinguish split
// children from epic sub-beads:
//
//  1. CreateOpts.Parent is empty — split children stay visible on the board.
//  2. CreateOpts.Type is beads.TypeTask even when the parent is an epic —
//     no accidental epic promotion (and no Linear sync for follow-ups).
//  3. AddDepTyped is called as (childID, parentID, "discovered-from") once
//     per split task, in input order — lineage is preserved via a
//     discovered-from edge instead of parent-child.
func TestTerminalSplit_ChildrenAreIndependentTasks(t *testing.T) {
	const parentID = "spi-parent"

	// Parent is an epic — the most failure-prone case for type inheritance.
	parentBead := Bead{
		ID:       parentID,
		Title:    "parent epic",
		Type:     "epic",
		Priority: 1,
		Labels:   []string{"feat-branch:feat/spi-parent"},
	}

	splitTasks := []SplitTask{
		{Title: "follow-up one", Description: "first remaining piece"},
		{Title: "follow-up two", Description: "second remaining piece"},
	}

	var createdOpts []CreateOpts
	type depCall struct {
		issueID, dependsOnID, depType string
	}
	var depCalls []depCall

	childCounter := 0
	deps := &Deps{
		GetBead: func(id string) (Bead, error) {
			if id != parentID {
				t.Fatalf("GetBead called with %q, want %q", id, parentID)
			}
			return parentBead, nil
		},
		HasLabel: func(b Bead, prefix string) string {
			for _, l := range b.Labels {
				if len(l) >= len(prefix) && l[:len(prefix)] == prefix {
					return l[len(prefix):]
				}
			}
			return ""
		},
		ResolveRepo: func(id string) (string, string, string, error) {
			return "/tmp/repo", "", "main", nil
		},
		ReviewHandleApproval: func(beadID, reviewerName, branch, baseBranch, repoPath string, log func(string, ...interface{})) error {
			// Simulate the real flow: approval closes the parent before
			// children are created. The fix we're testing makes this safe
			// because children no longer reference a closed parent.
			return nil
		},
		CreateBead: func(opts CreateOpts) (string, error) {
			createdOpts = append(createdOpts, opts)
			childCounter++
			return "spi-child-" + string(rune('0'+childCounter)), nil
		},
		AddDepTyped: func(issueID, dependsOnID, depType string) error {
			depCalls = append(depCalls, depCall{issueID, dependsOnID, depType})
			return nil
		},
		AddComment: func(id, text string) error { return nil },
		// ParseIssueType must remain unused by the split path — if the fix
		// regresses and calls it, this stub would still return a non-task
		// type, making the assertion below fail loudly.
		ParseIssueType: func(s string) beads.IssueType { return beads.IssueType(s) },
	}

	err := TerminalSplit(parentID, "sage-test", splitTasks, deps, func(string, ...interface{}) {})
	if err != nil {
		t.Fatalf("TerminalSplit returned error: %v", err)
	}

	if len(createdOpts) != len(splitTasks) {
		t.Fatalf("CreateBead call count = %d, want %d", len(createdOpts), len(splitTasks))
	}

	for i, opts := range createdOpts {
		if opts.Parent != "" {
			t.Errorf("split child %d: CreateOpts.Parent = %q, want empty (children must stay visible on the board)", i, opts.Parent)
		}
		if opts.Type != beads.TypeTask {
			t.Errorf("split child %d: CreateOpts.Type = %q, want %q (even when parent is epic)", i, opts.Type, beads.TypeTask)
		}
		if opts.Title != splitTasks[i].Title {
			t.Errorf("split child %d: CreateOpts.Title = %q, want %q", i, opts.Title, splitTasks[i].Title)
		}
	}

	if len(depCalls) != len(splitTasks) {
		t.Fatalf("AddDepTyped call count = %d, want %d", len(depCalls), len(splitTasks))
	}

	for i, call := range depCalls {
		wantChild := "spi-child-" + string(rune('0'+i+1))
		if call.issueID != wantChild {
			t.Errorf("AddDepTyped call %d: issueID = %q, want %q", i, call.issueID, wantChild)
		}
		if call.dependsOnID != parentID {
			t.Errorf("AddDepTyped call %d: dependsOnID = %q, want %q", i, call.dependsOnID, parentID)
		}
		if call.depType != "discovered-from" {
			t.Errorf("AddDepTyped call %d: depType = %q, want %q", i, call.depType, "discovered-from")
		}
	}
}
