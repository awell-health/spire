package executor

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	spgit "github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
)

// conflictSideContext bundles the commit + bead information for one side of
// a conflict. BeadID / Bead are zero-valued when the commit subject doesn't
// match the project commit convention, or when the bead isn't present in the
// local store — callers must nil-check the Bead pointer before using it.
type conflictSideContext struct {
	Label     string // "HEAD" / "incoming (rebase)" etc.
	Operation string // "rebase" / "merge" / "cherry-pick"
	Commit    *spgit.CommitMetadata
	BeadID    string
	Bead      *store.Bead
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

// repairWorkerAction is the closed set of plan.Action values that
// SpawnRepairWorker will dispatch. Anything outside this set errors
// rather than silently succeeding (spi-6wiz9): a new worker role must
// be added explicitly.
var repairWorkerActions = map[string]bool{
	"resolve-conflicts": true,
	"resummon":          true,
	"reset":             true,
	"triage":            true,
	"targeted-fix":      true,
}

// SpawnRepairWorker is the canonical RepairModeWorker entrypoint. It
// dispatches on plan.Action to one of the repair-role specific apprentice
// spawns — all of which share a single SpawnConfig construction site
// through ctx.BuildRuntimeContract, so local and k8s backends both
// receive the canonical Identity/Workspace/RunContext required by the
// substrate validator.
//
//   - "resolve-conflicts": assemble a conflict bundle for the paused
//     workspace, dispatch a resolver apprentice, and run the
//     conflict-specific validation gates.
//   - "resummon" / "reset" / "triage" / "targeted-fix": dispatch a
//     generic repair apprentice with plan context; the cleric's verify
//     step (VerifyPlan) is the authoritative success check — no
//     conflict-marker gates apply here.
//   - empty or unknown action: return an error so decide/execute can
//     reconsider. Previously an empty action plus zero conflicted files
//     silently returned success; that short-circuit is gone (spi-6wiz9).
//
// ws is the workspace the plan selected (borrowed from the target bead's
// wizard today). When ctx.Worktree is nil, SpawnRepairWorker reconstructs
// a WorktreeContext from ws so conflict helpers find the repo without
// callers having to thread a pre-built context.
//
// Required dispatcher deps on ctx: Spawner, BuildRuntimeContract (wired
// by buildRecoveryActionCtx), RecordAgentRun (optional), LogBaseDir
// (optional). When Spawner or BuildRuntimeContract are missing the
// function returns an error without dispatching so the caller can either
// retry on a different mode or escalate.
func SpawnRepairWorker(ctx *RecoveryActionCtx, plan recovery.RepairPlan, ws WorkspaceHandle) (RepairWorkerResult, error) {
	if ctx.Worktree == nil {
		ctx.Worktree = worktreeFromHandle(ws)
	}
	wc := ctx.Worktree
	if wc == nil || wc.Dir == "" {
		return RepairWorkerResult{}, fmt.Errorf("spawn repair worker: no workspace")
	}
	if plan.Action == "" {
		return RepairWorkerResult{}, fmt.Errorf("spawn repair worker: plan.Action is empty — decide must set a canonical action")
	}
	if !repairWorkerActions[plan.Action] {
		return RepairWorkerResult{}, fmt.Errorf("spawn repair worker: unsupported action %q (known: resolve-conflicts, resummon, reset, triage, targeted-fix)", plan.Action)
	}

	if plan.Action == "resolve-conflicts" {
		return spawnConflictResolverWorker(ctx, plan, ws, wc)
	}
	return spawnGenericRepairWorker(ctx, plan, ws, wc)
}

// spawnConflictResolverWorker is the resolve-conflicts branch of the
// worker dispatch. It inspects the paused worktree for conflict markers,
// assembles the conflict bundle, dispatches the resolver apprentice
// through the canonical spawn builder, and runs the conflict-specific
// validation gates on return.
func spawnConflictResolverWorker(ctx *RecoveryActionCtx, plan recovery.RepairPlan, ws WorkspaceHandle, wc *spgit.WorktreeContext) (RepairWorkerResult, error) {
	files, err := wc.ConflictedFiles()
	if err != nil {
		return RepairWorkerResult{}, fmt.Errorf("list conflicted files: %w", err)
	}
	if len(files) == 0 {
		// resolve-conflicts requested with no conflicts on disk is a
		// decide/execute vocabulary mismatch — not a silent success.
		return RepairWorkerResult{}, fmt.Errorf("resolve-conflicts: no conflicted files found on paused worktree (decide should have chosen a different action)")
	}

	bundle := buildConflictBundle(ctx, wc, files)

	workerAttemptID, err := dispatchConflictApprentice(ctx, bundle, ws)
	if err != nil {
		return RepairWorkerResult{WorkerAttemptID: workerAttemptID}, fmt.Errorf("dispatch conflict apprentice: %w", err)
	}

	if err := runConflictValidationGates(ctx, wc, files); err != nil {
		return RepairWorkerResult{WorkerAttemptID: workerAttemptID}, err
	}

	ctx.logf(fmt.Sprintf("repair worker completed; %d file(s) processed, all gates passed", len(files)))
	return RepairWorkerResult{WorkerAttemptID: workerAttemptID, Output: fmt.Sprintf("%d file(s) resolved", len(files))}, nil
}

// spawnGenericRepairWorker is the non-conflict branch of the worker
// dispatch. It builds a role-aware prompt (resummon / reset / triage /
// targeted-fix), stamps the canonical runtime contract onto the
// SpawnConfig via ctx.BuildRuntimeContract, and dispatches the
// apprentice. It does NOT run the conflict-marker validation gates —
// those are specific to the resolver path; the cleric's verify step
// (VerifyPlan) is the authoritative success check for these roles.
func spawnGenericRepairWorker(ctx *RecoveryActionCtx, plan recovery.RepairPlan, ws WorkspaceHandle, wc *spgit.WorktreeContext) (RepairWorkerResult, error) {
	if ctx.Spawner == nil {
		return RepairWorkerResult{}, fmt.Errorf("repair worker: no Spawner wired on ctx")
	}

	spawnName := repairWorkerSpawnName(ctx, plan.Action)
	prompt := renderGenericRepairPrompt(ctx, plan)
	cfg, err := buildRepairWorkerSpawnConfig(ctx, spawnName, plan, ws, prompt)
	if err != nil {
		return RepairWorkerResult{}, err
	}

	dispatch := ctx.DispatchFn
	if dispatch == nil {
		dispatch = ctx.Spawner.Spawn
	}

	started := time.Now()
	handle, spawnErr := dispatch(cfg)
	if spawnErr != nil {
		recordResolverRun(ctx, spawnName, started, "spawn_error")
		return RepairWorkerResult{WorkerAttemptID: spawnName}, fmt.Errorf("spawn %s apprentice: %w", plan.Action, spawnErr)
	}

	waitErr := handle.Wait()
	result := "success"
	if waitErr != nil {
		result = "error"
	}
	recordResolverRun(ctx, spawnName, started, result)

	if waitErr != nil {
		ctx.logf(fmt.Sprintf("repair worker %s exited with %v", spawnName, waitErr))
		return RepairWorkerResult{WorkerAttemptID: spawnName}, fmt.Errorf("repair worker %s: %w", plan.Action, waitErr)
	}
	ctx.logf(fmt.Sprintf("repair worker %s (%s) completed", spawnName, plan.Action))
	return RepairWorkerResult{WorkerAttemptID: spawnName, Output: fmt.Sprintf("%s apprentice completed", plan.Action)}, nil
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
		if beadID := spgit.BeadIDFromSubject(md.Subject); beadID != "" {
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

// dispatchConflictApprentice spawns an apprentice agent into the paused
// worktree with a pre-assembled conflict prompt and blocks until it returns.
// Returns the agent-run row ID recorded via ctx.RecordAgentRun (empty when
// RecordAgentRun is not wired). Any spawn error is returned to the caller;
// a non-nil Wait() is logged but not returned because the validation gates
// are the authoritative check — some subprocess errors are non-fatal (e.g.
// hook noise) and a clean exit with conflict markers still on disk is a
// real failure that the gates catch.
//
// The SpawnConfig is built through ctx.BuildRuntimeContract — the same
// construction site the normal apprentice path uses — so k8s substrate
// validation sees the canonical Identity/Workspace/RunContext fields.
func dispatchConflictApprentice(ctx *RecoveryActionCtx, bundle conflictBundle, ws WorkspaceHandle) (string, error) {
	if ctx.Spawner == nil {
		return "", fmt.Errorf("repair worker: no Spawner wired on ctx")
	}
	if ctx.Worktree == nil {
		return "", fmt.Errorf("repair worker: no worktree on ctx")
	}

	spawnName := repairWorkerSpawnName(ctx, "resolve-conflicts")
	prompt := renderConflictPrompt(ctx, bundle)
	plan := recovery.RepairPlan{Action: "resolve-conflicts"}

	cfg, err := buildRepairWorkerSpawnConfig(ctx, spawnName, plan, ws, prompt)
	if err != nil {
		return "", err
	}

	dispatch := ctx.DispatchFn
	if dispatch == nil {
		dispatch = ctx.Spawner.Spawn
	}

	started := time.Now()
	handle, spawnErr := dispatch(cfg)
	if spawnErr != nil {
		recordResolverRun(ctx, spawnName, started, "spawn_error")
		return spawnName, fmt.Errorf("spawn: %w", spawnErr)
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
		ctx.logf(fmt.Sprintf("repair worker %s exited with %v — validating output", spawnName, waitErr))
	} else {
		ctx.logf(fmt.Sprintf("repair worker %s completed", spawnName))
	}
	return spawnName, nil
}

// repairWorkerSpawnName assembles the canonical spawn name used for every
// repair-worker role. The action is included so downstream logs / metrics
// can distinguish roles without parsing the prompt.
func repairWorkerSpawnName(ctx *RecoveryActionCtx, action string) string {
	ns := ctx.AgentNamespace
	if ns == "" {
		ns = "cleric-repair"
	}
	slug := action
	if slug == "" {
		slug = "repair"
	}
	return fmt.Sprintf("%s-%s-%s-%d", ns, slug, ctx.TargetBeadID, time.Now().UnixNano()%1_000_000)
}

// buildRepairWorkerSpawnConfig is the single SpawnConfig construction
// site for every repair-worker role. It stamps the canonical runtime
// contract via ctx.BuildRuntimeContract (wired by buildRecoveryActionCtx
// to e.withRuntimeContract), so Identity/Workspace/Run fields match what
// the k8s substrate validator and process backend both expect. There is
// no hand-built process-only fallback: callers that bypass the builder
// (direct tests) must inject their own BuildRuntimeContract stub.
func buildRepairWorkerSpawnConfig(ctx *RecoveryActionCtx, spawnName string, plan recovery.RepairPlan, ws WorkspaceHandle, prompt string) (agent.SpawnConfig, error) {
	logPath := ""
	if ctx.LogBaseDir != "" {
		logPath = filepath.Join(ctx.LogBaseDir, "wizards", spawnName+".log")
		_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
	}

	workspace := ws
	if workspace.Name == "" {
		workspace.Name = "recovery"
	}
	if workspace.BaseBranch == "" {
		workspace.BaseBranch = ctx.BaseBranch
	}

	cfg := agent.SpawnConfig{
		Name:         spawnName,
		BeadID:       ctx.TargetBeadID,
		Role:         agent.RoleApprentice,
		Step:         plan.Action,
		ExtraArgs:    []string{"--worktree-dir", ctx.Worktree.Dir, "--no-review"},
		CustomPrompt: prompt,
		LogPath:      logPath,
	}

	if ctx.BuildRuntimeContract == nil {
		return agent.SpawnConfig{}, fmt.Errorf("repair worker: ctx.BuildRuntimeContract is not wired — every worker spawn must flow through the canonical runtime-contract builder")
	}
	stamped, err := ctx.BuildRuntimeContract(cfg, plan.Action, workspace.Name, workspace, HandoffBorrowed)
	if err != nil {
		return agent.SpawnConfig{}, fmt.Errorf("repair worker: stamp runtime contract: %w", err)
	}
	return stamped, nil
}

// renderGenericRepairPrompt composes the apprentice system prompt for
// non-conflict repair roles (resummon / reset / triage / targeted-fix).
// It leads with the role the decide step chose and the reason, then
// surfaces the target bead ID and any role-specific params from the plan
// so the apprentice can pick up the intent without reading recovery
// metadata.
func renderGenericRepairPrompt(ctx *RecoveryActionCtx, plan recovery.RepairPlan) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("You are a cleric repair apprentice running the %q role on a borrowed workspace.\n", plan.Action))
	sb.WriteString("Your job: resolve the failure that interrupted the parent bead so its wizard can resume.\n\n")

	sb.WriteString("## Rules\n")
	sb.WriteString("- Keep changes scoped to fixing the interruption. Do NOT redesign, reformat, or refactor unrelated code.\n")
	sb.WriteString("- Commit your fix with a descriptive message referencing the target bead.\n")
	sb.WriteString("- Do NOT create PRs, push, or touch other branches.\n\n")

	if ctx.TargetBeadID != "" {
		sb.WriteString(fmt.Sprintf("## Target bead\n%s\n\n", ctx.TargetBeadID))
	}
	if ctx.Worktree != nil && ctx.Worktree.Dir != "" {
		sb.WriteString(fmt.Sprintf("## Worktree\n%s\n\n", ctx.Worktree.Dir))
	}
	if plan.Reason != "" {
		sb.WriteString("## Decide reason\n")
		sb.WriteString(plan.Reason)
		if !strings.HasSuffix(plan.Reason, "\n") {
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}
	if len(plan.Params) > 0 {
		sb.WriteString("## Plan parameters\n")
		keys := make([]string, 0, len(plan.Params))
		for k := range plan.Params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sb.WriteString(fmt.Sprintf("- %s: %s\n", k, plan.Params[k]))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("Diagnose and fix the failure now.\n")
	return sb.String()
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
