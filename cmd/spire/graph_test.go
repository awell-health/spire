package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/beads"
)

// --- Test fixtures ---

// fakeStore holds stubbed bead/dep/comment data for graph tests. Keys
// are bead IDs.
type fakeStore struct {
	beads    map[string]Bead
	deps     map[string][]*beads.IssueWithDependencyMetadata
	comments map[string][]*beads.Comment
}

func (f *fakeStore) install(t *testing.T) {
	t.Helper()
	origGetBead := graphGetBeadFunc
	origGetDeps := graphGetDepsFunc
	origGetComments := graphGetCommentsFunc
	origGitRunner := graphGitRunner
	t.Cleanup(func() {
		graphGetBeadFunc = origGetBead
		graphGetDepsFunc = origGetDeps
		graphGetCommentsFunc = origGetComments
		graphGitRunner = origGitRunner
	})
	graphGetBeadFunc = func(id string) (Bead, error) {
		b, ok := f.beads[id]
		if !ok {
			return Bead{}, fmt.Errorf("bead %s not found", id)
		}
		return b, nil
	}
	graphGetDepsFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return f.deps[id], nil
	}
	graphGetCommentsFunc = func(id string) ([]*beads.Comment, error) {
		return f.comments[id], nil
	}
	graphGitRunner = stubGit{} // default: fail every shellout (no git repo)
}

// makeDep is a constructor that mirrors the upstream
// IssueWithDependencyMetadata shape with just the fields the walker
// reads.
func makeDep(id, title, status, issueType string, depType beads.DependencyType) *beads.IssueWithDependencyMetadata {
	d := &beads.IssueWithDependencyMetadata{}
	d.ID = id
	d.Title = title
	d.Status = beads.Status(status)
	d.IssueType = beads.IssueType(issueType)
	d.DependencyType = depType
	return d
}

// stubGit is a programmable gitRunner. Each entry maps an arg-prefix
// to its output (or error).
type stubGit struct {
	// responses is matched in order; the first prefix that fully
	// matches the leading args wins.
	responses []stubGitResp
	// inRepo returns the response for `rev-parse --git-dir`. When
	// false, every git invocation errors.
	inRepo bool
	// calls records every invocation for assertions.
	calls *[][]string
}

type stubGitResp struct {
	args []string
	out  []byte
	err  error
}

func (s stubGit) Run(args ...string) ([]byte, error) {
	if s.calls != nil {
		copyArgs := append([]string(nil), args...)
		*s.calls = append(*s.calls, copyArgs)
	}
	if len(args) > 0 && args[0] == "rev-parse" {
		if s.inRepo {
			return []byte(".git\n"), nil
		}
		return nil, errors.New("not a git repo")
	}
	for _, r := range s.responses {
		if argsHavePrefix(args, r.args) {
			if r.err != nil {
				return nil, r.err
			}
			return r.out, nil
		}
	}
	return nil, fmt.Errorf("stubGit: no response for %v", args)
}

func argsHavePrefix(args, prefix []string) bool {
	if len(prefix) > len(args) {
		return false
	}
	for i := range prefix {
		if prefix[i] != args[i] {
			return false
		}
	}
	return true
}

// --- Walk tests ---

func TestGraph_OneHopWalkRendersBodyAndComments(t *testing.T) {
	fs := &fakeStore{
		beads: map[string]Bead{
			"spi-root":     {ID: "spi-root", Title: "Root task", Description: "Root body.", Status: "in_progress", Type: "task"},
			"spi-design-a": {ID: "spi-design-a", Title: "Design A", Description: "Design body.", Status: "closed", Type: "design"},
		},
		deps: map[string][]*beads.IssueWithDependencyMetadata{
			"spi-root": {makeDep("spi-design-a", "Design A", "closed", "design", beads.DepDiscoveredFrom)},
		},
		comments: map[string][]*beads.Comment{
			"spi-design-a": {{Author: "archmage", Text: "first comment"}, {Author: "wizard", Text: "second comment"}},
		},
	}
	fs.install(t)

	walk, err := buildGraphWalk("spi-root", graphOpts{depth: 1, rel: []string{"discovered-from"}, format: "text", maxBytesPerBead: 4096, maxBytesTotal: 32768})
	if err != nil {
		t.Fatalf("buildGraphWalk: %v", err)
	}
	if len(walk.Beads) != 2 {
		t.Fatalf("expected 2 beads in walk, got %d", len(walk.Beads))
	}
	if walk.Beads[0].ID != "spi-root" {
		t.Errorf("first bead = %q, want spi-root", walk.Beads[0].ID)
	}
	neighbor := walk.Beads[1]
	if neighbor.ID != "spi-design-a" {
		t.Errorf("neighbor = %q, want spi-design-a", neighbor.ID)
	}
	if neighbor.Description != "Design body." {
		t.Errorf("neighbor description = %q, want Design body.", neighbor.Description)
	}
	if len(neighbor.Comments) != 2 {
		t.Errorf("neighbor comments = %d, want 2", len(neighbor.Comments))
	}
	if got := neighbor.DepTypes; len(got) != 1 || got[0] != "discovered-from" {
		t.Errorf("DepTypes = %v, want [discovered-from]", got)
	}
}

