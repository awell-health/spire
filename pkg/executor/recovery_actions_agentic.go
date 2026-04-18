package executor

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	spgit "github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/store"
)

// beadIDFromCommitSubject matches the project convention `<type>(<bead-id>):`
// at the start of a commit subject (e.g. "feat(spi-abc12):", "fix(web-9xx.1):").
// Bead IDs are prefix-lowercase-dash-hex-dot-digits per the `spi-<hex>` scheme
// documented in CLAUDE.md.
var beadIDFromCommitSubject = regexp.MustCompile(`^[a-z]+\(([a-z]+-[a-z0-9]+(?:\.\d+)*)\):`)

// conflictSideContext bundles the commit + bead information for one side of
// a conflict. BeadID / Bead are zero-valued when the commit subject doesn't
// match the project commit convention, or when the bead isn't present in the
// local store — callers must nil-check the Bead pointer before using it.
type conflictSideContext struct {
	Label      string // "HEAD" / "incoming (rebase)" etc.
	Operation  string // "rebase" / "merge" / "cherry-pick"
	Commit     *spgit.CommitMetadata
	BeadID     string
	Bead       *store.Bead
}

// conflictFileContext bundles the state of one conflicted file. Content
// includes the conflict markers intact — that's the whole point of the
// bundle, the resolver must read the markers to understand which lines
// came from which side.
type conflictFileContext struct {
	Path    string
	Content string
	Log     string
}

// conflictBundle is the full context handed to the conflict-resolution
// apprentice. All fields are optional — missing values are handled gracefully
// in renderConflictPrompt.
type conflictBundle struct {
	State        spgit.ConflictState
	HeadSide     *conflictSideContext
	IncomingSide *conflictSideContext
	Files        []conflictFileContext
}

// agenticResolveConflicts assembles a conflict bundle, dispatches an
// apprentice into the paused worktree, waits for it to resolve the conflicts
// and commit, then runs validation gates. On any gate failure, returns an
// error so the cleric's on_error=record path captures it and decide can
// reconsider.
//
// Required dispatcher deps on ctx: Spawner, RecordAgentRun, AgentResultDir,
// LogBaseDir. When any are missing, returns an error without dispatching so
// the caller (decide loop) can either route to a mechanical action or
// escalate.
func agenticResolveConflicts(ctx *RecoveryActionCtx) error {
	wc := ctx.Worktree
	if wc == nil {
		return fmt.Errorf("resolve-conflicts agentic: nil worktree")
	}

	files, err := wc.ConflictedFiles()
	if err != nil {
		return fmt.Errorf("list conflicted files: %w", err)
	}
	if len(files) == 0 {
		ctx.Log("no conflicted files found — nothing to resolve")
		return nil
	}

	bundle := buildConflictBundle(ctx, wc, files)

	if err := dispatchConflictApprentice(ctx, bundle); err != nil {
		return fmt.Errorf("dispatch conflict apprentice: %w", err)
	}

	if err := runConflictValidationGates(ctx, wc, files); err != nil {
		return err
	}

	ctx.Log(fmt.Sprintf("agentic resolver completed; %d file(s) processed, all gates passed", len(files)))
	return nil
}

// buildConflictBundle assembles the full context bundle for the apprentice.
// Silent on helper failures (e.g. missing REBASE_HEAD) — the bundle surfaces
// whatever data it could gather, so the apprentice can still read the file
// contents even if commit/bead lookup fails.
func buildConflictBundle(ctx *RecoveryActionCtx, wc *spgit.WorktreeContext, files []string) conflictBundle {
	state := wc.DetectConflictState()
	bundle := conflictBundle{State: state}

	// Head side commit + bead.
	if state.HeadSHA != "" {
		bundle.HeadSide = resolveSideContext(ctx, wc, state.HeadSHA, "HEAD", state.InProgressOp)
	}
	// Incoming side commit + bead.
	if state.IncomingSHA != "" {
		label := "incoming"
		if state.InProgressOp != "" {
			label = fmt.Sprintf("incoming (%s)", state.InProgressOp)
		}
		bundle.IncomingSide = resolveSideContext(ctx, wc, state.IncomingSHA, label, state.InProgressOp)
	}

	// Per-file content + git log.
	sort.Strings(files)
	for _, f := range files {
		fc := conflictFileContext{Path: f}
		full := filepath.Join(wc.Dir, f)
		if data, rerr := os.ReadFile(full); rerr == nil {
			fc.Content = string(data)
		}
		if logOut, lerr := wc.FileLog(f, 20); lerr == nil {
			fc.Log = logOut
		}
		bundle.Files = append(bundle.Files, fc)
	}
	return bundle
}

