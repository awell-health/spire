package promptctx

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// fakeStore is the in-memory test seam for BuildInlineFloor. Each
// neighbor-id maps to a Bead and its comments; the dep list is what the
// helper iterates over.
type fakeStore struct {
	deps     []*beads.IssueWithDependencyMetadata
	beads    map[string]store.Bead
	comments map[string][]*beads.Comment
	depsErr  error
}

func (f *fakeStore) toDeps() Deps {
	return Deps{
		GetDepsWithMeta: func(_ string) ([]*beads.IssueWithDependencyMetadata, error) {
			return f.deps, f.depsErr
		},
		GetBead: func(id string) (store.Bead, error) {
			if b, ok := f.beads[id]; ok {
				return b, nil
			}
			return store.Bead{}, errors.New("not found")
		},
		GetComments: func(id string) ([]*beads.Comment, error) {
			return f.comments[id], nil
		},
	}
}

// makeNeighbor builds an IssueWithDependencyMetadata fixture with the
// given relationship, status, body, and title.
func makeNeighbor(id, title, depType, issueType, status, description string) *beads.IssueWithDependencyMetadata {
	dm := &beads.IssueWithDependencyMetadata{}
	dm.ID = id
	dm.Title = title
	dm.Description = description
	dm.IssueType = beads.IssueType(issueType)
	dm.Status = beads.Status(status)
	dm.DependencyType = beads.DependencyType(depType)
	return dm
}

func makeComment(author, body string) *beads.Comment {
	return &beads.Comment{
		Author:    author,
		Text:      body,
		CreatedAt: time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC),
	}
}

func TestBuildInlineFloor_DiscoveredFromClosed(t *testing.T) {
	fs := &fakeStore{
		deps: []*beads.IssueWithDependencyMetadata{
			makeNeighbor("spi-design", "design title", "discovered-from", "design", "closed", "design body"),
		},
		comments: map[string][]*beads.Comment{
			"spi-design": {makeComment("archmage", "design comment one")},
		},
	}
	got := BuildInlineFloor("spi-target", fs.toDeps())
	if !strings.Contains(got, "## Inline graph context") {
		t.Fatalf("missing section header in:\n%s", got)
	}
	if !strings.Contains(got, "### spi-design (design, via discovered-from): design title") {
		t.Fatalf("missing neighbor header:\n%s", got)
	}
	if !strings.Contains(got, "design body") {
		t.Fatalf("missing description body:\n%s", got)
	}
	if !strings.Contains(got, "design comment one") {
		t.Fatalf("missing comment body:\n%s", got)
	}
	if !strings.Contains(got, "[archmage,") {
		t.Fatalf("missing comment author:\n%s", got)
	}
}

func TestBuildInlineFloor_RelatedClosed(t *testing.T) {
	fs := &fakeStore{
		deps: []*beads.IssueWithDependencyMetadata{
			makeNeighbor("spi-rel", "related title", "related", "task", "closed", "related body"),
		},
	}
	got := BuildInlineFloor("spi-target", fs.toDeps())
	if !strings.Contains(got, "spi-rel") || !strings.Contains(got, "related") {
		t.Fatalf("related neighbor not surfaced:\n%s", got)
	}
}

func TestBuildInlineFloor_CausedByClosed(t *testing.T) {
	fs := &fakeStore{
		deps: []*beads.IssueWithDependencyMetadata{
			makeNeighbor("spi-cause", "cause title", "caused-by", "bug", "closed", "cause body"),
		},
	}
	got := BuildInlineFloor("spi-target", fs.toDeps())
	if !strings.Contains(got, "spi-cause") || !strings.Contains(got, "via caused-by") {
		t.Fatalf("caused-by neighbor not surfaced:\n%s", got)
	}
}