func TestGraph_DepthTwoReachesGrandparent(t *testing.T) {
	fs := &fakeStore{
		beads: map[string]Bead{
			"spi-root":   {ID: "spi-root", Title: "Root", Status: "in_progress", Type: "task"},
			"spi-design": {ID: "spi-design", Title: "Design", Description: "Design body.", Status: "closed", Type: "design"},
			"spi-vision": {ID: "spi-vision", Title: "Vision", Description: "Vision body.", Status: "closed", Type: "design"},
		},
		deps: map[string][]*beads.IssueWithDependencyMetadata{
			"spi-root":   {makeDep("spi-design", "Design", "closed", "design", beads.DepDiscoveredFrom)},
			"spi-design": {makeDep("spi-vision", "Vision", "closed", "design", beads.DepDiscoveredFrom)},
		},
	}
	fs.install(t)

	walk, err := buildGraphWalk("spi-root", graphOpts{depth: 2, rel: []string{"discovered-from"}, format: "text", maxBytesPerBead: 4096, maxBytesTotal: 32768})
	if err != nil {
		t.Fatalf("buildGraphWalk: %v", err)
	}
	if len(walk.Beads) != 3 {
		t.Fatalf("expected 3 beads, got %d (ids=%v)", len(walk.Beads), nodeIDs(walk))
	}
	if !containsID(walk, "spi-vision") {
		t.Errorf("walk missing grandparent spi-vision: %v", nodeIDs(walk))
	}
}

func TestGraph_RelFilterExcludesRelated(t *testing.T) {
	fs := &fakeStore{
		beads: map[string]Bead{
			"spi-root":      {ID: "spi-root", Title: "Root", Type: "task"},
			"spi-design":    {ID: "spi-design", Title: "Design", Type: "design"},
			"spi-related":   {ID: "spi-related", Title: "Related sib", Type: "task"},
		},
		deps: map[string][]*beads.IssueWithDependencyMetadata{
			"spi-root": {
				makeDep("spi-design", "Design", "closed", "design", beads.DepDiscoveredFrom),
				makeDep("spi-related", "Related sib", "closed", "task", beads.DepRelated),
			},
		},
	}
	fs.install(t)

	walk, err := buildGraphWalk("spi-root", graphOpts{depth: 1, rel: []string{"discovered-from"}, format: "text", maxBytesPerBead: 4096, maxBytesTotal: 32768})
	if err != nil {
		t.Fatalf("buildGraphWalk: %v", err)
	}
	if containsID(walk, "spi-related") {
		t.Errorf("walk should not include spi-related: %v", nodeIDs(walk))
	}
	if !containsID(walk, "spi-design") {
		t.Errorf("walk missing spi-design: %v", nodeIDs(walk))
	}
}

