---
name: spire-design
description: Brainstorm and capture design thinking in a design bead. Use when the user wants to explore an idea, brainstorm an approach, or design a system before committing to work items. Triggers on "/spire-design", "let's brainstorm", "let's design", or when the user is exploring ideas that should be captured.
---

# Spire Design — Brainstorm and Capture

Design beads are thinking artifacts. They capture exploration, rejected approaches,
and design decisions BEFORE committing to work items (tasks, epics, bugs).

## When to use

- The user says "let's brainstorm", "let's design", "let's think about..."
- An idea is being explored that isn't ready to be filed as a task/epic
- A conversation produces design decisions worth preserving
- The user wants to capture the "why" and "why not" alongside the "what"

## Step 1: Create the design bead

At the START of a brainstorming conversation, create a design bead:

```bash
spire design "Title describing what we're exploring" -p 2
```

Tell the user: "Created design bead <id>. I'll capture our thinking here."

## Step 2: Capture as you go

As the conversation progresses, add comments to the design bead to capture:
- Key insights and decisions
- Rejected approaches (and why)
- Open questions
- Constraints discovered
- References to relevant code or docs

```bash
bd comments add <design-id> "Decision: use phase labels, not schema fields. Why: no upstream beads changes needed."
bd comments add <design-id> "Rejected: per-step review. Why: too slow for most work, sage sees incomplete picture."
```

Don't wait until the end — capture incrementally. The design bead is a living log.

## Step 3: When ready to commit

When the brainstorm settles into actionable work:

1. Summarize the final design as a comment on the design bead
2. Close the design bead
3. Create the work item (task, epic, bug) and link it:

```bash
bd comments add <design-id> "Final design: [summary]"
bd close <design-id>
spire file "Title" -t epic -p 1 --ref <design-id>
```

The `--ref` flag creates a `discovered-from` dependency linking the work item to its design.
This is required — the wizard's design check phase won't advance without it.

## Step 4: If the brainstorm doesn't lead to work

That's fine. Close the design bead with a note:

```bash
bd comments add <design-id> "Parking this — not actionable yet / superseded by X / decided against"
bd close <design-id>
```

The thinking is preserved in the archive for future reference.

## Rules

- Create the design bead EARLY — don't wait until the conversation is over
- Capture incrementally — decisions, rejections, questions as they arise
- One design bead per topic — don't mix unrelated explorations
- Always close design beads when done (settled or parked)
- Link work items to their design bead via `--ref` (creates a `discovered-from` dep)
- Design beads are NOT work items — they won't appear in `bd ready` or be picked up by `spire summon`
