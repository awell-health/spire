package board

import (
	"fmt"
	"sort"
	"strings"

	"github.com/steveyegge/beads"
	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/store"
)

// DAGProgress holds the executor DAG state for a single bead.
// Used by roster, watch, and board to show step pipeline, active attempt,
// and review history inline.
type DAGProgress struct {
	Steps   []DAGStep   `json:"steps,omitempty"`
	Attempt *DAGAttempt `json:"active_attempt,omitempty"`
	Reviews []DAGReview `json:"reviews,omitempty"`
}

// DAGStep represents one workflow step bead in the pipeline.
type DAGStep struct {
	Name   string `json:"name"`
	Status string `json:"status"` // closed, in_progress, open
}

// DAGAttempt represents the active attempt bead.
type DAGAttempt struct {
	ID     string `json:"id"`
	Agent  string `json:"agent"`
	Model  string `json:"model,omitempty"`
	Branch string `json:"branch,omitempty"`
}

// DAGReview represents a review round bead.
type DAGReview struct {
	Round   int    `json:"round"`
	Status  string `json:"status"`
	Verdict string `json:"verdict,omitempty"`
}

// FetchDAGProgress loads the executor DAG for a bead from the store.
// Returns nil if no DAG data exists (no step/attempt/review beads).
func FetchDAGProgress(beadID string) *DAGProgress {
	var dag DAGProgress
	hasData := false

	// Step beads (pipeline phases).
	steps, err := store.GetStepBeads(beadID)
	if err == nil && len(steps) > 0 {
		hasData = true
		for _, s := range steps {
			name := store.StepBeadPhaseName(s)
			if name == "" {
				continue
			}
			dag.Steps = append(dag.Steps, DAGStep{
				Name:   name,
				Status: s.Status,
			})
		}
		sort.Slice(dag.Steps, func(i, j int) bool {
			return phaseIndex(dag.Steps[i].Name) < phaseIndex(dag.Steps[j].Name)
		})
	}

	// Active attempt.
	attempt, err := store.GetActiveAttempt(beadID)
	if err == nil && attempt != nil {
		hasData = true
		ag := &DAGAttempt{ID: attempt.ID}
		ag.Agent = store.HasLabel(*attempt, "agent:")
		ag.Model = store.HasLabel(*attempt, "model:")
		ag.Branch = store.HasLabel(*attempt, "branch:")
		dag.Attempt = ag
	}

	// Review beads.
	reviews, err := store.GetReviewBeads(beadID)
	if err == nil && len(reviews) > 0 {
		hasData = true
		for _, r := range reviews {
			rn := store.ReviewRoundNumber(r)
			verdict := extractReviewVerdict(r)
			dag.Reviews = append(dag.Reviews, DAGReview{
				Round:   rn,
				Status:  r.Status,
				Verdict: verdict,
			})
		}
	}

	if !hasData {
		return nil
	}
	return &dag
}

// extractReviewVerdict parses the verdict from a review bead's description.
// Review beads have description: "verdict: <verdict>\n\n<summary>".
func extractReviewVerdict(b Bead) string {
	if b.Description == "" {
		return ""
	}
	if strings.HasPrefix(b.Description, "verdict: ") {
		line := b.Description
		if idx := strings.Index(line, "\n"); idx >= 0 {
			line = line[:idx]
		}
		return strings.TrimPrefix(line, "verdict: ")
	}
	return ""
}

// phaseIndex returns the canonical position of a phase name.
// Unknown phases sort to the end.
// v3StepOrder defines the canonical display order for v3 graph step names.
// Steps not in this list are sorted after known steps, alphabetically.
var v3StepOrder = map[string]int{
	"design-check": 0,
	"design":       0,
	"plan":         1,
	"materialize":  2,
	"implement":    3,
	"verify":       4,
	"verify-build": 4,
	"review":       5,
	"merge":        6,
	"close":        7,
	"discard":      7,
}

// PhaseIndex returns the display order for a step/phase name.
// Exported for use by cmd/spire/trace.go and other renderers.
func PhaseIndex(name string) int {
	return phaseIndex(name)
}