func TestGraph_TypeFilterRestrictsToBugs(t *testing.T) {
	fs := &fakeStore{
		beads: map[string]Bead{
			"spi-root":   {ID: "spi-root", Title: "Root", Type: "task"},
			"spi-design": {ID: "spi-design", Title: "Design", Type: "design"},
			"spi-bug":    {ID: "spi-bug", Title: "Bug", Type: "bug"},
		},
		deps: map[string][]*beads.IssueWithDependencyMetadata{
			"spi-root": {
				makeDep("spi-design", "Design", "closed", "design", beads.DepDiscoveredFrom),
				makeDep("spi-bug", "Bug", "closed", "bug", beads.DepRelated),
			},
		},
	}
	fs.install(t)

	walk, err := buildGraphWalk("spi-root", graphOpts{
		depth:           1,
		rel:             []string{"discovered-from", "related"},
		types:           []string{"bug"},
		format:          "text",
		maxBytesPerBead: 4096,
		maxBytesTotal:   32768,
	})
	if err != nil {
		t.Fatalf("buildGraphWalk: %v", err)
	}
	if containsID(walk, "spi-design") {
		t.Errorf("walk should not include spi-design: %v", nodeIDs(walk))
	}
	if !containsID(walk, "spi-bug") {
		t.Errorf("walk missing spi-bug: %v", nodeIDs(walk))
	}
}

func TestGraph_InternalTypesFilteredSilently(t *testing.T) {
	fs := &fakeStore{
		beads: map[string]Bead{
			"spi-root":    {ID: "spi-root", Title: "Root", Type: "task"},
			"spi-attempt": {ID: "spi-attempt", Title: "attempt: wizard", Type: "attempt"},
			"spi-design":  {ID: "spi-design", Title: "Design", Description: "body", Type: "design"},
		},
		deps: map[string][]*beads.IssueWithDependencyMetadata{
			"spi-root": {
				makeDep("spi-attempt", "attempt: wizard", "in_progress", "attempt", beads.DepRelated),
				makeDep("spi-design", "Design", "closed", "design", beads.DepDiscoveredFrom),
			},
		},
	}
	fs.install(t)

	walk, err := buildGraphWalk("spi-root", graphOpts{
		depth:           1,
		rel:             []string{"discovered-from", "related"},
		format:          "text",
		maxBytesPerBead: 4096,
		maxBytesTotal:   32768,
	})
	if err != nil {
		t.Fatalf("buildGraphWalk: %v", err)
	}
	if containsID(walk, "spi-attempt") {
		t.Errorf("walk must not include internal attempt bead: %v", nodeIDs(walk))
	}
	if !containsID(walk, "spi-design") {
		t.Errorf("walk missing spi-design: %v", nodeIDs(walk))
	}
}

func TestGraph_PerBeadBodyCapTruncates(t *testing.T) {
	long := strings.Repeat("x", 5000)
	fs := &fakeStore{
		beads: map[string]Bead{
			"spi-root":   {ID: "spi-root", Title: "Root", Type: "task"},
			"spi-design": {ID: "spi-design", Title: "Design", Description: long, Status: "closed", Type: "design"},
		},
		deps: map[string][]*beads.IssueWithDependencyMetadata{
			"spi-root": {makeDep("spi-design", "Design", "closed", "design", beads.DepDiscoveredFrom)},
		},
	}
	fs.install(t)

	walk, err := buildGraphWalk("spi-root", graphOpts{
		depth:           1,
		rel:             []string{"discovered-from"},
		format:          "text",
		maxBytesPerBead: 100,
		maxBytesTotal:   32768,
	})
	if err != nil {
		t.Fatalf("buildGraphWalk: %v", err)
	}
	neighbor := nodeByID(walk, "spi-design")
	if neighbor == nil {
		t.Fatalf("walk missing spi-design")
	}
	if !neighbor.Truncated {
		t.Errorf("expected neighbor to be marked truncated")
	}
	if len(neighbor.Description) > 100 {
		t.Errorf("description not truncated: len=%d", len(neighbor.Description))
	}

	var buf bytes.Buffer
	if err := renderGraphText(walk, &buf); err != nil {
		t.Fatalf("renderGraphText: %v", err)
	}
	want := fmt.Sprintf(graphTruncationMarkerFmt, "spi-design")
	if !strings.Contains(buf.String(), want) {
		t.Errorf("text output missing per-bead truncation marker %q\nout:\n%s", want, buf.String())
	}
}

