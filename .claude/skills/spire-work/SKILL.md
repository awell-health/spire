---
name: spire-work
description: Execute a full work cycle on a beads issue (design, implement, review, merge). Use when the user says "/spire-work BEAD_ID" or asks to work on a specific bead. Dispatches a fresh subagent with clean context that recovers everything from the bead graph. Works across session boundaries.
---

# Spire Work

Full agent work protocol on a bead: design -> implement -> review -> merge.

## IMPORTANT: Dispatch as subagent

This skill MUST be dispatched as a background Agent subagent to get clean context. Do NOT execute the steps inline in the current conversation.

Dispatch like this:

```
Agent tool:
  description: "spire-work: <bead-id>"
  run_in_background: true
  prompt: |
    You are executing the spire-work protocol on bead <bead-id>.
    Follow the steps below exactly. You have clean context —
    recover everything from the bead graph.

    [paste all steps below into the prompt]
```

After dispatching, tell the user: "Dispatched spire-work agent for <bead-id>. Working in background."

The subagent prompt should include ALL of the steps below verbatim.

---

## Step 0: Claim the bead

Use `spire claim` to verify and claim:

```bash
spire claim <bead-id>
```

This does: verify bead exists and isn't closed/owned by someone else -> `bd update --claim --status in_progress`.

If `spire claim` fails (bead closed, already owned, etc.) — **stop and report the error**. Do not proceed.

This MUST happen before any other work. It prevents double-work and signals to the team that the bead is being actively worked on.

## Step 1: Detect bead type and route

The JSON output from `spire claim` includes the bead's `type` field. Check it:

- **If `epic`**: go to "Epic routing" below
- **If `task`, `bug`, `feature`, `chore`**: go to "Task routing" below

### Epic routing

Epics are not worked directly — they are coordination points. For an epic:

1. List children: `bd children <bead-id> --json`
2. Find ready tasks: `bd ready --json` and filter for children of this epic
3. For each ready child task, dispatch a **separate** spire-work subagent:
   ```
   Agent tool:
     description: "spire-work: <child-id>"
     run_in_background: true
     isolation: "worktree"
     prompt: [full spire-work protocol for <child-id>]
   ```
4. Dispatch as many ready tasks in parallel as possible (use a single message with multiple Agent tool calls).
5. Report which children were dispatched and which are still blocked.
6. Do NOT close the epic — it closes when all children are done.

Then stop. The subagents handle the actual work.

### Task routing

Continue to Step 2 below.

## Step 2: Focus and recover context

```bash
spire focus <bead-id>
bd graph <bead-id>
```

`spire focus` assembles full context: bead details, workflow progress, referenced beads, messages, comments. It pours a `spire-agent-work` molecule on first focus (4 steps: design, implement, review, merge). On subsequent focuses it shows progress.

If the bead has children, review them: `bd children <bead-id>`

## Step 3: Find the current step

```bash
bd list --label "workflow:<bead-id>" --status=open --json   # find molecule
bd mol progress <molecule-id>                                # check progress
bd children <molecule-id>                                    # list steps
```

The first open, unblocked child step is where you are.

## Step 4: Execute the current step

### Design

1. Read bead description, linked beads (`ref:` labels), parent epic context
2. If a spec exists in `docs/superpowers/specs/`, read it — skip brainstorming
3. If no spec, invoke `superpowers:brainstorming` to create one
4. After spec approval, invoke `superpowers:writing-plans` for the implementation plan
5. `bd close <design-step-id>`

### Implement

1. Read the plan from `docs/superpowers/plans/`
2. Create worktree: `git worktree add .worktrees/<bead-id> -b feat/<bead-id>`
3. Execute the plan in the worktree — dispatch subagents if parallelizable
4. All work on branch `feat/<bead-id>` in `.worktrees/<bead-id>`
5. Run tests, verify build in worktree
6. `bd close <implement-step-id>`

### Review

1. Run full test suite and build in the worktree
2. Review changes against original task requirements
3. Fix issues on the same branch
4. `bd close <review-step-id>`

### Merge

1. `git merge feat/<bead-id> --no-ff -m "Merge feat/<bead-id>: <summary>"`
2. Run tests on merged result
3. `git worktree remove .worktrees/<bead-id> && git branch -d feat/<bead-id>`
4. `bd close <merge-step-id>`
5. `bd close <bead-id>`

## Step 5: Report completion

If the bead was assigned via spire mail, notify the sender:

```bash
bd list --rig=spi --label "msg,ref:<bead-id>" --json
spire send <sender> "Completed <bead-id>: <summary>" --ref <bead-id>
```

## Rules

- Always `spire claim` FIRST (Step 0) — verify/claim
- Always `spire focus` for tasks (Step 2) — single source of context
- One step at a time — close each molecule step when complete
- All implementation in `.worktrees/<bead-id>` on `feat/<bead-id>`
- Never merge without passing tests
- If blocked, stop and report
