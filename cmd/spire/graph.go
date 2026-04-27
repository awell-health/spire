// graph.go implements `spire graph`: walk and render a bead's semantic
// neighborhood (linked beads' titles, descriptions, comments, status)
// with optional --with-changes and --with-diffs expansion that surfaces
// the commits and file lists that closed those neighbors.
//
// Defaults: depth=1, semantic dep types only (discovered-from, related,
// caused-by — excludes parent-child / blocks / blocked-by /
// conditional-blocks). Internal bead types (message, step, attempt,
// review) are dropped from the walk silently.
//
// Output format defaults to text. --format=json emits a stable schema;
// the schema is currently EXPERIMENTAL until the prompt-integration
// consumer (separate feature) lands.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/awell-health/spire/pkg/store"
	"github.com/spf13/cobra"
	"github.com/steveyegge/beads"
)

// --- Defaults ---

const (
	graphDefaultDepth         = 1
	graphDefaultMaxBytesBead  = 4096
	graphDefaultMaxBytesTotal = 32768
	graphDefaultRel           = "discovered-from,related,caused-by"
)

// graphTruncationMarkerFmt is appended inside a bead section when the
// per-bead byte cap is hit while rendering its body or comments.
const graphTruncationMarkerFmt = "[truncated, run bd show %s for the rest]"

// graphWalkTruncatedMarker is emitted once at the end of the walk when
// --max-bytes-total is exceeded and further nodes are dropped.
const graphWalkTruncatedMarker = "[walk truncated at --max-bytes-total; rerun with higher cap]"

// --- Test-replaceable seams ---
//
// These mirror the seam pattern used in close_advance.go / cmdClose
// tests: package-level vars that point at the live bridge functions
// and can be swapped in *_test.go without spinning a real store.
var (
	graphGetBeadFunc     = storeGetBead
	graphGetDepsFunc     = storeGetDepsWithMeta
	graphGetCommentsFunc = storeGetComments
	graphGitRunner       gitRunner = realGitRunner{}
)

// --- Git seam ---

// gitRunner abstracts the git shellouts used by --with-changes /
// --with-diffs so tests can inject deterministic output (and verify the
// squash-fallback path) without touching a real repo.
type gitRunner interface {
	// Run executes git with the given args from cwd. Returns stdout on
	// success; on non-zero exit returns the error and the captured
	// stderr is folded into err.Error() via os/exec.
	Run(args ...string) ([]byte, error)
}

type realGitRunner struct{}

func (realGitRunner) Run(args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	out, err := cmd.Output()
	return out, err
}

// --- Cobra command ---

var graphCmd = &cobra.Command{
	Use:   "graph <bead-id>",
	Short: "Walk and render a bead's semantic neighborhood with optional commit-metadata expansion",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		opts, err := parseGraphOpts(cmd)
		if err != nil {
			return err
		}
		return cmdGraph(args[0], opts, os.Stdout)
	},
}

func init() {
	graphCmd.Flags().StringSlice("types", nil, "Filter neighbor beads by issue type (e.g. design,task,bug)")
	graphCmd.Flags().StringSlice("rel", nil, "Filter dep types to walk (default: "+graphDefaultRel+")")
	graphCmd.Flags().Int("depth", graphDefaultDepth, "Walk depth (1 = direct neighbors only)")
	graphCmd.Flags().Bool("with-changes", false, "Include commit subjects + file list + line-stats per closed bead")
	graphCmd.Flags().Bool("with-diffs", false, "Include diff hunks per closed bead (size-capped)")
	graphCmd.Flags().String("format", "text", "Output format: text | json")
	graphCmd.Flags().Int("max-bytes-per-bead", graphDefaultMaxBytesBead, "Per-bead body/diff truncation cap")
	graphCmd.Flags().Int("max-bytes-total", graphDefaultMaxBytesTotal, "Total walk byte budget")
}

// graphOpts holds parsed flag values.
type graphOpts struct {
	types           []string
	rel             []string
	depth           int
	withChanges     bool
	withDiffs       bool
	format          string
	maxBytesPerBead int
	maxBytesTotal   int
}