func TestGraph_TotalCapStopsExpansionAndEmitsGlobalMarker(t *testing.T) {
	long := strings.Repeat("y", 1500)
	fs := &fakeStore{
		beads: map[string]Bead{
			"spi-root": {ID: "spi-root", Title: "Root", Type: "task"},
			"spi-a":    {ID: "spi-a", Title: "A", Description: long, Status: "closed", Type: "task"},
			"spi-b":    {ID: "spi-b", Title: "B", Description: long, Status: "closed", Type: "task"},
			"spi-c":    {ID: "spi-c", Title: "C", Description: long, Status: "closed", Type: "task"},
		},
		deps: map[string][]*beads.IssueWithDependencyMetadata{
			"spi-root": {
				makeDep("spi-a", "A", "closed", "task", beads.DepRelated),
				makeDep("spi-b", "B", "closed", "task", beads.DepRelated),
				makeDep("spi-c", "C", "closed", "task", beads.DepRelated),
			},
		},
	}
	fs.install(t)

	walk, err := buildGraphWalk("spi-root", graphOpts{
		depth:           1,
		rel:             []string{"related"},
		format:          "text",
		maxBytesPerBead: 1500,
		maxBytesTotal:   2000,
	})
	if err != nil {
		t.Fatalf("buildGraphWalk: %v", err)
	}
	if !walk.Truncated {
		t.Errorf("expected walk.Truncated=true")
	}
	// Should have stopped before all three neighbors fit.
	if len(walk.Beads) >= 4 {
		t.Errorf("expected fewer than 4 beads (root + truncated), got %d", len(walk.Beads))
	}

	var buf bytes.Buffer
	if err := renderGraphText(walk, &buf); err != nil {
		t.Fatalf("renderGraphText: %v", err)
	}
	if !strings.Contains(buf.String(), graphWalkTruncatedMarker) {
		t.Errorf("text output missing global truncation marker\nout:\n%s", buf.String())
	}
}

func TestGraph_MultiDepTypeRendersOnceWithJoinedLabels(t *testing.T) {
	fs := &fakeStore{
		beads: map[string]Bead{
			"spi-root":   {ID: "spi-root", Title: "Root", Type: "task"},
			"spi-design": {ID: "spi-design", Title: "Design", Description: "body", Status: "closed", Type: "design"},
		},
		deps: map[string][]*beads.IssueWithDependencyMetadata{
			"spi-root": {
				makeDep("spi-design", "Design", "closed", "design", beads.DepDiscoveredFrom),
				makeDep("spi-design", "Design", "closed", "design", beads.DepRelated),
			},
		},
	}
	fs.install(t)

	walk, err := buildGraphWalk("spi-root", graphOpts{
		depth:           1,
		rel:             []string{"discovered-from", "related"},
		format:          "text",
		maxBytesPerBead: 4096,
		maxBytesTotal:   32768,
	})
	if err != nil {
		t.Fatalf("buildGraphWalk: %v", err)
	}
	count := 0
	for _, b := range walk.Beads {
		if b.ID == "spi-design" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected spi-design to render once, got %d", count)
	}
	dep := nodeByID(walk, "spi-design")
	if len(dep.DepTypes) != 2 {
		t.Errorf("expected 2 dep types on spi-design, got %v", dep.DepTypes)
	}
	if !contains(dep.DepTypes, "discovered-from") || !contains(dep.DepTypes, "related") {
		t.Errorf("expected discovered-from and related, got %v", dep.DepTypes)
	}
}

// --- --with-changes / --with-diffs tests ---

