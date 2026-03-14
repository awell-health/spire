# Spire Grok — Design Spec

## Problem

`spire focus` shows bead-local context: title, description, labels, comments, workflow progress, and referenced beads. But when an epic is linked to Linear (via a `linear:` label), the agent has no way to see the live Linear data — status, assignee, sprint, comments from PMs — without leaving the terminal and opening the Linear UI.

`spire grok` closes this gap: it does everything `spire focus` does, then enriches the output with live data from the Linear API.

## Architecture

`spire grok` is a new subcommand in the existing `spire` Go binary. It:

1. Runs the same bead-local context assembly as `spire focus` (bead details, workflow, references, comments)
2. Looks for a `linear:` label on the bead (e.g., `linear:AWE-123`)
3. If found, calls the Linear GraphQL API to fetch the issue
4. Appends Linear-enriched context to the output

```
┌─────────────────────────────────────────────────────────┐
│                    spire grok <bead-id>                  │
│                                                          │
│  1. bd show <bead-id> --json        ← same as focus      │
│  2. Workflow progress (if molecule)  ← same as focus      │
│  3. Referenced beads, messages       ← same as focus      │
│  4. Comments                         ← same as focus      │
│  5. Find linear: label                                    │
│     ↓ if found:                                           │
│     a. GraphQL query to Linear API                        │
│     b. Fetch: title, description, status, assignee,       │
│        priority, labels, comments, url                    │
│     c. Print enriched Linear section                      │
│                                                          │
│  If no linear: label, output is identical to focus.       │
└─────────────────────────────────────────────────────────┘
```

### Design principles

- **Same process, new subcommand.** No separate binary. `spire grok` is just another command in `cmd/spire/`.
- **Superset of focus.** If there is no `linear:` label, grok produces the same output as focus. An agent can always use `grok` instead of `focus`.
- **Direct Linear API calls.** Unlike the daemon (which only reads webhook payloads), grok makes live GraphQL API calls to Linear. This is intentional — the agent needs real-time data, not stale webhook snapshots.
- **Graceful degradation.** If `LINEAR_API_KEY` is not set or the API call fails, grok still prints the bead-local context and logs a warning about the missing Linear data. It never fails hard because of Linear.
- **No mutations.** Grok is read-only. It never creates, updates, or closes anything in Linear or beads.

## Subcommand: `spire grok`

```
spire grok <bead-id>
```

No flags beyond the bead ID. The Linear API key is sourced from the environment.

### API key resolution

Priority order:

1. `LINEAR_API_KEY` environment variable
2. `bd config get linear-api-key` (beads config)

If neither is set, grok skips the Linear enrichment and prints a warning:

```
spire: warning: LINEAR_API_KEY not set, skipping Linear enrichment
```

### Linear GraphQL query

A single GraphQL query fetches everything needed:

```graphql
query IssueByIdentifier($identifier: String!) {
  issueByIdentifier(identifier: $identifier) {
    id
    identifier
    title
    description
    url
    priority
    priorityLabel
    state {
      name
      type
    }
    assignee {
      name
      email
    }
    labels {
      nodes {
        name
      }
    }
    comments {
      nodes {
        body
        user {
          name
        }
        createdAt
      }
    }
  }
}
```

This is a single request. The Linear API supports fetching an issue by its human-readable identifier (e.g., `AWE-123`).

### Output format

The Linear section is appended after the bead-local context (after comments):

```
--- Linear: AWE-123 ---
URL: https://linear.app/awell/issue/AWE-123
Status: In Progress (started)
Assignee: JB (jb@awellhealth.com)
Priority: High
Labels: Panels - Design, Bug
Description:
  The issue description from Linear, indented.

--- Linear Comments (3) ---
[JB, 2026-03-12]: First comment body
[PM, 2026-03-11]: Second comment body
[Bot, 2026-03-10]: Third comment body
```

Comments are sorted most-recent-first (the API returns them this way by default).

If the Linear issue is not found (deleted, identifier changed), grok logs:

```
spire: warning: Linear issue AWE-123 not found
```

and continues with bead-local context only.

## Implementation

### New file: `grok.go`

Contains:

- `cmdGrok(args []string) error` — the subcommand handler
- `linearAPIKey() string` — resolves the API key
- `fetchLinearIssue(apiKey, identifier string) (*LinearIssue, error)` — GraphQL call
- `LinearIssue` struct — parsed response
- Output formatting logic

### Linear API client

The client is minimal — a single function, not a reusable class. It:

1. Builds the GraphQL query with the identifier variable
2. POSTs to `https://api.linear.app/graphql`
3. Sets `Authorization: <api-key>` header (Linear uses bare API keys, no "Bearer" prefix)
4. Parses the JSON response into a Go struct
5. Returns the parsed issue or an error

The client uses `net/http` from the Go stdlib. No external dependencies.

### LinearIssue struct

```go
type LinearIssue struct {
    ID            string
    Identifier    string
    Title         string
    Description   string
    URL           string
    Priority      int
    PriorityLabel string
    State         struct {
        Name string
        Type string // "started", "completed", "canceled", etc.
    }
    Assignee *struct {
        Name  string
        Email string
    }
    Labels   []string
    Comments []LinearComment
}

type LinearComment struct {
    Body      string
    User      string
    CreatedAt string
}
```

### Modified file: `main.go`

Add `"grok"` case to the switch and update `printUsage()`.

### Modified file: `spire_test.go`

- Unit test: `TestLinearAPIKey` — test key resolution from env
- Unit test: `TestParseLinearIssue` — test JSON response parsing
- Integration test: `TestIntegrationGrok` — requires `LINEAR_API_KEY` and a known Linear issue; skip if not available

## Error handling

- **No API key**: warn, skip Linear section, return nil (not an error)
- **API call fails (network, auth)**: warn with error message, skip Linear section, return nil
- **Issue not found**: warn, skip Linear section, return nil
- **Bead not found**: return error (same as focus)
- **No linear: label**: skip Linear section silently (this is normal for non-epic beads)

## Testing

- Unit test for API key resolution (pure env-based, no network)
- Unit test for GraphQL response parsing (canned JSON, no network)
- Unit test for output formatting
- Integration test with real Linear API (skipped if no API key)
- Integration test for grok on a bead without linear: label (should behave like focus)

## Out of scope (v1)

- **Caching** — each grok call makes a fresh API request. No local cache.
- **Multiple linear: labels** — only the first `linear:` label is used.
- **Linear sub-issues** — only the top-level issue is fetched, not its children.
- **Writing back to Linear** — grok is strictly read-only.
- **Pagination** — comments are limited to what the API returns in a single page (typically 50, which is plenty).