func phaseIndex(name string) int {
	// Try v2 phase ordering first.
	for i, p := range formula.ValidPhases {
		if p == name {
			return i
		}
	}
	// Try v3 step ordering.
	if idx, ok := v3StepOrder[name]; ok {
		return idx
	}
	return 99
}

// FetchDAGProgressFromChildren builds DAGProgress from a pre-fetched children
// slice instead of querying the store per bead. The filtering logic mirrors
// store.GetStepBeads, store.GetActiveAttempt, and store.GetReviewBeads.
func FetchDAGProgressFromChildren(beadID string, children []store.Bead) *DAGProgress {
	var dag DAGProgress
	hasData := false

	// Step beads (pipeline phases) — mirrors store.GetStepBeads filter.
	var steps []store.Bead
	for _, c := range children {
		if store.IsStepBead(c) {
			steps = append(steps, c)
		}
	}
	if len(steps) > 0 {
		hasData = true
		for _, s := range steps {
			name := store.StepBeadPhaseName(s)
			if name == "" {
				continue
			}
			dag.Steps = append(dag.Steps, DAGStep{
				Name:   name,
				Status: s.Status,
			})
		}
		sort.Slice(dag.Steps, func(i, j int) bool {
			return phaseIndex(dag.Steps[i].Name) < phaseIndex(dag.Steps[j].Name)
		})
	}

	// Active attempt — mirrors store.GetActiveAttempt filter.
	var active []store.Bead
	for _, c := range children {
		if (c.Status == "open" || c.Status == "in_progress") && store.IsAttemptBead(c) {
			active = append(active, c)
		}
	}
	if len(active) == 1 {
		hasData = true
		a := active[0]
		ag := &DAGAttempt{ID: a.ID}
		ag.Agent = store.HasLabel(a, "agent:")
		ag.Model = store.HasLabel(a, "model:")
		ag.Branch = store.HasLabel(a, "branch:")
		dag.Attempt = ag
	}

	// Review beads — mirrors store.GetReviewBeads filter + sort.
	var reviews []store.Bead
	for _, c := range children {
		if store.IsReviewRoundBead(c) {
			reviews = append(reviews, c)
		}
	}
	if len(reviews) > 0 {
		// Sort by round number ascending (same as store.GetReviewBeads).
		for i := 0; i < len(reviews); i++ {
			for j := i + 1; j < len(reviews); j++ {
				ri := store.ReviewRoundNumber(reviews[i])
				rj := store.ReviewRoundNumber(reviews[j])
				if rj < ri {
					reviews[i], reviews[j] = reviews[j], reviews[i]
				}
			}
		}
		hasData = true
		for _, r := range reviews {
			rn := store.ReviewRoundNumber(r)
			verdict := extractReviewVerdict(r)
			dag.Reviews = append(dag.Reviews, DAGReview{
				Round:   rn,
				Status:  r.Status,
				Verdict: verdict,
			})
		}
	}

	if !hasData {
		return nil
	}
	return &dag
}

// BuildDAGProgressMap builds a DAGProgress map for multiple beads using
// pre-fetched children data. Only includes entries with meaningful data.
func BuildDAGProgressMap(beadIDs []string, childrenMap map[string][]store.Bead) map[string]*DAGProgress {
	result := make(map[string]*DAGProgress)
	for _, id := range beadIDs {
		dag := FetchDAGProgressFromChildren(id, childrenMap[id])
		if dag != nil && len(dag.Steps) > 0 {
			result[id] = dag
		}
	}
	return result
}

// RenderPipelineANSI renders step beads as an inline ANSI pipeline.
// Example: [✅ design] → [▶ plan] → [○ implement] → [○ review] → [○ merge]
func RenderPipelineANSI(steps []DAGStep) string {
	if len(steps) == 0 {
		return ""
	}
	var parts []string
	for _, s := range steps {
		parts = append(parts, renderStepANSI(s))
	}
	return strings.Join(parts, " → ")
}