func TestGraph_WithChangesHappyPath(t *testing.T) {
	fs := &fakeStore{
		beads: map[string]Bead{
			"spi-root": {ID: "spi-root", Title: "Root", Type: "task"},
			"spi-prev": {
				ID:       "spi-prev",
				Title:    "Prev",
				Status:   "closed",
				Type:     "task",
				Metadata: map[string]string{"commits": `["abc123def4567"]`},
			},
		},
		deps: map[string][]*beads.IssueWithDependencyMetadata{
			"spi-root": {makeDep("spi-prev", "Prev", "closed", "task", beads.DepRelated)},
		},
	}
	fs.install(t)

	graphGitRunner = stubGit{
		inRepo: true,
		responses: []stubGitResp{
			{args: []string{"show", "--stat", "--format=%s", "abc123def4567"}, out: []byte("feat(spi-prev): subject\n\n file_a.go | 12 +++++++-----\n file_b.go |  3 +--\n 2 files changed, 8 insertions(+), 7 deletions(-)\n")},
			{args: []string{"show", "-s", "--format=%B", "abc123def4567"}, out: []byte("feat(spi-prev): subject\n")},
		},
	}

	walk, err := buildGraphWalk("spi-root", graphOpts{
		depth:           1,
		rel:             []string{"related"},
		withChanges:     true,
		format:          "text",
		maxBytesPerBead: 4096,
		maxBytesTotal:   32768,
	})
	if err != nil {
		t.Fatalf("buildGraphWalk: %v", err)
	}
	dep := nodeByID(walk, "spi-prev")
	if dep == nil {
		t.Fatalf("walk missing spi-prev")
	}
	if len(dep.Commits) != 1 {
		t.Fatalf("expected 1 commit, got %d", len(dep.Commits))
	}
	c := dep.Commits[0]
	if c.SHA != "abc123def4567" {
		t.Errorf("commit SHA = %q", c.SHA)
	}
	if c.Subject != "feat(spi-prev): subject" {
		t.Errorf("commit subject = %q", c.Subject)
	}
	if c.Source != "metadata" {
		t.Errorf("commit source = %q, want metadata", c.Source)
	}
	if len(c.Files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(c.Files))
	}
	if c.Files[0].Path != "file_a.go" {
		t.Errorf("first file path = %q", c.Files[0].Path)
	}
	if c.Files[0].Added == 0 || c.Files[0].Deleted == 0 {
		t.Errorf("first file +N -M = %d/%d, expected non-zero",
			c.Files[0].Added, c.Files[0].Deleted)
	}
}

func TestGraph_SquashFallbackUsesGitLogGrep(t *testing.T) {
	fs := &fakeStore{
		beads: map[string]Bead{
			"spi-root": {ID: "spi-root", Title: "Root", Type: "task"},
			"spi-prev": {
				ID:       "spi-prev",
				Title:    "Prev",
				Status:   "closed",
				Type:     "task",
				Metadata: map[string]string{"commits": `["unreachable123"]`},
			},
		},
		deps: map[string][]*beads.IssueWithDependencyMetadata{
			"spi-root": {makeDep("spi-prev", "Prev", "closed", "task", beads.DepRelated)},
		},
	}
	fs.install(t)

	graphGitRunner = stubGit{
		inRepo: true,
		responses: []stubGitResp{
			{args: []string{"show", "--stat", "--format=%s", "unreachable123"}, err: errors.New("fatal: bad object")},
			{args: []string{"log", "--grep", "spi-prev", "--all", "--format=%H%x09%s"}, out: []byte("squashedabc\tfeat(spi-prev): squashed subject\n")},
		},
	}

	walk, err := buildGraphWalk("spi-root", graphOpts{
		depth:           1,
		rel:             []string{"related"},
		withChanges:     true,
		format:          "text",
		maxBytesPerBead: 4096,
		maxBytesTotal:   32768,
	})
	if err != nil {
		t.Fatalf("buildGraphWalk: %v", err)
	}
	dep := nodeByID(walk, "spi-prev")
	if len(dep.Commits) != 1 {
		t.Fatalf("expected 1 grep-fallback commit, got %d", len(dep.Commits))
	}
	c := dep.Commits[0]
	if c.Source != "grep" {
		t.Errorf("expected source=grep, got %q", c.Source)
	}
	if c.SHA != "squashedabc" {
		t.Errorf("expected sha=squashedabc, got %q", c.SHA)
	}
	if !strings.Contains(c.Subject, "squashed subject") {
		t.Errorf("expected subject to contain 'squashed subject', got %q", c.Subject)
	}

	// And the text renderer marks it.
	var buf bytes.Buffer
	if err := renderGraphText(walk, &buf); err != nil {
		t.Fatalf("renderGraphText: %v", err)
	}
	if !strings.Contains(buf.String(), "(via grep, post-squash)") {
		t.Errorf("expected '(via grep, post-squash)' in text output, got:\n%s", buf.String())
	}
}