func parseGraphOpts(cmd *cobra.Command) (graphOpts, error) {
	o := graphOpts{}
	o.types, _ = cmd.Flags().GetStringSlice("types")
	o.rel, _ = cmd.Flags().GetStringSlice("rel")
	if len(o.rel) == 0 {
		o.rel = strings.Split(graphDefaultRel, ",")
	}
	o.depth, _ = cmd.Flags().GetInt("depth")
	if o.depth < 1 {
		o.depth = 1
	}
	o.withChanges, _ = cmd.Flags().GetBool("with-changes")
	o.withDiffs, _ = cmd.Flags().GetBool("with-diffs")
	o.format, _ = cmd.Flags().GetString("format")
	if o.format != "text" && o.format != "json" {
		return o, fmt.Errorf("unknown --format %q (want text|json)", o.format)
	}
	o.maxBytesPerBead, _ = cmd.Flags().GetInt("max-bytes-per-bead")
	if o.maxBytesPerBead <= 0 {
		o.maxBytesPerBead = graphDefaultMaxBytesBead
	}
	o.maxBytesTotal, _ = cmd.Flags().GetInt("max-bytes-total")
	if o.maxBytesTotal <= 0 {
		o.maxBytesTotal = graphDefaultMaxBytesTotal
	}
	// --with-diffs implies --with-changes (diff requires the SHA list).
	if o.withDiffs {
		o.withChanges = true
	}
	return o, nil
}

// cmdGraph is the command entrypoint.
func cmdGraph(rootID string, opts graphOpts, out io.Writer) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	walk, err := buildGraphWalk(rootID, opts)
	if err != nil {
		return err
	}

	switch opts.format {
	case "json":
		return renderGraphJSON(walk, out)
	default:
		return renderGraphText(walk, out)
	}
}

// --- Walker ---

