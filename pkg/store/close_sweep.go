package store

import (
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"

	"github.com/awell-health/spire/pkg/config"
)

// closeSweepCommand is the seam through which the post-close sweep
// runs `git log --grep`. Tests can swap it out to inject deterministic
// output without touching a real repo. Mirrors the gitRunner pattern in
// cmd/spire/graph.go: a function-shaped seam keeps the production path
// real (os/exec) while keeping the unit tests hermetic.
var closeSweepCommand = func(repoPath, beadID string) (string, error) {
	out, err := exec.Command("git", "-C", repoPath,
		"log", "--all", "--no-color", "--oneline",
		"--grep="+beadID).Output()
	return string(out), err
}

// closeSweepAppendCommit is the seam through which the sweep records
// the SHAs it discovered. Tests install a fake so they can verify
// writes without going through the dispatched store layer. Bound at
// init() time to AppendBeadMetadataList; declaring without an initial
// value avoids Go's static init-cycle complaint (the closure-value
// would otherwise transitively reference the sweep through the
// AppendBeadMetadataList → SetBeadMetadata → UpdateBead → sweep
// dependency chain).
var closeSweepAppendCommit func(id, key, value string) error

func init() {
	closeSweepAppendCommit = func(id, key, value string) error {
		return AppendBeadMetadataList(id, key, value)
	}
}

// repoPathLookupFunc resolves the local on-disk path for a bead's repo
// prefix. Tests swap it to bypass config.Load() and inject a fake path
// (or simulate "prefix unbound" by returning ""). Mirrors the existing
// fxn-shape seams in this package.
var repoPathLookupFunc = func(prefix string) string {
	if prefix == "" {
		return ""
	}
	cfg, err := config.Load()
	if err != nil || cfg == nil {
		return ""
	}
	inst, ok := cfg.Instances[prefix]
	if !ok || inst == nil {
		return ""
	}
	return inst.Path
}

// sweepWG tracks in-flight close sweeps so tests can wait for them
// to drain. Production callers don't need to touch it; the sweep is
// fire-and-forget (process exit is the only thing that aborts it).
var sweepWG sync.WaitGroup

// WaitCloseSweeps blocks until every in-flight close sweep finishes.
// Intended for tests that close a bead and immediately verify the
// resulting metadata.commits[]. Production code should never call this
// — the sweep is best-effort and silent.
func WaitCloseSweeps() {
	sweepWG.Wait()
}

// firePostCloseSweep starts the best-effort grep sweep on a bead that
// just transitioned into the closed state. It runs asynchronously so
// the close transaction is not held up by a slow git invocation; all
// failures are logged at debug-level and silently swallowed (no git
// repo, no commits found, prefix unbound — none of these should
// surface as errors to the user closing the bead).
//
// The sweep:
//  1. Resolves the bead's prefix → registered repo path (silent no-op
//     when unmapped — that's a valid configuration for a tower that
//     coordinates beads but doesn't host the repo locally).
//  2. Runs `git log --all --no-color --oneline --grep=<bead-id>` to
//     find every commit that referenced the bead in its message
//     (the convention enforced by every spire repo's CLAUDE.md).
//  3. Parses `<sha> <subject>` lines and appends novel SHAs to the
//     bead's metadata.commits[] via AppendBeadMetadataList — which
//     dedupes by exact value, so re-running the sweep is a no-op.
//
// All work happens inside a goroutine; sweepWG tracks it so tests
// can drain. The sweep is INTENTIONALLY post-commit: the close
// transaction has already succeeded by the time we start, so a sweep
// failure cannot revert the close.
func firePostCloseSweep(beadID string) {
	if beadID == "" {
		return
	}
	prefix := PrefixFromID(beadID)
	if prefix == "" {
		return
	}

	sweepWG.Add(1)
	go func() {
		defer sweepWG.Done()
		runCloseSweep(beadID, prefix)
	}()
}

