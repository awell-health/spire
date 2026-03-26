# Contributing to Spire

Thank you for your interest in contributing to Spire, a coordination hub for AI agents across repositories. Spire provides centralized work tracking (via beads), agent-to-agent messaging, and integrations with project management tools like Linear.

## Prerequisites

Before you begin, make sure you have the following installed:

- **Go 1.26+** -- the CLI and core logic are written in Go
- **beads CLI** (`bd`) -- Spire's work-tracking system; see the [beads repository](https://github.com/steveyegge/beads) for installation instructions
- **Dolt** -- Spire uses a shared Dolt database for persistent storage; install from [dolthub.com](https://www.dolthub.com/docs/getting-started/installation/)
- **Node.js 20+** -- required for the MCP server package
- **pnpm** -- used as the package manager for the Node.js workspace

## Getting Started

1. **Fork and clone the repository:**

   ```bash
   git clone https://github.com/<your-fork>/spire.git
   cd spire
   ```

2. **Run the setup script:**

   ```bash
   ./setup.sh
   ```

   This will initialize the Dolt database, install Node.js dependencies, and configure local defaults.

3. **Build the CLI:**

   ```bash
   go build -o ~/.local/bin/spire ./cmd/spire
   ```

4. **Verify your installation:**

   ```bash
   spire --help
   ```

## Project Structure

Spire is organized as a monorepo managed with pnpm and Turbo:

```
spire/
  cmd/spire/          Go CLI — all commands, daemon, webhook server, epic sync
  packages/
    mcp-server/       MCP server for Cursor/Claude Code integration (Node.js)
  docs/
    superpowers/
      specs/          Design specs for new features
      plans/          Implementation plans
  .beads/
    formulas/         Workflow templates (spire-agent-work)
    routes.jsonl      Multi-repo routing config
  cursor/             Cursor IDE rules
  setup.sh            Hub + satellite setup
  satellites.conf     Your satellite repos (gitignored)
```

- **`cmd/spire/`** -- The Go source for the `spire` CLI. Includes agent messaging, Linear OAuth connect flow, epic sync, webhook HTTP server, daemon, and keychain integration. Zero external Go dependencies.
- **`packages/mcp-server/`** -- A Node.js MCP (Model Context Protocol) server that exposes Spire's capabilities to AI coding tools like Cursor and Claude Code.
- **`docs/`** -- Design specifications and implementation plans.

## Adding a New PM Integration

Spire is designed to integrate with multiple project management tools. To add a new integration, follow the pattern established by the Linear integration:

1. **Study the existing Linear integration.** The design spec is at `docs/superpowers/specs/2026-03-15-spire-connect-linear.md`. It covers the full lifecycle: OAuth connect flow, daemon sync logic, and webhook handling.

2. **Implement the OAuth connect flow.** Allow users to authenticate Spire with the PM tool. Store credentials securely using the same pattern as the Linear integration.

3. **Implement daemon sync logic.** Add sync logic to the daemon (`cmd/spire/daemon.go`) that watches for bead changes and mirrors them to the external tool, and vice versa. See `cmd/spire/epic_sync.go` for the Linear example.

4. **Implement webhook handling.** The webhook server (`cmd/spire/serve.go`) receives inbound events. Add a new handler for your integration's webhook events, or extend the existing queue-based processing in `cmd/spire/webhook.go`.

5. **Add a design spec.** Document your integration in `docs/superpowers/specs/` following the naming convention: `YYYY-MM-DD-spire-connect-<tool>.md`.

## Code Style

- **Go:** Use the standard library only. Spire's Go code has zero external dependencies, and we intend to keep it that way. Run `gofmt` before committing.
- **Node.js:** Use ES modules (`import`/`export`). No CommonJS. Follow the existing code style in `packages/`.
- **General:** Keep functions small, names descriptive, and comments focused on _why_ rather than _what_.

## Testing

Run the Go test suite:

```bash
go test ./cmd/spire/...
```

For the MCP server and other Node.js packages:

```bash
pnpm test
```

Please add tests for any new functionality. If you are fixing a bug, add a regression test that would have caught it.

## Developer Certificate of Origin (DCO)

Spire requires all contributors to sign off on their commits using the [Developer Certificate of Origin (DCO 1.1)](DCO). The sign-off certifies that you wrote the code or have the right to submit it under the Apache 2.0 license.

To sign off, add a `Signed-off-by` trailer to each commit message. Git can do this automatically with the `-s` flag:

```bash
git commit -s -m "feat: add new feature"
```

This produces a commit message like:

```
feat: add new feature

Signed-off-by: Your Name <your@email.com>
```

Pull requests with unsigned commits will not be merged. If you forgot to sign off, you can amend recent commits:

```bash
# Amend the last commit
git commit --amend -s --no-edit

# Or sign off multiple commits with rebase
git rebase --signoff HEAD~N
```

## Pull Request Process

1. **Fork** the repository and create a feature branch from `main`.
2. **Make your changes** in focused, well-described commits. Sign each commit with `-s`.
3. **Run tests** locally to make sure nothing is broken.
4. **Open a pull request** against `main` with a clear description of what you changed and why.
5. A maintainer will review your PR. Please be responsive to feedback.

## Issue Tracking

We use **beads** for issue tracking. If you find a bug or want to propose a feature, you can file it directly:

```bash
bd create "Title of the issue" -p 2 -t bug --description="Detailed description of the problem."
```

Or for a feature request:

```bash
bd create "Title of the feature" -p 2 -t feature --description="What you'd like to see and why."
```

You can also open a GitHub issue if you prefer.

## Code of Conduct

This project follows the Contributor Covenant Code of Conduct. Please read [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) before participating.

## License

By contributing to Spire, you agree that your contributions will be licensed under the [Apache License 2.0](LICENSE).