func TestBuildInlineFloor_OpenNeighborsOmitted(t *testing.T) {
	fs := &fakeStore{
		deps: []*beads.IssueWithDependencyMetadata{
			makeNeighbor("spi-open", "open title", "discovered-from", "design", "open", "open body"),
			makeNeighbor("spi-prog", "prog title", "related", "task", "in_progress", "prog body"),
			makeNeighbor("spi-block", "block title", "caused-by", "bug", "blocked", "block body"),
			makeNeighbor("spi-defer", "defer title", "discovered-from", "design", "deferred", "defer body"),
			makeNeighbor("spi-ready", "ready title", "related", "task", "ready", "ready body"),
		},
	}
	got := BuildInlineFloor("spi-target", fs.toDeps())
	if got != "" {
		t.Fatalf("expected empty result for non-closed neighbors only, got:\n%s", got)
	}
}

func TestBuildInlineFloor_ScheduleDepsExcluded(t *testing.T) {
	fs := &fakeStore{
		deps: []*beads.IssueWithDependencyMetadata{
			makeNeighbor("spi-block", "blocking title", "blocks", "task", "closed", "blocking body"),
			makeNeighbor("spi-pc", "child title", "parent-child", "task", "closed", "child body"),
		},
	}
	got := BuildInlineFloor("spi-target", fs.toDeps())
	if got != "" {
		t.Fatalf("expected empty result for non-semantic dep types, got:\n%s", got)
	}
}

func TestBuildInlineFloor_NoDeps(t *testing.T) {
	fs := &fakeStore{deps: nil}
	got := BuildInlineFloor("spi-target", fs.toDeps())
	if got != "" {
		t.Fatalf("expected empty section when no deps, got: %q", got)
	}
}

func TestBuildInlineFloor_PerBeadCapTruncates(t *testing.T) {
	bigBody := strings.Repeat("A", PerBeadCapBytes*2)
	fs := &fakeStore{
		deps: []*beads.IssueWithDependencyMetadata{
			makeNeighbor("spi-big", "big title", "discovered-from", "design", "closed", bigBody),
		},
	}
	got := BuildInlineFloor("spi-target", fs.toDeps())
	if !strings.Contains(got, "[…truncated; run `spire graph spi-big` for full content]") {
		t.Fatalf("expected per-bead truncation marker:\n%s", got)
	}
	// Section length: header (~26 bytes) + one chunk capped at PerBeadCapBytes
	// + a trailing newline. Confirm the cap bound is honored on the chunk.
	headerLen := len("## Inline graph context\n\n")
	chunkLen := len(got) - headerLen - 1 // trailing "\n" between chunk and EOF
	if chunkLen > PerBeadCapBytes {
		t.Fatalf("chunk size %d exceeds per-bead cap %d (full output:\n%s)", chunkLen, PerBeadCapBytes, got)
	}
}

func TestBuildInlineFloor_TotalCapDropsTrailing(t *testing.T) {
	// Each neighbor renders ~PerBeadCapBytes (4KB), so 9 neighbors should
	// blow through the 32KB total cap; expect a trailing summary line.
	chunk := strings.Repeat("B", PerBeadCapBytes-200) // leave room for header/footer of chunk
	deps := make([]*beads.IssueWithDependencyMetadata, 0, 12)
	for i := 0; i < 12; i++ {
		id := "spi-n" + string(rune('a'+i))
		deps = append(deps, makeNeighbor(id, "title", "related", "task", "closed", chunk))
	}
	fs := &fakeStore{deps: deps}
	got := BuildInlineFloor("spi-target", fs.toDeps())
	if !strings.Contains(got, "additional neighbor(s) omitted") {
		t.Fatalf("expected total-cap omission marker:\n%s", got[len(got)-200:])
	}
	if !strings.Contains(got, "spire graph spi-target") {
		t.Fatalf("expected total-cap marker to reference the parent bead:\n%s", got[len(got)-200:])
	}
	// Verify some early neighbor was rendered.
	if !strings.Contains(got, "spi-na") {
		t.Fatalf("expected first neighbor to be rendered: prefix=\n%s", got[:200])
	}
	// Verify the very last neighbor was NOT rendered (it was dropped).
	if strings.Contains(got, "spi-nl") {
		t.Fatalf("expected last neighbor to be dropped (total cap), but found it")
	}
}

