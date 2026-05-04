package git

// stash.go — primitive that stashes uncommitted work in a worktree so that a
// subsequent dispatch can run on a clean tree. The dispatch-time workspace
// validator in pkg/executor used to refuse on a dirty tree; that produced a
// deadlock when the apprentice died mid-edit because the cleric dispatched to
// recover hit the same refusal. Stashing preserves the work (operator can
// `git stash pop` later) while letting the next agent operate on a clean
// workspace. No policy or logging lives here — the validator wraps the call,
// builds the stash message, and posts the comment on the parent bead.

import (
	"fmt"
	"os/exec"
	"strings"
)

// StashResult describes the stash entry produced by StashUncommitted.
//
// When the worktree is clean, StashUncommitted returns the zero value with a
// nil error — Created is false and the other fields are empty. Callers must
// check Created before reading Ref/SHA/Files. The zero-value path lets the
// caller treat "nothing to stash" as a benign no-op rather than an error.
type StashResult struct {
	// Created is true when a stash entry was actually produced. False when
	// the worktree was clean at stash time.
	Created bool
	// Ref is the symbolic stash ref (`stash@{0}`) immediately after the
	// push. The ref's index shifts as new stashes are added; for a stable
	// handle, use SHA.
	Ref string
	// SHA is the full commit SHA of the stash entry. Stable across later
	// stash pushes — operators inspect with `git stash show -p <sha>`.
	SHA string
	// Files lists the paths included in the stash, captured from
	// `git stash show --name-only stash@{0}`.
	Files []string
}

// StashUncommitted runs `git stash push -u -m message` at path, capturing the
// resulting stash ref, SHA, and file list. The `-u` flag includes untracked
// files so half-edited new files are preserved alongside modified tracked
// files.
//
// Behavior:
//   - Clean worktree: returns StashResult{Created: false} and a nil error.
//     Git's own "No local changes to save" output is recognized; nothing is
//     created on the stash list.
//   - Successful push: returns StashResult{Created: true, Ref: "stash@{0}",
//     SHA: <commit-sha>, Files: [<file>...]} and a nil error.
//   - Push failure (e.g. corrupted index, unwritable git dir): returns the
//     zero value plus a wrapped error. The validator falls back to its old
//     refusal behavior in that case.
//
// path must be a worktree (linked or main); message is the human-readable
// stash subject. The validator uses `spire-autoStash:<bead-id>:<step>` so
// operators tracing `git stash list` can find the originating task.
func StashUncommitted(path, message string) (StashResult, error) {
	if path == "" {
		return StashResult{}, fmt.Errorf("stash uncommitted: empty path")
	}
	if message == "" {
		return StashResult{}, fmt.Errorf("stash uncommitted at %s: empty message", path)
	}

	// Capture the file list from porcelain status BEFORE the push.
	// `git stash show --name-only` only shows the tracked-file diff; the
	// untracked entries live on a separate parent commit and aren't
	// surfaced in the default name-only output. Reading porcelain status
	// before the push gives us a single, consistent list that includes
	// both modified-tracked and untracked-`-u`-captured paths.
	files, err := porcelainFiles(path)
	if err != nil {
		return StashResult{}, fmt.Errorf("read dirty files at %s: %w", path, err)
	}

	out, err := exec.Command("git", "-C", path, "stash", "push", "-u", "-m", message).CombinedOutput()
	if err != nil {
		return StashResult{}, fmt.Errorf("git stash push at %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}

	// Git prints "No local changes to save" when the tree is clean and the
	// push is a no-op. Treat as a benign zero-value result so callers don't
	// have to special-case the empty-tree path themselves.
	if strings.Contains(strings.ToLower(string(out)), "no local changes to save") {
		return StashResult{}, nil
	}

	// `git rev-parse stash@{0}` resolves the ref produced by the push above
	// to a stable SHA. The ref itself shifts as later stashes are added;
	// the SHA is what operators paste into `git stash show -p`.
	shaOut, err := exec.Command("git", "-C", path, "rev-parse", "stash@{0}").Output()
	if err != nil {
		return StashResult{}, fmt.Errorf("git rev-parse stash@{0} at %s: %w", path, err)
	}
	sha := strings.TrimSpace(string(shaOut))

	return StashResult{
		Created: true,
		Ref:     "stash@{0}",
		SHA:     sha,
		Files:   files,
	}, nil
}

// porcelainFiles returns the path component of every entry in
// `git status --porcelain`. Used by StashUncommitted to pre-capture the file
// list before the stash push; see the comment at the call site for why we
// don't read this from `git stash show`.
//
// Porcelain v1 lines have the form `XY <path>` (or `XY <orig> -> <renamed>`
// for renames). We always take the rightmost path so a renamed entry tracks
// the new name, matching how operators see it on disk.
func porcelainFiles(path string) ([]string, error) {
	out, err := exec.Command("git", "-C", path, "status", "--porcelain").Output()
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if len(line) < 4 {
			continue
		}
		entry := line[3:]
		if i := strings.Index(entry, " -> "); i >= 0 {
			entry = entry[i+4:]
		}
		files = append(files, entry)
	}
	return files, nil
}
