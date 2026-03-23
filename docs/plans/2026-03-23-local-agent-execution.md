# Local Agent Execution (Phase 2 MVP)

**Goal:** `spire summon N` spawns wizards that claim beads and execute them
using Claude Code on the developer's laptop.

**Date:** 2026-03-23

---

## Architecture

### Pull model (wizard self-serves)

Each wizard is a one-shot background process:

```
spire summon 3
  → find 3 ready beads
  → for each: spawn `spire wizard-run <bead-id>` as background process
  → register in wizards.json with PID
  → exit (summon returns immediately)

spire wizard-run <bead-id>:
  1. resolve repo URL + branch from repos table
  2. create git worktree at /tmp/spire-wizard/<name>/<bead-id>
  3. claim the bead
  4. run spire focus → capture context
  5. load spire.yaml → get model, timeout, validation commands
  6. build prompt (mirrors agent-entrypoint.sh prompt format)
  7. run: claude --dangerously-skip-permissions -p "<prompt>"
     in the worktree directory
  8. validate: lint, build, test (from spire.yaml)
  9. commit + push branch (feat/<bead-id>)
  10. update bead: add comment, mark review-ready or close
  11. write result to ~/.local/share/spire/wizards/<name>/result.json
  12. clean up worktree
```

### Why pull model, not steward-driven

- Steward already works for k8s (assigns via messages, operator creates pods)
- Local process mode doesn't need that indirection
- Simplest path: summon finds ready work, spawns one process per bead
- Steward integration (push model) can be added later

### Why worktrees, not clones

- Repo is already local — worktree is instant (shared .git)
- Matches the existing summon code (creates worktree dirs)
- Clone would duplicate the entire repo each time

---

## Implementation

### New file: `cmd/spire/wizard.go`

The wizard work loop. Mirrors `agent-entrypoint.sh` but in Go,
running as a local process.

```go
func cmdWizardRun(args []string) error  // entry point: spire wizard-run <bead-id>
func wizardResolveRepo(cfg, beadID)     // find repo URL + branch from repos table
func wizardCreateWorktree(repoDir, beadID, baseBranch) (worktreePath, branchName)
func wizardBuildPrompt(beadID, branchName, focusContext, repoConfig)  string
func wizardRunClaude(worktreeDir, prompt, model, timeout)  error
func wizardValidate(worktreeDir, repoConfig)  error
func wizardCommitAndPush(worktreeDir, beadID, branchName)  (commitSHA, error)
func wizardUpdateBead(beadID, branchName, commitSHA, result)  error
func wizardCleanup(worktreePath)
```

### Modified file: `cmd/spire/summon.go`

Wire `summonLocal()` to:
1. Query ready beads (existing `storeGetReadyWork`)
2. Pick min(count, readyCount) beads
3. For each: spawn `spire wizard-run <bead-id>` as background process
4. Track PID in wizards.json

### Modified file: `cmd/spire/main.go`

Add `wizard-run` case to command dispatch (internal command, not
in help text).

### Prompt format

Matches agent-entrypoint.sh so wizard behavior is consistent
between k8s and local:

```
You are Spire autonomous wizard <name>.

Task:
- bead: <bead-id>
- title: <title>
- base branch: main
- feature branch: feat/<bead-id>
- target model: <model>
- max turns: <max-turns>
- hard timeout: <timeout>

Before making changes:
1. Read the focus context below.
2. Read the repo context paths below.

Repo context paths:
- CLAUDE.md
- SPIRE.md

Validation commands:
- install: <cmd>
- lint: <cmd>
- build: <cmd>
- test: <cmd>

Constraints:
- Do not create a PR.
- Prefer leaving file changes for the wrapper to commit and push.

Focus context:
<focus output>

Bead JSON:
<bead json>
```

### Claude invocation

```
claude --dangerously-skip-permissions -p "<prompt>"
```

- `--dangerously-skip-permissions`: no interactive permission prompts
- `-p`: print mode (non-interactive, runs task and exits)
- Working directory: the worktree
- Environment: inherits ANTHROPIC_API_KEY from parent process

---

## What's NOT in this MVP

- Docker mode (2.2) — process mode only for now
- Steward integration (2.1) — wizards self-serve
- `spire logs` (2.4) — output goes to log files, read manually
- `spire up` steward integration (2.5) — run steward separately
- Timeout enforcement — relies on claude's own timeout
- Health monitoring — check PID liveness via roster

---

## Verification

After implementation:

```bash
spire file "Add a hello world endpoint" -t task -p 2
spire summon 1
spire roster          # shows wizard-1 with PID
spire board           # shows bead in Working
# wait for wizard to finish...
spire board           # shows bead moved
git log --oneline --all  # shows feat/<bead-id> branch
```