// graphNode is a single bead in the walk result, fully expanded with
// description, comments, deps, and (optionally) commit metadata.
type graphNode struct {
	ID          string         `json:"id"`
	Type        string         `json:"type"`
	Status      string         `json:"status"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	Comments    []graphComment `json:"comments,omitempty"`
	// DepTypes lists every dependency-type label via which this node
	// was reached during the walk. Empty for the root.
	DepTypes  []string      `json:"dep_types,omitempty"`
	Depth     int           `json:"depth"`
	Truncated bool          `json:"truncated,omitempty"`
	Commits   []graphCommit `json:"commits,omitempty"`
	// CommitsNote is set when --with-changes was requested but the
	// command is running outside a git repo; describes the degraded
	// behavior so consumers don't think there were no commits.
	CommitsNote string `json:"commits_note,omitempty"`
}

// graphComment is a single bead comment.
type graphComment struct {
	Author string `json:"author,omitempty"`
	Text   string `json:"text"`
}

// graphCommit describes one closing commit on a bead.
type graphCommit struct {
	SHA     string       `json:"sha"`
	Subject string       `json:"subject,omitempty"`
	Files   []graphFile  `json:"files,omitempty"`
	Diff    string       `json:"diff,omitempty"`
	// Source is "metadata" when the SHA came from bead.metadata.commits[]
	// and "grep" when it came from the post-squash `git log --grep`
	// fallback.
	Source string `json:"source"`
}

// graphFile is one path touched by a commit.
type graphFile struct {
	Path       string   `json:"path"`
	Added      int      `json:"added"`
	Deleted    int      `json:"deleted"`
	SharedWith []string `json:"shared_with,omitempty"`
}

// graphWalk is the full assembled result.
type graphWalk struct {
	Root      string      `json:"root"`
	Beads     []graphNode `json:"beads"`
	Truncated bool        `json:"truncated,omitempty"`
}

// buildGraphWalk performs BFS from rootID and returns the assembled walk
// honoring depth, type, dep-type, and byte-budget filters.
func buildGraphWalk(rootID string, opts graphOpts) (*graphWalk, error) {
	root, err := graphGetBeadFunc(rootID)
	if err != nil {
		return nil, fmt.Errorf("graph %s: %w", rootID, err)
	}
	// Filter the root by issue type only if --types was given. The
	// root being internal would still be allowed since the user asked
	// for it explicitly.
	walk := &graphWalk{Root: rootID}

	relFilter := makeRelFilter(opts.rel)
	typeFilter := makeTypeFilter(opts.types)

	visited := make(map[string]int) // beadID -> index in walk.Beads
	depMap := make(map[string]map[string]struct{})

	// Approximate running-byte counter — checked after each node is
	// finalized. We err on the side of letting one more bead through
	// rather than mid-section truncation; the per-bead cap handles
	// per-bead bloat.
	totalBytes := 0
	noteRoot := graphMakeNode(root, 0, opts.maxBytesPerBead, nil)
	if opts.withChanges {
		attachCommitMetadata(&noteRoot, opts)
	}
	totalBytes += graphNodeApproxBytes(noteRoot)
	walk.Beads = append(walk.Beads, noteRoot)
	visited[root.ID] = 0
	depMap[root.ID] = map[string]struct{}{}

	type queueItem struct {
		id    string
		depth int
	}
	queue := []queueItem{{id: root.ID, depth: 0}}

	for len(queue) > 0 {
		head := queue[0]
		queue = queue[1:]
		if head.depth >= opts.depth {
			continue
		}

		deps, derr := graphGetDepsFunc(head.id)
		if derr != nil {
			// Surface the error on stderr but keep walking; partial
			// graph output is more useful than an empty error.
			fmt.Fprintf(os.Stderr, "spire graph: warning: load deps for %s: %v\n", head.id, derr)
			continue
		}

		for _, dep := range deps {
			if dep == nil {
				continue
			}
			depTypeStr := string(dep.DependencyType)
			if !relFilter[depTypeStr] {
				continue
			}
			beadType := string(dep.IssueType)
			if store.InternalTypes[beadType] {
				continue
			}
			if !typeFilter(beadType) {
				continue
			}
			// Total byte budget: stop adding new nodes once exceeded,
			// but still annotate the dep edge on existing nodes
			// (cheaper than dropping the multi-edge information).
			if idx, seen := visited[dep.ID]; seen {
				if _, dup := depMap[dep.ID][depTypeStr]; !dup {
					depMap[dep.ID][depTypeStr] = struct{}{}
					walk.Beads[idx].DepTypes = appendUniqueSorted(walk.Beads[idx].DepTypes, depTypeStr)
				}
				continue
			}
			if totalBytes >= opts.maxBytesTotal {
				walk.Truncated = true
				continue
			}
			full, ferr := graphGetBeadFunc(dep.ID)
			if ferr != nil {
				fmt.Fprintf(os.Stderr, "spire graph: warning: load bead %s: %v\n", dep.ID, ferr)
				continue
			}
			node := graphMakeNode(full, head.depth+1, opts.maxBytesPerBead, []string{depTypeStr})
			if opts.withChanges {
				attachCommitMetadata(&node, opts)
			}
			totalBytes += graphNodeApproxBytes(node)
			visited[full.ID] = len(walk.Beads)
			depMap[full.ID] = map[string]struct{}{depTypeStr: {}}
			walk.Beads = append(walk.Beads, node)

			if head.depth+1 < opts.depth {
				queue = append(queue, queueItem{id: full.ID, depth: head.depth + 1})
			}
		}
	}

	return walk, nil
}

// graphMakeNode builds a graphNode for bead b. Description and comments
// share the per-bead byte budget; once exceeded the section is
// truncated and the truncation marker is appended.
func graphMakeNode(b Bead, depth, maxBytesPerBead int, depTypes []string) graphNode {
	n := graphNode{
		ID:       b.ID,
		Type:     b.Type,
		Status:   b.Status,
		Title:    b.Title,
		Depth:    depth,
		DepTypes: depTypes,
	}

	// Root (depth 0) renders the full body without truncation — the
	// user asked for it explicitly, so we don't apply the neighbor cap.
	if depth == 0 {
		n.Description = b.Description
		comments, _ := graphGetCommentsFunc(b.ID)
		for _, c := range comments {
			if c == nil {
				continue
			}
			n.Comments = append(n.Comments, graphComment{Author: c.Author, Text: c.Text})
		}
		return n
	}

	budget := maxBytesPerBead
	desc, used, truncated := truncatePiece(b.Description, budget)
	n.Description = desc
	budget -= used
	n.Truncated = truncated

	if budget > 0 && !truncated {
		comments, _ := graphGetCommentsFunc(b.ID)
		for _, c := range comments {
			if c == nil {
				continue
			}
			line := c.Text
			authorBytes := 0
			if c.Author != "" {
				authorBytes = len(c.Author) + 4 // "[X]: "
			}
			needed := authorBytes + len(line)
			if needed > budget {
				cut, used, _ := truncatePiece(line, budget-authorBytes)
				_ = used
				if strings.TrimSpace(cut) != "" {
					n.Comments = append(n.Comments, graphComment{Author: c.Author, Text: cut})
				}
				n.Truncated = true
				break
			}
			n.Comments = append(n.Comments, graphComment{Author: c.Author, Text: line})
			budget -= needed
		}
	}

	return n
}

// truncatePiece returns the prefix of s that fits in budget bytes. The
// second return is the number of bytes consumed (== len(returned
// string)); the third is whether truncation occurred. If budget <= 0
// the empty string is returned with truncated=true (caller must then
// emit the marker).
func truncatePiece(s string, budget int) (string, int, bool) {
	if budget <= 0 {
		if s == "" {
			return "", 0, false
		}
		return "", 0, true
	}
	if len(s) <= budget {
		return s, len(s), false
	}
	return s[:budget], budget, true
}

// graphNodeApproxBytes is a cheap approximation of the rendered size of
// a node; only used for the global budget gate.
func graphNodeApproxBytes(n graphNode) int {
	total := len(n.Title) + len(n.Description) + len(n.ID) + len(n.Type) + len(n.Status)
	for _, c := range n.Comments {
		total += len(c.Author) + len(c.Text) + 8
	}
	for _, c := range n.Commits {
		total += len(c.SHA) + len(c.Subject) + len(c.Diff)
		for _, f := range c.Files {
			total += len(f.Path) + 16
		}
	}
	return total
}

// makeRelFilter returns a set of allowed dep-type labels.
func makeRelFilter(rel []string) map[string]bool {
	out := make(map[string]bool, len(rel))
	for _, r := range rel {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		out[r] = true
	}
	return out
}

// makeTypeFilter returns a predicate; if types is empty, all
// non-internal types pass. Internal types are filtered separately.
func makeTypeFilter(types []string) func(string) bool {
	if len(types) == 0 {
		return func(string) bool { return true }
	}
	allowed := make(map[string]bool, len(types))
	for _, t := range types {
		t = strings.TrimSpace(t)
		if t != "" {
			allowed[t] = true
		}
	}
	return func(t string) bool { return allowed[t] }
}

// appendUniqueSorted appends v to xs only if not already present, then
// sorts the result. Stable order makes test assertions and rendering
// deterministic.
func appendUniqueSorted(xs []string, v string) []string {
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	xs = append(xs, v)
	sort.Strings(xs)
	return xs
}

// --- Commit metadata ---

// beadIDPattern matches `<prefix>-<token>` style bead IDs (e.g.
// `spi-gynkyj`). Conservative: alphanumeric prefix + dash + alphanumeric
// token of at least 4 chars.
var beadIDPattern = regexp.MustCompile(`\b[a-z0-9]+-[a-z0-9]{4,}\b`)

// attachCommitMetadata reads bead.metadata.commits[] and resolves each
// SHA via git. Falls back to `git log --grep <bead-id>` when a SHA is
// unreachable (squash-merge case). Only runs for *closed* beads —
// in_progress / open beads haven't produced a closing commit yet.
//
// Best-effort: degrades to a friendly note when not in a git repo
// rather than failing the whole walk.
func attachCommitMetadata(n *graphNode, opts graphOpts) {
	if n.Status != string(beads.StatusClosed) {
		return
	}
	if !inGitRepo() {
		n.CommitsNote = "(not in a git repo; --with-changes/--with-diffs degraded)"
		return
	}

	shas := readCommitMetadata(*n)
	seen := make(map[string]struct{}, len(shas))
	for _, sha := range shas {
		if _, dup := seen[sha]; dup {
			continue
		}
		seen[sha] = struct{}{}
		commit, ok := readCommitFromSHA(n.ID, sha, opts)
		if !ok {
			// SHA unreachable. Leave it for the post-loop fallback;
			// don't emit a half-populated entry here.
			continue
		}
		n.Commits = append(n.Commits, commit)
	}

	// If we got nothing from metadata (or every SHA was unreachable),
	// fall back to grepping the log for the bead ID. This catches the
	// post-squash case where the original commits no longer exist on
	// any branch.
	if len(n.Commits) == 0 {
		grepHits := readCommitsFromGrep(n.ID)
		for _, g := range grepHits {
			n.Commits = append(n.Commits, g)
		}
	}
}

// readCommitMetadata parses the JSON-encoded "commits" array stored
// under the bead's metadata. Returns the SHA list (possibly empty).
func readCommitMetadata(n graphNode) []string {
	// We don't keep the raw map on the node; refetch the bead to read
	// metadata. Cheap because the walker already cached the bead, but
	// keeping the metadata coupling local to this helper makes the
	// graphNode struct narrower and the renderer easier to test.
	b, err := graphGetBeadFunc(n.ID)
	if err != nil {
		return nil
	}
	if b.Metadata == nil {
		return nil
	}
	raw := b.Metadata["commits"]
	if raw == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		// Tolerate a single-string value (older format) — wrap it.
		if strings.HasPrefix(raw, "[") {
			return nil
		}
		return []string{raw}
	}
	return out
}

// readCommitFromSHA shells out to git to materialize a commit's
// subject + file list (and optional diff). Returns ok=false when the
// SHA isn't reachable so the caller can fall back to the grep path.
func readCommitFromSHA(beadID, sha string, opts graphOpts) (graphCommit, bool) {
	out, err := graphGitRunner.Run("show", "--stat", "--format=%s", sha)
	if err != nil {
		return graphCommit{}, false
	}
	subject, files := parseShowStat(string(out))

	commit := graphCommit{
		SHA:     sha,
		Subject: subject,
		Files:   files,
		Source:  "metadata",
	}

	// Multi-bead annotation: parse the full message body for other
	// bead IDs and stamp them onto every file in this commit.
	body, err := graphGitRunner.Run("show", "-s", "--format=%B", sha)
	if err == nil {
		others := otherBeadIDs(beadID, string(body))
		if len(others) > 0 {
			for i := range commit.Files {
				commit.Files[i].SharedWith = others
			}
		}
	}

	if opts.withDiffs {
		// Per-bead diff cap is shared with body+comments. Use a
		// generous slice of the per-bead cap so the description
		// already rendered above doesn't entirely starve the diff.
		// This is intentionally simple — the global cap protects the
		// total walk size.
		raw, derr := graphGitRunner.Run("show", "--no-color", sha)
		if derr == nil {
			cap := opts.maxBytesPerBead
			if cap <= 0 {
				cap = graphDefaultMaxBytesBead
			}
			if len(raw) > cap {
				commit.Diff = string(raw[:cap]) + "\n" + fmt.Sprintf(graphTruncationMarkerFmt, beadID)
			} else {
				commit.Diff = string(raw)
			}
		}
	}

	return commit, true
}

// readCommitsFromGrep falls back to `git log --grep <bead-id>` and
// surfaces the matching subjects. Used when bead.metadata.commits is
// empty or every SHA is unreachable (post-squash).
func readCommitsFromGrep(beadID string) []graphCommit {
	out, err := graphGitRunner.Run("log", "--grep", beadID, "--all", "--format=%H%x09%s")
	if err != nil {
		return nil
	}
	var commits []graphCommit
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) < 2 {
			continue
		}
		commits = append(commits, graphCommit{
			SHA:     parts[0],
			Subject: parts[1],
			Source:  "grep",
		})
	}
	return commits
}

// parseShowStat parses `git show --stat --format=%s <sha>` output.
// First line is the subject; subsequent lines are the diffstat in the
// form `path | N +-` followed by a summary line.
func parseShowStat(out string) (string, []graphFile) {
	lines := strings.Split(out, "\n")
	if len(lines) == 0 {
		return "", nil
	}
	subject := strings.TrimSpace(lines[0])
	var files []graphFile
	for _, line := range lines[1:] {
		// Format: " path | N +++--"
		// Or summary: " 5 files changed, 12 insertions(+), 3 deletions(-)"
		if !strings.Contains(line, "|") {
			continue
		}
		parts := strings.SplitN(line, "|", 2)
		path := strings.TrimSpace(parts[0])
		stat := strings.TrimSpace(parts[1])
		if path == "" {
			continue
		}
		// Count +/- markers in the stat column. Binary files render
		// as "Bin <n> -> <m>" — record those with zero counts.
		added, deleted := countDiffMarks(stat)
		files = append(files, graphFile{Path: path, Added: added, Deleted: deleted})
	}
	return subject, files
}

// countDiffMarks counts the leading numeric total and the
// `+`/`-` markers in a diffstat fragment like "12 ++++++--".
// `Bin <n> -> <m>` returns 0,0 since byte counts aren't comparable.
func countDiffMarks(stat string) (int, int) {
	if strings.HasPrefix(stat, "Bin") {
		return 0, 0
	}
	// Split out the numeric prefix.
	fields := strings.Fields(stat)
	if len(fields) == 0 {
		return 0, 0
	}
	total, err := strconv.Atoi(fields[0])
	if err != nil || total == 0 {
		return 0, 0
	}
	added, deleted := 0, 0
	for _, c := range strings.Join(fields[1:], "") {
		switch c {
		case '+':
			added++
		case '-':
			deleted++
		}
	}
	if added == 0 && deleted == 0 {
		// No markers (renamed without lines, or unrecognized form):
		// keep the total in `added` so callers can still see the
		// touched-line count.
		return total, 0
	}
	return added, deleted
}

// otherBeadIDs returns bead IDs found in the message body that are not
// equal to self. Order is deterministic (first-occurrence order with
// dedup).
func otherBeadIDs(self, body string) []string {
	matches := beadIDPattern.FindAllString(body, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := map[string]struct{}{self: {}}
	var others []string
	for _, m := range matches {
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		others = append(others, m)
	}
	return others
}

// inGitRepo returns true when running inside a git repo (i.e. `git
// rev-parse --git-dir` succeeds). Used so --with-changes / --with-diffs
// degrade gracefully when invoked from a non-git cwd.
func inGitRepo() bool {
	_, err := graphGitRunner.Run("rev-parse", "--git-dir")
	return err == nil
}

// --- Renderers ---

// renderGraphText emits the human-readable walk format. The start bead
// is rendered first with full body and no DepTypes label.
func renderGraphText(walk *graphWalk, w io.Writer) error {
	for i, n := range walk.Beads {
		if i > 0 {
			fmt.Fprintln(w)
		}
		if i == 0 {
			fmt.Fprintf(w, "=== %s [%s/%s] %s\n", n.ID, n.Type, n.Status, n.Title)
		} else {
			depLabel := ""
			if len(n.DepTypes) > 0 {
				depLabel = " (" + strings.Join(n.DepTypes, ", ") + ")"
			}
			fmt.Fprintf(w, "=== %s [%s/%s] %s%s\n", n.ID, n.Type, n.Status, n.Title, depLabel)
		}
		if n.Description != "" {
			for _, line := range strings.Split(n.Description, "\n") {
				fmt.Fprintf(w, "  %s\n", line)
			}
		}
		if len(n.Comments) > 0 {
			fmt.Fprintln(w, "  Comments:")
			for _, c := range n.Comments {
				if c.Author != "" {
					fmt.Fprintf(w, "    [%s] %s\n", c.Author, c.Text)
				} else {
					fmt.Fprintf(w, "    %s\n", c.Text)
				}
			}
		}
		if n.Truncated {
			fmt.Fprintf(w, "  %s\n", fmt.Sprintf(graphTruncationMarkerFmt, n.ID))
		}
		if n.CommitsNote != "" {
			fmt.Fprintf(w, "  %s\n", n.CommitsNote)
		}
		if len(n.Commits) > 0 {
			fmt.Fprintln(w, "  Commits:")
			for _, c := range n.Commits {
				renderTextCommit(w, c)
			}
		}
	}
	if walk.Truncated {
		fmt.Fprintln(w)
		fmt.Fprintln(w, graphWalkTruncatedMarker)
	}
	return nil
}

func renderTextCommit(w io.Writer, c graphCommit) {
	tag := ""
	if c.Source == "grep" {
		tag = " (via grep, post-squash)"
	}
	short := c.SHA
	if len(short) > 12 {
		short = short[:12]
	}
	fmt.Fprintf(w, "    %s %s%s\n", short, c.Subject, tag)
	for _, f := range c.Files {
		shared := ""
		if len(f.SharedWith) > 0 {
			shared = " (shared with " + strings.Join(f.SharedWith, ", ") + ")"
		}
		fmt.Fprintf(w, "      %s  +%d -%d%s\n", f.Path, f.Added, f.Deleted, shared)
	}
	if c.Diff != "" {
		fmt.Fprintln(w, "      diff:")
		for _, line := range strings.Split(strings.TrimRight(c.Diff, "\n"), "\n") {
			fmt.Fprintf(w, "        %s\n", line)
		}
	}
}

// renderGraphJSON emits the walk as JSON with stable field names.
//
// SCHEMA STABILITY: This schema is currently EXPERIMENTAL — fields may
// be added, renamed, or restructured before the prompt-integration
// consumer (separate feature) lands. Programmatic consumers should pin
// to a specific spire version while it stabilizes.
func renderGraphJSON(walk *graphWalk, w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(walk)
}