func TestBuildInlineFloor_MissingComments(t *testing.T) {
	fs := &fakeStore{
		deps: []*beads.IssueWithDependencyMetadata{
			makeNeighbor("spi-d", "design", "discovered-from", "design", "closed", "body only"),
		},
	}
	got := BuildInlineFloor("spi-target", fs.toDeps())
	if !strings.Contains(got, "body only") {
		t.Fatalf("description not rendered:\n%s", got)
	}
	if strings.Contains(got, "#### Comments") {
		t.Fatalf("expected no Comments section when zero comments, got:\n%s", got)
	}
}

func TestBuildInlineFloor_NilSeams(t *testing.T) {
	got := BuildInlineFloor("spi-x", Deps{})
	if got != "" {
		t.Fatalf("expected empty result with nil seams, got: %q", got)
	}
}

func TestBuildInlineFloor_DepsErrorIsSilent(t *testing.T) {
	fs := &fakeStore{depsErr: errors.New("boom")}
	got := BuildInlineFloor("spi-x", fs.toDeps())
	if got != "" {
		t.Fatalf("expected empty result on deps error, got: %q", got)
	}
}

func TestBuildPromptSuffix_NoFloor_StillEmitsStopCriterion(t *testing.T) {
	fs := &fakeStore{deps: nil}
	got := BuildPromptSuffix("spi-target", fs.toDeps(), false)
	if !strings.Contains(got, "## Graph context") {
		t.Fatalf("stop-criterion block missing:\n%s", got)
	}
	if strings.Contains(got, "## Inline graph context") {
		t.Fatalf("inline-floor header should not appear when no closed neighbors:\n%s", got)
	}
}

func TestBuildPromptSuffix_ClericExtraLine(t *testing.T) {
	fs := &fakeStore{deps: nil}
	got := BuildPromptSuffix("spi-target", fs.toDeps(), true)
	if !strings.Contains(got, ClericGraphWalkLine) {
		t.Fatalf("cleric extra line missing:\n%s", got)
	}
	got2 := BuildPromptSuffix("spi-target", fs.toDeps(), false)
	if strings.Contains(got2, ClericGraphWalkLine) {
		t.Fatalf("non-cleric callers must not get the extra line:\n%s", got2)
	}
}

// TestBuildPromptSuffix_RoleParity pins the cleric=false path to exactly
// one shape across the wizard / apprentice / sage / arbiter call sites:
// the inline-floor section + the stop-criterion block, no extras. The
// cleric=true path has exactly one additional line.
func TestBuildPromptSuffix_RoleParity(t *testing.T) {
	fs := &fakeStore{
		deps: []*beads.IssueWithDependencyMetadata{
			makeNeighbor("spi-d", "design", "discovered-from", "design", "closed", "design body"),
		},
		comments: map[string][]*beads.Comment{
			"spi-d": {makeComment("a", "c1")},
		},
	}

	wizard := BuildPromptSuffix("spi-target", fs.toDeps(), false)
	apprentice := BuildPromptSuffix("spi-target", fs.toDeps(), false)
	sage := BuildPromptSuffix("spi-target", fs.toDeps(), false)
	arbiter := BuildPromptSuffix("spi-target", fs.toDeps(), false)
	cleric := BuildPromptSuffix("spi-target", fs.toDeps(), true)

	if wizard != apprentice || apprentice != sage || sage != arbiter {
		t.Fatalf("non-cleric role suffixes must be byte-identical for the same bead")
	}

	if !strings.HasPrefix(cleric, wizard[:len(wizard)-1]) {
		// Cleric output must start with the same prefix as wizard (sans
		// the trailing newline) and then add the extra line.
		t.Fatalf("cleric prefix diverges from non-cleric:\nwizard: %q\ncleric: %q", wizard, cleric)
	}

	if !strings.Contains(cleric, ClericGraphWalkLine) {
		t.Fatalf("cleric output missing extra line: %q", cleric)
	}
}