func TestGraph_MultiBeadCommitAnnotated(t *testing.T) {
	fs := &fakeStore{
		beads: map[string]Bead{
			"spi-root": {ID: "spi-root", Title: "Root", Type: "task"},
			"spi-prev": {
				ID:       "spi-prev",
				Title:    "Prev",
				Status:   "closed",
				Type:     "task",
				Metadata: map[string]string{"commits": `["abc123def4567"]`},
			},
		},
		deps: map[string][]*beads.IssueWithDependencyMetadata{
			"spi-root": {makeDep("spi-prev", "Prev", "closed", "task", beads.DepRelated)},
		},
	}
	fs.install(t)

	graphGitRunner = stubGit{
		inRepo: true,
		responses: []stubGitResp{
			{args: []string{"show", "--stat", "--format=%s", "abc123def4567"}, out: []byte("feat(spi-prev): joint commit\n\n file_a.go | 5 +++++\n 1 file changed, 5 insertions(+)\n")},
			{args: []string{"show", "-s", "--format=%B", "abc123def4567"}, out: []byte("feat(spi-prev,spi-other): joint commit\n\nThis touches both spi-prev and spi-other.\n")},
		},
	}

	walk, err := buildGraphWalk("spi-root", graphOpts{
		depth:           1,
		rel:             []string{"related"},
		withChanges:     true,
		format:          "text",
		maxBytesPerBead: 4096,
		maxBytesTotal:   32768,
	})
	if err != nil {
		t.Fatalf("buildGraphWalk: %v", err)
	}
	dep := nodeByID(walk, "spi-prev")
	if dep == nil || len(dep.Commits) == 0 {
		t.Fatalf("missing commit on spi-prev")
	}
	c := dep.Commits[0]
	if len(c.Files) == 0 {
		t.Fatalf("expected files on commit")
	}
	if !contains(c.Files[0].SharedWith, "spi-other") {
		t.Errorf("expected SharedWith to include spi-other, got %v", c.Files[0].SharedWith)
	}

	var buf bytes.Buffer
	if err := renderGraphText(walk, &buf); err != nil {
		t.Fatalf("renderGraphText: %v", err)
	}
	if !strings.Contains(buf.String(), "(shared with spi-other)") {
		t.Errorf("expected '(shared with spi-other)' in text output, got:\n%s", buf.String())
	}
}

func TestGraph_NotInGitRepoDegradesGracefully(t *testing.T) {
	fs := &fakeStore{
		beads: map[string]Bead{
			"spi-root": {ID: "spi-root", Title: "Root", Type: "task"},
			"spi-prev": {
				ID:       "spi-prev",
				Title:    "Prev",
				Status:   "closed",
				Type:     "task",
				Metadata: map[string]string{"commits": `["abc"]`},
			},
		},
		deps: map[string][]*beads.IssueWithDependencyMetadata{
			"spi-root": {makeDep("spi-prev", "Prev", "closed", "task", beads.DepRelated)},
		},
	}
	fs.install(t)
	graphGitRunner = stubGit{inRepo: false}

	walk, err := buildGraphWalk("spi-root", graphOpts{
		depth:           1,
		rel:             []string{"related"},
		withChanges:     true,
		format:          "text",
		maxBytesPerBead: 4096,
		maxBytesTotal:   32768,
	})
	if err != nil {
		t.Fatalf("buildGraphWalk: %v", err)
	}
	dep := nodeByID(walk, "spi-prev")
	if dep == nil {
		t.Fatalf("walk missing spi-prev")
	}
	if dep.CommitsNote == "" {
		t.Errorf("expected CommitsNote when not in git repo")
	}
	if len(dep.Commits) != 0 {
		t.Errorf("expected no commits when not in git repo, got %d", len(dep.Commits))
	}
}

