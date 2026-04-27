package store

import (
	"encoding/json"
	"os/exec"
	"strings"
)

// CommitRef is one commit row returned by LookupBeadCommits, paired
// with reachability info. SHA is the abbreviated or full SHA recorded
// for the bead. Reachable means `git cat-file -e <sha>^{commit}`
// confirmed the commit object still exists in the local repo (post-
// squash merges may render previously-recorded SHAs unreachable).
// Source distinguishes write-time records from close-time grep
// fallback so renderers can display "(squash-merged elsewhere)".
type CommitRef struct {
	SHA       string `json:"sha"`
	Reachable bool   `json:"reachable"`
	Source    string `json:"source"`            // "metadata" | "grep"
	Subject   string `json:"subject,omitempty"` // commit subject (oneline summary)
}

// lookupCommitsRunner is the seam through which LookupBeadCommits
// shells out to git. Mirrors closeSweepCommand — tests inject a fake
// runner to verify the helper without touching a real repo.
type lookupCommitsRunner interface {
	// LogGrep returns the raw output of `git log --grep=<beadID>
	// --all --no-color --oneline` from repoPath.
	LogGrep(repoPath, beadID string) (string, error)
	// CatFileExists returns true when `git cat-file -e <sha>^{commit}`
	// succeeds (the commit is reachable in repoPath).
	CatFileExists(repoPath, sha string) bool
	// LogSubject returns the commit subject for sha via
	// `git show --no-patch --format=%s <sha>`. Empty on error.
	LogSubject(repoPath, sha string) string
}

type realLookupRunner struct{}

func (realLookupRunner) LogGrep(repoPath, beadID string) (string, error) {
	out, err := exec.Command("git", "-C", repoPath,
		"log", "--all", "--no-color", "--oneline",
		"--grep="+beadID).Output()
	return string(out), err
}

func (realLookupRunner) CatFileExists(repoPath, sha string) bool {
	cmd := exec.Command("git", "-C", repoPath, "cat-file", "-e", sha+"^{commit}")
	return cmd.Run() == nil
}

func (realLookupRunner) LogSubject(repoPath, sha string) string {
	out, err := exec.Command("git", "-C", repoPath,
		"show", "--no-patch", "--format=%s", sha).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// lookupRunnerVar is the package-level seam tests swap.
var lookupRunnerVar lookupCommitsRunner = realLookupRunner{}

// getBeadMetadataForLookup is the seam through which LookupBeadCommits
// reads `metadata.commits[]`. Defaults to the real GetBeadMetadata; tests
// install a fake to bypass the dispatched store. Pulled out as a separate
// var so swapping it doesn't disturb instance_meta.go's own seam.
var getBeadMetadataForLookup = GetBeadMetadata

// LookupBeadCommits returns every commit referenced by beadID, merging
// two sources:
//
//  1. metadata.commits[] — written at land-time by the wizard's
//     recordCommitMetadata. Source: "metadata".
//  2. `git log --grep=<bead-id>` against repoPath — picks up commits
//     missed by the wizard path (other apprentice flows, recovery,
//     human commits, squash-merge SHAs on main). Source: "grep".
//
// Each returned CommitRef is reachability-checked via `git cat-file
// -e <sha>^{commit}`. When a wizard-recorded SHA is unreachable
// post-squash, the helper transparently falls back to grep-found
// SHAs (which are reachable on main even after squash) so callers
// always see at least one Reachable=true row when the bead's work
// is still present in the tree.
//
// Dedupe rules:
//   - If a SHA appears in both sources, the metadata row wins (it's
//     the write-time canonical record); the grep row is dropped.
//   - Subject is filled from `git show --no-patch --format=%s`; if
//     git fails (e.g. unreachable SHA), Subject stays empty.
//
// The helper is read-only: it never writes to the bead's metadata.
// All git failures degrade gracefully (empty result, never error).
func LookupBeadCommits(beadID, repoPath string) ([]CommitRef, error) {
	if beadID == "" || repoPath == "" {
		return nil, nil
	}

	var refs []CommitRef
	seen := make(map[string]int) // sha → index into refs

	// Source 1: metadata.commits[]
	for _, sha := range readMetadataCommits(beadID) {
		if !looksLikeSHA(sha) {
			continue
		}
		if _, dup := seen[sha]; dup {
			continue
		}
		ref := CommitRef{
			SHA:       sha,
			Source:    "metadata",
			Reachable: lookupRunnerVar.CatFileExists(repoPath, sha),
		}
		if ref.Reachable {
			ref.Subject = lookupRunnerVar.LogSubject(repoPath, sha)
		}
		refs = append(refs, ref)
		seen[sha] = len(refs) - 1
	}

	// Source 2: git log --grep
	out, err := lookupRunnerVar.LogGrep(repoPath, beadID)
	if err == nil {
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			sha, subject := splitOneline(line)
			if !looksLikeSHA(sha) {
				continue
			}
			if _, dup := seen[sha]; dup {
				// Metadata row already covered this SHA — but if its
				// Subject is empty (e.g. unreachable from metadata),
				// patch it with the oneline subject we just got.
				idx := seen[sha]
				if refs[idx].Subject == "" && subject != "" {
					refs[idx].Subject = subject
				}
				continue
			}
			ref := CommitRef{
				SHA:       sha,
				Source:    "grep",
				Reachable: lookupRunnerVar.CatFileExists(repoPath, sha),
				Subject:   subject,
			}
			refs = append(refs, ref)
			seen[sha] = len(refs) - 1
		}
	}

	return refs, nil
}

// readMetadataCommits returns the parsed metadata.commits[] list for
// beadID, decoded from the JSON-array form that AppendBeadMetadataList
// writes. Returns nil when no entries exist or the bead is missing.
func readMetadataCommits(beadID string) []string {
	meta, err := getBeadMetadataForLookup(beadID)
	if err != nil {
		return nil
	}
	if meta == nil {
		return nil
	}
	raw := meta["commits"]
	if raw == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		// Tolerate the "single string" legacy shape too — early
		// recordCommitMetadata might have written a bare string in
		// pre-v0.40 towers. We don't expect to see it now, but
		// erroring out would block the helper for those beads.
		if raw != "" && !strings.HasPrefix(raw, "[") {
			return []string{strings.Trim(raw, `" `)}
		}
		return nil
	}
	return out
}

// splitOneline splits a `git log --oneline` line into its SHA prefix
// and remaining subject text.
func splitOneline(line string) (sha, subject string) {
	parts := strings.SplitN(line, " ", 2)
	if len(parts) == 0 {
		return "", ""
	}
	sha = strings.TrimSpace(parts[0])
	if len(parts) > 1 {
		subject = strings.TrimSpace(parts[1])
	}
	return sha, subject
}
