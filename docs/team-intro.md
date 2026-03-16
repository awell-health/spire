Hey team — wanted to share something I've been building that I'm pretty excited about.

**Spire** is a coordination hub for AI agents working across multiple repos. We're going to open-source it under awell-health, with us as the first users.

**The problem:** AI coding agents are powerful but isolated. An agent working in one repo has no idea what an agent in another repo is doing. When your system spans multiple repos — like ours does — agents can't communicate, delegate, or coordinate. Spire fixes that.

**How it works:** Every repo gets a prefix (e.g. `web-`, `api-`). All issues, messages, and workflows from all repos live in one shared database (Dolt — basically git for data). Agents register, send messages to each other, and track work through a shared graph. It's built on beads (https://github.com/steveyegge/beads) for issue tracking and ships as a single Go binary.

**What you get:**
- Cross-repo visibility — one command shows all work everywhere
- Agent-to-agent messaging — "the endpoint you changed broke my tests"
- Epic sync to Linear — create an epic in beads, it appears in Linear automatically. Close it in beads, it closes in Linear.
- `spire connect linear` — OAuth flow sets up everything (team, project, webhooks) from your terminal
- MCP server for Cursor and Claude Code
- `brew install spire` (once we cut v0.1.0)

**Where it stands:** The core is working — CLI, messaging, daemon, webhook receiver, Linear integration, Homebrew distribution pipeline. We're dogfooding it now. The Linear OAuth app is registered and the `spire connect linear` flow is built. Next up is end-to-end testing and cutting the first release.

**The vision:** Any team running AI agents across multiple repos should be able to `brew install spire`, run `spire init`, and have their agents talking to each other in minutes. Linear is the first PM integration — Jira, GitHub Issues, etc. are contribution opportunities once we open source.

Design docs are in the repo if you want the full picture: `docs/superpowers/specs/`
Linear: AWL-3946
