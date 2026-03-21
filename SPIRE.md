# SPIRE.md — Agent Work Instructions

This repo is connected to Spire (prefix: **spi**). Use Spire for work coordination.

## Session lifecycle

```bash
spire up                        # ensure services are running
spire collect                   # check inbox + read your context brief
spire claim <bead-id>           # claim a task (verify → set in_progress)
spire focus <bead-id>           # assemble full context for the task
# ... do the work ...
spire send <agent> "done" --ref <bead-id>   # notify others
```

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