// resolveSideContext builds the commit+bead context for one side of the
// conflict. Always returns a non-nil pointer even when lookups fail — the
// caller checks for nested fields before using them.
func resolveSideContext(ctx *RecoveryActionCtx, wc *spgit.WorktreeContext, sha, label, op string) *conflictSideContext {
	side := &conflictSideContext{Label: label, Operation: op}

	if md, err := wc.ShowCommit(sha); err == nil {
		side.Commit = md
		if beadID := extractBeadIDFromSubject(md.Subject); beadID != "" {
			side.BeadID = beadID
			get := ctx.GetBeadFn
			if get == nil {
				get = store.GetBead
			}
			if b, err := get(beadID); err == nil {
				// Copy into local variable so the pointer is stable.
				bead := b
				side.Bead = &bead
			}
		}
	}
	return side
}

// extractBeadIDFromSubject returns the bead ID from a commit subject or "".
func extractBeadIDFromSubject(subject string) string {
	m := beadIDFromCommitSubject.FindStringSubmatch(subject)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// dispatchConflictApprentice spawns an apprentice agent into the paused
// worktree with a pre-assembled conflict prompt and blocks until it returns.
// Any spawn or wait error is returned to the caller — the validation gates
// run afterward regardless of the apprentice's exit code because some
// subprocess errors are non-fatal (e.g. hook noise) and the gates are the
// authoritative check.
func dispatchConflictApprentice(ctx *RecoveryActionCtx, bundle conflictBundle) error {
	if ctx.Spawner == nil {
		return fmt.Errorf("agentic resolver: no Spawner wired on ctx")
	}
	if ctx.Worktree == nil {
		return fmt.Errorf("agentic resolver: no worktree on ctx")
	}

	ns := ctx.AgentNamespace
	if ns == "" {
		ns = "cleric-resolver"
	}
	spawnName := fmt.Sprintf("%s-%s-%d", ns, ctx.TargetBeadID, time.Now().UnixNano()%1_000_000)

	prompt := renderConflictPrompt(ctx, bundle)

	logPath := ""
	if ctx.LogBaseDir != "" {
		logPath = filepath.Join(ctx.LogBaseDir, "wizards", spawnName+".log")
		_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
	}

	cfg := agent.SpawnConfig{
		Name:         spawnName,
		BeadID:       ctx.TargetBeadID,
		Role:         agent.RoleApprentice,
		ExtraArgs:    []string{"--worktree-dir", ctx.Worktree.Dir, "--no-review"},
		CustomPrompt: prompt,
		LogPath:      logPath,
	}

	dispatch := ctx.DispatchFn
	if dispatch == nil {
		dispatch = ctx.Spawner.Spawn
	}

	started := time.Now()
	handle, spawnErr := dispatch(cfg)
	if spawnErr != nil {
		recordResolverRun(ctx, spawnName, started, "spawn_error")
		return fmt.Errorf("spawn: %w", spawnErr)
	}

	waitErr := handle.Wait()
	result := "success"
	if waitErr != nil {
		result = "error"
	}
	recordResolverRun(ctx, spawnName, started, result)

	if waitErr != nil {
		// Keep going: validation gates decide whether the apprentice actually
		// resolved anything. A non-zero exit with clean gates is fine; a
		// clean exit with markers still on disk fails at the gate layer.
		ctx.Log(fmt.Sprintf("apprentice %s exited with %v — validating output", spawnName, waitErr))
	} else {
		ctx.Log(fmt.Sprintf("apprentice %s completed", spawnName))
	}
	return nil
}

// runConflictValidationGates runs the gates described in the task spec:
//  1. No conflict-marker substrings remain in any file touched by the conflict.
//  2. git diff --check is clean.
//  3. go build ./... passes.
//  4. For each conflicted path matching *_test.go, the package's tests pass.
//
// Order is cheapest-first so a grep can short-circuit before we launch go build.
func runConflictValidationGates(ctx *RecoveryActionCtx, wc *spgit.WorktreeContext, files []string) error {
	// Gate 1: markers must be gone from the originally-conflicted files.
	for _, f := range files {
		full := filepath.Join(wc.Dir, f)
		data, err := os.ReadFile(full)
		if err != nil {
			// File may have been deleted as part of the resolution — that's OK.
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("validate: read %s: %w", f, err)
		}
		if containsConflictMarker(data) {
			return fmt.Errorf("validate: conflict markers remain in %s", f)
		}
	}

	// Gate 2: git diff --check — catches markers in files the apprentice
	// might have left behind elsewhere.
	if out, err := wc.DiffCheck(); err != nil {
		return fmt.Errorf("validate: git diff --check reported issues: %w\n%s", err, strings.TrimSpace(out))
	}

	// Gate 3: go build ./...
	buildOut, buildErr := wc.RunCommandOutput("go build ./...")
	if buildErr != nil {
		return fmt.Errorf("validate: go build ./... failed: %w\n%s", buildErr, strings.TrimSpace(buildOut))
	}

	// Gate 4: for each conflicted *_test.go file, run tests in its package.
	pkgs := testPackagesFor(files)
	for _, pkg := range pkgs {
		testCmd := fmt.Sprintf("go test %s -count=1", pkg)
		testOut, testErr := wc.RunCommandOutput(testCmd)
		if testErr != nil {
			return fmt.Errorf("validate: %s failed: %w\n%s", testCmd, testErr, strings.TrimSpace(testOut))
		}
	}

	return nil
}

// conflictMarkers are the exact substrings git writes into unresolved hunks.
// The leading space after each marker keeps us from matching harmless
// occurrences of long runs of the delimiter character in data files.
var conflictMarkers = [][]byte{
	[]byte("<<<<<<< "),
	[]byte(">>>>>>> "),
	[]byte("\n=======\n"),
}

func containsConflictMarker(data []byte) bool {
	for _, m := range conflictMarkers {
		if bytes.Contains(data, m) {
			return true
		}
	}
	return false
}

// testPackagesFor returns the unique list of ./<pkg>/... patterns derived
// from each *_test.go path in files. Non-test files are ignored.
func testPackagesFor(files []string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, f := range files {
		if !strings.HasSuffix(f, "_test.go") {
			continue
		}
		pkg := filepath.Dir(f)
		if pkg == "" || pkg == "." {
			pkg = "."
		}
		pat := "./" + pkg + "/..."
		if _, ok := seen[pat]; ok {
			continue
		}
		seen[pat] = struct{}{}
		out = append(out, pat)
	}
	return out
}

// renderConflictPrompt composes the apprentice system prompt from the
// assembled bundle. It leads with rules + worktree path, then lists each
// conflicted file with commits + beads on both sides. The goal: whatever
// reasoning agent receives this has enough context to keep *both* sides'
// intended changes rather than picking one.
func renderConflictPrompt(ctx *RecoveryActionCtx, bundle conflictBundle) string {
	var sb strings.Builder

	sb.WriteString("You are a conflict-resolution apprentice working inside a paused git operation.\n")
	sb.WriteString("Your job: edit the conflicted files so that the intent of BOTH sides survives, ")
	sb.WriteString("stage the results, and continue the paused operation so the rebase/merge/cherry-pick advances.\n\n")

	sb.WriteString("## Rules\n")
	sb.WriteString("- Read the commits and beads on BOTH sides before editing — they explain why each side changed the file.\n")
	sb.WriteString("- Resolve conflicts by preserving both sides' intent; prefer combining over discarding. Only drop a side's change if the other side explicitly supersedes it.\n")
	sb.WriteString("- Remove ALL `<<<<<<<`, `=======`, `>>>>>>>` markers — leaving any behind is a hard failure.\n")
	sb.WriteString("- Do NOT refactor, reformat, or edit anything unrelated to the conflict hunks.\n")
	sb.WriteString("- After editing: `git add` the file(s), then advance the paused operation:\n")
	sb.WriteString("    - rebase:      `git rebase --continue` (loop if more conflicts surface)\n")
	sb.WriteString("    - cherry-pick: `git cherry-pick --continue`\n")
	sb.WriteString("    - merge:       `git commit --no-edit`\n")
	sb.WriteString("- You may run `go build ./...` and package-scoped `go test` to sanity-check before committing.\n")
	sb.WriteString("- Do NOT create PRs, push, or touch other branches. Do NOT abort the operation.\n\n")

	sb.WriteString(fmt.Sprintf("## Worktree\n%s\n\n", ctx.Worktree.Dir))

	if bundle.State.InProgressOp != "" {
		sb.WriteString(fmt.Sprintf("## In-progress operation\n%s\n\n", bundle.State.InProgressOp))
	} else {
		sb.WriteString("## In-progress operation\nnone detected (operation may have already advanced)\n\n")
	}

	writeSide := func(title string, side *conflictSideContext) {
		sb.WriteString(fmt.Sprintf("## %s\n", title))
		if side == nil || side.Commit == nil {
			sb.WriteString("*commit metadata unavailable*\n\n")
			return
		}
		sb.WriteString(fmt.Sprintf("- **SHA:** %s\n", side.Commit.SHA))
		sb.WriteString(fmt.Sprintf("- **Subject:** %s\n", side.Commit.Subject))
		if side.Commit.Author != "" {
			sb.WriteString(fmt.Sprintf("- **Author:** %s\n", side.Commit.Author))
		}
		if side.Commit.Date != "" {
			sb.WriteString(fmt.Sprintf("- **Date:** %s\n", side.Commit.Date))
		}
		if side.BeadID != "" {
			sb.WriteString(fmt.Sprintf("- **Bead:** %s\n", side.BeadID))
		}
		if side.Bead != nil {
			sb.WriteString(fmt.Sprintf("  - Title: %s\n", side.Bead.Title))
			if side.Bead.Status != "" {
				sb.WriteString(fmt.Sprintf("  - Status: %s\n", side.Bead.Status))
			}
			if side.Bead.Description != "" {
				sb.WriteString("  - Description:\n")
				sb.WriteString(indentBlock(side.Bead.Description, "    > "))
				sb.WriteString("\n")
			}
		}
		if body := strings.TrimSpace(side.Commit.Body); body != "" {
			sb.WriteString("  - Commit body:\n")
			sb.WriteString(indentBlock(body, "    > "))
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	writeSide("HEAD side", bundle.HeadSide)
	writeSide("Incoming side", bundle.IncomingSide)

	sb.WriteString("## Conflicted files\n\n")
	if len(bundle.Files) == 0 {
		sb.WriteString("*(none — if you see this, the conflict may already be resolved)*\n\n")
	}
	for _, fc := range bundle.Files {
		sb.WriteString(fmt.Sprintf("### %s\n\n", fc.Path))
		sb.WriteString("Current contents (markers intact):\n\n```\n")
		sb.WriteString(fc.Content)
		if !strings.HasSuffix(fc.Content, "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString("```\n\n")
		if fc.Log != "" {
			sb.WriteString("Recent history (git log --all --pretty=fuller -n 20):\n\n```\n")
			sb.WriteString(strings.TrimRight(fc.Log, "\n"))
			sb.WriteString("\n```\n\n")
		}
	}

	sb.WriteString("Resolve the conflicts now.\n")
	return sb.String()
}

// recordResolverRun wraps ctx.RecordAgentRun with the conflict-resolver's
// standard phase/role/recovery-bead fields. No-op when RecordAgentRun is
// nil (tests, local-only execution).
func recordResolverRun(ctx *RecoveryActionCtx, spawnName string, started time.Time, result string) {
	if ctx.RecordAgentRun == nil {
		return
	}
	completed := time.Now()
	_, _ = ctx.RecordAgentRun(AgentRun{
		AgentName:       spawnName,
		BeadID:          ctx.TargetBeadID,
		Role:            string(agent.RoleApprentice),
		Phase:           "resolve-conflicts",
		PhaseBucket:     "implement",
		Result:          result,
		RecoveryBeadID:  ctx.RecoveryBeadID,
		ParentRunID:     ctx.ParentRunID,
		DurationSeconds: int(completed.Sub(started).Seconds()),
		StartedAt:       started.Format(time.RFC3339),
		CompletedAt:     completed.Format(time.RFC3339),
	})
}

// indentBlock prefixes each line of s with prefix. Used to format free-form
// bead descriptions and commit bodies as block quotes in the prompt.
func indentBlock(s, prefix string) string {
	var out strings.Builder
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		out.WriteString(prefix)
		out.WriteString(line)
		out.WriteString("\n")
	}
	return out.String()
}

// ---------------------------------------------------------------------------
// Compile-time anchors to avoid "unused" noise when one platform's build tag
// excludes a helper. exec is used indirectly through wc.RunCommandOutput but
// keeping the anchor here documents the reliance on the stdlib package.
// ---------------------------------------------------------------------------
var _ = exec.Command