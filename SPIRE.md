# SPIRE.md — Agent Work Instructions

This repo is connected to Spire (prefix: **spi**). Use Spire for work coordination.

## Session lifecycle

```bash
spire up                        # ensure services are running
spire collect                   # check inbox + read your context brief
spire claim <bead-id>           # claim a task (atomic: pull → verify → set in_progress → push)
spire focus <bead-id>           # assemble full context for the task
# ... do the work ...
bd close <step-id>              # close each molecule step as you complete it
bd close <bead-id>              # close the bead when all work is done
bd dolt push                    # push state to remote
spire send <agent> "done" --ref <bead-id>   # notify others
```

## Completing work

When you finish a task, you MUST close things in order:

1. **Close molecule steps** — `spire focus <bead-id>` shows your workflow molecule.
   Close each step (design, implement, review, merge) with `bd close <step-id>`
2. **Close the bead** — `bd close <bead-id>`
3. **Push state** — `bd dolt push`
4. **Notify** — `spire send <agent> "done" --ref <bead-id>` if assigned via mail

## Filing work

```bash
spire file "Title" -t task -p 2             # file from anywhere (prefix auto-detected in repo)
spire file "Title" --prefix spi -t bug -p 1 # explicit prefix
```

## Messaging

```bash
spire register <name> [context]  # register as an agent, optionally with a brief
spire collect                    # check inbox (also prints your context brief)
spire send <to> "message" --ref <bead-id>
spire read <bead-id>             # mark a message as read
```

## Commit format

Always reference the bead in commit messages:

```
<type>(<bead-id>): <message>
```

Examples:
- `feat(spi-a3f8): add OAuth2 support`
- `fix(xserver-0hy): handle nil pointer in rate limiter`
- `chore(pan-b7d0): upgrade dependencies`

Types: `feat`, `fix`, `chore`, `docs`, `refactor`, `test`

## Key conventions

- **Claim before working**: prevents double-work
- **Priority**: -p 0 (critical) → -p 4 (nice-to-have)
- **Types**: task, bug, feature, epic, chore
- **Epics** auto-sync to Linear — do not create Linear issues manually