func TestGraph_WithDiffsCapTruncates(t *testing.T) {
	bigDiff := strings.Repeat("d", 5000)
	fs := &fakeStore{
		beads: map[string]Bead{
			"spi-root": {ID: "spi-root", Title: "Root", Type: "task"},
			"spi-prev": {
				ID:       "spi-prev",
				Title:    "Prev",
				Status:   "closed",
				Type:     "task",
				Metadata: map[string]string{"commits": `["sha1"]`},
			},
		},
		deps: map[string][]*beads.IssueWithDependencyMetadata{
			"spi-root": {makeDep("spi-prev", "Prev", "closed", "task", beads.DepRelated)},
		},
	}
	fs.install(t)

	graphGitRunner = stubGit{
		inRepo: true,
		responses: []stubGitResp{
			{args: []string{"show", "--stat", "--format=%s", "sha1"}, out: []byte("feat(spi-prev): subj\n\n a.go | 2 +-\n 1 file changed, 1 insertion(+), 1 deletion(-)\n")},
			{args: []string{"show", "-s", "--format=%B", "sha1"}, out: []byte("feat(spi-prev): subj\n")},
			{args: []string{"show", "--no-color", "sha1"}, out: []byte(bigDiff)},
		},
	}

	walk, err := buildGraphWalk("spi-root", graphOpts{
		depth:           1,
		rel:             []string{"related"},
		withChanges:     true,
		withDiffs:       true,
		format:          "text",
		maxBytesPerBead: 256,
		maxBytesTotal:   32768,
	})
	if err != nil {
		t.Fatalf("buildGraphWalk: %v", err)
	}
	dep := nodeByID(walk, "spi-prev")
	if dep == nil || len(dep.Commits) == 0 {
		t.Fatalf("missing commits on spi-prev")
	}
	if !strings.Contains(dep.Commits[0].Diff, fmt.Sprintf(graphTruncationMarkerFmt, "spi-prev")) {
		t.Errorf("expected truncation marker in diff, got %q", dep.Commits[0].Diff)
	}
	if len(dep.Commits[0].Diff) > 256+128 { // marker adds a small constant
		t.Errorf("diff longer than expected cap: %d bytes", len(dep.Commits[0].Diff))
	}
}

// --- JSON output test ---

func TestGraph_JSONOutputSchema(t *testing.T) {
	fs := &fakeStore{
		beads: map[string]Bead{
			"spi-root":   {ID: "spi-root", Title: "Root", Description: "rd", Status: "in_progress", Type: "task"},
			"spi-design": {ID: "spi-design", Title: "Design", Description: "dd", Status: "closed", Type: "design"},
		},
		deps: map[string][]*beads.IssueWithDependencyMetadata{
			"spi-root": {makeDep("spi-design", "Design", "closed", "design", beads.DepDiscoveredFrom)},
		},
		comments: map[string][]*beads.Comment{
			"spi-design": {{Author: "archmage", Text: "comment"}},
		},
	}
	fs.install(t)

	walk, err := buildGraphWalk("spi-root", graphOpts{
		depth:           1,
		rel:             []string{"discovered-from"},
		format:          "json",
		maxBytesPerBead: 4096,
		maxBytesTotal:   32768,
	})
	if err != nil {
		t.Fatalf("buildGraphWalk: %v", err)
	}

	var buf bytes.Buffer
	if err := renderGraphJSON(walk, &buf); err != nil {
		t.Fatalf("renderGraphJSON: %v", err)
	}

	var parsed struct {
		Root  string `json:"root"`
		Beads []struct {
			ID          string `json:"id"`
			Type        string `json:"type"`
			Status      string `json:"status"`
			Title       string `json:"title"`
			Description string `json:"description"`
			Comments    []struct {
				Author string `json:"author"`
				Text   string `json:"text"`
			} `json:"comments"`
			DepTypes []string `json:"dep_types"`
			Depth    int      `json:"depth"`
		} `json:"beads"`
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("invalid JSON: %v\noutput:\n%s", err, buf.String())
	}
	if parsed.Root != "spi-root" {
		t.Errorf("Root = %q, want spi-root", parsed.Root)
	}
	if len(parsed.Beads) != 2 {
		t.Fatalf("expected 2 beads, got %d", len(parsed.Beads))
	}
	dep := parsed.Beads[1]
	if dep.ID != "spi-design" {
		t.Errorf("neighbor id = %q", dep.ID)
	}
	if dep.Description != "dd" {
		t.Errorf("neighbor description = %q", dep.Description)
	}
	if len(dep.Comments) != 1 {
		t.Errorf("neighbor comments = %d, want 1", len(dep.Comments))
	}
	if len(dep.DepTypes) != 1 || dep.DepTypes[0] != "discovered-from" {
		t.Errorf("dep_types = %v", dep.DepTypes)
	}
}

// --- Helpers ---

func nodeIDs(w *graphWalk) []string {
	ids := make([]string, 0, len(w.Beads))
	for _, b := range w.Beads {
		ids = append(ids, b.ID)
	}
	return ids
}

func nodeByID(w *graphWalk, id string) *graphNode {
	for i := range w.Beads {
		if w.Beads[i].ID == id {
			return &w.Beads[i]
		}
	}
	return nil
}

func containsID(w *graphWalk, id string) bool {
	for _, b := range w.Beads {
		if b.ID == id {
			return true
		}
	}
	return false
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