// runCloseSweep is the synchronous body of firePostCloseSweep. Pulled
// out so tests that prefer to drive the sweep deterministically can
// call it directly without spawning a goroutine.
func runCloseSweep(beadID, prefix string) {
	repoPath := repoPathLookupFunc(prefix)
	if repoPath == "" {
		// Prefix isn't bound to a local repo on this machine. That's
		// not an error — it just means the sweep silently no-ops.
		log.Printf("[store] close-sweep: skipped %s (prefix %q unbound)", beadID, prefix)
		return
	}

	// Sanity-check the registered path is actually a git work tree
	// before invoking git log; otherwise the user gets a "fatal: not
	// a git repository" line in the daemon log on every close, and
	// the sweep would silently produce no commits anyway.
	if err := ensureRepoOK(repoPath); err != nil {
		log.Printf("[store] close-sweep: skipped %s (%s not a git work tree: %v)", beadID, repoPath, err)
		return
	}

	out, err := closeSweepCommand(repoPath, beadID)
	if err != nil {
		log.Printf("[store] close-sweep: git log for %s failed (best-effort, skipping): %v", beadID, err)
		return
	}

	shas := parseGrepShas(out)
	if len(shas) == 0 {
		return
	}
	for _, sha := range shas {
		if appendErr := closeSweepAppendCommit(beadID, "commits", sha); appendErr != nil {
			log.Printf("[store] close-sweep: append commit %s to %s metadata: %v", sha, beadID, appendErr)
			// Continue: one append failure shouldn't block the rest.
		}
	}
}

// parseGrepShas pulls SHAs from `git log --oneline` output. Each line
// is "<sha> <subject>" (or just "<sha>"); we keep the first whitespace-
// separated token and skip empty/garbage rows.
func parseGrepShas(out string) []string {
	if out == "" {
		return nil
	}
	var shas []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		sha := fields[0]
		// Defense against malformed lines: a real short SHA is hex and
		// at least 4 chars. We don't want to dump arbitrary strings
		// into metadata.commits[].
		if !looksLikeSHA(sha) {
			continue
		}
		shas = append(shas, sha)
	}
	return shas
}

// looksLikeSHA returns true if s is plausibly an abbreviated git SHA
// (hex, 4..40 chars). Strict enough to filter parse errors; loose
// enough to accept the various oneline lengths git emits.
func looksLikeSHA(s string) bool {
	if len(s) < 4 || len(s) > 40 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// readPriorBeadStatus best-effort reads a bead's current status before
// an UpdateBead call so the caller can detect "first close" transitions
// and only fire the sweep on real (prior != closed) → closed moves.
// Errors are swallowed — if the read fails we err on the side of
// firing the sweep (idempotent-by-AppendBeadMetadataList anyway).
func readPriorBeadStatus(beadID string) string {
	if beadID == "" {
		return ""
	}
	b, err := GetBead(beadID)
	if err != nil {
		return ""
	}
	return string(b.Status)
}

// firePostCloseSweepIfTransitioned guards firePostCloseSweep so it only
// fires when the bead's status actually transitions into closed (prior
// status was not already "closed"). Without the guard, every replay or
// admin re-close would re-run grep needlessly and append duplicate
// SHAs (the AppendBeadMetadataList dedupe absorbs duplicates, but the
// shellout is still wasted work).
//
// priorStatus is the status the caller observed BEFORE the update.
// Pass "" if unknown — in that case we conservatively fire (matches
// AppendBeadMetadataList's idempotent-by-SHA behaviour).
func firePostCloseSweepIfTransitioned(beadID, priorStatus string) {
	if strings.EqualFold(priorStatus, "closed") {
		return
	}
	firePostCloseSweep(beadID)
}

// ensureRepoOK is a soft-validation helper: sanity-check that
// repoPath actually points at a git repository before we run git
// commands inside it. Avoids spamming logs with "fatal: not a git
// repository" on towers that happen to register a non-git path.
// Function-shaped so tests can stub it out the same way the other
// close-sweep seams do.
var ensureRepoOK = func(repoPath string) error {
	out, err := exec.Command("git", "-C", repoPath, "rev-parse", "--is-inside-work-tree").Output()
	if err != nil {
		return fmt.Errorf("git rev-parse: %w", err)
	}
	if strings.TrimSpace(string(out)) != "true" {
		return fmt.Errorf("not inside a git work tree: %s", repoPath)
	}
	return nil
}