// RenderPipelineCompactANSI renders step beads as compact icons.
// Example: ✅ ▶ ○ ○ ○
func RenderPipelineCompactANSI(steps []DAGStep) string {
	if len(steps) == 0 {
		return ""
	}
	var parts []string
	for _, s := range steps {
		parts = append(parts, stepIconANSI(s))
	}
	return strings.Join(parts, " ")
}

func renderStepANSI(s DAGStep) string {
	switch s.Status {
	case "closed":
		return Green + "[✓ " + s.Name + "]" + Reset
	case "in_progress":
		return Cyan + "[▶ " + s.Name + "]" + Reset
	default:
		return Dim + "[○ " + s.Name + "]" + Reset
	}
}

func stepIconANSI(s DAGStep) string {
	switch s.Status {
	case "closed":
		return Green + "✓" + Reset
	case "in_progress":
		return Cyan + "▶" + Reset
	default:
		return Dim + "○" + Reset
	}
}

// RenderAttemptANSI renders attempt info as an ANSI string.
func RenderAttemptANSI(a *DAGAttempt) string {
	if a == nil {
		return ""
	}
	parts := []string{Cyan + a.Agent + Reset}
	if a.Model != "" {
		parts = append(parts, Dim+a.Model+Reset)
	}
	return strings.Join(parts, " ")
}

// RenderReviewSummaryANSI renders a compact review summary.
// Example: "review r2 (request_changes → approve)"
func RenderReviewSummaryANSI(reviews []DAGReview) string {
	if len(reviews) == 0 {
		return ""
	}
	last := reviews[len(reviews)-1]
	verdict := last.Verdict
	if verdict == "" && last.Status == "in_progress" {
		verdict = "pending"
	} else if verdict == "" {
		verdict = "unknown"
	}
	icon := reviewVerdictIconANSI(verdict)
	return fmt.Sprintf("r%d %s %s", last.Round, icon, verdict)
}

func reviewVerdictIconANSI(verdict string) string {
	switch verdict {
	case "approve":
		return Green + "✓" + Reset
	case "request_changes":
		return Yellow + "↻" + Reset
	case "reject":
		return Red + "✗" + Reset
	default:
		return Dim + "…" + Reset
	}
}

// EpicChildSummary holds the subtask progress for an epic.
type EpicChildSummary struct {
	Total   int `json:"total"`
	Done    int `json:"done"`
	Working int `json:"working"`
	Blocked int `json:"blocked"`
	Ready   int `json:"ready"`
}

// FetchEpicChildSummary returns a count of subtask statuses for an epic.
// Only counts "real" children (not step/attempt/review beads).
func FetchEpicChildSummary(epicID string) *EpicChildSummary {
	children, err := store.GetChildren(epicID)
	if err != nil || len(children) == 0 {
		return nil
	}

	// Build a set of blocked bead IDs so we can detect blocked children.
	blockedSet := make(map[string]bool)
	if blocked, err := store.GetBlockedIssues(beads.WorkFilter{}); err == nil {
		for _, b := range blocked {
			blockedSet[b.ID] = true
		}
	}

	var s EpicChildSummary
	for _, c := range children {
		if store.IsStepBead(c) || store.IsAttemptBead(c) || store.IsReviewRoundBead(c) {
			continue
		}
		s.Total++
		switch c.Status {
		case "closed":
			s.Done++
		case "in_progress":
			s.Working++
		default:
			if blockedSet[c.ID] {
				s.Blocked++
			} else {
				s.Ready++
			}
		}
	}
	if s.Total == 0 {
		return nil
	}
	return &s
}

// RenderEpicProgressANSI renders epic subtask progress inline.
// Example: "(3/5 done, 2 working)"
func RenderEpicProgressANSI(s *EpicChildSummary) string {
	if s == nil || s.Total == 0 {
		return ""
	}
	parts := []string{fmt.Sprintf("%d/%d done", s.Done, s.Total)}
	if s.Working > 0 {
		parts = append(parts, fmt.Sprintf("%d working", s.Working))
	}
	return "(" + strings.Join(parts, ", ") + ")"
}
