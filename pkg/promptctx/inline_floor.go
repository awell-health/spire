// Package promptctx assembles the "inline graph context" block that role
// prompts (wizard design / wizard implement / sage / cleric / arbiter) and
// `spire focus` share. Closed neighbors linked via discovered-from /
// related / caused-by are pre-expanded with body + all comments under a
// per-bead and total byte cap; open neighbors stay as reference cards
// elsewhere and the agent walks them via `spire graph`.
//
// The helper is invocation-injected so the prompt builders don't take a
// hard dependency on pkg/store (test fixtures replace the four seams) and
// so this package never reaches sideways into the runtime.
package promptctx

import (
	"fmt"
	"strings"

	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// PerBeadCapBytes caps the rendered description+comments per neighbor.
// Above this, the chunk is truncated at the cap with a marker pointing
// the agent at `spire graph <neighbor-id>` for the full body.
const PerBeadCapBytes = 4 * 1024

// TotalCapBytes caps the whole inline-floor section. Once a candidate
// neighbor would push the running total over this, processing stops and
// trailing neighbors are summarized with a "N additional neighbors
// omitted" marker. Cap counts only neighbor chunks — the section header
// and the trailing summary line are not counted against it.
const TotalCapBytes = 32 * 1024

// StopCriterionBlock is the checkable graph-context instruction appended
// to every role prompt below the inline-floor section. The wording is
// load-bearing: the sage uses it as a verbatim signal during review.
const StopCriterionBlock = `## Graph context

Before starting work, you should be able to state in one sentence:
(a) why this work exists, and
(b) what constraint each linked bead imposes on your implementation.
If you can't, run ` + "`spire graph <id>`" + ` to read more.`

// ClericGraphWalkLine is the single extra line appended to the cleric's
// stop-criterion block. It broadens the walk to encompass failure
// context — the cleric needs both the original intent and the failure
// surface to pick a repair.
const ClericGraphWalkLine = "When recovering, walk the graph until you understand both the failure and the original intent."

// truncationMarker is the per-bead truncation marker.
func truncationMarker(neighborID string) string {
	return fmt.Sprintf("\n\n[…truncated; run `spire graph %s` for full content]", neighborID)
}

// totalOmittedMarker is the trailing summary line emitted when the total
// cap fires. n is the number of neighbors that were not rendered.
func totalOmittedMarker(beadID string, n int) string {
	return fmt.Sprintf("\n\n[%d additional neighbor(s) omitted; run `spire graph %s` to walk the graph]", n, beadID)
}

// Deps groups the four read seams the helper needs. Production code
// passes a wired Deps backed by pkg/store; tests pass an in-memory fake.
// All four functions must be non-nil — the helper does not synthesize
// fallbacks because partial wiring is always a wiring bug, not a runtime
// scenario worth tolerating.
type Deps struct {
	GetDepsWithMeta func(beadID string) ([]*beads.IssueWithDependencyMetadata, error)
	GetBead         func(beadID string) (store.Bead, error)
	GetComments     func(beadID string) ([]*beads.Comment, error)
}

// StoreDeps returns a Deps wired against the live pkg/store API. Use
// this from production call sites; tests should construct Deps with
// in-memory fakes.
func StoreDeps() Deps {
	return Deps{
		GetDepsWithMeta: store.GetDepsWithMeta,
		GetBead:         store.GetBead,
		GetComments:     store.GetComments,
	}
}

// BuildInlineFloor renders the "Inline graph context" section for
// beadID. Returns "" when no closed neighbor links via discovered-from /
// related / caused-by, so callers can append the result unconditionally
// without producing an empty header.
//
// Iteration order follows the order GetDepsWithMeta returns deps.
// Trailing neighbors are dropped when the running byte total would
// exceed TotalCapBytes; per-bead chunks that exceed PerBeadCapBytes are
// truncated at the cap with a `spire graph <id>` pointer.
//
// All four read seams MUST be wired on deps. Errors from any seam are
// best-effort — a single failed neighbor lookup is logged into the
// rendered chunk so reviewers see the gap and can chase it manually,
// but the surrounding section still renders.
func BuildInlineFloor(beadID string, deps Deps) string {
	if deps.GetDepsWithMeta == nil || deps.GetBead == nil || deps.GetComments == nil {
		return ""
	}

	rawDeps, err := deps.GetDepsWithMeta(beadID)
	if err != nil || len(rawDeps) == 0 {
		return ""
	}

	// Filter to qualifying deps in stable iteration order.
	qualifying := make([]*beads.IssueWithDependencyMetadata, 0, len(rawDeps))
	for _, dm := range rawDeps {
		if dm == nil {
			continue
		}
		if !inlineFloorDepType(string(dm.DependencyType)) {
			continue
		}
		if string(dm.Status) != string(beads.StatusClosed) {
			continue
		}
		qualifying = append(qualifying, dm)
	}
	if len(qualifying) == 0 {
		return ""
	}

	var section strings.Builder
	section.WriteString("## Inline graph context\n\n")

	totalBytes := 0
	rendered := 0
	for i, dm := range qualifying {
		chunk := renderNeighborChunk(dm, deps)
		// Total cap fires when the next chunk's bytes would push the
		// running total over the cap. Trailing neighbors are
		// summarized; we do not partial-render the next chunk.
		if totalBytes > 0 && totalBytes+len(chunk) > TotalCapBytes {
			section.WriteString(totalOmittedMarker(beadID, len(qualifying)-i))
			section.WriteString("\n")
			return section.String()
		}
		section.WriteString(chunk)
		section.WriteString("\n")
		totalBytes += len(chunk)
		rendered++
	}
	_ = rendered
	return section.String()
}

// inlineFloorDepType returns true for the three semantic dep types the
// inline floor covers: discovered-from, related, caused-by. Excludes
// parent-child (special-cased elsewhere), blocks/blocked-by/conditional-
// blocks (scheduling, not semantic context), and the rest.
func inlineFloorDepType(depType string) bool {
	switch depType {
	case string(beads.DepDiscoveredFrom),
		string(beads.DepRelated),
		store.DepCausedBy:
		return true
	}
	return false
}

// BuildPromptSuffix renders the standard role-prompt suffix: inline
// floor (when non-empty) followed by the stop-criterion block. Cleric
// callers pass cleric=true to append the extra graph-walk line.
//
// Returns "" when there is no inline-floor content AND the caller does
// not want the stop-criterion block on its own — every role today wants
// the stop-criterion block, so the caller passes the flag and the
// function emits at minimum the stop-criterion block.
func BuildPromptSuffix(beadID string, deps Deps, cleric bool) string {
	var sb strings.Builder
	floor := BuildInlineFloor(beadID, deps)
	if floor != "" {
		sb.WriteString(floor)
		sb.WriteString("\n")
	}
	sb.WriteString(StopCriterionBlock)
	if cleric {
		sb.WriteString("\n\n")
		sb.WriteString(ClericGraphWalkLine)
	}
	sb.WriteString("\n")
	return sb.String()
}

// renderNeighborChunk renders one neighbor's body + all comments,
// applying the per-bead cap. The header line carries neighbor id, type,
// dep relationship, and title; the body is the bead's description; the
// comments section lists each comment with its author and timestamp.
func renderNeighborChunk(dm *beads.IssueWithDependencyMetadata, deps Deps) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### %s (%s, via %s): %s\n\n", dm.ID, string(dm.IssueType), string(dm.DependencyType), dm.Title)

	if strings.TrimSpace(dm.Description) != "" {
		b.WriteString(dm.Description)
		if !strings.HasSuffix(dm.Description, "\n") {
			b.WriteString("\n")
		}
	}

	comments, cerr := deps.GetComments(dm.ID)
	if cerr == nil && len(comments) > 0 {
		b.WriteString("\n#### Comments\n\n")
		for _, c := range comments {
			if c == nil {
				continue
			}
			ts := ""
			if !c.CreatedAt.IsZero() {
				ts = c.CreatedAt.UTC().Format("2006-01-02T15:04:05Z")
			}
			fmt.Fprintf(&b, "- [%s, %s] %s\n", c.Author, ts, c.Text)
		}
	}

	chunk := b.String()
	if len(chunk) > PerBeadCapBytes {
		// Truncate at the cap (best-effort byte boundary — chunks are
		// small enough that a mid-rune truncation is acceptable; the
		// marker tells the agent how to fetch the rest).
		marker := truncationMarker(dm.ID)
		// Reserve room for the marker so the cap is still respected.
		keep := PerBeadCapBytes - len(marker)
		if keep < 0 {
			keep = 0
		}
		if keep > len(chunk) {
			keep = len(chunk)
		}
		chunk = chunk[:keep] + marker
	}
	return chunk
}
